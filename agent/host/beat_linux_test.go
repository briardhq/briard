package host

import (
	"os"
	"strconv"
	"testing"
	"time"
)

// Outside systemd there is no watchdog to feed, and the agent must run exactly as without a beat --
// no goroutines, no sends, no caller needing to ask which kind of beat it holds.
func TestNewBeatDisabledWithoutWatchdogEnv(t *testing.T) {
	t.Setenv("WATCHDOG_USEC", "")
	if b := newBeat(quietf); b != nil {
		t.Fatalf("no WATCHDOG_USEC should mean no watchdog, got %+v", b)
	}
	t.Setenv("WATCHDOG_USEC", "not-a-number")
	if b := newBeat(quietf); b != nil {
		t.Fatal("an unparseable WATCHDOG_USEC should disable rather than guess")
	}
	// WATCHDOG_PID names the one process systemd wants the pings from. Answering for another
	// process would report liveness we cannot vouch for.
	t.Setenv("WATCHDOG_USEC", "30000000")
	t.Setenv("WATCHDOG_PID", strconv.Itoa(os.Getpid()+1))
	if b := newBeat(quietf); b != nil {
		t.Fatal("a WATCHDOG_PID naming another process should disable the beat")
	}
}

// The interval is derived from the unit rather than declared here, so WatchdogSec has one
// definition. Asserted because a constant here would drift silently.
func TestNewBeatDerivesIntervalFromUnit(t *testing.T) {
	t.Setenv("WATCHDOG_USEC", "30000000") // WatchdogSec=30
	t.Setenv("WATCHDOG_PID", strconv.Itoa(os.Getpid()))
	b := newBeat(quietf)
	if b == nil {
		t.Fatal("WATCHDOG_USEC set for this pid should arm the beat")
	}
	if want := 10 * time.Second; b.every != want {
		t.Fatalf("ping interval = %s, want %s (a third of the period)", b.every, want)
	}
	if b.every*2 >= 30*time.Second {
		t.Fatalf("interval %s leaves no margin under a 30s period", b.every)
	}
}
