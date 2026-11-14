package host

import (
	"context"
	"time"
)

// The host agent's liveness signal to systemd, for the rung of the unresponsive-guest ladder that
// neither the guest deadman nor the host's own recovery reaches: an agent that runs but is wedged.
// systemd sees a healthy process, so Restart=on-failure never fires; a crashed agent self-heals, a
// hung one does not. On a single node nothing else can relaunch the guest (`-no-reboot` means its
// own reflex needs the agent).
//
// The ping must come from the observe loop itself, for a Go-specific reason: goroutines are M:N
// over OS threads, so a ticker in its own goroutine keeps reporting "alive" while the loop is
// blocked forever on a mutex, channel or syscall.
//
// Long operations don't force a long threshold. The loop legitimately blocks for minutes
// (recover() waits guestRecoveryWindow, rebootGuest up to BringUpBudget+3*shutdownGrace), but
// every such wait is ctx-bounded, so the wedge hunted is never "too slow" but a block that ignores
// its context; the threshold is the longest gap between pings, which is chosen, not the longest
// stall, which is not. ⚠️ A ctx deadline bounds only calls that take the ctx or have a watcher
// closing their fd: rebootGuest's 6.5-minute ctx does nothing if u.vm.Stop(), which takes no ctx,
// hangs.
//
// Beat runs before each bounded call in the loop, not once per iteration: the 5 s deadlines add up
// to ~35 s on a slow healthy cycle, and recover() has no loop top. Lease covers one long operation
// by pinging until its own budget ctx is done, so no interval is chosen. It is deliberately not an
// enable/disable flag: `defer Disable()` never runs on a goroutine that never returns, so a wedge
// inside the operation would leave the watchdog off forever: the worse the wedge, the longer. A
// lease ends at the caller's `defer cancel()` or at the deadline, and nesting and concurrency need
// no bookkeeping: the watchdog is loose while any lease is alive, tight when the last one exits. A
// lease changes the predicate from "the loop is progressing" to "this operation finished within the
// budget it declared", so an overrun is itself the fault signal, not a blind spot.
//
// The payoff is the traceback, not the restart: WatchdogSignal= defaults to SIGABRT, which Go
// answers by dumping goroutine stacks and dying, evidence for a bug that otherwise leaves none.
// That needs GOTRACEBACK=all on the unit; the default `single` dumps the current goroutine, an
// arbitrary one at signal-delivery time.
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

// Lease pings on the caller's behalf until ctx is done — for a single long operation, passed that
// operation's own budget context (the file header says why a lease and not a flag). It panics on a
// context with no deadline, before the nil-beat return so dev runs and tests, which have no
// watchdog, catch the mistake before production: leasing an unbounded context silently rebuilds the
// permanent-disable bug the lease exists to avoid, and context.WithoutCancel strips the deadline
// along with the cancellation and is used on exactly these paths with a WithTimeout wrapped back
// around it, so swapping the two goes unnoticed. A panic rather than a clamp, deliberately: a clamp
// ships a real bug as a slightly-wrong threshold, and the one place this feature can afford to be
// loud is the one that would otherwise leave no evidence.
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

// budget bounds an operation and puts it under the watchdog in one call, because the two must agree
// and nothing else makes them. A budget without a lease is the trap: `app install` (measured at
// 48.8 s) declared one but ran unpinged under WatchdogSec=20 and was SIGABRTed — a lease gets
// written where a wedge is feared, not where work is merely long. Through budget, forgetting the
// lease means forgetting the deadline, which is not subtle: an unbounded operation on the observe
// loop is visible, and Lease panics on a context with no deadline. Nil-safe through Lease: a dev
// run, the lab fleet or any harness with no WatchdogSec gets the deadline and no goroutine.
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
// in-process dumper and the warden) measure staleness, where systemd does on Linux. Ticks, never
// the wall clock: Windows' monotonic clock runs through a sleep, so a clock-based check would read
// every resume as a wedged loop, while a suspended watcher does not tick. A tick far later than its
// interval means the watcher was paused itself (the machine slept, or was too starved to run it)
// and saw nothing either way, so it resets the count. That is also the resume's grace, a full
// threshold for the loop to beat again before a wake can trip it, at the cost that a wedge starting
// just before a sleep trips one threshold later.
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
