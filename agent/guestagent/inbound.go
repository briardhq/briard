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
	"strconv"
	"strings"
	"time"

	"briard.io/agent/quadlet"
	"briard.io/agent/services"
	"briard.io/shared/manifest"
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
		// FROM INSIDE A RUNNING CONTAINER: Home Assistant restarting itself, which is the one
		// boundary nothing outside the container can see -- and one whose previous instance the
		// clean-stop marker says nothing about.
		detail, err := startingMember(ctx, x, service, false)
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
// containerStart says this is the CONTAINER's own start (the rendered unit's pre-start) rather than
// a restart inside a container that stayed up -- the only caller that may read the clean-stop
// marker, since the marker is a claim about the last container stop ([B.143]).
func startingMember(ctx context.Context, x Executor, service string, containerStart bool) (string, error) {
	if err := safeUnitName(service); err != nil { // the name becomes a path element
		return "", err
	}
	at := time.Now()
	// The manifest is on the volume, node-local to read — which is why this needs no host in the
	// loop, and why it must not have one: the host is not in the start path on a promotion or a
	// crash restart, which is most of what this channel exists to catch.
	//
	// IT IS READ BEFORE THE RATE LIMIT because the rate limit does not apply to every member: a
	// titled one is never skipped, and which this is can only be known from the manifest (the
	// registry's marker paths are per service and per container). The cheap refusal therefore
	// costs one directory listing and one small read, and still writes nothing at all — the
	// property that matters when the caller is a crash loop or something hammering the socket.
	raw, err := x.ReadFile(manifestPath(service))
	if err != nil {
		return "", fmt.Errorf("read the running manifest: %w", err)
	}
	// WHAT THE BYTES ARE, and only the container's own start may ask ([B.143]). The marker is a
	// claim about the last STOP, so it belongs to the boundary where the container stopped and
	// started again — not to Home Assistant restarting itself inside a container that never went
	// down, where the previous instance ended the way HA's own restart ends and the marker has
	// nothing to say about it.
	cons, title := quadlet.Quiesced, service+" starting"
	if containerStart {
		if cons = consumeCleanStop(ctx, x, service); cons == quadlet.Crash {
			// A promotion after the other node died, or this node's own power cut. Named for what
			// is known — that nothing shut the service down — rather than for a cause this cannot
			// tell apart.
			title = service + " starting after an unclean stop"
		}
	}
	trigger := quadlet.TriggerStart
	switch backup, phase := restorePhase(ctx, x, service, raw, at); phase {
	case restoreBefore:
		trigger, title = quadlet.TriggerRestoreBefore, "before restoring "+backup
	case restoreAfter:
		trigger, title = quadlet.TriggerRestoreAfter, "after restoring "+backup
	default:
		// THE RATE LIMIT, and only here. A crash loop restarts every few seconds and would fill
		// the picker with hundreds of identical plain members; the one that matters is the first,
		// taken before whatever went wrong ever ran. A titled member is the opposite case — there
		// are two of them at most, minutes apart by construction, and skipping one would leave a
		// household's own restore with no way back.
		newest, found, err := newestMember(ctx, x, service)
		if err != nil {
			return "", err
		}
		if found && at.Sub(newest) < plainStartFloor {
			return fmt.Sprintf("a member from %s ago is still current; not taking another", at.Sub(newest).Truncate(time.Second)), nil
		}
	}
	meta := quadlet.SnapshotMeta{
		Service: service,
		Trigger: trigger,
		Title:   title,
		TakenAt: at,
		// WHAT THE BYTES ARE, derived above rather than assumed here. A container start reads the
		// clean-stop marker, because "nothing is running" and "the data was flushed" are different
		// facts and a promotion after a dead primary is where they come apart; a restart inside a
		// running container has no such question to ask.
		Consistency: cons,
		Manifest:    string(raw),
	}
	sidecar, err := json.Marshal(meta)
	if err != nil {
		return "", fmt.Errorf("render the member's sidecar: %w", err)
	}
	member := quadlet.SnapshotMember(service, trigger, at)
	run := func(name string, args ...string) error { _, err := x.Run(ctx, name, args...); return err }
	if err := takeSnapshot(ctx, x, run, quadlet.DataRoot(service), member, string(sidecar)); err != nil {
		return "", err
	}
	// AFTER the take, never before: pruning first would mean a failed take leaves the ring
	// shorter for nothing, and a ring at its bound is the state each take should restore rather
	// than the state each take should find.
	pruneRing(ctx, x, service, at)
	return "took " + path.Base(member), nil
}

// THE CLEAN-STOP MARKER ([B.143]): the one fact that says whether a service's data was FLUSHED,
// which is the question quadlet.Consistency actually asks and the one a stopped container does not
// answer.
//
// ⚠️ A STOPPED CONTAINER IS NOT THE SAME FACT AS FLUSHED DATA. The member taken as a service
// starts after a promotion has nothing running either — but the old primary never shut the service
// down, so those bytes are whatever it left, and the same holds for the first start after a node's
// own power cut. Labelling those `quiesced` because nothing was running at the take would be the
// field saying the opposite of the truth precisely when it matters.
//
// SO THE STOP WRITES IT AND THE START CONSUMES IT. A container unit that stops cleanly runs
// ExecStopPost with SERVICE_RESULT=success and the marker lands here; a node that dies writes
// nothing. It lives on the REPLICATED volume beside the manifests, never inside the data
// subvolume, because the node that reads it next may not be the node that wrote it — which is
// exactly the failover case. Absent means crash-consistent, which is also what a service starting
// for the first time on a volume somebody else provisioned would see, so provision writes one too.
//
// ⚠️ ITS LIMIT, stated rather than papered over: a clean unit stop is not proof the application
// flushed. podman stops a container with a signal and a timeout, and a workload that ignores both
// is killed while the unit still ends `success`. It is the same evidence the upgrade point has
// claimed since [B.121] — one stop, believed — and it is strictly better than assuming every
// start had one.
func cleanStopPath(service string) string { return manifestDir + "/" + service + ".clean" }

// RecordServiceStop writes that marker, or removes it when the stop was not clean. It is what the
// rendered unit's ExecStopPost calls, and systemd's own SERVICE_RESULT is the evidence.
func RecordServiceStop(ctx context.Context, x Executor, service, result string) error {
	if err := safeUnitName(service); err != nil {
		return err
	}
	if result != "success" {
		// A failed, killed or timed-out stop leaves NO claim behind. Removing rather than leaving
		// whatever was there keeps "absent means unflushed" true after a stop that half-happened.
		_, err := x.Run(ctx, "rm", "-f", cleanStopPath(service))
		return err
	}
	if err := x.WriteFile(cleanStopPath(service), []byte(result+"\n")); err != nil {
		return err
	}
	// Flushed to the DRBD backing for the same reason the manifest is: the node that reads this is
	// the one that promotes after this one goes away, and a claim still sitting in the writeback
	// window is a claim the survivor never sees.
	_, err := x.Run(ctx, "sync", "-f", cleanStopPath(service))
	return err
}

// consumeCleanStop answers "was this service's data flushed by a clean stop", and spends the
// answer: the moment the container runs again the claim stops being true.
func consumeCleanStop(ctx context.Context, x Executor, service string) quadlet.Consistency {
	if _, err := x.ReadFile(cleanStopPath(service)); err != nil {
		return quadlet.Crash
	}
	if _, err := x.Run(ctx, "rm", "-f", cleanStopPath(service)); err != nil {
		log.Printf("ring %s: could not spend the clean-stop marker (%v); the next member may claim more than it should", service, err)
	}
	return quadlet.Quiesced
}

// THE BACKUP-RESTORE PAIR ([B.143]): the two members either side of a household restoring one of
// Home Assistant's OWN backups, which is a different operation from restoring one of our members
// and needs its own pair of points.
//
// THE SEQUENCE IT READS, and why nothing else in the system can read it. The household asks a live
// HA for the restore; HA writes its marker inside /config and exits 100. The wrapper's
// notification lands in the restart that follows and sees the marker — that is the *before* point,
// taken on data HA has not touched yet. HA's restore process then unlinks the marker in a `finally`
// right after parsing and BEFORE the wipe (V3b §6.2, "prevent a boot loop"), wipes /config, extracts
// the tar, and exits 100 again. The second notification sees no marker at all, which is why the
// *after* point cannot be recognised from the volume and needs the node-local fact below.
//
// IT DEGRADES TO A PLAIN START, always. A failover between the two notifications loses the fact
// (it lives in tmpfs), an unfinished restore leaves one that expires, and either way the member is
// an ordinary start member — a pair with one half missing is a smaller loss than a mislabelled
// point, and every other path here keeps that same direction.
type restoreStage int

const (
	restoreNone restoreStage = iota
	restoreBefore
	restoreAfter
)

// restorePendingTTL bounds how long the node-local fact may sit unclaimed. A restore that is going
// to happen takes the time of one HA restart plus a tar extraction; a fact older than this belongs
// to one that never completed, and using it would title an ordinary start hours later as the
// second half of a restore that never happened.
const restorePendingTTL = 6 * time.Hour

func restorePendingPath(service string) string { return "/run/briard/restore-pending." + service }

// restorePhase says which half of a backup restore this start is, if either, and what to call the
// backup. The service name has already been checked as a path element by the caller.
func restorePhase(ctx context.Context, x Executor, service string, rawManifest []byte, at time.Time) (string, restoreStage) {
	m, _, err := manifest.Parse(rawManifest)
	if err != nil {
		return "", restoreNone // a manifest we cannot read tells us nothing about markers
	}
	for _, rel := range services.RestoreMarkers(m) {
		body, err := x.ReadFile(quadlet.DataRoot(service) + "/" + rel)
		if err != nil {
			continue
		}
		name := backupName(body)
		// The fact the second notification will need, since by then the marker is gone. Written
		// best-effort: a member titled as the first half of a pair is right whether or not the
		// second half can be titled at all.
		if err := x.WriteFile(restorePendingPath(service), []byte(fmt.Sprintf("%d\t%s", at.Unix(), name))); err != nil {
			log.Printf("ring %s: could not record the restore in flight (%v); its second point will read as a plain start", service, err)
		}
		return name, restoreBefore
	}
	body, err := x.ReadFile(restorePendingPath(service))
	if err != nil {
		return "", restoreNone
	}
	// Consumed on the way in, whatever it says: a fact left behind would title the NEXT start as
	// the second half of a restore too.
	if _, err := x.Run(ctx, "rm", "-f", restorePendingPath(service)); err != nil {
		log.Printf("ring %s: could not clear the restore fact (%v)", service, err)
	}
	stamp, name, ok := strings.Cut(strings.TrimSpace(string(body)), "\t")
	if !ok {
		return "", restoreNone
	}
	secs, err := strconv.ParseInt(stamp, 10, 64)
	if err != nil || at.Sub(time.Unix(secs, 0)) > restorePendingTTL {
		return "", restoreNone
	}
	return name, restoreAfter
}

// backupName is what the picker calls the backup, read out of HA's marker.
//
// BEST-EFFORT AND SANITISED, because the content is written by the service rather than by us: the
// format has changed upstream before (a bare path, then JSON carrying one), and it lands in a line
// an operator reads. So: the path under a "path" key if it parses as JSON, else the whole body if
// it looks like one, reduced to its base name, stripped of anything unprintable and capped. A
// marker we cannot read at all still titles the pair — "a backup" is the honest answer, and the
// timestamps either side say which one it was.
func backupName(body []byte) string {
	raw := strings.TrimSpace(string(body))
	var fields struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(body, &fields); err == nil && fields.Path != "" {
		raw = fields.Path
	}
	name := path.Base(raw)
	var b strings.Builder
	for _, r := range name {
		if r < 0x20 || r == 0x7f || b.Len() >= 80 {
			continue
		}
		b.WriteRune(r)
	}
	if name = b.String(); name == "" || name == "." || name == "/" || strings.HasPrefix(name, "{") {
		return "a backup"
	}
	return "backup " + name
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

// ringMembers lists one service's members, oldest first.
//
// The NAME IS THE INDEX, which is the whole reason quadlet.SnapshotMember puts a fixed-width UTC
// stamp in it: this needs no sidecar read, no parsing of btrfs output and no walk in timestamp
// order. A ring of a thousand members costs one listing and a sort.
func ringMembers(ctx context.Context, x Executor, service string) []string {
	out, err := x.Run(ctx, "ls", "-1", quadlet.SnapshotsDir)
	if err != nil {
		// An absent .snapshots dir is a node whose volume was just made, not a failure: there
		// are no members because there is nothing to hold them.
		return nil
	}
	var names []string
	for _, n := range strings.Fields(string(out)) {
		// The sidecars are named after their members, so they parse as members too. Skipping
		// them by suffix keeps this about SUBVOLUMES, which is what both callers mean.
		if strings.HasSuffix(n, ".json") {
			continue
		}
		// Anything whose name we cannot read is somebody else's: a human's copy, a future
		// feature's, a leftover. The ring counts — and deletes — only what it named.
		if svc, ok := quadlet.SnapshotMemberService(n); ok && svc == service {
			names = append(names, n)
		}
	}
	// ⚠️ SORT ON THE PARSED TIME, NOT THE NAME. A member is `<service>-<trigger>-<stamp>`, so the
	// TRIGGER sits between the service and the stamp and dominates any string comparison: every
	// `-start-` member sorts before every `-upgrade-` one whatever their times, and once a
	// `-daily-` trigger exists it sorts before both. Sorting names put the newest member wherever
	// the alphabet happened to put its trigger -- which made newestMember answer with an upgrade
	// point's age, so the rate limit compared against the wrong member on any service that had
	// ever been upgraded.
	//
	// The stamp is fixed-width UTC so that TIMES compare correctly once parsed; that was always
	// the property, and "lexical order is chronological" was only ever true within one trigger.
	sort.Slice(names, func(i, j int) bool {
		ti, _ := quadlet.SnapshotMemberTime(names[i])
		tj, _ := quadlet.SnapshotMemberTime(names[j])
		return ti.Before(tj)
	})
	return names
}

// newestMember is the most recent member of a service's ring, by the timestamp in its NAME.
func newestMember(ctx context.Context, x Executor, service string) (time.Time, bool, error) {
	names := ringMembers(ctx, x, service)
	if len(names) == 0 {
		return time.Time{}, false, nil
	}
	at, ok := quadlet.SnapshotMemberTime(names[len(names)-1])
	if !ok {
		// A member whose name we cannot read is not a reason to refuse to take another: the ring
		// is a convenience, and a stranger's directory entry must not be able to stop it.
		return time.Time{}, false, nil
	}
	return at, true, nil
}

// pruneRing brings a service's ring back to what quadlet's retention ladder keeps — the three
// windows, which is where the policy and its reasoning live. This is the enforcement, and it is
// the GUEST's because the host is not in the start path on a promotion or a crash restart.
//
// THE CLOCK COMES FROM THE CALLER, the same instant the take used. A prune that asked the clock
// again would be answering a question one call later than the one it was asked.
//
// BEST-EFFORT, ALWAYS. The caller is holding a household's service stopped waiting for an answer,
// so a member that will not delete is logged and stepped over — a ring one member too long is
// nothing; a service that would not start because a delete failed is an outage.
//
// The sidecar goes with its member, and in that order: a member with no sidecar is the state the
// take path refuses to leave behind, so the delete must not create one either.
func pruneRing(ctx context.Context, x Executor, service string, now time.Time) {
	for _, n := range quadlet.RetentionPrune(ringMembers(ctx, x, service), now) {
		member := quadlet.SnapshotsDir + n
		if _, err := x.Run(ctx, "btrfs", "subvolume", "delete", member); err != nil {
			log.Printf("ring %s: could not prune %s: %v", service, n, err)
			continue
		}
		if _, err := x.Run(ctx, "rm", "-f", quadlet.SnapshotSidecar(member)); err != nil {
			log.Printf("ring %s: pruned %s but left its sidecar: %v", service, n, err)
		}
	}
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
	ensureToolsOnPath()
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

// ToolsBin is the image's tool profile, for a caller that must put it on PATH itself.
//
// The generic pre-start hook is that caller ([B.143]): it is an ExecStartPre on a unit podman's
// quadlet generator writes, and teaching the RENDERER about the image's profile would give a pure
// function of the manifest a second thing to know. The entry point sets its own PATH instead, so
// the rendered line stays one absolute path and a service name.
func ToolsBin() string { return toolsBin() }

// TakeStartMember takes the ring member for a service that is about to start, for a caller
// outside this package. The inbound channel reaches the same code through its own verb; this is
// the entry point the generic pre-start hook uses, on a node where nothing is inside a container
// yet to ask.
func TakeStartMember(ctx context.Context, x Executor, service string) (string, error) {
	ensureToolsOnPath()
	return startingMember(ctx, x, service, true)
}

// ensureToolsOnPath puts the image's tool profile on this process's PATH.
//
// ⚠️ THE RING SHELLS OUT TO btrfs, so every entry point that can take a member needs the profile
// — and not every one of them is started by a unit that sets it. In the product the listener runs
// inside `run --guest`, whose unit does; the generic pre-start hook is an ExecStartPre on a unit
// the RENDERER writes, which deliberately carries no profile (agent/quadlet); and an agent-less
// rig starts the listener on its own. Measured by the third of those, on L0 2026-09-22:
// "inbound home-assistant: exec: \"btrfs\": executable file not found in $PATH" — the channel
// worked end to end, the token resolved, and the take failed on a missing tool.
//
// So the code that needs the tools puts them there, rather than every caller remembering to.
// Prepended, never replacing: a caller that already set a good PATH keeps it.
func ensureToolsOnPath() {
	if tools := ToolsBin(); !strings.Contains(os.Getenv("PATH"), tools) {
		os.Setenv("PATH", tools+":"+os.Getenv("PATH"))
	}
}

// listMembers is one service's ring as a reader sees it: every member, with the sidecar beside
// it, oldest first.
//
// A MEMBER WITH NO READABLE SIDECAR IS SKIPPED, not reported half-formed. The take path removes a
// member it could not label, so one here means something outside the ring made it -- a human's
// copy, an interrupted older build -- and the picker must not offer a household a rollback point
// whose code identity nobody knows.
func listMembers(ctx context.Context, x Executor, service string) ([]quadlet.SnapshotEntry, error) {
	if err := safeUnitName(service); err != nil {
		return nil, err
	}
	var out []quadlet.SnapshotEntry
	for _, n := range ringMembers(ctx, x, service) {
		member := quadlet.SnapshotsDir + n
		raw, err := x.ReadFile(quadlet.SnapshotSidecar(member))
		if err != nil {
			log.Printf("ring %s: %s has no readable sidecar; not offering it", service, n)
			continue
		}
		var meta quadlet.SnapshotMeta
		if err := json.Unmarshal(raw, &meta); err != nil {
			log.Printf("ring %s: %s has an unreadable sidecar (%v); not offering it", service, n, err)
			continue
		}
		out = append(out, quadlet.SnapshotEntry{Member: member, Meta: meta})
	}
	return out, nil
}
