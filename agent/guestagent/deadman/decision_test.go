package deadman

import (
	"testing"
	"time"
)

// The link is alive whenever contact is more recent than the deadman — regardless of quorum.
func TestDecideServesWhileLinkAlive(t *testing.T) {
	for _, safe := range []bool{true, false} {
		got := Decide(Input{SinceContact: 1 * time.Minute, Deadman: 6 * time.Minute, Allowed: safe})
		if got != Serve {
			t.Errorf("Allowed=%v: link-alive Decide = %v, want Serve", safe, got)
		}
	}
}

// Deadman fired + quorum survives without me + no recent reboot → reboot (the failover trigger).
func TestDecideRebootsWhenQuorumSafe(t *testing.T) {
	got := Decide(Input{
		SinceContact: 7 * time.Minute, Deadman: 6 * time.Minute,
		Allowed: true, SinceReboot: time.Hour, Attempt: 0,
	})
	if got != Reboot {
		t.Errorf("quorum-safe deadman = %v, want Reboot", got)
	}
}

// The load-bearing negative: deadman fired but rebooting would tip the cluster below quorum →
// Hold, never Reboot. A deadman reboot must never be able to cause a serving outage.
func TestDecideHoldsWhenQuorumCritical(t *testing.T) {
	got := Decide(Input{
		SinceContact: 30 * time.Minute, Deadman: 6 * time.Minute,
		Allowed: false, SinceReboot: time.Hour, Attempt: 3,
	})
	if got != Hold {
		t.Errorf("quorum-critical deadman = %v, want Hold (never self-outage)", got)
	}
}

// Even when allowed, a reboot within the cadence since the last one holds (keep serving, wait) —
// so a persistently-absent agent doesn't produce a reboot loop.
func TestDecideHoldsWithinCadence(t *testing.T) {
	got := Decide(Input{
		SinceContact: 7 * time.Minute, Deadman: 6 * time.Minute,
		Allowed: true, SinceReboot: RebootCadence - time.Minute, Attempt: 3,
	})
	if got != Hold {
		t.Errorf("within-cadence deadman = %v, want Hold", got)
	}
	// Past the cadence → reboots again.
	got = Decide(Input{
		SinceContact: 7 * time.Minute, Deadman: 6 * time.Minute,
		Allowed: true, SinceReboot: RebootCadence + time.Minute, Attempt: 3,
	})
	if got != Reboot {
		t.Errorf("past-cadence deadman = %v, want Reboot", got)
	}
}

// A simulated self-update window (the worst-case trial gap) must NOT trip the deadman — this is
// the threshold-suppression proof at the decision layer.
func TestDecideSelfUpdateWindowDoesNotReboot(t *testing.T) {
	got := Decide(Input{
		SinceContact: SelfUpdateWindow, // the whole trial+revert window elapsed with no contact
		Deadman:      DefaultDeadman,   // the production base threshold
		Allowed:      true, SinceReboot: time.Hour,
	})
	if got != Serve {
		t.Errorf("a self-update-length gap tripped the deadman (%v) — the trial must revert first", got)
	}
}

// The cadence: one immediate attempt, then a flat RebootCadence forever. Flat rather than the
// old 30s/1m/2m/4m ramp because a lone node now reboots itself (RebootAllowed), and there every
// attempt is a real service interruption — the ramp would spend three of them in four minutes.
func TestBackoffIsImmediateThenFlat(t *testing.T) {
	if Backoff(0) != 0 {
		t.Errorf("Backoff(0) = %v, want 0 (first reboot is immediate)", Backoff(0))
	}
	for _, a := range []int{1, 2, 3, 17, 1000} {
		if got := Backoff(a); got != RebootCadence {
			t.Errorf("Backoff(%d) = %v, want the flat cadence %v", a, got, RebootCadence)
		}
	}
	// It never becomes "stop" — a persistently degraded node keeps gently retrying, which is both
	// a standing chance that whatever wedged it clears and the visible sign of degradation a
	// remote fix looks for. An attempt count this large returning 0 (retry storm) or something
	// unbounded (silence) would break one or the other.
	if d := Backoff(1 << 30); d != RebootCadence {
		t.Errorf("Backoff at a huge attempt = %v, want %v (never stops, never runs away)", d, RebootCadence)
	}
}

// THE invariant: the base T_deadman — even before jitter, which only adds — must
// exceed the worst-case self-update window, so a trial always converges or reverts first.
func TestInvariantDeadmanExceedsSelfUpdateWindow(t *testing.T) {
	if DefaultDeadman <= SelfUpdateWindow {
		t.Fatalf("INVARIANT VIOLATED: DefaultDeadman (%v) must exceed the self-update window (%v) "+
			"or a self-update would trip the deadman", DefaultDeadman, SelfUpdateWindow)
	}
	// Jitter only ever ADDS, so the minimum effective deadman is the base — still above the window.
	minEffective := DefaultDeadman
	if minEffective <= SelfUpdateWindow {
		t.Errorf("min effective deadman %v not above the self-update window %v", minEffective, SelfUpdateWindow)
	}
}

func TestEffectiveDeadmanStaysInBand(t *testing.T) {
	base, window := DefaultDeadman, DefaultJitter
	for _, seed := range []string{"n1:0", "n2:0", "n3:0", "n1:1", "n1:2", "anchor-a:7"} {
		d := EffectiveDeadman(base, window, seed)
		if d < base || d >= base+window {
			t.Errorf("EffectiveDeadman(%q) = %v, want [%v, %v)", seed, d, base, base+window)
		}
	}
	// Deterministic (survives a reboot, reproducible): same seed twice → same value.
	first := EffectiveDeadman(base, window, "n1:0")
	again := EffectiveDeadman(base, window, "n1:0")
	if first != again {
		t.Errorf("EffectiveDeadman not deterministic: %v vs %v", first, again)
	}
	// Different nodes get spread out (not all identical) — the anti-thundering-herd property.
	if EffectiveDeadman(base, window, "n1:0") == EffectiveDeadman(base, window, "n2:0") &&
		EffectiveDeadman(base, window, "n1:0") == EffectiveDeadman(base, window, "n3:0") {
		t.Error("EffectiveDeadman did not spread distinct nodes")
	}
	// Zero window → exactly the base.
	if EffectiveDeadman(base, 0, "n1:0") != base {
		t.Error("zero jitter window should return the base unchanged")
	}
}

func TestRebootAllowed(t *testing.T) {
	cases := []struct {
		name string
		f    Fabric
		want bool
	}{
		// SINGLE NODE — allowed. This is the case the old gate got backwards, and getting it
		// backwards left the reflex inert on the shape most briard installs actually have: the
		// node held, alerted once, and then sat wedged and silent forever. There is no cluster to
		// disrupt, quorum is guaranteed on the way back, and the data is on its own disk.
		{"single node, quorate", Fabric{Peers: 0, Connected: 0, Quorate: true}, true},
		{"single node, not even quorate", Fabric{Peers: 0, Connected: 0, Quorate: false}, true},

		// ALREADY FAILED — allowed. No quorum means DRBD has stopped this node writing, so it is
		// serving nobody and there is nothing left to protect by staying up. The partitioned
		// anchor: WAN out, peer and cloud witness both unreachable.
		{"3-voter, fully partitioned and out of quorum", Fabric{Peers: 2, Connected: 0, Quorate: false}, true},

		// SURVIVES WITHOUT ME — the original gate, and the case it was built for.
		{"3-voter, all present, I leave → 2 remain (majority)", Fabric{Peers: 2, Connected: 2, Quorate: true}, true},
		{"5-voter, 3 connected, I leave → 3 remain (majority)", Fabric{Peers: 4, Connected: 3, Quorate: true}, true},

		// THE LOAD-BEARING NEGATIVES: quorate, so I am serving, and my departure drops the
		// survivors below majority. 2 anchors + a cloud witness with the witness already gone is
		// exactly this — a second self-removal would outage the house.
		{"3-voter at the edge (2/3), I leave → 1 remains", Fabric{Peers: 2, Connected: 1, Quorate: true}, false},
		{"5-voter, 2 connected, I leave → 2 remain (< majority 3)", Fabric{Peers: 4, Connected: 2, Quorate: true}, false},
	}
	for _, c := range cases {
		if got := RebootAllowed(c.f); got != c.want {
			t.Errorf("%s: RebootAllowed(%+v) = %v, want %v", c.name, c.f, got, c.want)
		}
	}
}
