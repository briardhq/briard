package platform

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// This file is the service-manager half of the platform seam: every place the agent reaches for
// systemd to start, inspect or stop a transient unit. The policy above it -- what a state MEANS,
// how long to wait for a name, when a corpse is worth clearing -- stays OS-neutral in qemu.go
// and forwarder.go, so it reads and tests the same everywhere. What a port owes here is
// re-adoption of a detached, named unit, not detachment; the analogues and the three gaps with
// none are decided in the design.

// startTransient runs a command as a detached, named transient unit that outlives this process.
// args is the whole command line, built purely by the caller so it stays testable off-platform.
func startTransient(ctx context.Context, args []string) ([]byte, error) {
	return exec.CommandContext(ctx, "systemd-run", args...).CombinedOutput()
}

// unitShow reads one property of a unit. The manager answers for units that do not exist too
// (absent reads as "inactive", exit 0), so an error means the QUERY failed -- which is not an
// answer about the unit and must not be read as one.
func unitShow(unit, prop string) (string, error) {
	// Bounded here, for every caller: a manager that does not answer is a query that failed, and
	// no reader of a unit property may wait on it indefinitely.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "systemctl", "show", "-p", prop, "--value", unit).Output()
	return strings.TrimSpace(string(out)), err
}

// unitTail is the unit's last words from the journal: its current invocation's when it has one,
// so a previous launch's exit does not stand in for this one's, and the unit's as a whole when
// it is stopped. "" when the journal does not answer.
func unitTail(ctx context.Context, s QEMUSpec, n int) string {
	match := "-u" // the unit as a whole
	sel := s.unit()
	if id, err := unitShow(s.unit(), "InvocationID"); err == nil && id != "" {
		match, sel = "_SYSTEMD_INVOCATION_ID="+id, ""
	}
	args := []string{match}
	if sel != "" {
		args = append(args, sel)
	}
	args = append(args, "-n", strconv.Itoa(n), "-o", "cat", "--no-pager")
	out, err := exec.CommandContext(ctx, "journalctl", args...).Output()
	if err != nil {
		return ""
	}
	return lastLines(string(out), n)
}

// unitIsActive is the cheap liveness probe. Any non-"active" reading -- including a query that
// failed -- reads as false, so the caller launches fresh rather than adopting a ghost.
func unitIsActive(ctx context.Context, unit string) bool {
	out, _ := exec.CommandContext(ctx, "systemctl", "is-active", unit).Output()
	return strings.TrimSpace(string(out)) == "active"
}

// unitResetFailed clears a finished unit that something still references, freeing its name.
// Best-effort: it is free, and it says nothing to a unit that is still stopping.
func unitResetFailed(unit string) { _ = exec.Command("systemctl", "reset-failed", unit).Run() }

// unitKill SIGKILLs the unit's whole cgroup. Best-effort -- a unit already gone makes it fail,
// and it is the stop that follows which decides whether the guest is really down.
func unitKill(unit string) { _ = exec.Command("systemctl", "kill", "--signal=SIGKILL", unit).Run() }

// unitStop asks the manager to stop the unit, returning its output so the caller can report it.
func unitStop(unit string) ([]byte, error) {
	return exec.Command("systemctl", "stop", unit).CombinedOutput()
}

// unitDisable removes the links a unit's [Install] section made (its WantedBy=), leaving the unit
// itself running and its file where it is.
func unitDisable(unit string) ([]byte, error) {
	return exec.Command("systemctl", "disable", unit).CombinedOutput()
}

// unitsReload makes the manager re-read its unit files, so one that was deleted is forgotten.
func unitsReload() ([]byte, error) {
	return exec.Command("systemctl", "daemon-reload").CombinedOutput()
}

// unitTransient reports whether a unit's FragmentPath is one systemd-run wrote: the manager's own
// file, which it deletes when it collects the unit, and which no [Install] section ever enabled.
func unitTransient(fragment string) bool {
	return strings.HasPrefix(fragment, "/run/systemd/transient/")
}

// unitStamp is where systemd records when a Persistent= timer last fired -- state it writes on the
// unit's behalf, which outlives the unit file unless someone removes it.
func unitStamp(unit string) string { return "/var/lib/systemd/timers/stamp-" + unit }

// StopService stops a long-running service through its ORDINARY stop path and waits, with
// patience, for it to finish what it is doing: SIGTERM to the main process alone, so the
// service's TimeoutStopSec is not the bound (raising that would hold every shutdown), then a poll
// until that process is gone, then `systemctl stop` -- instant once it has exited, it also
// cancels a restart the exit queued, and past the patience it is the force (TimeoutStopSec, then
// SIGKILL). patience 0 is that plain stop alone. The self-update's stop: the agent drains its act
// lane on SIGTERM, so "stopped" means "stopped between acts".
func StopService(ctx context.Context, unit string, patience time.Duration, logf func(string, ...any)) error {
	pid, err := unitShow(unit, "MainPID")
	if err != nil {
		return fmt.Errorf("read %s's main process: %w", unit, err)
	}
	if pid != "" && pid != "0" && patience > 0 {
		if out, err := exec.CommandContext(ctx, "systemctl", "kill", "--signal=SIGTERM", "--kill-who=main", unit).CombinedOutput(); err != nil {
			return fmt.Errorf("ask %s to stop: %w (%s)", unit, err, strings.TrimSpace(string(out)))
		}
		deadline := time.Now().Add(patience)
		for {
			if now, err := unitShow(unit, "MainPID"); err == nil && now != pid {
				break
			}
			if time.Now().After(deadline) {
				logf("%s did not stop within %s; forcing the stop", unit, patience)
				break
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Second):
			}
		}
	}
	if out, err := exec.CommandContext(ctx, "systemctl", "stop", unit).CombinedOutput(); err != nil {
		return fmt.Errorf("stop %s: %w (%s)", unit, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// StartService starts a Type=notify service and returns once it reported READY, or, when the
// start failed (TimeoutStartSec included), an error carrying the service's last lines since the
// start: the reason a candidate did not start is what its revert has to report.
func StartService(ctx context.Context, unit string) error {
	since := "@" + strconv.FormatInt(time.Now().Unix(), 10)
	out, err := exec.CommandContext(ctx, "systemctl", "start", unit).CombinedOutput()
	if err == nil {
		return nil
	}
	if tail, terr := exec.CommandContext(ctx, "journalctl", "-u", unit, "--since", since, "-n", "5", "-o", "cat", "--no-pager").Output(); terr == nil && len(strings.TrimSpace(string(tail))) > 0 {
		out = tail
	}
	return fmt.Errorf("start %s: %w: %s", unit, err, strings.Join(strings.Fields(strings.ReplaceAll(string(out), "\n", " | ")), " "))
}

// PowerOffGuest stops the guest cleanly and returns once it is down: a PLAIN stop of its unit, the
// one RemoveUnit makes, which runs the unit's ExecStop -- the powerdown over the monitor -- and
// returns when the unit has stopped. Never Guest.Stop, which is the kill. During a machine
// shutdown the manager has already queued the same stop, and this one joins it. A unit the manager
// does not know has no guest to stop. The monitor socket is the Windows arm's; here the unit's
// ExecStop names it.
func PowerOffGuest(_ context.Context, _ string, _ func(string, ...any)) error {
	load, err := unitShow(GuestUnit, "LoadState")
	if err != nil {
		return fmt.Errorf("platform: asking about %s: %w", GuestUnit, err)
	}
	if load == "not-found" {
		return nil
	}
	return stopUnit(GuestUnit)
}

// SystemState is the manager's own word for where the machine is (`systemctl is-system-running`):
// "stopping" for the whole of a shutdown or a reboot, from before it stops the first unit, "" when
// it could not be asked. It exits non-zero for every state but "running", so the answer is read
// whatever the exit status.
func SystemState(ctx context.Context) string {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, _ := exec.CommandContext(ctx, "systemctl", "is-system-running").Output()
	return strings.TrimSpace(string(out))
}

// ServiceStopped reports whether a service is down and staying down: stopped, or exited cleanly,
// with no restart queued. A failed unit, and one waiting out its RestartSec (which reads
// "activating"), are on their way back up or need someone, so neither is.
func ServiceStopped(unit string) (bool, error) {
	st, err := unitState(unit)
	if err != nil {
		return false, err
	}
	return stoppedState(st), nil
}

// stoppedState is ServiceStopped's reading of an ActiveState.
func stoppedState(active string) bool { return active == "inactive" }
