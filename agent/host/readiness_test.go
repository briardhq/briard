package host

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"briard.io/agent/guest"
	"briard.io/agent/hass"
	"briard.io/shared/manifest"
)

// probe queues the samples an assessor will read: the first call is the baseline, the second the
// settled post-change sample.
type probe struct {
	samples [][]hass.Entry
	err     error
	old     bool
	calls   int
	ports   []int
}

func (p *probe) ServiceReadiness(_ context.Context, _ string, port int) ([]hass.Entry, error) {
	p.calls++
	p.ports = append(p.ports, port)
	if p.err != nil {
		return nil, p.err
	}
	if p.calls <= len(p.samples) {
		return p.samples[p.calls-1], nil
	}
	return nil, nil
}

func (p *probe) SupportsServiceReadiness() bool { return !p.old }

func entriesOf(states ...string) []hass.Entry {
	out := make([]hass.Entry, len(states))
	for i, st := range states {
		out[i] = hass.Entry{ID: string(rune('a' + i)), Domain: "d" + string(rune('a'+i)), State: st}
	}
	return out
}

func hassManifest() manifest.Manifest { return testManifest() }

// judge runs one whole gate cycle over a pre/post pair, the way an upgrade does.
func judge(t *testing.T, pre, post []hass.Entry) (guest.Verdict, string) {
	t.Helper()
	p := &probe{samples: [][]hass.Entry{pre, post}}
	a := Config{readinessSettle: time.Millisecond}.assessorFor(hassManifest(), p, func(string, ...any) {})
	if a == nil {
		t.Fatal("home-assistant got no assessor")
	}
	base, err := a.Baseline(context.Background())
	if err != nil {
		t.Fatalf("Baseline: %v", err)
	}
	v, reason, err := a.Assess(context.Background(), base)
	if err != nil {
		t.Fatalf("Assess: %v", err)
	}
	return v, reason
}

// TestAssessorIsKeyedOnTheServiceName is the registry's whole contract: the product knows how to
// judge Home Assistant beyond liveness, and knows nothing about anything else. The negative half
// is the one that matters — the shipped default for every service is the floor alone, which is
// what mosquitto and every future catalog entry get on the day they land ([V3b.4]).
func TestAssessorIsKeyedOnTheServiceName(t *testing.T) {
	cfg := Config{}
	if a := cfg.assessorFor(hassManifest(), &probe{}, func(string, ...any) {}); a == nil {
		t.Fatal("home-assistant got no assessor")
	}
	other := hassManifest()
	other.Name = "mosquitto"
	if a := cfg.assessorFor(other, &probe{}, func(string, ...any) {}); a != nil {
		t.Fatalf("a service the product knows nothing about got an assessor: %T", a)
	}
}

// TestAssessorUsesTheManifestsPort: the port is declared once, in the catalog, and read from
// there — the same field the liveness floor builds its URL from. A hardcoded 8123 would be a
// second place for the catalog to be wrong about.
func TestAssessorUsesTheManifestsPort(t *testing.T) {
	m := hassManifest()
	m.Containers[0].Port = 9123
	p := &probe{samples: [][]hass.Entry{nil}}
	a := Config{readinessSettle: time.Millisecond}.assessorFor(m, p, func(string, ...any) {})
	if _, err := a.Baseline(context.Background()); err != nil {
		t.Fatalf("Baseline: %v", err)
	}
	if len(p.ports) != 1 || p.ports[0] != 9123 {
		t.Fatalf("sampled ports = %v, want [9123] from the manifest", p.ports)
	}
}

// TestAssessorDegradesOnAnOldGuest: a guest that cannot sample readiness leaves the install on
// the liveness floor — the behaviour every node had before the gate existed. Degrade, never
// refuse: an install must not fail because the layer ABOVE its floor is unavailable, which is
// the opposite call from service.installed/service.converge (an install is wrong without those).
func TestAssessorDegradesOnAnOldGuest(t *testing.T) {
	var logged strings.Builder
	a := Config{}.assessorFor(hassManifest(), &probe{old: true}, func(f string, args ...any) {
		logged.WriteString(f)
	})
	if a != nil {
		t.Fatalf("an old guest got an assessor: %T", a)
	}
	if !strings.Contains(logged.String(), "service.readiness") {
		t.Fatalf("the degrade was silent; logged %q", logged.String())
	}
}

// TestGateRollsBackOnACluster: two integrations that were loaded and went terminally failed is
// the high-confidence signal — HA parks environmental failures in setup_retry, so simultaneous
// terminal failures are a version bug, not the weather.
func TestGateRollsBackOnACluster(t *testing.T) {
	v, reason := judge(t,
		entriesOf("loaded", "loaded"),
		entriesOf("setup_error", "setup_error"))
	if v != guest.VerdictRollback {
		t.Fatalf("verdict = %q (%s), want rollback", v, reason)
	}
}

// TestGateHoldsOnASingleRegression: the ambiguous middle. One integration going terminal might
// be a genuinely removed or changed one, so the gate surfaces it and leaves the upgrade standing
// rather than reverting a household's service on one entry.
func TestGateHoldsOnASingleRegression(t *testing.T) {
	v, reason := judge(t,
		entriesOf("loaded", "loaded"),
		entriesOf("setup_error", "loaded"))
	if v != guest.VerdictHold {
		t.Fatalf("verdict = %q (%s), want hold", v, reason)
	}
}

// TestGateExcludesWhatWasAlreadyBroken is the reason the gate is differential at all. An
// integration failing BEFORE the upgrade cannot be the upgrade's fault, and a gate that could not
// tell the difference would revert every household with one flaky device on it.
func TestGateExcludesWhatWasAlreadyBroken(t *testing.T) {
	v, reason := judge(t,
		entriesOf("setup_error", "setup_error"),
		entriesOf("setup_error", "setup_error"))
	if v != guest.VerdictPass {
		t.Fatalf("verdict = %q (%s), want pass — nothing regressed", v, reason)
	}
}

// TestGatePassesAHealthyUpgrade: the common case must be silent, or the gate is a nuisance that
// gets turned off.
func TestGatePassesAHealthyUpgrade(t *testing.T) {
	if v, reason := judge(t, entriesOf("loaded", "loaded"), entriesOf("loaded", "loaded")); v != guest.VerdictPass {
		t.Fatalf("verdict = %q (%s), want pass", v, reason)
	}
}

// TestGateRefusesToJudgeUnsettled: out of budget before the signal could settle is an ERROR, not
// a verdict. An unsettled sample judged as though it were settled is exactly how HA's retry
// backoff turns into a rollback of a working upgrade — and the Gate turns this error into "keep",
// which is the safe direction.
func TestGateRefusesToJudgeUnsettled(t *testing.T) {
	p := &probe{samples: [][]hass.Entry{entriesOf("loaded"), entriesOf("setup_error")}}
	a := Config{readinessSettle: time.Hour}.assessorFor(hassManifest(), p, func(string, ...any) {})
	base, err := a.Baseline(context.Background())
	if err != nil {
		t.Fatalf("Baseline: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := a.Assess(ctx, base); err == nil {
		t.Fatal("an unsettled signal was judged anyway")
	}
	// And the Gate's policy over that error: keep, never revert on our own telemetry failing.
	if err := (guest.Gate{Assessor: a, Logf: func(string, ...any) {}}).Judge(ctx, guest.Readiness{}); err != nil {
		t.Fatalf("an uncaptured baseline must be a no-op, got %v", err)
	}
}

// TestGateKeepsWhenTheSampleFails: S1 never reverts a household's service because its own
// telemetry broke. A sample that errors is not evidence of a regression.
func TestGateKeepsWhenTheSampleFails(t *testing.T) {
	p := &probe{samples: [][]hass.Entry{entriesOf("loaded")}}
	a := Config{readinessSettle: time.Millisecond}.assessorFor(hassManifest(), p, func(string, ...any) {})
	gate := guest.Gate{Assessor: a, Logf: func(string, ...any) {}}
	rd := gate.Capture(context.Background())
	p.err = errors.New("connection refused")
	if err := gate.Judge(context.Background(), rd); err != nil {
		t.Fatalf("a failed post-sample reverted the upgrade: %v", err)
	}
}

// TestGateWithNoAssessorIsTheFloor: the zero Readiness is what every non-Home-Assistant install
// carries, and it must cost nothing and decide nothing.
func TestGateWithNoAssessorIsTheFloor(t *testing.T) {
	gate := guest.Gate{Logf: func(string, ...any) {}}
	rd := gate.Capture(context.Background())
	if err := gate.Judge(context.Background(), rd); err != nil {
		t.Fatalf("floor-only gate returned %v", err)
	}
}
