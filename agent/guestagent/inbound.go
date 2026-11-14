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
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	apps "briard.io/agent/apps"
	"briard.io/agent/quadlet"
	"briard.io/shared/manifest"
)

// The inbound channel: the first thing in this system that lets an app's container ask the guest
// agent for something. Everything else briard does to an app goes the other way — files written
// into a stopped container, HTTP dialled at a running one — and the asymmetry was not an
// accident, so reversing it for one feature deserves its own file and its own rules.
//
// It has to exist because the generic snapshot hook fires at container start, the only boundary
// the guest agent can see from outside, while Home Assistant restarts itself far more often than
// its container does — `homeassistant.restart` exits 100 and s6 re-runs the service — and each
// of those is a stopped window where a snapshot is application-consistent. The container unit
// stays active throughout, so there is no transition to poll for and no signal to subscribe to:
// only the process inside knows, and it needs a way to say so and to be told when it may proceed.
//
// ⚠️ The caller is untrusted, the rule the whole design hangs off. The socket is bind-mounted into
// the app's container, so everything running as that app can reach it — for Home Assistant, every
// custom component the household ever installed from HACS. Without this channel a compromised
// Home Assistant could not reach the guest agent at all. So:
//
//   - The caller does not name itself. It presents a token minted per app at converge and mounted
//     read-only into that app's container alone, and the agent maps that back to a name. An `app`
//     field in the request would be one a hostile custom component could set; a token it can only
//     hold if it was given one.
//   - No verb names a path. Every path is derived from the resolved app (quadlet.DataRoot,
//     quadlet.SnapshotMember); a verb taking a path reads or writes anywhere the agent can.
//   - No verb chooses what is destroyed or rewritten. The ring prunes itself and records events
//     after every take, but both are derived from the ring on disk; restoring and deleting on
//     request stay on the host's side, where the caller is the product.
//   - Every verb is bounded. A start replaces the start still pending before it (replacePending),
//     so a hostile or looping caller holds one pending member however often it calls, and cannot
//     fill the replicated volume — the full-volume failure arriving by a new road. Such a caller
//     costs a take and a delete per call, one call at a time (ListenInbound).
//
// One request, one response, then the connection is done: no session and no state carried
// between calls, every answer derived from the ring on disk and the manifest on the volume.
//
// ⚠️ One socket and a per-app token, not a socket per app. Per-app sockets would make identity a
// property of the transport, which is genuinely stronger — but they also make the listener set a
// function of the app list, which only converge knows, and converge runs in two processes (the
// host's app.converge verb and briard-apps' one-shot). That forces systemd to own the binds —
// template units, a converge step to start instances, socket activation, fd inheritance, an
// idle-exit handler — and puts the logic in a short-lived process while everything it reasons
// about lives in the long-running agent: a great deal of machinery to buy a property a per-app
// secret already provides (ListenInbound).

// InboundVerb is a request this channel accepts. The set is closed and deliberately tiny; read
// the trust rules above before adding to it.
type InboundVerb string

const (
	// VerbAppStarting says "my app is about to start, and I am holding it until you
	// answer". The wait is the point: the caller blocks before handing over to the real
	// entrypoint, so whatever the agent does happens while the app is genuinely stopped.
	VerbAppStarting InboundVerb = "app.starting"
)

// inboundRequest is one call. It carries a token and never an app name, and the difference is
// the whole trust story (the file header's first rule): the token was minted for one app and
// written where only that app's container can read it, so a caller cannot name itself — it can
// only present something it was given.
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

// ServeInbound reads one request from r, resolves who sent it, serves it, and writes one response
// to w. An error is both reported to the caller and returned, because the two readers differ: the
// caller logs it and carries on (it must never fail a household's app over this), while the
// return value is what reaches the agent's journal for a human to read afterwards.
func ServeInbound(ctx context.Context, x Executor, r io.Reader, w io.Writer) error {
	var app string
	reply := func(resp inboundResponse) error {
		b, err := json.Marshal(resp)
		if err != nil {
			return err
		}
		if _, err := w.Write(append(b, '\n')); err != nil {
			return err
		}
		if resp.Error != "" {
			return fmt.Errorf("inbound %s: %s", app, resp.Error)
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
	// Who is calling, before what they want. Resolution is the only thing that stands between a
	// workload and this channel, so it happens once, up front, and nothing below it can be
	// reached without it. The refusal deliberately says nothing about which tokens exist.
	app, ok := resolveCaller(ctx, x, req.Token)
	if !ok {
		return reply(inboundResponse{Error: "unknown caller"})
	}
	switch req.Verb {
	case VerbAppStarting:
		// From inside a running container: Home Assistant restarting itself, which is the one
		// boundary nothing outside the container can see -- and one whose previous instance the
		// clean-stop marker says nothing about.
		defer signalProxy(ctx, x)
		detail, err := startingMember(ctx, x, app, false)
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

// startingMember takes the ring member for an app that is about to start, or explains why it
// did not. It is stateless by construction: everything it decides, it decides from the ring on
// disk and the manifest on the volume, so a handler process that has never run before reaches the
// same answer as one that has.
//
// containerStart says this is the container's own start (the rendered unit's pre-start) rather than
// a restart inside a container that stayed up -- the only caller that may read the clean-stop
// marker, since the marker is a claim about the last container stop.
func startingMember(ctx context.Context, x Executor, app string, containerStart bool) (string, error) {
	if err := safeUnitName(app); err != nil { // the name becomes a path element
		return "", err
	}
	at := time.Now()
	// The manifest is on the volume, node-local to read — which is why this needs no host in the
	// loop, and why it must not have one: the host is not in the start path on a promotion or a
	// crash restart, which is most of what this channel exists to catch.
	raw, err := x.ReadFile(manifestPath(app))
	if err != nil {
		return "", fmt.Errorf("read the running manifest: %w", err)
	}
	// What the bytes are, and only the container's own start may ask: the marker is a claim
	// about the last stop, so it belongs to the boundary where the container stopped and started
	// again — not to Home Assistant restarting itself inside a container that never went down,
	// where the previous instance ended the way HA's own restart ends and the marker has nothing
	// to say about it.
	cons := quadlet.Quiesced
	if containerStart {
		// A promotion after the other node died, or this node's own power cut: nothing shut the
		// app down, and the member says so.
		cons = consumeCleanStop(ctx, x, app)
	}
	trigger := quadlet.TriggerStart
	var ev *quadlet.Event
	if backup, ok := hassRestore(x, app, raw); ok {
		trigger = quadlet.TriggerHassRestoreBefore
		// The household's own restore is an event, on the point that undoes it.
		ev = &quadlet.Event{At: at, Reasons: []quadlet.Reason{{Kind: quadlet.ReasonHassRestore, What: "Restored " + backup}}}
	}
	meta := quadlet.SnapshotMeta{
		App:     app,
		Trigger: trigger,
		TakenAt: at,
		// What the bytes are, derived above rather than assumed here: "nothing is running" and
		// "the data was flushed" are different facts, and a promotion after a dead primary is
		// where they come apart.
		Consistency: cons,
		Manifest:    string(raw),
		Event:       ev,
		// A start waits for its boot's verdict before it is evaluated (recordMember).
		Pending: trigger == quadlet.TriggerStart,
	}
	sidecar, err := json.Marshal(meta)
	if err != nil {
		return "", fmt.Errorf("render the member's sidecar: %w", err)
	}
	member := quadlet.SnapshotMember(app, trigger, at)
	run := func(name string, args ...string) error { _, err := x.Run(ctx, name, args...); return err }
	if err := takeSnapshot(ctx, x, run, quadlet.DataRoot(app), member, string(sidecar)); err != nil {
		return "", err
	}
	recordMember(ctx, x, member, meta)
	return "took " + path.Base(member), nil
}

// ringMu serialises the ring's in-process writers: the verb handlers (the host's takes, a
// restore, the recorder's restore), the inbound listener (a container's start) and the start
// evaluator's ticker all settle, prune or swap the same members, and the serve loop runs verbs
// beside each other and beside those two goroutines. Taken at the three funnels -- recordMember,
// evaluatePending, the restore swap -- never inside them. The `--app-starting` ExecStartPre
// subprocess stays outside it: best-effort by design, it cannot share a lock with this process.
var ringMu sync.Mutex

// recordMember is what every take does once its member exists: evaluate it, add any reasons that
// finds to the member they land on, let its app hold it, and prune. One function for every way a
// member is taken (a start, the host's data.member, the quiesced clock sample, the recorder
// restore's undo point), so the history cannot depend on which door a sample came through.
//
// A start is not evaluated here: what its boot reveals is part of its evaluation, and the
// pre-start hook cannot wait for a boot. It is written pending, replaces the start still pending
// before it, and EvaluateStarts evaluates it once the boot has a verdict. Every other sample is
// evaluated now.
//
// It runs after the take, never before: pruning first would mean a failed take leaves the ring
// shorter for nothing, and a member is only replaceable once its successor exists. Best-effort,
// like the prune: the caller may be holding a household's app stopped, and a row missing from
// its history is a smaller loss than an app that would not start.
func recordMember(ctx context.Context, x Executor, member string, meta quadlet.SnapshotMeta) {
	ringMu.Lock()
	defer ringMu.Unlock()
	if meta.Pending {
		replacePending(ctx, x, member, meta)
	} else if members, err := listMembers(ctx, x, meta.App); err != nil {
		log.Printf("ring %s: could not read the ring to record %s: %v", meta.App, path.Base(member), err)
	} else {
		settle(ctx, x, members, quadlet.SnapshotEntry{Member: member, Meta: meta}, nil, meta.TakenAt)
	}
	sampled(ctx, x, member, meta)
	pruneRing(ctx, x, meta.App, meta.TakenAt)
}

// sampled gives the member's app its say (apps.Sampled) before the prune, so a hold it places
// counts there.
//
// Not for the recorder restore's own sample: it holds the database the check just found damaged,
// which is no restore candidate.
func sampled(ctx context.Context, x Executor, member string, meta quadlet.SnapshotMeta) {
	if meta.Trigger == quadlet.TriggerHassDBRestoreBefore {
		return
	}
	m, _, err := manifest.Parse([]byte(meta.Manifest))
	if err != nil {
		return // a member whose manifest we cannot read tells us nothing about its app
	}
	members, err := listMembers(ctx, x, meta.App)
	if err != nil {
		return
	}
	ring := func(e quadlet.SnapshotEntry) apps.RingMember {
		at, _ := quadlet.SnapshotMemberTime(e.Member)
		return apps.RingMember{Path: e.Member, App: quadlet.AppSidecar(e.Member), At: at, Quiesced: e.Meta.Consistency == quadlet.Quiesced}
	}
	// The member as the ring holds it, not as the caller took it: replacing a pending start can
	// carry a weaker consistency onto it, and the hold may trust only what the sidecar says.
	var all []apps.RingMember
	var self *apps.RingMember
	for _, e := range members {
		all = append(all, ring(e))
		if e.Member == member {
			self = &all[len(all)-1]
		}
	}
	if self == nil {
		return
	}
	if err := apps.Sampled(ctx, x, m, all, *self); err != nil {
		log.Printf("ring %s: %s's app could not record it: %v", meta.App, path.Base(member), err)
	}
}

// replacePending deletes the starts still pending before a new one. A start with no verdict yet
// stands for a boot that never finished, so the new start's boot is the one its verdict is about,
// and content is compared from the last evaluated sample to the new start. An evaluated start is
// never replaced: "change something, restart" keeps its row. This is what bounds a crash loop,
// and a caller hammering the socket: one pending member per app, however often it starts.
//
// The replaced boots' renames are carried: a rename the replaced boot made is in the new member
// already, so the new boot has nothing left to rename and its verdict would find nothing. What is
// new here over the replaced member goes into the new sidecar's Resets, to land with the verdict.
// So is a crash-consistent label (below).
//
// ⚠️ A delete, gated on a positive read: the sidecar parsed, and it says a pending
// start with no event. listMembers leaves out what it cannot read, so an unreadable member is
// kept. The carried renames are written first, and a failed write keeps the old members.
func replacePending(ctx context.Context, x Executor, member string, meta quadlet.SnapshotMeta) {
	members, err := listMembers(ctx, x, meta.App)
	if err != nil {
		log.Printf("ring %s: could not read the ring to replace a pending start: %v", meta.App, err)
		return
	}
	at, ok := quadlet.SnapshotMemberTime(member)
	if !ok {
		return
	}
	var old []quadlet.SnapshotEntry
	for _, m := range members {
		t, ok := quadlet.SnapshotMemberTime(m.Member)
		if ok && t.Before(at) && m.Meta.Pending && m.Meta.Trigger == quadlet.TriggerStart && m.Meta.Event == nil {
			old = append(old, m)
		}
	}
	if len(old) == 0 {
		return
	}
	changed := false
	if mf, _, err := manifest.Parse([]byte(meta.Manifest)); err == nil {
		now := apps.Corrupt(ctx, x, mf, member)
		carried := meta.Resets
		for _, o := range old {
			carried = union(carried, o.Meta.Resets, without(now, apps.Corrupt(ctx, x, mf, o.Member)))
		}
		if len(carried) > len(meta.Resets) {
			meta.Resets, changed = carried, true
		}
	}
	// The weaker consistency is carried too. A Home Assistant container start takes two members
	// seconds apart: the unit's pre-start, which reads the clean-stop marker, and the `run`
	// wrapper's, which cannot and says quiesced. After a promotion or a power cut the first says
	// crash, and the second holds the same bytes, so keeping it must not upgrade the claim.
	for _, o := range old {
		if o.Meta.Consistency == quadlet.Crash && meta.Consistency != quadlet.Crash {
			meta.Consistency, changed = quadlet.Crash, true
		}
	}
	if changed && !writeMeta(ctx, x, member, meta, "carried what the replaced starts knew") {
		return
	}
	for _, o := range old {
		if _, err := x.Run(ctx, "btrfs", "subvolume", "delete", o.Member); err != nil {
			log.Printf("ring %s: could not replace the pending %s: %v", meta.App, path.Base(o.Member), err)
			continue
		}
		if _, err := x.Run(ctx, "rm", "-f", quadlet.SnapshotSidecar(o.Member), quadlet.AppSidecar(o.Member)); err != nil {
			log.Printf("ring %s: replaced %s but left its sidecar: %v", meta.App, path.Base(o.Member), err)
		}
		log.Printf("ring %s: %s replaced the pending %s", meta.App, path.Base(member), path.Base(o.Member))
	}
}

// without is a minus b, in a's order.
func without(a, b []string) []string {
	var out []string
	for _, s := range a {
		if !slices.Contains(b, s) {
			out = append(out, s)
		}
	}
	return out
}

// union is every string in the lists, once, first appearance first.
func union(lists ...[]string) []string {
	var out []string
	for _, l := range lists {
		for _, s := range l {
			if !slices.Contains(out, s) {
				out = append(out, s)
			}
		}
	}
	return out
}

// settle is one sample's evaluation, whoever calls it: it closes the older samples still pending
// as replaced, registers what the evaluation finds (plus `extra`, a start's health reason), and
// records the sample as evaluated with the health it carries forward. A crash loop is one
// evaluation: the pending starts before this sample are closed without being evaluated, and this
// one is compared with the newest evaluated sample, so what the loop's first start brought in is
// still found, and its reasons land on the last point before all of it.
func settle(ctx context.Context, x Executor, members []quadlet.SnapshotEntry, e quadlet.SnapshotEntry, extra []quadlet.Reason, at time.Time) {
	// The baseline first, while the loop's starts still read as pending: closed, they would look
	// evaluated, and the newest of them would be what this sample is compared with.
	prev, hasPrev := previousEvaluated(members, e.Member)
	eAt, _ := quadlet.SnapshotMemberTime(e.Member)
	for i, m := range members {
		if t, ok := quadlet.SnapshotMemberTime(m.Member); ok && m.Meta.Pending && t.Before(eAt) {
			m.Meta.Pending = false
			writeMeta(ctx, x, m.Member, m.Meta, "closed as replaced")
			members[i] = m
		}
	}
	was := e.Meta
	if e.Meta.Health == apps.HealthUnknown && hasPrev {
		e.Meta.Health = prev.Meta.Health // carried forward (SnapshotMeta.Health)
	}
	e.Meta.Pending = false
	if e.Meta.Pending != was.Pending || e.Meta.Health != was.Health {
		writeMeta(ctx, x, e.Member, e.Meta, "evaluated")
	}
	if !hasPrev {
		return
	}
	if point, reasons := evaluate(ctx, x, members, prev, e.Member, e.Meta, extra); len(reasons) > 0 {
		addReasons(ctx, x, point, reasons, at)
	}
}

// previousEvaluated is the newest member taken before this one that is not still pending.
func previousEvaluated(members []quadlet.SnapshotEntry, member string) (quadlet.SnapshotEntry, bool) {
	at, ok := quadlet.SnapshotMemberTime(member)
	if !ok {
		return quadlet.SnapshotEntry{}, false
	}
	var prev quadlet.SnapshotEntry
	var prevAt time.Time
	for _, m := range members {
		t, ok := quadlet.SnapshotMemberTime(m.Member)
		if ok && !m.Meta.Pending && m.Member != member && t.Before(at) && t.After(prevAt) {
			prev, prevAt = m, t
		}
	}
	return prev, prev.Member != ""
}

// evaluate is a new member's one evaluation: the member its reasons land on, and the
// reasons, or none when nothing is registered.
//
// Compare only if the app ran between the two. The previous member is never a *-before sample
// when they are compared: the app was stopped for it and did not run again until the next start.
// A *-before member itself is compared with the one before it, so edits made just before an
// update are caught, and its finding lands on that earlier member rather than on the operation's
// own.
//
// A comparison can find a change and a reset at once, and a start's evaluation may add its health
// (`extra`); all happened in the same interval, so all are reasons on the same point. Nothing
// found, at a member that is not an operation's own point, is quiet time (quadlet.QuietPoint). A
// start's boot resets are its own (meta.Resets): read after the boot, so they are not in the
// member and are counted whatever prev is. The earlier side's are not found again: they were its
// boot's, already landed.
func evaluate(ctx context.Context, x Executor, members []quadlet.SnapshotEntry, prev quadlet.SnapshotEntry, member string, meta quadlet.SnapshotMeta, extra []quadlet.Reason) (quadlet.SnapshotEntry, []quadlet.Reason) {
	var found []quadlet.Reason
	var resets []string
	if !prev.Meta.Trigger.Before() {
		if m, _, err := manifest.Parse([]byte(meta.Manifest)); err == nil {
			// The clock's sample is the one taken while the app runs.
			changed, rs, err := apps.Detect(ctx, x, m, prev.Member, member, meta.Trigger == quadlet.TriggerClock)
			if err != nil {
				log.Printf("ring %s: could not compare %s with %s: %v", meta.App, path.Base(prev.Member), path.Base(member), err)
			}
			if changed != "" {
				found = append(found, quadlet.Reason{Kind: quadlet.ReasonChanged, What: changed})
			}
			resets = without(rs, prev.Meta.Resets)
		}
	}
	if resets = union(resets, meta.Resets); len(resets) > 0 {
		found = append(found, quadlet.Reason{Kind: quadlet.ReasonReset, What: apps.ResetPhrase(resets)})
	}
	if found = append(found, extra...); len(found) > 0 {
		return prev, found
	}
	if meta.Event != nil {
		return quadlet.SnapshotEntry{}, nil
	}
	if point, ok := quadlet.QuietPoint(members, member); ok {
		return point, []quadlet.Reason{{Kind: quadlet.ReasonQuiet}}
	}
	return quadlet.SnapshotEntry{}, nil
}

// addReasons adds reasons to an existing member's event, or gives it one. `at` is the
// evaluation's time, which becomes the event's creation time if it is new.
//
// A quiet event never shares its record, so quiet is only ever added to a member with no event.
func addReasons(ctx context.Context, x Executor, point quadlet.SnapshotEntry, rs []quadlet.Reason, at time.Time) {
	meta := point.Meta
	var kinds []string
	for _, r := range rs {
		if r.Kind == quadlet.ReasonQuiet && meta.Event != nil {
			continue
		}
		meta.Event = meta.Event.With(r, at)
		kinds = append(kinds, string(r.Kind))
	}
	if len(kinds) > 0 {
		writeMeta(ctx, x, point.Member, meta, "recorded "+strings.Join(kinds, "+"))
	}
}

// writeMeta replaces an existing member's sidecar; `what` names the change for the log. It says
// whether the sidecar now holds meta, for the one caller that must not go on otherwise
// (replacePending).
//
// ⚠️ A replace, never a rewrite in place: a sidecar truncated by a power cut is a member nobody
// can describe, which the ring then neither offers nor prunes. So tmp + rename, and `sync -f`
// because the next node to read this may be the one that promotes after this one dies.
func writeMeta(ctx context.Context, x Executor, member string, meta quadlet.SnapshotMeta, what string) bool {
	b, err := json.Marshal(meta)
	if err != nil {
		log.Printf("ring %s: could not render %s's sidecar: %v", meta.App, path.Base(member), err)
		return false
	}
	sidecar := quadlet.SnapshotSidecar(member)
	tmp := sidecar + ".tmp"
	if err := x.WriteFile(tmp, b); err != nil {
		log.Printf("ring %s: %s on %s failed: %v", meta.App, what, path.Base(member), err)
		return false
	}
	if _, err := x.Run(ctx, "mv", "-f", tmp, sidecar); err != nil {
		log.Printf("ring %s: %s on %s failed: %v", meta.App, what, path.Base(member), err)
		return false
	}
	if _, err := x.Run(ctx, "sync", "-f", sidecar); err != nil {
		log.Printf("ring %s: %s on %s but could not flush it: %v", meta.App, what, path.Base(member), err)
		return false
	}
	return true
}

// startEvalTick is how often the long-running agent looks for a start whose boot has a verdict.
const startEvalTick = 10 * time.Second

// EvaluateStarts evaluates each pending start sample once its boot has a verdict, until
// ctx ends. It runs in the long-running agent, beside the inbound listener, because the pre-start
// hook that takes a start sample cannot wait for the boot it precedes. Everything it decides is
// read from the ring and the volume, so a restart of this process loses nothing: a pending sample
// is still pending on disk.
func EvaluateStarts(ctx context.Context, x Executor) {
	ensureToolsOnPath()
	t := time.NewTicker(startEvalTick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		out, err := x.Run(ctx, "ls", "-1", manifestDir)
		if err != nil {
			continue // no volume mounted here: this node serves nothing
		}
		for _, n := range strings.Fields(string(out)) {
			if app, ok := strings.CutSuffix(n, ".json"); ok && safeUnitName(app) == nil {
				evaluatePending(ctx, x, app, time.Now())
			}
		}
	}
}

// evaluatePending evaluates an app's newest pending start if its boot has a verdict: healthy as
// soon as it is, anything else once apps.HealthGate has passed since the start. A booting app
// refuses connections and answers 5xx, so nothing but healthy is conclusive early.
//
// Unhealthy registers only on a transition: the newest evaluated sample carried healthy (or no
// verdict yet). The reason lands on the sample before the start, which is what undoing it
// restores. So does a boot's reset: what the app set aside as undecodable during this boot is in
// the live data and not in the start's member, which holds the undecodable original. Undoing to
// the start would put that file back and the next boot would set it aside again, so the reset is
// the boot's verdict on the start's data, like health.
func evaluatePending(ctx context.Context, x Executor, app string, now time.Time) {
	ringMu.Lock()
	defer ringMu.Unlock()
	members, err := listMembers(ctx, x, app)
	if err != nil {
		return
	}
	var pending quadlet.SnapshotEntry
	for _, m := range members { // oldest first
		if m.Meta.Pending {
			pending = m
		}
	}
	if pending.Member == "" {
		return
	}
	h, err := appHealth(ctx, x, app)
	if err != nil {
		h = apps.HealthUnknown
	}
	if h != apps.Healthy && now.Sub(pending.Meta.TakenAt) < apps.HealthGate {
		return
	}
	h = h.Settled() // HealthGate has passed: still starting is not working
	var extra []quadlet.Reason
	if prev, ok := previousEvaluated(members, pending.Member); ok && h == apps.Unhealthy && prev.Meta.Health != apps.Unhealthy {
		extra = append(extra, quadlet.Reason{Kind: quadlet.ReasonUnhealthy})
	}
	pending.Meta.Health = h
	if m, _, err := manifest.Parse([]byte(pending.Meta.Manifest)); err == nil {
		live := apps.Corrupt(ctx, x, m, quadlet.DataRoot(app))
		pending.Meta.Resets = union(pending.Meta.Resets, without(live, apps.Corrupt(ctx, x, m, pending.Member)))
	}
	settle(ctx, x, members, pending, extra, now)
	pruneRing(ctx, x, app, now)
}

// The clean-stop marker: the one fact that says whether an app's data was flushed, which is the
// question quadlet.Consistency actually asks and the one a stopped container does not answer.
//
// ⚠️ A stopped container is not the same fact as flushed data. The member taken as an app starts
// after a promotion has nothing running either — but the old primary never shut the app down, so
// those bytes are whatever it left, and the same holds for the first start after a node's own
// power cut. Labelling those `quiesced` because nothing was running at the take would be the
// field saying the opposite of the truth precisely when it matters.
//
// So the stop writes it and the start consumes it: a container unit that stops cleanly runs
// ExecStopPost with SERVICE_RESULT=success and the marker lands here; a node that dies writes
// nothing. It lives on the replicated volume beside the manifests, never inside the data
// subvolume, because the node that reads it next may not be the one that wrote it — exactly the
// failover case. Absent means crash-consistent, which is also what an app starting for the first
// time on a volume somebody else provisioned would see, so provision writes one too.
//
// ⚠️ Its limit: a clean unit stop is not proof the application flushed. podman stops a container
// with a signal and a timeout, and a workload that ignores both is killed while the unit still
// ends `success`. It is the same evidence the upgrade point has claimed — one stop, believed —
// and strictly better than assuming every start had one.
func cleanStopPath(app string) string { return manifestDir + "/" + app + ".clean" }

// RecordAppStop writes that marker, or removes it when the stop was not clean. It is what the
// rendered unit's ExecStopPost calls, and systemd's own SERVICE_RESULT is the evidence.
func RecordAppStop(ctx context.Context, x Executor, app, result string) error {
	ensureToolsOnPath() // `sync` and `rm`: the rendered unit's ExecStopPost carries no tool profile
	if err := safeUnitName(app); err != nil {
		return err
	}
	if result != "success" {
		// A failed, killed or timed-out stop leaves no claim behind. Removing rather than leaving
		// whatever was there keeps "absent means unflushed" true after a stop that half-happened.
		_, err := x.Run(ctx, "rm", "-f", cleanStopPath(app))
		return err
	}
	if err := x.WriteFile(cleanStopPath(app), []byte(result+"\n")); err != nil {
		return err
	}
	// Flushed to the DRBD backing for the same reason the manifest is: the node that reads this is
	// the one that promotes after this one goes away, and a claim still sitting in the writeback
	// window is a claim the survivor never sees.
	_, err := x.Run(ctx, "sync", "-f", cleanStopPath(app))
	return err
}

// consumeCleanStop answers "was this app's data flushed by a clean stop", and spends the
// answer: the moment the container runs again the claim stops being true.
func consumeCleanStop(ctx context.Context, x Executor, app string) quadlet.Consistency {
	if _, err := x.ReadFile(cleanStopPath(app)); err != nil {
		return quadlet.Crash
	}
	if _, err := x.Run(ctx, "rm", "-f", cleanStopPath(app)); err != nil {
		log.Printf("ring %s: could not spend the clean-stop marker (%v); the next member may claim more than it should", app, err)
	}
	return quadlet.Quiesced
}

// The backup restore: a household restoring one of Home Assistant's own backups, a different
// operation from undoing to one of our members, with its own *-before sample.
//
// The sequence it reads: the household asks a live HA for the restore; HA writes its marker
// inside /config and exits 100. The wrapper's notification lands in the restart that follows and
// sees the marker: that is the hass-restore-before sample, taken on data HA has not touched yet.
// HA's restore process then unlinks the marker right after parsing and before the wipe (its own
// "prevent a boot loop" step), wipes /config, extracts the tar, and exits 100 again. The second
// notification sees no marker and is an ordinary start, which is not compared with the *-before
// sample: the app did not run between them, the restore did.
//
// hassRestore says whether this start is that first restart, and what to call the backup. The
// app name has already been checked as a path element by the caller.
func hassRestore(x Executor, app string, rawManifest []byte) (string, bool) {
	m, _, err := manifest.Parse(rawManifest)
	if err != nil {
		return "", false // a manifest we cannot read tells us nothing about markers
	}
	for _, rel := range apps.RestoreMarkers(m) {
		if body, err := x.ReadFile(quadlet.DataRoot(app) + "/" + rel); err == nil {
			return backupName(body), true
		}
	}
	return "", false
}

// backupName is what the history calls the backup, read out of HA's marker. Best-effort and
// sanitised, because the content is written by the app rather than by us: the format has changed
// upstream before (a bare path, then JSON carrying one), and it lands in a line an operator
// reads. So: the path under a "path" key if it parses as JSON, else the whole body if
// it looks like one, reduced to its base name, stripped of anything unprintable and capped. A
// marker we cannot read at all still names the restore — "a backup" is the honest answer, and the
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

// takeSnapshot makes one ring member: the read-only subvolume and the sidecar beside it. It
// refuses a collision rather than resolving it: members are a series (quadlet.SnapshotMember),
// distinct by construction, so nothing legitimately supersedes anything.
//
// ⚠️ A delete-before-take is fatal to a ring: it would silently destroy a member whenever two
// landed in the same second -- losing history inside the one function whose job is keeping it.
// Taking over an existing one is no better: `btrfs subvolume snapshot` given an existing
// directory creates the new snapshot inside it, which on a read-only member fails with
// "Read-only file system" -- an error naming the filesystem rather than the collision. Refusing
// makes the case loud and leaves the earlier member intact.
//
// The sidecar cannot live inside the member (read-only from the instant it exists), so it cannot
// be atomic with it. The history and the restore path both need a member's event and the manifest
// it was taken under, and an unlabelled subvolume is worse than no member at all: something a
// human must identify by hand before trusting it with their data. So the invariant is "every
// member has a sidecar", bought by removing the member when its sidecar cannot be written.
func takeSnapshot(ctx context.Context, x Executor, run func(string, ...string) error, dataDir, member, sidecar string) error {
	if _, err := x.Run(ctx, "btrfs", "subvolume", "show", member); err == nil {
		return fmt.Errorf("snapshot %s already exists -- refusing to replace a ring member", member)
	}
	group, err := areaGroup(ctx, x, dataDir)
	if err != nil {
		return err
	}
	if err := run("btrfs", "subvolume", "snapshot", "-r", "-i", group, dataDir, member); err != nil {
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

// ringMembers lists one app's members, oldest first.
//
// The name is the index: quadlet.SnapshotMember puts the app and a fixed-width UTC stamp in it,
// so this needs no sidecar read and no parsing of btrfs output. A ring of a thousand members
// costs one listing and a sort.
func ringMembers(ctx context.Context, x Executor, app string) []string {
	out, err := x.Run(ctx, "ls", "-1", quadlet.SnapshotsDir)
	if err != nil {
		// An absent .snapshots dir is a node whose volume was just made, not a failure: there
		// are no members because there is nothing to hold them.
		return nil
	}
	var names []string
	for _, n := range strings.Fields(string(out)) {
		// The sidecars are named after their members, so they parse as members too. Skipping
		// them by suffix keeps this about subvolumes, which is what both callers mean.
		if strings.HasSuffix(n, ".json") {
			continue
		}
		// Anything whose name we cannot read is somebody else's: a human's copy, a future
		// feature's, a leftover. The ring counts — and deletes — only what it named.
		if svc, ok := quadlet.SnapshotMemberApp(n); ok && svc == app {
			names = append(names, n)
		}
	}
	// ⚠️ Sort on the parsed time, not the name. A member is `<app>-<trigger>-<stamp>`, so the
	// trigger sits between the app and the stamp and dominates any string comparison: every
	// `-clock-` member sorts before every `-start-` one whatever their times, and every reader that
	// asks for "the newest" or "the one before" would get the alphabet's answer. Lexical order is
	// chronological only within one trigger.
	sort.Slice(names, func(i, j int) bool {
		ti, _ := quadlet.SnapshotMemberTime(names[i])
		tj, _ := quadlet.SnapshotMemberTime(names[j])
		return ti.Before(tj)
	})
	return names
}

// pruneRing brings an app's ring back to what the history keeps (quadlet.RetentionPrune, where
// the policy and its reasoning live). This is the enforcement, and it is the guest's because the
// host is not in the start path on a promotion or a crash restart.
//
// It reads the sidecars, because what a member is kept for is the event it anchors; a member
// whose sidecar cannot be read is not listed, so it is never pruned — the ring deletes only what
// it can describe. The clock comes from the caller, the same instant the take used: a prune that
// asked the clock again would be answering a question one call later than the one it was asked.
//
// Best-effort, always: the caller is holding a household's app stopped waiting for an answer, so
// a member that will not delete is logged and stepped over — a ring one member too long is
// nothing; an app that would not start because a delete failed is an outage. Both sidecars go
// with their member, and after it: a member with no sidecar is the state the take path refuses to
// leave behind, so the delete must not create one either.
func pruneRing(ctx context.Context, x Executor, app string, now time.Time) {
	members, err := listMembers(ctx, x, app)
	if err != nil {
		log.Printf("ring %s: could not read the ring to prune it: %v", app, err)
		return
	}
	for _, member := range quadlet.RetentionPrune(members, now) {
		n := path.Base(member)
		if _, err := x.Run(ctx, "btrfs", "subvolume", "delete", member); err != nil {
			log.Printf("ring %s: could not prune %s: %v", app, n, err)
			continue
		}
		if _, err := x.Run(ctx, "rm", "-f", quadlet.SnapshotSidecar(member), quadlet.AppSidecar(member)); err != nil {
			log.Printf("ring %s: pruned %s but left its sidecar: %v", app, n, err)
		}
	}
}

// ListenInbound binds the one inbound socket and serves it until ctx ends. It runs in the
// long-running agent, which the per-app token makes possible: the identity is carried by the
// request, so the listener needs to know nothing in advance (the file header says why not a
// socket per app). One socket, bound unconditionally at agent start, whether this node runs zero
// apps or five.
//
// ⚠️ The socket dies with this process, and the agent restarts when the host link drops (the
// unit's Restart=always, one host connection per run). So a container starting in the seconds
// around a host reconnect finds nothing listening, gets a refused connection, and starts without
// a member -- the `|| true` case: the cost of the listener living here, bounded and benign.
func ListenInbound(ctx context.Context, x Executor) error {
	ensureToolsOnPath()
	if err := os.MkdirAll(apps.RunDir(), 0o755); err != nil {
		return fmt.Errorf("inbound: %w", err)
	}
	// A socket left by a previous run is not a listener -- bind would fail with EADDRINUSE on a
	// path nothing is serving. Removing it is safe precisely because /run is tmpfs and this
	// process is the only thing that ever binds here.
	if err := os.Remove(apps.InboundSocket()); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("inbound: clear stale socket: %w", err)
	}
	ln, err := net.Listen("unix", apps.InboundSocket())
	if err != nil {
		return fmt.Errorf("inbound: listen: %w", err)
	}
	// 0660 and the mount, not one or the other. The bind mount is what puts this in front of a
	// container at all; the mode is what keeps every other reader on the node off it. Neither is
	// the authentication -- that is the token -- but a channel that acts on a household's data
	// should not be reachable by anything that merely knows the path.
	if err := os.Chmod(apps.InboundSocket(), 0o660); err != nil {
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
		// One at a time, deliberately: the work is a btrfs snapshot of one app's subvolume, two at
		// once would race the same ring and turn one of them into a collision refusal for no
		// reason, and the load is an app start. The caller is untrusted, so it does not get to
		// hold this open: a container that connects and says nothing is bounded by the deadline
		// rather than by parking the channel for every other app's next start.
		_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
		if err := ServeInbound(ctx, x, conn, conn); err != nil {
			log.Printf("inbound: %v", err)
		}
		conn.Close()
	}
}

// resolveCaller maps a request's token to the app that was given it. An empty or absent token
// resolves to nothing: there is no anonymous caller.
//
// The directory name is the mapping. Each app has one directory named for it, holding everything
// briard hands that app -- the token among them -- and mounted read-only into its container, so
// resolving a caller is reading one directory listing, and the agent needs no table, no cache and
// no notification when an app arrives on a node that was never told about it: the property the
// per-app socket design could not have at any price.
//
// Constant time, and not as a ritual: the caller can retry as fast as it likes against a secret
// this process holds, which is the shape a comparison timing leak is actually exploitable in.
func resolveCaller(ctx context.Context, x Executor, token string) (string, bool) {
	// A short token is refused before anything is read. It cannot be one of ours -- they are 32
	// random bytes, hex -- and this keeps an empty or missing token from ever walking the
	// directory, which is the shape a caller would probe with.
	if len(token) < 32 {
		return "", false
	}
	out, err := x.Run(ctx, "ls", "-1", apps.RunDir())
	if err != nil {
		return "", false
	}
	// The run directory holds plenty that is not an app — sockets, json, the drbd drop-in
	// dirs. Nothing needs to tell them apart: an entry with no readable token inside it is
	// simply not a candidate, so the filter is the read itself.
	for _, name := range strings.Fields(string(out)) {
		want, err := x.ReadFile(apps.InboundTokenPath(name))
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
// The generic pre-start hook is that caller: it is an ExecStartPre on a unit podman's
// quadlet generator writes, and teaching the renderer about the image's profile would give a pure
// function of the manifest a second thing to know. The entry point sets its own PATH instead, so
// the rendered line stays one absolute path and an app name.
func ToolsBin() string { return toolsBin() }

// TakeStartMember takes the ring member for an app that is about to start, for a caller
// outside this package. The inbound channel reaches the same code through its own verb; this is
// the entry point the generic pre-start hook uses, on a node where nothing is inside a container
// yet to ask. The same start is when the app's cache is emptied, if its space is short.
func TakeStartMember(ctx context.Context, x Executor, app string) (string, error) {
	ensureToolsOnPath()
	defer signalProxy(ctx, x)
	if done, err := cacheAtStart(ctx, x, app); err != nil {
		log.Printf("app-starting %s: the cache: %v", app, err)
	} else if done != "" {
		log.Printf("app-starting %s: %s", app, done)
	}
	return startingMember(ctx, x, app, true)
}

// signalProxy tells the reverse proxy an app is starting, so it asks again whether that app
// accepts X-Forwarded-For from it: an app reads its proxy trust at start, so a start is the only
// moment the answer can change. Both start boundaries come through here -- the container's own
// (TakeStartMember) and Home Assistant restarting inside it (the inbound verb) -- whatever the
// member's outcome.
//
// Best-effort, like the take: a reverse proxy that is not running yet (a promotion starts apps
// before the reverse proxy) asks at its own start instead, and a missed recheck costs client
// addresses, never a request.
func signalProxy(ctx context.Context, x Executor) {
	if _, err := x.Run(ctx, "systemctl", "kill", "--kill-whom=main", "--signal=SIGHUP", reverseProxyUnit); err != nil {
		log.Printf("signal the reverse proxy to recheck forwarded-header trust: %v", err)
	}
}

// ensureToolsOnPath puts the image's tool profile on this process's PATH: the code that needs the
// tools puts them there, rather than every caller remembering to. Prepended, never replacing: a
// caller that already set a good PATH keeps it.
//
// ⚠️ The ring shells out to btrfs, so every entry point that can take a member needs the profile
// — and not every one of them is started by a unit that provides it. In the product the listener
// runs inside `run --guest`, whose unit carries btrfs on its PATH; the generic pre-start hook is
// an ExecStartPre on a unit the renderer writes, which deliberately carries no profile
// (agent/quadlet); and an agent-less rig starts the listener on its own. Without this the channel
// works end to end, the token resolves, and the take fails with `exec: "btrfs": executable file
// not found in $PATH`.
func ensureToolsOnPath() {
	if tools := ToolsBin(); !strings.Contains(os.Getenv("PATH"), tools) {
		os.Setenv("PATH", tools+":"+os.Getenv("PATH"))
	}
}

// listMembers is one app's ring as a reader sees it: every member, with the sidecar beside it,
// oldest first. A member with no readable sidecar is skipped, not reported half-formed: the take
// path removes a member it could not label, so one here means something outside the ring made
// it -- a human's copy, an interrupted older build -- and the history must not offer a household
// a rollback point whose code identity nobody knows.
func listMembers(ctx context.Context, x Executor, app string) ([]quadlet.SnapshotEntry, error) {
	if err := safeUnitName(app); err != nil {
		return nil, err
	}
	var out []quadlet.SnapshotEntry
	for _, n := range ringMembers(ctx, x, app) {
		member := quadlet.SnapshotsDir + n
		raw, err := x.ReadFile(quadlet.SnapshotSidecar(member))
		if err != nil {
			log.Printf("ring %s: %s has no readable sidecar; not offering it", app, n)
			continue
		}
		var meta quadlet.SnapshotMeta
		if err := json.Unmarshal(raw, &meta); err != nil {
			log.Printf("ring %s: %s has an unreadable sidecar (%v); not offering it", app, n, err)
			continue
		}
		_, err = x.ReadFile(quadlet.AppSidecar(member))
		out = append(out, quadlet.SnapshotEntry{Member: member, Meta: meta, App: err == nil})
	}
	return out, nil
}
