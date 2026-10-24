package install

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"briard.io/agent/selfupdate"
	"briard.io/shared/atomicfile"
)

// THE UPDATE VERB. `briard-agent --update <target>` is the whole self-update, run by the frozen
// update unit on a FRESH binary it just pulled from the target's pointer, never on the committed
// one, so a fetch/verify/manifest bug in the running agent cannot prevent its own replacement
// (install.sh's bootstrap pattern, on a timer). One process, in order: reconcile a run that died
// mid-swap, decide against the installed manifest, fetch and verify, stage, smoke-test a staged
// qemu, stop the agent through its ordinary stop path, swap, start and watch it reach READY, then
// commit -- or revert and leave the reason for the agent that comes back. The running agent
// carries no update logic: its stop path is the safe point.
//
// Scope is the BRIARD BUNDLE: the agent, the guest launch shim, the qemu tree and the guest
// bundle move as one release, staged as .next siblings and swapped together. Only entries whose
// sha256 differs from the installed manifest's are fetched -- a JSON compare, no hashing of the
// 86 MB qemu tree -- which is what keeps a daily tick at the agent's ~9 MB rather than the
// bundle's ~28 MB, and is safe by construction: an identical hash is an identical artifact, so a
// skipped qemu is still the pairing the release was tested with. A staged qemu is smoke-tested
// by THIS binary -- the release's own agent -- before anything is stopped, so a release that does
// not run on this host is refused at no cost, and the node never lands on agent N with qemu N-1.

// The artifacts of the briard bundle, by the names the manifest uses; the agent's and the qemu
// bundle's are per OS (update_other.go, update_windows.go). Anything else a briard manifest
// carries -- an installer, a driver -- is left to whoever installs it.
const (
	artifactNetWrap = "briard-net-wrap"      // Linux only: the Windows arm ships none
	artifactGuest   = "guest-bundle.tar.zst" // the binaries the host dresses its guest with
)

// ErrBelowFloor is returned for an exact pin older than the current stable (or one whose floor
// cannot be read): refused loudly, so a bad pin looks like one to whoever sent it.
var ErrBelowFloor = errors.New("install: pinned release is below the stable floor")

// ErrTooOldToUpgrade is the UPGRADE FLOOR's refusal: the installed release is older
// than the oldest this one can be installed over, so the remedy is a reinstall rather than
// another update. Loud, and naming the remedy, because the alpha's answer to an unupgradable
// node is exactly that (it is reinstall-only) and a node that cannot say so is a node
// whose owner discovers it from the symptom instead.
var ErrTooOldToUpgrade = errors.New("install: this node is too old to upgrade to that release")

// MinUpgradeFrom is THE FLOOR THIS TREE DECLARES: the oldest installed release the release built
// from this tree can be installed OVER. Empty means no floor -- any installed release may
// upgrade to it, which is the normal state and should stay the normal state.
//
// ⚠️ IT IS A CONSTANT IN THE TREE, NOT A FLAG, and that is the point. The floor is a fact about
// THE CODE -- "this release stopped being able to upgrade a node older than X" -- so it belongs
// in the commit that makes it true, reviewable in the diff, and not in an operator's memory at
// publish time. `--stage-manifest --chain briard` reads it from here, so there is nothing to pass
// and nothing to forget. Every other manifest fact the pipeline is TOLD; this one it IS.
//
// WHEN TO MOVE IT: when this release can no longer complete an upgrade from some older release
// -- an on-disk format this tree no longer reads, a migration deleted because the floor had
// already passed it, a boundary in the guest image a pushed binary cannot cross. Not for a
// behaviour change, not for a bug fix, and never as tidying: raising it tells every node below
// it to reinstall, which in this product means a household's machine comes apart and goes back
// together.
//
// ⚠️ THE FLOOR AND THE PUBLISH GATE INTERACT, and the interaction is not optional. Gate 3
// (lab/vanilla-linux/tests/upgrade.sh) drives `stable` -> the candidate; a floor
// ABOVE the current stable makes that refusal correct and the gate red for a true reason. So a
// release that raises the floor past stable has DELIBERATELY CLOSED its own upgrade path, and
// the publish sequence has to record the refusal as the declared outcome rather than as a
// finding. Decide that before raising it, not while reading a red gate.
//
// THE SECOND THING IT BUYS, which may be the larger one: a migration becomes deletable. Code
// that exists to read an old on-disk shape can go once the floor is past the release that wrote
// it -- and without a floor there is no moment at which removing it is provably safe, so shims
// accumulate forever.
//
// ⚠️ A var, not a const, for ONE reason: the wiring that carries it into a manifest cannot be
// tested against a value that never varies, and a test that cannot fail is not a test.
// Nothing at runtime writes it -- the only writer
// besides this line is TestWriteManifestCarriesTheTreesFloor, which restores it.
var MinUpgradeFrom = "v3.20261004.5ee3525"

// Decision is what Decide concluded: whether to install, and the one line saying why either way.
type Decision struct {
	Install bool
	Reason  string
}

// Decide applies the comparison rules to a target's manifest (want) against the installed one
// (have; nil when the node keeps none yet) and, for an exact pin, the current stable (the floor).
//
//   - `stable`: install when date(have) < date(want). Ordering on the DATE FIELD ALONE, numerically:
//     the epoch token only ever moves forward with the dates, and dropping it removes the trap
//     where `v3` sorts before `v10`. No same-date rule, by decision — one would make the timer
//     revert a cloud pin that shares a date with stable; the constraint sits in the publish path
//     instead (promote refuses a same-date build).
//   - `latest` / an exact id: install when the full id differs. These force past the ordering
//     but still no-op at equality, or `briard update` would bounce an up-to-date agent.
//   - An exact id may be OLDER than the installed one (a bad release must be revocable without
//     reinstalling every home) but NEVER older than stable: unbounded, a buggy or compromised
//     agent could name any old signed release; floored, the worst it reaches is a build we
//     currently vouch for. To go below stable, move stable. A pin below the floor fails loudly.
//
// The chain/platform precondition is cheap and load-bearing: a crossed wire between the briard and
// vm chains would otherwise compare a vm date against a briard date and silently no-op.
func Decide(target string, want Manifest, have, stable *Manifest) (Decision, error) {
	if have != nil && (have.Chain != want.Chain || have.Platform != want.Platform) {
		return Decision{}, fmt.Errorf("%w: installed %s, offered %s", ErrWrongChain,
			path.Join(have.Chain, have.Platform), path.Join(want.Chain, want.Platform))
	}
	// THE UPGRADE FLOOR, BEFORE ANY TARGET RULE. It is a fact about the pair
	// (installed, offered) rather than about the target word, so it gates `stable`, `latest` and
	// an exact pin alike -- there is no target that may cross it, because the release simply
	// cannot complete the upgrade. A node with nothing installed is a fresh install and has
	// nothing to be too old for.
	//
	// ⚠️ THE ONE DIRECTION A FLOOR MAY POINT (the floor's one rule): outer-to-inner, with the older
	// SELF as the inner term. A release may refuse the past it cannot carry; it may never declare
	// a minimum on a layer it is itself responsible for upgrading -- if the host needs a newer
	// guest, the host upgrades the guest, it does not wait for one. That keeps the graph a DAG
	// and is why this check reads `have` and nothing else.
	if have != nil && want.MinUpgradeFrom != "" {
		floorDate, err := dateOf(want.MinUpgradeFrom)
		if err != nil {
			return Decision{}, fmt.Errorf("%w: %s declares min_upgrade_from %q, which has no date to compare against",
				ErrManifest, want.Version, want.MinUpgradeFrom)
		}
		haveDate, err := dateOf(have.Version)
		if err != nil {
			return Decision{}, fmt.Errorf("%w: installed release id %q has no date to compare min_upgrade_from %s against",
				ErrTooOldToUpgrade, have.Version, want.MinUpgradeFrom)
		}
		// On the DATE, like every other ordering in this channel (owner, 2026-09-20). It
		// inherits the same-day blind spot `min_briard` has, and for the same reason it is
		// tolerable: `promote` refuses a same-date build, so `stable` cannot cross a floor
		// twice in one day.
		if haveDate < floorDate {
			return Decision{}, fmt.Errorf("%w: installed %s is older than %s's min_upgrade_from %s — this node cannot be upgraded to it and must be reinstalled",
				ErrTooOldToUpgrade, have.Version, want.Version, want.MinUpgradeFrom)
		}
	}
	wantDate, err := dateOf(want.Version)
	if err != nil {
		return Decision{}, err
	}
	switch target {
	case TargetStable:
		if have == nil {
			return Decision{Install: true, Reason: "no installed manifest — taking stable " + want.Version}, nil
		}
		haveDate, err := dateOf(have.Version)
		if err != nil {
			return Decision{}, err
		}
		if haveDate < wantDate {
			return Decision{Install: true, Reason: fmt.Sprintf("stable moved to %s (installed %s)", want.Version, have.Version)}, nil
		}
		// ⚠️ "AT OR PAST" IS FOR THE CASE IT DESCRIBES, AND EQUALITY IS NOT IT. A node sitting on
		// the release stable names falls through to the shared `already at X` line below, which
		// is what `briard update` promises to print (agent/cli/cli.go's help row) and
		// what an operator reads as "nothing owed". Found by the rigs the moment the default became
		// `stable`: the bare verb started taking this branch instead of latest's, and
		// two host-agent rigs asserting `already at <id>` went red on the wording alone.
		// The longer line stays for what it actually means -- installed is genuinely PAST stable,
		// which is a pin, and saying so is the point.
		if have.Version != want.Version {
			return Decision{Reason: fmt.Sprintf("installed %s is past stable %s; nothing to do", have.Version, want.Version)}, nil
		}
	case TargetLatest, TargetDev:
		// latest is by construction never older than stable, so no floor to check. dev is cut
		// from HEAD and asked for by name, by a test node: it goes wherever dev points, floor or no.
	default:
		if want.Version != target {
			return Decision{}, fmt.Errorf("%w: asked for %s, manifest there names %s", ErrManifest, target, want.Version)
		}
		if stable == nil {
			return Decision{}, fmt.Errorf("%w: cannot pin %s — no stable to floor against", ErrBelowFloor, target)
		}
		stableDate, err := dateOf(stable.Version)
		if err != nil {
			return Decision{}, err
		}
		if wantDate < stableDate {
			return Decision{}, fmt.Errorf("%w: %s is older than stable %s — move stable to go there", ErrBelowFloor, target, stable.Version)
		}
	}
	if have != nil && have.Version == want.Version {
		return Decision{Reason: fmt.Sprintf("already at %s; nothing to do", want.Version)}, nil
	}
	from := "no installed manifest"
	if have != nil {
		from = "installed " + have.Version
	}
	return Decision{Install: true, Reason: fmt.Sprintf("%s -> %s (%s)", target, want.Version, from)}, nil
}

// dateOf is the numeric date field of a release id (`v3.20260905.abc1234` -> 20260905), the
// only part of an id the stable path orders on.
func dateOf(id string) (int64, error) {
	parts := strings.Split(id, ".")
	if len(parts) < 2 {
		return 0, fmt.Errorf("%w: release id %q has no date field", ErrManifest, id)
	}
	n, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || len(parts[1]) != 8 {
		return 0, fmt.Errorf("%w: release id %q has a non-numeric date field", ErrManifest, id)
	}
	return n, nil
}

// StopPatience is how long the updater waits for the agent to finish an act in flight before the
// stop is forced: an install's whole budget, with room, and well inside the update unit's own
// TimeoutStartSec.
const StopPatience = 30 * time.Minute

// Update is one run of the verb. The three service verbs are the OS's (the agent's main wires
// them: systemd's on Linux, the service manager's and the warden's on Windows), handed in so the
// procedure reads and tests the same on every system.
type Update struct {
	Fetcher *Fetcher
	Layout  selfupdate.Layout
	Logf    func(string, ...any)
	// Stop stops the agent through its ordinary stop path, forcing it after patience (0: at once).
	Stop func(ctx context.Context, patience time.Duration) error
	// Start starts the agent and returns once it reported READY, or with why it did not.
	Start func(ctx context.Context) error
	// Smoke proves a staged qemu tree runs on this host.
	Smoke func(ctx context.Context, tree string) error
}

// Run returns the one line the run ended on — "already at …" or "updated to …" — and an error
// for a refusal (bad pin, bad signature, tampered artifact, a qemu that does not run here), in
// which case nothing was swapped (refuse-and-stay), or for a candidate that did not start, in
// which case the committed release is back in place and running.
func (u *Update) Run(ctx context.Context, target string) (string, error) {
	logf := u.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if !validSegment(target) {
		return "", fmt.Errorf("install: bad release target %q", target)
	}
	if err := u.reconcile(ctx, logf); err != nil {
		return "", err
	}
	want, wantBytes, err := u.Fetcher.fetchManifest(ctx, target)
	if err != nil {
		return "", err
	}
	have := u.installed(logf)
	var stable *Manifest
	if !IsPointer(target) {
		s, _, err := u.Fetcher.fetchManifest(ctx, TargetStable)
		if err != nil {
			return "", fmt.Errorf("%w: cannot pin %s — reading stable failed: %v", ErrBelowFloor, target, err)
		}
		stable = &s
	}
	d, err := Decide(target, want, have, stable)
	if err != nil {
		return "", err
	}
	if !d.Install {
		return d.Reason, nil
	}
	logf("update: %s", d.Reason)

	// FETCH EVERYTHING FIRST, STAGE NOTHING UNTIL IT ALL VERIFIED. Into a private temp dir on
	// the same filesystem as the candidates; a refusal anywhere (a tampered net-wrap, a qemu
	// tarball whose hash disagrees) returns before any .next exists, so refuse-and-stay holds
	// for the bundle as it did for the agent alone. The one thing that may outlive a refused
	// run is an extracted qemu TREE, and that is deliberate: it is verified bytes under a
	// name no link points at, inert until a run stages a link to it.
	tmp, err := os.MkdirTemp(u.Layout.Base, ".update-")
	if err != nil {
		return "", fmt.Errorf("install: update temp dir: %w", err)
	}
	defer os.RemoveAll(tmp)
	from := path.Join(u.Fetcher.Chain, want.Version, u.Fetcher.Platform)
	var agent, netWrap *Entry
	guestTree := ""
	qemuTree := ""
	for i := range want.Artifacts {
		a := &want.Artifacts[i]
		switch a.Name {
		case artifactAgent:
			// Never hash-skipped: the candidate is what the swap starts, its id is baked into
			// it (so on a real release the skip could not fire anyway), and its .prev is the trial marker.
			if err := u.Fetcher.fetchArtifact(ctx, from, tmp, *a); err != nil {
				return "", err
			}
			agent = a
		case artifactNetWrap:
			if unchanged(have, *a) {
				logf("update: %s unchanged since %s; not fetched", a.Name, have.Version)
				continue
			}
			if err := u.Fetcher.fetchArtifact(ctx, from, tmp, *a); err != nil {
				return "", err
			}
			netWrap = a
		case artifactQEMU:
			if unchanged(have, *a) {
				logf("update: %s unchanged since %s; not fetched", a.Name, have.Version)
				continue
			}
			tree := u.Layout.QEMUTree(want.Version)
			if committed, ok := u.Layout.CommittedQEMUTree(); ok && committed == tree {
				// No installed manifest to compare, but the link already names this release's
				// tree (a node whose manifest was lost): nothing to stage for qemu.
				logf("update: qemu tree %s is already the committed one", filepath.Base(tree))
				continue
			}
			if _, err := os.Stat(tree); err == nil {
				// Left by an earlier run of this same release (a reverted one, most likely):
				// complete by construction, since a tree appears only by rename.
				logf("update: reusing the extracted qemu tree %s", filepath.Base(tree))
			} else {
				if err := u.Fetcher.fetchArtifact(ctx, from, tmp, *a); err != nil {
					return "", err
				}
				// fetchArtifact expanded the verified .zst to the bare tarball beside it.
				if err := extractTree(ctx, filepath.Join(tmp, strings.TrimSuffix(a.Name, compressedSuffix)), tree); err != nil {
					return "", err
				}
			}
			qemuTree = tree
		case artifactGuest:
			// The guest bundle rides exactly qemu's mechanics: one extracted tree per
			// release, a `.next` link the swap moves, hash-skipped when unchanged.
			if unchanged(have, *a) {
				logf("update: %s unchanged since %s; not fetched", a.Name, have.Version)
				continue
			}
			tree := u.Layout.GuestTree(want.Version)
			if committed, ok := u.Layout.CommittedGuestTree(); ok && committed == tree {
				logf("update: guest tree %s is already the committed one", filepath.Base(tree))
				continue
			}
			if _, err := os.Stat(tree); err == nil {
				logf("update: reusing the extracted guest tree %s", filepath.Base(tree))
			} else {
				if err := u.Fetcher.fetchArtifact(ctx, from, tmp, *a); err != nil {
					return "", err
				}
				if err := extractTree(ctx, filepath.Join(tmp, strings.TrimSuffix(a.Name, compressedSuffix)), tree); err != nil {
					return "", err
				}
			}
			guestTree = tree
		default:
			logf("update: %s is not part of the briard bundle this agent knows; left alone", a.Name)
		}
	}
	if agent == nil {
		return "", fmt.Errorf("%w: release %s ships no %s", ErrManifest, want.Version, artifactAgent)
	}

	// STAGE. Whatever an earlier run staged of the bundle goes first, so the only .next
	// siblings the swap can find are this release's; then each verified file lands whole
	// (write + fsync + rename) beside the one it replaces.
	if err := u.Layout.DiscardNextBundle(); err != nil {
		return "", fmt.Errorf("install: discard stale candidates: %w", err)
	}
	staged := []string{"agent"}
	if err := stageFile(filepath.Join(tmp, agent.Name), u.Layout.StageNext); err != nil {
		return "", fmt.Errorf("install: stage candidate: %w", err)
	}
	if netWrap != nil {
		if err := stageFile(filepath.Join(tmp, netWrap.Name), u.Layout.StageNextNetWrap); err != nil {
			return "", fmt.Errorf("install: stage net-wrap: %w", err)
		}
		staged = append(staged, "net-wrap")
	}
	if qemuTree != "" {
		if err := u.Layout.StageNextQEMU(qemuTree); err != nil {
			return "", fmt.Errorf("install: stage qemu: %w", err)
		}
		staged = append(staged, "qemu")
	}
	if guestTree != "" {
		if err := u.Layout.StageNextGuest(guestTree); err != nil {
			return "", fmt.Errorf("install: stage guest bundle: %w", err)
		}
		staged = append(staged, "guest")
	}
	if err := u.Layout.StageNextManifest(wantBytes); err != nil {
		return "", fmt.Errorf("install: stage manifest: %w", err)
	}
	logf("update: staged %s (%s)", want.Version, strings.Join(staged, ", "))

	// THE QEMU QUESTION, ANSWERED BEFORE ANYTHING STOPS. This binary is the release's own agent,
	// so the pairing proven is agent N with qemu N, and a refusal
	// costs the household nothing: the staged set goes, nothing was touched. The question is not
	// "is this qemu good" but "does release N work on this host" (a missing lib, an old glibc, an
	// absent device model), and no is a reason to refuse the release.
	if qemuTree != "" {
		logf("update: smoke-testing the staged qemu %s", filepath.Base(qemuTree))
		if err := u.Smoke(ctx, qemuTree); err != nil {
			u.discard(logf)
			return "", fmt.Errorf("host bundle %s refused on this host: %w", want.Version, err)
		}
	}

	// THE STOP IS THE SAFE POINT. The agent's ordinary stop path refuses new acts, finishes the one
	// in flight and exits; the patience is for that act, and only past it is the stop forced.
	logf("update: stopping the agent (it finishes any act in flight first)")
	if err := u.Stop(ctx, StopPatience); err != nil {
		u.discard(logf)
		if serr := u.Start(ctx); serr != nil {
			logf("update: the agent did not come back after a failed stop: %v", serr)
		}
		return "", fmt.Errorf("install: stop the agent: %w", err)
	}
	moved, err := u.Layout.Swap()
	if err == nil {
		logf("update: swapped in %s (%s); starting it", want.Version, strings.Join(moved, ", "))
		err = u.Start(ctx)
	}
	if err != nil {
		return "", u.revert(ctx, want.Version, err, logf)
	}
	if err := u.Layout.Commit(); err != nil {
		// The new release is running and READY; what failed is deleting what it displaced. A
		// .prev left behind is finished by the next run's reconcile (Commit is idempotent).
		logf("update: %s is running, but the commit did not finish (%v); the next run finishes it", want.Version, err)
	}
	return fmt.Sprintf("updated to %s (%s)", want.Version, strings.Join(staged, ", ")), nil
}

// revert puts the committed release back after a candidate that did not start (or a swap that
// failed part-way), starts it, and leaves the reason for that agent to escalate: the run has no
// notifier, and the agent that comes back is the one that can still report.
func (u *Update) revert(ctx context.Context, version string, cause error, logf func(string, ...any)) error {
	logf("update: %s did not start (%v); reverting", version, cause)
	if err := u.Stop(ctx, 0); err != nil {
		logf("update: stopping the candidate: %v", err)
	}
	back, err := u.Layout.Revert()
	if err != nil {
		return fmt.Errorf("install: %s did not start (%v), and the revert failed after putting back %v: %w", version, cause, back, err)
	}
	u.discard(logf)
	if werr := u.Layout.WriteFailure(version, cause.Error()); werr != nil {
		logf("update: could not leave the failure for the agent (%v)", werr)
	}
	if err := u.Start(ctx); err != nil {
		return fmt.Errorf("install: %s did not start (%v); reverted, but the previous agent did not start either: %w", version, cause, err)
	}
	return fmt.Errorf("host bundle %s did not start on this host and was reverted: %w", version, cause)
}

// reconcile settles what an earlier run left mid-procedure, before this run decides anything. The
// agent's .prev means that run stopped between its swap and its verdict -- a power cut, a reboot,
// a killed unit -- so its candidate is reverted (Layout.Interrupted). Other .prev without the
// agent's are the tail of a commit, finished here. Stale .next go at staging.
func (u *Update) reconcile(ctx context.Context, logf func(string, ...any)) error {
	if !u.Layout.Interrupted() {
		return u.Layout.Commit()
	}
	logf("update: an earlier run stopped between its swap and its verdict; reverting it")
	if err := u.Stop(ctx, StopPatience); err != nil {
		return fmt.Errorf("install: stop the agent to revert an interrupted update: %w", err)
	}
	back, err := u.Layout.Revert()
	if err != nil {
		return fmt.Errorf("install: revert an interrupted update (put back %v): %w", back, err)
	}
	u.discard(logf)
	logf("update: put back %s; starting the agent", strings.Join(back, ", "))
	if err := u.Start(ctx); err != nil {
		return fmt.Errorf("install: reverted an interrupted update, but the agent did not start: %w", err)
	}
	return nil
}

// discard drops every staged .next, logging rather than failing: a .next left behind is inert,
// and the next run's staging drops it anyway.
func (u *Update) discard(logf func(string, ...any)) {
	if err := u.Layout.DiscardNextBundle(); err != nil {
		logf("update: could not discard the staged set (%v); the next run does", err)
	}
}

// unchanged reports whether the installed manifest pins a.Name at the same sha256 the target
// does -- the whole of the "download only what changed" rule. No installed manifest, or none
// naming this artifact, means fetch (the safe default).
func unchanged(have *Manifest, a Entry) bool {
	if have == nil {
		return false
	}
	for _, h := range have.Artifacts {
		if h.Name == a.Name {
			return h.SHA256 == a.SHA256
		}
	}
	return false
}

func stageFile(src string, stage func(io.Reader) error) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	return stage(f)
}

// extractTree unpacks a verified qemu tarball into dest, which appears whole or not at all
// (unpacked into a sibling temp dir, renamed into place). tar(1) rather than archive/tar: the
// install unpacks this same tarball with it, and every host we install on has one (Windows
// ships tar.exe) -- one way to open the bundle, not two.
func extractTree(ctx context.Context, tarball, dest string) error {
	tmp, err := os.MkdirTemp(filepath.Dir(dest), ".update-tree-")
	if err != nil {
		return fmt.Errorf("install: qemu tree temp dir: %w", err)
	}
	if out, err := exec.CommandContext(ctx, "tar", "-xf", tarball, "-C", tmp).CombinedOutput(); err != nil {
		os.RemoveAll(tmp)
		return fmt.Errorf("install: unpack %s: %w: %s", filepath.Base(tarball), err, strings.TrimSpace(string(out)))
	}
	if err := os.Chmod(tmp, 0o755); err != nil { // MkdirTemp makes it 0700; a tree is an ordinary directory
		os.RemoveAll(tmp)
		return err
	}
	// Flushed before the rename, and the rename flushed after: otherwise the name can
	// reach the disk before the bytes, and a power cut leaves a whole-looking tree of lost files
	// that the next commit makes current.
	if err := atomicfile.SyncTree(tmp); err != nil {
		os.RemoveAll(tmp)
		return fmt.Errorf("install: flush qemu tree: %w", err)
	}
	if err := os.Rename(tmp, dest); err != nil {
		os.RemoveAll(tmp)
		return fmt.Errorf("install: place qemu tree: %w", err)
	}
	return atomicfile.SyncDir(filepath.Dir(dest))
}

// installed reads the committed release's manifest; nil (and a log line) when absent or
// unreadable, which Decide treats as "install" — the safe default.
func (u *Update) installed(logf func(string, ...any)) *Manifest {
	b, err := os.ReadFile(u.Layout.ManifestPath())
	if err != nil {
		logf("update: no installed manifest at %s (%v)", u.Layout.ManifestPath(), err)
		return nil
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil || m.Version == "" {
		logf("update: installed manifest at %s unreadable (%v)", u.Layout.ManifestPath(), err)
		return nil
	}
	return &m
}

// DirectiveUpdateVM is the LOCAL directive kind of the vm chain: `briard update --vm`
// and the agent's own nightly timer submit it through the admin door; the payload is a target
// (`stable`, `latest`, an exact vm id; "" is stable). It is deliberately NOT in
// shared/api: the cloud names closures (`upgrade-system`) and the wire allowlist stays closed --
// a kind that never crosses to the cloud does not belong in the contract that says what can.
// It lives here rather than in agent/host so the CLI and the host share one spelling.
//
// ⚠️ IT WAS `update-guest` ONCE, and the rename was safe for the reason the comment
// above already gives: nothing carries this kind across a version boundary. The cloud never
// emits it, the CLI reaches a binary it is symlinked to, the timer is in-process, and no spool
// persists a kind across an upgrade. What DID read the old spelling was the journal -- the fleet
// tests wait on these lines by text -- and every one of those waits fails by timeout rather than
// passing vacuously, which is what made the rename cheap to prove.
const DirectiveUpdateVM = "update-vm"

// ErrBriardTooOld is the min_briard refusal: the installed briard predates what a vm release
// tolerates.
var ErrBriardTooOld = errors.New("install: briard is older than the vm release requires")

// BriardSatisfies applies a vm release's min_briard to the installed briard release id: nil when
// briard is at or past it (ordered on the date field, as the stable path is), ErrBriardTooOld
// otherwise. An empty minBriard places no requirement. The message names both remedies, because
// a briard that cannot update past the floor is a node outside its support window, and the
// answer there is reinstall -- it must never drift silently. One vocabulary end to end:
// the wire field, the thing compared and the remedy all say `briard`.
func BriardSatisfies(minBriard, briard string) error {
	if minBriard == "" {
		return nil
	}
	need, err := dateOf(minBriard)
	if err != nil {
		return err
	}
	have, err := dateOf(briard)
	if err != nil {
		return fmt.Errorf("%w: the installed briard release id %q has no date to compare min_briard %s against", ErrBriardTooOld, briard, minBriard)
	}
	if have < need {
		return fmt.Errorf("%w: briard %s < min_briard %s — run `briard update` first; a briard that can no longer update is outside its support window and must be reinstalled", ErrBriardTooOld, briard, minBriard)
	}
	return nil
}
