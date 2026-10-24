package host

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"

	"briard.io/agent/platform"
)

// WardenName is the warden's name in the service manager.
const WardenName = "briard-warden"

// wardenExe is the warden's program: a copy of the agent in its own directory, laid by the install
// and replaced by an update once the agent it watches is up (StartWarden).
const wardenExe = "briard-warden.exe"

// wardenAfter is how many beatless ticks end a running agent: past dumpAfter, so the agent's own
// dump of every goroutine is written before its process is ended.
const wardenAfter = dumpAfter + 2

// startAfter is startLimit (beat.go) in ticks.
const startAfter = int(startLimit / beatTick)

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
			if err := platform.EndProcess(st.ProcessId); err != nil {
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

// THE WARDEN IS ONE MORE ENTRY of the self-update: its program is a copy of the agent, so an
// update that moves the agent moves it too. It is stopped FIRST (StopWarden), before the agent,
// so it does not count the agent's quiet as a wedge, and because its program must not be running
// to be replaced; it is brought to the agent in place LAST (StartWarden), once the agent is up.
// It is put back on its own when it does not start, and the agent never waits on it.

// StopWarden stops the warden for an update.
func StopWarden(ctx context.Context, logf func(string, ...any)) error {
	return platform.StopService(ctx, WardenName, 0, logf)
}

// StartWarden starts the warden, first swapping its program for the agent in place when converge
// is set and the two differ: the old copy to .prev, the agent copied in, started, and the .prev
// deleted -- or, when the new one does not reach RUNNING, put back and started. Without converge
// (the agent did not start, so what is in place is a candidate being reverted) the warden starts
// as it is. A swap cut short is finished here: a .prev with no program is put back, a .prev
// beside one is the tail of a commit.
func StartWarden(ctx context.Context, converge bool, logf func(string, ...any)) error {
	agent := filepath.Join(prefixDir, "agent", agentExe)
	w := filepath.Join(prefixDir, "warden", wardenExe)
	prev := w + ".prev"
	if exists(prev) {
		if !exists(w) {
			if err := os.Rename(prev, w); err != nil {
				return fmt.Errorf("putting the warden back: %w", err)
			}
		} else {
			os.Remove(prev)
		}
	}
	if converge {
		if same, err := sameFile(agent, w); err != nil {
			logf("update: comparing the warden with the agent: %v; starting it as it is", err)
		} else if !same {
			return swapWarden(ctx, agent, w, logf)
		}
	}
	return platform.StartService(ctx, WardenName)
}

func swapWarden(ctx context.Context, agent, w string, logf func(string, ...any)) error {
	logf("update: the warden is a copy of an older agent; replacing it")
	next, prev := w+".next", w+".prev"
	if err := installFile(agent, next, 0o755); err != nil {
		logf("update: copying the agent for the warden: %v; starting the old one", err)
		return platform.StartService(ctx, WardenName)
	}
	if err := os.Rename(w, prev); err != nil {
		os.Remove(next)
		return fmt.Errorf("moving the old warden aside: %w", err)
	}
	if err := os.Rename(next, w); err != nil {
		os.Rename(prev, w)
		return fmt.Errorf("placing the new warden: %w", err)
	}
	err := platform.StartService(ctx, WardenName)
	if err == nil {
		os.Remove(prev)
		return nil
	}
	logf("update: the new warden did not start (%v); putting the old one back", err)
	if serr := platform.StopService(ctx, WardenName, 0, logf); serr != nil {
		logf("update: stopping the new warden: %v", serr)
	}
	if rerr := os.Remove(w); rerr != nil {
		return fmt.Errorf("the new warden did not start (%v), and it could not be removed: %w", err, rerr)
	}
	if rerr := os.Rename(prev, w); rerr != nil {
		return fmt.Errorf("the new warden did not start (%v), and the old one could not be put back: %w", err, rerr)
	}
	return platform.StartService(ctx, WardenName)
}

// sameFile reports whether two files hold the same bytes.
func sameFile(a, b string) (bool, error) {
	ha, err := fileHash(a)
	if err != nil {
		return false, err
	}
	hb, err := fileHash(b)
	if err != nil {
		return false, err
	}
	return ha == hb, nil
}

func fileHash(p string) ([sha256.Size]byte, error) {
	var sum [sha256.Size]byte
	f, err := os.Open(p)
	if err != nil {
		return sum, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return sum, err
	}
	copy(sum[:], h.Sum(nil))
	return sum, nil
}
