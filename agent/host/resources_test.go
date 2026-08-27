package host

import (
	"context"
	"errors"
	"testing"

	"briard.io/shared/model"
	"briard.io/shared/telemetry"
)

// resources() composes the appliance telemetry from the guest with the agent's own
// footprint measured in-process. The agent fields are always present (this process really
// has an RSS); the appliance fields come from the guest read.
func TestResourcesComposesAgentAndAppliance(t *testing.T) {
	cfg := Config{Services: []model.ServiceSpec{{Name: "ha", DataDir: "/d"}}}
	r := fakeStatus{res: telemetry.NodeResources{PayloadRSSKB: 88000, SnapshotCount: 3, VolumeUsedKB: 4200}}

	got := cfg.resources(context.Background(), r)
	if got == nil {
		t.Fatal("resources must never be nil (agent footprint always measures)")
	}
	if got.PayloadRSSKB != 88000 || got.SnapshotCount != 3 || got.VolumeUsedKB != 4200 {
		t.Errorf("appliance fields not carried through: %+v", got)
	}
	if got.AgentRSSKB <= 0 {
		t.Errorf("agent RSS should be measured from /proc/self, got %d", got.AgentRSSKB)
	}
	if got.AgentFDs <= 0 {
		t.Errorf("agent fd count should be measured from /proc/self, got %d", got.AgentFDs)
	}
}

// A guest read error degrades to agent-only telemetry rather than blanking the report or
// breaking the observe loop -- the appliance fields stay zero, the agent footprint is kept.
func TestResourcesGuestErrorDegradesToAgentOnly(t *testing.T) {
	cfg := Config{Services: []model.ServiceSpec{{Name: "ha", DataDir: "/d"}}}
	r := fakeStatus{resErr: errors.New("control channel hiccup")}

	got := cfg.resources(context.Background(), r)
	if got == nil {
		t.Fatal("resources must never be nil")
	}
	if got.PayloadRSSKB != 0 || got.VolumeUsedKB != 0 {
		t.Errorf("appliance fields should be zero on guest error, got %+v", got)
	}
	if got.AgentRSSKB <= 0 {
		t.Errorf("agent footprint should still be measured, got %d", got.AgentRSSKB)
	}
}

// A witness (no payload) skips the guest read entirely but still reports its own agent
// footprint -- an agent leak on a witness is still a soak signal.
func TestResourcesWitnessReportsAgentOnly(t *testing.T) {
	cfg := Config{} // the empty set: nothing installed
	called := false
	r := witnessReader{onResources: func() { called = true }}

	got := cfg.resources(context.Background(), r)
	if called {
		t.Error("witness must not read appliance telemetry from the guest")
	}
	if got.AgentRSSKB <= 0 {
		t.Errorf("witness should still report its agent footprint, got %d", got.AgentRSSKB)
	}
}

// witnessReader asserts Resources is never called on the witness path.
type witnessReader struct {
	fakeStatus
	onResources func()
}

func (w witnessReader) Resources(context.Context, string, string) (telemetry.NodeResources, error) {
	w.onResources()
	return telemetry.NodeResources{}, nil
}

// recordingReader captures what unit the telemetry probe asked systemd about -- the whole point of
// the fix, so the assertion has to be on the argument, not on the returned numbers.
type recordingReader struct {
	fakeStatus
	gotUnit    string
	gotDataDir string
}

func (r *recordingReader) Resources(_ context.Context, unit, dataDir string) (telemetry.NodeResources, error) {
	r.gotUnit, r.gotDataDir = unit, dataDir
	return r.res, r.resErr
}

// The probe must ask about the unit that actually serves. A runtime-installed service's units come
// from the quadlet renderer, so rebuilding "podman-<name>.service" here named nothing and every
// payload metric -- including the crash-loop counter -- read zero for exactly the services users
// install. The baked slot keeps its derived name.
func TestResourcesProbesTheServingUnit(t *testing.T) {
	t.Run("runtime-installed service", func(t *testing.T) {
		cfg := Config{Services: []model.ServiceSpec{{
			Name:    "home-assistant",
			DataDir: "/var/lib/briard/services/home-assistant",
			Unit:    "briard-home-assistant-app.service",
		}}}
		r := &recordingReader{}
		cfg.resources(context.Background(), r)
		if r.gotUnit != "briard-home-assistant-app.service" {
			t.Errorf("probed unit = %q, want the quadlet-rendered serving unit", r.gotUnit)
		}
		if r.gotDataDir != "/var/lib/briard/services/home-assistant" {
			t.Errorf("probed data dir = %q", r.gotDataDir)
		}
	})
	t.Run("baked slot keeps its derived name", func(t *testing.T) {
		cfg := Config{Services: []model.ServiceSpec{{Name: "briard-payload", DataDir: "/d"}}}
		r := &recordingReader{}
		cfg.resources(context.Background(), r)
		if r.gotUnit != "podman-briard-payload.service" {
			t.Errorf("probed unit = %q, want the baked slot's derived name", r.gotUnit)
		}
	})
}
