package host

import (
	"context"
	"fmt"
	"time"

	"briard.io/agent/guestagent"
	"briard.io/agent/guestagent/deadman"
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
// that will not talk: take the VM down and bring it back up. Then it waits the same steady
// cadence and does it again, indefinitely -- it never gives up. An earlier version stopped after
// K attempts, on the reasoning that a guest three reboots did not fix is not one a fourth will
// fix. That reasoning is sound about the FOURTH attempt and wrong about the hundredth: at a
// two-hour cadence a retry costs almost nothing, occasionally catches a condition that cleared
// on its own, and is the visible sign of degradation a remote fix looks for. What the K-bound
// actually bought was a node with no reflex left, which nobody chose (see rebootCadence).
//
// THE GATE, AND WHY IT IS THE GUEST'S ANSWER RATHER THAN THE HOST'S QUESTION.
//
// The guard anyone asks for first is "don't reboot a guest that is still serving". The host
// cannot evaluate that itself: DRBD's quorum view lives in the guest, the payload's health verb
// rides the channel that just died, and under the default macvtap substrate the host cannot
// reach the VIP either. Every question that would gate the decision is asked over the channel
// whose death is the trigger.
//
// What was wrong with the previous version of this comment is that it concluded from this that
// the host STRUCTURALLY cannot know -- which is true of the VIP and false of the guest. There is
// a private point-to-point link (the guest's eth3), and since it is now created on every install
// rather than only on a managed pairing, the deadman answers the gate on it: the same
// RebootAllowed the guest applies to itself, published every tick and served over raw TCP
// (deadman/gate.go). So this rung asks the one process that is still running, still reads DRBD,
// and is not the thing that wedged.
//
// UNREACHABLE MEANS ALLOWED, deliberately, and it is the opposite default from the guest side
// (which treats an unreadable cluster as NOT allowed). Both fail toward the safer answer from
// where they stand. In the guest, not knowing the cluster means not knowing whether a peer
// depends on you, so you hold. Out here, failing to reach a gate that answers from a healthy
// guest in milliseconds means the guest is far past wedged -- which is the case this rung exists
// for, and refusing to act on it would restore the wedged-forever outcome the whole ladder is
// against. So the gate can only ever WITHHOLD a reboot, never authorise one that would not
// otherwise have happened.
//
// THE GUEST GOES FIRST. recoveryWindow sits above the deadman's T_deadman on purpose, so the
// in-guest graceful reboot -- whose promoter teardown demotes cleanly and lets DRBD record its
// quorum metadata on the way down -- gets its chance before the host resorts to a power cycle.
// The host is the backstop for the case the guest side cannot cover: one where `systemctl
// reboot` itself is what failed. Ordering it the other way would spend the clean demote on every
// incident where the kernel was fine and only the agent had wedged.
//
// The residual risk is smaller than it was but not zero, and it is named rather than argued
// away: a guest whose agent is wedged AND whose deadman is dead (so the gate is unreachable)
// while its payload still serves gets rebooted, and on a node with no peer that is an outage the
// host chose. That is the trade B.22b locked in 2026-07-14 -- serving is not healthy; reboot
// serving nodes, patiently -- and it is now confined to the case where two independent in-guest
// processes are both gone.

const (
	// How long a dead control channel gets to heal itself before the host calls the guest wedged.
	//
	// It MUST exceed the guest deadman's own threshold (deadman.DefaultDeadman + its jitter
	// window), and that ordering is the design rather than a safety margin: the guest's graceful
	// `systemctl reboot` demotes cleanly and lets DRBD write its quorum metadata on the way down,
	// where the host's only lever is a power cycle. Whoever fires first decides which of those
	// the house gets, so the guest fires first and the host backstops the case where the guest's
	// own reboot is what failed. guestrecover_test.go asserts the inequality, because a later
	// tuning of either number could silently invert it and every test would still pass.
	//
	// Generous in absolute terms too: the reconnect that matters lands in about a second (B.23),
	// so silence at this scale is not a slow guest, it is a stopped one.
	//
	// A constant rather than a knob (AGENTS §3). The one caller that needs a different number is
	// a test, and the ladder's decisions are driven through recover()'s arguments so a test can
	// set them without the product growing an env var.
	guestRecoveryWindow = 10 * time.Minute

	// The interval between host-driven reboots of a guest that stays wedged. The SAME constant
	// the guest deadman paces itself by -- one cadence for one ladder, so the two rungs cannot be
	// tuned apart. It never runs out: see the header on why the old K-attempt bound was wrong.
	guestRebootCadence = deadman.RebootCadence

	// A channel that stayed up at least this long ends the incident: the next failure is a new
	// one, announced afresh rather than folded into an alert the owner already read and acted on.
	guestRecoveryReset = 30 * time.Minute

	// How many times a STOPPED guest is relaunched immediately before relaunches fall back to the
	// cadence. Not a give-up -- it never stops, it only stops being instant. A guest that panics
	// its way through boot exits (`-no-reboot`), and without this damping the agent would relaunch
	// it as fast as QEMU could die. Three tries covers the transient crash; a fourth in quick
	// succession is a broken node, and hammering it helps nobody.
	guestRelaunchBurst = 3

	// A gate verdict older than this is treated as no verdict at all. It catches the deadman
	// whose evaluation loop has wedged while its accept loop still answers -- the one failure
	// where the gate keeps replying and the reply means nothing. The deadman re-evaluates every
	// 15s, so anything at this scale is not a slow tick.
	guestGateStale = 5 * time.Minute
)

// guestRecovery is the ladder's state across one agent lifetime.
//
// The two alert latches are BOTH incident-scoped, and only served() clears them. An earlier
// version cleared `announced` when the all-clear went out, which quietly re-armed the trouble
// alert: a guest that recovered and failed again inside the same incident produced a fresh
// trouble/all-clear pair each time round, at whatever period it was flapping on. Bounded by the
// window rather than by the incident -- so not the alert storm a first reading suggests, but
// still a pair every ten minutes indefinitely for a node flapping on that period, which is the
// phone-flap this rung exists to avoid arriving by a different door.
type guestRecovery struct {
	window    time.Duration // wait-for-self-heal before judging the guest wedged; 0 -> guestRecoveryWindow
	cadence   time.Duration // interval between reboots of a still-wedged guest; 0 -> guestRebootCadence
	reset     time.Duration // uptime that ends an incident; 0 -> guestRecoveryReset
	attempts  int           // reboots spent on the current incident
	announced bool          // the degraded alert has been sent for this incident
	cleared   bool          // the all-clear has been sent for this incident
}

func (r *guestRecovery) waitFor() time.Duration {
	if r.window <= 0 {
		return guestRecoveryWindow
	}
	return r.window
}

func (r *guestRecovery) cadenceFor() time.Duration {
	if r.cadence <= 0 {
		return guestRebootCadence
	}
	return r.cadence
}

func (r *guestRecovery) resetAfter() time.Duration {
	if r.reset <= 0 {
		return guestRecoveryReset
	}
	return r.reset
}

// served tells the ladder how long the channel that just died had been up. A stretch long enough
// to call the node recovered ends the incident; anything shorter is the same incident still
// running -- a guest that reboots, converges, and wedges again ninety seconds later is one guest
// failing repeatedly, not three unrelated events, and must not re-announce itself as each.
func (r *guestRecovery) served(up time.Duration) {
	if up >= r.resetAfter() {
		r.attempts, r.announced, r.cleared = 0, false, false
	}
}

// The two alert latches, as decisions rather than as `if` statements inside the emitters -- the
// same pure-decision-under-a-driver split the rest of this file uses, and here it is what makes
// them testable at all. Left inline in announce()/resolved(), the only way to test them is a local
// re-implementation of the same condition, which passes whatever the product does.
//
// Each CONSUMES its latch when it returns true, so a caller cannot forget to record that it fired.

// takeAnnounce reports whether the degraded alert should go out now, once per incident.
func (r *guestRecovery) takeAnnounce() bool {
	if r.announced {
		return false
	}
	r.announced = true
	return true
}

// takeClear reports whether the all-clear should go out now: once per incident, and only for an
// incident that actually announced something. It deliberately does NOT re-arm takeAnnounce -- only
// served() does, after a stretch long enough to call the incident over.
func (r *guestRecovery) takeClear() bool {
	if !r.announced || r.cleared {
		return false
	}
	r.cleared = true
	return true
}

// step is what the ladder does next, once the recovery window has closed on a silent guest.
type step int

const (
	stepRelaunch step = iota // the VM is not running: start it, now
	stepReboot               // it is running but wedged: power-cycle it
	stepHold                 // do not: the gate says no, or it is too soon
)

// next is the whole decision, taken apart from anything that can restart a VM so it can be proven
// without one -- the same split the guest-side deadman uses (its pure Decide under a driver,
// V3.4d). Calling it CONSUMES an attempt when it acts, because that is a one-shot fact about the
// incident and a caller that had to remember to record it separately would eventually forget on
// one path.
//
// STOPPED IS A DIFFERENT CASE FROM SILENT, and separating them is what makes the guest's own
// reboot work. Because of `-no-reboot` every guest restart -- its deadman's graceful reboot, a
// kernel panic, an OOM kill -- ends the VM's unit, so the host's ONLY signal (a dead control
// channel) is shared by two very different situations. When the unit is stopped there is no
// socket that could ever appear, so waiting the window is pure outage; there is nothing running to
// consult a gate about; and a VM that is not running cannot outage a peer, so there is nothing the
// gate could protect. Relaunch at once. This is also what closes the circularity the guest deadman
// would otherwise have: its reboot needs the agent to finish, and the agent now finishes it in
// seconds rather than after a ten-minute wait for a socket belonging to a dead process.
//
// The relaunch burst damps a crash loop without ever giving up: instant while attempts remain,
// then the same slow cadence as everything else.
//
// The two ways to hold a RUNNING guest, cheapest first:
//
//	TOO SOON — we already power-cycled this guest inside the cadence. Nothing has changed.
//	DENIED — the guest's own gate says rebooting it would drop a peer below quorum. The one case
//	where the host defers to knowledge it cannot obtain itself.
//
// An unreachable or stale gate is NOT a hold: see the header. It can only ever withhold a reboot
// that would otherwise happen, never authorise one.
func (r *guestRecovery) next(g gateVerdict, sinceAction time.Duration, unitDown bool) step {
	if unitDown {
		if r.attempts >= guestRelaunchBurst && sinceAction < r.cadenceFor() {
			return stepHold
		}
		r.attempts++
		return stepRelaunch
	}
	if sinceAction < r.cadenceFor() {
		return stepHold
	}
	if g.reached && g.fresh && !g.allowed {
		return stepHold
	}
	r.attempts++
	return stepReboot
}

// recover runs the ladder for one channel-down event and returns the live channel it ends on. It
// returns an error only when ctx ends -- a clean agent shutdown -- because there is no other
// terminal state. It never stops trying: it either gets its guest back or the agent goes down.
//
// It is a method on osUpgrade because osUpgrade already owns this swap. A reboot replaces the
// VM, the channel and the Manager together, and there is exactly one place that knows how to
// put all three back -- the same place the rollback leg uses. A second one would be a second
// way to do it (AGENTS §5).
func (u *osUpgrade) recover(ctx context.Context, r *guestRecovery, n notify.Notifier) (*guestagent.Client, error) {
	var lastAction time.Time // zero -> "nothing done yet this incident"; sinceAction reads as huge
	for {
		sinceAction := time.Duration(1 << 62)
		if !lastAction.IsZero() {
			sinceAction = time.Since(lastAction)
		}

		// IS THE VM EVEN THERE? Asked first, and asked of systemd rather than inferred from the
		// channel, because these are two different questions that the channel gives one answer to.
		// A stopped unit cannot produce a socket however long we wait, so the window would be pure
		// outage -- and this is the ordinary way a guest reboots itself (`-no-reboot`), so it is
		// the common case, not the exotic one.
		if !platform.Running(ctx, u.cfg.guestSpec()) {
			if r.next(gateVerdict{}, sinceAction, true) == stepHold {
				// Logged as well as announced, because announce is a no-op once the incident has
				// already alerted -- and this branch is an ESCALATION (fast relaunches gave up and
				// we are now on the slow cadence), which the owner would otherwise never learn.
				u.logf("guest-recovery: the guest keeps stopping after %d relaunches; "+
					"backing off to the %s cadence", r.attempts, r.cadenceFor())
				u.announce(ctx, n, r, fmt.Sprintf("the guest keeps stopping after being restarted "+
					"%d times, so it is now being restarted every %s rather than immediately.",
					r.attempts, r.cadenceFor()))
				// Sleep out the rest of the cadence by waiting on the channel -- a guest someone
				// else starts is then picked up at once rather than after the full interval.
				if _, err := u.awaitChannel(ctx, r.waitFor()); err == nil {
					continue // fall through to the success path on the next pass
				}
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				continue
			}
			u.logf("guest-recovery: the guest unit is stopped; relaunching (attempt %d)", r.attempts)
			lastAction = time.Now()
			client, err := u.converge(ctx)
			if err != nil {
				u.logf("guest-recovery: relaunch attempt %d failed: %v", r.attempts, err)
				continue
			}
			u.logf("guest-recovery: guest relaunched and converged (attempt %d)", r.attempts)
			u.resolved(ctx, n, r)
			return client, nil
		}

		// The unit is up but not talking. The wait for self-heal: every ordinary bounce returns
		// here on the first pass and the rest of this function never runs. It also runs between
		// reboot attempts, so a guest that comes back on its own during the cadence is picked up
		// within one window rather than waiting out the full two hours.
		client, err := u.awaitChannel(ctx, r.waitFor())
		if err == nil {
			// ANSWERING IS NOT CONVERGED, and telling the two apart is [B.102]. The guest unit is
			// Restart=always and qemu runs -no-reboot, so a guest kernel panic exits qemu and
			// systemd relaunches it with NO agent involved. What comes back is the baked image:
			// runtime identity -- hostname, addresses, the .res -- is applied by bring-up and is
			// never baked, which is why bring-up is deliberately one call. The channel, though,
			// comes back exactly as a bounced in-guest agent's does, so this branch used to
			// re-attach and resume OBSERVING a guest it had not CONVERGED: no second CONVERGED
			// line, connected=0 healthy=false for the rest of the run, the node dark on the wire
			// while the peer served alone.
			if reboot, from, to := u.guestRebooted(client); reboot {
				u.logf("guest-recovery: the guest rebooted underneath us (boot %s -> %s); "+
					"re-converging rather than resuming", from, to)
				// Hand the channel back first: the in-guest agent serves ONE connection at a
				// time, so holding this one open would make bring-up's own re-dial wait out a
				// guest that is answering perfectly well.
				_ = client.Close()
				fresh, cerr := u.converge(ctx)
				if cerr != nil {
					u.logf("guest-recovery: re-converging the rebooted guest failed: %v", cerr)
					continue // back round the ladder, which will reach for a restart
				}
				u.resolved(ctx, n, r)
				return fresh, nil
			}
			u.resolved(ctx, n, r)
			u.rebind(client)
			return client, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err() // agent shutting down, not a wedged guest
		}

		// Ask the guest whether taking it down is safe. Unreachable is not a refusal -- see the
		// header: the gate can withhold a reboot, never authorise one.
		gate := readGate(ctx, deadman.GateAddr(), u.logf)
		if r.next(gate, sinceAction, false) == stepHold {
			// Say it ONCE per incident, whichever branch we are in. The old ladder announced only
			// when it gave up rebooting, so a node the gate keeps denying stayed silently down --
			// the same defect the guest side had, where a held node alerted once and was never
			// heard from again. Holding is a state the owner needs to know about too, because it
			// is the one the house cannot leave without them.
			u.announce(ctx, n, r, fmt.Sprintf("no answer from the guest for %s. The host is not "+
				"restarting it: the guest reports that restarting it would drop the rest of the "+
				"cluster below quorum. It is being left alone and watched.", r.waitFor()))
			continue
		}

		u.announce(ctx, n, r, fmt.Sprintf("no answer from the guest for %s. The host is "+
			"restarting it.", r.waitFor()))
		lastAction = time.Now()
		client, err = u.rebootGuest(ctx)
		if err != nil {
			u.logf("guest-recovery: restart attempt %d failed: %v", r.attempts, err)
			continue
		}
		u.logf("guest-recovery: guest restarted and converged (attempt %d)", r.attempts)
		u.resolved(ctx, n, r)
		return client, nil
	}
}

// RescueGuest rebuilds this node's guest from the verified image under its overlay: stop the VM,
// discard the OS disk, lay down a fresh overlay on the same backing file, bring it up, re-converge.
// The replicated DATA disk is never touched, so the node's identity, its DRBD replica and the
// service manifest pinned on it all survive -- what comes back is the same node with a factory
// code half, not a new node.
//
// B.10's last rung, and the ONLY one that is not a reflex. Everything above it (relaunch, reboot,
// the cadence) fires on its own; this fires when a human, or later the cloud, has read the logs and
// decided. The reasoning is in platform/overlay.go: the remedy is drastic, its result uncertain,
// and the rebuilt guest must re-pull its OCI images over the WAN at the worst possible moment.
// Automating it against an unknown fault would be a coin flip that can deepen the outage.
//
// It is a method on osUpgrade for the reason recover() is: a rebuild replaces the VM, the channel
// and the Manager together, and one place knows how to put all three back.
//
// STOPPING IS CLEAN FIRST, FORCED AS THE FALLBACK -- the same order and the same reason as the
// rollback leg, and it matters MORE here, not less. The data disk is where DRBD keeps its
// metadata, and a clean stop is what lets it record MDF_HAVE_QUORUM + prev_members on the way
// down. Forcing would hand the rebuilt guest a stranded replica to come back to, which on the one
// node whose code half was just discarded is exactly the wrong pairing.
func (u *osUpgrade) RescueGuest(ctx context.Context) error {
	qspec := u.cfg.guestSpec()

	// Refuse BEFORE stopping anything. A node whose disk cannot be rebuilt should keep running the
	// guest it has, and finding that out after the VM is down would turn a refusal into an outage.
	backing, err := qspec.BackingFile(ctx)
	if err != nil {
		return fmt.Errorf("rescue: read the guest disk's backing image: %w", err)
	}
	if backing == "" {
		return fmt.Errorf("rescue: %s is not an overlay (no backing image), so there is nothing to "+
			"rebuild it from; this node was not laid down by install.sh's overlay path", qspec.DiskImage)
	}
	// THE SECOND REFUSAL, and it is not obvious until you ask where the mesh lives. A paired node's
	// DRBD configuration -- the `.res` naming its peers -- is written by the agent into the GUEST,
	// and rebuilding the overlay discards it.
	//
	// ⚠️ THE REASON THIS REFUSAL EXISTED IS GONE, AND LIFTING IT IS A DECISION NOBODY HAS TAKEN.
	// It read "the host does not keep a copy: applyPair unmarshals the cloud's MeshSpec, applies it,
	// and forgets it." Since [V3b.16b] the host DOES keep a copy, durably, and re-pushes it at every
	// bring-up -- which is exactly what the rescue's own bring-up would do. So the refusal is now
	// conservative rather than necessary. It stays until someone decides deliberately, because
	// "probably fine" is the wrong standard for the verb that discards a node's disk, and because
	// the cloud-witness half of a managed pairing is NOT yet restored the same way (the host
	// forwarder is a systemd-run transient unit that only applyPair starts).
	//
	// Until then: rebuilding a PAIRED node is treated as returning it un-meshed -- alive, serving
	// from its own replica, and no longer replicating to anyone until the cloud re-pairs it. That is
	// not a rescue, it is a different kind of outage. Refuse and say who can.
	//
	// Asked over the channel BEFORE the stop, so a node that cannot be rescued keeps the guest it
	// has. An unreadable cluster is NOT a refusal: this verb exists for broken guests, and treating
	// "I could not ask" as "you are paired" would make it useless on exactly those. The cost of
	// being wrong that way is a single-node install being told it is paired; the cost the other way
	// is silently un-meshing an anchor.
	if cl, e := u.client.Cluster(ctx, u.cfg.Resource.Name); e != nil {
		u.logf("rescue: could not read the cluster (%v); proceeding -- this verb is for guests that "+
			"cannot answer, so an unreadable one is expected here", e)
	} else if len(cl.Peers) > 0 {
		return fmt.Errorf("rescue: this node is paired with %d peer(s), and its mesh configuration "+
			"lives on the disk this would discard -- rebuilding would return it un-meshed and only "+
			"the cloud can re-pair it. Rescue is supported on an unpaired node; for a paired one, "+
			"ask the cloud", len(cl.Peers))
	}
	u.logf("rescue: rebuilding the guest from %s (the data disk is not touched)", backing)

	if platform.Running(ctx, qspec) {
		if e := stopCleanly(ctx, u.vm, u.client, u.logf); e != nil {
			u.logf("rescue: could not stop the guest cleanly, forcing (%v)", e)
			if e := u.vm.Stop(); e != nil {
				return fmt.Errorf("rescue: stop the guest: %w", e)
			}
		} else {
			u.logf("rescue: stopped the guest cleanly")
		}
	}

	if _, e := qspec.RebuildOverlay(ctx); e != nil {
		return fmt.Errorf("rescue: %w", e)
	}
	u.logf("rescue: overlay rebuilt; bringing the guest up")

	// Detached, like every other recovery path here: this IS the recovery and must not inherit a
	// deadline only to find there is no time left to finish it. Past this point the old disk is
	// gone, so a bring-up that fails leaves a node that needs another rescue, not a rollback.
	rb, cancel := context.WithTimeout(context.WithoutCancel(ctx), u.cfg.BringUpBudget)
	defer cancel()
	g, client, e := u.cfg.bringUp(rb, qspec, u.logf)
	if e != nil {
		return fmt.Errorf("rescue: bring the rebuilt guest up: %w", e)
	}
	u.vm = g
	u.rebind(client)
	u.logf("rescue: the guest was rebuilt and has re-converged")
	return nil
}

// converge brings the guest to the state the host means it to be in, and is the ONE place that
// does: bringUp launches a stopped guest and adopts a running one (it decides from
// platform.Running), then drives the same hostname -> addresses -> DRBD -> quorate sequence
// either way. Both of this ladder's non-reboot exits therefore share it -- the guest whose unit
// stopped and had to be started, and the guest that came back on its own but came back FRESH
// ([B.102]) -- because they need the identical thing done and there is no second way to do it
// (AGENTS §5).
//
// Detached from the caller's context for the reason the rollback leg is: this IS the recovery,
// and it must not inherit a deadline in order to find there is no time left to recover.
func (u *osUpgrade) converge(ctx context.Context) (*guestagent.Client, error) {
	rb, cancel := context.WithTimeout(context.WithoutCancel(ctx), u.cfg.BringUpBudget)
	defer cancel()

	g, client, err := u.cfg.bringUp(rb, u.cfg.guestSpec(), u.logf)
	if err != nil {
		return nil, fmt.Errorf("guest-recovery: bring the guest up: %w", err)
	}
	u.vm = g
	u.rebind(client)
	return client, nil
}

// guestRebooted reports whether the channel just re-established reached a DIFFERENT boot of the
// guest than the one the host last converged, and the two boot ids so the caller can say so.
//
// Both sides must be known for this to answer yes. A guest too old to report a boot id sends
// nothing, and silence is not evidence of a reboot -- reading it as one would re-converge a
// healthy serving Primary on every ordinary channel bounce, which is a worse fault than the one
// this detects ([B.102]).
func (u *osUpgrade) guestRebooted(fresh *guestagent.Client) (bool, string, string) {
	if u.client == nil || fresh == nil {
		return false, "", ""
	}
	was, now := u.client.BootID(), fresh.BootID()
	return was != "" && now != "" && was != now, was, now
}

// resolved tells the owner the guest is answering again, on BOTH ways out of the ladder -- the
// guest healing itself during a wait, and the host's restart working. Only the first was obvious;
// the second is the common one, and omitting it would mean the owner is told a node is in trouble
// by the mechanism that then fixes it and says nothing. An unannounced incident stays silent:
// there is nothing to close.
//
// ONCE per incident, and it does NOT re-arm the trouble alert -- only served() does, after a
// healthy stretch long enough to call the incident over. Firing this and clearing `announced`
// together is what let a flapping guest send a fresh pair every window.
//
// It says the guest is answering NOW rather than promising the trouble is over, because at this
// point that is all we know: the node converged seconds ago and a crash-looping one will falsify
// anything stronger before the message is read. The previous wording ("No action is needed") was
// the same class of over-claim as the degraded alert's fixed "no answer for <window>" lead.
func (u *osUpgrade) resolved(ctx context.Context, n notify.Notifier, r *guestRecovery) {
	if !r.takeClear() {
		return
	}
	u.fire(ctx, n, notify.Alert{
		Level: notify.Recovered,
		Title: "Briard: the guest is answering again",
		Body: fmt.Sprintf("node %s: the guest is back and serving as of now. If it stops again "+
			"the host will keep restarting it, and will say so if it stops recovering.", u.cfg.Node),
	})
}

// announce fires the degraded alert once per incident. Once, because recover re-evaluates on
// every expiry of the window: a reminder each time would move the flap this rung exists to
// prevent from the VM to the owner's phone.
//
// The caller supplies the WHOLE situation sentence rather than a fragment appended to a fixed
// lead. The fixed lead used to be "no answer from the guest for <window>", which is true on the
// wedged path and false on the crash-loop one -- there the guest was answering seconds ago and
// nothing waited ten minutes for it. An owner-facing sentence that is sometimes untrue is worse
// than a vaguer one that is always true.
func (u *osUpgrade) announce(ctx context.Context, n notify.Notifier, r *guestRecovery, situation string) {
	if !r.takeAnnounce() {
		return
	}
	u.fire(ctx, n, notify.Alert{
		Level: notify.Warning,
		Title: "Briard: the guest has stopped answering",
		Body: fmt.Sprintf("node %s: %s This node is not serving until it comes back.",
			u.cfg.Node, situation),
	})
}

// awaitChannel re-dials until the guest answers or the window closes. It is reconnect() under a
// deadline rather than a second retry loop -- the bring-up path already drives it that way, so
// the backoff, the logging and the handshake stay one implementation.
func (u *osUpgrade) awaitChannel(ctx context.Context, window time.Duration) (*guestagent.Client, error) {
	wctx, cancel := context.WithTimeout(ctx, window)
	defer cancel()
	// The single longest legitimate stall in the agent -- ten minutes on a wedged guest, by design.
	// Leased on its OWN deadline, so the watchdog neither misfires through it nor has to be widened
	// to survive it. Ending the wait ends the lease, whichever way it ends (V3.32).
	u.cfg.beat.Lease(wctx)
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
	// Leased for the STOP phase in particular. bringUp below takes its own lease, but the stop
	// ahead of it does not: stopCleanly spends up to a full shutdownGrace, and u.vm.Stop() takes no
	// context at all -- so rb's deadline bounds this stretch on paper while nothing is watching it.
	// That is precisely the shape the watchdog is for, and precisely why the lease must be here
	// rather than only inside bringUp (V3.32; the un-ctx'd Stop is why an enclosing deadline is not
	// a bound).
	u.cfg.beat.Lease(rb)

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
