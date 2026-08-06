// Package selfupdate holds the on-disk half of the host-agent self-update mechanism
// : a flat two-binary layout the frozen systemd wrappers act on.
//
//	<base>/briard-agent       the COMMITTED binary — what ExecStart runs (mutable state, seeded
//	                          on install, since a /nix/store path can't be overwritten in place)
//	<base>/briard-agent.next  a staged candidate on the SAME filesystem, so the commit the
//	                          briard-commit wrapper does is a single atomic rename(2)
//	<run>/update              tmpfs flag: "an update is armed — trial briard-agent.next this boot"
//	<run>/trial               tmpfs marker: "this boot IS a trial — commit briard-agent.next on success"
//
// The commit and revert are dumb, frozen, agent-INDEPENDENT shell wrappers
// (briard-exec / briard-commit) + systemd `Type=notify` — so a bug in the volatile agent
// can never wedge the update mechanism. This package is therefore small: it owns only what
// the *proven* side does — atomically STAGE agent.next (write + fsync, so it is durable
// before any commit) and ARM the trial flag. It deliberately does NOT implement commit or
// revert (those are the wrappers), keeping the pivot out of Go.
package selfupdate

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// DefaultBase is the agent's state dir (alongside assignment.json etc., host.ConfigFromEnv);
// the committed binary and its staged candidate both live here so a commit is a same-fs rename.
const DefaultBase = "/var/lib/briard"

// DefaultRunDir is the tmpfs dir holding the ephemeral trial flags — cleared on every boot,
// which is what makes a power loss mid-trial revert to the committed binary for free.
const DefaultRunDir = "/run/briard"

const (
	agentName  = "briard-agent"
	nextName   = "briard-agent.next"
	updateFlag = "update"
	trialFlag  = "trial"
)

// Layout resolves the self-update paths under a state base + a tmpfs run dir.
type Layout struct {
	Base   string // committed binary + staged candidate live here (same filesystem)
	RunDir string // tmpfs; the trial/commit decision flags live here
}

// New returns a Layout, defaulting empty fields to DefaultBase / DefaultRunDir.
func New(base, runDir string) Layout {
	if base == "" {
		base = DefaultBase
	}
	if runDir == "" {
		runDir = DefaultRunDir
	}
	return Layout{Base: base, RunDir: runDir}
}

// AgentPath is the committed binary the systemd unit runs by default (via briard-exec).
func (l Layout) AgentPath() string { return filepath.Join(l.Base, agentName) }

// NextPath is the staged candidate — same directory as AgentPath, so committing it is an
// atomic rename(2) (briard-commit does the rename; this package only writes the file).
func (l Layout) NextPath() string { return filepath.Join(l.Base, nextName) }

// UpdateFlagPath / TrialMarkerPath are the tmpfs decision flags (see the package doc).
func (l Layout) UpdateFlagPath() string  { return filepath.Join(l.RunDir, updateFlag) }
func (l Layout) TrialMarkerPath() string { return filepath.Join(l.RunDir, trialFlag) }

// StageNext atomically writes the candidate binary to agent.next (temp file in the same
// directory, fsync, rename), mode 0755 — durable on disk BEFORE it can ever be committed, so
// the briard-commit rename is safe against a power loss. It does NOT arm the
// trial: a staged agent.next is inert until Arm() + a restart. Idempotent (overwrites).
func (l Layout) StageNext(src io.Reader) error {
	dst := l.NextPath()
	tmp := dst + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, src); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil { // durable before the rename makes it the candidate
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst) // atomic within Base
}

// NextStaged reports whether a candidate is staged (agent.next exists as a regular file).
func (l Layout) NextStaged() bool {
	fi, err := os.Stat(l.NextPath())
	return err == nil && fi.Mode().IsRegular()
}

// Arm writes the update flag so the next start trials agent.next (briard-exec renames it to
// the trial marker, single-use). It refuses to arm without a staged candidate — arming an
// absent agent.next would trial a missing binary and needlessly bounce the agent.
func (l Layout) Arm() error {
	if !l.NextStaged() {
		return fmt.Errorf("selfupdate: arm: no staged agent.next to trial")
	}
	if err := os.MkdirAll(l.RunDir, 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(l.UpdateFlagPath(), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	return f.Close()
}

// Armed reports whether an update trial is armed (the update flag is present).
func (l Layout) Armed() bool {
	_, err := os.Stat(l.UpdateFlagPath())
	return err == nil
}
