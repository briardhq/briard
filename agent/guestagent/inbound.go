package guestagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"briard.io/agent/quadlet"
)

// The INBOUND channel: the first thing in this system that lets a service's container ask the
// guest agent for something. Everything else briard does to a service goes the other way — files
// written into a stopped container, HTTP dialled at a running one — and the asymmetry was not an
// accident, so reversing it for one feature deserves its own file and its own rules.
//
// WHY IT HAS TO EXIST ([B.143]). The generic snapshot hook fires at CONTAINER start, which is the
// only boundary the guest agent can see from outside. Home Assistant restarts itself far more
// often than its container does — `homeassistant.restart` exits 100 and s6 re-runs the service —
// and each of those is a stopped window where a snapshot is application-consistent. The container
// unit stays active throughout, so there is no transition to poll for and no signal to subscribe
// to: the only thing that knows is the process inside, and it needs a way to say so AND to be
// told when it may proceed.
//
// ⚠️ THE CALLER IS UNTRUSTED, and this is the rule the whole design hangs off. The socket is
// bind-mounted into the service's container, so everything running as that service can reach it —
// for Home Assistant that includes every custom component the household ever installed from HACS.
// Before this channel a compromised Home Assistant could not reach the guest agent at all. So:
//
//   - THE CALLER DOES NOT NAME ITSELF. The service identity comes from WHICH socket the call
//     arrived on, fixed when the listener was created, and is passed to Serve by the caller that
//     owns the listener. Nothing in the request body can change it. A request that could name its
//     own service would let any container act for any other.
//   - NO VERB NAMES A PATH. Every path is derived from the service identity (quadlet.DataRoot,
//     quadlet.SnapshotMember). A verb taking a path is a verb that reads or writes anywhere the
//     agent can.
//   - NO VERB DESTROYS ANYTHING. Pruning, restoring and deleting stay on the host's side of the
//     channel, where the caller is the product rather than the workload.
//   - EVERY VERB IS BOUNDED. The rate limit below is not only picker hygiene: it is what stops a
//     hostile or looping caller from filling the replicated volume, which is [B.155]'s failure
//     arriving by a new road.
//
// ONE REQUEST, ONE RESPONSE, THEN THE CONNECTION IS DONE. No session and no state carried
// between calls: every answer is derived from the ring on disk and the manifest on the volume,
// so a handler that has never run before reaches the same answer as one that has.
//
// SYSTEMD OWNS THE BIND (socket activation, briard-inbound@.socket), and that is not packaging
// detail. Converge runs in TWO different processes — the host's verb inside the long-running
// agent, and drbd-reactor's one-shot `--converge` on the promotion path — so a listener owned by
// the agent would never learn about a service the other one promoted, and two listeners would
// contend for one path. A socket unit is learned once and serves both. The handler inherits the
// listening fd, serves connections one at a time, and exits when it has been idle a while;
// systemd brings it back on the next connection.

// InboundVerb is a request this channel accepts. The set is closed and deliberately tiny; read
// the trust rules above before adding to it.
type InboundVerb string

const (
	// VerbServiceStarting says "my service is about to start, and I am holding it until you
	// answer". The WAIT is the point: the caller blocks before handing over to the real
	// entrypoint, so whatever the agent does happens while the service is genuinely stopped.
	VerbServiceStarting InboundVerb = "service.starting"
)

// inboundRequest is one call. It carries no service name on purpose — see the trust rules.
type inboundRequest struct {
	Verb InboundVerb `json:"verb"`
}

// inboundResponse is the answer. Error is empty on success; Detail is for the caller's log and
// never for its control flow — the wrapper that calls this can do nothing with either.
type inboundResponse struct {
	Error  string `json:"error,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// plainStartFloor is how young the newest member may be before another plain start is skipped.
//
// A crash loop restarts every few seconds and would otherwise fill the picker with hundreds of
// identical entries. Space is not the issue — nothing changed between them — the SELECTOR is, and
// the member that matters in a crash loop is the FIRST one, taken before the bad change. Skipping
// the rest keeps exactly that one.
//
// It is also the abuse bound. See the trust rules above.
const plainStartFloor = 90 * time.Second

// ServeInbound reads one request from r, serves it as `service`, and writes one response to w.
//
// `service` is the identity of the LISTENER, not of the caller: whoever owns the socket decides
// what it speaks for, and the request cannot say otherwise.
//
// An error is reported to the caller AND returned, because the two readers are different: the
// caller logs it and carries on (it must never fail a household's service over this), while the
// return value is what the handler process exits on so a human reading the journal sees it.
func ServeInbound(ctx context.Context, x Executor, service string, r io.Reader, w io.Writer) error {
	reply := func(resp inboundResponse) error {
		b, err := json.Marshal(resp)
		if err != nil {
			return err
		}
		if _, err := w.Write(append(b, '\n')); err != nil {
			return err
		}
		if resp.Error != "" {
			return fmt.Errorf("inbound %s: %s", service, resp.Error)
		}
		return nil
	}
	var req inboundRequest
	// One line, one request. io.LimitReader because the caller is untrusted: a container that
	// never sends a newline must not be able to grow this process without bound.
	dec := json.NewDecoder(io.LimitReader(r, 4<<10))
	if err := dec.Decode(&req); err != nil {
		return reply(inboundResponse{Error: "malformed request"})
	}
	switch req.Verb {
	case VerbServiceStarting:
		detail, err := startingMember(ctx, x, service)
		if err != nil {
			return reply(inboundResponse{Error: err.Error()})
		}
		return reply(inboundResponse{Detail: detail})
	default:
		// Named back, because the realistic cause is a wrapper newer than the agent beneath it
		// and "unknown verb" with the name in it is the difference between a five-minute
		// diagnosis and an afternoon.
		return reply(inboundResponse{Error: fmt.Sprintf("unknown verb %q", req.Verb)})
	}
}

// startingMember takes the ring member for a service that is about to start, or explains why it
// did not. It is stateless by construction: everything it decides, it decides from the ring on
// disk and the manifest on the volume, so a handler process that has never run before reaches the
// same answer as one that has.
func startingMember(ctx context.Context, x Executor, service string) (string, error) {
	if err := safeUnitName(service); err != nil { // the name becomes a path element
		return "", err
	}
	at := time.Now()
	// THE RATE LIMIT IS CHECKED BEFORE ANYTHING IS READ OR WRITTEN, so the cheap refusal stays
	// cheap under exactly the conditions that produce it — a crash loop, or a caller hammering
	// the socket on purpose.
	newest, found, err := newestMember(ctx, x, service)
	if err != nil {
		return "", err
	}
	if found && at.Sub(newest) < plainStartFloor {
		return fmt.Sprintf("a member from %s ago is still current; not taking another", at.Sub(newest).Truncate(time.Second)), nil
	}
	// The manifest is on the volume, node-local to read — which is why this needs no host in the
	// loop, and why it must not have one: the host is not in the start path on a promotion or a
	// crash restart, which is most of what this channel exists to catch.
	raw, err := x.ReadFile(manifestPath(service))
	if err != nil {
		return "", fmt.Errorf("read the running manifest: %w", err)
	}
	meta := quadlet.SnapshotMeta{
		Service:  service,
		Trigger:  quadlet.TriggerStart,
		Title:    service + " starting",
		TakenAt:  at,
		Manifest: string(raw),
	}
	sidecar, err := json.Marshal(meta)
	if err != nil {
		return "", fmt.Errorf("render the member's sidecar: %w", err)
	}
	member := quadlet.SnapshotMember(service, quadlet.TriggerStart, at)
	run := func(name string, args ...string) error { _, err := x.Run(ctx, name, args...); return err }
	if err := takeSnapshot(ctx, x, run, quadlet.DataRoot(service), member, string(sidecar)); err != nil {
		return "", err
	}
	return "took " + path.Base(member), nil
}

// takeSnapshot makes one ring member: the read-only subvolume and the sidecar beside it.
//
// REFUSE A COLLISION; do not resolve it. This used to DELETE an existing destination first, and
// that was right for exactly as long as the name was fixed: one `<service>-preupgrade` per service
// meant the second upgrade found the first one's snapshot sitting there, and `btrfs subvolume
// snapshot` given an existing directory creates the new snapshot INSIDE it -- which on a read-only
// snapshot fails with "Read-only file system". Measured on a soak run 2026-08-28: every upgrade
// after the first failed, the fleet stopped converging, and the error named the filesystem rather
// than the collision it actually was.
//
// ⚠️ THAT DELETE IS FATAL TO A RING ([B.143]). Members are a series now
// (quadlet.SnapshotMember), so they are distinct by construction and nothing legitimately
// supersedes anything. A delete-before-take kept here would silently destroy a member whenever two
// landed in the same second -- losing history inside the one function whose job is keeping it.
// Refusing makes that case loud and leaves the earlier member intact.
//
// THE SIDECAR, AND WHY THE MEMBER GOES IF IT CANNOT BE WRITTEN. It cannot live inside the member
// (read-only from the instant it exists) and so cannot be atomic with it. The picker and the
// restore path both need a member's title and the manifest it was taken under, and an unlabelled
// subvolume is worse than no member at all: it is something a human must identify by hand before
// trusting it with their data. So the invariant is "every member has a sidecar", bought by undoing
// the half-made one.
func takeSnapshot(ctx context.Context, x Executor, run func(string, ...string) error, dataDir, member, sidecar string) error {
	if _, err := x.Run(ctx, "btrfs", "subvolume", "show", member); err == nil {
		return fmt.Errorf("snapshot %s already exists -- refusing to replace a ring member", member)
	}
	if err := run("btrfs", "subvolume", "snapshot", "-r", dataDir, member); err != nil {
		return err
	}
	if sidecar == "" {
		return nil
	}
	if err := x.WriteFile(quadlet.SnapshotSidecar(member), []byte(sidecar)); err != nil {
		if derr := run("btrfs", "subvolume", "delete", member); derr != nil {
			return fmt.Errorf("write snapshot sidecar: %w; AND the unlabelled member could not be removed: %v", err, derr)
		}
		return fmt.Errorf("write snapshot sidecar (the member was removed): %w", err)
	}
	return nil
}

// newestMember is the most recent member of a service's ring, by the timestamp in its NAME.
//
// The name is the index, which is the whole reason quadlet.SnapshotMember puts a fixed-width UTC
// stamp in it: this needs no sidecar read, no parsing of btrfs output and no directory walk in
// timestamp order. A ring of a thousand members costs one listing.
func newestMember(ctx context.Context, x Executor, service string) (time.Time, bool, error) {
	out, err := x.Run(ctx, "ls", "-1", quadlet.SnapshotsDir)
	if err != nil {
		// An absent .snapshots dir is a node whose volume was just made, not a failure: there is
		// no newest member because there are no members.
		return time.Time{}, false, nil
	}
	var names []string
	for _, n := range strings.Fields(string(out)) {
		// The sidecars are named after their members, so they parse as members too. Skipping
		// them by suffix keeps the newest-member answer about SUBVOLUMES, which is what the
		// rate limit is really asking about.
		if strings.HasSuffix(n, ".json") {
			continue
		}
		// Anything whose name we cannot read is somebody else's: a human's copy, a future
		// feature's, a leftover. The ring only counts what it named.
		if svc, ok := quadlet.SnapshotMemberService(n); ok && svc == service {
			names = append(names, n)
		}
	}
	if len(names) == 0 {
		return time.Time{}, false, nil
	}
	sort.Strings(names) // lexical order is chronological order, by construction
	at, ok := quadlet.SnapshotMemberTime(names[len(names)-1])
	if !ok {
		// A member whose name we cannot read is not a reason to refuse to take another: the ring
		// is a convenience, and a stranger's directory entry must not be able to stop it.
		return time.Time{}, false, nil
	}
	return at, true, nil
}

// inboundIdle is how long a handler waits for another connection before exiting. Short, because
// the socket unit survives the handler and systemd starts a new one on the next connection: an
// idle household carries one socket file and no process.
const inboundIdle = 2 * time.Minute

// InboundListener is the listening socket systemd passed us, at the well-known activation fd.
//
// THE CONTRACT IS SYSTEMD'S ($LISTEN_FDS / $LISTEN_PID, sd_listen_fds): fds are handed over
// starting at 3, and LISTEN_PID names the process they were meant for so an fd inherited by some
// grandchild is not mistaken for an activation. We want exactly one, and more than one means the
// unit was edited into something this code does not implement -- worth refusing rather than
// guessing which.
func InboundListener() (net.Listener, error) {
	if pid := os.Getenv("LISTEN_PID"); pid != strconv.Itoa(os.Getpid()) {
		return nil, fmt.Errorf("inbound: not socket-activated (LISTEN_PID=%q, pid=%d)", pid, os.Getpid())
	}
	if n := os.Getenv("LISTEN_FDS"); n != "1" {
		return nil, fmt.Errorf("inbound: want exactly one activation fd, got LISTEN_FDS=%q", n)
	}
	f := os.NewFile(3, "briard-inbound")
	defer f.Close()
	ln, err := net.FileListener(f)
	if err != nil {
		return nil, fmt.Errorf("inbound: adopt the activation fd: %w", err)
	}
	return ln, nil
}

// ServeInboundSocket serves `service`'s inbound socket until ctx ends or it has been idle for
// inboundIdle, then returns.
//
// SEQUENTIAL, ONE CONNECTION AT A TIME, and deliberately so. The work is a btrfs snapshot of one
// service's subvolume; two at once would race the same ring and the collision refusal would turn
// one of them into an error for no reason. The load is a service start, so a queue of one is
// not a bottleneck -- and a caller that opens a connection and says nothing is bounded by the
// read deadline rather than by holding the channel for everyone.
//
// A FAILED REQUEST IS NOT A FAILED SERVER. The caller has been answered either way; the error is
// logged and the next connection is served. Only the listener breaking ends this.
func ServeInboundSocket(ctx context.Context, x Executor, service string, ln net.Listener) error {
	go func() { <-ctx.Done(); ln.Close() }()
	for {
		if err := ln.(*net.UnixListener).SetDeadline(time.Now().Add(inboundIdle)); err != nil {
			return err
		}
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil // asked to stop
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				return nil // idle: let systemd start the next one on demand
			}
			return err
		}
		// The caller is untrusted, so it does not get to hold this open: a container that
		// connects and never writes must not park the channel for the service's next real start.
		_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
		if err := ServeInbound(ctx, x, service, conn, conn); err != nil {
			log.Printf("inbound %s: %v", service, err)
		}
		conn.Close()
	}
}
