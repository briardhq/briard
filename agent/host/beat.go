package host

import (
	"context"
	"time"
)

// THE HOST AGENT'S LIVENESS SIGNAL TO systemd — the rung of the unresponsive-guest
// ladder that neither the guest deadman nor the host's own recovery can reach: an agent that is
// RUNNING but WEDGED. systemd sees a healthy process, Restart=on-failure never fires, and on a
// single-node install nothing else can relaunch the guest, because `-no-reboot` means the guest's
// own reflex needs the agent to finish it. A crashed agent self-heals; a hung one does not.
//
// A BARE TIMER GOROUTINE WOULD BE VACUOUS, and that is Go-specific: goroutines are M:N over OS
// threads, so a `time.Ticker` in its own goroutine keeps firing while the observe loop is blocked
// forever on a mutex, a channel or a syscall — reporting "alive" through exactly the failure this
// exists to catch. The ping has to come from the loop whose liveness is the question.
//
// THE LONG OPERATIONS DO NOT FORCE A LONG THRESHOLD. The observe goroutine legitimately blocks for
// minutes — recover() waits guestRecoveryWindow, rebootGuest runs to BringUpBudget+3*shutdownGrace
// — but EVERY long operation is ctx-bounded, so the wedge being hunted is never "too slow"; it is
// a block that IGNORES its context. The threshold is set by the longest gap between two pings,
// which is chosen, not by the longest stall, which is not.
//
// ⚠️ ONE CAVEAT CARRIES THE WEIGHT: an enclosing ctx with a deadline does not make a step bounded.
// rebootGuest builds a 6.5-minute ctx and then calls u.vm.Stop(), which takes no ctx at all; if
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
// Lease covers a single long operation with its OWN budget ctx: ping until that context is done,
// so no interval is ever chosen. It is deliberately NOT an enable/disable flag: `defer Disable()`
// does not run on a goroutine that never returns, so a wedge inside the operation would leave the
// pings running FOREVER — the more completely the agent wedged, the longer the watchdog stayed
// off. A lease expires on its own: normal completion ends it through the caller's `defer
// cancel()`, a wedge ends it at the deadline. Nesting and concurrency need no bookkeeping — the
// watchdog is loose while any lease is alive and tight when the last one exits.
//
// The predicate CHANGES under a lease rather than disappearing: steady state asserts THE LOOP IS
// PROGRESSING; a lease asserts THIS OPERATION FINISHED WITHIN THE BUDGET IT DECLARED. Overrunning
// a declared budget is itself the fault signal, not a blind spot.
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
// Reorder those two and the bug is silent. Checking before the nil return makes it surface in dev
// runs and tests, where there is no watchdog and the mistake would otherwise be invisible until
// production.
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

// budget bounds an operation and puts it under the watchdog IN ONE CALL, because those two facts
// must agree and nothing else makes them. A budget without a lease is the trap: `app install`
// (measured at 48.8 s) declared one but ran unpinged under WatchdogSec=20 and was SIGABRTed — a
// lease gets written where a wedge is feared, not where work is merely long. Through
// budget, forgetting the lease means forgetting the deadline, which is not subtle: an unbounded
// operation on the observe loop is visible, and Lease panics on a context with no deadline.
//
// Nil-safe through Lease, so a dev run, the lab fleet and any harness with no WatchdogSec get the
// deadline and no goroutine.
func (b *beat) budget(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	bctx, cancel := context.WithTimeout(ctx, d)
	b.Lease(bctx)
	return bctx, cancel
}

// The Windows watchers' two numbers, which stand in there for the agent unit's on Linux and are
// pinned to that unit's text by a test, so a change to one side alone is red: staleAfter is
// WatchdogSec, how long the loop may go without a beat before it is a wedge, and startLimit is
// TimeoutStartSec, how long the agent may take to report that it started.
const (
	staleAfter = 20 * time.Second
	startLimit = 60 * time.Second
)

// misses is a watcher's count of its own ticks that saw no beat -- how the Windows watchers (the
// in-process dumper and the warden) measure staleness, where systemd measures it on Linux.
//
// IN TICKS, NEVER ON THE WALL CLOCK, because Windows' monotonic clock runs through a sleep: a
// clock-based check finds hours since the last beat at every resume and reads the machine's sleep
// as a wedged loop. A suspended watcher does not tick, so a sleep adds no miss.
//
// AND A LATE TICK RESETS THE COUNT. A tick arriving far later than its interval means the watcher
// was paused itself -- the machine slept, or was too starved to run it -- and a paused watcher saw
// nothing either way. It is also the resume's grace: the loop gets a full threshold to beat again
// before a wake can trip it. The cost is that a wedge starting just before a sleep trips one
// threshold later.
type misses struct {
	every time.Duration // the watcher's tick interval
	n     int
	last  time.Time
}

// tick records one tick at now, beat saying whether a beat arrived since the previous tick, and
// returns the run of ticks that saw none.
func (m *misses) tick(now time.Time, beat bool) int {
	paused := !m.last.IsZero() && now.Sub(m.last) > 2*m.every
	m.last = now
	if beat || paused {
		m.n = 0
	} else {
		m.n++
	}
	return m.n
}
