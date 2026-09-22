package guestagent

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path"
	"sort"
	"strings"
	"time"

	"briard.io/agent/quadlet"
	"briard.io/agent/services"
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
//   - THE CALLER DOES NOT NAME ITSELF. It presents a TOKEN minted per service at converge and
//     mounted read-only into that service's container alone, and the agent maps that back to a
//     name. A `service` field in the request would be a field a hostile custom component could
//     set; a token it can only hold if it was given one.
//   - NO VERB NAMES A PATH. Every path is derived from the resolved service (quadlet.DataRoot,
//     quadlet.SnapshotMember). A verb taking a path is a verb that reads or writes anywhere the
//     agent can.
//   - NO VERB DESTROYS ANYTHING. Pruning, restoring and deleting stay on the host's side of the
//     channel, where the caller is the product rather than the workload.
//   - EVERY VERB IS BOUNDED. The rate limit below is not only picker hygiene: it is what stops a
//     hostile or looping caller from filling the replicated volume, which is [B.155]'s failure
//     arriving by a new road.
//
// ONE REQUEST, ONE RESPONSE, THEN THE CONNECTION IS DONE. No session and no state carried
// between calls: every answer is derived from the ring on disk and the manifest on the volume.
//
// ⚠️ THE FIRST BUILD GAVE EACH SERVICE ITS OWN SOCKET, and the token is what replaced it
// (2026-09-22, the owner's call). Per-service sockets made identity a property of the transport,
// which is genuinely stronger — but they also made the LISTENER SET a function of the service
// list, and the only thing that knows that list is converge, which runs in two processes (the
// host's verb and drbd-reactor's one-shot). That forced systemd to own the binds: template units,
// a converge step to start instances, socket activation, fd inheritance, an idle-exit handler —
// and it put the logic in a short-lived process while everything that logic reasons about lives
// in the long-running agent. A great deal of machinery downstream of one decision, buying a
// property a per-service secret already provides.

// InboundVerb is a request this channel accepts. The set is closed and deliberately tiny; read
// the trust rules above before adding to it.
type InboundVerb string

const (
	// VerbServiceStarting says "my service is about to start, and I am holding it until you
	// answer". The WAIT is the point: the caller blocks before handing over to the real
	// entrypoint, so whatever the agent does happens while the service is genuinely stopped.
	VerbServiceStarting InboundVerb = "service.starting"
)

// inboundRequest is one call.
//
// It carries a TOKEN and never a service name, and the difference is the whole trust story: the
// token was minted for one service and written where only that service's container can read it,
// so a caller cannot name itself — it can only present something it was given. A `service` field
// here would be a field a hostile custom component could set.
type inboundRequest struct {
	Verb  InboundVerb `json:"verb"`
	Token string      `json:"token"`
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

// ServeInbound reads one request from r, resolves who sent it, serves it, and writes one response
// to w.
//
// An error is reported to the caller AND returned, because the two readers are different: the
// caller logs it and carries on (it must never fail a household's service over this), while the
// return value is what reaches the agent's journal for a human to read afterwards.
func ServeInbound(ctx context.Context, x Executor, r io.Reader, w io.Writer) error {
	var service string
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
	// WHO IS CALLING, BEFORE WHAT THEY WANT. Resolution is the only thing that stands between a
	// workload and this channel, so it happens once, up front, and nothing below it can be
	// reached without it. The refusal deliberately says nothing about which tokens exist.
	service, ok := resolveCaller(ctx, x, req.Token)
	if !ok {
		return reply(inboundResponse{Error: "unknown caller"})
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

// ListenInbound binds the one inbound socket and serves it until ctx ends.
//
// IT RUNS IN THE LONG-RUNNING AGENT, which is the whole of the correction the token made
// possible ([B.143], 2026-09-22). The first build gave each service its own socket, which made a
// caller's identity a property of the transport -- and made the listener SET a function of the
// service list, which only converge knows, which runs in two processes, which forced systemd to
// own the binds: template units, socket activation, fd inheritance, a per-connection process. All
// of it downstream of one decision, and it left the logic in a short-lived process while
// everything that logic reasons about lives here.
//
// With a per-service token the identity is carried by the request and the listener needs to know
// nothing in advance. One socket, bound unconditionally at agent start, whether this node runs
// zero services or five.
//
// ⚠️ THE SOCKET DIES WITH THIS PROCESS, and the agent restarts when the host link drops (the
// unit's Restart=always, one host connection per run). So a container starting in the seconds
// around a host reconnect finds nothing listening, gets a refused connection, and starts without
// a member -- the `|| true` case. That is the cost of moving the listener here, it is bounded and
// benign, and it is worth stating rather than discovering.
func ListenInbound(ctx context.Context, x Executor) error {
	if err := os.MkdirAll(services.RunDir(), 0o755); err != nil {
		return fmt.Errorf("inbound: %w", err)
	}
	// A socket left by a previous run is not a listener -- bind would fail with EADDRINUSE on a
	// path nothing is serving. Removing it is safe precisely because /run is tmpfs and this
	// process is the only thing that ever binds here.
	if err := os.Remove(services.InboundSocket()); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("inbound: clear stale socket: %w", err)
	}
	ln, err := net.Listen("unix", services.InboundSocket())
	if err != nil {
		return fmt.Errorf("inbound: listen: %w", err)
	}
	// 0660 AND THE MOUNT, not one or the other. The bind mount is what puts this in front of a
	// container at all; the mode is what keeps every other reader on the node off it. Neither is
	// the authentication -- that is the token -- but a channel that acts on a household's data
	// should not be reachable by anything that merely knows the path.
	if err := os.Chmod(services.InboundSocket(), 0o660); err != nil {
		return fmt.Errorf("inbound: %w", err)
	}
	go func() { <-ctx.Done(); ln.Close() }()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		// ONE AT A TIME, deliberately. The work is a btrfs snapshot of one service's subvolume;
		// two at once would race the same ring and turn one of them into a collision refusal for
		// no reason. The load is a service start.
		//
		// The caller is untrusted, so it does not get to hold this open: a container that
		// connects and says nothing is bounded by the deadline rather than by parking the
		// channel for every other service's next start.
		_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
		if err := ServeInbound(ctx, x, conn, conn); err != nil {
			log.Printf("inbound: %v", err)
		}
		conn.Close()
	}
}

// resolveCaller maps a request's token to the service that was given it.
//
// THE DIRECTORY NAME IS THE MAPPING. Each service has one directory named for it, holding
// everything briard hands that service -- the token among them -- and mounted read-only into its
// container. So resolving a caller is reading one directory listing, and the agent needs no
// table, no cache and no notification when a service arrives on a node that was never told about
// it. That last property is what the per-service socket design could not have at any price.
//
// CONSTANT TIME, and not as a ritual: the caller can retry as fast as it likes against a secret
// this process holds, which is the shape a comparison timing leak is actually exploitable in.
//
// An empty or absent token resolves to nothing. There is no anonymous caller.
func resolveCaller(ctx context.Context, x Executor, token string) (string, bool) {
	// A short token is refused before anything is read. It cannot be one of ours -- they are 32
	// random bytes, hex -- and this keeps an empty or missing token from ever walking the
	// directory, which is the shape a caller would probe with.
	if len(token) < 32 {
		return "", false
	}
	out, err := x.Run(ctx, "ls", "-1", services.RunDir())
	if err != nil {
		return "", false
	}
	// The run directory holds plenty that is not a service — sockets, json, the drbd drop-in
	// dirs. Nothing needs to tell them apart: an entry with no readable token inside it is
	// simply not a candidate, so the filter is the read itself.
	for _, name := range strings.Fields(string(out)) {
		want, err := x.ReadFile(services.InboundTokenPath(name))
		if err != nil {
			continue
		}
		if subtle.ConstantTimeCompare([]byte(strings.TrimSpace(string(want))), []byte(token)) == 1 {
			return name, true
		}
	}
	return "", false
}
