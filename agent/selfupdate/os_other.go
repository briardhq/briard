//go:build !windows

package selfupdate

import (
	"context"
	"os/exec"
	"time"
)

const (
	agentName = "briard-agent"

	// DefaultBase is where the committed agent and every release entry beside it live, so a swap
	// is a same-filesystem rename.
	DefaultBase = "/opt/briard/agent"

	// DefaultRunDir is the tmpfs dir the update messages pass through. Nothing in it decides a
	// swap: that state is the .prev entries under Base, which a power cut cannot clear.
	DefaultRunDir = "/run/briard"

	// DefaultUpdateUnit is the frozen oneshot that runs the update below the agent; every trigger
	// -- the cloud's directive, the timer, `briard update` -- is a `systemctl start` of it, and
	// systemd merging a start into a running job is what makes the unit its own mutual exclusion.
	DefaultUpdateUnit = "briard-update.service"

	// DefaultAgentUnit is the service the updater stops, swaps under and starts again.
	DefaultAgentUnit = "briard-agent.service"
)

// runLog is where a run's own log is read.
func runLog(unit string) string { return "journalctl -u " + unit }

// startRun starts the oneshot. `systemctl start` blocks until the run has finished, which is the
// wait; --no-block returns once the start is queued.
func (Layout) startRun(ctx context.Context, unit string, wait bool, _ time.Time) ([]byte, error) {
	if !wait {
		return exec.CommandContext(ctx, "systemctl", "start", "--no-block", unit).CombinedOutput()
	}
	return exec.CommandContext(ctx, "systemctl", "start", unit).CombinedOutput()
}
