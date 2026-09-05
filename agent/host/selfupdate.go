package host

import (
	"context"
	"fmt"
	"os/exec"

	"briard.io/agent/selfupdate"
)

// hostSelfUpdater is the production selfUpdater. Since [B.86a] it is a FLAG-WATCHER plus a
// trigger: the fetch, verify and stage happen below the agent, in the frozen update unit and the
// fresh binary it pulls, so this type keeps only what the running agent must still do -- hand a
// cloud directive's version to that unit, notice an armed candidate, and restart itself at its
// safe point (DETACHED, through the Type=notify pivot).
//
// It is NO LONGER KEYRING-GATED, and that is a trust-boundary change rather than a relocation
// ([B.86c]): the keyring now gates the unit's fetch, and the agent honours an arm flag it did
// not create and cannot verify. That is correct -- verification already happened upstream, on
// bytes the unit staged under the same keyring this agent used to hold -- and it is what lets
// the timer path work on a node whose agent is wedged, which is the case the whole design exists
// for. What the agent can still refuse is a directive naming a version it is already running.
type hostSelfUpdater struct {
	layout  selfupdate.Layout
	unit    string // the agent's own unit, restarted to trial a candidate
	update  string // the frozen update unit, started to fetch one
	version string
	restart func(ctx context.Context, unit string) error // overridable in tests
}

// newSelfUpdater builds the host selfUpdater from config. Never nil: with the fetch below the
// agent there is no keyring to be missing here.
func (cfg Config) newSelfUpdater() selfUpdater {
	unit := cfg.UpdateUnit
	if unit == "" {
		unit = "briard-agent.service"
	}
	return &hostSelfUpdater{
		layout:  selfupdate.New(cfg.UpdateBase, cfg.UpdateRunDir),
		unit:    unit,
		update:  selfupdate.DefaultUpdateUnit,
		version: cfg.Version,
		restart: systemdRestart,
	}
}

// Trigger is the cloud trigger of [B.86a]: the target as a message, one `systemctl start` of the
// frozen unit, its one-line verdict back.
func (h *hostSelfUpdater) Trigger(ctx context.Context, version string) (string, error) {
	return h.layout.Trigger(ctx, h.update, version)
}

func (h *hostSelfUpdater) Armed() bool                       { return h.layout.Armed() }
func (h *hostSelfUpdater) Current() string                   { return h.version }
func (h *hostSelfUpdater) Restart(ctx context.Context) error { return h.restart(ctx, h.unit) }

// systemdRestart restarts unit in a DETACHED transient unit (systemd-run), so the restart job
// isn't in the cgroup of the agent process it is about to replace and can't be torn down with it
// -- the same decoupling used for the guest. --collect reaps the transient unit after it
// exits so a repeated update doesn't leave stale failed units behind.
func systemdRestart(ctx context.Context, unit string) error {
	cmd := exec.CommandContext(ctx, "systemd-run", "--collect",
		"--unit=briard-agent-selfupdate-restart", "systemctl", "restart", unit)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("systemd-run restart %s: %w (%s)", unit, err, out)
	}
	return nil
}

var _ selfUpdater = (*hostSelfUpdater)(nil)
