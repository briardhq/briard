package host

import (
	"context"
	"errors"
	"slices"
	"testing"

	"briard.io/shared/model"
	"briard.io/shared/telemetry"
)

// resources() composes the appliance telemetry from the guest with the agent's own
// footprint measured in-process. The agent fields are always present (this process really
// has an RSS); the appliance fields come from the guest read.
func TestResourcesComposesAgentAndAppliance(t *testing.T) {
	cfg := Config{Services: []model.ServiceSpec{{Name: "ha", DataDir: "/d"}}}
	r := fakeStatus{res: telemetry.NodeResources{
		Payloads:      []telemetry.ServiceResources{{Name: "ha", RSSKB: 88000}},
		SnapshotCount: 3, VolumeUsedKB: 4200,
	}}

	got := cfg.resources(context.Background(), r)
	if got == nil {
		t.Fatal("resources must never be nil (agent footprint always measures)")
	}
	if len(got.Payloads) != 1 || got.Payloads[0].RSSKB != 88000 || got.SnapshotCount != 3 || got.VolumeUsedKB != 4200 {
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
	if len(got.Payloads) != 0 || got.VolumeUsedKB != 0 {
		t.Errorf("appliance fields should be zero on guest error, got %+v", got)
	}
	if got.AgentRSSKB <= 0 {
		t.Errorf("agent footprint should still be measured, got %d", got.AgentRSSKB)
	}
}

// A ZERO-SERVICE node still reads the appliance, and this test used to assert the opposite.
//
// It was written for a witness -- "no payload, so skip the guest read" -- but the gate it asserted
// was "is there exactly one service?", which is equally true of the shipped zero-service anchor:
// what install.sh leaves behind, and the whole free tier. Those nodes have a guest, and load
// average, journal size, podman-store size and the guest's KERNEL ERRORS are node-scoped facts
// that need neither a unit nor a data dir. Skipping the read lost all of them, so a zero-service
// anchor's kernel problems were invisible to the soak oracle. Same bug as [V3b.3](d), one layer
// out, and the same lesson: the test had encoded it.
//
// Rewritten to assert the read HAPPENS and carries the node-scoped series, with no per-service
// entries because there are no services. Confirmed to fail against the old gate before it passed
// against the new one ([[verification-assertions-must-fail]]).
func TestResourcesZeroServiceNodeStillReadsTheAppliance(t *testing.T) {
	cfg := Config{} // the empty set: nothing installed, the shipped state
	r := &recordingReader{fakeStatus: fakeStatus{res: telemetry.NodeResources{Load1: 0.4, LogSizeKB: 900, KernelErrors: []string{"oops"}}}}

	got := cfg.resources(context.Background(), r)
	if !r.called {
		t.Fatal("a zero-service node skipped the appliance read -- load, journal size and the guest kernel log go dark with it")
	}
	if got.Load1 != 0.4 || got.LogSizeKB != 900 || len(got.KernelErrors) != 1 {
		t.Errorf("node-scoped series not carried through: %+v", got)
	}
	if len(got.Payloads) != 0 {
		t.Errorf("nothing is installed, so there is no per-service footprint to report: %+v", got.Payloads)
	}
	if len(r.gotServices) != 0 {
		t.Errorf("asked the guest about %v, want no services named", r.gotServices)
	}
	if got.AgentRSSKB <= 0 {
		t.Errorf("the agent footprint should still be measured, got %d", got.AgentRSSKB)
	}
}

// recordingReader captures WHICH units the telemetry probe asked systemd about -- the whole point
// of the pairing, so the assertion has to be on the argument, not on the returned numbers.
type recordingReader struct {
	fakeStatus
	called      bool
	gotServices map[string]string
	gotDataDir  string
}

func (r *recordingReader) Resources(_ context.Context, services map[string]string, dataDir string) (telemetry.NodeResources, error) {
	r.called = true
	r.gotServices, r.gotDataDir = services, dataDir
	return r.res, r.resErr
}

// The probe must ask about the unit that actually serves. A service's units come from the quadlet
// renderer, so rebuilding "podman-<name>.service" here named nothing and every metric -- including
// the crash-loop counter -- read zero for exactly the services users install.
func TestResourcesProbesTheServingUnit(t *testing.T) {
	cfg := Config{Services: []model.ServiceSpec{{
		Name:    "home-assistant",
		DataDir: "/var/lib/briard/services/home-assistant",
		Unit:    "briard-home-assistant-app.service",
	}}}
	r := &recordingReader{}
	cfg.resources(context.Background(), r)
	if r.gotServices["home-assistant"] != "briard-home-assistant-app.service" {
		t.Errorf("probed units = %v, want the quadlet-rendered serving unit", r.gotServices)
	}
	if r.gotDataDir != "/var/lib/briard/services/home-assistant" {
		t.Errorf("probed data dir = %q", r.gotDataDir)
	}
}

// N SERVICES, EACH MEASURED SEPARATELY ([V3b.3](b)). Summing them was the alternative and it is
// the one that loses the signal: NRestarts exists to make a crash-loop loud, and one service
// flapping inside a total reads as a small climb. So the probe names every service and the oracle
// gets one series per name.
func TestResourcesMeasuresEveryService(t *testing.T) {
	cfg := Config{Services: []model.ServiceSpec{
		{Name: "home-assistant", DataDir: "/var/lib/briard/home-assistant", Unit: "briard-home-assistant-app.service"},
		{Name: "mosquitto", DataDir: "/var/lib/briard/mosquitto", Unit: "briard-mosquitto-broker.service"},
	}}
	r := &recordingReader{}
	cfg.resources(context.Background(), r)
	want := map[string]string{
		"home-assistant": "briard-home-assistant-app.service",
		"mosquitto":      "briard-mosquitto-broker.service",
	}
	if len(r.gotServices) != 2 {
		t.Fatalf("probed %v, want both services -- one of N going unmeasured is the whole failure", r.gotServices)
	}
	for name, unit := range want {
		if r.gotServices[name] != unit {
			t.Errorf("%s probed as %q, want %q", name, r.gotServices[name], unit)
		}
	}
	if !slices.Contains([]string{"/var/lib/briard/home-assistant", "/var/lib/briard/mosquitto"}, r.gotDataDir) {
		t.Errorf("data dir = %q, want one of the services' -- df/btrfs measure the shared volume", r.gotDataDir)
	}
}
