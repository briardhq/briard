package host

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"briard.io/agent/selfupdate"
	"briard.io/shared/notify"
)

// newTestUpdateAlerter builds an alerter over a temp run dir and a fixed clock, behind an alert
// store (the alerter asserts every reading; the store is what makes it an edge).
func newTestUpdateAlerter(t *testing.T, n notify.Notifier) (*updateAlerter, *selfupdate.Layout, *time.Time) {
	t.Helper()
	lay := selfupdate.New(t.TempDir(), t.TempDir())
	logf := func(f string, v ...any) { t.Logf(f, v...) }
	st := newAlertStore(filepath.Join(t.TempDir(), "alerts.json"), n, logf)
	a := newUpdateAlerter(st, "n1", lay, logf)
	now := a.start
	a.now = func() time.Time { return now }
	return a, &lay, &now
}

// result leaves a run's verdict the way the update script does, finished at `at`.
func result(t *testing.T, lay *selfupdate.Layout, line string, at time.Time) {
	t.Helper()
	if err := os.WriteFile(lay.ResultPath(), []byte(line+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(lay.ResultPath(), at, at); err != nil {
		t.Fatal(err)
	}
}

func levels(as []notify.Alert) string {
	var out []string
	for _, a := range as {
		out = append(out, string(a.Kind))
	}
	return strings.Join(out, ",")
}

// The contract: one warning when the node stops updating, silence while it stays stopped, one
// recovered when it updates again. Same no-fatigue shape as the redundancy alerter.
func TestUpdateAlerterEdges(t *testing.T) {
	fn := &fakeNotifier{}
	a, lay, now := newTestUpdateAlerter(t, fn)
	ctx := context.Background()
	for _, step := range []string{"ok already at v1", "ok already at v1", "failed could not fetch", "failed could not fetch", "failed could not fetch", "ok updated to v2"} {
		result(t, lay, step, *now)
		a.observe(ctx)
	}
	if got := levels(fn.alerts); got != "open,resolved" {
		t.Fatalf("expected exactly one open across a steady failure, then resolved, got %q (%+v)", got, fn.alerts)
	}
}

// ⚠️ THE REGRESSION THIS ALERTER IS ABOUT. The alerter must NOT prime: a node that was already
// failing when the agent came up is the steady state of the case the alerter exists for (a
// floored node's agent is the one agent that never gets replaced), so the FIRST reading must
// fire. An alerter that primed like the redundancy one would be silent here forever, and this
// test is what fails when somebody adds that symmetry back.
func TestUpdateAlerterFiresOnTheFirstReading(t *testing.T) {
	fn := &fakeNotifier{}
	a, lay, now := newTestUpdateAlerter(t, fn)
	result(t, lay, "failed could not fetch a bootstrap agent", *now)
	a.observe(context.Background())
	if got := levels(fn.alerts); got != "open" {
		t.Fatalf("a first reading of a failed run must warn (no priming), got %q", got)
	}
}

// The remedy lives in the update run's own last line -- for a floored node, the refusal that
// says in so many words that it must be reinstalled. The warning must carry it, and must leave
// the verdict on disk: the CLI reads it too.
func TestUpdateAlerterCarriesTheRunsLastLine(t *testing.T) {
	fn := &fakeNotifier{}
	a, lay, now := newTestUpdateAlerter(t, fn)
	line := "install: this node is too old to upgrade to that release: installed v3.20260910.64a7834 is older than v3.20260921.5d18301's min_upgrade_from v3.20260920.ec4d22a — this node cannot be upgraded to it and must be reinstalled"
	result(t, lay, "failed "+line, *now)
	a.observe(context.Background())
	if len(fn.alerts) != 1 || !strings.Contains(fn.alerts[0].Body, "The update run said: "+line) {
		t.Fatalf("the alert must carry the run's own remedy, got %+v", fn.alerts)
	}
	if _, err := os.Stat(lay.ResultPath()); err != nil {
		t.Fatalf("the verdict must survive being read: %v", err)
	}
}

// No run yet is not a verdict while the agent is young -- a fresh boot has none until the timer's
// boot run -- but two days without one is the node having stopped updating.
func TestUpdateAlerterNoRunAtAll(t *testing.T) {
	fn := &fakeNotifier{}
	a, _, now := newTestUpdateAlerter(t, fn)
	a.observe(context.Background())
	if len(fn.alerts) != 0 {
		t.Fatalf("no run since a fresh start must hold, got %+v", fn.alerts)
	}
	*now = now.Add(updateStaleAfter + time.Minute)
	a.observe(context.Background())
	if got := levels(fn.alerts); got != "open" || !strings.Contains(fn.alerts[0].Body, "n1") {
		t.Fatalf("two days without a run must warn and name the node, got %+v", fn.alerts)
	}
}

// A good verdict that is two days old is a node whose runs stopped finishing: the timer gone, the
// unit wedged. It warns, saying since when; a fresh good run clears it.
func TestUpdateAlerterStaleVerdict(t *testing.T) {
	fn := &fakeNotifier{}
	a, lay, now := newTestUpdateAlerter(t, fn)
	result(t, lay, "ok already at v1", now.Add(-updateStaleAfter-time.Hour))
	a.observe(context.Background())
	if got := levels(fn.alerts); got != "open" || !strings.Contains(fn.alerts[0].Body, "No update check has finished since") {
		t.Fatalf("a stale verdict must warn, got %+v", fn.alerts)
	}
	result(t, lay, "ok already at v1", *now)
	a.observe(context.Background())
	if got := levels(fn.alerts); got != "open,resolved" {
		t.Fatalf("a fresh run must clear it, got %q", got)
	}
}

// THE FREE TIER IS THE POINT. A node with no notifier configured delivers nowhere, and the
// alert store is then the whole of the alert -- it must still be recorded there, where
// `briard alerts` reads it.
func TestUpdateAlerterRecordsWithNoNotifier(t *testing.T) {
	a, lay, now := newTestUpdateAlerter(t, nil)
	result(t, lay, "failed could not fetch", *now)
	a.observe(context.Background())
	recs, err := ReadAlerts(a.n.(*alertStore).path)
	if err != nil {
		t.Fatal(err)
	}
	if open := notify.OpenNow(recs); len(open) != 1 || open[0].Key != "updates" {
		t.Fatalf("a nil notifier must still leave the alert in the store, got %+v", recs)
	}
}
