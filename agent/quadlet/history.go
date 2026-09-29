package quadlet

import (
	"sort"
	"strings"
	"time"

	"briard.io/agent/services"
)

// THE HISTORY: what a household sees of the ring. Members are SAMPLES, taken at every
// start, by the clock, and just before each operation; what the household reads is a list of
// EVENTS, and each event is undoable because it sits on a sample, its RESTORE POINT.
//
// ONE EVALUATION PER SAMPLE. What is found at sample T1 happened between T0 and T1, and returning
// to T0 undoes it, so a finding is recorded on T0. Two samples are compared only if the app RAN
// between them: a *-before sample is taken with the app stopped and the app does not run again
// until the next start, so nothing is compared against a *-before sample (Trigger.Before), and
// everything else is.
//
// AN EVENT IS ONE RECORD with a creation time and a set of REASONS that only ever grows. At most one
// event sits on a restore point; a later finding adds a reason to it rather than making another.
//
// THE INVARIANT "undo back to here" rests on: the history is ordered by restore point, and each row
// restores its own, so undoing a row undoes exactly the rows above it and none below. Every new
// restore point is later than the one before it, because each is the *-before sample an operation
// takes first, the sample before the one evaluated, or a sample after the last event.
//
// A sample that anchors no event is REPLACED by the next one (RetentionPrune), so sampling often
// costs nothing a household can see.

// A ReasonKind says why an event exists. The set is closed: an operation, a detected change, a
// reset the app made on its own, a start that ended unhealthy, or quiet time.
type ReasonKind string

const (
	// ReasonAppUpdate is briard moving the app to another version, on its app-update-before sample.
	ReasonAppUpdate ReasonKind = "app-update"
	// ReasonAppUndo is briard putting an earlier sample back, on its app-undo-before sample.
	ReasonAppUndo ReasonKind = "app-undo"
	// ReasonHassRestore is Home Assistant restoring one of its OWN backups, which briard sees from
	// the inside and did not perform, on its hass-restore-before sample.
	ReasonHassRestore ReasonKind = "hass-restore"
	// ReasonChanged is what an app's detector found (services.Detect).
	ReasonChanged ReasonKind = "changed"
	// ReasonReset is the app setting its own data aside and starting empty: Home Assistant renaming
	// an undecodable store or database to `*.corrupt.*`. Silent data loss that no health
	// signal sees, and undo is its remedy.
	ReasonReset ReasonKind = "reset"
	// ReasonUnhealthy is a start whose boot ended unhealthy after a healthy one, on the sample
	// before that start. Only the healthy → unhealthy transition registers: the recovery is an
	// app-undo or the household's own fix, which are rows of their own.
	ReasonUnhealthy ReasonKind = "unhealthy"
	// ReasonQuiet is a restore point where nothing was found: the backstop for every change the
	// detectors do not see (QuietPoint). It never shares its record.
	ReasonQuiet ReasonKind = "quiet"
)

// Reason is one reason an event exists.
type Reason struct {
	Kind ReasonKind `json:"kind"`
	// What is the phrase a household reads, e.g. "Updated to 2026.9.1" or "Added Frigate, changed
	// automations". Empty for quiet, whose words depend on the rows around it, and for unhealthy,
	// which is a row's caption rather than words of its own (History).
	What string `json:"what,omitempty"`
}

// Event is one row of an app's history as it is stored.
type Event struct {
	// At is the event's CREATION time, when its first reason arrived, and it is what a row shows.
	At      time.Time `json:"at"`
	Reasons []Reason  `json:"reasons"`
}

// Has reports whether the event carries a reason of this kind.
func (e *Event) Has(k ReasonKind) bool {
	if e == nil {
		return false
	}
	for _, r := range e.Reasons {
		if r.Kind == k {
			return true
		}
	}
	return false
}

// With is the event after a reason arrives at `at`: a new event on a point that has none, and the
// same event with one more reason otherwise. The creation time never moves.
func (e *Event) With(r Reason, at time.Time) *Event {
	if e == nil {
		return &Event{At: at, Reasons: []Reason{r}}
	}
	out := *e
	out.Reasons = append(append([]Reason(nil), e.Reasons...), r)
	return &out
}

// How long the history keeps what (owner's numbers).
const (
	// RetainEvents is how long every event stays undoable.
	RetainEvents = 5 * 24 * time.Hour
	// RetainLastUpdate keeps the most recent update undoable for longer: "the update broke my
	// house" is discovered days later rather than minutes.
	RetainLastUpdate = 14 * 24 * time.Hour
)

// Quiet time fills the gaps: a stretch with nothing found gets a restore point at its
// start once it has lasted QuietAfter, and about one every QuietEvery after that.
const (
	QuietAfter = 5 * time.Hour
	QuietEvery = 24 * time.Hour
)

// sortedMembers is one service's members with a parseable name, OLDEST FIRST.
//
// ⚠️ IT ORDERS BY THE TIME IN THE NAME, never by the name itself: the trigger sits between the
// service and the stamp and would dominate a string comparison. A member whose name does not
// parse is somebody else's and is left out.
func sortedMembers(members []SnapshotEntry) []SnapshotEntry {
	var all []SnapshotEntry
	for _, e := range members {
		if _, ok := SnapshotMemberTime(e.Member); ok {
			all = append(all, e)
		}
	}
	sort.Slice(all, func(i, j int) bool {
		ti, _ := SnapshotMemberTime(all[i].Member)
		tj, _ := SnapshotMemberTime(all[j].Member)
		return ti.Before(tj)
	})
	return all
}

// quietStart is the index of S₀ in `all` (oldest first): the first sample after the newest event
// when that event is not itself quiet, or the first sample of a ring with no event yet. It is -1
// when the stretch already has its quiet point, or when no sample follows the newest event yet.
func quietStart(all []SnapshotEntry) int {
	for i := len(all) - 1; i >= 0; i-- {
		if ev := all[i].Meta.Event; ev != nil {
			if ev.Has(ReasonQuiet) || i+1 == len(all) {
				return -1
			}
			return i + 1
		}
	}
	if len(all) == 0 {
		return -1
	}
	return 0
}

// QuietPoint says which member a quiet evaluation of `member` registers a `quiet` event on, if any.
// The caller has already found nothing at `member`, and `member` carries no event of
// its own.
//
// Let S₀ be the first sample after the last event. The first quiet sample at least QuietAfter after
// S₀ registers quiet on S₀, and later ones change nothing. Once QuietEvery has passed since the
// newest quiet point, the next quiet sample starts another, on the sample before it.
//
// ⚠️ ON THE SAMPLE BEFORE, never on `member` itself: a finding at the next sample lands on
// `member`, and a quiet event never shares its record.
func QuietPoint(members []SnapshotEntry, member string) (SnapshotEntry, bool) {
	at, ok := SnapshotMemberTime(member)
	if !ok {
		return SnapshotEntry{}, false
	}
	var before []SnapshotEntry
	for _, e := range sortedMembers(members) {
		if t, _ := SnapshotMemberTime(e.Member); e.Member != member && t.Before(at) {
			before = append(before, e)
		}
	}
	if len(before) == 0 {
		return SnapshotEntry{}, false
	}
	if s := quietStart(before); s >= 0 {
		t, _ := SnapshotMemberTime(before[s].Member)
		if at.Sub(t) >= QuietAfter {
			return before[s], true
		}
		return SnapshotEntry{}, false
	}
	// The newest event is quiet, or sits on the sample right before this one.
	prev := before[len(before)-1]
	if prev.Meta.Event != nil {
		return SnapshotEntry{}, false
	}
	for i := len(before) - 1; i >= 0; i-- {
		if before[i].Meta.Event.Has(ReasonQuiet) {
			t, _ := SnapshotMemberTime(before[i].Member)
			if at.Sub(t) >= QuietEvery {
				return prev, true
			}
			return SnapshotEntry{}, false
		}
	}
	return SnapshotEntry{}, false
}

// RetentionPrune reports which of one service's members the history no longer needs, OLDEST
// FIRST — the order they should be deleted in, so an interrupted prune has still dropped the least
// useful ones.
//
// A member is kept if it is the restore point of an event under RetainEvents old, of the latest
// app-update for RetainLastUpdate, the NEWEST member, a start still PENDING evaluation, the newest
// EVALUATED member (what that evaluation compares with, so pruning waits for it), or S₀, the start
// of a quiet stretch that has no quiet point yet (QuietPoint needs it). Everything else is a
// sample that anchors nothing, and the next one has replaced it.
//
// NO COUNT LIMIT, because the event rate is bounded by the sampling rate: at most one finding per
// sample, plus the operations and one quiet point a day. A crash loop is a run of samples with
// nothing between them to detect, so every one of them is replaced by the next.
func RetentionPrune(members []SnapshotEntry, now time.Time) []string {
	all := sortedMembers(members)
	if len(all) == 0 {
		return nil
	}
	lastUpdate := -1
	for i, m := range all {
		if m.Meta.Event.Has(ReasonAppUpdate) {
			lastUpdate = i
		}
	}
	s0 := quietStart(all)
	evaluated := -1
	for i, m := range all {
		if !m.Meta.Pending {
			evaluated = i
		}
	}
	var prune []string
	for i, m := range all {
		ev := m.Meta.Event
		switch {
		case i == len(all)-1, i == s0, i == evaluated, m.Meta.Pending:
		case ev != nil && now.Sub(ev.At) <= RetainEvents:
		case i == lastUpdate && now.Sub(ev.At) <= RetainLastUpdate:
		default:
			prune = append(prune, m.Member)
		}
	}
	return prune
}

// A HistoryRow is one line of an app's history as a household reads it.
type HistoryRow struct {
	Reasons []Reason
	// What is the row's title, built from its reasons. An `unhealthy` reason is not in it; it is
	// the row's Caption.
	What string
	// Caption is the red-marked line under a row whose app did not come up healthy after it,
	// or "" for every other row.
	Caption string
	// At is the event's creation time, which is what the row shows.
	At time.Time
	// Point is the restore point: undoing this row, and so every row above it, puts it back.
	Point SnapshotEntry
}

// History is one service's events, NEWEST FIRST, from its members (any order). Times in a title
// are read in loc, which is the reader's.
func History(members []SnapshotEntry, loc *time.Location) []HistoryRow {
	var rows []HistoryRow
	all := sortedMembers(members)
	// BY RESTORE POINT, newest first, which is the invariant's order (see the top of this file).
	for i := len(all) - 1; i >= 0; i-- {
		if ev := all[i].Meta.Event; ev != nil {
			rows = append(rows, HistoryRow{Reasons: ev.Reasons, At: ev.At, Point: all[i]})
		}
	}
	for i := range rows {
		var next *HistoryRow
		if i > 0 {
			next = &rows[i-1]
		}
		rows[i].What = title(rows[i], next, loc)
		if rows[i].Point.Meta.Event.Has(ReasonUnhealthy) {
			rows[i].Caption = rows[i].Point.Meta.Service + " failed to start cleanly after this change."
		}
	}
	return rows
}

// title is a row's words, from its reasons in the order they arrived. A quiet row's span is the
// time until the next event, so it is rendered here rather than stored.
func title(r HistoryRow, next *HistoryRow, loc *time.Location) string {
	var parts []string
	for _, reason := range r.Reasons {
		switch {
		case reason.Kind == ReasonUnhealthy:
		case reason.Kind != ReasonQuiet:
			parts = append(parts, reason.What)
		case next == nil:
			parts = append(parts, "Running normally")
		default:
			parts = append(parts, "Ran normally until "+next.At.In(loc).Format("Mon 15:04"))
		}
	}
	return strings.Join(parts, "; ")
}

// Unhealthy says whether the app is unhealthy as of its newest evaluated sample, and if so the
// restore point that undoes it: the newest row with an `unhealthy` reason, the last healthy state.
// It is what the history's banner offers.
func Unhealthy(members []SnapshotEntry) (SnapshotEntry, bool) {
	all := sortedMembers(members)
	for i := len(all) - 1; i >= 0; i-- {
		if all[i].Meta.Pending {
			continue
		}
		if all[i].Meta.Health != services.Unhealthy {
			return SnapshotEntry{}, false
		}
		break
	}
	for i := len(all) - 1; i >= 0; i-- {
		if all[i].Meta.Event.Has(ReasonUnhealthy) {
			return all[i], true
		}
	}
	return SnapshotEntry{}, false
}
