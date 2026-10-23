package host

import (
	"context"
	"fmt"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

// WardenName is the warden's name in the service manager.
const WardenName = "briard-warden"

// wardenExe is the warden's program: a copy of the agent laid by the install into its own
// directory, which nothing but an install writes, so a release can never change the program that
// watches it.
const wardenExe = "briard-warden.exe"

// wardenAfter is how many beatless ticks end a running agent: past dumpAfter, so the agent's own
// dump of every goroutine is written before its process is ended.
const wardenAfter = dumpAfter + 2

// startLimit is how long the agent may take to report RUNNING -- the Linux unit's TimeoutStartSec,
// so a start means the same on both systems -- and startAfter is that in ticks.
const (
	startLimit = 60 * time.Second
	startAfter = int(startLimit / beatTick)
)

// Warden is the agent's outside watcher on Windows, the half of systemd's watchdog the service
// manager does not give: it notices the ABSENCE of beats, which no goroutine inside the agent can
// (beat.go), and ends a wedged agent so the service manager's failure actions restart it.
//
// It carries no product knowledge: it reads one bit per tick from the beat event, asks the service
// manager one question, and ends one process. Two limits, the two systemd sets on Linux:
//   - RUNNING is liveness: wardenAfter ticks in a row with no beat. A long step beats under its
//     lease (beat.go), so only silence counts.
//   - START_PENDING is the start: every tick counts, beat or not, and startAfter of them end it.
//     A start is what must finish before the agent can be relied on at all, so it is bounded by
//     its length, like TimeoutStartSec -- anything slow belongs after RUNNING, under a lease.
//
// A stop, or a service with no process, counts nothing, and a new state or process starts the
// count again. Ending the process is the whole act -- a process that ends without reporting STOPPED is a
// failure to the service manager, whose recovery already restarts the agent; the guest runs in its
// own job and the next agent re-adopts it.
func Warden(ctx context.Context, logf func(string, ...any)) error {
	ev, err := beatEvent()
	if err != nil {
		return fmt.Errorf("the beat event: %w", err)
	}
	defer windows.CloseHandle(ev)
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("the service manager: %w", err)
	}
	defer m.Disconnect()
	logf("warden: watching %s, ending it after %d checks with no beat (%s) or a start of %d checks (%s)", ServiceName,
		wardenAfter, (wardenAfter * beatTick).Round(time.Second), startAfter, (time.Duration(startAfter) * beatTick).Round(time.Second))
	count := misses{every: beatTick}
	var last svc.Status
	t := time.NewTicker(beatTick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
		beat := beatSince(ev) // consumed on every tick, so a stale bit never outlives the look
		st := agentStatus(m)
		if st.State != last.State || st.ProcessId != last.ProcessId {
			count, last = misses{every: beatTick}, st
		}
		limit, why := 0, ""
		switch {
		case st.ProcessId == 0:
		case st.State == svc.Running:
			limit, why = wardenAfter, "no beat"
		case st.State == svc.StartPending:
			limit, why, beat = startAfter, "still starting", false
		}
		if n := count.tick(time.Now(), beat || limit == 0); limit > 0 && n >= limit {
			logf("warden: %s from %s for %d checks (%s) -- ending process %d for the service manager to restart",
				why, ServiceName, n, (time.Duration(n) * beatTick).Round(time.Second), st.ProcessId)
			if err := endProcess(st.ProcessId); err != nil {
				logf("warden: ending process %d: %v", st.ProcessId, err)
			}
			count = misses{every: beatTick}
		}
	}
}

// beatSince reports whether the agent beat since the last look, and clears the bit.
func beatSince(ev windows.Handle) bool {
	r, err := windows.WaitForSingleObject(ev, 0)
	return err == nil && r == windows.WAIT_OBJECT_0
}

// agentStatus asks the service manager for the agent's state, opening it afresh each time so a
// service deleted and registered again (a reinstall) is still the one asked about. Any failure to
// ask reads as the zero status, no state and no process: the warden never ends a process it could
// not identify.
func agentStatus(m *mgr.Mgr) svc.Status {
	s, err := m.OpenService(ServiceName)
	if err != nil {
		return svc.Status{}
	}
	defer s.Close()
	st, err := s.Query()
	if err != nil {
		return svc.Status{}
	}
	return st
}

func endProcess(pid uint32) error {
	h, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, pid)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	return windows.TerminateProcess(h, 1)
}
