package host

import (
	"context"
	"fmt"
	"time"

	"briard.io/agent/guestagent"
	"briard.io/agent/platform"
	"briard.io/shared/notify"
)

// THE HOST-SIDE RUNG OF THE UNRESPONSIVE-GUEST LADDER (B.22b).
//
// B.22a made the control channel survive a drop: observe returns ErrChannelDown and Run
// re-dials rather than going blind for the rest of the process's life. Re-dialling FOREVER is
// the right answer to every gap the guest closes by itself -- the in-guest agent serves one
// connection then exits, an OS upgrade bounces the channel on its way through, a per-call
// deadline closes it -- and those are almost all of them, which is why forever was a fine
// first answer. It is the wrong answer to the one case none of them cover: a guest that is
// not coming back. Against that, patience alone is a node that stays down while the host
// writes a reconnect line every fifteen seconds and nobody is told anything.
//
// So the wait is bounded, and what follows it is the only lever the host holds over a guest
// that will not talk: take the VM down and bring it back up. That is bounded in turn -- K
// attempts, then alert and stop -- because a guest three reboots did not fix is not one a
// fourth will fix, and a host that keeps trying has turned a broken node into a broken node
// that also never stays up long enough for anyone to look at it.
//
// WHAT THE HOST CANNOT KNOW, and why there is no serving gate here.
//
// The guard anyone would ask for first is "don't reboot a guest that is still serving". It
// cannot be built on this side. Answering it means asking whether the VIP answers, and under
// the default macvtap substrate the host structurally cannot ask: macvtap isolates guest from
// host, which is exactly why the readiness probe prefers the in-guest payload.health verb and
// keeps a host-side GET only as the tap-era fallback (guest.probeReady). Quorum is the same
// story one layer down -- DRBD's view of it lives in the guest. Every question that would gate
// this decision is asked over the channel whose death is the trigger.
//
// That is the asymmetry with the guest-side half of this ladder, which shipped first (V3.4d).
// The deadman reboots from INSIDE, where quorum is readable, so it can hold the property that
// a deadman reboot never causes a serving outage. The host reboots from OUTSIDE and blind, so
// it cannot hold that property and does not claim it. What it holds instead: it acts only on a
// guest that has been silent for the whole recovery window -- long past every gap the guest
// heals on its own -- it stops after K, and it says so before and after, on the local trail
// `briard alerts` reads as well as out of the house.
//
// The residual risk is therefore real and named: a guest whose agent is wedged while its
// payload still serves gets rebooted, and on a node with no peer that is an outage the host
// chose. The alternative is a node that is wedged forever with no reflex able to tell the
// difference, which is worse, and it is the trade B.22b locked in 2026-07-14 -- serving is not
// healthy; reboot serving nodes, patiently.

const (
	// How long a dead control channel gets to heal itself before the host calls the guest
	// wedged. Generous on purpose: the reconnect that matters lands in about a second (B.23),
	// so silence at this scale is not a slow guest, it is a stopped one. Long enough that no
	// ordinary bounce ever reaches this rung; short enough that a wedged node is not down for
	// an evening.
	//
	// It is a constant rather than a knob deliberately (AGENTS §3). The one caller that needs
	// a different number is a test, and the ladder's decisions are driven through recover()'s
	// arguments so a test can set them without the product growing an env var.
	guestRecoveryWindow = 3 * time.Minute

	// How many times the host will reboot a wedged guest before it stops and escalates.
	guestRebootAttempts = 3

	// A channel that stayed up at least this long ends the incident and clears the ladder.
	// Without it the attempt counter is a LIFETIME budget rather than an incident one: a node
	// needing a reboot once a quarter would spend its third some years in, and from then on be
	// the one node in the house with no recovery reflex left -- an outcome nobody chose and
	// nothing would report.
	guestRecoveryReset = 30 * time.Minute
)

// guestRecovery is the ladder's state across one agent lifetime: how many reboots the current
// incident has spent, and whether the give-up has already been announced (so it is announced
// once, not on every subsequent expiry of the window).
type guestRecovery struct {
	window   time.Duration // wait-for-self-heal before rebooting; 0 -> guestRecoveryWindow
	attempts int           // reboots spent on the current incident
	limit    int           // reboots allowed per incident; 0 -> guestRebootAttempts
	reset    time.Duration // uptime that ends an incident; 0 -> guestRecoveryReset
	spent    bool          // the give-up has been announced
}

func (r *guestRecovery) waitFor() time.Duration {
	if r.window <= 0 {
		return guestRecoveryWindow
	}
	return r.window
}

func (r *guestRecovery) allowed() int {
	if r.limit <= 0 {
		return guestRebootAttempts
	}
	return r.limit
}

func (r *guestRecovery) resetAfter() time.Duration {
	if r.reset <= 0 {
		return guestRecoveryReset
	}
	return r.reset
}

// served tells the ladder how long the channel that just died had been up. A stretch long
// enough to call the node recovered ends the incident; anything shorter is the same incident
// still running, which is the case that must not silently refill the budget -- a guest that
// reboots, converges, and wedges again ninety seconds later is one guest failing repeatedly,
// not three unrelated events.
func (r *guestRecovery) served(up time.Duration) {
	if up >= r.resetAfter() {
		r.attempts, r.spent = 0, false
	}
}

// step is what the ladder does next, once the recovery window has closed on a silent guest.
type step int

const (
	stepReboot step = iota // restart the VM, and say so
	stepGiveUp             // out of attempts: announce it, once
	stepWait               // out of attempts and already announced: keep waiting, say nothing
)

// next is the whole decision, taken apart from anything that can restart a VM so it can be
// proven without one -- the same split the guest-side deadman uses (its pure Decide under a
// driver, V3.4d). Calling it CONSUMES an attempt when it returns stepReboot, and marks the
// give-up spent when it returns stepGiveUp, because both are one-shot facts about the incident
// and a caller that had to remember to record them separately would eventually forget on one
// path -- which on this ladder means either a reboot that was never counted or an alert that
// fires on every expiry of the window forever.
func (r *guestRecovery) next() step {
	if r.attempts < r.allowed() {
		r.attempts++
		return stepReboot
	}
	if !r.spent {
		r.spent = true
		return stepGiveUp
	}
	return stepWait
}

// recover runs the ladder for one channel-down event and returns the live channel it ends on.
// It returns an error only when ctx ends -- a clean agent shutdown -- because there is no other
// terminal state: past the give-up point it keeps waiting for a guest that may still come back
// on its own, which is the one thing left that costs nothing and occasionally works.
//
// It is a method on osUpgrade because osUpgrade already owns this swap. A reboot replaces the
// VM, the channel and the Manager together, and there is exactly one place that knows how to
// put all three back -- the same place the rollback leg uses. A second one would be a second
// way to do it (AGENTS §5).
func (u *osUpgrade) recover(ctx context.Context, r *guestRecovery, n notify.Notifier) (*guestagent.Client, error) {
	for {
		// The wait for self-heal. Every ordinary bounce returns here on the first attempt and
		// the rest of this function never runs.
		client, err := u.awaitChannel(ctx, r.waitFor())
		if err == nil {
			u.rebind(client)
			return client, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err() // agent shutting down, not a wedged guest
		}

		switch r.next() {
		case stepWait:
			// Out of attempts and already announced. Keep waiting, keep the reconnect trail
			// running, never reboot again and never re-announce -- repeating the alert would
			// be the flap this rung exists to avoid, moved from the VM to the owner's phone.
			continue

		case stepGiveUp:
			u.fire(ctx, n, notify.Alert{
				Level: notify.Warning,
				Title: "Briard: node needs attention",
				Body: fmt.Sprintf("node %s: the guest has not answered for %s and %d restarts did not "+
					"recover it. The host has stopped restarting it and is still watching, so a guest "+
					"that comes back on its own will be picked up. Until then this node is not serving "+
					"and recovering it needs a human.", u.cfg.Node, r.waitFor(), r.allowed()),
			})
			continue

		default: // stepReboot
			u.fire(ctx, n, notify.Alert{
				Level: notify.Warning,
				Title: "Briard: restarting an unresponsive guest",
				Body: fmt.Sprintf("node %s: no answer from the guest for %s. Restarting it "+
					"(attempt %d of %d).", u.cfg.Node, r.waitFor(), r.attempts, r.allowed()),
			})
			client, err = u.rebootGuest(ctx)
			if err != nil {
				u.logf("guest-recovery: restart attempt %d failed: %v", r.attempts, err)
				continue
			}
			u.logf("guest-recovery: guest restarted and converged (attempt %d)", r.attempts)
			return client, nil
		}
	}
}

// awaitChannel re-dials until the guest answers or the window closes. It is reconnect() under a
// deadline rather than a second retry loop -- the bring-up path already drives it that way, so
// the backoff, the logging and the handshake stay one implementation.
func (u *osUpgrade) awaitChannel(ctx context.Context, window time.Duration) (*guestagent.Client, error) {
	wctx, cancel := context.WithTimeout(ctx, window)
	defer cancel()
	return reconnect(wctx, u.cfg.ControlSock, u.logf)
}

// rebootGuest takes the wedged guest down and brings it back, replacing the whole binding.
//
// It runs on a context detached from the caller's, for the reason the rollback leg is detached
// from the upgrade's: this IS the recovery, and it must not inherit a deadline in order to
// discover there is no time left to recover. The window that just expired is the caller's
// evidence, not this call's budget.
func (u *osUpgrade) rebootGuest(ctx context.Context) (*guestagent.Client, error) {
	rb, cancel := context.WithTimeout(context.WithoutCancel(ctx), u.cfg.BringUpBudget+3*shutdownGrace)
	defer cancel()

	qspec := u.cfg.guestSpec()
	if platform.Running(rb, qspec) {
		// Clean first, forced as the fallback: the same order and the same reason as the
		// rollback leg. The data disk is where DRBD keeps its metadata, and a clean stop is
		// what lets it record MDF_HAVE_QUORUM + prev_members on the way down -- the difference
		// between a node that comes back quorate and one that is stranded.
		//
		// What differs here is which route is expected to work. The agent route is asking a
		// channel already known to be dead, so it will not be the one that lands. The ACPI
		// button will, more often than the situation suggests: a wedged AGENT is not a wedged
		// KERNEL, and the kernel is what answers the power button. Trying it is most of the
		// value of stopping cleanly at all on this path.
		if e := stopCleanly(rb, u.vm, u.client, u.logf); e != nil {
			u.logf("guest-recovery: could not stop the guest cleanly, forcing (%v)", e)
			if e := u.vm.Stop(); e != nil {
				return nil, fmt.Errorf("guest-recovery: stop the guest: %w", e)
			}
		} else {
			u.logf("guest-recovery: stopped the guest cleanly")
		}
	}

	// Nothing is armed and nothing is restored: the disk is the disk. This is a power cycle of
	// a wedged machine, not a rollback -- there is no upgrade in flight to undo, and bringUp is
	// idempotent (B.22b's other half) precisely so it can be re-driven like this.
	g, client, e := u.cfg.bringUp(rb, qspec, u.logf)
	if e != nil {
		return nil, fmt.Errorf("guest-recovery: bring the guest back up: %w", e)
	}
	u.vm = g
	u.rebind(client)
	return client, nil
}

// Fire writes the local trail and then attempts delivery, in that order and never the other
// way round -- see fireAlert, which both alert emitters on this side share.
func (u *osUpgrade) fire(ctx context.Context, n notify.Notifier, al notify.Alert) {
	fireAlert(ctx, n, u.logf, al)
}
