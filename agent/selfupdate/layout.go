// Package selfupdate holds the on-disk half of the host-agent self-update mechanism
// : a flat two-binary layout the frozen systemd wrappers act on.
//
//	<base>/briard-agent        the COMMITTED binary — what ExecStart runs (mutable state, seeded
//	                           on install, since a /nix/store path can't be overwritten in place)
//	<base>/briard-agent.next   a staged candidate on the SAME filesystem, so the commit the
//	                           briard-commit wrapper does is a single atomic rename(2)
//	<base>/manifest.json       the signed manifest of the committed release — what an update
//	                           compares the channel against ([B.86a])
//	<base>/manifest.json.next  the candidate's manifest, committed beside it by the same wrapper
//	<run>/update               tmpfs flag: "an update is armed — trial briard-agent.next this boot";
//	                           its mtime is armed-at, which the frozen update unit reads
//	<run>/trial                tmpfs marker: "this boot IS a trial — commit briard-agent.next on success"
//	<run>/update-target        a MESSAGE to the update unit: which release to converge to
//	<run>/update-result        a MESSAGE back: the one line the unit's run ended on
//
// The commit and revert are dumb, frozen, agent-INDEPENDENT shell wrappers
// (briard-exec / briard-commit) + systemd `Type=notify` — so a bug in the volatile agent
// can never wedge the update mechanism. The FETCH is likewise below the agent ([B.86a]): a
// frozen oneshot (briard-update.service) pulls a fresh agent from the channel and lets THAT
// binary do the verified fetch, so a fetch bug in the running agent cannot prevent its own
// replacement. This package is therefore small: it owns only what the *proven* side does —
// atomically STAGE agent.next (write + fsync, so it is durable before any commit), ARM the
// trial, and pass the two messages. It deliberately does NOT implement commit or revert
// (those are the wrappers), keeping the pivot out of Go.
//
// The two messages are written by one side and read-and-unlinked by the other, so nothing
// accumulates a lifecycle: a target that was consumed is gone, a result that was read is gone.
package selfupdate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// DefaultBase is the agent's state dir (alongside assignment.json etc., host.ConfigFromEnv);
// the committed binary and its staged candidate both live here so a commit is a same-fs rename.
const DefaultBase = "/var/lib/briard"

// DefaultRunDir is the tmpfs dir holding the ephemeral trial flags — cleared on every boot,
// which is what makes a power loss mid-trial revert to the committed binary for free.
const DefaultRunDir = "/run/briard"

// DefaultUpdateUnit is the frozen oneshot that fetches below the agent; every trigger — the
// cloud's directive, the timer, `briard update host` — is a `systemctl start` of it, and systemd
// merging a start into a running job is what makes the unit its own mutual exclusion.
const DefaultUpdateUnit = "briard-update.service"

const (
	agentName    = "briard-agent"
	nextName     = "briard-agent.next"
	manifestName = "manifest.json"
	nextManifest = "manifest.json.next"
	updateFlag   = "update"
	trialFlag    = "trial"
	targetName   = "update-target"
	resultName   = "update-result"
)

// Layout resolves the self-update paths under a state base + a tmpfs run dir.
type Layout struct {
	Base   string // committed binary + staged candidate live here (same filesystem)
	RunDir string // tmpfs; the trial/commit decision flags and the two messages live here
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

// ManifestPath / NextManifestPath are the committed release's signed manifest and the staged
// candidate's, committed together by briard-commit (existence-guarded, so a node installed
// before manifests were kept simply has none until its first update).
func (l Layout) ManifestPath() string     { return filepath.Join(l.Base, manifestName) }
func (l Layout) NextManifestPath() string { return filepath.Join(l.Base, nextManifest) }

// UpdateFlagPath / TrialMarkerPath are the tmpfs decision flags (see the package doc).
func (l Layout) UpdateFlagPath() string  { return filepath.Join(l.RunDir, updateFlag) }
func (l Layout) TrialMarkerPath() string { return filepath.Join(l.RunDir, trialFlag) }

// TargetPath / ResultPath are the two messages between a trigger and the update unit.
func (l Layout) TargetPath() string { return filepath.Join(l.RunDir, targetName) }
func (l Layout) ResultPath() string { return filepath.Join(l.RunDir, resultName) }

// StageNext atomically writes the candidate binary to agent.next (temp file in the same
// directory, fsync, rename), mode 0755 — durable on disk BEFORE it can ever be committed, so
// the briard-commit rename is safe against a power loss. It does NOT arm the
// trial: a staged agent.next is inert until Arm() + a restart. Idempotent (overwrites).
//
// The temp name is unique per call rather than a fixed `.tmp`: with the fetch now run by a
// unit rather than the agent, two writers are at least conceivable (a directive and a timer
// tick merging late), and a fixed name would let them tear each other's candidate.
func (l Layout) StageNext(src io.Reader) error {
	return atomicWrite(l.NextPath(), 0o755, func(f *os.File) error {
		_, err := io.Copy(f, src)
		return err
	})
}

// StageNextManifest writes the candidate's signed manifest beside it, the same way — so the
// commit moves both and the node's idea of "which release am I" stays with the binary it runs.
func (l Layout) StageNextManifest(manifest []byte) error {
	return atomicWrite(l.NextManifestPath(), 0o644, func(f *os.File) error {
		_, err := f.Write(manifest)
		return err
	})
}

func atomicWrite(dst string, mode os.FileMode, fill func(*os.File) error) error {
	f, err := os.CreateTemp(filepath.Dir(dst), filepath.Base(dst)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	fail := func(err error) error {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := fill(f); err != nil {
		return fail(err)
	}
	if err := f.Chmod(mode); err != nil {
		return fail(err)
	}
	if err := f.Sync(); err != nil { // durable before the rename makes it the candidate
		return fail(err)
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

// ArmedSince returns when the trial was armed (the flag's mtime) and whether one is armed at
// all. The frozen update unit reads the same mtime with stat(1); this is the Go side of that
// one contract.
func (l Layout) ArmedSince() (time.Time, bool) {
	fi, err := os.Stat(l.UpdateFlagPath())
	if err != nil {
		return time.Time{}, false
	}
	return fi.ModTime(), true
}

// WriteTarget leaves the release the next update run should converge to: `stable`, `latest`,
// or an exact host id. One line, read and unlinked by the unit — a message, not state. It
// replaces whatever target was waiting, since the unit runs once per start and the newest
// intent is the one that should win.
func (l Layout) WriteTarget(target string) error {
	if target == "" || strings.ContainsAny(target, "/\n\r \t") {
		return fmt.Errorf("selfupdate: bad update target %q", target)
	}
	if err := os.MkdirAll(l.RunDir, 0o755); err != nil {
		return err
	}
	return atomicWrite(l.TargetPath(), 0o644, func(f *os.File) error {
		_, err := f.WriteString(target + "\n")
		return err
	})
}

// TakeResult reads and unlinks the unit's result line; "" when there is none.
func (l Layout) TakeResult() string {
	b, err := os.ReadFile(l.ResultPath())
	if err != nil {
		return ""
	}
	os.Remove(l.ResultPath())
	return strings.TrimSpace(string(b))
}

// Trigger runs one update check against target and returns the line the unit ended on: it
// writes the target message, starts the frozen unit and BLOCKS on it (`systemctl start` waits
// for a oneshot), then takes the result. A non-zero unit exit surfaces as an error carrying
// that same line, so a bad pin looks like one — the outcome the cloud sees is the unit's own
// verdict, and the CLI prints the same words.
//
// It is deliberately NOT an injector over the admin socket: the case the timer and the CLI
// exist for is the agent being down, and the cloud's handler uses this same path so there is
// one way to start an update, not two.
func (l Layout) Trigger(ctx context.Context, unit, target string) (string, error) {
	if unit == "" {
		unit = DefaultUpdateUnit
	}
	if err := l.WriteTarget(target); err != nil {
		return "", err
	}
	cmd := exec.CommandContext(ctx, "systemctl", "start", unit)
	out, runErr := cmd.CombinedOutput()
	result := l.TakeResult()
	if runErr != nil {
		if result == "" {
			result = strings.TrimSpace(string(out))
		}
		if result == "" {
			result = runErr.Error()
		}
		return result, errors.New(result)
	}
	if result == "" {
		result = "update run finished without a result line (see journalctl -u " + unit + ")"
	}
	return result, nil
}
