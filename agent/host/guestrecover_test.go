package host

import (
	"testing"
	"time"
)

// The ladder spends its attempts in order and then stops, announcing the give-up exactly once.
// The once matters as much as the stopping: recover() calls next() every time the window
// closes, which on a permanently wedged guest is forever, so a give-up that re-fired would
// alert the owner every few minutes for as long as the node stayed broken.
func TestLadderSpendsAttemptsThenGivesUpOnce(t *testing.T) {
	r := &guestRecovery{limit: 2}

	if got := r.next(); got != stepReboot {
		t.Fatalf("first expiry = %v, want stepReboot", got)
	}
	if got := r.next(); got != stepReboot {
		t.Fatalf("second expiry = %v, want stepReboot", got)
	}
	if r.attempts != 2 {
		t.Fatalf("attempts = %d after two reboots, want 2", r.attempts)
	}
	if got := r.next(); got != stepGiveUp {
		t.Fatalf("third expiry = %v, want stepGiveUp (limit is 2)", got)
	}
	for i := 0; i < 5; i++ {
		if got := r.next(); got != stepWait {
			t.Fatalf("expiry %d after the give-up = %v, want stepWait (announce once, then hold)", i+4, got)
		}
	}
}

// A channel that stayed up long enough to call the node recovered ends the incident, so the
// next failure gets the full ladder again. Without this the counter is a lifetime budget: a
// node that needs a restart once a quarter eventually has none left, and nothing says so.
func TestServedLongEnoughClearsTheLadder(t *testing.T) {
	r := &guestRecovery{limit: 1, reset: time.Hour}
	if got := r.next(); got != stepReboot {
		t.Fatalf("first expiry = %v, want stepReboot", got)
	}
	if got := r.next(); got != stepGiveUp {
		t.Fatalf("second expiry = %v, want stepGiveUp", got)
	}

	r.served(2 * time.Hour)
	if r.attempts != 0 || r.spent {
		t.Fatalf("after a 2h healthy stretch: attempts=%d spent=%v, want 0/false", r.attempts, r.spent)
	}
	if got := r.next(); got != stepReboot {
		t.Fatalf("expiry after a recovered stretch = %v, want stepReboot (a fresh incident)", got)
	}
}

// The case the reset must NOT cover: a guest that restarts, converges, and wedges again
// shortly after is ONE guest failing repeatedly. Refilling the budget on those short stretches
// is what would turn "restart it three times" into an unbounded restart loop, which is the
// failure mode this rung is bounded to avoid.
func TestShortStretchesDoNotRefillTheBudget(t *testing.T) {
	r := &guestRecovery{limit: 2, reset: 30 * time.Minute}
	for i := 0; i < 2; i++ {
		if got := r.next(); got != stepReboot {
			t.Fatalf("expiry %d = %v, want stepReboot", i+1, got)
		}
		r.served(90 * time.Second) // converged, then wedged again -- same incident
	}
	if r.attempts != 2 {
		t.Fatalf("attempts = %d after two short-lived recoveries, want 2 (budget not refilled)", r.attempts)
	}
	if got := r.next(); got != stepGiveUp {
		t.Fatalf("third expiry = %v, want stepGiveUp -- short stretches refilled the budget", got)
	}
}

// The zero value is the shipped configuration. The fields exist so a test can shorten the
// ladder, and a caller that sets none of them must get the product's constants -- if this
// drifts, the unit tests above would be proving a ladder nobody runs.
func TestZeroValueIsTheShippedLadder(t *testing.T) {
	var r guestRecovery
	if got := r.waitFor(); got != guestRecoveryWindow {
		t.Errorf("waitFor() = %v, want %v", got, guestRecoveryWindow)
	}
	if got := r.allowed(); got != guestRebootAttempts {
		t.Errorf("allowed() = %v, want %v", got, guestRebootAttempts)
	}
	if got := r.resetAfter(); got != guestRecoveryReset {
		t.Errorf("resetAfter() = %v, want %v", got, guestRecoveryReset)
	}
}

// The window has to be long enough that no ordinary channel bounce reaches this rung. The
// reconnect that matters lands in about a second (B.23); a guest agent that exits per
// connection is back within its RestartSec. A window of seconds would make the host restart
// VMs for events the guest heals by itself, which is strictly worse than the forever-retry
// this replaced -- so the ordering is asserted rather than left to the constant's comment.
func TestRecoveryWindowIsLongerThanAnyOrdinaryBounce(t *testing.T) {
	if guestRecoveryWindow < time.Minute {
		t.Errorf("guestRecoveryWindow = %v: too short to distinguish a wedged guest from a bounce", guestRecoveryWindow)
	}
	if guestRecoveryReset <= guestRecoveryWindow {
		t.Errorf("guestRecoveryReset (%v) <= guestRecoveryWindow (%v): a node could be declared "+
			"recovered without ever having outlived the wait that judges it",
			guestRecoveryReset, guestRecoveryWindow)
	}
}
