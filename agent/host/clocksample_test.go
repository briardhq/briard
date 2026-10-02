package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"briard.io/agent/hass"
	"briard.io/agent/quadlet"
	"briard.io/shared/model"
	"briard.io/shared/notify"
)

// The clock sample's fixtures. `night` is any moment; the sampler has no window.
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

// TestClockSampleTakesOneAnInterval is the history's floor: a service nobody restarts is
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
// running service, and the picker and the snapshot-integrity supervisor are entitled to know that before trusting it. A Home
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

// TestClockSampleIsQuiescedWhenTheServiceHeldStill: the whole point of asking. Home
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
// channel: the host renders `crash` and the GUEST upgrades it, because only the guest
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

// The recorder check's fixtures, in a zone of their own so "05:30 local" is the zone's and not
// the test machine's.
var athens, _ = time.LoadLocation("Europe/Athens")

func dbFixture(t *testing.T) (Config, fakeStatus, *fakeDB, *dbChecker, *fakeNotifier) {
	t.Helper()
	db := &fakeDB{}
	cfg := Config{Node: "anchor-1", Services: []model.ServiceSpec{{Name: "home-assistant"}}}
	return cfg, fakeStatus{db: db}, db, &dbChecker{loc: athens}, &fakeNotifier{}
}

func localAt(day, h, m int) time.Time { return time.Date(2026, 10, day, h, m, 0, 0, athens) }

func askRecorder(cfg Config, g fakeStatus, d *dbChecker, n *fakeNotifier, at time.Time) {
	cfg.checkRecorder(context.Background(), g, d, cfg.Services, true, at.UTC(), n, func(string, ...any) {})
}

// TestTheRecorderCheckStartsOnceANightAtHalfPastFive: after the update window and Home
// Assistant's own purge, in the household's zone, once per night however often the loop asks.
func TestTheRecorderCheckStartsOnceANightAtHalfPastFive(t *testing.T) {
	cfg, g, db, d, n := dbFixture(t)
	askRecorder(cfg, g, d, n, localAt(1, 5, 29))
	if db.starts != 0 {
		t.Fatal("started before 05:30")
	}
	askRecorder(cfg, g, d, n, localAt(1, 5, 30))
	db.finish(hass.DBReport{Verdict: hass.VerdictClean})
	askRecorder(cfg, g, d, n, localAt(1, 5, 31)) // collects
	askRecorder(cfg, g, d, n, localAt(1, 5, 45))
	if db.starts != 1 {
		t.Fatalf("started %d times in one night, want once", db.starts)
	}
	askRecorder(cfg, g, d, n, localAt(2, 6, 31))
	if db.starts != 1 {
		t.Fatal("started after the window closed: an agent restart at noon must not check then")
	}
	askRecorder(cfg, g, d, n, localAt(3, 6, 10))
	if db.starts != 2 {
		t.Fatal("a late agent inside the window did not start the check")
	}
	if len(n.alerts) != 0 || len(db.restored) != 0 {
		t.Errorf("a clean night alerted or restored: %v %v", n.alerts, db.restored)
	}
}

// TestTheRecorderCheckNeverWaitsInTheLoop: starting it and asking for its report are each one
// short call; a check still running is asked again next cycle, never waited for.
func TestTheRecorderCheckNeverWaitsInTheLoop(t *testing.T) {
	cfg, g, db, d, n := dbFixture(t)
	askRecorder(cfg, g, d, n, localAt(1, 5, 30))
	for i := 1; i <= 20; i++ {
		askRecorder(cfg, g, d, n, localAt(1, 5, 30+i))
	}
	if !db.running || db.starts != 1 || len(db.restored) != 0 {
		t.Fatalf("running=%t starts=%d restored=%v", db.running, db.starts, db.restored)
	}
	db.finish(hass.DBReport{Verdict: hass.VerdictCorrupt, Candidate: "/m", CandidateAt: localAt(1, 1, 0)})
	askRecorder(cfg, g, d, n, localAt(1, 5, 51))
	if len(db.restored) != 1 {
		t.Fatalf("the report was not acted on once it came: %v", db.restored)
	}
}

// TestALostCheckIsGivenUpOn: a check whose report never comes (the guest agent restarted) stops
// being waited for, and the next night starts afresh.
func TestALostCheckIsGivenUpOn(t *testing.T) {
	cfg, g, db, d, n := dbFixture(t)
	askRecorder(cfg, g, d, n, localAt(1, 5, 30))
	db.running = false // gone without a report
	askRecorder(cfg, g, d, n, localAt(1, 5, 31))
	if !d.waiting.IsZero() {
		t.Fatal("still waiting for a check the guest no longer runs")
	}
	askRecorder(cfg, g, d, n, localAt(2, 5, 30))
	if db.starts != 2 {
		t.Fatalf("the next night did not start a check: starts=%d", db.starts)
	}
}

// TestTheRecorderCheckNeedsTheVolumeAndHomeAssistant: only the serving node holds the ring, and
// a node without Home Assistant has nothing to check.
func TestTheRecorderCheckNeedsTheVolumeAndHomeAssistant(t *testing.T) {
	cfg, g, db, d, n := dbFixture(t)
	cfg.checkRecorder(context.Background(), g, d, cfg.Services, false, localAt(1, 5, 31), n, func(string, ...any) {})
	cfg.checkRecorder(context.Background(), g, d, []model.ServiceSpec{{Name: "mosquitto"}}, true, localAt(1, 5, 31), n, func(string, ...any) {})
	if db.starts != 0 {
		t.Fatalf("started %d times on a node that should not", db.starts)
	}
}

// TestADamagedRecorderReachesTheHousehold: repaired or not, damage is an alert, not only a row,
// and the restore is asked for exactly the member the report named.
func TestADamagedRecorderReachesTheHousehold(t *testing.T) {
	for _, tc := range []struct {
		name       string
		rep        hass.DBReport
		restoreErr error
		want       string // in the alert's body
	}{
		{"repaired", hass.DBReport{Verdict: hass.VerdictCorrupt, Candidate: "/m", CandidateAt: localAt(1, 1, 0)}, nil, "Thu 1 Oct, 01:00"},
		{"no candidate", hass.DBReport{Verdict: hass.VerdictCorrupt, Why: "no held copy checked clean"}, nil, "no held copy checked clean"},
		{"the restore refused", hass.DBReport{Verdict: hass.VerdictCorrupt, Candidate: "/m", CandidateAt: localAt(1, 1, 0)}, errors.New("not restoring: schema 48 newer than 47"), "schema 48 newer than 47"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, g, db, d, n := dbFixture(t)
			db.restoreErr = tc.restoreErr
			askRecorder(cfg, g, d, n, localAt(1, 5, 30))
			db.finish(tc.rep)
			askRecorder(cfg, g, d, n, localAt(1, 5, 31))
			if tc.rep.Candidate != "" && (len(db.restored) != 1 || db.restored[0] != tc.rep.Candidate) {
				t.Fatalf("restored %v, want the report's candidate", db.restored)
			}
			if len(n.alerts) != 1 || n.alerts[0].Level != notify.Warning {
				t.Fatalf("alerts %v, want one warning", n.alerts)
			}
			if !strings.Contains(n.alerts[0].Body, tc.want) {
				t.Errorf("alert body %q does not say %q", n.alerts[0].Body, tc.want)
			}
		})
	}
}
