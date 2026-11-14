package host

import (
	"context"

	"briard.io/agent/selfupdate"
)

// hostSelfUpdater is the production selfUpdater: a trigger and nothing else. The whole update
// (fetch, verify, stage, stop, swap, start, commit or revert) runs below the agent, in the frozen
// update unit and the fresh binary it pulls; the running agent's part is to hand a cloud
// directive's version to that unit. Its stop path is the update's safe point, with no code here.
//
// It is deliberately not keyring-gated: the keyring gates the unit's fetch, on bytes this agent
// never holds, which lets the timer path work on a node whose agent is wedged, the case the whole
// design exists for. The agent still decides to skip, as done, a directive naming the version it
// already runs (Current).
type hostSelfUpdater struct {
	layout  selfupdate.Layout
	update  string // the frozen update unit, started to run one
	version string
}

// newSelfUpdater builds the host selfUpdater from config. Never nil: with the fetch below the
// agent there is no keyring to be missing here.
func (cfg Config) newSelfUpdater() selfUpdater {
	return &hostSelfUpdater{
		layout:  selfupdate.New(cfg.UpdateBase, cfg.UpdateRunDir),
		update:  selfupdate.DefaultUpdateUnit,
		version: cfg.Version,
	}
}

// Trigger is the cloud trigger: the target as a message and the frozen unit started, without
// waiting for it: the run it starts stops this agent, and the stop waits for the act that would
// be waiting on the run.
func (h *hostSelfUpdater) Trigger(ctx context.Context, version string) (string, error) {
	return h.layout.Trigger(ctx, h.update, version, false)
}

func (h *hostSelfUpdater) Current() string { return h.version }

var _ selfUpdater = (*hostSelfUpdater)(nil)
