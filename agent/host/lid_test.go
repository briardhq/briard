package host

import (
	"context"
	"strings"
	"testing"
	"time"

	"briard.io/shared/notify"
)

// TestLidAlerter walks the lid setting through the readings a node takes, via the real store so
// its dedup is the one under test: one read per interval, an unknown neither opens nor clears, a
// steady "sleeps" says it once, and changing the setting resolves it.
func TestLidAlerter(t *testing.T) {
	ctx := context.Background()
	t0 := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	answer, reads := "", 0
	fn := &fakeNotifier{}
	st := testStore(t, fn)
	l := &lidAlerter{read: func() string { reads++; return answer }}
	at := func(d time.Duration, a string) {
		answer = a
		l.observe(ctx, st, "n1", t0.Add(d), func(string, ...any) {})
	}

	at(0, "") // no lid, or no answer: nothing
	at(time.Minute, "sleeps")
	if reads != 1 || len(fn.alerts) != 0 {
		t.Fatalf("read %d times inside one lidReadEvery (want 1), alerts %+v", reads, fn.alerts)
	}
	at(lidReadEvery, "sleeps")
	if len(fn.alerts) != 1 || fn.alerts[0].Kind != notify.Open || fn.alerts[0].Severity != notify.Warning ||
		!strings.Contains(fn.alerts[0].Body, "n1") || !strings.Contains(fn.alerts[0].Body, "LIDACTION 0") {
		t.Fatalf("want one warning naming the node and the setting, got %+v", fn.alerts)
	}
	at(2*lidReadEvery, "sleeps") // steady: no fatigue
	at(3*lidReadEvery, "")       // unknown does not clear
	if len(fn.alerts) != 1 {
		t.Fatalf("re-fired or cleared on a steady/unknown reading: %+v", fn.alerts)
	}
	at(4*lidReadEvery, "stays")
	if len(fn.alerts) != 2 || fn.alerts[1].Kind != notify.Resolved {
		t.Fatalf("want a resolved alert once the lid is set to do nothing, got %+v", fn.alerts)
	}
}

// A machine that never answers -- Linux, a desktop with no lid -- never alerts.
func TestLidAlerterUnknownNeverFires(t *testing.T) {
	fn := &fakeNotifier{}
	l := &lidAlerter{read: func() string { return "" }}
	t0 := time.Now()
	for d := time.Duration(0); d < 5*time.Hour; d += lidReadEvery {
		l.observe(context.Background(), fn, "n1", t0.Add(d), func(string, ...any) {})
	}
	if len(fn.alerts) != 0 {
		t.Fatalf("an unknown lid alerted: %+v", fn.alerts)
	}
}
