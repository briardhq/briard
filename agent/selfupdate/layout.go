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
//	<base>/briard-net-wrap     the guest launch shim, cattle that rides with the agent ([B.86b])
//	<base>/briard-net-wrap.next
//	<base>/qemu                a SYMLINK to the committed qemu tree, qemu-<release>/ beside it —
//	                           so committing qemu is one rename(2) of a link, never a recursive
//	                           delete near a frozen script, and N-1 stays on disk for free
//	<base>/qemu.next           the staged link, committed by the same wrapper with `mv -T`
//	<base>/qemu-<release>/     one extracted qemu bundle per release (bin/ lib/ share/)
//	<run>/update               tmpfs flag: "an update is armed — trial briard-agent.next this boot";
//	                           its mtime is armed-at, which the frozen update unit reads
//	<run>/trial                tmpfs marker: "this boot IS a trial — commit briard-agent.next on success"
//	<run>/update-target        a MESSAGE to the update unit: which release to converge to
//	<run>/update-result        a MESSAGE back: the one line the unit's run ended on
//	<run>/update-failure       a MESSAGE from a trial that refused its own release (the staged
//	                           qemu failed its smoke test) to the agent that comes back after it
//
// The commit and revert are dumb, frozen, agent-INDEPENDENT shell wrappers
// (briard-exec / briard-commit) + systemd `Type=notify` — so a bug in the volatile agent
// can never wedge the update mechanism. The FETCH is likewise below the agent ([B.86a]): a
// frozen oneshot (briard-update.service) pulls a fresh agent from the channel and lets THAT
// binary do the verified fetch, so a fetch bug in the running agent cannot prevent its own
// replacement. This package is therefore small: it owns only what the *proven* side does —
// atomically STAGE the candidate bundle (write + fsync, so it is durable before any commit),
// ARM the trial, and pass the messages. It deliberately does NOT implement commit or revert
// (those are the wrappers), keeping the pivot out of Go.
//
// The bundle moves as ONE release ([B.86b]): whatever of {agent, manifest, net-wrap, qemu} a
// release changed is staged as `.next` siblings, and briard-commit commits every `.next` it
// finds, each existence-guarded — a partial set is the normal case (an unchanged artifact is
// not re-fetched), not an error. All-or-nothing is the point: a trial that fails leaves every
// `.next` staged and inert, so the node lands back on a combination that WAS tested, never on
// agent N with qemu N-1.
//
// The messages are written by one side and read-and-unlinked by the other, so nothing
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
	failureName  = "update-failure"
	netWrapName  = "briard-net-wrap"
	nextNetWrap  = "briard-net-wrap.next"
	qemuLink     = "qemu"
	nextQEMULink = "qemu.next"
	qemuTree     = "qemu-" // + release id: one extracted bundle per release, beside the link
	// The guest bundle ([B.86j]) -- the binaries the host dresses its guest with -- rides the
	// same mechanics: one extracted tree per release, a link the frozen commit moves with -T.
	guestLink     = "guest"
	nextGuestLink = "guest.next"
	guestTree     = "guest-"
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

// TargetPath / ResultPath are the two messages between a trigger and the update unit;
// FailurePath is the one from a trial back to the agent that replaces it.
func (l Layout) TargetPath() string  { return filepath.Join(l.RunDir, targetName) }
func (l Layout) ResultPath() string  { return filepath.Join(l.RunDir, resultName) }
func (l Layout) FailurePath() string { return filepath.Join(l.RunDir, failureName) }

// NetWrapPath / NextNetWrapPath are the guest launch shim and its staged candidate. The shim's
// contract is with the AGENT (the fd-passing argv both sides agree on), so it must never
// straddle a release boundary: it commits with the agent, in the same rename burst.
func (l Layout) NetWrapPath() string     { return filepath.Join(l.Base, netWrapName) }
func (l Layout) NextNetWrapPath() string { return filepath.Join(l.Base, nextNetWrap) }

// QEMUPath / NextQEMUPath are the committed and staged LINKS to a qemu tree; QEMUTree is where
// the tree for one release lives. The link is what the agent's QEMU/QEMU_DATADIR point through
// (install.sh keeps the public /opt/briard/qemu path as a link onto QEMUPath, because the
// bundle bakes that prefix into its ELF interpreter), so the commit is one rename of a link.
func (l Layout) QEMUPath() string               { return filepath.Join(l.Base, qemuLink) }
func (l Layout) NextQEMUPath() string           { return filepath.Join(l.Base, nextQEMULink) }
func (l Layout) QEMUTree(release string) string { return filepath.Join(l.Base, qemuTree+release) }

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

// StageNextNetWrap writes the candidate launch shim beside the agent's, the same way.
func (l Layout) StageNextNetWrap(src io.Reader) error {
	return atomicWrite(l.NextNetWrapPath(), 0o755, func(f *os.File) error {
		_, err := io.Copy(f, src)
		return err
	})
}

// StageNextQEMU points qemu.next at an extracted tree under Base (tmp link + rename, so the
// staged link is never half-written). The target is RELATIVE — the tree's basename — so the
// layout stays addressable from Base alone. It does not check the tree's contents: that is the
// trial's smoke test, run by the candidate agent before it sends READY.
func (l Layout) StageNextQEMU(tree string) error {
	return l.stageTreeLink(tree, qemuTree, l.NextQEMUPath(), nextQEMULink)
}

// stageTreeLink points `link` at an extracted `<prefix><release>` tree under Base, atomically:
// the link is made in a temp dir and renamed over any existing one. Shared by the qemu and the
// guest bundle ([B.86j]); the frozen commit moves each `.next` link with `mv -T`.
func (l Layout) stageTreeLink(tree, prefix, link, linkName string) error {
	if filepath.Dir(tree) != l.Base || !strings.HasPrefix(filepath.Base(tree), prefix) {
		return fmt.Errorf("selfupdate: tree %s is not a %s* directory under %s", tree, prefix, l.Base)
	}
	if fi, err := os.Stat(tree); err != nil || !fi.IsDir() {
		return fmt.Errorf("selfupdate: tree %s is not a directory (%v)", tree, err)
	}
	tmp, err := os.MkdirTemp(l.Base, linkName+".*.tmp")
	if err != nil {
		return err
	}
	staged := filepath.Join(tmp, "link")
	if err := os.Symlink(filepath.Base(tree), staged); err != nil {
		os.RemoveAll(tmp)
		return err
	}
	if err := os.Rename(staged, link); err != nil { // replaces an existing link atomically
		os.RemoveAll(tmp)
		return err
	}
	return os.Remove(tmp)
}

// The guest bundle's tree and links ([B.86j]): the same shape as qemu's, and the same commit --
// briard-commit renames guest.next onto guest with -T after READY.
func (l Layout) GuestPath() string                  { return filepath.Join(l.Base, guestLink) }
func (l Layout) NextGuestPath() string              { return filepath.Join(l.Base, nextGuestLink) }
func (l Layout) GuestTree(release string) string    { return filepath.Join(l.Base, guestTree+release) }
func (l Layout) CommittedGuestTree() (string, bool) { return l.qemuTarget(l.GuestPath()) }
func (l Layout) NextGuestTree() (string, bool)      { return l.qemuTarget(l.NextGuestPath()) }
func (l Layout) StageNextGuest(tree string) error {
	return l.stageTreeLink(tree, guestTree, l.NextGuestPath(), nextGuestLink)
}

// CommittedGuestRelease is the release id the committed guest tree carries in its name --
// what the guest must report as its Bundle once dressed. "" when no tree is committed (an
// install that predates the bundle, or a candidate not yet committed).
func (l Layout) CommittedGuestRelease() string {
	t, ok := l.CommittedGuestTree()
	if !ok {
		return ""
	}
	return strings.TrimPrefix(filepath.Base(t), guestTree)
}

// PruneGuestTrees removes guest-<release> trees no link names -- committed, staged, or the last
// GOOD one, which a refused release falls back to for as long as it is refused.
func (l Layout) PruneGuestTrees() ([]string, error) {
	return l.pruneTrees(guestTree, l.CommittedGuestTree, l.NextGuestTree, func() (string, bool) { return l.qemuTarget(l.GoodGuestPath()) })
}

// THE PERMANENT REVERT ([B.86j], owner 2026-09-07). The guest's own pivot falls back only until
// its next launch (a fresh overlay knows nothing), so the host remembers two things beside the
// committed tree: which tree the guest last ran SUCCESSFULLY (`guest.good`, a link like the
// others, so its tree survives pruning) and which release it REFUSED (`guest.reverted`, an id).
// A refused release is never pushed again; the good tree is, on every launch, until a new host
// commit brings a different tree -- at which point the refusal no longer names the committed
// release and is simply ignored.
const (
	goodGuestLink = "guest.good"
	guestReverted = "guest.reverted"
)

func (l Layout) GoodGuestPath() string     { return filepath.Join(l.Base, goodGuestLink) }
func (l Layout) GuestRevertedPath() string { return filepath.Join(l.Base, guestReverted) }

// GoodGuestRelease is the release of the last tree the guest ran after a push, "" if none yet.
func (l Layout) GoodGuestRelease() string {
	t, ok := l.qemuTarget(l.GoodGuestPath())
	if !ok {
		return ""
	}
	return strings.TrimPrefix(filepath.Base(t), guestTree)
}

// MarkGuestGood records `release`'s tree as the last one the guest ran: a link, replaced
// atomically, so a later refusal has a tree to fall back to and the pruner keeps it.
func (l Layout) MarkGuestGood(release string) error {
	return l.stageTreeLink(l.GuestTree(release), guestTree, l.GoodGuestPath(), goodGuestLink)
}

// MarkGuestReverted records that the guest refused `release`'s bundle. Durable (Base, not the
// run dir): the refusal must outlive the guest's launches, which is the whole point.
func (l Layout) MarkGuestReverted(release string) error {
	return atomicWrite(l.GuestRevertedPath(), 0o644, func(f *os.File) error {
		_, err := f.WriteString(release + "\n")
		return err
	})
}

// GuestReverted is the refused release, if one is recorded.
func (l Layout) GuestReverted() (string, bool) {
	b, err := os.ReadFile(l.GuestRevertedPath())
	if err != nil {
		return "", false
	}
	r := strings.TrimSpace(string(b))
	return r, r != ""
}

// NextQEMUStaged reports whether a qemu link is staged. The candidate agent runs its smoke test
// only when this is true AND the boot is a trial — a staged-and-refused qemu stays on disk
// inert, and must not make every later start of the committed agent pay (or fail) the test.
func (l Layout) NextQEMUStaged() bool {
	_, err := os.Lstat(l.NextQEMUPath())
	return err == nil
}

// CommittedQEMUTree / NextQEMUTree resolve the two links to absolute tree paths; ok is false
// when the link is absent.
func (l Layout) CommittedQEMUTree() (string, bool) { return l.qemuTarget(l.QEMUPath()) }
func (l Layout) NextQEMUTree() (string, bool)      { return l.qemuTarget(l.NextQEMUPath()) }

func (l Layout) qemuTarget(link string) (string, bool) {
	t, err := os.Readlink(link)
	if err != nil {
		return "", false
	}
	if !filepath.IsAbs(t) {
		t = filepath.Join(l.Base, t)
	}
	return filepath.Clean(t), true
}

// DiscardNextBundle removes a staged net-wrap and qemu link left by an earlier run — a trial
// that failed, or a release the node never got round to trialling. The update verb calls it
// before staging a release, so a release that did NOT change qemu is never paired with a
// stale qemu.next from one that did: the commit moves every .next it finds, and the only
// .next that may exist are the ones THIS release staged. The agent and manifest need no such
// step; every run re-stages both.
func (l Layout) DiscardNextBundle() error {
	for _, p := range []string{l.NextNetWrapPath(), l.NextQEMUPath(), l.NextGuestPath()} {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

// PruneQEMUTrees removes every qemu-<release> tree under Base that neither the committed nor
// the staged link points at, and returns what it removed. Called at guest LAUNCH, and only
// there: that is the one moment no qemu of ours is running from any tree — a guest that keeps
// serving across an agent update still runs the binary it was launched from, and a tree a live
// process runs from must not be pulled out from under it. Keeping N-1 is therefore free; it is
// N-2 and older this collects.
func (l Layout) PruneQEMUTrees() ([]string, error) {
	return l.pruneTrees(qemuTree, l.CommittedQEMUTree, l.NextQEMUTree)
}

// pruneTrees removes every `<prefix>*` directory under Base that no given link names.
func (l Layout) pruneTrees(prefix string, links ...func() (string, bool)) ([]string, error) {
	keep := map[string]bool{}
	for _, link := range links {
		if t, ok := link(); ok {
			keep[t] = true
		}
	}
	ents, err := os.ReadDir(l.Base)
	if err != nil {
		return nil, err
	}
	var removed []string
	for _, e := range ents {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), prefix) {
			continue
		}
		p := filepath.Join(l.Base, e.Name())
		if keep[p] {
			continue
		}
		if err := os.RemoveAll(p); err != nil {
			return removed, err
		}
		removed = append(removed, e.Name())
	}
	return removed, nil
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

// InTrial reports whether THIS boot is a trial: briard-exec renamed the update flag to the
// trial marker before exec'ing the candidate, and briard-commit removes it only after READY.
// It is what scopes the qemu smoke test to the candidate's own start.
func (l Layout) InTrial() bool {
	_, err := os.Stat(l.TrialMarkerPath())
	return err == nil
}

// WriteFailure is the trial's last word before it exits without READY: which release refused
// itself and why. The agent that comes back after the revert takes it and escalates — the
// trial cannot, because failing the start is the whole mechanism and nothing of it survives.
func (l Layout) WriteFailure(release, reason string) error {
	if err := os.MkdirAll(l.RunDir, 0o755); err != nil {
		return err
	}
	return atomicWrite(l.FailurePath(), 0o644, func(f *os.File) error {
		_, err := fmt.Fprintf(f, "%s %s\n", release, strings.ReplaceAll(reason, "\n", " "))
		return err
	})
}

// TakeFailure reads and unlinks a trial's failure message; ok is false when there is none.
func (l Layout) TakeFailure() (release, reason string, ok bool) {
	b, err := os.ReadFile(l.FailurePath())
	if err != nil {
		return "", "", false
	}
	os.Remove(l.FailurePath())
	release, reason, _ = strings.Cut(strings.TrimSpace(string(b)), " ")
	return release, reason, release != ""
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
