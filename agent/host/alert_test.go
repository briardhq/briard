package host

import (
	"context"
	"strings"
	"testing"
	"time"

	"briard.io/shared/api"
	"briard.io/shared/model"
	"briard.io/shared/notify"
)

type fakeNotifier struct{ alerts []notify.Alert }

func (f *fakeNotifier) Notify(_ context.Context, a notify.Alert) error {
	f.alerts = append(f.alerts, a)
	return nil
}

// qstat is a reading with NO peer detail -- a guest too old to report peers. The alerter may
// then say only what the count supports, so these readings can never classify as "alone".
func qstat(quorate bool, connected int) model.Cluster {
	return model.Cluster{QuorumState: model.QuorumState{Quorate: quorate, Connected: connected}}
}

// seen is the same reading plus the peer list the alerter actually classifies on.
func seen(quorate bool, connected int, peers ...model.PeerState) model.Cluster {
	c := qstat(quorate, connected)
	c.Peers = peers
	return c
}

var (
	// An anchor that is up carries storage and is fully replicated -- a copy that could take
	// over. A witness is connected but diskless, so it is a vote and never a copy: the two are
	// what the peer COUNT cannot tell apart.
	anchorUp    = model.PeerState{Name: "n2", Connected: true, Diskful: true, UpToDate: true}
	anchorGone  = model.PeerState{Name: "n2"}
	anchor3Up   = model.PeerState{Name: "n3", Connected: true, Diskful: true, UpToDate: true}
	anchor3Gone = model.PeerState{Name: "n3"}
	witnessUp   = model.PeerState{Name: "w01", Connected: true}
	witnessGone = model.PeerState{Name: "w01"}
)

// The alerter primes silently, warns once on a full->reduced edge, does not re-fire while
// steadily reduced, and fires recovered on the way back -- the no-fatigue contract.
func TestRedundancyAlerter(t *testing.T) {
	fn := &fakeNotifier{}
	a := newRedundancyAlerter(testStore(t, fn), "n1", 2, func(string, ...any) {})
	ctx := context.Background()

	a.observe(ctx, qstat(true, 2)) // prime full -- no alert
	a.observe(ctx, qstat(true, 2)) // steady full -- no alert
	if len(fn.alerts) != 0 {
		t.Fatalf("no alert expected while full, got %+v", fn.alerts)
	}
	a.observe(ctx, qstat(true, 1)) // lost a replica -> warning
	if len(fn.alerts) != 1 || fn.alerts[0].Kind != notify.Open {
		t.Fatalf("expected one warning, got %+v", fn.alerts)
	}
	a.observe(ctx, qstat(true, 1)) // steady reduced -- must NOT re-fire
	if len(fn.alerts) != 1 {
		t.Errorf("re-fired on a steady degrade: %+v", fn.alerts)
	}
	a.observe(ctx, qstat(true, 2)) // reconnected -> recovered
	if len(fn.alerts) != 2 || fn.alerts[1].Kind != notify.Resolved {
		t.Errorf("expected a recovered alert, got %+v", fn.alerts)
	}
}

// Starting already reduced primes silently (no startup false-positive) -- and since nothing was
// announced, nothing is resolved when it clears: an all-clear for a problem nobody heard of is
// noise. The next real loss is announced.
func TestRedundancyAlerterPrimesReduced(t *testing.T) {
	fn := &fakeNotifier{}
	a := newRedundancyAlerter(testStore(t, fn), "n1", 2, func(string, ...any) {})
	a.observe(context.Background(), qstat(true, 1)) // first reading reduced -> prime, no warning
	if len(fn.alerts) != 0 {
		t.Fatalf("must prime silently, not warn on the first reading: %+v", fn.alerts)
	}
	a.observe(context.Background(), qstat(true, 2)) // converged: nothing was open, nothing to resolve
	if len(fn.alerts) != 0 {
		t.Errorf("resolved a condition that was never announced: %+v", fn.alerts)
	}
	a.observe(context.Background(), qstat(true, 1)) // a real loss after convergence
	if len(fn.alerts) != 1 || fn.alerts[0].Kind != notify.Open {
		t.Errorf("expected the loss to be announced, got %+v", fn.alerts)
	}
}

// A non-quorate reading (outage / minority partition) holds state without firing -- the
// agent can't tell minority from true outage; that's the controller's fleet view.
func TestRedundancyAlerterNotQuorateHolds(t *testing.T) {
	fn := &fakeNotifier{}
	a := newRedundancyAlerter(testStore(t, fn), "n1", 2, func(string, ...any) {})
	ctx := context.Background()
	a.observe(ctx, qstat(true, 2))  // prime full
	a.observe(ctx, qstat(false, 0)) // not quorate -- hold, no alert
	if len(fn.alerts) != 0 {
		t.Fatalf("not-quorate must not alert, got %+v", fn.alerts)
	}
	a.observe(ctx, qstat(true, 1)) // quorate again but reduced -> warning
	if len(fn.alerts) != 1 || fn.alerts[0].Kind != notify.Open {
		t.Errorf("expected a warning after recovering to quorate-but-reduced, got %+v", fn.alerts)
	}
}

// A nil alerter (witness) and a single-node cluster (peers==0) have no redundancy signal.
func TestRedundancyAlerterNilAndSingleNode(t *testing.T) {
	var nilA *redundancyAlerter
	nilA.observe(context.Background(), qstat(true, 0)) // must not panic

	fn := &fakeNotifier{}
	single := newRedundancyAlerter(testStore(t, fn), "n1", 0, func(string, ...any) {})
	single.observe(context.Background(), qstat(true, 0))
	if len(fn.alerts) != 0 {
		t.Errorf("single-node has no redundancy to lose, got %+v", fn.alerts)
	}
}

// On the shipped anchor+anchor+witness flock, losing the WITNESS and losing
// the PEER ANCHOR both read as connected 1-of-2 -- and only one of them means the household's
// data is down to a single disk. The count cannot tell them apart; the peer list can, and the
// owner must be told which one happened.
func TestRedundancyAlerterSaysWhichCopyWentAway(t *testing.T) {
	for _, tc := range []struct {
		name      string
		lost      model.Cluster
		wantTitle string
		wantBody  string
	}{
		{
			name:      "the witness went, both copies are intact",
			lost:      seen(true, 1, anchorUp, witnessGone),
			wantTitle: "Briard: reduced redundancy",
			wantBody:  "Another node still holds a full copy",
		},
		{
			name:      "the peer anchor went, the house is on one disk",
			lost:      seen(true, 1, anchorGone, witnessUp),
			wantTitle: "Briard: no second copy",
			wantBody:  "no second copy of your files",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fn := &fakeNotifier{}
			a := newRedundancyAlerter(testStore(t, fn), "n1", 2, func(string, ...any) {})
			ctx := context.Background()
			a.observe(ctx, seen(true, 2, anchorUp, witnessUp)) // prime full
			a.observe(ctx, tc.lost)
			if len(fn.alerts) != 1 {
				t.Fatalf("expected exactly one alert, got %+v", fn.alerts)
			}
			if fn.alerts[0].Title != tc.wantTitle {
				t.Errorf("title = %q, want %q", fn.alerts[0].Title, tc.wantTitle)
			}
			if !strings.Contains(fn.alerts[0].Body, tc.wantBody) {
				t.Errorf("body = %q, want it to contain %q", fn.alerts[0].Body, tc.wantBody)
			}
		})
	}
}

// The edge this item exists for. A node already reported as reduced that then loses its last
// usable copy must say so: under the old two-state machine that transition was "more of what
// we already told you", which is how a pair could stop being a pair in silence.
func TestRedundancyAlerterFiresOnReducedToAlone(t *testing.T) {
	fn := &fakeNotifier{}
	a := newRedundancyAlerter(testStore(t, fn), "n1", 3, func(string, ...any) {})
	ctx := context.Background()

	a.observe(ctx, seen(true, 3, anchorUp, anchor3Up, witnessUp))     // prime full
	a.observe(ctx, seen(true, 2, anchorUp, anchor3Gone, witnessUp))   // one anchor gone -> reduced
	a.observe(ctx, seen(true, 1, anchorGone, anchor3Gone, witnessUp)) // the last copy gone -> alone
	if len(fn.alerts) != 2 {
		t.Fatalf("expected a warning for each state, got %+v", fn.alerts)
	}
	if fn.alerts[0].Title != "Briard: reduced redundancy" {
		t.Errorf("first alert = %q, want the reduced warning", fn.alerts[0].Title)
	}
	if fn.alerts[1].Title != "Briard: no second copy" || fn.alerts[1].Kind != notify.Open {
		t.Errorf("second alert = %+v, want the no-second-copy warning", fn.alerts[1])
	}
	// And back to full is still one recovered, from either degraded state.
	a.observe(ctx, seen(true, 3, anchorUp, anchor3Up, witnessUp))
	if len(fn.alerts) != 3 || fn.alerts[2].Kind != notify.Resolved {
		t.Errorf("expected recovered from alone, got %+v", fn.alerts)
	}
}

// AN UNKNOWN IS NOT AN ALARM -- the same rule the reconnect check follows for boot ids. A guest
// too old to report peers sends none, and the alerter must fall back to the claim the count
// supports rather than announce a lost second copy it cannot see, or claim a surviving copy it
// cannot see either.
func TestRedundancyAlerterWithoutPeerDetailClaimsNothing(t *testing.T) {
	fn := &fakeNotifier{}
	a := newRedundancyAlerter(testStore(t, fn), "n1", 2, func(string, ...any) {})
	ctx := context.Background()
	a.observe(ctx, qstat(true, 2)) // prime full
	a.observe(ctx, qstat(true, 1)) // reduced, and nothing known about who is left
	if len(fn.alerts) != 1 {
		t.Fatalf("expected one alert, got %+v", fn.alerts)
	}
	if fn.alerts[0].Title != "Briard: reduced redundancy" {
		t.Errorf("no peer detail must not read as a lost copy: title = %q", fn.alerts[0].Title)
	}
	if strings.Contains(fn.alerts[0].Body, "still holds a full copy") {
		t.Errorf("must not claim a surviving copy it never saw: %q", fn.alerts[0].Body)
	}
}

// A peer that is connected but still resyncing is NOT a copy that can be failed over to, and
// the owner is better served by the stronger statement: the second copy is not usable yet.
func TestRedundancyAlerterResyncingPeerIsNotACopy(t *testing.T) {
	fn := &fakeNotifier{}
	a := newRedundancyAlerter(testStore(t, fn), "n1", 2, func(string, ...any) {})
	ctx := context.Background()
	resyncing := model.PeerState{Name: "n2", Connected: true, Diskful: true} // UpToDate false
	a.observe(ctx, seen(true, 2, anchorUp, witnessUp))
	a.observe(ctx, seen(true, 1, resyncing, witnessGone))
	if len(fn.alerts) != 1 || fn.alerts[0].Title != "Briard: no second copy" {
		t.Errorf("a resyncing peer is not a usable copy, got %+v", fn.alerts)
	}
}

// The clock alert fires only after an hour of continuous "no", once, and clears on "yes"; an
// unknown neither starts nor clears the hour, and reads are paced.
func TestClockAlerter(t *testing.T) {
	ctx := context.Background()
	t0 := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	answer, reads := "", 0
	fn := &fakeNotifier{}
	st := testStore(t, fn)
	c := &clockAlerter{read: func(context.Context) string { reads++; return answer }}
	at := func(d time.Duration, a string) {
		answer = a
		c.observe(ctx, st, "n1", t0.Add(d), func(string, ...any) {})
	}

	at(0, "no") // just booted: the hour starts
	at(time.Minute, "no")
	if reads != 1 {
		t.Fatalf("read %d times inside one clockReadEvery, want 1", reads)
	}
	at(30*time.Minute, "") // unknown: the hour keeps running, nothing fires
	at(55*time.Minute, "no")
	if len(fn.alerts) != 0 {
		t.Fatalf("fired before an hour of \"no\": %+v", fn.alerts)
	}
	at(time.Hour, "no")
	if len(fn.alerts) != 1 || fn.alerts[0].Kind != notify.Open || !strings.Contains(fn.alerts[0].Body, "n1") {
		t.Fatalf("want one warning naming the node, got %+v", fn.alerts)
	}
	at(2*time.Hour, "no") // steady: no fatigue
	at(3*time.Hour, "")   // unknown does not clear
	if len(fn.alerts) != 1 {
		t.Fatalf("re-fired or cleared on a steady/unknown reading: %+v", fn.alerts)
	}
	at(4*time.Hour, "yes")
	if len(fn.alerts) != 2 || fn.alerts[1].Kind != notify.Resolved {
		t.Fatalf("want a recovered alert, got %+v", fn.alerts)
	}
	// A short unsync after the recovery starts a fresh hour rather than inheriting the old one.
	at(5*time.Hour, "no")
	at(5*time.Hour+30*time.Minute, "yes")
	at(6*time.Hour, "no")
	at(6*time.Hour+50*time.Minute, "no")
	if len(fn.alerts) != 2 {
		t.Fatalf("a run of \"no\" under an hour fired: %+v", fn.alerts)
	}
}

// A host that never answers (Windows, no timedatectl) never alerts.
func TestClockAlerterUnknownNeverFires(t *testing.T) {
	fn := &fakeNotifier{}
	c := &clockAlerter{read: func(context.Context) string { return "" }}
	t0 := time.Now()
	for d := time.Duration(0); d < 5*time.Hour; d += clockReadEvery {
		c.observe(context.Background(), fn, "n1", t0.Add(d), func(string, ...any) {})
	}
	if len(fn.alerts) != 0 {
		t.Fatalf("an unknown clock alerted: %+v", fn.alerts)
	}
}

// TestServiceAlerter walks the service condition through the readings a node actually takes, via
// the real store so its dedup is the one under test: down only past the grace, a crash-loop's
// starting readings do not break the run, a blip followed by a long start never opens, an unknown
// says nothing, only running AND healthy resolves, and a node that hands the volume over resolves
// what it opened -- each service on its own key.
func TestServiceAlerter(t *testing.T) {
	ctx := context.Background()
	t0 := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	at := func(m int) time.Time { return t0.Add(time.Duration(m) * time.Minute) }
	ha := func(state, health string) []api.ServiceStatus {
		return []api.ServiceStatus{{Name: "home-assistant", State: state, Health: health}}
	}
	var (
		stopped   = ha(api.StateStopped, "")
		starting  = ha(api.StateRunning, api.StateStarting)
		unhealthy = ha(api.StateRunning, api.StateUnhealthy)
		healthy   = ha(api.StateRunning, api.StateHealthy)
		unknown   = ha("", "")
	)
	type reading struct {
		min            int
		svcs           []api.ServiceStatus
		known, serving bool
	}
	on := func(min int, svcs []api.ServiceStatus) reading { return reading{min, svcs, true, true} }
	run := func(t *testing.T, rs ...reading) []notify.Alert {
		fn := &fakeNotifier{}
		n, a := testStore(t, fn), &serviceAlerter{}
		for _, r := range rs {
			a.observe(ctx, n, "n1", r.svcs, r.known, r.serving, at(r.min), t.Logf)
		}
		return fn.alerts
	}
	kinds := func(as []notify.Alert) string {
		var out []string
		for _, a := range as {
			out = append(out, string(a.Kind))
		}
		return strings.Join(out, ",")
	}

	for _, c := range []struct {
		name string
		rs   []reading
		want string
	}{
		{"down inside the grace", []reading{on(0, stopped), on(9, stopped)}, ""},
		{"down past the grace", []reading{on(0, stopped), on(10, stopped), on(20, stopped)}, "open"},
		{"crash-loop", []reading{on(0, stopped), on(4, starting), on(8, stopped), on(9, starting), on(11, stopped)}, "open"},
		{"blip then a long start", []reading{on(0, stopped), on(1, starting), on(30, starting)}, ""},
		{"running and unhealthy", []reading{on(0, unhealthy), on(10, unhealthy)}, "open"},
		{"healthy resets the run", []reading{on(0, stopped), on(5, healthy), on(12, stopped)}, ""},
		{"unknown says nothing", []reading{on(0, unknown), on(30, unknown)}, ""},
		{"opens then resolves", []reading{on(0, stopped), on(10, stopped), on(11, healthy), on(12, healthy)}, "open,resolved"},
		{"unknown neither breaks nor resolves", []reading{on(0, stopped), on(10, stopped), on(11, unknown), on(12, stopped)}, "open"},
		{"handed over resolves", []reading{on(0, stopped), on(10, stopped), {11, unknown, true, false}}, "open,resolved"},
		{"a failed read is not a handover", []reading{on(0, stopped), on(10, stopped), {11, unknown, false, false}}, "open"},
		{"a standby never opens", []reading{{0, unknown, true, false}, {30, unknown, true, false}}, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := kinds(run(t, c.rs...)); got != c.want {
				t.Fatalf("alerts = %q, want %q", got, c.want)
			}
		})
	}

	t.Run("each service on its own key", func(t *testing.T) {
		two := []api.ServiceStatus{
			{Name: "home-assistant", State: api.StateStopped},
			{Name: "mosquitto", State: api.StateRunning, Health: api.StateHealthy},
		}
		as := run(t, on(0, two), on(10, two))
		if len(as) != 1 || as[0].Key != "service:home-assistant" || as[0].Severity != notify.Critical {
			t.Fatalf("alerts = %+v, want one critical open for home-assistant only", as)
		}
		if !strings.Contains(as[0].Body, "has not been running") {
			t.Errorf("body %q does not say the service is not running", as[0].Body)
		}
	})
	t.Run("a running service says it is not answering", func(t *testing.T) {
		as := run(t, on(0, unhealthy), on(10, unhealthy))
		if len(as) != 1 || !strings.Contains(as[0].Body, "without answering") {
			t.Fatalf("alerts = %+v, want the running-but-not-answering body", as)
		}
	})
}
