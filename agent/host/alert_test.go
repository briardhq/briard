package host

import (
	"context"
	"testing"

	"briard.io/shared/api"
	"briard.io/shared/model"
	"briard.io/shared/notify"
)

type fakeNotifier struct{ alerts []notify.Alert }

func (f *fakeNotifier) Notify(_ context.Context, a notify.Alert) error {
	f.alerts = append(f.alerts, a)
	return nil
}

func qstat(quorate bool, connected int) api.NodeStatus {
	return api.NodeStatus{Quorum: model.QuorumState{Quorate: quorate, Connected: connected}}
}

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
