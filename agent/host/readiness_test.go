package host

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"briard.io/agent/guest"
	"briard.io/agent/hass"
	"briard.io/agent/mosquitto"
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
	// the probe half
	stored     string
	probes     int
	probeErr   error
	notServing int
	dropWrite  bool
	loseState  bool
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

// The probe half of the seam ([V3b.4]). `stored` is what the service is holding: a write puts the
// token there, a read hands it back — so a test makes the service LOSE its state by clearing it
// between the two calls, which is what a broken upgrade does.
func (p *probe) ServiceProbe(_ context.Context, _ string, token string) (mosquitto.Sample, error) {
	p.probes++
	if p.probeErr != nil {
		return mosquitto.Sample{}, p.probeErr
	}
	if p.notServing > 0 {
		p.notServing--
		return mosquitto.Sample{Serving: false}, nil
	}
	if token != "" {
		if p.dropWrite {
			return mosquitto.Sample{Serving: true}, nil // the write did not round-trip
		}
		p.stored = token
	}
	if p.loseState {
		p.stored = ""
	}
	return mosquitto.Sample{Serving: true, Token: p.stored}, nil
}

func (p *probe) SupportsServiceProbe() bool { return !p.old }

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
// judge the services it curates beyond liveness, and knows nothing about anything else. The
// negative half is the one that matters — the shipped default for every other service is the
// floor alone.
//
// The unknown name here USED to be "mosquitto", which is the nicest possible demonstration of
// what this test is for: the day the broker landed ([V3b.4]) it stopped being unknown, and the
// example had to move rather than the contract.
func TestAssessorIsKeyedOnTheServiceName(t *testing.T) {
	cfg := Config{}
	if a := cfg.assessorFor(hassManifest(), &probe{}, func(string, ...any) {}); a == nil {
		t.Fatal("home-assistant got no assessor")
	}
	// The broker gets a different KIND of assessor, not a copy of HA's: its work is invisible to
	// a sample, so it is judged on a token it was given rather than on what it reports.
	broker := hassManifest()
	broker.Name = mosquitto.Name
	switch a := cfg.assessorFor(broker, &probe{}, func(string, ...any) {}).(type) {
	case *probeAssessor:
	default:
		t.Fatalf("mosquitto got %T, not the probe assessor", a)
	}
	other := hassManifest()
	other.Name = "sample-app"
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

// judgeProbe runs one whole probe cycle the way an upgrade does: baseline while the old version
// serves, then assess after the new one is up. `breakIt` is what the upgrade did to the service in
// between.
func judgeProbe(t *testing.T, breakIt func(*probe)) (guest.Verdict, string, error) {
	t.Helper()
	m := hassManifest()
	m.Name = mosquitto.Name
	p := &probe{}
	a := Config{readinessSettle: time.Millisecond}.assessorFor(m, p, func(string, ...any) {})
	if a == nil {
		t.Fatal("mosquitto got no assessor")
	}
	base, err := a.Baseline(context.Background())
	if err != nil {
		t.Fatalf("Baseline: %v", err)
	}
	breakIt(p)
	return func() (guest.Verdict, string, error) { return a.Assess(context.Background(), base) }()
}

// TestProbeSurvivesAGoodUpgrade: the token the broker was given before the change is the token it
// hands back after, so nothing is held or reverted. The control for every case below — without it
// a gate that always said Rollback would pass all of them.
func TestProbeSurvivesAGoodUpgrade(t *testing.T) {
	v, reason, err := judgeProbe(t, func(*probe) {})
	if err != nil {
		t.Fatalf("Assess: %v", err)
	}
	if v != guest.VerdictPass {
		t.Errorf("a healthy upgrade was judged %s (%s)", v, reason)
	}
}

// TestProbeCatchesLostState is the failure the liveness floor cannot see: the service answers its
// health endpoint perfectly and has come back with an empty store. For a broker that is every
// retained message the household had.
func TestProbeCatchesLostState(t *testing.T) {
	v, reason, err := judgeProbe(t, func(p *probe) { p.loseState = true })
	if err != nil {
		t.Fatalf("Assess: %v", err)
	}
	if v != guest.VerdictRollback {
		t.Fatalf("a broker that lost its state was judged %s (%s)", v, reason)
	}
	if !strings.Contains(reason, "state") {
		t.Errorf("the reason does not say what was lost: %q", reason)
	}
}

// TestProbeCatchesAServiceThatServesNobody — the other invisible failure. The management endpoint
// the floor probes is not the port clients use, so a broker that refuses every connection answers
// the floor and fails here.
func TestProbeCatchesAServiceThatServesNobody(t *testing.T) {
	// Long enough to outlast the poll: this is a service that never comes back, not a slow one.
	v, reason, err := judgeProbe(t, func(p *probe) { p.notServing = 100 })
	if err != nil {
		t.Fatalf("Assess: %v", err)
	}
	if v != guest.VerdictRollback {
		t.Fatalf("a broker accepting no clients was judged %s (%s)", v, reason)
	}
	if !strings.Contains(reason, "no clients") {
		t.Errorf("the reason does not say what is wrong: %q", reason)
	}
}

// TestProbeWaitsForASlowStart: a service that is not serving on the first look but is on the
// second must PASS. Judging the first sample would turn every slow start into a rollback.
func TestProbeWaitsForASlowStart(t *testing.T) {
	v, reason, err := judgeProbe(t, func(p *probe) { p.notServing = 1 })
	if err != nil {
		t.Fatalf("Assess: %v", err)
	}
	if v != guest.VerdictPass {
		t.Errorf("a service that came up on the second look was judged %s (%s)", v, reason)
	}
}

// TestProbeRefusesToBaselineABrokenService is the confounder control, and it is the same rule the
// config-entry gate follows: a service that was ALREADY not serving before the change cannot have
// been broken by it. No baseline means the install runs on the liveness floor alone.
func TestProbeRefusesToBaselineABrokenService(t *testing.T) {
	m := hassManifest()
	m.Name = mosquitto.Name
	p := &probe{notServing: 1}
	a := Config{readinessSettle: time.Millisecond}.assessorFor(m, p, func(string, ...any) {})
	_, err := a.Baseline(context.Background())
	if err == nil {
		t.Fatal("a service that was not serving before the change produced a baseline")
	}
	// The REASON matters, and asserting it is what keeps this test from passing for the wrong one:
	// without it, the round-trip check below would refuse this baseline anyway and the
	// confounder rule could be deleted with every test still green.
	if !strings.Contains(err.Error(), "not serving clients") {
		t.Errorf("the refusal does not name the confounder: %v", err)
	}
}

// TestProbeRefusesABaselineThatDidNotStick: if the write did not round-trip, the gate has no
// evidence — and must say so rather than proceed to look for a token it never stored, which would
// report a healthy upgrade as data loss.
func TestProbeRefusesABaselineThatDidNotStick(t *testing.T) {
	m := hassManifest()
	m.Name = mosquitto.Name
	p := &probe{dropWrite: true}
	a := Config{readinessSettle: time.Millisecond}.assessorFor(m, p, func(string, ...any) {})
	_, err := a.Baseline(context.Background())
	if err == nil {
		t.Fatal("a probe token that never stored produced a baseline")
	}
	if !strings.Contains(err.Error(), "did not store") {
		t.Errorf("the refusal does not name the failed write: %v", err)
	}
}

// TestProbeFailureIsNeverAVerdict: S1 must not revert a household's service because its own
// telemetry broke. A probe that errors returns an error, never Rollback.
func TestProbeFailureIsNeverAVerdict(t *testing.T) {
	m := hassManifest()
	m.Name = mosquitto.Name
	p := &probe{}
	a := Config{readinessSettle: time.Millisecond}.assessorFor(m, p, func(string, ...any) {})
	base, err := a.Baseline(context.Background())
	if err != nil {
		t.Fatalf("Baseline: %v", err)
	}
	p.probeErr = errors.New("the guest is not answering")
	v, _, err := a.Assess(context.Background(), base)
	if err == nil {
		t.Fatalf("a broken probe produced a verdict: %s", v)
	}
	if v == guest.VerdictRollback {
		t.Error("a broken probe rolled a household's service back")
	}
}

// TestEveryUpgradeGetsAFreshToken. The topic is RETAINED, so a token left by a previous upgrade
// would still be there — and a broker that had lost everything since would hand it back, turning
// total data loss into a Pass.
func TestEveryUpgradeGetsAFreshToken(t *testing.T) {
	m := hassManifest()
	m.Name = mosquitto.Name
	seen := map[string]bool{}
	for i := 0; i < 3; i++ {
		p := &probe{}
		a := Config{readinessSettle: time.Millisecond}.assessorFor(m, p, func(string, ...any) {})
		base, err := a.Baseline(context.Background())
		if err != nil {
			t.Fatalf("Baseline: %v", err)
		}
		tok, _ := base.(string)
		if tok == "" {
			t.Fatal("the baseline carries no token")
		}
		if seen[tok] {
			t.Fatalf("the same probe token was minted twice: %q", tok)
		}
		seen[tok] = true
	}
}
