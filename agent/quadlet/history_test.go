package quadlet

import (
	"testing"
	"time"
)

// The history's fixtures ([B.167]). `now` is the moment the prune is asked about, and every
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

// anchor is a member that is the restore point of an event created `after` it.
func anchor(trigger Trigger, ago time.Duration, kind EventKind, after time.Duration, what string) SnapshotEntry {
	e := sample(trigger, ago)
	e.Meta.Event = &Event{Kind: kind, At: e.Meta.TakenAt.Add(after), What: what}
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
// The newest stays: it is the baseline the next sample is compared with.
func TestRetentionReplacesASampleThatAnchorsNothing(t *testing.T) {
	older, newest := sample(TriggerStart, 2*time.Hour), sample(TriggerStart, time.Hour)
	got := pruned(t, older, newest)
	if !got[older.Member] {
		t.Errorf("a sample anchoring no event survived its successor")
	}
	if got[newest.Member] {
		t.Errorf("the newest sample was pruned; it is the next comparison's baseline")
	}
}

// TestRetentionKeepsTheNewestEvenWhenOld: a service nobody touches for a week still has a
// baseline. Age never removes the newest member.
func TestRetentionKeepsTheNewestEvenWhenOld(t *testing.T) {
	only := sample(TriggerDaily, 30*day)
	if pruned(t, only)[only.Member] {
		t.Error("the only member was pruned for its age")
	}
}

// TestRetentionKeepsEventsForFiveDays: every event stays undoable for five days, measured from
// the EVENT's time rather than its point's.
func TestRetentionKeepsEventsForFiveDays(t *testing.T) {
	young := anchor(TriggerStart, 4*day, EventChange, time.Hour, "Changed automations")
	old := anchor(TriggerStart, 6*day, EventChange, time.Hour, "Changed automations")
	edge := anchor(TriggerStart, 5*day+30*time.Minute, EventDay, time.Hour, "Ran normally") // event 4d23h30m ago
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
	superseded := anchor(TriggerUpgrade, 12*day, EventUpdate, 0, "Updated to 2026.8.0")
	last := anchor(TriggerUpgrade, 10*day, EventUpdate, 0, "Updated to 2026.9.0")
	got := pruned(t, superseded, last, sample(TriggerStart, time.Minute))
	if got[last.Member] {
		t.Error("the most recent update was pruned at ten days")
	}
	if !got[superseded.Member] {
		t.Error("a superseded update kept the fortnight; only the last one has it")
	}
	ancient := anchor(TriggerUpgrade, 20*day, EventUpdate, 0, "Updated to 2026.7.0")
	if !pruned(t, ancient, sample(TriggerStart, time.Minute))[ancient.Member] {
		t.Error("a twenty-day-old update survived the fortnight")
	}
}

// TestRetentionHasNoCount: a crash loop is a run of samples with nothing between them, so it costs
// one member however long it runs -- and an afternoon of real changes keeps every one of them.
func TestRetentionHasNoCount(t *testing.T) {
	var ring []SnapshotEntry
	for i := 40; i > 0; i-- {
		ring = append(ring, anchor(TriggerStart, time.Duration(i)*10*time.Minute, EventChange, 5*time.Minute, "Changed configuration"))
	}
	ring = append(ring, sample(TriggerStart, time.Minute))
	if got := pruned(t, ring...); len(got) != 0 {
		t.Errorf("pruned %d of an afternoon's events; the history has no count", len(got))
	}
	var loop []SnapshotEntry
	for i := 1000; i > 0; i-- {
		loop = append(loop, sample(TriggerStart, time.Duration(i)*2*time.Minute))
	}
	if got := pruned(t, loop...); len(got) != 999 {
		t.Errorf("a crash loop of 1000 samples kept %d; it should cost exactly one", 1000-len(got))
	}
}

// TestRetentionOrdersByTimeNotName: the trigger sits between the service and the stamp, so by
// NAME every `-upgrade-` member sorts after every `-start-` one. An old upgrade member with no
// event must not be kept as though it were the newest.
func TestRetentionOrdersByTimeNotName(t *testing.T) {
	oldUpgrade := sample(TriggerUpgrade, 2*day)
	newest := sample(TriggerStart, time.Minute)
	got := pruned(t, newest, oldUpgrade)
	if !got[oldUpgrade.Member] || got[newest.Member] {
		t.Fatalf("pruned %v; want only the old upgrade member -- anything else ranked by NAME", got)
	}
}

// TestRetentionPrunesOldestFirst: an interrupted prune has still dropped the least useful ones.
func TestRetentionPrunesOldestFirst(t *testing.T) {
	a, b, c := sample(TriggerStart, 3*time.Hour), sample(TriggerStart, 2*time.Hour), sample(TriggerStart, time.Hour)
	got := RetentionPrune([]SnapshotEntry{b, c, a}, retentionNow)
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

// TestHistoryIsOrderedByRestorePoint is the invariant: undoing a row undoes exactly the rows above
// it, which holds only if the list is ordered by the points the rows put back.
func TestHistoryIsOrderedByRestorePoint(t *testing.T) {
	update := anchor(TriggerUpgrade, 3*time.Hour, EventUpdate, 0, "Updated to 2026.9.0")
	change := anchor(TriggerStart, 2*time.Hour, EventChange, 30*time.Minute, "Changed automations")
	plain := sample(TriggerStart, time.Hour)
	rows := History([]SnapshotEntry{plain, change, update}, time.UTC)
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want the two events and no row for a plain sample: %+v", len(rows), rows)
	}
	if rows[0].Point.Member != change.Member || rows[1].Point.Member != update.Member {
		t.Errorf("rows are not newest first by point: %s, %s", rows[0].What, rows[1].What)
	}
	if !rows[0].At.Equal(change.Meta.Event.At) {
		t.Errorf("a row shows %s, want the event's own time %s", rows[0].At, change.Meta.Event.At)
	}
}

// TestHistoryMergesQuietDays: "Ran normally" five times is one row, and undoing it undoes all five
// -- so its point is the OLDEST day's. A day run is broken by any other event.
func TestHistoryMergesQuietDays(t *testing.T) {
	// retentionNow is Wednesday 23 Sep; these are Sat, Sun, Mon (events at the next sample).
	sat := anchor(TriggerDaily, 4*day+time.Hour, EventDay, time.Hour, "Ran normally")
	sun := anchor(TriggerDaily, 3*day+time.Hour, EventDay, time.Hour, "Ran normally")
	mon := anchor(TriggerDaily, 2*day+time.Hour, EventDay, time.Hour, "Ran normally")
	edit := anchor(TriggerStart, day+time.Hour, EventChange, time.Hour, "Changed automations")
	tue := anchor(TriggerDaily, time.Hour, EventDay, 30*time.Minute, "Ran normally")
	rows := History([]SnapshotEntry{sat, sun, mon, edit, tue}, time.UTC)
	if len(rows) != 3 {
		t.Fatalf("got %d rows, want Tue, the edit, and Sat–Mon merged: %+v", len(rows), rows)
	}
	merged := rows[2]
	if merged.Point.Member != sat.Member {
		t.Errorf("the merged row puts back %s, want the oldest day's point %s", merged.Point.Member, sat.Member)
	}
	if merged.What != "Ran normally, Sat–Mon" {
		t.Errorf("merged row reads %q", merged.What)
	}
	if !merged.At.Equal(mon.Meta.Event.At) {
		t.Errorf("the merged row shows %s, want its newest day's time", merged.At)
	}
	if rows[0].What != "Ran normally" {
		t.Errorf("a lone quiet day reads %q", rows[0].What)
	}
}
