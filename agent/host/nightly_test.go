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

// The nightly's fixtures ([B.143]). `night` is inside the window and `noon` is not, both on the
// same day, so a case can move the clock without moving the calendar.
var (
	night = time.Date(2026, 9, 23, nightlyHour, 12, 0, 0, time.Local)
	noon  = time.Date(2026, 9, 23, 12, 0, 0, 0, time.Local)
)

func nightlyFixture(t *testing.T) (Config, *fakeStatus, *[]takenMember, *nightly) {
	t.Helper()
	took := &[]takenMember{}
	f := &fakeStatus{took: took, volume: map[string]string{"home-assistant": `{"name":"home-assistant"}`}}
	cfg := Config{
		Services: []model.ServiceSpec{{Name: "home-assistant"}},
		clock:    func() time.Time { return night },
	}
	return cfg, f, took, newNightly()
}

// TestNightlyTakesOneMemberANight is the ring's floor ([B.143]): a service nobody restarts still
// gets a member, which is what lets the retention ladder have no floor of its own.
func TestNightlyTakesOneMemberANight(t *testing.T) {
	cfg, f, took, n := nightlyFixture(t)
	cfg.consider(context.Background(), f, n, cfg.Services, true, night, func(string, ...any) {})
	if len(*took) != 1 {
		t.Fatalf("took %d members, want tonight's one: %v", len(*took), *took)
	}
	svc, tr, _, ok := quadlet.ParseSnapshotMember((*took)[0].member)
	if !ok || svc != "home-assistant" || tr != quadlet.TriggerDaily {
		t.Errorf("member = %q, want home-assistant's daily", (*took)[0].member)
	}
	// AND ONLY ONE. The observe loop runs on a cadence, so every later cycle inside the hour asks
	// again -- a nightly that took one each time would fill the ring with an hour of duplicates.
	cfg.consider(context.Background(), f, n, cfg.Services, true, night.Add(30*time.Second), func(string, ...any) {})
	if len(*took) != 1 {
		t.Errorf("a second cycle in the same window took another member: %v", *took)
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

// TestNightlyIsCrashConsistentWhenTheServiceCannotHoldStill: it is the ONE member taken against a
// running service, and the picker and [B.32] are entitled to know that before trusting it. A Home
// Assistant that is down, too old for the view, or whose lock broke all land here.
func TestNightlyIsCrashConsistentWhenTheServiceCannotHoldStill(t *testing.T) {
	cfg, f, took, n := nightlyFixture(t)
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
	if meta.Trigger != quadlet.TriggerDaily || meta.Title == "" {
		t.Errorf("meta = %+v, want a titled daily member", meta)
	}
	// AND IT SAYS SO. "The nightly is crash-consistent again tonight" is how a household would
	// find out that Home Assistant stopped answering, or that an upgrade moved the API this
	// leans on -- silence would make that invisible until somebody read a sidecar.
	if !strings.Contains(strings.Join(logged, "\n"), "without holding the service still") {
		t.Errorf("nothing said the service did not hold still: %v", logged)
	}
}

// TestNightlyIsQuiescedWhenTheServiceHeldStill ([B.143]): the whole point of asking. Home
// Assistant offers the mechanism its own backups use, and a member taken across it is
// application-consistent rather than something HA has to recover from.
func TestNightlyIsQuiescedWhenTheServiceHeldStill(t *testing.T) {
	cfg, f, took, n := nightlyFixture(t)
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
	cfg, f, took, n := nightlyFixture(t)
	f.held = true // the guest upgrades it; what the HOST asked for is what this is about
	cfg.consider(context.Background(), f, n, cfg.Services, true, night, func(string, ...any) {})
	if len(*took) != 1 {
		t.Fatalf("took %d members, want one: %v", len(*took), *took)
	}
	if got := metaOf(t, (*took)[0].asked).Consistency; got != quadlet.Crash {
		t.Errorf("the host asked for %q; it may only ever ask for crash", got)
	}
}

// TestNightlyFallsBackOnAGuestThatCannotAsk: an older guest has no quiesced take, and the answer
// is the plain one — a member that says crash-consistent, which is what it is. Refusing to take a
// nightly at all would leave the ring a hole for the sake of a label.
func TestNightlyFallsBackOnAGuestThatCannotAsk(t *testing.T) {
	cfg, f, took, n := nightlyFixture(t)
	f.noQuiesce = true
	cfg.consider(context.Background(), f, n, cfg.Services, true, night, func(string, ...any) {})
	if len(*took) != 1 {
		t.Fatalf("took %d members, want one: %v", len(*took), *took)
	}
	if got := metaOf(t, (*took)[0].sidecar).Consistency; got != quadlet.Crash {
		t.Errorf("consistency = %q, want crash", got)
	}
}

// TestNightlyWaitsForTheWindow: the whole point of a clock-taken member is that it lands when
// nobody is using the house. A nightly that fired on the first cycle after an agent start would
// be an ordinary-hours snapshot with a misleading name.
func TestNightlyWaitsForTheWindow(t *testing.T) {
	cfg, f, took, n := nightlyFixture(t)
	cfg.consider(context.Background(), f, n, cfg.Services, true, noon, func(string, ...any) {})
	if len(*took) != 0 {
		t.Errorf("a member was taken at midday: %v", *took)
	}
}

// TestNightlyOnlyOnTheServingNode: the volume is mounted on one node. A secondary asking for a
// member is asking about data it does not hold, and the guest would refuse -- but the host must
// not ask, because "the ring is per volume" is a property of the product, not of the error.
func TestNightlyOnlyOnTheServingNode(t *testing.T) {
	cfg, f, took, n := nightlyFixture(t)
	cfg.consider(context.Background(), f, n, cfg.Services, false, night, func(string, ...any) {})
	if len(*took) != 0 {
		t.Errorf("a secondary took a member: %v", *took)
	}
}

// TestNightlyDoesNotRepeatAfterAnAgentRestart: the in-memory record is a fast path, not the
// truth. The agent restarts -- an update, a crash, a re-dial -- and a fresh map inside the window
// would otherwise take a member every cycle for an hour, each of bytes that had not changed.
func TestNightlyDoesNotRepeatAfterAnAgentRestart(t *testing.T) {
	cfg, f, took, _ := nightlyFixture(t)
	f.members = map[string][]quadlet.SnapshotEntry{"home-assistant": {{
		Member: quadlet.SnapshotMember("home-assistant", quadlet.TriggerDaily, night.Add(-10*time.Minute)),
		Meta:   quadlet.SnapshotMeta{Trigger: quadlet.TriggerDaily, TakenAt: night.Add(-10 * time.Minute)},
	}}}
	cfg.consider(context.Background(), f, newNightly(), cfg.Services, true, night, func(string, ...any) {})
	if len(*took) != 0 {
		t.Errorf("a restarted agent took a second member for tonight: %v", *took)
	}
	// LAST NIGHT'S DOES NOT COUNT, which is the other half of the same read: a member from
	// yesterday is exactly what tonight's is replacing.
	f.members["home-assistant"][0].Meta.TakenAt = night.Add(-24 * time.Hour)
	cfg.consider(context.Background(), f, newNightly(), cfg.Services, true, night, func(string, ...any) {})
	if len(*took) != 1 {
		t.Errorf("yesterday's member stopped tonight's being taken: %v", *took)
	}
}

// TestNightlyFailureIsNotRetriedAllNight: a household's night is not the place to hammer a volume
// that is having trouble, and tomorrow's member costs the same as tonight's. The ring is a
// convenience; the service is the product.
func TestNightlyFailureIsNotRetriedAllNight(t *testing.T) {
	cfg, f, took, n := nightlyFixture(t)
	f.snapErr = errors.New("no space left on device")
	var logged int
	logf := func(string, ...any) { logged++ }
	for i := 0; i < 5; i++ {
		cfg.consider(context.Background(), f, n, cfg.Services, true, night.Add(time.Duration(i)*time.Minute), logf)
	}
	if len(*took) != 1 {
		t.Errorf("a failing take was retried: %d attempts", len(*took))
	}
	if logged == 0 {
		t.Error("the failure was silent; a ring that stopped filling must say so")
	}
}

// TestNightlyRefusesAnOlderGuest: the ring's verbs are capability-gated, and a guest that does not
// advertise data.member would take the old data.snapshot instead -- an unlabelled member, which is
// the one thing the sidecar exists to prevent.
func TestNightlyRefusesAnOlderGuest(t *testing.T) {
	cfg, f, took, n := nightlyFixture(t)
	f.noRing = true
	cfg.consider(context.Background(), f, n, cfg.Services, true, night, func(string, ...any) {})
	if len(*took) != 0 {
		t.Errorf("a guest without the ring's verbs was asked for a member: %v", *took)
	}
}
