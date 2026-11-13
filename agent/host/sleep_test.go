package host

import (
	"context"
	"strings"
	"testing"
	"time"

	"briard.io/shared/notify"
)

// TestSleepAlerter walks the machine's last sleep through the readings a node takes, via the real
// store: the first reading primes (a sleep before the agent is not news), the same wake read again
// with a tick's jitter says nothing, a new wake is an Info event naming the node and the stretch
// plus the Warning condition that this computer sleeps, and the lid's fix rides in the bodies only
// when the plan sleeps on the lid. The condition is one record across sleeps -- so a dismissal
// holds -- and resolves a week after the last sleep, after which a sleep asks again.
func TestSleepAlerter(t *testing.T) {
	ctx := context.Background()
	t0 := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	slept, woke, ok, lid := t0.Add(-time.Hour), t0.Add(-50*time.Minute), true, ""
	fn := &fakeNotifier{}
	st := testStore(t, fn)
	clock := t0
	st.now = func() time.Time { return clock }
	s := &sleepAlerter{
		read: func() (time.Time, time.Time, bool) { return slept, woke, ok },
		lid:  func() string { return lid },
	}
	tick := func() { s.observe(ctx, st, "n1", clock, func(string, ...any) {}) }

	tick() // primes on the sleep that ended before the agent
	woke = woke.Add(20 * time.Millisecond)
	tick() // the same wake, placed a little differently
	if len(fn.alerts) != 0 {
		t.Fatalf("a sleep from before the agent, or the same wake re-read, alerted: %+v", fn.alerts)
	}

	slept, woke = t0.Add(time.Hour), t0.Add(3*time.Hour)
	clock = woke
	tick()
	tick()
	if len(fn.alerts) != 2 {
		t.Fatalf("want the event and the condition for one sleep, got %+v", fn.alerts)
	}
	ev, cond := fn.alerts[0], fn.alerts[1]
	if ev.Key != sleepEventKey || ev.Kind != notify.Event || ev.Severity != notify.Info || !strings.Contains(ev.Body, "n1") ||
		!strings.Contains(ev.Body, "2h0m") || strings.Contains(ev.Body, "LIDACTION") {
		t.Fatalf("want an info event naming the node and the two hours, without the lid fix: %+v", ev)
	}
	if cond.Key != sleepsKey || cond.Kind != notify.Open || cond.Severity != notify.Warning || strings.Contains(cond.Body, "LIDACTION") {
		t.Fatalf("want the warning condition, without the lid fix: %+v", cond)
	}

	recs, _ := st.snapshot()
	if att := notify.Attention(recs); len(att) != 1 || att[0].Key != sleepsKey {
		t.Fatalf("want only the condition to need attention: %+v", att)
	}
	if found, err := st.dismiss(sleepsKey, att0(recs, sleepsKey).At); !found || err != nil {
		t.Fatalf("dismiss: %v %v", found, err)
	}

	slept, woke, lid = t0.Add(4*time.Hour), t0.Add(5*time.Hour), "sleeps"
	clock = woke
	tick()
	if len(fn.alerts) != 3 || fn.alerts[2].Key != sleepEventKey || !strings.Contains(fn.alerts[2].Body, "LIDACTION 0") {
		t.Fatalf("want a second event carrying the lid fix, and the open condition not said again, got %+v", fn.alerts)
	}
	if recs, _ := st.snapshot(); len(notify.Attention(recs)) != 0 {
		t.Fatalf("a second sleep undid the dismissal: %+v", notify.Attention(recs))
	}

	ok = false // a reading that fails changes nothing
	tick()
	clock = woke.Add(sleepQuiet - time.Minute)
	tick()
	if len(fn.alerts) != 3 {
		t.Fatalf("an unknown reading, or a quiet week not yet over, alerted: %+v", fn.alerts[3:])
	}

	clock = woke.Add(sleepQuiet)
	tick()
	tick()
	if len(fn.alerts) != 4 || fn.alerts[3].Key != sleepsKey || fn.alerts[3].Kind != notify.Resolved {
		t.Fatalf("want the condition resolved once, a week after the last sleep: %+v", fn.alerts[3:])
	}

	ok = true
	slept, woke = clock.Add(time.Hour), clock.Add(2*time.Hour)
	clock = woke
	tick()
	if recs, _ := st.snapshot(); len(notify.Attention(recs)) != 1 {
		t.Fatalf("a sleep after the quiet week did not ask again: %+v", recs)
	}
}

// att0 is the latest record of key.
func att0(recs []notify.Record, key string) notify.Record {
	for i := len(recs) - 1; i >= 0; i-- {
		if recs[i].Key == key {
			return recs[i]
		}
	}
	return notify.Record{}
}

// A restart forgets nothing the condition needs: a new alerter over the same store resolves the
// condition a week after the sleep the store recorded, not a week after it started.
func TestSleepConditionResolvesFromTheStore(t *testing.T) {
	ctx := context.Background()
	t0 := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	st := testStore(t, nil)
	st.now = func() time.Time { return t0 }
	must(t, st.Notify(ctx, notify.Alert{Key: sleepEventKey, Kind: notify.Event, Severity: notify.Info, Title: "slept"}))
	must(t, st.Notify(ctx, notify.Alert{Key: sleepsKey, Kind: notify.Open, Severity: notify.Warning, Title: "sleeps"}))

	none := func() (time.Time, time.Time, bool) { return time.Time{}, time.Time{}, true } // no sleep since boot
	s := &sleepAlerter{read: none, lid: func() string { return "" }}
	s.observe(ctx, st, "n1", t0.Add(6*24*time.Hour), t.Logf)
	if recs, _ := st.snapshot(); len(notify.OpenNow(recs)) != 1 {
		t.Fatalf("resolved inside the week: %+v", recs)
	}
	s.observe(ctx, st, "n1", t0.Add(sleepQuiet), t.Logf)
	if recs, _ := st.snapshot(); len(notify.OpenNow(recs)) != 0 {
		t.Fatalf("still open a week after the stored sleep: %+v", recs)
	}
}

// The rig's sequence: an agent started on a machine that has not slept since boot primes on that
// answer, and the FIRST sleep after it is told -- reading "no sleep yet" as unknown made the first
// sleep the priming reading, and it went untold (measured on the Windows rig).
func TestSleepAlerterFirstSleepAfterBoot(t *testing.T) {
	ctx := context.Background()
	t0 := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	var slept, woke time.Time // no sleep since boot: ok, zero times
	fn := &fakeNotifier{}
	s := &sleepAlerter{
		read: func() (time.Time, time.Time, bool) { return slept, woke, true },
		lid:  func() string { return "" },
	}
	tick := func() { s.observe(ctx, fn, "n1", t0, func(string, ...any) {}) }
	tick()
	tick()
	if len(fn.alerts) != 0 {
		t.Fatalf("no sleep since boot alerted: %+v", fn.alerts)
	}
	slept, woke = t0, t0.Add(10*time.Minute)
	tick()
	if len(fn.alerts) != 2 || fn.alerts[0].Kind != notify.Event || fn.alerts[1].Kind != notify.Open {
		t.Fatalf("the first sleep after boot went untold: %+v", fn.alerts)
	}
}

// A machine that never says (Linux, a failed read) never alerts.
func TestSleepAlerterUnknownNeverFires(t *testing.T) {
	fn := &fakeNotifier{}
	s := &sleepAlerter{
		read: func() (time.Time, time.Time, bool) { return time.Time{}, time.Time{}, false },
		lid:  func() string { return "sleeps" },
	}
	for range 100 {
		s.observe(context.Background(), fn, "n1", time.Now(), func(string, ...any) {})
	}
	if len(fn.alerts) != 0 {
		t.Fatalf("an unknown sleep alerted: %+v", fn.alerts)
	}
}

// A Fast Startup Shut down reaches the agent as a sleep. It is told as the shut down the household
// chose, from the sleep's own start, and without the lid's fix, which is not why it happened.
func TestSleepAlerterShutDown(t *testing.T) {
	ctx := context.Background()
	t0 := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	var slept, woke, asked time.Time
	fn := &fakeNotifier{}
	s := &sleepAlerter{
		read:     func() (time.Time, time.Time, bool) { return slept, woke, true },
		lid:      func() string { return "sleeps" },
		shutDown: func(since time.Time) bool { asked = since; return true },
	}
	tick := func() { s.observe(ctx, fn, "n1", t0, func(string, ...any) {}) }
	tick()
	slept, woke = t0, t0.Add(10*time.Minute)
	tick()
	if len(fn.alerts) != 2 {
		t.Fatalf("want the event and the condition, got %+v", fn.alerts)
	}
	a := fn.alerts[0]
	if !asked.Equal(slept) || a.Title != "Briard: this computer was shut down" ||
		!strings.Contains(a.Body, "was shut down from") || strings.Contains(a.Body, "asleep") ||
		strings.Contains(a.Body, "LIDACTION") || strings.Contains(fn.alerts[1].Body, "LIDACTION") {
		t.Fatalf("want a shut-down event asked from the sleep's start, without sleep or the lid: asked %v, %+v", asked, fn.alerts)
	}
}
