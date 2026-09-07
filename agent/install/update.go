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

	"briard.io/agent/selfupdate"
)

// THE UPDATE VERB ([B.86a]). `briard-agent --fetch-update <target>` is the narrowed fetch the
// frozen update unit runs — on a FRESH binary it just pulled from the target's pointer, never on
// the committed one, so a fetch/verify/manifest bug in the running agent cannot prevent its own
// replacement (install.sh's bootstrap pattern, on a timer). Everything here is therefore the
// SUSPECT side's job: resolve the target, decide against the installed manifest, fetch and
// verify the agent artifact, stage it beside its manifest, arm the trial — and STOP. It never
// restarts anything: the running agent restarts itself at its safe point, and the unit's
// forcing after a grace is the backstop. That separation is what keeps "the case where forcing
// is risky" and "the case where forcing happens" from ever overlapping.
//
// Scope is the HOST BUNDLE ([B.86b]): the agent, the guest launch shim and the qemu tree move
// as one release, staged as .next siblings and committed together by briard-commit. Only
// entries whose sha256 differs from the installed manifest's are fetched -- a JSON compare, no
// hashing of the 86 MB qemu tree -- which is what keeps a daily tick at the agent's ~9 MB
// rather than the bundle's ~28 MB, and is safe by construction: an identical hash is an
// identical artifact, so a skipped qemu is still the pairing the release was tested with.
// A staged qemu is smoke-tested by the CANDIDATE before it sends READY (host.Run), so a
// release that does not run on this host refuses itself whole and the pivot lands back on
// the combination that was tested, never on agent N with qemu N-1.

// The three artifacts of the host bundle, by the names the manifest (and install.sh) use.
// Anything else a host manifest may one day carry is left to the release that knows it.
const (
	artifactAgent   = "briard-agent"
	artifactNetWrap = "briard-net-wrap"
	artifactQEMU    = "qemu-bundle.tar.zst"
	artifactGuest   = "guest-bundle.tar.zst" // the binaries the host dresses its guest with ([B.86j])
)

// ErrBelowFloor is returned for an exact pin older than the current stable (or one whose floor
// cannot be read): refused loudly, so a bad pin looks like one to whoever sent it.
var ErrBelowFloor = errors.New("install: pinned release is below the stable floor")

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
//     but still no-op at equality, or `briard update host` would bounce an up-to-date agent.
//   - An exact id may be OLDER than the installed one (a bad release must be revocable without
//     reinstalling every home) but NEVER older than stable: unbounded, a buggy or compromised
//     agent could name any old signed release; floored, the worst it reaches is a build we
//     currently vouch for. To go below stable, move stable. A pin below the floor fails loudly.
//
// The chain/platform precondition is cheap and load-bearing: a crossed wire between the host and
// guest chains would otherwise compare a guest date against a host date and silently no-op.
func Decide(target string, want Manifest, have, stable *Manifest) (Decision, error) {
	if have != nil && (have.Chain != want.Chain || have.Platform != want.Platform) {
		return Decision{}, fmt.Errorf("%w: installed %s, offered %s", ErrWrongChain,
			path.Join(have.Chain, have.Platform), path.Join(want.Chain, want.Platform))
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
		return Decision{Reason: fmt.Sprintf("installed %s is at or past stable %s; nothing to do", have.Version, want.Version)}, nil
	case TargetLatest:
		// latest is by construction never older than stable, so no floor to check.
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

// Update is one run of the verb: resolve target on the fetcher's chain/platform, decide against
// the installed manifest, and stage + arm the changed parts of the host bundle when due.
type Update struct {
	Fetcher *Fetcher
	Layout  selfupdate.Layout
	Logf    func(string, ...any)
}

// Run returns the one line the run ended on — "already at …" or "staged …, armed" — and an
// error for a refusal (bad pin, bad signature, tampered artifact), in which case nothing was
// staged and nothing armed (refuse-and-stay).
func (u *Update) Run(ctx context.Context, target string) (string, error) {
	logf := u.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if !validSegment(target) {
		return "", fmt.Errorf("install: bad release target %q", target)
	}
	want, wantBytes, err := u.Fetcher.fetchManifest(ctx, target)
	if err != nil {
		return "", err
	}
	have := u.installed(logf)
	var stable *Manifest
	if target != TargetStable && target != TargetLatest {
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
			// Never hash-skipped: the candidate is what the trial RUNS, and its id is baked into
			// it, so on a real release the skip could not fire anyway.
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
				// Left by an earlier run of this same release (a failed trial, most likely):
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
			// The guest bundle rides exactly qemu's mechanics ([B.86j]): one extracted tree per
			// release, a `.next` link the frozen commit moves, hash-skipped when unchanged.
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
			logf("update: %s is not part of the host bundle this agent knows; left alone", a.Name)
		}
	}
	if agent == nil {
		return "", fmt.Errorf("%w: release %s ships no %s", ErrManifest, want.Version, artifactAgent)
	}

	// STAGE. Whatever an earlier run staged of the bundle goes first, so the only .next
	// siblings briard-commit can find are this release's; then each verified file lands whole
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
	if err := u.Layout.Arm(); err != nil {
		return "", fmt.Errorf("install: arm: %w", err)
	}
	return fmt.Sprintf("staged %s (%s), armed — the agent restarts itself at its next safe point (or now: systemctl restart briard-agent)",
		want.Version, strings.Join(staged, ", ")), nil
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
// (unpacked into a sibling temp dir, renamed into place). tar(1) rather than archive/tar:
// install.sh unpacks this same tarball with it, and the update unit's PATH reaches it on
// every host we install on -- one way to open the bundle, not two.
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
	if err := os.Rename(tmp, dest); err != nil {
		os.RemoveAll(tmp)
		return fmt.Errorf("install: place qemu tree: %w", err)
	}
	return nil
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

// DirectiveUpdateGuest is the LOCAL directive kind of the guest chain ([B.86d]): `briard update
// guest` and the agent's own nightly timer submit it through the admin door; the payload is a
// target (`latest`, `stable`, an exact guest id; "" is latest). It is deliberately NOT in
// shared/api: the cloud names closures (`upgrade-system`) and the wire allowlist stays closed --
// a kind that never crosses to the cloud does not belong in the contract that says what can.
// It lives here rather than in agent/host so the CLI and the host share one spelling.
const DirectiveUpdateGuest = "update-guest"

// ErrHostTooOld is the min_host refusal: this host predates what a guest release tolerates.
var ErrHostTooOld = errors.New("install: this host is older than the guest release requires")

// HostSatisfies applies a guest release's min_host to this host's release id: nil when the host
// is at or past it (ordered on the date field, as the stable path is), ErrHostTooOld otherwise.
// An empty minHost places no requirement. The message names both remedies, because a host that
// cannot update past the floor is a node outside its support window, and the answer there is
// reinstall -- it must never drift silently ([B.86e]).
func HostSatisfies(minHost, host string) error {
	if minHost == "" {
		return nil
	}
	need, err := dateOf(minHost)
	if err != nil {
		return err
	}
	have, err := dateOf(host)
	if err != nil {
		return fmt.Errorf("%w: this host's release id %q has no date to compare min_host %s against", ErrHostTooOld, host, minHost)
	}
	if have < need {
		return fmt.Errorf("%w: host %s < min_host %s — update the host first (`briard update host`); a host that can no longer update is outside its support window and must be reinstalled", ErrHostTooOld, host, minHost)
	}
	return nil
}
