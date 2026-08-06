// The local admin door — the surface `briard` (the operator CLI) talks to.
//
// Nothing in the host agent listened before this. It dials OUT to the cloud (the long-poll) and
// IN to the guest's virtio-serial socket, so an operator standing on the box had no way to ask it
// for anything. And the CLI cannot simply do the work itself: the agent holds the guest control
// channel, so a second process driving guest verbs would fight it for the wire.
//
// So the CLI is an INJECTOR. It hands the agent an api.Directive over this socket and the agent
// applies it through the exact path the cloud's directives take (dispatch, in the observe loop).
// shared/api already names the cloud twin of this gesture — DirectivePath, "admin: enqueue a
// directive for a node" — so this is that endpoint's LOCAL DOOR rather than a second way to drive
// a node. The payoff is that a CLI which sees little production use cannot rot: it is a different
// doorway onto a permanently hot path, exercised by every cloud-driven install.
//
// Root-only by construction: the socket is created 0600 and the agent runs as root.
package host

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"

	"briard.io/shared/api"
)

// localRequest is one CLI-submitted directive plus the channel its terminal outcome returns on.
// The observe loop is the only consumer: applying there rather than in the accept goroutine is
// what keeps an admin op off the guest channel while a cycle is using it.
type localRequest struct {
	d    api.Directive
	resp chan api.DirectiveOutcome
}

// serveLocal accepts on the admin socket until ctx is cancelled, handing each submitted directive
// to the observe loop and returning the loop's outcome to the caller.
//
// Every failure here is non-fatal and logged: a node that cannot open its admin door still serves,
// and losing the CLI is an inconvenience where refusing to run would be an outage. That matters
// most on a box where something else already holds the path — the door is worth strictly less
// than the node.
func serveLocal(ctx context.Context, sock string, reqs chan<- localRequest, logf func(string, ...any)) {
	if sock == "" {
		return // explicitly disabled
	}
	if err := os.MkdirAll(filepath.Dir(sock), 0o700); err != nil {
		logf("admin socket: %v (local CLI unavailable)", err)
		return
	}
	// A socket left behind by an unclean stop would refuse the bind. The agent is the only
	// writer of this path, so removing it is safe and makes a restart reliable.
	_ = os.Remove(sock)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		logf("admin socket: listen %s: %v (local CLI unavailable)", sock, err)
		return
	}
	// Root-only. The bind honours the process umask, so an explicit chmod is the only way to
	// know the mode; a door we cannot prove is shut is one we close.
	if err := os.Chmod(sock, 0o600); err != nil {
		logf("admin socket: chmod %s: %v (local CLI unavailable)", sock, err)
		_ = ln.Close()
		return
	}
	logf("admin socket listening at %s", sock)
	go func() {
		<-ctx.Done()
		_ = ln.Close() // unblocks Accept below
	}()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() == nil {
				logf("admin socket: accept: %v (local CLI unavailable)", err)
			}
			return
		}
		go handleLocal(ctx, conn, reqs, logf)
	}
}

// handleLocal reads one directive, waits for the observe loop to apply it, and writes back the
// terminal outcome. One directive per connection: the CLI is a one-shot, and a request/response
// pair with no framing beyond JSON's own is the least machinery that carries an outcome home.
//
// There is deliberately NO read deadline on the wait. A locally-submitted upgrade legitimately
// runs for minutes (applyDirective bounds it at 10), and a CLI that gave up early would report a
// failure for an op that went on to succeed — the worst answer an admin tool can give.
func handleLocal(ctx context.Context, conn net.Conn, reqs chan<- localRequest, logf func(string, ...any)) {
	defer conn.Close()
	failed := func(detail string) api.DirectiveOutcome {
		return api.DirectiveOutcome{State: api.OutcomeFailed, Detail: detail}
	}
	var d api.Directive
	if err := json.NewDecoder(conn).Decode(&d); err != nil {
		writeOutcome(conn, failed("bad request: "+err.Error()))
		return
	}
	if d.Kind == "" {
		writeOutcome(conn, api.DirectiveOutcome{ID: d.ID, State: api.OutcomeFailed, Detail: "directive has no kind"})
		return
	}
	logf("admin socket: directive kind=%s submitted locally", d.Kind)
	// Buffered so the observe loop's send never blocks, even if the CLI hung up mid-op.
	resp := make(chan api.DirectiveOutcome, 1)
	select {
	case reqs <- localRequest{d: d, resp: resp}:
	case <-ctx.Done():
		writeOutcome(conn, failed("agent is shutting down"))
		return
	}
	select {
	case o := <-resp:
		writeOutcome(conn, o)
	case <-ctx.Done():
		writeOutcome(conn, failed("agent shut down before the directive was applied"))
	}
}

// writeOutcome is best-effort: the caller may already be gone, and the agent has done (or
// refused) the work either way — the outcome the cloud cares about rides its own path.
func writeOutcome(conn net.Conn, o api.DirectiveOutcome) {
	_ = json.NewEncoder(conn).Encode(o)
}
