package quadlet

import (
	"testing"
	"time"

	"briard.io/agent/services"
)

// The history's fixtures. `now` is the moment the prune is asked about, and every
// member is named by how long before it it was taken, so each case reads as the calendar question
// it is.
var retentionNow = time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)

const day = 24 * time.Hour

// sample is a plain member taken `ago` before now.
func sample(trigger Trigger, ago time.Duration) SnapshotEntry {
	at := retentionNow.Add(-ago)
	return SnapshotEntry{
		Member: SnapshotMember("home-assistant", trigger, at),
		Meta:   SnapshotMeta{Service: "home-assistant", Trigger: trigger, TakenAt: at},
	}
}

// anchor is a member that is the restore point of an event created `after` it, with one reason.
func anchor(trigger Trigger, ago time.Duration, kind ReasonKind, after time.Duration, what string) SnapshotEntry {
	e := sample(trigger, ago)
	e.Meta.Event = &Event{At: e.Meta.TakenAt.Add(after), Reasons: []Reason{{Kind: kind, What: what}}}
	return e
}

func pruned(t *testing.T, members ...SnapshotEntry) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, n := range RetentionPrune(members, retentionNow) {
		out[n] = true
	}
	return out
}

// TestRetentionReplacesASampleThatAnchorsNothing: sampling often costs nothing a household can
// see, because a sample with no event on it is replaced by the next one -- however young it is.
// The newest stays: it is the next comparison's other half.
func TestRetentionReplacesASampleThatAnchorsNothing(t *testing.T) {
	ev := anchor(TriggerStart, 3*time.Hour, ReasonChanged, time.Hour, "Changed automations")
	s0 := sample(TriggerClock, 150*time.Minute)
	older, newest := sample(TriggerStart, 2*time.Hour), sample(TriggerStart, time.Hour)
	got := pruned(t, ev, s0, older, newest)
	if !got[older.Member] {
		t.Errorf("a sample anchoring no event survived its successor")
	}
	if got[newest.Member] {
		t.Errorf("the newest sample was pruned; the next sample is compared with it")
	}
}

// TestRetentionKeepsTheQuietStart: S₀, the first sample after the last event, is what quiet time
// will put its restore point on, so it survives its successors until then -- and only until then.
func TestRetentionKeepsTheQuietStart(t *testing.T) {
	ev := anchor(TriggerAppUpdateBefore, 4*time.Hour, ReasonAppUpdate, 0, "Updated to 2026.9.0")
	s0, mid, newest := sample(TriggerStart, 4*time.Hour-time.Minute), sample(TriggerClock, 2*time.Hour), sample(TriggerClock, time.Hour)
	got := pruned(t, ev, s0, mid, newest)
	if got[s0.Member] {
		t.Error("the start of the quiet stretch was pruned before quiet time could use it")
	}
	if !got[mid.Member] {
		t.Error("a sample inside the stretch survived; only its start is needed")
	}

	// Once quiet sits on S₀, nothing more is held for the stretch.
	s0.Meta.Event = &Event{At: retentionNow.Add(-time.Hour), Reasons: []Reason{{Kind: ReasonQuiet}}}
	later := sample(TriggerClock, time.Minute)
	if got := pruned(t, ev, s0, mid, newest, later); !got[newest.Member] || got[s0.Member] {
		t.Errorf("pruned %v; want the samples after the quiet point and never the point itself", got)
	}

	// A fresh install has no event: its first sample is S₀.
	first := sample(TriggerStart, 3*time.Hour)
	if got := pruned(t, first, sample(TriggerClock, 2*time.Hour), sample(TriggerClock, time.Hour)); got[first.Member] {
		t.Error("a new ring's first sample was pruned before quiet time could use it")
	}
}

// TestRetentionKeepsTheNewestEvenWhenOld: a service nobody touches for a week still has a
// baseline. Age never removes the newest member.
func TestRetentionKeepsTheNewestEvenWhenOld(t *testing.T) {
	only := sample(TriggerClock, 30*day)
	if pruned(t, only)[only.Member] {
		t.Error("the only member was pruned for its age")
	}
}

// TestRetentionKeepsEventsForFiveDays: every event stays undoable for five days, measured from
// the EVENT's creation time rather than its point's.
func TestRetentionKeepsEventsForFiveDays(t *testing.T) {
	young := anchor(TriggerStart, 4*day, ReasonChanged, time.Hour, "Changed automations")
	old := anchor(TriggerStart, 6*day, ReasonChanged, time.Hour, "Changed automations")
	edge := anchor(TriggerStart, 5*day+30*time.Minute, ReasonQuiet, time.Hour, "") // event 4d23h30m ago
	got := pruned(t, old, edge, young, sample(TriggerStart, time.Minute))
	if got[young.Member] {
		t.Error("a four-day-old event lost its restore point")
	}
	if got[edge.Member] {
		t.Error("the age was measured from the point rather than from the event")
	}
	if !got[old.Member] {
		t.Error("a six-day-old event survived the five-day window")
	}
}

// TestRetentionKeepsTheLastUpdateForAFortnight: "the update broke my house" is discovered days
// later, so the most recent update -- and only it -- outlives the five days.
func TestRetentionKeepsTheLastUpdateForAFortnight(t *testing.T) {
	superseded := anchor(TriggerAppUpdateBefore, 12*day, ReasonAppUpdate, 0, "Updated to 2026.8.0")
	last := anchor(TriggerAppUpdateBefore, 10*day, ReasonAppUpdate, 0, "Updated to 2026.9.0")
	got := pruned(t, superseded, last, sample(TriggerStart, time.Minute))
	if got[last.Member] {
		t.Error("the most recent update was pruned at ten days")
	}
	if !got[superseded.Member] {
		t.Error("a superseded update kept the fortnight; only the last one has it")
	}
	ancient := anchor(TriggerAppUpdateBefore, 20*day, ReasonAppUpdate, 0, "Updated to 2026.7.0")
	if !pruned(t, ancient, sample(TriggerStart, time.Minute))[ancient.Member] {
		t.Error("a twenty-day-old update survived the fortnight")
	}
}

// TestRetentionHasNoCount: a crash loop is a run of samples with nothing between them, so it costs
// one member however long it runs -- and an afternoon of real changes keeps every one of them.
func TestRetentionHasNoCount(t *testing.T) {
	var ring []SnapshotEntry
	for i := 40; i > 0; i-- {
		ring = append(ring, anchor(TriggerStart, time.Duration(i)*10*time.Minute, ReasonChanged, 5*time.Minute, "Changed configuration"))
	}
	ring = append(ring, sample(TriggerStart, time.Minute))
	if got := pruned(t, ring...); len(got) != 0 {
		t.Errorf("pruned %d of an afternoon's events; the history has no count", len(got))
	}
	loop := []SnapshotEntry{anchor(TriggerAppUpdateBefore, 3000*time.Minute, ReasonAppUpdate, 0, "Updated to 2026.9.0")}
	for i := 1000; i > 0; i-- {
		loop = append(loop, sample(TriggerStart, time.Duration(i)*2*time.Minute))
	}
	// The update, S₀ and the newest.
	if got := pruned(t, loop...); len(got) != 998 {
		t.Errorf("a crash loop of 1000 samples kept %d; it should cost the stretch's start and the newest", 1000-len(got))
	}
}

// TestRetentionOrdersByTimeNotName: the trigger sits between the service and the stamp, so by
// NAME every `-start-` member sorts after every `-clock-` one. An old start member with no event
// must not be kept as though it were the newest.
func TestRetentionOrdersByTimeNotName(t *testing.T) {
	ev := anchor(TriggerClock, 3*day, ReasonChanged, time.Hour, "Changed automations")
	s0 := sample(TriggerClock, 2*day+time.Hour)
	oldStart := sample(TriggerStart, 2*day)
	newest := sample(TriggerClock, time.Minute)
	got := pruned(t, newest, oldStart, s0, ev)
	if !got[oldStart.Member] || got[newest.Member] {
		t.Fatalf("pruned %v; want the old start member -- anything else ranked by NAME", got)
	}
}

// TestRetentionPrunesOldestFirst: an interrupted prune has still dropped the least useful ones.
func TestRetentionPrunesOldestFirst(t *testing.T) {
	ev := anchor(TriggerStart, 5*time.Hour, ReasonChanged, time.Hour, "Changed automations")
	s0 := sample(TriggerStart, 4*time.Hour)
	a, b, c := sample(TriggerStart, 3*time.Hour), sample(TriggerStart, 2*time.Hour), sample(TriggerStart, time.Hour)
	got := RetentionPrune([]SnapshotEntry{b, c, ev, a, s0}, retentionNow)
	if len(got) != 2 || got[0] != a.Member || got[1] != b.Member {
		t.Errorf("pruned %v, want [%s %s]", got, a.Member, b.Member)
	}
}

// TestRetentionNeverTouchesWhatItCannotName: `.snapshots` is shared with whatever a human or a
// future feature put there. The ring deletes only what it named.
func TestRetentionNeverTouchesWhatItCannotName(t *testing.T) {
	stranger := SnapshotEntry{Member: SnapshotsDir + "someones-copy-before-i-tried-something"}
	got := pruned(t, stranger, sample(TriggerStart, 9*day), sample(TriggerStart, time.Minute))
	if got[stranger.Member] {
		t.Errorf("the prune returned a name it cannot parse: %v", got)
	}
}

// TestQuietPointAtTheStartOfTheStretch: the first quiet sample QuietAfter past S₀ puts quiet on S₀,
// and one earlier does not.
func TestQuietPointAtTheStartOfTheStretch(t *testing.T) {
	ev := anchor(TriggerStart, 10*time.Hour, ReasonChanged, time.Hour, "Changed automations")
	s0 := sample(TriggerClock, 9*time.Hour)
	prev := sample(TriggerClock, 5*time.Hour)
	ring := []SnapshotEntry{ev, s0, prev}

	early := SnapshotMember("home-assistant", TriggerClock, retentionNow.Add(-5*time.Hour+time.Minute))
	if p, ok := QuietPoint(ring, early); ok {
		t.Errorf("a sample 4h after S₀ registered quiet on %s", p.Member)
	}
	now := SnapshotMember("home-assistant", TriggerClock, retentionNow)
	if p, ok := QuietPoint(append(ring, SnapshotEntry{Member: now}), now); !ok || p.Member != s0.Member {
		t.Errorf("quiet landed on %q (ok=%t), want S₀ %s", p.Member, ok, s0.Member)
	}
	// A new ring: S₀ is its first sample.
	if p, ok := QuietPoint([]SnapshotEntry{s0, prev}, now); !ok || p.Member != s0.Member {
		t.Errorf("a ring with no event put quiet on %q (ok=%t), want its first sample", p.Member, ok)
	}
}

// TestQuietPointEveryDay: after the stretch's first quiet point, one more a QuietEvery later, on
// the sample BEFORE the one evaluated, so a finding at the next sample never lands on it.
func TestQuietPointEveryDay(t *testing.T) {
	q := anchor(TriggerClock, 30*time.Hour, ReasonQuiet, 5*time.Hour, "")
	prev := sample(TriggerClock, time.Hour)
	ring := []SnapshotEntry{q, prev}
	if p, ok := QuietPoint(ring, SnapshotMember("home-assistant", TriggerClock, retentionNow.Add(-8*time.Hour))); ok {
		t.Errorf("a sample 22h after the quiet point started another on %s", p.Member)
	}
	now := SnapshotMember("home-assistant", TriggerClock, retentionNow)
	if p, ok := QuietPoint(ring, now); !ok || p.Member != prev.Member {
		t.Errorf("the day's quiet point landed on %q (ok=%t), want the sample before %s", p.Member, ok, prev.Member)
	}
	// Right after an event, nothing: the event's own point is the sample before.
	op := anchor(TriggerAppUndoBefore, time.Hour, ReasonAppUndo, 0, "Undid changes")
	if p, ok := QuietPoint([]SnapshotEntry{q, op}, now); ok {
		t.Errorf("quiet shared a record with an undo on %s", p.Member)
	}
}

// TestEventWithKeepsItsCreationTime: a later reason joins the record and never moves its time.
func TestEventWithKeepsItsCreationTime(t *testing.T) {
	at := retentionNow.Add(-time.Hour)
	ev := (*Event)(nil).With(Reason{Kind: ReasonChanged, What: "Changed automations"}, at)
	ev2 := ev.With(Reason{Kind: ReasonChanged, What: "Added Hue"}, retentionNow)
	if !ev2.At.Equal(at) || len(ev2.Reasons) != 2 || len(ev.Reasons) != 1 {
		t.Errorf("With gave %+v from %+v; want the creation time kept and the original untouched", ev2, ev)
	}
}

// TestHistoryIsOrderedByRestorePoint is the invariant: undoing a row undoes exactly the rows above
// it, which holds only if the list is ordered by the points the rows put back.
func TestHistoryIsOrderedByRestorePoint(t *testing.T) {
	update := anchor(TriggerAppUpdateBefore, 3*time.Hour, ReasonAppUpdate, 0, "Updated to 2026.9.0")
	change := anchor(TriggerStart, 2*time.Hour, ReasonChanged, 30*time.Minute, "Changed automations")
	plain := sample(TriggerStart, time.Hour)
	rows := History([]SnapshotEntry{plain, change, update}, time.UTC)
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want the two events and no row for a plain sample: %+v", len(rows), rows)
	}
	if rows[0].Point.Member != change.Member || rows[1].Point.Member != update.Member {
		t.Errorf("rows are not newest first by point: %s, %s", rows[0].What, rows[1].What)
	}
	if !rows[0].At.Equal(change.Meta.Event.At) {
		t.Errorf("a row shows %s, want the event's creation time %s", rows[0].At, change.Meta.Event.At)
	}
}

// TestHistoryTitlesFromReasons: a row's words are its reasons', and quiet reads as the span until
// the next event.
func TestHistoryTitlesFromReasons(t *testing.T) {
	both := anchor(TriggerStart, 30*time.Hour, ReasonChanged, time.Hour, "Changed automations")
	both.Meta.Event = both.Meta.Event.With(Reason{Kind: ReasonChanged, What: "Added Hue"}, retentionNow)
	quiet := anchor(TriggerClock, 28*time.Hour, ReasonQuiet, 5*time.Hour, "")
	update := anchor(TriggerAppUpdateBefore, 3*time.Hour, ReasonAppUpdate, 0, "Updated to 2026.9.0")
	lastQuiet := anchor(TriggerStart, 2*time.Hour, ReasonQuiet, 5*time.Hour, "")
	rows := History([]SnapshotEntry{both, quiet, update, lastQuiet}, time.UTC)
	want := []string{"Running normally", "Updated to 2026.9.0", "Ran normally until Wed 09:00", "Changed automations; Added Hue"}
	if len(rows) != len(want) {
		t.Fatalf("got %d rows, want %d", len(rows), len(want))
	}
	for i, w := range want {
		if rows[i].What != w {
			t.Errorf("row %d reads %q, want %q", i, rows[i].What, w)
		}
	}
}

// TestSnapshotMemberParsesEveryTrigger: the *-before triggers carry dashes of their own, and a
// service name may too.
func TestSnapshotMemberParsesEveryTrigger(t *testing.T) {
	for _, tr := range []Trigger{TriggerStart, TriggerClock, TriggerAppUpdateBefore, TriggerAppUndoBefore, TriggerHassRestoreBefore} {
		svc, got, at, ok := ParseSnapshotMember(SnapshotMember("home-assistant", tr, retentionNow))
		if !ok || svc != "home-assistant" || got != tr || !at.Equal(retentionNow) {
			t.Errorf("%s parsed as (%q, %q, %s, %t)", tr, svc, got, at, ok)
		}
		if tr.Before() != (tr != TriggerStart && tr != TriggerClock) {
			t.Errorf("%s.Before() = %t", tr, tr.Before())
		}
	}
}

// TestRetentionWaitsForEvaluation: a pending start is kept, and so is the newest evaluated sample
// its evaluation will compare with (pruning waits for evaluation).
func TestRetentionWaitsForEvaluation(t *testing.T) {
	ev := anchor(TriggerStart, 3*time.Hour, ReasonChanged, time.Hour, "Changed automations")
	s0 := sample(TriggerClock, 150*time.Minute)
	older, baseline := sample(TriggerClock, 2*time.Hour), sample(TriggerClock, time.Hour)
	p1, p2 := sample(TriggerStart, 10*time.Minute), sample(TriggerStart, time.Minute)
	p1.Meta.Pending, p2.Meta.Pending = true, true
	got := pruned(t, ev, s0, older, baseline, p1, p2)
	if got[baseline.Member] || got[p1.Member] || got[p2.Member] {
		t.Errorf("pruned %v; a pending start or its baseline went before the evaluation", got)
	}
	if !got[older.Member] {
		t.Error("an evaluated sample that is nobody's baseline survived")
	}
}

// TestHistoryCaptionsUnhealthy (owner, 2026-09-27): `unhealthy` is never in a title. A row that
// carries it gets the red caption, alone or beside other reasons, and no other row does.
func TestHistoryCaptionsUnhealthy(t *testing.T) {
	alone := anchor(TriggerClock, 3*time.Hour, ReasonUnhealthy, time.Hour, "")
	update := anchor(TriggerAppUpdateBefore, 2*time.Hour, ReasonAppUpdate, 0, "Updated to 2026.9.0")
	update.Meta.Event = update.Meta.Event.With(Reason{Kind: ReasonUnhealthy}, retentionNow)
	change := anchor(TriggerStart, time.Hour, ReasonChanged, 0, "Changed automations")
	rows := History([]SnapshotEntry{alone, update, change}, time.UTC)
	const caption = "home-assistant failed to start cleanly after this change."
	if rows[1].What != "Updated to 2026.9.0" || rows[2].What != "" {
		t.Errorf("titles = %q, %q; unhealthy must not be in a title", rows[1].What, rows[2].What)
	}
	if rows[1].Caption != caption || rows[2].Caption != caption || rows[0].Caption != "" {
		t.Errorf("captions = %q, %q, %q", rows[0].Caption, rows[1].Caption, rows[2].Caption)
	}
}

// TestUnhealthyOffersTheLastHealthyState: while the newest evaluated sample says unhealthy, the
// banner's undo is the newest unhealthy row's point; once healthy again, no banner.
func TestUnhealthyOffersTheLastHealthyState(t *testing.T) {
	point := anchor(TriggerClock, 3*time.Hour, ReasonUnhealthy, time.Hour, "")
	start := sample(TriggerStart, 2*time.Hour)
	start.Meta.Health = services.Unhealthy
	pending := sample(TriggerStart, time.Minute)
	pending.Meta.Pending = true
	if got, ok := Unhealthy([]SnapshotEntry{point, start, pending}); !ok || got.Member != point.Member {
		t.Errorf("Unhealthy = (%s, %t), want the unhealthy row's point", got.Member, ok)
	}
	start.Meta.Health = services.Healthy
	if _, ok := Unhealthy([]SnapshotEntry{point, start}); ok {
		t.Error("a healthy app still offers the banner")
	}
}

// TestRetentionHonoursTheAppsHoldForAWeek: an app sidecar keeps a member the history would
// replace, and stops keeping it after RetainApp, so an app's bug cannot hold the ring forever.
func TestRetentionHonoursTheAppsHoldForAWeek(t *testing.T) {
	quiet := anchor(TriggerClock, 9*day, ReasonQuiet, 0, "")
	held := sample(TriggerClock, 6*day)
	held.App = true
	expired := sample(TriggerClock, 8*day)
	expired.App = true
	plain := sample(TriggerClock, 5*day)
	newest := sample(TriggerClock, time.Hour)
	got := pruned(t, quiet, expired, held, plain, newest)
	if got[held.Member] {
		t.Errorf("a member its app holds was pruned inside RetainApp")
	}
	if !got[expired.Member] {
		t.Errorf("an app's hold outlived RetainApp")
	}
	if !got[plain.Member] {
		t.Errorf("a plain sample survived; the hold must be the only thing keeping its neighbour")
	}
}

// TestTheRecorderRestoreSampleIsABeforeSample: nothing is compared against it, which is what
// keeps a restore from registering as a change, and its name reads back.
func TestTheRecorderRestoreSampleIsABeforeSample(t *testing.T) {
	if !TriggerHassDBRestoreBefore.Before() {
		t.Fatal("hass-db-restore-before is not a *-before sample")
	}
	at := retentionNow
	svc, tr, got, ok := ParseSnapshotMember(SnapshotMember("home-assistant", TriggerHassDBRestoreBefore, at))
	if !ok || svc != "home-assistant" || tr != TriggerHassDBRestoreBefore || !got.Equal(at) {
		t.Fatalf("parsed %q %q %v %t", svc, tr, got, ok)
	}
	// The older trigger whose name it contains still parses as itself.
	if _, tr, _, _ := ParseSnapshotMember(SnapshotMember("home-assistant", TriggerHassRestoreBefore, at)); tr != TriggerHassRestoreBefore {
		t.Fatalf("hass-restore-before parsed as %q", tr)
	}
}
