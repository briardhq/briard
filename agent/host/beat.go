package host

import (
	"context"
	"os"
	"strconv"
	"time"

	"briard.io/shared/sdnotify"
)

// THE HOST AGENT'S LIVENESS SIGNAL TO systemd (V3.32) — the rung of the unresponsive-guest
// ladder that neither the guest deadman nor the host's own recovery can reach: an agent that is
// RUNNING but WEDGED. systemd sees a healthy process, Restart=on-failure never fires, and on a
// single-node install nothing else can relaunch the guest, because `-no-reboot` means the guest's
// own reflex needs the agent to finish it. A crashed agent self-heals; a hung one does not.
//
// WHY A BARE TIMER GOROUTINE WOULD BE VACUOUS, AND WHY THAT IS GO-SPECIFIC.
//
// Goroutines are M:N over real OS threads. A `time.Ticker` in its own goroutine keeps firing on
// another thread while the observe loop is blocked forever on a mutex, a channel or a syscall —
// so it would report "alive" through exactly the failure this exists to catch. (In an event-loop
// runtime a wedged handler wedges the timer too, and a heartbeat timer WOULD be a real signal.
// Here it is not.) The ping has to come from the loop whose liveness is the question.
//
// WHY THE LONG OPERATIONS DO NOT FORCE A LONG THRESHOLD.
//
// The obvious objection is that the observe goroutine legitimately blocks for minutes — recover()
// waits guestRecoveryWindow, rebootGuest runs to BringUpBudget+3*shutdownGrace — so the threshold
// would have to exceed the longest legitimate stall, which is long enough that the watchdog would
// rarely be the thing that notices.
//
// It does not, because EVERY long operation here is already ctx-bounded. A legitimate operation
// that overran would have had a deadline fire. So the wedge being hunted is never "too slow" — it
// is a block that IGNORES its context — and the threshold is set by the longest gap between two
// pings, which is chosen, rather than by the longest stall, which is not.
//
// ⚠️ ONE CAVEAT CARRIES THE WEIGHT: an enclosing ctx with a deadline does not make a step bounded.
// rebootGuest builds a 9.5-minute ctx and then calls u.vm.Stop(), which takes no ctx at all; if
// that hangs, the deadline does nothing, because nobody is watching it. The per-step question is
// whether every blocking call inside either takes the ctx or has a watcher closing its fd.
//
// SO: Beat() BETWEEN BOUNDED STEPS, Lease() ACROSS THEM.
//
// Beat is called in the observe loop before each of its bounded calls. One ping at the top of the
// loop would not do: the loop's calls carry 5s deadlines each, so an ordinary slow-but-healthy
// cycle can run ~35s, and it gives nothing at all once observe returns into recover(), where
// there is no top of loop to be at.
//
// Lease covers a single long operation with its OWN budget: ping until that context is done. It
// is deliberately NOT a flag with an enable/disable pair. `defer Disable()` runs on return and on
// panic — not on a goroutine that never returns — so a wedge inside the operation would leave the
// pings running FOREVER, disabling the watchdog permanently, and the more completely the agent
// wedged the longer it would stay off. A lease expires on its own, so releasing early is an
// optimisation rather than a correctness requirement.
//
// It also means no interval is ever chosen: the lease IS the operation's existing budget ctx.
// Normal completion ends it through the `defer cancel()` already there; a wedge ends it at the
// deadline. Nesting and concurrency need no bookkeeping either — the goroutine's lifetime is the
// lease, so the watchdog is loose while any is alive and tight when the last one exits.
//
// The predicate changes under a lease rather than disappearing. Steady state asserts THE LOOP IS
// PROGRESSING; a lease asserts THIS OPERATION FINISHED WITHIN THE BUDGET IT DECLARED. Overrunning
// a declared budget is itself the fault signal, which is why the second is an assertion and not a
// blind spot.
//
// AND THE PAYOFF IS THE TRACEBACK, NOT THE RESTART. WatchdogSignal= defaults to SIGABRT, and Go's
// runtime answers SIGABRT by dumping goroutine stacks and dying — so a trip leaves the stack of
// every goroutine at the moment it wedged, for a bug whose whole difficulty is leaving no
// evidence. That requires GOTRACEBACK=all on the unit: the default (`single`) dumps the current
// goroutine, which at signal-delivery time is an arbitrary one and therefore useless here.
type beat struct {
	every time.Duration // lease ping interval, derived from systemd's WATCHDOG_USEC
	send  func() error  // seam: sdnotify.Watchdog in production
	logf  func(string, ...any)
}

// newBeat reads the watchdog contract out of the environment systemd sets, and returns nil when
// there is no watchdog to feed — no WatchdogSec on the unit (a dev run, the lab fleet, a
// nixosTest that writes its own unit), or a WATCHDOG_PID naming someone else.
//
// nil is a WORKING beat, not a broken one: every method is nil-safe, so an agent running outside
// systemd spawns no goroutines and sends nothing, and no caller needs to ask which it has.
//
// The interval comes from the unit rather than a constant here, so WatchdogSec has exactly one
// definition and the two cannot drift apart. A third of the period rather than the documented
// half: the margin costs one extra datagram per interval, and misfiring is the one outcome a
// watchdog must not have.
func newBeat(logf func(string, ...any)) *beat {
	usec, err := strconv.ParseUint(os.Getenv("WATCHDOG_USEC"), 10, 64)
	if err != nil || usec == 0 {
		return nil // no WatchdogSec on this unit -> nothing expects pings
	}
	// systemd sets WATCHDOG_PID when it wants the pings from one specific process. Absent (the
	// ordinary case under NotifyAccess=main) means "whoever holds the socket".
	if pid := os.Getenv("WATCHDOG_PID"); pid != "" && pid != strconv.Itoa(os.Getpid()) {
		return nil
	}
	period := time.Duration(usec) * time.Microsecond
	logf("systemd watchdog armed: period %s, pinging every %s", period, period/3)
	return &beat{every: period / 3, send: sdnotify.Watchdog, logf: logf}
}

// Beat sends one keep-alive. Call it between bounded steps — it is a datagram write, so the cost
// is in the gaps you leave, not in the calls you make.
func (b *beat) Beat() {
	if b == nil {
		return
	}
	// Logged rather than swallowed: a failing ping means the watchdog is about to trip anyway, so
	// this is at most a few lines before the traceback, and a silent one would make the trip look
	// like a wedge when it was a broken socket.
	if err := b.send(); err != nil {
		b.logf("watchdog ping failed (the watchdog will trip): %v", err)
	}
}

// Lease pings on the caller's behalf until ctx is done — for a single long operation, passing
// that operation's OWN budget context. See the file header for why this is a lease and not a flag.
//
// It PANICS on a context with no deadline, and the check runs before the nil-beat return on
// purpose. Leasing an unbounded context rebuilds the permanent-disable bug the lease exists to
// avoid, and it would do it invisibly: context.WithoutCancel strips the deadline along with the
// cancellation, and it is used on exactly these paths with a WithTimeout wrapped back around it.
// Reorder those two and the bug is silent. Checking before the nil return is what makes it
// surface in dev runs and tests, where there is no watchdog and the goroutine would never spawn —
// i.e. everywhere the mistake would otherwise be invisible until production.
//
// A panic rather than a clamp, deliberately: a clamp ships a real bug as a slightly-wrong
// threshold, and the one place this feature can afford to be loud is the one that would otherwise
// leave no evidence.
func (b *beat) Lease(ctx context.Context) {
	if _, ok := ctx.Deadline(); !ok {
		panic("host: beat.Lease on a context with no deadline — lease the operation's own budget " +
			"ctx, not an unbounded parent (see beat.go)")
	}
	if b == nil {
		return
	}
	go func() {
		t := time.NewTicker(b.every)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				b.Beat()
			}
		}
	}()
}
