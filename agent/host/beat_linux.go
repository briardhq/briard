package host

import (
	"os"
	"strconv"
	"time"

	"briard.io/shared/sdnotify"
)

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
