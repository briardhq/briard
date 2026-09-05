package install

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
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
// Scope is the agent binary alone ([B.86b] widens it to the bundle): the one thing whose
// restart is transparent, because the guest lives in its own transient unit and is re-adopted.

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
// the installed manifest, and stage + arm the agent artifact when due.
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

	var agent *Entry
	for i := range want.Artifacts {
		if want.Artifacts[i].Name == "briard-agent" {
			agent = &want.Artifacts[i]
		}
	}
	if agent == nil {
		return "", fmt.Errorf("%w: release %s ships no briard-agent", ErrManifest, want.Version)
	}
	// Into a private temp dir on the same filesystem as the candidate, then staged through the
	// layout (write + fsync + rename) — verified bytes only ever reach agent.next whole.
	tmp, err := os.MkdirTemp(u.Layout.Base, ".update-")
	if err != nil {
		return "", fmt.Errorf("install: update temp dir: %w", err)
	}
	defer os.RemoveAll(tmp)
	if err := u.Fetcher.fetchArtifact(ctx, path.Join(u.Fetcher.Chain, want.Version, u.Fetcher.Platform), tmp, *agent); err != nil {
		return "", err
	}
	f, err := os.Open(tmp + "/briard-agent")
	if err != nil {
		return "", err
	}
	defer f.Close()
	if err := u.Layout.StageNext(f); err != nil {
		return "", fmt.Errorf("install: stage candidate: %w", err)
	}
	if err := u.Layout.StageNextManifest(wantBytes); err != nil {
		return "", fmt.Errorf("install: stage manifest: %w", err)
	}
	if err := u.Layout.Arm(); err != nil {
		return "", fmt.Errorf("install: arm: %w", err)
	}
	return fmt.Sprintf("staged %s, armed — the agent restarts itself at its next safe point (or now: systemctl restart briard-agent)", want.Version), nil
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
