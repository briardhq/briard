package selfupdate

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

const (
	agentName = "briard-agent.exe"

	// DefaultBase is where the committed agent and every release entry beside it live, so a swap
	// is a same-volume rename.
	DefaultBase = `C:\ProgramData\briard\agent`

	// DefaultRunDir is the dir the update messages pass through. Nothing in it decides a swap:
	// that state is the .prev entries under Base.
	DefaultRunDir = `C:\ProgramData\briard\run`

	// DefaultUpdateUnit is the frozen scheduled task that runs the update below the agent; every
	// trigger -- the cloud's directive, the task's own schedule, `briard update` -- runs it, and the
	// task's one-instance rule (a second start while one runs is ignored) is what makes it its own
	// mutual exclusion, as the oneshot is on Linux.
	DefaultUpdateUnit = "briard-update"

	// DefaultAgentUnit is the service the updater stops, swaps under and starts again.
	DefaultAgentUnit = "briard-agent"
)

// launcher is the frozen script the task runs, laid beside the agent by the install.
const launcher = "briard-update.ps1"

// runLimit bounds a wait on a run: the task's own time limit, with room.
const runLimit = 65 * time.Minute

// runLog is where a run's own log is read.
func runLog(string) string { return `C:\ProgramData\briard\log\briard-update.log` }

// startRun runs the task. `schtasks /run` returns once the task has started, never when it ends,
// so a wait is a wait for the run's verdict: the result file written after started. A start that
// merged into a run already going waits for that run's verdict, as `systemctl start` does.
func (l Layout) startRun(ctx context.Context, unit string, wait bool, started time.Time) ([]byte, error) {
	if _, err := os.Stat(filepath.Join(l.Base, launcher)); err != nil {
		return nil, fmt.Errorf("this node has no update launcher (%v); reinstall briard to keep updating", err)
	}
	out, err := exec.CommandContext(ctx, "schtasks", "/run", "/tn", unit).CombinedOutput()
	if err != nil || !wait {
		return out, err
	}
	deadline := time.Now().Add(runLimit)
	for {
		if r, ok := l.ReadResult(); ok && r.At.After(started) {
			if !r.OK {
				return nil, errors.New(r.Line)
			}
			return nil, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("the %s task left no verdict within %s", unit, runLimit)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Second):
		}
	}
}
