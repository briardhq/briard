package host

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"briard.io/agent/quadlet"
	"briard.io/shared/api"
)

// crashedUpgrade is the volume an upgrade leaves when its installer died inside the health gate:
// the prior manifest accepted, the new one staged, the rollback point recorded -- and nothing in
// any process's memory.
func crashedUpgrade(t *testing.T) (*fakeInstaller, string, string) {
	t.Helper()
	prior, priorRaw := priorManifest(t)
	staged, err := json.Marshal(testManifest())
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeInstaller{primary: true, active: true, prior: prior,
		staged:    map[string]string{"home-assistant": string(staged)},
		rollbacks: map[string]string{"home-assistant": "/snap/before"}}
	return f, priorRaw, string(staged)
}

// THE PROMISE THIS ITEM EXISTS FOR: an install nobody is running any more is undone from the
// volume alone, by the same procedure a failed gate uses -- stop, put the data back from the
// recorded point, discard the stage, converge to the accepted manifest -- in that order.
func TestAStagedInstallIsUndoneFromTheVolumeAlone(t *testing.T) {
	f, priorRaw, _ := crashedUpgrade(t)
	cfg := Config{}
	if !cfg.undoPending(context.Background(), f, func(string, ...any) {}) {
		t.Fatal("the check did not settle")
	}
	joined := strings.Join(f.steps, ",")
	order := []string{"stop:briard-home-assistant-ha.service", "restore:/snap/before", "discard:home-assistant", "converge"}
	at := -1
	for _, step := range order {
		i := strings.Index(joined, step)
		if i < 0 || i < at {
			t.Fatalf("want %v in that order, got %v", order, f.steps)
		}
		at = i
	}
	if strings.Contains(joined, "converge:") {
		t.Fatalf("the undo converged with an install live: %v", f.steps)
	}
	if f.prior["home-assistant"] != priorRaw || len(f.staged) != 0 {
		t.Fatalf("volume after the undo: accepted %q, staged %v", f.prior["home-assistant"], f.staged)
	}
}

// THE UNDO'S OWN POINT IS PINNED TO THE VERSION THAT RAN, which the volume never accepted -- read
// from the stage, not from the accepted manifest, or the history would say the data under the
// failed version belongs to the prior one and "redo" would put back the wrong code.
func TestTheUndoPointIsPinnedToTheStagedManifest(t *testing.T) {
	f, _, staged := crashedUpgrade(t)
	cfg := Config{clock: func() time.Time { return fixedNow }}
	cfg.undoPending(context.Background(), f, func(string, ...any) {})
	if len(f.sidecars) != 1 {
		t.Fatalf("undo points taken: %d, want 1", len(f.sidecars))
	}
	var meta quadlet.SnapshotMeta
	if err := json.Unmarshal([]byte(f.sidecars[0]), &meta); err != nil {
		t.Fatal(err)
	}
	if meta.Manifest != staged || meta.Trigger != quadlet.TriggerAppUndoBefore {
		t.Fatalf("undo point pinned to %q (%s), want the staged manifest", meta.Manifest, meta.Trigger)
	}
}

// A FRESH install has nothing to put back: no restore, the stage discarded, and the service gone.
func TestAStagedFreshInstallIsDiscarded(t *testing.T) {
	staged, _ := json.Marshal(testManifest())
	f := &fakeInstaller{staged: map[string]string{"home-assistant": string(staged)}, rollbacks: map[string]string{"home-assistant": ""}}
	Config{}.undoPending(context.Background(), f, func(string, ...any) {})
	if strings.Contains(strings.Join(f.steps, ","), "restore") {
		t.Fatalf("a fresh install's undo restored data: %v", f.steps)
	}
	if _, ok := f.prior["home-assistant"]; ok || len(f.staged) != 0 {
		t.Fatalf("the fresh install survived its undo: accepted %v, staged %v", f.prior, f.staged)
	}
}

// A RESTORE THAT FAILS LEAVES THE STAGE IN PLACE: converge keeps holding the service back, so the
// prior version is never started on data that was not put back, and the next promotion tries again.
func TestAnUndoWhoseRestoreFailsKeepsTheServiceHeldBack(t *testing.T) {
	f, _, _ := crashedUpgrade(t)
	f.restoreEr = errors.New("btrfs: cannot delete subvolume")
	Config{}.undoPending(context.Background(), f, func(string, ...any) {})
	joined := strings.Join(f.steps, ",")
	if strings.Contains(joined, "discard:") || strings.Contains(joined, "converge") {
		t.Fatalf("a failed restore went on to discard/converge: %v", f.steps)
	}
	if len(f.staged) != 1 {
		t.Fatal("the stage was dropped though the data was not put back")
	}
}

// AN INSTALL IN PROGRESS IS NOT A CRASHED ONE: while an act holds the slot, the check touches
// nothing and asks again later.
func TestUndoWaitsWhileAnActRuns(t *testing.T) {
	f, _, _ := crashedUpgrade(t)
	cfg := Config{acts: newActLane()}
	if _, ok := cfg.acts.claim(api.DirectiveServiceInstall); !ok {
		t.Fatal("could not take the act slot")
	}
	if cfg.undoPending(context.Background(), f, func(string, ...any) {}) {
		t.Fatal("the check settled while an install was in progress")
	}
	if len(f.steps) != 0 {
		t.Fatalf("the check touched the guest while an act ran: %v", f.steps)
	}
}

// A COMMIT THAT FAILS IS UNDONE like a failed gate: a staged install must never outlive the act
// that staged it with the act reporting success.
func TestAFailedCommitIsUndone(t *testing.T) {
	cfg := catalogFor(t, testManifest())
	cfg.readinessSettle = time.Millisecond
	f := &fakeInstaller{primary: true, active: true, healthy: true, prior: mustPrior(t), commitEr: errors.New("mv: I/O error")}
	o := installService(cfg, f)
	if o.State != api.OutcomeRolledBack {
		t.Fatalf("outcome = %+v, want rolled-back", o)
	}
	if !strings.Contains(strings.Join(f.steps, ","), "discard:home-assistant") || len(f.staged) != 0 {
		t.Fatalf("the uncommitted install was not undone: %v", f.steps)
	}
}
