package platform

import (
	"context"
	"os/exec"
	"strings"
)

// This file is the service-manager half of the platform seam: every place the agent reaches for
// systemd to start, inspect or stop a transient unit. The policy above it -- what a state MEANS,
// how long to wait for a name, when a corpse is worth clearing -- stays OS-neutral in qemu.go
// and forwarder.go, so it reads and tests the same everywhere. What a port owes here is
// re-adoption of a detached, named unit, not detachment; the analogues and the three gaps with
// none are decided in DESIGN §9.9.1.

// startTransient runs a command as a detached, named transient unit that outlives this process.
// args is the whole command line, built purely by the caller so it stays testable off-platform.
func startTransient(ctx context.Context, args []string) ([]byte, error) {
	return exec.CommandContext(ctx, "systemd-run", args...).CombinedOutput()
}

// unitShow reads one property of a unit. The manager answers for units that do not exist too
// (absent reads as "inactive", exit 0), so an error means the QUERY failed -- which is not an
// answer about the unit and must not be read as one.
func unitShow(unit, prop string) (string, error) {
	out, err := exec.Command("systemctl", "show", "-p", prop, "--value", unit).Output()
	return strings.TrimSpace(string(out)), err
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
