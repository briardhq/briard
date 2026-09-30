package host

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"briard.io/agent/platform"
	"briard.io/shared/atomicfile"
	"briard.io/shared/telemetry"
)

// The guest's memory: the size it boots at, how far it may grow, and growing it.
//
// The guest starts small and GROWS, one platform.MemoryStepMB at a time, because no size chosen
// at install is right: sized for the worst case it hoards the household's RAM, sized for the
// common case it is OOM-killed, and "the common case" cannot be measured before the household has
// installed anything. Growth never shrinks back on its own.
//
// THE SIZE IS NODE-LOCAL PET STATE. A guest grows while it runs, and every relaunch -- recovery,
// an OS upgrade, a host restart -- must boot it at the size it grew to, not at the configured
// floor it had already outgrown. And every launch builds its spec from a COPY of cfg, so the size
// is read from its file each time rather than carried in a field some copy would hold stale.

// guestMemoryName is the record of the size the guest has grown to, beside the node's other
// records.
const guestMemoryName = "guest-memory"

// hostMemoryReserveMB is what the guest may never grow into: the host's own OS, the agent and
// QEMU's overhead. The report card refuses a host below 4 GB, so the smallest admitted host can
// still give its guest half its RAM.
const hostMemoryReserveMB = 2048

var errMemoryCeiling = errors.New("the guest is at its memory ceiling")

func (cfg Config) guestMemoryPath() string {
	if cfg.AssignmentCache == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(cfg.AssignmentCache), guestMemoryName)
}

// guestMemoryMB is the size the guest boots at: what it has grown to, never below cfg.MemoryMB. A
// missing or unreadable record reads as "never grown".
func (cfg Config) guestMemoryMB() int {
	n := cfg.MemoryMB
	if p := cfg.guestMemoryPath(); p != "" {
		if b, err := os.ReadFile(p); err == nil {
			if v, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && v > n {
				n = v
			}
		}
	}
	return n
}

// guestMemoryCeilingMB is how far the guest may grow on a host with hostMB of RAM; 0 when that
// cannot be read, which leaves the guest at the size it boots at rather than guessing.
func guestMemoryCeilingMB(hostMB int) int {
	if c := hostMB - hostMemoryReserveMB; hostMB > 0 && c > 0 {
		return c
	}
	return 0
}

// growGuestMemory adds one step to the running guest through add and records the new size, so
// every later launch boots at it. At the ceiling it adds nothing and returns errMemoryCeiling.
//
// ADD FIRST, RECORD SECOND: the record says what the guest HAS. A failure between the two leaves
// a guest bigger than its record, whose next launch boots one step smaller -- a step growth can
// take again -- never a record claiming memory the guest was refused.
func (cfg Config) growGuestMemory(ctx context.Context, hostMB int, add func(context.Context) error, logf func(string, ...any)) (int, error) {
	cur := cfg.guestMemoryMB()
	next := cur + platform.MemoryStepMB
	if next > guestMemoryCeilingMB(hostMB) {
		return cur, errMemoryCeiling
	}
	if err := add(ctx); err != nil {
		return cur, fmt.Errorf("grow the guest's memory past %d MB: %w", cur, err)
	}
	if p := cfg.guestMemoryPath(); p != "" {
		if err := atomicfile.Write(p, []byte(strconv.Itoa(next)+"\n"), 0o644, 0o700); err != nil {
			logf("memory: grew the guest to %d MB but could not record it at %s: %v -- its next launch boots at %d MB", next, p, err, cur)
		}
	}
	return next, nil
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
