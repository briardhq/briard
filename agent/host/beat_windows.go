package host

import (
	"runtime"
	"sync/atomic"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// staleAfter is how long the loop may go without a beat before its goroutines are dumped -- the
// Linux unit's WatchdogSec, so a wedge reads the same on both systems. The watchers tick at a
// third of it and count in ticks (misses), so the dump comes at dumpAfter beatless ticks.
const (
	staleAfter = 20 * time.Second
	beatTick   = staleAfter / 3
	dumpAfter  = 3
)

// beatEventName is the named event the agent sets at every beat and the warden consumes at every
// tick: one bit across the process boundary, "a beat arrived since your last look". Auto-reset,
// so the warden's look is also the clear -- a message, never state. Global, because both are
// services in session 0 and a name outside Global\ is per session.
const beatEventName = `Global\briard-agent-beat`

// beatEventSDDL lets only SYSTEM and the Administrators open the event: a process that could set
// it could vouch for a wedged agent and keep the warden from ending it.
const beatEventSDDL = "D:P(A;;GA;;;SY)(A;;GA;;;BA)"

// beatEvent creates the beat event, or opens it when the other side made it first. Whoever holds
// a handle keeps the name alive, and the warden holds one for its life, so an agent restart
// re-opens the same event.
func beatEvent() (windows.Handle, error) {
	sd, err := windows.SecurityDescriptorFromString(beatEventSDDL)
	if err != nil {
		return 0, err
	}
	name, err := windows.UTF16PtrFromString(beatEventName)
	if err != nil {
		return 0, err
	}
	sa := &windows.SecurityAttributes{SecurityDescriptor: sd}
	sa.Length = uint32(unsafe.Sizeof(*sa))
	h, err := windows.CreateEvent(sa, 0 /* auto-reset */, 0, name)
	if err == windows.ERROR_ALREADY_EXISTS {
		err = nil // opened, not created: the warden made it
	}
	return h, err
}

// newBeat on Windows keeps the beat and does the half of the watchdog that can be done in-process:
// the DIAGNOSIS. The restart is the warden's (warden_windows.go), which this feeds through the
// beat event; an in-process detector could not be trusted to restart (beat.go: a timer goroutine
// keeps firing while the loop it vouches for is stuck). But that same property is exactly what a
// stack dump needs -- the dumper keeps running while the loop is stuck -- and Windows has no signal
// that makes the Go runtime print every goroutine the way SIGABRT does under systemd's watchdog. So
// the trip's evidence is produced here, once per stall, before the warden's threshold ends the
// process.
func newBeat(logf func(string, ...any)) *beat {
	var beaten atomic.Bool
	ev, err := beatEvent()
	if err != nil {
		logf("watchdog: no beat event for the warden (%v) -- a wedge is still diagnosed here, but nothing restarts it", err)
	}
	go dumpWhenStale(&beaten, beatTick, logf)
	return &beat{every: beatTick, logf: logf, send: func() error {
		beaten.Store(true)
		if ev == 0 {
			return nil
		}
		return windows.SetEvent(ev)
	}}
}

// dumpWhenStale logs every goroutine's stack once dumpAfter ticks in a row have seen no beat, and
// re-arms when beats resume, so one stall is one dump rather than one every few seconds. It never
// returns: it runs for the agent's life.
func dumpWhenStale(beaten *atomic.Bool, every time.Duration, logf func(string, ...any)) {
	m := misses{every: every}
	dumped := false
	for range time.Tick(every) {
		beat := beaten.Swap(false)
		n := m.tick(time.Now(), beat)
		switch {
		case n >= dumpAfter && !dumped:
			buf := make([]byte, 4<<20)
			buf = buf[:runtime.Stack(buf, true)]
			logf("watchdog: no beat for %d checks (%s) -- the loop is wedged; every goroutine follows\n%s",
				n, (time.Duration(n) * every).Round(time.Second), buf)
			dumped = true
		case beat && dumped:
			logf("watchdog: beats resumed")
			dumped = false
		}
	}
}
