// Package selfupdate holds the on-disk half of the host-agent self-update: a flat layout the
// updater moves by rename.
//
//	<base>/briard-agent        the COMMITTED binary -- what the service runs (mutable state, seeded
//	                           on install, since a /nix/store path can't be overwritten in place)
//	<base>/manifest.json       the signed manifest of the committed release -- what an update
//	                           compares the channel against
//	<base>/briard-net-wrap     the guest launch shim, cattle that rides with the agent
//	<base>/qemu                a SYMLINK to the committed qemu tree, qemu-<release>/ beside it --
//	                           so moving qemu is a rename of a link, never a recursive delete, and
//	                           N-1 stays on disk for free
//	<base>/guest               the same, for the guest bundle (guest-<release>/)
//	<base>/<entry>.next        fetched and verified, not yet in place
//	<base>/<entry>.prev        displaced by a swap, not yet committed
//	<run>/update-target        a MESSAGE to the update run: which release to converge to
//	<run>/update-result        the last run's verdict: ok or failed, then its one line
//	<run>/update-failure       a MESSAGE from a reverted run to the agent that comes back after it
//
// ONE PROCESS DOES THE WHOLE UPDATE: the candidate binary's --update, run by the frozen update
// unit from a fresh download (agent/install.Update). It stages each changed entry as <entry>.next,
// stops the agent, SWAPS (the committed entry to .prev, .next onto the committed name), starts the
// agent and watches it reach READY, then COMMITS by deleting every .prev -- or REVERTS by renaming
// each .prev back. Names never change, so every path that reaches the agent by name stays valid;
// every move is a rename on one filesystem, so a torn entry cannot exist.
//
// THE AGENT'S .prev IS THE TRIAL MARKER. Every run stages the agent, so a swap moves it first and a
// commit deletes its .prev first, while a revert puts it back last: "the agent's .prev exists"
// therefore means exactly "a run stopped between its swap and its verdict", and the next run
// reverts it (Interrupted). One rule, no flag, no state file.
//
// The bundle moves as ONE release: whatever of {agent, manifest, net-wrap, qemu, guest} a release
// changed is staged, and a partial set is the normal case (an unchanged artifact is not
// re-fetched). A revert puts every displaced entry back, so the node lands on a combination that
// WAS tested, never on agent N with qemu N-1.
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

	"briard.io/shared/atomicfile"
)

// DefaultBase is the agent's state dir (alongside assignment.json etc., host.ConfigFromEnv);
// the committed binary and its staged candidate both live here so a commit is a same-fs rename.
const DefaultBase = "/var/lib/briard"

// DefaultRunDir is the tmpfs dir the update messages pass through. Nothing in it decides a swap:
// that state is the .prev entries under Base, which a power cut cannot clear.
const DefaultRunDir = "/run/briard"

// DefaultUpdateUnit is the frozen oneshot that runs the update below the agent; every trigger —
// the cloud's directive, the timer, `briard update` — is a `systemctl start` of it, and systemd
// merging a start into a running job is what makes the unit its own mutual exclusion.
const DefaultUpdateUnit = "briard-update.service"

// DefaultAgentUnit is the service the updater stops, swaps under and starts again.
const DefaultAgentUnit = "briard-agent.service"

const (
	agentName    = "briard-agent"
	nextName     = "briard-agent.next"
	manifestName = "manifest.json"
	nextManifest = "manifest.json.next"
	targetName   = "update-target"
	resultName   = "update-result"
	failureName  = "update-failure"
	netWrapName  = "briard-net-wrap"
	nextNetWrap  = "briard-net-wrap.next"
	qemuLink     = "qemu"
	nextQEMULink = "qemu.next"
	qemuTree     = "qemu-" // + release id: one extracted bundle per release, beside the link
	// The guest bundle -- the binaries the host dresses its guest with -- rides the
	// same mechanics: one extracted tree per release, a link the swap moves.
	guestLink     = "guest"
	nextGuestLink = "guest.next"
	guestTree     = "guest-"
	nextSuffix    = ".next"
	prevSuffix    = ".prev"
)

// entries are the names a swap moves, in swap order. The agent goes first: every run stages it,
// so its .prev is on disk before any other entry moves, which is what makes it the trial marker.
var entries = []string{agentName, manifestName, netWrapName, qemuLink, guestLink}

// Layout resolves the self-update paths under a state base + a tmpfs run dir.
type Layout struct {
	Base   string // the committed entries, their .next and .prev (one filesystem)
	RunDir string // tmpfs; the update messages live here
}

// New returns a Layout, defaulting empty fields to DefaultBase / DefaultRunDir.
func New(base, runDir string) Layout {
	if base == "" {
		base = DefaultBase
	}
	if runDir == "" {
		runDir = DefaultRunDir
	}
	return Layout{Base: filepath.Clean(base), RunDir: filepath.Clean(runDir)}
}

// AgentPath is the committed binary the service runs.
func (l Layout) AgentPath() string { return filepath.Join(l.Base, agentName) }

// NextPath is the staged candidate — same directory as AgentPath, so the swap is a rename(2).
func (l Layout) NextPath() string { return filepath.Join(l.Base, nextName) }

// PrevPath is the committed agent a swap displaced: present only between a swap and its verdict.
func (l Layout) PrevPath() string { return filepath.Join(l.Base, agentName+prevSuffix) }

// ManifestPath / NextManifestPath are the committed release's signed manifest and the staged
// candidate's, swapped together with the agent (existence-guarded, so a node installed before
// manifests were kept simply has none until its first update).
func (l Layout) ManifestPath() string     { return filepath.Join(l.Base, manifestName) }
func (l Layout) NextManifestPath() string { return filepath.Join(l.Base, nextManifest) }

// TargetPath / ResultPath are the two messages between a trigger and the update unit;
// FailurePath is the one from a reverted run to the agent that comes back after it.
func (l Layout) TargetPath() string  { return filepath.Join(l.RunDir, targetName) }
func (l Layout) ResultPath() string  { return filepath.Join(l.RunDir, resultName) }
func (l Layout) FailurePath() string { return filepath.Join(l.RunDir, failureName) }

// NetWrapPath / NextNetWrapPath are the guest launch shim and its staged candidate. The shim's
// contract is with the AGENT (the fd-passing argv both sides agree on), so it must never
// straddle a release boundary: it is swapped with the agent, in the same rename burst.
func (l Layout) NetWrapPath() string     { return filepath.Join(l.Base, netWrapName) }
func (l Layout) NextNetWrapPath() string { return filepath.Join(l.Base, nextNetWrap) }

// QEMUPath / NextQEMUPath are the committed and staged LINKS to a qemu tree; QEMUTree is where
// the tree for one release lives. The link is what the agent's QEMU/QEMU_DATADIR point through
// (install.sh keeps the public /opt/briard/qemu path as a link onto QEMUPath, because the
// bundle bakes that prefix into its ELF interpreter), so moving it is a rename of a link.
func (l Layout) QEMUPath() string               { return filepath.Join(l.Base, qemuLink) }
func (l Layout) NextQEMUPath() string           { return filepath.Join(l.Base, nextQEMULink) }
func (l Layout) QEMUTree(release string) string { return filepath.Join(l.Base, qemuTree+release) }

// StageNext atomically writes the candidate binary to agent.next (temp file in the same
// directory, fsync, rename), mode 0755 — durable on disk BEFORE it can ever be swapped in, so
// the swap is safe against a power loss. Idempotent (overwrites).
//
// The temp name is unique per call rather than a fixed `.tmp`: with the fetch run by a
// unit rather than the agent, two writers are at least conceivable (a directive and a timer
// tick merging late), and a fixed name would let them tear each other's candidate.
func (l Layout) StageNext(src io.Reader) error {
	return atomicWrite(l.NextPath(), 0o755, func(f *os.File) error {
		_, err := io.Copy(f, src)
		return err
	})
}

// StageNextManifest writes the candidate's signed manifest beside it, the same way — so the
// swap moves both and the node's idea of "which release am I" stays with the binary it runs.
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
// updater's smoke test, run before anything is swapped.
func (l Layout) StageNextQEMU(tree string) error {
	return l.stageTreeLink(tree, qemuTree, l.NextQEMUPath(), nextQEMULink)
}

// stageTreeLink points `link` at an extracted `<prefix><release>` tree under Base, atomically:
// the link is made in a temp dir and renamed over any existing one. Shared by the qemu and the
// guest bundle.
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

// The guest bundle's tree and links: the same shape as qemu's, swapped the same way.
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
	return l.pruneTrees(guestTree, l.CommittedGuestTree, l.NextGuestTree, func() (string, bool) {
		r := l.GoodGuestRelease()
		return l.GuestTree(r), r != ""
	})
}

// THE PERMANENT REVERT. The guest's own pivot falls back only until
// its next launch (a fresh scratch knows nothing), so the host remembers two release ids beside
// the committed tree: which tree the guest last ran SUCCESSFULLY (`guest.good`, which also keeps
// that tree from the pruner) and which release it REFUSED (`guest.reverted`). A refused release is
// never pushed again; the good tree is, on every launch, until a new host commit brings a different
// tree -- at which point the refusal no longer names the committed release and is simply ignored.
//
// Ids in files, not links: nothing ever opens the good tree THROUGH its name, only asks which it
// is, and on Windows a link cannot be atomically renamed over another directory link, nor opened
// at all as the file link a relative target made from a temp dir becomes.
const (
	goodGuest     = "guest.good"
	guestReverted = "guest.reverted"
)

func (l Layout) GoodGuestPath() string     { return filepath.Join(l.Base, goodGuest) }
func (l Layout) GuestRevertedPath() string { return filepath.Join(l.Base, guestReverted) }

// GoodGuestRelease is the release of the last tree the guest ran after a push, "" if none yet.
func (l Layout) GoodGuestRelease() string {
	b, err := os.ReadFile(l.GoodGuestPath())
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// MarkGuestGood records `release`'s tree as the last one the guest ran, replaced atomically, so a
// later refusal has a tree to fall back to and the pruner keeps it. The tree must be there.
func (l Layout) MarkGuestGood(release string) error {
	tree := l.GuestTree(release)
	if fi, err := os.Stat(tree); err != nil || !fi.IsDir() {
		return fmt.Errorf("selfupdate: tree %s is not a directory (%v)", tree, err)
	}
	return atomicWrite(l.GoodGuestPath(), 0o644, func(f *os.File) error {
		_, err := f.WriteString(release + "\n")
		return err
	})
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

// DiscardNextBundle removes every staged .next -- a run that stopped before its swap, or a
// release refused at stage time. The update verb calls it before staging, so a release that did
// NOT change qemu is never paired with a stale qemu.next from one that did: the swap moves every
// .next it finds, and the only .next that may exist are the ones THIS release staged. A staged
// tree is left where it is: only its link goes.
func (l Layout) DiscardNextBundle() error {
	for _, e := range entries {
		if err := os.Remove(filepath.Join(l.Base, e+nextSuffix)); err != nil && !os.IsNotExist(err) {
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

func present(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

// Swap puts every staged entry in place: the committed entry renamed to .prev, the .next renamed
// onto its name. It returns the entries it moved. The agent moves first and the directory is
// flushed after it, so on disk the agent's .prev exists before any other entry has moved: a run
// that dies from here on is found by Interrupted and reverted whole. Refused without a staged
// agent, which every run stages -- a swap without one would have no trial marker.
func (l Layout) Swap() ([]string, error) {
	if !l.NextStaged() {
		return nil, fmt.Errorf("selfupdate: swap: no staged %s", nextName)
	}
	var moved []string
	for _, e := range entries {
		cur := filepath.Join(l.Base, e)
		next := cur + nextSuffix
		if !present(next) {
			continue
		}
		if present(cur) {
			if err := os.Rename(cur, cur+prevSuffix); err != nil {
				return moved, err
			}
		}
		if err := os.Rename(next, cur); err != nil {
			return moved, err
		}
		moved = append(moved, e)
		if e == agentName {
			if err := atomicfile.SyncDir(l.Base); err != nil {
				return moved, err
			}
		}
	}
	return moved, atomicfile.SyncDir(l.Base)
}

// Interrupted reports whether a run stopped between its swap and its verdict: the agent's .prev
// is on disk. The next run reverts it before anything else (agent/install.Update).
func (l Layout) Interrupted() bool { return present(l.PrevPath()) }

// Revert puts every displaced entry back over the candidate that replaced it, and returns the
// entries it restored. The agent goes back LAST, after a flush, so a revert cut short still
// reads as Interrupted and is simply repeated. A candidate entry with nothing displaced behind it
// (a name the committed release did not have) stays, inert.
func (l Layout) Revert() ([]string, error) {
	var back []string
	for i := len(entries) - 1; i >= 0; i-- {
		cur := filepath.Join(l.Base, entries[i])
		if !present(cur + prevSuffix) {
			continue
		}
		if entries[i] == agentName {
			if err := atomicfile.SyncDir(l.Base); err != nil {
				return back, err
			}
		}
		if err := os.Rename(cur+prevSuffix, cur); err != nil {
			return back, err
		}
		back = append(back, entries[i])
	}
	return back, atomicfile.SyncDir(l.Base)
}

// Commit makes the swap final by deleting every .prev. The agent's goes FIRST, flushed, so a
// commit cut short is never read as Interrupted: a .prev left without the agent's is the tail of
// a commit, and Commit finishes it. A displaced tree link goes; its tree stays until a guest
// launch prunes it.
func (l Layout) Commit() error {
	for i, e := range entries {
		if err := os.Remove(filepath.Join(l.Base, e+prevSuffix)); err != nil && !os.IsNotExist(err) {
			return err
		}
		if i == 0 {
			if err := atomicfile.SyncDir(l.Base); err != nil {
				return err
			}
		}
	}
	return atomicfile.SyncDir(l.Base)
}

// WriteFailure is a reverted run's last word: which release failed to start and why. The agent
// that comes back after the revert takes it and escalates -- the run itself has no notifier, and
// the agent is the one that can still report.
func (l Layout) WriteFailure(release, reason string) error {
	if err := os.MkdirAll(l.RunDir, 0o755); err != nil {
		return err
	}
	return atomicWrite(l.FailurePath(), 0o644, func(f *os.File) error {
		_, err := fmt.Fprintf(f, "%s %s\n", release, strings.ReplaceAll(reason, "\n", " "))
		return err
	})
}

// TakeFailure reads and unlinks a reverted run's failure message; ok is false when there is none.
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

// Result is the last update run's verdict, as the frozen update script left it: one line,
// `ok <line>` or `failed <line>`, written at every exit of the run.
type Result struct {
	OK   bool
	Line string
	At   time.Time // when the run ended (the file's mtime)
}

// ReadResult reads the last run's verdict WITHOUT consuming it: the CLI reads the run it waited
// on, and the update alerter reads whatever run came last, so neither may take it from the
// other. ok is false when no run has left one since boot (the run dir is tmpfs).
func (l Layout) ReadResult() (Result, bool) {
	fi, err := os.Stat(l.ResultPath())
	if err != nil {
		return Result{}, false
	}
	b, err := os.ReadFile(l.ResultPath())
	if err != nil {
		return Result{}, false
	}
	word, line, _ := strings.Cut(strings.TrimSpace(string(b)), " ")
	return Result{OK: word == "ok", Line: line, At: fi.ModTime()}, true
}

// Trigger starts one update run against target: it writes the target message and starts the
// frozen unit. With wait, it BLOCKS on the run (`systemctl start` waits for a oneshot) and
// returns the line that run ended on, a failed run surfacing as an error carrying that same
// line, so a bad pin looks like one. Without wait it returns once the run is queued: the
// agent's own trigger must not wait, because the run it starts stops that agent, and the stop
// waits for the act that is waiting on the run.
//
// It is deliberately NOT an injector over the admin socket: the case the timer and the CLI
// exist for is the agent being down, and the cloud's handler uses this same path so there is
// one way to start an update, not two.
func (l Layout) Trigger(ctx context.Context, unit, target string, wait bool) (string, error) {
	if unit == "" {
		unit = DefaultUpdateUnit
	}
	if err := l.WriteTarget(target); err != nil {
		return "", err
	}
	if !wait {
		if out, err := exec.CommandContext(ctx, "systemctl", "start", "--no-block", unit).CombinedOutput(); err != nil {
			return "", fmt.Errorf("start %s: %w (%s)", unit, err, strings.TrimSpace(string(out)))
		}
		return "update run started (journalctl -u " + unit + ")", nil
	}
	// A result older than this start is an earlier run's, never this one's. A second of slack
	// for a coarse clock: a run takes far longer than that to reach its verdict.
	started := time.Now().Add(-time.Second)
	out, runErr := exec.CommandContext(ctx, "systemctl", "start", unit).CombinedOutput()
	result := ""
	if r, ok := l.ReadResult(); ok && r.At.After(started) {
		result = r.Line
	}
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
