// The guest's admin port ([V3b.31i]) -- the household dashboard's way to ask the host for
// something.
//
// The dashboard lives in the guest ([V3b.31a](b)) and a service install is a host directive, so
// "Set up Home Assistant" needs a way for a process in the guest to reach the supervisor. It is
// NOT the network: the host and the guest already share a virtio-serial bus, and qemu serves a
// second port on it as a unix socket on the host, exactly like the control channel with the roles
// reversed. The dashboard writes one directive as a JSON line to its end of the port; this file
// dials the host end, hands the directive to the observe loop through the same channel the local
// admin door uses, and writes the outcome back. No listener, no address, no credential: only the
// host agent and a root-owned unit inside this guest can see the wire, on Linux and Windows alike.
//
// The guest runs no logic on it -- a person on a trusted device pressed a button, and the host
// decides through the one dispatch every directive takes. What the port DOES restrict is what the
// guest may ask for: only directives whose effect stays inside the guest (a service install or
// prewarm). A guest that has been taken over can then at most install a catalogued service into
// itself, which it could run anyway; it cannot roll the host's OS, hand the flock over, or touch
// anything the guest does not already own.
package host

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"time"

	"briard.io/shared/api"
)

// guestMayAsk is the allowlist: the directives whose effect stays inside the guest.
func guestMayAsk(kind string) bool {
	return kind == api.DirectiveServiceInstall || kind == api.DirectiveServicePrewarm
}

// serveAdminPort dials the host end of the guest's admin port and serves it until ctx ends,
// reconnecting whenever the wire drops (qemu restarted with the guest, or the socket was not
// there yet). Every failure is logged and survived: a node whose dashboard cannot ask for an
// install still serves, and the CLI is the same request typed.
func serveAdminPort(ctx context.Context, sock string, reqs chan<- localRequest, logf func(string, ...any)) {
	if sock == "" {
		return // no port: a launch without a dashboard
	}
	for ctx.Err() == nil {
		conn, err := dialControl(ctx, sock)
		if err != nil {
			return // ctx ended
		}
		logf("admin port: serving the guest's dashboard at %s", sock)
		err = serveAdminPortConn(ctx, conn, reqs, logf)
		_ = conn.Close()
		if ctx.Err() != nil {
			return
		}
		logf("admin port: %v; reconnecting", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
}

// serveAdminPortConn is one life of the wire: a directive per line, its outcome per line, in
// order. A line that does not parse gets a failed outcome with no ID, which the dashboard ignores
// -- it waits for the ID it sent -- so a stale half-line from a dashboard that died mid-write
// cannot be mistaken for the answer to the next request. Returns when the wire ends.
func serveAdminPortConn(ctx context.Context, conn net.Conn, reqs chan<- localRequest, logf func(string, ...any)) error {
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var d api.Directive
		if err := json.Unmarshal(line, &d); err != nil {
			writeOutcome(conn, api.DirectiveOutcome{State: api.OutcomeFailed, Detail: "bad request: " + err.Error()})
			continue
		}
		if !guestMayAsk(d.Kind) {
			logf("admin port: refused a %q directive from the guest", d.Kind)
			writeOutcome(conn, api.DirectiveOutcome{ID: d.ID, State: api.OutcomeFailed, Detail: "the guest may not ask for " + d.Kind + "; only a service install"})
			continue
		}
		logf("admin port: directive kind=%s submitted by the guest's dashboard", d.Kind)
		resp := make(chan api.DirectiveOutcome, 1)
		select {
		case reqs <- localRequest{d: d, resp: resp}:
		case <-ctx.Done():
			writeOutcome(conn, api.DirectiveOutcome{ID: d.ID, State: api.OutcomeFailed, Detail: "agent is shutting down"})
			return ctx.Err()
		}
		select {
		case o := <-resp:
			writeOutcome(conn, o)
		case <-ctx.Done():
			writeOutcome(conn, api.DirectiveOutcome{ID: d.ID, State: api.OutcomeFailed, Detail: "agent shut down before the directive was applied"})
			return ctx.Err()
		}
	}
	if err := sc.Err(); err != nil && !errors.Is(err, net.ErrClosed) {
		return err
	}
	return io.EOF
}
