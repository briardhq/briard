package quadlet

import (
	"sort"
	"strings"
	"time"
)

// THE HISTORY ([B.167]): what a household sees of the ring. Members are SAMPLES, taken at every
// start and by the clock; what the household reads is a list of EVENTS, and each event is
// undoable because it sits on a sample.
//
// AN EVENT'S RESTORE POINT IS ALWAYS THE SAMPLE BEFORE IT, so undoing it lands before the event.
// The event is therefore stored on that sample (SnapshotMeta.Event): a performed act writes it with
// the member it takes first, and a detected change or a day boundary writes it into the previous
// member's sidecar when the next sample finds it. Every event is created at a sample and anchored
// to the one before, so each sample anchors at most one event, and ordering the history by
// restore point orders it by event too. That is the invariant "undo back to here" rests on: it
// undoes exactly the rows above and none below, because nothing is ever inserted retroactively
// somewhere else in the list.
//
// A sample that anchors no event is REPLACED by the next one (RetentionPrune), so sampling often
// costs nothing a household can see.

// An EventKind says where an event came from. The set is closed: a performed act, a detected
// change, or the clock.
type EventKind string

const (
	// EventUpdate is the app moving to another version. Its point is the pre-upgrade member.
	EventUpdate EventKind = "update"
	// EventUndo is a household putting a point back. Undoing it is the redo.
	EventUndo EventKind = "undo"
	// EventBackupRestore is the app restoring one of its OWN backups (Home Assistant's), which
	// briard sees from the inside and did not perform.
	EventBackupRestore EventKind = "backup-restore"
	// EventChange is what an app's detector found between two samples (services.Detect).
	EventChange EventKind = "change"
	// EventDay is the clock's event: a day passed with nothing else to show for it. It is the
	// safety net for every change the detectors do not see, and what lets their list stay small.
	EventDay EventKind = "day"
)

// Event is one row of an app's history as it is stored.
type Event struct {
	Kind EventKind `json:"kind"`
	// At is the event's OWN time: when the act ran, or the sample that found the change. It
	// differs from its point's time only for a detected event, by at most one sampling interval.
	At time.Time `json:"at"`
	// What is the line a household reads, e.g. "Updated to 2026.9.1" or "Added Frigate, changed
	// automations".
	What string `json:"what"`
}

// Baseline reports whether a member is taken right AFTER a performed act, which makes it the
// baseline the next sample is compared with rather than a sample compared with the one before.
//
// The act changed the data by design — an undo rewound it — and its own event already says so;
// comparing across it would report the act a second time as a detected change directly above it.
func Baseline(t Trigger) bool { return t == TriggerRestoreAfter || t == TriggerUpgradeAfter }

// How long the history keeps what (owner, [B.167]).
const (
	// RetainEvents is how long every event stays undoable.
	RetainEvents = 5 * 24 * time.Hour
	// RetainLastUpdate keeps the most recent update undoable for longer: "the update broke my
	// house" is discovered days later rather than minutes.
	RetainLastUpdate = 14 * 24 * time.Hour
)

// RetentionPrune reports which of one service's members the history no longer needs, OLDEST
// FIRST — the order they should be deleted in, so an interrupted prune has still dropped the least
// useful ones.
//
// A member is kept if it is the restore point of a retained event, or it is the NEWEST member —
// the baseline the next sample is compared with. Everything else is a sample that anchors
// nothing, and the next one has replaced it.
//
// NO COUNT LIMIT, because the event rate is bounded by the sampling rate: at most one detected
// event per sample, plus the performed ones and one a day. A crash loop is a run of samples with
// nothing between them to detect, so every one of them is replaced by the next.
//
// ⚠️ IT ORDERS BY THE TIME IN THE NAME, never by the name itself: the trigger sits between the
// service and the stamp and would dominate a string comparison. A member whose name does not
// parse is somebody else's and is never returned.
func RetentionPrune(members []SnapshotEntry, now time.Time) []string {
	type member struct {
		path string
		at   time.Time
		ev   *Event
	}
	var all []member
	for _, e := range members {
		at, ok := SnapshotMemberTime(e.Member)
		if !ok {
			continue
		}
		all = append(all, member{e.Member, at, e.Meta.Event})
	}
	if len(all) == 0 {
		return nil
	}
	sort.Slice(all, func(i, j int) bool { return all[i].at.Before(all[j].at) }) // oldest first

	lastUpdate := -1
	for i, m := range all {
		if m.ev != nil && m.ev.Kind == EventUpdate {
			lastUpdate = i
		}
	}
	var prune []string
	for i, m := range all {
		switch {
		case i == len(all)-1: // the baseline
		case m.ev != nil && now.Sub(m.ev.At) <= RetainEvents:
		case i == lastUpdate && now.Sub(m.ev.At) <= RetainLastUpdate:
		default:
			prune = append(prune, m.path)
		}
	}
	return prune
}

// A HistoryRow is one line of an app's history as a household reads it.
type HistoryRow struct {
	Kind EventKind
	What string
	// At is the event's own time, which is what the row shows. A merged run of quiet days shows
	// its newest.
	At time.Time
	// Point is the restore point: undoing this row, and so every row above it, puts it back.
	Point SnapshotEntry
}

// History is one service's events, NEWEST FIRST, from its members (any order).
//
// Consecutive quiet days merge into one row ("Ran normally, Mon–Fri") whose point is the OLDEST
// day's, so undoing the row undoes all of it and the invariant holds for the merged row too.
// Day names are read in loc, which is the reader's.
func History(members []SnapshotEntry, loc *time.Location) []HistoryRow {
	var rows []HistoryRow
	for _, e := range members {
		if e.Meta.Event == nil {
			continue
		}
		if _, ok := SnapshotMemberTime(e.Member); !ok {
			continue
		}
		rows = append(rows, HistoryRow{Kind: e.Meta.Event.Kind, What: e.Meta.Event.What, At: e.Meta.Event.At, Point: e})
	}
	// BY RESTORE POINT, which is the invariant's order (see the top of this file).
	sort.Slice(rows, func(i, j int) bool {
		ti, _ := SnapshotMemberTime(rows[i].Point.Member)
		tj, _ := SnapshotMemberTime(rows[j].Point.Member)
		return ti.After(tj)
	})
	var out []HistoryRow
	for i := 0; i < len(rows); {
		j := i + 1
		for rows[i].Kind == EventDay && j < len(rows) && rows[j].Kind == EventDay {
			j++
		}
		row := rows[i]
		if j-i > 1 {
			oldest := rows[j-1]
			row.Point = oldest.Point
			row.What = strings.TrimSuffix(oldest.What, ".") + ", " +
				oldest.At.In(loc).Format("Mon") + "–" + rows[i].At.In(loc).Format("Mon")
		}
		out = append(out, row)
		i = j
	}
	return out
}
