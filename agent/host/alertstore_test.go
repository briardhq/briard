package host

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"briard.io/shared/notify"
)

func testStore(t *testing.T, inner notify.Notifier) *alertStore {
	t.Helper()
	s := newAlertStore(filepath.Join(t.TempDir(), "alerts.json"), inner, t.Logf)
	tick := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { tick = tick.Add(time.Minute); return tick }
	return s
}

func open(key, title string) notify.Alert {
	return notify.Alert{Key: key, Kind: notify.Open, Severity: notify.Warning, Title: title, Body: "b"}
}
func resolved(key string) notify.Alert {
	return notify.Alert{Key: key, Kind: notify.Resolved, Title: "ok " + key, Body: "b"}
}
func event(key string) notify.Alert {
	return notify.Alert{Key: key, Kind: notify.Event, Severity: notify.Info, Title: "happened", Body: "b"}
}

// The transition rule: emitters assert, the store writes (and delivers) only a change.
func TestAlertStoreTransitions(t *testing.T) {
	ctx := context.Background()
	c := &fakeNotifier{}
	s := testStore(t, c)
	fire := func(a notify.Alert) { t.Helper(); _ = s.Notify(ctx, a) }

	fire(resolved("disk")) // nothing is open: dropped, nobody is told the disk is fine
	fire(open("disk", "low"))
	fire(open("disk", "low")) // the same state again: dropped (every tick says this)
	fire(open("disk", "low"))
	fire(open("redundancy", "reduced"))
	fire(open("redundancy", "none")) // a different title while open: the condition changed, recorded
	fire(resolved("disk"))
	fire(resolved("disk")) // already resolved: dropped
	fire(event("reparent"))
	fire(event("reparent")) // events are never deduped

	want := []string{"disk/open/low", "redundancy/open/reduced", "redundancy/open/none", "disk/resolved/ok disk", "reparent/event/happened", "reparent/event/happened"}
	if len(c.alerts) != len(want) {
		t.Fatalf("delivered %d alerts, want %d: %+v", len(c.alerts), len(want), c.alerts)
	}
	for i, a := range c.alerts {
		if got := fmt.Sprintf("%s/%s/%s", a.Key, a.Kind, a.Title); got != want[i] {
			t.Errorf("delivered[%d] = %s, want %s", i, got, want[i])
		}
	}

	// What was delivered is exactly what was recorded, and the open alerts are derived from it.
	recs, err := ReadAlerts(s.path)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != len(want) {
		t.Fatalf("recorded %d, want %d", len(recs), len(want))
	}
	for i := 1; i < len(recs); i++ {
		if !recs[i].At.After(recs[i-1].At) {
			t.Errorf("records are not stamped in order: %v then %v", recs[i-1].At, recs[i].At)
		}
	}
	o := notify.OpenNow(recs)
	if len(o) != 1 || o[0].Key != "redundancy" || o[0].Title != "none" {
		t.Errorf("open alerts = %+v, want only redundancy/none", o)
	}
}

// The store is what survives a restart: a new process over the same file knows what is open,
// so a condition that persisted is not re-pushed and one that cleared meanwhile is resolved.
func TestAlertStoreSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "alerts.json")
	c1 := &fakeNotifier{}
	s1 := newAlertStore(path, c1, t.Logf)
	_ = s1.Notify(ctx, open("disk", "low"))
	_ = s1.Notify(ctx, open("clock", "unsynced"))

	c2 := &fakeNotifier{}
	s2 := newAlertStore(path, c2, t.Logf)   // the agent restarted; emitters start blank
	_ = s2.Notify(ctx, open("disk", "low")) // still low: nothing to say
	_ = s2.Notify(ctx, resolved("clock"))   // synced while we were down: now it is said
	if len(c2.alerts) != 1 || c2.alerts[0].Key != "clock" || c2.alerts[0].Kind != notify.Resolved {
		t.Fatalf("after restart delivered %+v, want exactly clock/resolved", c2.alerts)
	}
	if last, ok := s2.latest("disk"); !ok || last.Kind != notify.Open {
		t.Errorf("latest(disk) = %+v/%v, want the open from before the restart", last, ok)
	}
}

// A write failure keeps the record in memory and still delivers: a full disk is exactly when
// the disk alert must reach somebody.
func TestAlertStoreWriteFailureStillDelivers(t *testing.T) {
	ctx := context.Background()
	c := &fakeNotifier{}
	s := newAlertStore(filepath.Join(t.TempDir(), "nodir", "sub", "alerts.json"), c, t.Logf)
	// Make the parent unwritable by making it a file.
	if err := os.WriteFile(filepath.Dir(filepath.Dir(s.path)), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	_ = s.Notify(ctx, open("disk", "low"))
	_ = s.Notify(ctx, open("disk", "low"))
	if len(c.alerts) != 1 {
		t.Fatalf("delivered %d, want 1 (delivered despite the failed write, deduped from memory)", len(c.alerts))
	}
}

// The trim keeps the last alertStoreCap records and never drops a key's latest.
func TestAlertStoreTrimKeepsEveryKeysLatest(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, nil)
	_ = s.Notify(ctx, open("old", "still open"))
	for i := 0; i < alertStoreCap+10; i++ {
		_ = s.Notify(ctx, event(fmt.Sprintf("e%d", i%7)))
	}
	recs, err := ReadAlerts(s.path)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != alertStoreCap+1 {
		t.Errorf("kept %d records, want cap %d + the old key's latest", len(recs), alertStoreCap)
	}
	if recs[0].Key != "old" {
		t.Errorf("the oldest kept record is %s, want the trimmed-past key's latest", recs[0].Key)
	}
	if o := notify.OpenNow(recs); len(o) != 1 || o[0].Key != "old" {
		t.Errorf("open alerts after trim = %+v, want old", o)
	}
}

// A nil delivery (a witness: nobody to push to) still records.
func TestAlertStoreRecordsWithoutDelivery(t *testing.T) {
	s := testStore(t, nil)
	if err := s.Notify(context.Background(), open("guest", "silent")); err != nil {
		t.Fatal(err)
	}
	if recs, _ := ReadAlerts(s.path); len(recs) != 1 {
		t.Errorf("recorded %d, want 1", len(recs))
	}
}

// fakeAlertsGuest records the copies the page was handed.
type fakeAlertsGuest struct {
	old    bool // an image without the verb
	fail   bool
	copies [][]notify.Record
}

func (g *fakeAlertsGuest) DashboardAlerts(_ context.Context, recs []notify.Record) error {
	if g.fail {
		return fmt.Errorf("channel hiccup")
	}
	g.copies = append(g.copies, recs)
	return nil
}
func (g *fakeAlertsGuest) SupportsDashboardAlerts() bool { return !g.old }

// The page's copy: handed over at the start of every connection (even an empty store -- "nothing
// has happened" is something the page shows), again only when the store records something, and
// retried after a failed push. A guest without the verb is never asked.
func TestPushAlertsCopiesTheStoreWhenItMoves(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, nil)
	g := &fakeAlertsGuest{}
	pushed := -1 // a fresh connection
	pushAlerts(ctx, g, s, &pushed, t.Logf)
	if len(g.copies) != 1 || g.copies[0] == nil || len(g.copies[0]) != 0 {
		t.Fatalf("first cycle pushed %+v; want one empty, non-nil copy", g.copies)
	}
	pushAlerts(ctx, g, s, &pushed, t.Logf)
	if len(g.copies) != 1 {
		t.Fatalf("an unchanged store was pushed again (%d copies)", len(g.copies))
	}
	_ = s.Notify(ctx, open("disk", "low"))
	_ = s.Notify(ctx, open("disk", "low")) // dropped by the store: no new copy either
	g.fail = true
	pushAlerts(ctx, g, s, &pushed, t.Logf)
	g.fail = false
	pushAlerts(ctx, g, s, &pushed, t.Logf)
	pushAlerts(ctx, g, s, &pushed, t.Logf)
	if len(g.copies) != 2 || len(g.copies[1]) != 1 || g.copies[1][0].Key != "disk" {
		t.Fatalf("copies = %+v; want the retried copy holding the one disk alert, once", g.copies)
	}

	// A new connection (the agent restarted, or the guest was relaunched): the store on disk is
	// handed over at once, though this process recorded nothing.
	s2 := newAlertStore(s.path, nil, t.Logf)
	g2 := &fakeAlertsGuest{}
	pushed = -1
	pushAlerts(ctx, g2, s2, &pushed, t.Logf)
	if len(g2.copies) != 1 || len(g2.copies[0]) != 1 {
		t.Fatalf("after a restart the copy was %+v; want the store's one record", g2.copies)
	}

	old := &fakeAlertsGuest{old: true}
	pushed = -1
	pushAlerts(ctx, old, s, &pushed, t.Logf)
	if len(old.copies) != 0 || pushed != -1 {
		t.Error("a guest without the verb was handed a copy")
	}
}
