package host

import (
	"context"
	"errors"
	"fmt"
	"time"

	"briard.io/agent/platform"
	"briard.io/shared/telemetry"
)

// The guest's memory: how far it may grow, and growing it.
//
// The guest starts small and GROWS, one platform.MemoryStepMB at a time, because no size chosen
// at install is right: sized for the worst case it hoards the household's RAM, sized for the
// common case it is OOM-killed, and "the common case" cannot be measured before the household has
// installed anything.
//
// GROWTH LASTS UNTIL THE NEXT LAUNCH, AND NO LONGER. Every launch boots at its boot size
// (bootMemoryMB) and grows again if it needs to. A DIMM cannot reliably be taken back from a
// running guest, so a relaunch is the one moment memory returns to the host -- and making every
// relaunch that moment is what keeps a rare leak, or a one-off burst, from being carried into
// every boot after it. What it costs is a guest that genuinely needs more re-growing after a
// restart, one window at a time, in zram meanwhile rather than out of memory.

// hostMemoryReserveMB is what the guest may never grow into: the host's own OS, the agent and
// QEMU's overhead. The report card refuses a host below 4 GB, so the smallest admitted host can
// still give its guest half its RAM.
const hostMemoryReserveMB = 2048

var errMemoryCeiling = errors.New("the guest is at its memory ceiling")

// guestMemoryCeilingMB is how far the guest may grow on a host with hostMB of RAM; 0 when that
// cannot be read, which leaves the guest at the size it boots at rather than guessing.
func guestMemoryCeilingMB(hostMB int) int {
	if c := hostMB - hostMemoryReserveMB; hostMB > 0 && c > 0 {
		return c
	}
	return 0
}

// growGuestMemory adds one step to the running guest and returns its new size. The current size
// is the VM's own answer (size), not something the agent remembers: an agent restarted while the
// guest kept running has nothing to remember it with. At the ceiling it adds nothing and returns
// errMemoryCeiling.
func growGuestMemory(ctx context.Context, hostMB int, size func(context.Context) (int, error), add func(context.Context) error) (int, error) {
	cur, err := size(ctx)
	if err != nil {
		return 0, fmt.Errorf("read the guest's memory size: %w", err)
	}
	if cur+platform.MemoryStepMB > guestMemoryCeilingMB(hostMB) {
		return cur, errMemoryCeiling
	}
	if err := add(ctx); err != nil {
		return cur, fmt.Errorf("grow the guest's memory past %d MB: %w", cur, err)
	}
	return cur + platform.MemoryStepMB, nil
}

// guestSystemMB is the guest's own share, before any service: the OS, the agent, the kernel's
// reservations. Measured, not guessed: a guest with no service boots with ~380 MB of its 964 MB
// MemTotal not available.
const guestSystemMB = 384

// memoryNeeded is what the guest must hold for its services from their first second -- the
// system's share plus every installed service's declared minimum, with `except` left out and
// extraMB added (an install replacing or adding one service). Growth covers whatever a service
// needs beyond its minimum; this covers what it needs before growth can react.
func (cfg Config) memoryNeeded(except string, extraMB int) int {
	n := guestSystemMB + extraMB
	for _, s := range cfg.Services {
		if s.Name != except {
			n += s.MinMemoryMB
		}
	}
	return n
}

// bootMemoryMB is the size every launch boots at: the configured size, or more when the installed
// services' minimums say so.
func (cfg Config) bootMemoryMB() int { return max(cfg.MemoryMB, cfg.memoryNeeded("", 0)) }

// growGuestMemoryTo grows the running guest one step at a time until it holds at least needMB.
// A target above the ceiling is refused up front (errMemoryCeiling) rather than half-reached.
func growGuestMemoryTo(ctx context.Context, needMB, hostMB int, size func(context.Context) (int, error), add func(context.Context) error) error {
	if needMB > guestMemoryCeilingMB(hostMB) {
		if cur, err := size(ctx); err == nil && cur >= needMB {
			return nil // it already holds it (it booted there)
		}
		return errMemoryCeiling
	}
	for {
		cur, err := size(ctx)
		if err != nil {
			return fmt.Errorf("read the guest's memory size: %w", err)
		}
		if cur >= needMB {
			return nil
		}
		if _, err := growGuestMemory(ctx, hostMB, size, add); err != nil {
			return err
		}
	}
}

// When the guest grows. Two conditions, each a number the resources verb already reports, each
// sustained so a spike is absorbed (by zram, by the kernel) rather than answered:
//
//   - LEVEL: MemAvailable below max(memLowFloorMB, memLowShare of MemTotal) for memLowFor. It
//     already reflects zram -- parking cold memory RAISES it by the compression saving -- so it
//     falls only once zram stops keeping up: slow growth, a new service settling in.
//   - PRESSURE: memory PSI "some" (avg60) above memPressurePct for memPressureFor. The case the
//     level cannot see: a working set that does not fit, swapping in and out of zram while
//     MemAvailable looks fine.
//
// A step restarts both windows, so each later decision is measured on the guest AFTER its last
// step; memGrowCooldown then bounds how often a step can come at all, which is what paces the
// pressure rule (its window is shorter). Page cache never triggers either: it counts as available
// and it stalls nothing.
const (
	memLowFloorMB   = 256
	memLowShare     = 0.15
	memLowFor       = 5 * time.Minute
	memPressurePct  = 10.0
	memPressureFor  = time.Minute
	memGrowCooldown = 5 * time.Minute
)

// memoryGrower holds the decision's clocks across observe cycles. Zero value ready.
type memoryGrower struct {
	lowSince, pressureSince time.Time // zero: the condition does not hold right now
	lastGrow                time.Time
}

// decide reports whether the guest should grow by a step now, given this cycle's sample. An
// unread sample (no MemTotal) says nothing, so it clears the clocks rather than extending them:
// a condition must hold across samples that were actually read.
func (m *memoryGrower) decide(now time.Time, r *telemetry.NodeResources) bool {
	if r == nil || r.MemTotalKB == 0 {
		m.lowSince, m.pressureSince = time.Time{}, time.Time{}
		return false
	}
	floorKB := max(int64(memLowFloorMB)<<10, int64(float64(r.MemTotalKB)*memLowShare))
	since := func(t *time.Time, holds bool) time.Duration {
		if !holds {
			*t = time.Time{}
			return 0
		}
		if t.IsZero() {
			*t = now
		}
		return now.Sub(*t)
	}
	low := since(&m.lowSince, r.MemAvailableKB < floorKB)
	pressure := since(&m.pressureSince, r.MemPSISome60 > memPressurePct)
	if !m.lastGrow.IsZero() && now.Sub(m.lastGrow) < memGrowCooldown {
		return false
	}
	if low < memLowFor && pressure < memPressureFor {
		return false
	}
	m.lastGrow = now
	m.lowSince, m.pressureSince = time.Time{}, time.Time{}
	return true
}
