package deadman

import (
	"testing"
	"time"
)

// The link is alive whenever contact is more recent than the deadman — regardless of quorum.
func TestDecideServesWhileLinkAlive(t *testing.T) {
	for _, safe := range []bool{true, false} {
		got := Decide(Input{SinceContact: 1 * time.Minute, Deadman: 6 * time.Minute, QuorumSafe: safe})
		if got != Serve {
			t.Errorf("QuorumSafe=%v: link-alive Decide = %v, want Serve", safe, got)
		}
	}
}

// Deadman fired + quorum survives without me + no recent reboot → reboot (the failover trigger).
func TestDecideRebootsWhenQuorumSafe(t *testing.T) {
	got := Decide(Input{
		SinceContact: 7 * time.Minute, Deadman: 6 * time.Minute,
		QuorumSafe: true, SinceReboot: time.Hour, Attempt: 0,
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
		QuorumSafe: false, SinceReboot: time.Hour, Attempt: 3,
	})
	if got != Hold {
		t.Errorf("quorum-critical deadman = %v, want Hold (never self-outage)", got)
	}
}

// Even quorum-safe, a reboot within the backoff window since the last one holds (keep serving,
// wait) — so a persistently-absent agent doesn't produce a tight reboot loop.
func TestDecideHoldsWithinBackoff(t *testing.T) {
	// Attempt 3 → Backoff = 2m; only 30s since the last reboot → hold.
	got := Decide(Input{
		SinceContact: 7 * time.Minute, Deadman: 6 * time.Minute,
		QuorumSafe: true, SinceReboot: 30 * time.Second, Attempt: 3,
	})
	if got != Hold {
		t.Errorf("within-backoff deadman = %v, want Hold", got)
	}
	// Past the backoff → reboots again.
	got = Decide(Input{
		SinceContact: 7 * time.Minute, Deadman: 6 * time.Minute,
		QuorumSafe: true, SinceReboot: 5 * time.Minute, Attempt: 3,
	})
	if got != Reboot {
		t.Errorf("past-backoff deadman = %v, want Reboot", got)
	}
}

// A simulated self-update window (the worst-case trial gap) must NOT trip the deadman — this is
// the threshold-suppression proof at the decision layer.
func TestDecideSelfUpdateWindowDoesNotReboot(t *testing.T) {
	got := Decide(Input{
		SinceContact: SelfUpdateWindow, // the whole trial+revert window elapsed with no contact
		Deadman:      DefaultDeadman,   // the production base threshold
		QuorumSafe:   true, SinceReboot: time.Hour,
	})
	if got != Serve {
		t.Errorf("a self-update-length gap tripped the deadman (%v) — the trial must revert first", got)
	}
}

func TestBackoffScheduleGrowsAndCaps(t *testing.T) {
	if Backoff(0) != 0 {
		t.Errorf("Backoff(0) = %v, want 0 (first reboot is immediate)", Backoff(0))
	}
	if Backoff(1) != backoffBase {
		t.Errorf("Backoff(1) = %v, want %v", Backoff(1), backoffBase)
	}
	// Monotonic non-decreasing, and capped — never a hard stop.
	prev := time.Duration(0)
	for a := 0; a < 100; a++ {
		d := Backoff(a)
		if d < prev {
			t.Errorf("Backoff not monotonic at attempt %d: %v < %v", a, d, prev)
		}
		if d > backoffCap {
			t.Errorf("Backoff(%d) = %v exceeds the cap %v", a, d, backoffCap)
		}
		prev = d
	}
	if Backoff(1000) != backoffCap {
		t.Errorf("Backoff at a huge attempt = %v, want the cap %v (never stops)", Backoff(1000), backoffCap)
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

func TestQuorumSafe(t *testing.T) {
	cases := []struct {
		name             string
		total, connected int
		want             bool
	}{
		{"solo (lone node is the quorum)", 1, 0, false},
		{"3-node all present, one leaves → 2 remain", 3, 2, true},
		{"3-node at the edge (2/3), I leave → 1 remains", 3, 1, false},
		{"2-anchor+witness, witness down, I leave → 1 remains", 3, 1, false},
		{"5-node, 3 peers connected, I leave → 3 remain (majority)", 5, 3, true},
		{"5-node, 2 peers connected, I leave → 2 remain (< majority 3)", 5, 2, false},
	}
	for _, c := range cases {
		if got := QuorumSafe(c.total, c.connected); got != c.want {
			t.Errorf("%s: QuorumSafe(total=%d, connected=%d) = %v, want %v", c.name, c.total, c.connected, got, c.want)
		}
	}
}
