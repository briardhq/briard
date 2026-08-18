package host

import (
	"context"
	"strings"
	"testing"

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
	a := newRedundancyAlerter(fn, "n1", 2, func(string, ...any) {})
	ctx := context.Background()

	a.observe(ctx, qstat(true, 2)) // prime full -- no alert
	a.observe(ctx, qstat(true, 2)) // steady full -- no alert
	if len(fn.alerts) != 0 {
		t.Fatalf("no alert expected while full, got %+v", fn.alerts)
	}
	a.observe(ctx, qstat(true, 1)) // lost a replica -> warning
	if len(fn.alerts) != 1 || fn.alerts[0].Level != notify.Warning {
		t.Fatalf("expected one warning, got %+v", fn.alerts)
	}
	a.observe(ctx, qstat(true, 1)) // steady reduced -- must NOT re-fire
	if len(fn.alerts) != 1 {
		t.Errorf("re-fired on a steady degrade: %+v", fn.alerts)
	}
	a.observe(ctx, qstat(true, 2)) // reconnected -> recovered
	if len(fn.alerts) != 2 || fn.alerts[1].Level != notify.Recovered {
		t.Errorf("expected a recovered alert, got %+v", fn.alerts)
	}
}

// Starting already reduced primes silently (no startup false-positive); recovery still fires.
func TestRedundancyAlerterPrimesReduced(t *testing.T) {
	fn := &fakeNotifier{}
	a := newRedundancyAlerter(fn, "n1", 2, func(string, ...any) {})
	a.observe(context.Background(), qstat(true, 1)) // first reading reduced -> prime, no warning
	if len(fn.alerts) != 0 {
		t.Fatalf("must prime silently, not warn on the first reading: %+v", fn.alerts)
	}
	a.observe(context.Background(), qstat(true, 2)) // -> recovered
	if len(fn.alerts) != 1 || fn.alerts[0].Level != notify.Recovered {
		t.Errorf("expected recovered from primed-reduced, got %+v", fn.alerts)
	}
}

// A non-quorate reading (outage / minority partition) holds state without firing -- the
// agent can't tell minority from true outage; that's the controller's fleet view.
func TestRedundancyAlerterNotQuorateHolds(t *testing.T) {
	fn := &fakeNotifier{}
	a := newRedundancyAlerter(fn, "n1", 2, func(string, ...any) {})
	ctx := context.Background()
	a.observe(ctx, qstat(true, 2))  // prime full
	a.observe(ctx, qstat(false, 0)) // not quorate -- hold, no alert
	if len(fn.alerts) != 0 {
		t.Fatalf("not-quorate must not alert, got %+v", fn.alerts)
	}
	a.observe(ctx, qstat(true, 1)) // quorate again but reduced -> warning
	if len(fn.alerts) != 1 || fn.alerts[0].Level != notify.Warning {
		t.Errorf("expected a warning after recovering to quorate-but-reduced, got %+v", fn.alerts)
	}
}

// A nil alerter (witness) and a single-node cluster (peers==0) have no redundancy signal.
func TestRedundancyAlerterNilAndSingleNode(t *testing.T) {
	var nilA *redundancyAlerter
	nilA.observe(context.Background(), qstat(true, 0)) // must not panic

	fn := &fakeNotifier{}
	single := newRedundancyAlerter(fn, "n1", 0, func(string, ...any) {})
	single.observe(context.Background(), qstat(true, 0))
	if len(fn.alerts) != 0 {
		t.Errorf("single-node has no redundancy to lose, got %+v", fn.alerts)
	}
}

// [B.102]/[B.100]: on the shipped anchor+anchor+witness flock, losing the WITNESS and losing
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
			a := newRedundancyAlerter(fn, "n1", 2, func(string, ...any) {})
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
	a := newRedundancyAlerter(fn, "n1", 3, func(string, ...any) {})
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
	if fn.alerts[1].Title != "Briard: no second copy" || fn.alerts[1].Level != notify.Warning {
		t.Errorf("second alert = %+v, want the no-second-copy warning", fn.alerts[1])
	}
	// And back to full is still one recovered, from either degraded state.
	a.observe(ctx, seen(true, 3, anchorUp, anchor3Up, witnessUp))
	if len(fn.alerts) != 3 || fn.alerts[2].Level != notify.Recovered {
		t.Errorf("expected recovered from alone, got %+v", fn.alerts)
	}
}

// AN UNKNOWN IS NOT AN ALARM -- the same rule the reconnect check follows for boot ids. A guest
// too old to report peers sends none, and the alerter must fall back to the claim the count
// supports rather than announce a lost second copy it cannot see, or claim a surviving copy it
// cannot see either.
func TestRedundancyAlerterWithoutPeerDetailClaimsNothing(t *testing.T) {
	fn := &fakeNotifier{}
	a := newRedundancyAlerter(fn, "n1", 2, func(string, ...any) {})
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
	a := newRedundancyAlerter(fn, "n1", 2, func(string, ...any) {})
	ctx := context.Background()
	resyncing := model.PeerState{Name: "n2", Connected: true, Diskful: true} // UpToDate false
	a.observe(ctx, seen(true, 2, anchorUp, witnessUp))
	a.observe(ctx, seen(true, 1, resyncing, witnessGone))
	if len(fn.alerts) != 1 || fn.alerts[0].Title != "Briard: no second copy" {
		t.Errorf("a resyncing peer is not a usable copy, got %+v", fn.alerts)
	}
}
