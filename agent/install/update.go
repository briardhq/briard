package install

import (
	"bytes"
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

// The update verb: `briard-agent --update <target>` is the whole self-update, one process. The
// frozen update unit runs it on a fresh binary pulled from the target's pointer, never the
// committed one, so a fetch/manifest bug in the running agent cannot prevent its own replacement
// (install.sh's bootstrap pattern, on a timer), and only once the committed agent's VerifyAgent
// passed it: the one thing the running agent does for its replacement, so a release proves its
// own copy of that check (the self-test) before it may become the committed agent. In order:
// reconcile a run that died mid-swap, defer if briard was stopped, decide against the installed
// manifest, fetch and verify, stage, smoke-test a staged qemu, self-test the candidate, lay the
// release's keyring, stop the agent through its ordinary stop path, swap, start and watch it
// reach READY, then commit, or revert and leave the reason for the agent that comes back. Beyond
// that check the running agent carries no update logic: its stop path is the safe point.
//
// Scope is the briard bundle: the agent, its manifest, the guest launch shim (the tray on
// Windows), the qemu tree and the guest bundle move as one release, staged as .next siblings and
// swapped together. Only entries whose sha256 differs from the installed manifest's are fetched
// (a JSON compare, no hashing the 86 MB qemu tree), so a daily tick costs the agent's ~9 MB, not
// the bundle's ~28 MB; safe by construction, as an identical hash is an identical artifact and a
// skipped qemu is still the tested pairing. This binary, the release's own agent, smoke-tests a
// staged qemu before anything stops: a release that does not run on this host is refused at no
// cost, and the node never lands on agent N with qemu N-1.

// The artifacts of the briard bundle, by the names the manifest uses; the agent's and the qemu
// bundle's are per OS (update_other.go, update_windows.go). Anything else a briard manifest
// carries -- an installer, a driver -- is left to whoever installs it.
const (
	artifactNetWrap = "briard-net-wrap"      // Linux only: the Windows arm ships none
	artifactTray    = "briard-tray.exe"      // Windows only: the notification-area icon
	artifactGuest   = "guest-bundle.tar.zst" // the binaries the host dresses its guest with
	artifactKeyring = "keyring.pem"          // the release keys this node trusts from now on
)

// ErrBelowFloor is returned for an exact pin older than the current stable (or one whose floor
// cannot be read): refused loudly, so a bad pin looks like one to whoever sent it.
var ErrBelowFloor = errors.New("install: pinned release is below the stable floor")

// ErrTooOldToUpgrade is the upgrade floor's refusal: the installed release is older than the
// oldest this one can be installed over, so the remedy is a reinstall rather than another update.
// Loud, and naming the remedy, so the owner learns it from the message rather than the symptom.
var ErrTooOldToUpgrade = errors.New("install: this machine is too old to upgrade to that release")

// MinUpgradeFrom is the floor this tree declares: the oldest installed release the release built
// from it can be installed over. Empty (any installed release may upgrade) is the normal state
// and should stay so.
//
// ⚠️ Deliberately in the tree, not a flag: it is a fact about the code ("this release stopped
// being able to upgrade a node older than X"), so it belongs in the commit that makes it true,
// reviewable in the diff, not in an operator's memory at publish time. `--stage-manifest --chain
// briard` reads it from here, nothing to pass or forget: every other manifest fact the pipeline
// is told; this one its own code holds.
//
// Move it only when this release can no longer complete an upgrade from some older one (an
// on-disk format it no longer reads, a migration deleted because the floor had passed it, a
// guest-image boundary a pushed binary cannot cross); never for a behaviour change, a bug fix or
// tidying, because every node below it must then reinstall, and a household's machine comes
// apart and goes back together. The second thing a floor buys, maybe the larger: without a floor
// there is no moment when deleting a migration (code reading an old on-disk shape) is provably
// safe, so shims accumulate forever; with one, it can go once the floor passes the release that
// wrote it.
//
// ⚠️ The floor and the publish gate interact, unavoidably: a floor above the current stable makes
// the upgrade gate's `stable` -> candidate refusal correct and the gate red for a true reason.
// Such a release has deliberately closed its own upgrade path, and the publish sequence must
// record the refusal as the declared outcome, not a finding. Decide that before raising it, not
// while reading a red gate.
//
// ⚠️ A var, not a const, for one reason only: the wiring into a manifest cannot be tested against
// a value that never varies, and a test that cannot fail is not a test. Nothing at runtime writes
// it; the only other writer is TestWriteManifestCarriesTheTreesFloor, which restores it.
var MinUpgradeFrom = "v3.26111720.5c45882"

// Decision is what Decide concluded: whether to install, and the one line saying why either way.
type Decision struct {
	Install bool
	Reason  string
}

// Decide applies the comparison rules to a target's manifest (want) against the installed one
// (have; nil when the node keeps none yet) and, for an exact pin, the current stable (the floor).
//
//   - `stable`: install when date(have) < date(want), numerically on the date field alone (the
//     epoch token only ever moves forward with the dates, and dropping it removes the trap where
//     `v3` sorts before `v10`). No same-date rule, by decision: a same-date rule would make the
//     timer revert a cloud pin sharing stable's date; the publish path holds the constraint
//     instead (promote refuses a build whose date field equals stable's).
//   - `latest` / an exact id: install when the full id differs, forcing past the ordering but
//     still a no-op at equality, or `briard update` would bounce an up-to-date agent.
//   - An exact id may be older than the installed one (a bad release must be revocable without
//     reinstalling every home) but never older than stable: unbounded, a buggy or compromised
//     agent could name any old signed release; floored, the worst it reaches is a build we
//     currently vouch for. To go below stable, move stable; a pin below the floor fails loudly.
//
// The chain/platform precondition is cheap and load-bearing: a crossed briard/vm wire would
// otherwise compare a vm date against a briard date and silently no-op.
func Decide(target string, want Manifest, have, stable *Manifest) (Decision, error) {
	if have != nil && (have.Chain != want.Chain || have.Platform != want.Platform) {
		return Decision{}, fmt.Errorf("%w: installed %s, offered %s", ErrWrongChain,
			path.Join(have.Chain, have.Platform), path.Join(want.Chain, want.Platform))
	}
	// The upgrade floor comes before any target rule: a fact about the pair (installed, offered),
	// not the target word, it gates `stable`, `latest` and an exact pin alike, and no target may
	// cross it because the release simply cannot complete the upgrade. A node with nothing
	// installed is a fresh install, with nothing to be too old for.
	//
	// ⚠️ The floor's one rule: it points outer-to-inner, with the older self as the inner term. A
	// release may refuse the past it cannot carry, never declare a minimum on a layer it is itself
	// responsible for upgrading (a host needing a newer guest upgrades the guest rather than
	// waiting for one). That keeps the graph a DAG and is why this check reads `have` and nothing
	// else.
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
		// On the date, like every other ordering in this channel. It inherits the blind spot
		// `min_briard` has within one date field, tolerable for the same reason: `promote` refuses
		// a build whose date field equals stable's, so `stable` cannot cross a floor twice in one
		// hour.
		if haveDate < floorDate {
			return Decision{}, fmt.Errorf("%w: installed %s is older than %s's min_upgrade_from %s — this machine cannot be upgraded to it and must be reinstalled",
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
		// ⚠️ "Past stable" is not "at stable". A node on the release stable names falls through to
		// the shared `already at X` line below: what `briard update` promises to print
		// (agent/cli/cli.go's help row), what an operator reads as "nothing owed", and what
		// host-agent rigs assert by text. This line is only for installed genuinely past stable,
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

// dateOf is the numeric date field of a release id (`v3.26090514.abc1234` -> 26090514), the
// only part of an id the stable path orders on. Two forms, one order: the commit's YYMMDDHH, and
// the YYYYMMDD older releases carry, every one of which is below every YYMMDDHH (20xxxxxx vs
// 26xxxxxx and up), so comparing the numbers orders both by time with no form to tell apart.
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

// Update is one run of the verb. The three verbs on the agent's OS service are the OS's (the
// agent's main wires them: systemd's on Linux, the service manager's and the warden's on
// Windows), handed in so the procedure reads and tests the same on every system.
type Update struct {
	Fetcher *Fetcher
	Layout  selfupdate.Layout
	// Keyring is the path of the node's release keyring, the file a release's own keyring.pem
	// replaces.
	Keyring string
	Logf    func(string, ...any)
	// Stop stops the agent through its ordinary stop path, forcing it after patience (0: at once).
	Stop func(ctx context.Context, patience time.Duration) error
	// Start starts the agent and returns once it reported READY, or with why it did not.
	Start func(ctx context.Context) error
	// Smoke proves a staged qemu tree runs on this host.
	Smoke func(ctx context.Context, tree string) error
	// SelfTest runs the candidate agent at path the way the update launcher will run it once it
	// is committed -- its VerifyAgent check, as its own process, against dir (laid out as
	// VerifyAgent reads it) with the keyring at keyring -- and returns why it refused.
	SelfTest func(ctx context.Context, path, dir, keyring string) error
	// Stopped reports whether the agent's OS service is down and staying down -- `briard stop` -- in
	// which case the run defers. nil never defers.
	Stopped func(ctx context.Context) (bool, error)
	// Guest, once the agent is current, asks the running agent to take the guest OS image the
	// release now in place pairs with (DirectiveUpdateVMAuto), and returns that answer's line; an
	// error is the run's. The two chains then move in one sequence, the agent first, so an image
	// never meets the agent older than its min_briard on its way in. nil skips the step.
	Guest func(ctx context.Context, target string) (string, error)
}

// Deferred is the line a run ends on when briard was stopped: an answer, so the update's result
// reads ok and nothing alerts, and the update happens on the first run after `briard start`.
const Deferred = "deferred: briard is stopped"

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
	// A node the user stopped stays stopped: the run would stop the stopped agent, swap, and start
	// it, the guest back up in the night. A crashed node is not stopped (its OS service failed, or is
	// restarting) and still updates, because the update may be its fix; so does a node whose state
	// could not be read.
	if u.Stopped != nil {
		if down, err := u.Stopped(ctx); err != nil {
			logf("update: could not tell whether briard was stopped (%v); updating", err)
		} else if down {
			return Deferred, nil
		}
	}
	want, wantBytes, wantSig, err := u.Fetcher.fetchManifest(ctx, target)
	if err != nil {
		return "", err
	}
	have := u.installed(logf)
	var stable *Manifest
	if !IsPointer(target) {
		s, _, _, err := u.Fetcher.fetchManifest(ctx, TargetStable)
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
		return u.thenGuest(ctx, target, want, d.Reason)
	}
	logf("update: %s", d.Reason)

	// Fetch everything first, into a private temp dir on the candidates' filesystem, and stage
	// nothing until it all verified: a refusal anywhere (a tampered net-wrap, a qemu tarball whose
	// hash disagrees) returns before any .next exists, so refuse-and-stay holds for the whole
	// bundle. The one thing that may outlive a refused run, deliberately, is an extracted qemu or
	// guest tree: verified bytes under a name no link points at, inert until a run stages a link.
	tmp, err := os.MkdirTemp(u.Layout.Base, ".update-")
	if err != nil {
		return "", fmt.Errorf("install: update temp dir: %w", err)
	}
	defer os.RemoveAll(tmp)
	from := path.Join(u.Fetcher.Chain, want.Version, u.Fetcher.Platform)
	var agent, netWrap, tray, keyring *Entry
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
		case artifactTray:
			if unchanged(have, *a) {
				logf("update: %s unchanged since %s; not fetched", a.Name, have.Version)
				continue
			}
			if err := u.Fetcher.fetchArtifact(ctx, from, tmp, *a); err != nil {
				return "", err
			}
			tray = a
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
		case artifactKeyring:
			// Never hash-skipped: a few hundred bytes, and what it is compared with is the file on
			// disk, not the installed manifest, so a run that died after the swap heals on the next.
			if err := u.Fetcher.fetchArtifact(ctx, from, tmp, *a); err != nil {
				return "", err
			}
			keyring = a
		default:
			logf("update: %s is not part of the briard bundle this agent knows; left alone", a.Name)
		}
	}
	if agent == nil {
		return "", fmt.Errorf("%w: release %s ships no %s", ErrManifest, want.Version, artifactAgent)
	}

	// Stage. Whatever an earlier run staged of the bundle goes first, so the only .next
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
	if tray != nil {
		if err := stageFile(filepath.Join(tmp, tray.Name), u.Layout.StageNextTray); err != nil {
			return "", fmt.Errorf("install: stage tray: %w", err)
		}
		staged = append(staged, "tray")
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

	// The qemu question, answered before anything stops: not "is this qemu good" but "does release
	// N work on this host" (a missing lib, an old glibc, an absent device model), and no refuses
	// the release. This binary is the release's own agent, so the pairing proven is agent N with
	// qemu N, and a refusal costs the household nothing: the staged set goes, nothing was touched.
	if qemuTree != "" {
		logf("update: smoke-testing the staged qemu %s", filepath.Base(qemuTree))
		if err := u.Smoke(ctx, qemuTree); err != nil {
			u.discard(logf)
			return "", fmt.Errorf("host bundle %s refused on this host: %w", want.Version, err)
		}
	}

	// The release's keyring, read before the self-test that uses it. A keyring with no key would
	// fail every later fetch closed, and nothing but a reinstall could bring the node back.
	var ringBytes []byte
	if keyring != nil {
		if ringBytes, err = releaseKeyring(filepath.Join(tmp, keyring.Name)); err != nil {
			u.discard(logf)
			return "", err
		}
	}

	// The check every later update rests on, proven before anything stops: once committed, this
	// candidate is what the launcher asks whether the next release's agent may run, so a failing
	// check would refuse every release after it, and only a reinstall would mend the node. It runs
	// as the launcher will (its own process, this release's manifest and signature, its own bytes,
	// the keyring that will be on disk), so it also refuses a release whose keyring lacks the key
	// that signed it. A refusal costs nothing: the staged set goes, the committed agent stays.
	ring := u.Keyring
	if ringBytes != nil {
		ring = filepath.Join(tmp, keyring.Name)
	}
	if err := os.WriteFile(filepath.Join(tmp, ManifestName), wantBytes, 0o644); err != nil {
		u.discard(logf)
		return "", fmt.Errorf("install: self-test: %w", err)
	}
	if err := os.WriteFile(filepath.Join(tmp, ManifestName+sigSuffix), wantSig, 0o644); err != nil {
		u.discard(logf)
		return "", fmt.Errorf("install: self-test: %w", err)
	}
	if err := u.SelfTest(ctx, filepath.Join(tmp, agent.Name), tmp, ring); err != nil {
		u.discard(logf)
		return "", fmt.Errorf("host bundle %s refused: its agent cannot verify its own release, so it could verify no later one: %w", want.Version, err)
	}
	logf("update: %s's agent verifies its own release", want.Version)

	// The keyring moves here: past every refusal, before the agent that reads it starts. It is how
	// a key rotates, so it is not an entry of the swap: its path is the one the frozen launcher
	// hands every run, outside the layout, and a revert leaves it, since a revert is about this
	// release's code and the keys were vouched for by a key the node already trusts.
	if ringBytes != nil {
		if err := u.layKeyring(ringBytes, logf); err != nil {
			u.discard(logf)
			return "", err
		}
	}

	// The stop is the safe point. The agent's ordinary stop path refuses new acts, finishes the one
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
	return u.thenGuest(ctx, target, want, fmt.Sprintf("updated to %s (%s)", want.Version, strings.Join(staged, ", ")))
}

// thenGuest is the run's last step, once the agent is current: the image the release in place
// pairs with. A pointer names its pair on both chains (`latest`, `promote` and `dev` move them
// together), so the run's own target word; an exact release id names it in its manifest's `vm`.
// The image's line joins the agent's, and its failure is the run's, so the one verdict covers both.
func (u *Update) thenGuest(ctx context.Context, target string, want Manifest, line string) (string, error) {
	vm := target
	if !IsPointer(target) {
		vm = want.VM
	}
	if u.Guest == nil || vm == "" {
		return line, nil
	}
	g, err := u.Guest(ctx, vm)
	if err != nil {
		return "", fmt.Errorf("%s; vm: %w", line, err)
	}
	return line + "; vm: " + g, nil
}

// revert puts the committed release back after a candidate that did not start (or a swap that
// failed part-way), starts it, and leaves the reason for that agent to escalate: the run has no
// notifier, and the agent that comes back is the one that can still report.
func (u *Update) revert(ctx context.Context, version string, cause error, logf func(string, ...any)) error {
	logf("update: %s did not start (%v); reverting", version, cause)
	if err := u.Stop(ctx, 0); err != nil {
		logf("update: stopping the candidate: %v", err)
	}
	back, err := u.revertLayout(ctx, logf)
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

// revertTries bounds the revert's retry, revertPause spaces it. The service manager may restart
// the stopped agent under the revert (on Windows, recovery restarts a failed candidate 5 s
// later), and a rename landing on the restarted program fails, so a miss stops the agent and
// reverts again: a lost race costs a round, not a night on the candidate. Layout.Revert is safe
// to repeat: an entry it put back has no .prev left.
const (
	revertTries = 4
	revertPause = time.Second
)

// revertLayout puts the committed release back (Layout.Revert), retrying a miss up to revertTries.
// It returns every entry put back across the tries.
func (u *Update) revertLayout(ctx context.Context, logf func(string, ...any)) ([]string, error) {
	var all []string
	for try := 1; ; try++ {
		back, err := u.Layout.Revert()
		all = append(all, back...)
		if err == nil || try == revertTries {
			return all, err
		}
		logf("update: the revert missed (%v); stopping the agent again and retrying (%d of %d)", err, try+1, revertTries)
		select {
		case <-ctx.Done():
			return all, err
		case <-time.After(revertPause):
		}
		if serr := u.Stop(ctx, 0); serr != nil {
			logf("update: stopping the agent again: %v", serr)
		}
	}
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
	back, err := u.revertLayout(ctx, logf)
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

// releaseKeyring reads the verified keyring at src, refused when it holds no key.
func releaseKeyring(src string) ([]byte, error) {
	b, err := os.ReadFile(src)
	if err != nil {
		return nil, fmt.Errorf("install: read the release's keyring: %w", err)
	}
	if kr, err := selfupdate.NewKeyring(b); err != nil {
		return nil, fmt.Errorf("%w: the release's keyring: %v", ErrManifest, err)
	} else if kr.Len() == 0 {
		return nil, fmt.Errorf("%w: the release's keyring holds no key", ErrManifest)
	}
	return b, nil
}

// layKeyring replaces the node's keyring with the release's (releaseKeyring), unless they already
// agree.
func (u *Update) layKeyring(b []byte, logf func(string, ...any)) error {
	if have, err := os.ReadFile(u.Keyring); err == nil && bytes.Equal(have, b) {
		return nil
	}
	if err := atomicfile.Write(u.Keyring, b, 0o644, 0o755); err != nil {
		return fmt.Errorf("install: write the keyring: %w", err)
	}
	logf("update: the release keyring is now the release's own")
	return nil
}

func stageFile(src string, stage func(io.Reader) error) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	return stage(f)
}

// extractTree unpacks a verified qemu or guest-bundle tarball into dest, which appears whole or
// not at all (unpacked into a sibling temp dir, renamed into place). tar(1) rather than
// archive/tar: the install unpacks this same tarball with it, and every host we install on has
// one (Windows ships tar.exe) -- one way to open the bundle, not two.
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

// DirectiveUpdateVM is the vm chain's local directive kind: `briard update --vm` submits it
// through the admin door, the agent's own nightly timer in-process; the payload is a target
// (`stable`, `latest`, an exact vm id; "" is stable). Deliberately not in shared/api: the cloud's
// kind is `upgrade-system` (an exact vm release id) and the wire allowlist stays closed: a kind
// that never crosses to the cloud does not belong in the contract that says what can. It lives
// here, not in agent/host, so the CLI and the host share one spelling. ⚠️ Fleet tests also read
// that spelling: they wait on `directive update-vm` in the journal by text.
const DirectiveUpdateVM = "update-vm"

// DirectiveUpdateVMAuto is the update run's ask once the agent is current (Update.Guest): the
// same operation as update-vm, which the agent applies only where this node moves its own OS. The
// rule is the agent's because only it knows the node's role now (a pairing changes it at
// runtime); a node it does not apply to answers done, unchanged.
const DirectiveUpdateVMAuto = "update-vm-auto"

// ErrBriardTooOld is the min_briard refusal: the installed briard predates what a vm release
// tolerates.
var ErrBriardTooOld = errors.New("install: briard is older than the vm release requires")

// BriardSatisfies applies a vm release's min_briard to the installed briard release id: nil when
// briard is at or past it (ordered on the date field, as the stable path is) or minBriard is
// empty, ErrBriardTooOld otherwise. The message names both remedies: a briard that cannot update
// past the floor is outside its support window, the answer there is reinstall, and it must never
// drift silently. One vocabulary end to end: wire field, thing compared and remedy all say
// `briard`.
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
