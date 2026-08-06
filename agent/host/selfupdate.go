package host

import (
	"context"
	"encoding/base64"
	"fmt"
	"os/exec"

	"briard.io/agent/selfupdate"
	"briard.io/shared/api"
)

// hostSelfUpdater is the production selfUpdater: it fetches + verifies + arms a signed
// agent binary via the selfupdate package, and restarts the systemd unit -- DETACHED -- to trial
// the staged binary through the Type=notify pivot. Constructed only when a release keyring
// is provisioned; otherwise self-update is off and an agent-update directive refuses.
type hostSelfUpdater struct {
	fetcher *selfupdate.Fetcher
	layout  selfupdate.Layout
	unit    string
	version string
	restart func(ctx context.Context, unit string) error // overridable in tests
}

// NewSelfUpdater builds the host selfUpdater from config, or returns nil (self-update off) when
// no keyring is provisioned or the keyring fails to parse -- fail closed, never fetch-and-trust.
func (cfg Config) newSelfUpdater(logf func(string, ...any)) selfUpdater {
	if len(cfg.UpdateKeyring) == 0 {
		return nil // no trusted keys -> self-update off
	}
	kr, err := selfupdate.NewKeyring(cfg.UpdateKeyring)
	if err != nil {
		logf("self-update disabled: bad release keyring: %v", err)
		return nil
	}
	if kr.Len() == 0 {
		logf("self-update disabled: release keyring has no keys")
		return nil
	}
	layout := selfupdate.New(cfg.UpdateBase, cfg.UpdateRunDir)
	unit := cfg.UpdateUnit
	if unit == "" {
		unit = "briard-agent.service"
	}
	return &hostSelfUpdater{
		fetcher: &selfupdate.Fetcher{Layout: layout, Keyring: kr, Logf: logf},
		layout:  layout,
		unit:    unit,
		version: cfg.Version,
		restart: systemdRestart,
	}
}

// Stage decodes the base64 signature and fetches+verifies+arms the offered artifact. A bad
// base64 sig is a refusal like any other (never stages).
func (h *hostSelfUpdater) Stage(ctx context.Context, u api.AgentUpdate) error {
	sig, err := base64.StdEncoding.DecodeString(u.Sig)
	if err != nil {
		return fmt.Errorf("selfupdate: bad base64 signature: %w", err)
	}
	return h.fetcher.FetchAndStage(ctx, u.URL, sig)
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
