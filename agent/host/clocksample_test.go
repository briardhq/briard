package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"briard.io/agent/quadlet"
	"briard.io/shared/model"
)

// The clock sample's fixtures ([B.143], [B.167]). `night` is any moment; the sampler has no window.
var night = time.Date(2026, 9, 23, 3, 12, 0, 0, time.Local)

func clockFixture(t *testing.T) (Config, *fakeStatus, *[]takenMember, *clockSampler) {
	t.Helper()
	took := &[]takenMember{}
	f := &fakeStatus{took: took, volume: map[string]string{"home-assistant": `{"name":"home-assistant"}`}}
	cfg := Config{
		Services: []model.ServiceSpec{{Name: "home-assistant"}},
		clock:    func() time.Time { return night },
	}
	return cfg, f, took, newClockSampler()
}

// TestClockSampleTakesOneAnInterval is the history's floor ([B.167]): a service nobody restarts is
// still compared with an hour ago -- and only once an hour, because the observe loop asks on every
// cycle and a sampler that took one each time would snapshot a household every few seconds.
func TestClockSampleTakesOneAnInterval(t *testing.T) {
	cfg, f, took, n := clockFixture(t)
	cfg.consider(context.Background(), f, n, cfg.Services, true, night, func(string, ...any) {})
	if len(*took) != 1 {
		t.Fatalf("took %d members, want one: %v", len(*took), *took)
	}
	svc, tr, _, ok := quadlet.ParseSnapshotMember((*took)[0].member)
	if !ok || svc != "home-assistant" || tr != quadlet.TriggerClock {
		t.Errorf("member = %q, want home-assistant's clock sample", (*took)[0].member)
	}
	cfg.consider(context.Background(), f, n, cfg.Services, true, night.Add(30*time.Minute), func(string, ...any) {})
	if len(*took) != 1 {
		t.Errorf("a cycle inside the interval took another member: %v", *took)
	}
	cfg.consider(context.Background(), f, n, cfg.Services, true, night.Add(clockInterval), func(string, ...any) {})
	if len(*took) != 2 {
		t.Errorf("took %d members after an interval, want the next one", len(*took))
	}
}

// TestClockSampleCountsAnyRecentMember: "hourly unless a recent one exists". A start the guest
// sampled is a sample, and an agent that restarted with an empty record must read the RING rather
// than take a member of bytes that have not changed.
func TestClockSampleCountsAnyRecentMember(t *testing.T) {
	cfg, f, took, _ := clockFixture(t)
	start := night.Add(-10 * time.Minute)
	f.members = map[string][]quadlet.SnapshotEntry{"home-assistant": {{
		Member: quadlet.SnapshotMember("home-assistant", quadlet.TriggerStart, start),
		Meta:   quadlet.SnapshotMeta{Trigger: quadlet.TriggerStart, TakenAt: start},
	}}}
	cfg.consider(context.Background(), f, newClockSampler(), cfg.Services, true, night, func(string, ...any) {})
	if len(*took) != 0 {
		t.Errorf("a ten-minute-old start member did not count: %v", *took)
	}
	old := night.Add(-2 * time.Hour)
	f.members["home-assistant"][0].Member = quadlet.SnapshotMember("home-assistant", quadlet.TriggerStart, old)
	cfg.consider(context.Background(), f, newClockSampler(), cfg.Services, true, night, func(string, ...any) {})
	if len(*took) != 1 {
		t.Errorf("a two-hour-old member stopped the sample being taken: %v", *took)
	}
}

// TestClockSampleFailureWaitsAnInterval: a volume that is having trouble is not helped by being
// asked every cycle, and the next sample costs the same as this one.
func TestClockSampleFailureWaitsAnInterval(t *testing.T) {
	cfg, f, took, n := clockFixture(t)
	f.snapErr = errors.New("no space left on device")
	var logged int
	logf := func(string, ...any) { logged++ }
	for i := 0; i < 5; i++ {
		cfg.consider(context.Background(), f, n, cfg.Services, true, night.Add(time.Duration(i)*time.Minute), logf)
	}
	if len(*took) != 1 {
		t.Errorf("a failing take was retried inside the interval: %d attempts", len(*took))
	}
	if logged == 0 {
		t.Error("the failure was silent; a ring that stopped filling must say so")
	}
}

// TestClockSampleOnlyOnTheServingNode: the volume is mounted on one node. A secondary asking for a
// member is asking about data it does not hold, and the guest would refuse -- but the host must
// not ask, because "the ring is per volume" is a property of the product, not of the error.
func TestClockSampleOnlyOnTheServingNode(t *testing.T) {
	cfg, f, took, n := clockFixture(t)
	cfg.consider(context.Background(), f, n, cfg.Services, false, night, func(string, ...any) {})
	if len(*took) != 0 {
		t.Errorf("a secondary took a member: %v", *took)
	}
}

func metaOf(t *testing.T, raw string) quadlet.SnapshotMeta {
	t.Helper()
	var meta quadlet.SnapshotMeta
	if err := json.Unmarshal([]byte(raw), &meta); err != nil {
		t.Fatalf("sidecar does not parse: %v", err)
	}
	return meta
}

// TestClockSampleIsCrashConsistentWhenTheServiceCannotHoldStill: it is the ONE member taken against a
// running service, and the picker and [B.32] are entitled to know that before trusting it. A Home
// Assistant that is down, too old for the view, or whose lock broke all land here.
func TestClockSampleIsCrashConsistentWhenTheServiceCannotHoldStill(t *testing.T) {
	cfg, f, took, n := clockFixture(t)
	f.held = false
	var logged []string
	cfg.consider(context.Background(), f, n, cfg.Services, true, night, func(s string, a ...any) {
		logged = append(logged, fmt.Sprintf(s, a...))
	})
	if len(*took) != 1 {
		t.Fatalf("took %d members, want one: %v", len(*took), *took)
	}
	meta := metaOf(t, (*took)[0].sidecar)
	if meta.Consistency != quadlet.Crash {
		t.Errorf("consistency = %q, want crash -- the service went on writing", meta.Consistency)
	}
	if meta.Trigger != quadlet.TriggerClock {
		t.Errorf("meta = %+v, want a clock member", meta)
	}
	// AND IT SAYS SO. "The clock sample is crash-consistent again" is how a household would
	// find out that Home Assistant stopped answering, or that an upgrade moved the API this
	// leans on -- silence would make that invisible until somebody read a sidecar.
	if !strings.Contains(strings.Join(logged, "\n"), "without holding the service still") {
		t.Errorf("nothing said the service did not hold still: %v", logged)
	}
}

// TestClockSampleIsQuiescedWhenTheServiceHeldStill ([B.143]): the whole point of asking. Home
// Assistant offers the mechanism its own backups use, and a member taken across it is
// application-consistent rather than something HA has to recover from.
func TestClockSampleIsQuiescedWhenTheServiceHeldStill(t *testing.T) {
	cfg, f, took, n := clockFixture(t)
	f.held = true
	cfg.consider(context.Background(), f, n, cfg.Services, true, night, func(string, ...any) {})
	if len(*took) != 1 {
		t.Fatalf("took %d members, want one: %v", len(*took), *took)
	}
	if got := metaOf(t, (*took)[0].sidecar).Consistency; got != quadlet.Quiesced {
		t.Errorf("consistency = %q, want quiesced", got)
	}
}

// TestTheHostNeverClaimsTheServiceHeldStill is the rule that keeps the class honest across the
// channel ([B.143]): the host renders `crash` and the GUEST upgrades it, because only the guest
// watched the service hold. A host that rendered `quiesced` hopefully would make every failure
// between here and the snapshot into a member that lies.
func TestTheHostNeverClaimsTheServiceHeldStill(t *testing.T) {
	cfg, f, took, n := clockFixture(t)
	f.held = true // the guest upgrades it; what the HOST asked for is what this is about
	cfg.consider(context.Background(), f, n, cfg.Services, true, night, func(string, ...any) {})
	if len(*took) != 1 {
		t.Fatalf("took %d members, want one: %v", len(*took), *took)
	}
	if got := metaOf(t, (*took)[0].asked).Consistency; got != quadlet.Crash {
		t.Errorf("the host asked for %q; it may only ever ask for crash", got)
	}
}

// TestClockSampleFallsBackOnAGuestThatCannotAsk: an older guest has no quiesced take, and the answer
// is the plain one — a member that says crash-consistent, which is what it is. Refusing to take a
// sample at all would leave the ring a hole for the sake of a label.
func TestClockSampleFallsBackOnAGuestThatCannotAsk(t *testing.T) {
	cfg, f, took, n := clockFixture(t)
	f.noQuiesce = true
	cfg.consider(context.Background(), f, n, cfg.Services, true, night, func(string, ...any) {})
	if len(*took) != 1 {
		t.Fatalf("took %d members, want one: %v", len(*took), *took)
	}
	if got := metaOf(t, (*took)[0].sidecar).Consistency; got != quadlet.Crash {
		t.Errorf("consistency = %q, want crash", got)
	}
}

// TestClockSampleRefusesAnOlderGuest: the ring's verbs are capability-gated, and a guest that does not
// advertise data.member would take the old data.snapshot instead -- an unlabelled member, which is
// the one thing the sidecar exists to prevent.
func TestClockSampleRefusesAnOlderGuest(t *testing.T) {
	cfg, f, took, n := clockFixture(t)
	f.noRing = true
	cfg.consider(context.Background(), f, n, cfg.Services, true, night, func(string, ...any) {})
	if len(*took) != 0 {
		t.Errorf("a guest without the ring's verbs was asked for a member: %v", *took)
	}
}
