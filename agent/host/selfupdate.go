package host

import (
	"context"

	"briard.io/agent/selfupdate"
)

// hostSelfUpdater is the production selfUpdater: a TRIGGER and nothing else. The whole update
// -- fetch, verify, stage, stop, swap, start, commit or revert -- runs below the agent, in the
// frozen update unit and the fresh binary it pulls, so the running agent's part is to hand a cloud
// directive's version to that unit. Its stop path is the update's safe point, and it needs no
// code here to be one.
//
// It is NOT KEYRING-GATED: the keyring gates the unit's fetch, on bytes this agent never holds.
// That is what lets the timer path work on a node whose agent is wedged, which is the case the
// whole design exists for. What the agent can still refuse is a directive naming a version it is
// already running.
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

// Trigger is the cloud trigger: the target as a message and the frozen unit started, WITHOUT
// waiting for it -- the run it starts stops this agent, and the stop waits for the act that would
// be waiting on the run.
func (h *hostSelfUpdater) Trigger(ctx context.Context, version string) (string, error) {
	return h.layout.Trigger(ctx, h.update, version, false)
}

func (h *hostSelfUpdater) Current() string { return h.version }

var _ selfUpdater = (*hostSelfUpdater)(nil)
