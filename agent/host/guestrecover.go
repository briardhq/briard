package host

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"briard.io/agent/guestagent"
	"briard.io/agent/guestagent/deadman"
	"briard.io/agent/platform"
	"briard.io/shared/notify"
)

// The host-side rung of the unresponsive-guest ladder.
//
// The control channel survives a drop (observe returns ErrChannelDown, Run re-dials), which is
// right for every gap the guest closes by itself (the in-guest agent serves one connection then
// exits, an OS upgrade bounces the channel, a per-call deadline closes it) and wrong for a guest
// that is not coming back: patience alone is a node that stays down while the host logs a
// reconnect line every few seconds and nobody is told. So the wait is bounded, then the host pulls
// the only lever it holds over a guest that will not talk, taking the VM down and back up, and
// repeats at the same steady cadence indefinitely: it never gives up. A K-attempt bound looks
// prudent and is wrong: at a two-hour cadence a retry costs almost nothing, occasionally catches a
// condition that cleared on its own, and is the visible sign of degradation a remote fix looks
// for; the bound only buys a node with no reflex left (see guestRebootCadence).
//
// The gate is the guest's answer, not the host's question. The host cannot evaluate "don't reboot
// a guest that is still serving": DRBD's quorum view lives in the guest, the app-health verb rides
// the channel that just died, and under the default macvtap substrate the host cannot reach the
// VIP either. The guest's deadman still runs, reads DRBD and is not what wedged: it serves the
// RebootAllowed it applies to itself every tick over raw TCP on the guest's node IP
// (deadman/gate.go), dialled from the host's own system-subnet address (gateprobe.go).
//
// Unreachable means allowed, deliberately, the opposite default from the guest side (where an
// unreadable cluster is not allowed): each fails toward the safer answer from where it stands. In
// the guest, not knowing the cluster means not knowing whether a peer depends on you, so you hold.
// Out here, failing to reach a gate a healthy guest answers in milliseconds means the guest is far
// past wedged, the case this rung exists for; refusing to act would restore the wedged-forever
// outcome the ladder is against. So the gate can only ever withhold a reboot, never authorise one
// that would not otherwise have happened.
//
// The guest goes first: guestRecoveryWindow sits above the deadman's T_deadman on purpose, so the
// in-guest graceful reboot (its promoter teardown demotes cleanly and lets DRBD record its quorum
// metadata on the way down) gets its chance before the host power-cycles. The host backstops the
// case where `systemctl reboot` itself failed; the other order would spend the clean demote on
// every incident where the kernel was fine and only the agent had wedged.
//
// The residual risk, named rather than argued away: a guest whose agent is wedged and whose
// deadman is dead (gate unreachable) while its apps still serve gets rebooted, and on a node with
// no peer that is an outage the host chose. That is the trade locked in (serving is not healthy;
// reboot serving nodes, patiently), confined to two independent in-guest processes both gone.

const (
	// How long a dead control channel gets to heal itself before the host calls the guest wedged.
	// It must exceed the guest deadman's own threshold (deadman.DefaultDeadman + its jitter
	// window), and that ordering is the design, not a safety margin: whoever fires first decides
	// whether the house gets the guest's clean reboot or the host's power cycle (header).
	// TestTheGuestDeadmanFiresBeforeTheHostActs asserts the inequality, because a later tuning of
	// either number could silently invert it with every other test still passing. Generous in
	// absolute terms too: the reconnect that matters lands in about a second, so silence at this
	// scale is not a slow guest but a stopped one. Unit tests set it through guestRecovery.window
	// and need no knob; the VM tests, which drive the shipped binary end to end, cannot (see
	// guestRecoveryWindowOverride).
	guestRecoveryWindow = 10 * time.Minute

	// The interval between host-driven reboots of a guest that stays wedged. The same constant
	// the guest deadman paces itself by -- one cadence for one ladder, so the two rungs cannot be
	// tuned apart. It never runs out (header).
	guestRebootCadence = deadman.RebootCadence

	// A channel that stayed up at least this long ends the incident: the next failure is a new
	// one, announced afresh rather than folded into an alert the owner already read and acted on.
	guestRecoveryReset = 30 * time.Minute

	// How many times a stopped guest is relaunched immediately before relaunches fall back to the
	// cadence. Not a give-up: it never stops, it only stops being instant. A guest that panics its
	// way through boot exits (`-no-reboot`), and undamped the agent would relaunch it as fast as
	// QEMU could die. Three tries cover the transient crash; a fourth in quick succession is a
	// broken node, and hammering it helps nobody.
	guestRelaunchBurst = 3

	// The floor between a failed relaunch and the next. Three shots inside one second are not
	// three tries: whatever the launch failed on (a unit name still stopping, a device the kernel
	// has not released) cannot have cleared in the time it takes to ask systemd twice, so without
	// the floor the burst is spent in one second and the node sits out the two-hour cadence. It
	// floors only the failing path: a relaunch that starts a VM leaves recover, and a guest that
	// dies again is damped by the burst and then the cadence, so this costs an outage nothing in
	// the case the ladder is normally in.
	guestRelaunchFloor = 30 * time.Second

	// A gate verdict older than this is treated as no verdict at all. It catches the deadman
	// whose evaluation loop has wedged while its accept loop still answers -- the one failure
	// where the gate keeps replying and the reply means nothing. The deadman re-evaluates every
	// 15s, so anything at this scale is not a slow tick.
	guestGateStale = 5 * time.Minute
)

// guestRecovery is the ladder's state across one agent lifetime.
//
// The two alert latches are both incident-scoped, and only served() clears them. Clearing
// `announced` when the all-clear goes out looks tidy and re-arms the trouble alert: a guest that
// recovers and fails again inside one incident would send a fresh trouble/all-clear pair each
// time round -- every ten minutes, indefinitely, for a node flapping on that period, which is the
// phone-flap this rung exists to avoid arriving by a different door.
type guestRecovery struct {
	window    time.Duration // wait-for-self-heal before judging the guest wedged; 0 -> guestRecoveryWindow
	cadence   time.Duration // interval between reboots of a still-wedged guest; 0 -> guestRebootCadence
	reset     time.Duration // uptime that ends an incident; 0 -> guestRecoveryReset
	floor     time.Duration // minimum gap between a failed relaunch and the next; 0 -> guestRelaunchFloor
	attempts  int           // reboots spent on the current incident
	announced bool          // the degraded alert has been sent for this incident
	cleared   bool          // the all-clear has been sent for this incident

	mu    sync.Mutex
	doing string // what the ladder is doing now and when it next acts, for the local door (say / status)
}

// say records what the ladder is doing, and status reads it: the sentence the local door answers
// with while the ladder runs (recoverAnswering), read from a different goroutine than the one
// acting. Times are the household's clock, since the person reading is at the machine.
func (r *guestRecovery) say(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.doing = fmt.Sprintf(format, args...)
}

func (r *guestRecovery) status() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.doing
}

// at is a moment as the door says it.
func at(t time.Time) string { return t.Format("15:04") }

// tryNow is the way out every refusal and alert of the ladder names: the agent runs throughout,
// so `briard start` has nothing to start and keeps its one meaning; a stop powers the dead
// guest off and a start is a fresh agent with a fresh ladder, which launches at once.
const tryNow = "To try now: `briard stop`, then `briard start`."

// guestRecoveryWindowOverride shortens the wait above, and exists only for the VM tests that
// drive this ladder end to end on a real guest (nixosTest/agent-recover.nix,
// nixosTest/agent-readopt.nix): a test that proves a ten-minute window by waiting it out spends
// ~90% of its tier's wall clock on one idle wait. With the override the VM test still runs the
// shipped binary on a real wedged guest and asserts the host waited the configured window,
// proving the timer is wired to the value rather than to a number typed into the test.
//
// ⚠️ So the VM tests do not prove that the shipped value outlasts the guest deadman, which is what
// guarantees the guest's graceful reboot always gets its chance, the host only backstopping it.
// TestTheGuestDeadmanFiresBeforeTheHostActs compares the two constants directly and is the only
// thing standing between a future tuning and a silent inversion of that design.
//
// Never set in production: a shortened window power-cycles guests that were healing themselves,
// a worse outage than the one it recovers from.
var guestRecoveryWindowOverride = func() time.Duration {
	d, err := time.ParseDuration(os.Getenv("BRIARD_GUEST_RECOVERY_WINDOW"))
	if err != nil || d <= 0 {
		return 0 // absent, unparseable or nonsense: the constant stands
	}
	return d
}()

func (r *guestRecovery) waitFor() time.Duration {
	if r.window <= 0 {
		if guestRecoveryWindowOverride > 0 {
			return guestRecoveryWindowOverride
		}
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

func (r *guestRecovery) floorFor() time.Duration {
	if r.floor <= 0 {
		return guestRelaunchFloor
	}
	return r.floor
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

// The two alert latches, as decisions rather than `if` statements inside the emitters: the
// pure-decision-under-a-driver split the rest of this file uses, and here what makes them
// testable at all. Inline in announce()/resolved(), the only test is a local re-implementation of
// the same condition, which passes whatever the product does. Each consumes its latch when it
// returns true, so a caller cannot forget to record that it fired.

// takeAnnounce reports whether the degraded alert should go out now, once per incident.
func (r *guestRecovery) takeAnnounce() bool {
	if r.announced {
		return false
	}
	r.announced = true
	return true
}

// takeClear reports whether the all-clear should go out now: once per incident, and only for an
// incident that actually announced something. It deliberately does not re-arm takeAnnounce -- only
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

// next is the whole decision, kept apart from anything that can restart a VM so it can be proven
// without one (the guest-side deadman's pure Decide under a driver is the same split). Calling it
// consumes an attempt when it acts, because that is a one-shot fact about the incident and a
// caller that had to record it separately would eventually forget on one path.
//
// Stopped is a different case from silent, and separating them is what makes the guest's own
// reboot work. Because of `-no-reboot` every guest restart (its deadman's graceful reboot, a
// kernel panic, an OOM kill) ends the VM's unit, so the host's only signal, a dead control
// channel, is shared by two very different situations. A stopped unit has no socket that could
// ever appear (waiting the window is pure outage), nothing running to consult a gate about, and
// cannot outage a peer, so nothing the gate could protect: relaunch at once. That also closes the
// circularity the guest deadman would otherwise have: its reboot needs the agent to finish, and
// the agent finishes it in seconds rather than after a ten-minute wait for a dead process's
// socket. The relaunch burst damps a crash loop without ever giving up: instant while attempts
// remain, then the same slow cadence as everything else.
//
// A running guest is held in two cases, cheapest first: too soon (already power-cycled inside the
// cadence; nothing has changed), and denied (its own gate says rebooting it would drop a peer
// below quorum, the one case where the host defers to knowledge it cannot obtain itself). An
// unreachable or stale gate is not a hold (header): it can only ever withhold a reboot that would
// otherwise happen, never authorise one.
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
// returns an error only when ctx ends (a clean agent shutdown), because there is no other
// terminal state: it never stops trying, so it either gets its guest back or the agent goes down.
// It is a method on osUpgrade because osUpgrade already owns this swap: a reboot replaces the VM,
// the channel and the Manager together, and exactly one place, the one the rollback leg uses,
// knows how to put all three back; a second would be a second way to do it.
//
// It is also the only door for a guest that never spoke at bring-up: Run enters here with no
// prior client (u.client nil). Ending the agent instead looks like a clean give-up and is not, for
// a guest that is alive and dead inside (a vCPU WHPX gave up on, a kernel halted in a panic): the
// next agent adopts the same qemu, and a clean exit is a normal stop to systemd and the SCM alike,
// which stays down, a dead qemu holding the sockets and nobody told. The ladder's first question
// (is the unit even there) already splits that case the right way. Nothing detects the death;
// silence past the window is the verdict, and the logs are read only to say why (announce).
func (u *osUpgrade) recover(ctx context.Context, r *guestRecovery, n notify.Notifier) (*guestagent.Client, error) {
	var lastAction time.Time // zero -> "nothing done yet this incident"; sinceAction reads as huge
	defer r.say("")
	for {
		sinceAction := time.Duration(1 << 62)
		if !lastAction.IsZero() {
			sinceAction = time.Since(lastAction)
		}

		// Is the VM even there? Asked first, and of systemd rather than inferred from the channel,
		// which gives these two different questions one answer. A stopped unit cannot produce a
		// socket however long we wait, so the window would be pure outage, and this is the ordinary
		// way a guest reboots itself (`-no-reboot`): the common case, not the exotic one.
		if !platform.Running(ctx, u.cfg.guestSpec()) {
			if r.next(gateVerdict{}, sinceAction, true) == stepHold {
				// Logged as well as announced: announce is a no-op once the incident has alerted,
				// and this branch is an escalation (fast relaunches gave up, now the slow cadence)
				// the owner would otherwise never learn of.
				u.logf("guest-recovery: the guest keeps stopping after %d relaunches; "+
					"backing off to the %s cadence", r.attempts, r.cadenceFor())
				r.say("the VM keeps stopping; Briard restarts it again at %s", at(lastAction.Add(r.cadenceFor())))
				u.announce(ctx, n, r, fmt.Sprintf("the VM keeps stopping after being restarted "+
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
			r.say("the VM stopped; Briard is restarting it")
			lastAction = time.Now()
			client, err := u.converge(ctx)
			if err != nil {
				u.logf("guest-recovery: relaunch attempt %d failed: %v", r.attempts, err)
				// The floor. A launch that fails in milliseconds must not spend the next attempt
				// in the same second -- see guestRelaunchFloor: the budget is three tries, and a
				// try that could not possibly have found the condition changed is not one.
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-time.After(r.floorFor()):
				}
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
		r.say("the VM is not answering; Briard waits until %s for it before restarting it", at(time.Now().Add(r.waitFor())))
		client, err := u.awaitChannel(ctx, r.waitFor())
		if err == nil {
			// Answering is not converged. A channel can come back from a boot this agent never
			// converged (a guest started by anything but this ladder), and what boots is the baked
			// image: runtime identity (hostname, addresses, the .res) is applied by bring-up and
			// never baked, which is why bring-up is deliberately one call. That channel looks
			// exactly like a bounced in-guest agent's, and re-attaching resumes observing a guest
			// nobody converged: no CONVERGED line, connected=0 healthy=false for the rest of the
			// run, the node dark on the wire while the peer serves alone. So a new boot id
			// re-converges, and, one step more general for the ladder entered from bring-up, so
			// does a channel this agent has never converged (u.client nil), whatever boot it
			// belongs to: it may reach the guest the previous agent converged or the baked image
			// after a reboot nobody watched, and bring-up tells them apart for free.
			if reboot, from, to := u.guestRebooted(client); reboot || u.client == nil {
				if u.client == nil {
					u.logf("guest-recovery: the guest answers, and this agent has not converged it yet; converging")
				} else {
					u.logf("guest-recovery: the guest rebooted underneath us (boot %s -> %s); "+
						"re-converging rather than resuming", from, to)
				}
				// Hand the channel back first: the in-guest agent serves one connection at a
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
		// The wait ended because the unit stopped (awaitChannel): back to the top, which
		// relaunches a stopped unit at once rather than rebooting a guest that is not there.
		if !platform.Running(ctx, u.cfg.guestSpec()) {
			continue
		}

		// Ask the guest whether taking it down is safe. Unreachable is not a refusal -- see the
		// header: the gate can withhold a reboot, never authorise one.
		gate := readGate(ctx, deadman.GateAddr(u.cfg.guestNodeIP()), u.cfg.hostNodeIP(), u.logf)
		if r.next(gate, sinceAction, false) == stepHold {
			// Say it once per incident, whichever branch: a node the gate keeps denying must not
			// stay silently down, and holding is the one state the house cannot leave without the
			// owner.
			if !lastAction.IsZero() && sinceAction < r.cadenceFor() {
				r.say("the VM is not answering after a restart; Briard restarts it again at %s", at(lastAction.Add(r.cadenceFor())))
			} else {
				r.say("the VM is not answering, and restarting it would drop the rest of the cluster below quorum; Briard is leaving it alone and watching")
			}
			u.announce(ctx, n, r, fmt.Sprintf("no answer from the VM for %s. Briard is not "+
				"restarting it: the VM reports that restarting it would drop the rest of the "+
				"cluster below quorum. It is being left alone and watched.", r.waitFor()))
			continue
		}

		r.say("the VM is not answering; Briard is restarting it")
		u.announce(ctx, n, r, fmt.Sprintf("no answer from the VM for %s. Briard is "+
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

// converge brings the guest to the state the host means it to be in, and is the one place that
// does: bringUp launches a stopped guest and adopts a running one (deciding from
// platform.Running), then drives the same hostname -> addresses -> DRBD -> quorate sequence
// either way. Both of the ladder's non-reboot exits share it (the guest whose unit stopped and had
// to be started, and the guest that came back on its own but fresh), because they need the
// identical thing done and there is no second way to do it. Detached from the caller's context for
// the rollback leg's reason: this is the recovery, and it must not inherit a deadline only to find
// there is no time left to recover.
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

// guestRebooted reports whether the channel just re-established reached a different boot of the
// guest than the one the host last converged, and the two boot ids so the caller can say so. Both
// must be known to answer yes: a guest too old to report a boot id sends nothing, and silence is
// not evidence of a reboot; reading it as one would re-converge a healthy serving Primary on every
// ordinary channel bounce, a worse fault than the one this detects.
func (u *osUpgrade) guestRebooted(fresh *guestagent.Client) (bool, string, string) {
	if u.client == nil || fresh == nil {
		return false, "", ""
	}
	was, now := u.client.BootID(), fresh.BootID()
	return was != "" && now != "" && was != now, was, now
}

// resolved tells the owner the guest is answering again, on both ways out of the ladder: the guest
// healing itself during a wait, and the host's restart working. Omitting the second, the common
// one, would mean the owner is told a node is in trouble by the mechanism that then fixes it and
// says nothing. An unannounced incident stays silent: there is nothing to close. Once per
// incident, and it does not re-arm the trouble alert; only served() does, after a healthy stretch
// long enough to call the incident over (see guestRecovery). It says the guest is answering now
// rather than promising the trouble is over ("no action is needed"), because that is all we know:
// the node converged seconds ago and a crash-looping one will falsify anything stronger before the
// message is read.
func (u *osUpgrade) resolved(ctx context.Context, n notify.Notifier, r *guestRecovery) {
	if !r.takeClear() {
		return
	}
	u.fire(ctx, n, notify.Alert{
		Key:   "guest",
		Kind:  notify.Resolved,
		Title: "Briard: the VM is answering again",
		Body: fmt.Sprintf("machine %s: the VM is back and serving as of now. If it stops again "+
			"Briard will keep restarting it, and will say so if it stops recovering.", u.cfg.Node),
	})
}

// announce fires the degraded alert once per incident: recover re-evaluates on every expiry of the
// window, and a reminder each time would move the flap this rung exists to prevent from the VM to
// the owner's phone.
//
// The caller supplies the whole situation sentence rather than a fragment after a fixed lead: "no
// answer from the VM for <window>" is true on the wedged path and false on the crash-loop one,
// where the guest was answering seconds ago and nothing waited ten minutes for it. An owner-facing
// sentence that is sometimes untrue is worse than a vaguer one that is always true.
//
// The cause is read here, never decided on: the ladder judges on silence, and only then are the
// logs asked what the guest and its VM said last (platform.LastLines: the WHPX exit code or the
// kernel's panic line, where there is one), so the household is told why, from what the log
// already holds, instead of "stopped answering" alone. The way out is named too: stop + start is
// the person's "try now" (tryNow).
func (u *osUpgrade) announce(ctx context.Context, n notify.Notifier, r *guestRecovery, situation string) {
	if !r.takeAnnounce() {
		return
	}
	body := fmt.Sprintf("machine %s: %s This machine is not serving until it comes back.", u.cfg.Node, situation)
	if last := platform.LastLines(ctx, u.cfg.guestSpec()); last != "" {
		u.logf("guest-recovery: the guest's last words: %s", last)
		body += " The VM's last words: " + last + "."
	}
	u.fire(ctx, n, notify.Alert{
		Key:      "guest",
		Kind:     notify.Open,
		Severity: notify.Critical,
		Title:    "Briard: the VM has stopped answering",
		Body:     body + " " + tryNow,
	})
}

// recoverAnswering runs the ladder while the local door is answered (holdDoor): a stop is taken,
// and anything else is refused with what the ladder is doing and when it next acts. Without it
// the door has no reader for as long as the ladder runs -- `briard status` hangs, the tray reads
// a healthy process that says nothing, and `briard stop` works only through its fallback, the
// service manager, blaming an agent that "does not answer". It is what makes stop + start the ordinary
// way to cut a wait short rather than a fallback.
func (cfg Config) recoverAnswering(ctx context.Context, local <-chan localRequest, u *osUpgrade, r *guestRecovery, n notify.Notifier, logf func(string, ...any)) (*guestagent.Client, error) {
	done := make(chan struct{})
	var client *guestagent.Client
	var err error
	go func() {
		defer close(done)
		client, err = u.recover(ctx, r, n)
	}()
	_ = cfg.holdDoor(ctx, local, func() string { return r.status() + ". " + tryNow }, done, logf)
	<-done
	return client, err
}

// awaitChannel re-dials until the guest answers or the window closes. It is reconnect() under a
// deadline rather than a second retry loop -- the bring-up path already drives it that way, so
// the backoff, the logging and the handshake stay one implementation.
//
// The unit is asked again while we wait. The channel dies when the in-guest agent stops, which in
// a clean poweroff is before qemu exits, so the check at the top of recover() can read "running"
// about a guest that is seconds from gone -- and a stopped unit cannot produce a socket however
// long we wait (a Windows resume's clean restart is this shape). A unit that stops during the wait
// ends it early, and the ladder relaunches at once instead of after the window.
func (u *osUpgrade) awaitChannel(ctx context.Context, window time.Duration) (*guestagent.Client, error) {
	wctx, cancel := context.WithTimeout(ctx, window)
	defer cancel()
	// The single longest legitimate stall in the agent -- ten minutes on a wedged guest, by design.
	// Leased on its own deadline, so the watchdog neither misfires through it nor has to be widened
	// to survive it. Ending the wait ends the lease, whichever way it ends.
	u.cfg.beat.Lease(wctx)
	spec := u.cfg.guestSpec()
	go func() {
		tick := time.NewTicker(unitRecheckEvery)
		defer tick.Stop()
		for {
			select {
			case <-wctx.Done():
				return
			case <-tick.C:
				if !platform.Running(wctx, spec) {
					cancel()
					return
				}
			}
		}
	}()
	return reconnect(wctx, u.cfg.ControlSock, u.logf)
}

// unitRecheckEvery is how often a wait for the channel asks whether the guest's unit still runs.
const unitRecheckEvery = 5 * time.Second

// rebootGuest takes the wedged guest down and brings it back, replacing the whole binding. Its
// context is detached from the caller's, as the rollback leg's is from the upgrade's: this is the
// recovery, and it must not inherit a deadline only to discover there is no time left to recover.
// The window that just expired is the caller's evidence, not this call's budget.
func (u *osUpgrade) rebootGuest(ctx context.Context) (*guestagent.Client, error) {
	rb, cancel := context.WithTimeout(context.WithoutCancel(ctx), u.cfg.BringUpBudget+3*shutdownGrace)
	defer cancel()
	// Leased for the stop phase in particular: bringUp below takes its own lease, but the stop
	// ahead of it does not. stopCleanly spends up to two shutdownGraces and a reapGrace, and
	// u.vm.Stop() takes no context at all, so rb's deadline bounds this stretch on paper while
	// nothing watches it: precisely the shape the watchdog is for, and why the lease must be here
	// rather than only inside bringUp.
	u.cfg.beat.Lease(rb)

	qspec := u.cfg.guestSpec()
	if platform.Running(rb, qspec) {
		// Clean first, forced as the fallback, in the rollback leg's order and for its reason:
		// the data disk is where DRBD keeps its metadata, and a clean stop lets it record
		// MDF_HAVE_QUORUM + prev_members on the way down, the difference between a node that
		// comes back quorate and one that is stranded. What differs is which route lands: the
		// agent route asks a channel already known dead, but the ACPI button works more often
		// than the situation suggests, because a wedged agent is not a wedged kernel and the
		// kernel answers the power button. Trying it is most of the value of stopping cleanly at
		// all on this path.
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
	// idempotent precisely so it can be re-driven like this.
	g, client, e := u.cfg.bringUp(rb, qspec, u.logf)
	if e != nil {
		return nil, fmt.Errorf("guest-recovery: bring the guest back up: %w", e)
	}
	u.vm = g
	u.rebind(client)
	return client, nil
}

// fire sends the ladder's alerts the way every host-side alert leaves (fireAlert): the store
// writes the record first and only then attempts delivery.
func (u *osUpgrade) fire(ctx context.Context, n notify.Notifier, al notify.Alert) {
	fireAlert(ctx, n, u.logf, al)
}

// RescueGuest is a restart: stop the guest and bring it up again on its read-only image, its
// scratch emptied at boot and everything it is dressed with pushed afresh. The data disk and the
// state disk's persistent half are not touched: with the OS disk unwritable by construction there
// is nothing for it to discard, and the verb survives as the name of the gesture.
func (u *osUpgrade) RescueGuest(ctx context.Context) error {
	qspec := u.cfg.guestSpec()
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
	rb, cancel := context.WithTimeout(context.WithoutCancel(ctx), u.cfg.BringUpBudget)
	defer cancel()
	g, client, e := u.cfg.bringUp(rb, qspec, u.logf)
	if e != nil {
		return fmt.Errorf("rescue: bring the guest up on a fresh OS disk: %w", e)
	}
	u.vm = g
	u.rebind(client)
	u.logf("rescue: the guest was rebuilt and has re-converged")
	return nil
}

// RebootGuest stops the guest cleanly and brings it back up, so the bring-up re-reads what was
// just recorded: a topology transition's membership (node-storage then runs the spec × disk
// table), a `config set vip`, a newly leased address. It is the recovery ladder's power cycle,
// called on purpose, and on this type rather than beside it for the reason recover is: a
// second owner of the VM+channel+Manager swap would be a second way to do it.
func (u *osUpgrade) RebootGuest(ctx context.Context) error {
	_, err := u.rebootGuest(ctx)
	return err
}

// episodeReader is the guest's deadman.episode verb, as the observe loop sees it: an optional
// capability of the reader, because an older guest has none and must not be asked.
type episodeReader interface {
	DeadmanEpisode(ctx context.Context) (deadman.LastEpisode, bool, error)
	SupportsDeadmanEpisode() bool
}

// collectDeadmanEpisode asks the guest what it did while this agent was away and tells the
// owner. The guest reports, the host alerts: the deadman's only way out of the house is the
// host agent, the thing that was down, so its record waits for the host, and the host -- the
// side with the alert store and the endpoint -- raises the alert. An event, not a condition:
// the stretch is over by the time anyone can speak of it.
func (cfg Config) collectDeadmanEpisode(ctx context.Context, r guestReader, n notify.Notifier, logf func(string, ...any)) {
	er, ok := r.(episodeReader)
	if !ok || !er.SupportsDeadmanEpisode() {
		return
	}
	ectx, cancel := context.WithTimeout(ctx, 5*time.Second)
	ep, found, err := er.DeadmanEpisode(ectx)
	cancel()
	if err != nil {
		logf("deadman: could not collect the guest's episode: %v", err)
		return
	}
	if !found {
		return
	}
	fireAlert(ctx, n, logf, hostUnreachableAlert(cfg.Node, ep))
}

// hostUnreachableAlert is what the owner is told about a stretch the guest spent without the
// host agent: how long, and what the guest did about it -- held, or rebooted itself to come
// back or fail over.
func hostUnreachableAlert(node string, ep deadman.LastEpisode) notify.Alert {
	did := "kept serving on its own and waited"
	switch ep.Reboots {
	case 1:
		did = "restarted itself once to recover"
	default:
		if ep.Reboots > 1 {
			did = fmt.Sprintf("restarted itself %d times to recover", ep.Reboots)
		}
	}
	return notify.Alert{
		Key:      "host-unreachable",
		Kind:     notify.Event,
		Severity: notify.Warning,
		Title:    "Briard: the host agent was unreachable",
		Body: fmt.Sprintf("machine %s's host agent was unreachable from %s to %s (%s). The VM %s. "+
			"It is back now; if this was not a planned restart of the machine, check why the agent stopped.",
			node, ep.Since.Local().Format("Mon 2 Jan 15:04"), ep.Until.Local().Format("15:04"),
			ep.Until.Sub(ep.Since).Round(time.Minute), did),
	}
}
