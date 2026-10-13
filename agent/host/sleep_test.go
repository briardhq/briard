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
// with a tick's jitter says nothing, a new wake is one event naming the node and the stretch, and
// the lid's fix rides in its body only when the plan sleeps on the lid.
func TestSleepAlerter(t *testing.T) {
	ctx := context.Background()
	t0 := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	slept, woke, ok, lid := t0.Add(-time.Hour), t0.Add(-50*time.Minute), true, ""
	fn := &fakeNotifier{}
	st := testStore(t, fn)
	s := &sleepAlerter{
		read: func() (time.Time, time.Time, bool) { return slept, woke, ok },
		lid:  func() string { return lid },
	}
	tick := func() { s.observe(ctx, st, "n1", func(string, ...any) {}) }

	tick() // primes on the sleep that ended before the agent
	woke = woke.Add(20 * time.Millisecond)
	tick() // the same wake, placed a little differently
	if len(fn.alerts) != 0 {
		t.Fatalf("a sleep from before the agent, or the same wake re-read, alerted: %+v", fn.alerts)
	}

	slept, woke = t0.Add(time.Hour), t0.Add(3*time.Hour)
	tick()
	tick()
	if len(fn.alerts) != 1 {
		t.Fatalf("want one event for one sleep, got %+v", fn.alerts)
	}
	a := fn.alerts[0]
	if a.Kind != notify.Event || a.Severity != notify.Warning || !strings.Contains(a.Body, "n1") ||
		!strings.Contains(a.Body, "2h0m") || strings.Contains(a.Body, "LIDACTION") {
		t.Fatalf("want a warning event naming the node and the two hours, without the lid fix: %+v", a)
	}

	slept, woke, lid = t0.Add(4*time.Hour), t0.Add(5*time.Hour), "sleeps"
	tick()
	if len(fn.alerts) != 2 || !strings.Contains(fn.alerts[1].Body, "LIDACTION 0") {
		t.Fatalf("want a second event carrying the lid fix, got %+v", fn.alerts)
	}

	ok = false // a reading that fails changes nothing
	tick()
	if len(fn.alerts) != 2 {
		t.Fatalf("an unknown reading alerted: %+v", fn.alerts)
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
	tick := func() { s.observe(ctx, fn, "n1", func(string, ...any) {}) }
	tick()
	tick()
	if len(fn.alerts) != 0 {
		t.Fatalf("no sleep since boot alerted: %+v", fn.alerts)
	}
	slept, woke = t0, t0.Add(10*time.Minute)
	tick()
	if len(fn.alerts) != 1 || fn.alerts[0].Kind != notify.Event {
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
		s.observe(context.Background(), fn, "n1", func(string, ...any) {})
	}
	if len(fn.alerts) != 0 {
		t.Fatalf("an unknown sleep alerted: %+v", fn.alerts)
	}
}
