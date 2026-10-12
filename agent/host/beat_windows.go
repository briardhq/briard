package host

import (
	"runtime"
	"sync/atomic"
	"time"
)

// staleAfter is how long the loop may go without a beat before its goroutines are dumped -- the
// Linux unit's WatchdogSec, so a wedge reads the same on both systems.
const staleAfter = 20 * time.Second

// newBeat on Windows keeps the beat and does the half of the watchdog that can be done in-process:
// the DIAGNOSIS. There is no external watcher to restart a wedged agent yet, and an in-process one
// could not be trusted to (beat.go: a timer goroutine keeps firing while the loop it vouches for is
// stuck). But that same property is exactly what a stack dump needs -- the dumper keeps running
// while the loop is stuck -- and Windows has no signal that makes the Go runtime print every
// goroutine the way SIGABRT does under systemd's watchdog. So the trip's evidence is produced here,
// once per stall, and the restart is left to the external watcher that does not exist yet.
func newBeat(logf func(string, ...any)) *beat {
	var last atomic.Int64
	last.Store(time.Now().UnixNano())
	go dumpWhenStale(&last, staleAfter, logf)
	return &beat{every: staleAfter / 3, logf: logf, send: func() error {
		last.Store(time.Now().UnixNano())
		return nil
	}}
}

// dumpWhenStale logs every goroutine's stack once the beat has been stale past `after`, and
// re-arms when beats resume, so one stall is one dump rather than one every few seconds. It never
// returns: it runs for the agent's life.
func dumpWhenStale(last *atomic.Int64, after time.Duration, logf func(string, ...any)) {
	dumped := false
	for range time.Tick(after / 3) {
		stale := time.Since(time.Unix(0, last.Load()))
		switch {
		case stale > after && !dumped:
			buf := make([]byte, 4<<20)
			buf = buf[:runtime.Stack(buf, true)]
			logf("watchdog: no beat for %s -- the loop is wedged; every goroutine follows\n%s",
				stale.Round(time.Second), buf)
			dumped = true
		case stale <= after && dumped:
			logf("watchdog: beats resumed")
			dumped = false
		}
	}
}
