package host

import (
	"context"
	"fmt"
	"os/exec"
	"time"
)

// restartSelf restarts unit in a DETACHED transient unit (systemd-run), so the restart job
// isn't in the cgroup of the agent process it is about to replace and can't be torn down with it
// -- the same decoupling used for the guest. --collect reaps the transient unit after it
// exits so a repeated update doesn't leave stale failed units behind.
func restartSelf(ctx context.Context, unit string) error {
	// systemd-run returns once the job is queued, not when the restart ends; a bound is for a
	// manager that does not answer, which must not hold the observe loop.
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "systemd-run", "--collect",
		"--unit=briard-agent-selfupdate-restart", "systemctl", "restart", unit)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("systemd-run restart %s: %w (%s)", unit, err, out)
	}
	return nil
}
