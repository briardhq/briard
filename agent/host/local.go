// The local admin door — the surface `briard` (the operator CLI) talks to. The host agent
// otherwise only dials: out to the cloud (the report, whose answer carries the directives) and in
// to the guest's virtio-serial socket. The CLI cannot simply do the work itself: the agent holds
// the guest control channel, so a second process driving guest verbs would fight it for the wire.
//
// So the CLI is an injector: it hands the agent an api.Directive over this socket, and the agent
// applies it through the exact path the cloud's directives take (the observe loop's run, then
// dispatch); the tray and the guest's admin port (adminport.go) inject the same way. shared/api
// already names the cloud twin of this gesture, DirectivePath ("admin: enqueue a directive for a
// node"), so this is that endpoint's local door, not a second way to drive a node, and a CLI that
// sees little production use cannot rot: it is a different doorway onto a permanently hot path,
// exercised by every cloud-driven install.
//
// Root-only by construction: the socket is set to 0600 and the agent runs as root. On Windows the
// socket's DACL is the same gate -- connect needs write on the socket file there too -- and admits
// SYSTEM, Administrators and the install user's account alone (restrictDoor).
package host

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"time"

	"briard.io/agent/install"
	"briard.io/shared/api"
	"briard.io/shared/notify"
)

// localRequest is one locally submitted directive (the admin socket's or the guest admin port's)
// plus the channel its terminal outcome returns on. The observe loop is the only consumer: it
// decides the lane a directive runs on (acts.go), and one place must own that decision.
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
func serveLocal(ctx context.Context, sock, owner string, reqs chan<- localRequest, logf func(string, ...any)) {
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
	// Shut to all but the agent's own (restrictDoor), after every bind: the bind makes a new
	// file. A door we cannot prove is shut is one we close.
	if err := restrictDoor(sock, owner); err != nil {
		logf("admin socket: restricting %s: %v (local CLI unavailable)", sock, err)
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
// There is deliberately no read deadline on the wait. A locally-submitted upgrade legitimately
// runs for minutes (bounded by UpgradeBudget, 15 by default), and a CLI that gave up early would
// report a failure for an op that went on to succeed — the worst answer an admin tool can give.
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
		// The stop directive ends this context and is answered a moment after: the loop that
		// cancelled is the one that sends its outcome. A second covers that, and nothing else.
		select {
		case o := <-resp:
			writeOutcome(conn, o)
		case <-time.After(time.Second):
			writeOutcome(conn, failed("agent shut down before the directive was applied"))
		}
	}
}

// writeOutcome is best-effort: the caller may already be gone, and the agent has done (or
// refused) the work either way — the outcome the cloud cares about rides its own path.
func writeOutcome(conn net.Conn, o api.DirectiveOutcome) {
	_ = json.NewEncoder(conn).Encode(o)
}

// LocalStatus is a status directive's answer, as JSON in the outcome's Detail: what the tray
// shows. Everything in it is already on this host -- the last tick's health, the alert store,
// the act lane, the lease's refusal, the catalog's view over the installed-app cache -- so
// asking costs no read of the guest.
type LocalStatus struct {
	Serving   bool `json:"serving"`   // the last tick found the node healthy
	Attention int  `json:"attention"` // alerts needing attention now (notify.Attention)
	Updating  bool `json:"updating"`  // an update of the agent, the guest's image or its OS is in flight
	// Refused is why the node cannot serve until the household acts, and what to do (the network
	// refusal's own line, lease.go), or "" when nothing refuses. Waiting does not end it, so a
	// tray waiting for the page stops on it.
	Refused string `json:"refused,omitempty"`
	// Updates is every installed app the catalog offers an update for, in its order: an
	// offer, not a fault, so it is counted apart from Attention.
	Updates []AppUpdate `json:"updates,omitempty"`
}

// AppUpdate is one offered update: the app by its catalog name and its title, and the version on
// offer.
type AppUpdate struct {
	Name    string `json:"name"`
	Title   string `json:"title"`
	Version string `json:"version"`
}

// updates is the offers in the catalog's view now.
func (cfg Config) updates() []AppUpdate {
	var out []AppUpdate
	for _, r := range cfg.catalog.view(cfg.installedByName(func(string, ...any) {})).Releases() {
		if r.Update {
			out = append(out, AppUpdate{Name: r.Name, Title: r.Title, Version: r.Latest})
		}
	}
	return out
}

// updateKinds are the acts the tray calls an update.
var updateKinds = map[string]bool{
	api.DirectiveAgentUpdate:      true,
	install.DirectiveUpdateVM:     true,
	install.DirectiveUpdateVMAuto: true,
	api.DirectiveUpgradeSystem:    true,
}

func (cfg Config) applyStatus(d api.Directive, n notify.Notifier) api.DirectiveOutcome {
	st := LocalStatus{Serving: cfg.healthy != nil && cfg.healthy.Load(), Updating: updateKinds[cfg.acts.kind()]}
	if detail, fix := cfg.lease.refusal(); detail != "" {
		st.Refused = detail + "; " + fix
	}
	if s, ok := n.(*alertStore); ok {
		recs, _ := s.snapshot()
		st.Attention = len(notify.Attention(recs))
	}
	st.Updates = cfg.updates()
	b, err := json.Marshal(st)
	if err != nil {
		return api.DirectiveOutcome{ID: d.ID, State: api.OutcomeFailed, Detail: err.Error()}
	}
	return api.DirectiveOutcome{ID: d.ID, State: api.OutcomeDone, Detail: string(b)}
}

// holdDoor answers the local door until `until` fires or ctx ends, beating as it goes, for the
// stretches when the loop is not there to: the install finishing (finish.go), the disks being
// made (disks.go) and the recovery ladder (guestrecover.go). As in the network's wait (waitTick),
// a stop is the one directive taken; anything else is refused at once with the reason, read
// afresh each time so a wait that moves on can say so. The alternative is what the channel does
// with no reader: a CLI that blocks until the agent exits, which to the person at the machine is
// an agent that wedged.
func (cfg Config) holdDoor(ctx context.Context, local <-chan localRequest, refusal func() string, until <-chan struct{}, logf func(string, ...any)) error {
	tick := time.NewTicker(netTick)
	defer tick.Stop()
	for {
		cfg.beat.Beat()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-until:
			return nil
		case <-tick.C:
		case req := <-local:
			if req.d.Kind == api.DirectiveStop {
				req.resp <- cfg.applyStop(req.d, logf)
				continue
			}
			why := refusal()
			logf("admin socket: refusing %s -- %s", req.d.Kind, why)
			req.resp <- api.DirectiveOutcome{ID: req.d.ID, State: api.OutcomeFailed, Detail: why}
		}
	}
}
