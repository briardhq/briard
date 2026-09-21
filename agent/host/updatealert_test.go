package host

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"briard.io/agent/selfupdate"
	"briard.io/shared/notify"
)

// newTestUpdateAlerter builds an alerter over a scripted systemd and a temp run dir. The
// readings are consumed one per observe; running past the end repeats the last, so a test can
// say "and then it stays that way" without padding the script.
func newTestUpdateAlerter(t *testing.T, n notify.Notifier, readings ...string) (*updateAlerter, *selfupdate.Layout, *[]string) {
	t.Helper()
	run := t.TempDir()
	lay := selfupdate.New(t.TempDir(), run)
	var trail []string
	i := 0
	a := newUpdateAlerter(n, "n1", lay, func(f string, v ...any) { trail = append(trail, fmt.Sprintf(f, v...)) })
	a.state = func(string) (string, error) {
		s := readings[i]
		if i < len(readings)-1 {
			i++
		}
		if s == "ERR" {
			return "", errors.New("systemctl: connection refused")
		}
		return s, nil
	}
	return a, &lay, &trail
}

func levels(as []notify.Alert) string {
	var out []string
	for _, a := range as {
		out = append(out, string(a.Level))
	}
	return strings.Join(out, ",")
}

// The contract: one warning when the node stops updating, silence while it stays stopped, one
// recovered when it updates again. Same no-fatigue shape as the redundancy alerter.
func TestUpdateAlerterEdges(t *testing.T) {
	fn := &fakeNotifier{}
	a, _, _ := newTestUpdateAlerter(t, fn, "inactive", "inactive", "failed", "failed", "failed", "inactive")
	ctx := context.Background()

	a.observe(ctx) // healthy
	a.observe(ctx) // steady healthy
	if len(fn.alerts) != 0 {
		t.Fatalf("no alert expected while updates work, got %+v", fn.alerts)
	}
	a.observe(ctx) // the run failed -> warning
	a.observe(ctx) // still failing -- must NOT re-fire
	a.observe(ctx)
	if got := levels(fn.alerts); got != "warning" {
		t.Fatalf("expected exactly one warning across a steady failure, got %q (%+v)", got, fn.alerts)
	}
	a.observe(ctx) // updating again -> recovered
	if got := levels(fn.alerts); got != "warning,recovered" {
		t.Fatalf("expected warning then recovered, got %q", got)
	}
}

// ⚠️ THE REGRESSION THIS ITEM IS ABOUT. The alerter must NOT prime: a node that was already
// failing when the agent came up is the steady state of the case [B.161](a) exists for (a
// floored node's agent is the one agent that never gets replaced), so the FIRST reading must
// fire. An alerter that primed like the redundancy one would be silent here forever, and this
// test is what fails when somebody adds that symmetry back.
func TestUpdateAlerterFiresOnTheFirstReading(t *testing.T) {
	fn := &fakeNotifier{}
	a, _, _ := newTestUpdateAlerter(t, fn, "failed")
	a.observe(context.Background())
	if got := levels(fn.alerts); got != "warning" {
		t.Fatalf("a first reading of `failed` must warn (no priming), got %q", got)
	}
}

// A query that failed is not an answer about the unit, in EITHER direction: it must not be read
// as healthy (which would swallow the next warning) nor as failing (which would invent one).
func TestUpdateAlerterHoldsOnQueryError(t *testing.T) {
	fn := &fakeNotifier{}
	a, _, trail := newTestUpdateAlerter(t, fn, "ERR", "failed")
	ctx := context.Background()
	a.observe(ctx)
	if len(fn.alerts) != 0 {
		t.Fatalf("a failed query must not fire, got %+v", fn.alerts)
	}
	if a.failing {
		t.Fatal("a failed query must not move the edge state")
	}
	if len(*trail) == 0 {
		t.Fatal("a failed query must leave a line in the journal")
	}
	a.observe(ctx) // the real reading still gets through
	if got := levels(fn.alerts); got != "warning" {
		t.Fatalf("expected the warning after the query recovered, got %q", got)
	}
}

// A run in flight has no verdict yet, and neither does a state this code has never heard of.
// Both hold, and holding must not consume the transition that follows.
func TestUpdateAlerterHoldsWhileRunning(t *testing.T) {
	for _, state := range []string{"activating", "deactivating", "active", "reloading", ""} {
		t.Run(state, func(t *testing.T) {
			fn := &fakeNotifier{}
			a, _, _ := newTestUpdateAlerter(t, fn, state, "failed")
			ctx := context.Background()
			a.observe(ctx)
			if len(fn.alerts) != 0 {
				t.Fatalf("%q has no verdict and must not fire, got %+v", state, fn.alerts)
			}
			a.observe(ctx)
			if got := levels(fn.alerts); got != "warning" {
				t.Fatalf("the verdict after %q must still fire, got %q", state, got)
			}
		})
	}
}

// The remedy lives in the update run's own last line -- for a floored node, the refusal that
// says in so many words that it must be reinstalled. The warning must carry it, and must leave
// the message on disk: the CLI and the cloud consume that file, this one only reads it.
func TestUpdateAlerterCarriesTheRunsLastLine(t *testing.T) {
	fn := &fakeNotifier{}
	a, lay, _ := newTestUpdateAlerter(t, fn, "failed")
	line := "install: this node is too old to upgrade to that release: installed v3.20260910.64a7834 is older than v3.20260921.5d18301's min_upgrade_from v3.20260920.ec4d22a — this node cannot be upgraded to it and must be reinstalled"
	if err := os.WriteFile(lay.ResultPath(), []byte(line+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	a.observe(context.Background())
	if len(fn.alerts) != 1 {
		t.Fatalf("expected one alert, got %+v", fn.alerts)
	}
	if !strings.Contains(fn.alerts[0].Body, "must be reinstalled") {
		t.Fatalf("the alert must carry the run's own remedy, got %q", fn.alerts[0].Body)
	}
	if _, err := os.Stat(lay.ResultPath()); err != nil {
		t.Fatalf("the result message must survive being read: %v", err)
	}
}

// Missing that message is not a reason to stay quiet: the node has still stopped updating.
func TestUpdateAlerterWarnsWithoutAResultLine(t *testing.T) {
	fn := &fakeNotifier{}
	a, lay, _ := newTestUpdateAlerter(t, fn, "failed")
	if _, err := os.Stat(lay.ResultPath()); err == nil {
		t.Fatal("precondition: no result message")
	}
	a.observe(context.Background())
	if got := levels(fn.alerts); got != "warning" {
		t.Fatalf("expected a warning with no result line, got %q", got)
	}
	if !strings.Contains(fn.alerts[0].Body, "n1") {
		t.Fatalf("the alert must still name the node, got %q", fn.alerts[0].Body)
	}
}

// THE FREE TIER IS THE POINT. A node with no notifier configured delivers nowhere, and the
// local trail is then the whole of the alert -- it must still be written, in the shape
// `briard alerts` greps for.
func TestUpdateAlerterWritesTheTrailWithNoNotifier(t *testing.T) {
	a, _, trail := newTestUpdateAlerter(t, nil, "failed")
	a.observe(context.Background())
	var found bool
	for _, l := range *trail {
		if strings.HasPrefix(l, notify.LogMarker) {
			found = true
		}
	}
	if !found {
		t.Fatalf("a nil notifier must still leave the %q trail, got %q", notify.LogMarker, *trail)
	}
}

// The alerter reads the FROZEN unit, not the agent's own. Naming the wrong one would watch the
// thing that is running this code and never report the thing that replaces it.
func TestUpdateAlerterWatchesTheFrozenUnit(t *testing.T) {
	a, _, _ := newTestUpdateAlerter(t, nil, "inactive")
	if a.unit != selfupdate.DefaultUpdateUnit {
		t.Fatalf("alerter watches %q, want the frozen update unit %q", a.unit, selfupdate.DefaultUpdateUnit)
	}
	if filepath.Base(a.unit) == "briard-agent.service" {
		t.Fatal("that is the agent's own unit, not the updater")
	}
}
