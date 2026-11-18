package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"briard.io/agent/install"
	"briard.io/agent/selfupdate"
	"briard.io/shared/api"
	"briard.io/shared/atomicfile"
	"briard.io/shared/notify"
)

// The vm chain. The guest OS is its own release line (`vm.<date>.<inputs>` beside briard's
// `v<epoch>.<date>.<rev>`), and its signed manifest names the closure the image boots (Manifest.System)
// and the oldest briard that tolerates it (Manifest.MinBriard). Resolving a vm release: fetch and
// verify the target's manifest, refuse it if the installed briard is older than it needs, compare
// its closure with what the guest runs, stage the release's image, and hand it to the image
// upgrade (osupgrade.go: swap, boot, prove the closure, health-gate, commit or restore).
//
// Three triggers, mirroring the briard chain: the cloud (`upgrade-system`, an exact vm release
// id), `briard update --vm` (`update-vm` over the admin socket, resolving `stable` by default) and
// a nightly timer inside the agent (`update-vm` with `stable`). That timer is correct, not
// inconsistent with the briard chain's frozen unit, because the bootstrap problem is agent-only: a
// broken agent that cannot upgrade the guest is not bricked, since the host timer fixes the agent
// and the agent then fixes the guest. Only the thing at the bottom needs an updater beneath it.

// guestReleaseCacheName is the node-local record of the guest release this node last applied
// (the exact signed manifest bytes), seeded by the install's image lay-down (layDownImage) from
// the release it installed. It is what the stable path orders against: the guest itself knows
// only a closure path, not a release id. Not $PREFIX/guest-image/manifest.json -- that one is
// written only by the lay-down and describes the image the install placed, which an image
// upgrade does not rewrite.
const guestReleaseCacheName = "guest-release.json"

// systemReader is the one guest fact the resolver needs: which closure the guest runs. The
// guestReader satisfies it; a test hands in a stub.
type systemReader interface {
	SystemPath(ctx context.Context) (string, error)
}

// applyGuestUpdate is the update-vm directive, and the cloud's upgrade-system: resolve d.Payload
// (a target: `latest`, `stable`, or an exact vm id; "" is stable) on the vm chain and, if due,
// run the image upgrade to that release. The outcome is the upgrade's own -- done, rolled back,
// failed -- and a refusal before anything moved (an unverifiable manifest, a briard too old, a pin
// below the floor) is a failure that names its reason.
func (cfg Config) applyGuestUpdate(ctx context.Context, d api.Directive, r systemReader, up upgrader, n notify.Notifier, logf func(string, ...any)) api.DirectiveOutcome {
	failed := func(detail string) api.DirectiveOutcome {
		return api.DirectiveOutcome{ID: d.ID, State: api.OutcomeFailed, Detail: detail}
	}
	// The update run's ask (update-vm-auto) is honoured only where this node moves its own OS;
	// anywhere else it is answered done and unchanged, which is the run's whole verdict on it.
	if d.Kind == install.DirectiveUpdateVMAuto {
		if why := cfg.osUpdatesNotOurs(); why != "" {
			return api.DirectiveOutcome{ID: d.ID, State: api.OutcomeDone, Detail: why}
		}
		if up == nil {
			return api.DirectiveOutcome{ID: d.ID, State: api.OutcomeDone, Detail: "no VM on this machine"}
		}
	}
	target := d.Payload
	// An empty payload is `stable`, not `latest`. Every update target on every path
	// defaults to the promoted pointer; `latest` is proven by a canary that names it on purpose.
	if target == "" {
		target = install.TargetStable
	}
	if up == nil {
		logf("directive kind=update-vm ignored (no guest on this node)")
		return failed("no VM on this machine")
	}
	// Fail closed, as every verifier here does: no key, no release. The same PEM the catalog
	// path verifies with (app.go), parsed the same way.
	kr, err := selfupdate.NewKeyring(cfg.UpdateKeyring)
	if err != nil || kr.Len() == 0 {
		logf("directive kind=update-vm refused: no usable release keyring on this node (%v)", err)
		return failed("no release keyring on this machine; a VM release cannot be verified")
	}
	f := &install.Fetcher{BaseURL: cfg.ChannelURL, Chain: install.ChainVM, Platform: install.PlatformVM, Keyring: kr, Logf: logf}
	rctx, cancel := context.WithTimeout(ctx, 2*time.Minute) // two small signed files, at most three
	defer cancel()
	want, raw, err := f.Manifest(rctx, target)
	if err != nil {
		logf("directive update-vm: resolving %s failed: %v", target, err)
		return failed(err.Error())
	}
	var stable *install.Manifest
	if !install.IsPointer(target) {
		s, _, err := f.Manifest(rctx, install.TargetStable)
		if err != nil {
			return failed(fmt.Sprintf("%v: cannot pin %s — reading stable failed: %v", install.ErrBelowFloor, target, err))
		}
		stable = &s
	}
	running, err := r.SystemPath(rctx)
	if err != nil {
		return failed("cannot read the VM's running system: " + err.Error())
	}
	have := cfg.cachedGuestRelease(logf)
	dec, err := decideGuest(target, want, have, stable, running, cfg.Version)
	if err != nil {
		// No alert, min_briard included: the update run takes the agent first, so an agent below an
		// image's min_briard is one whose own update failed, which that run reports as this node
		// having stopped updating; and no published agent meeting it is the publisher's fault, with
		// nothing for a household to do about it.
		logf("directive update-vm refused: %v", err)
		return failed(err.Error())
	}
	if !dec.Install {
		logf("directive update-vm: %s", dec.Reason)
		if running == want.System && (have == nil || have.Version != want.Version) {
			// The guest is on this release's closure but the record did not say so (the cloud
			// moved it by closure, or the record was lost): make the record true.
			cfg.rememberGuestRelease(raw, logf)
		}
		return api.DirectiveOutcome{ID: d.ID, State: api.OutcomeDone, Detail: dec.Reason}
	}
	logf("directive update-vm: %s — staging %s's image", dec.Reason, want.Version)
	// The image, staged before anything stops: fetched and verified into a private
	// dir beside the image in use (same filesystem, so the swap is a rename), expanded, and
	// placed at nextImage(). A fetch or hash failure returns here with the node untouched.
	uctx, cancel := cfg.beat.budget(ctx, cfg.UpgradeBudget)
	defer cancel()
	if err := cfg.stageGuestImage(uctx, f, want, logf); err != nil {
		logf("directive update-vm: staging failed, node unchanged: %v", err)
		escalate(ctx, n, logf, "this machine", "guest OS image stage", want.Version, afterUnchanged, err)
		return failed(err.Error())
	}
	back, err := up.ImageUpgrade(uctx, want)
	switch {
	case errors.Is(err, ErrHandoverRequired):
		logf("directive update-vm DECLINED, node untouched and serving: %v", err)
		return api.DirectiveOutcome{ID: d.ID, State: api.OutcomeRolledBack, Detail: err.Error()}
	case err != nil && back:
		logf("directive update-vm rolled back: %v", err)
		escalate(ctx, n, logf, "this machine", "guest OS upgrade", want.Version, afterRolledBack, err)
		return api.DirectiveOutcome{ID: d.ID, State: api.OutcomeRolledBack, Detail: err.Error()}
	case err != nil:
		logf("directive update-vm FAILED without a clean rollback: %v", err)
		escalate(ctx, n, logf, "this machine", "guest OS upgrade", want.Version, afterNotUndone, err)
		return failed(err.Error())
	}
	cfg.rememberGuestRelease(raw, logf)
	logf("directive update-vm applied: now running %s", want.Version)
	return api.DirectiveOutcome{ID: d.ID, State: api.OutcomeDone, Detail: "now running " + want.Version}
}

// guestImageArtifact is the vm chain's image as the manifest names it (compressed; the
// fetcher expands it to nixos.qcow2 after verifying the bytes the release signed).
const guestImageArtifact = "nixos.qcow2.zst"

// stageGuestImage fetches and verifies rel's image and places it, expanded, at
// nextImage(cfg.GuestImage) -- the exact file ImageUpgrade renames into place. A staged image
// left by an earlier run is replaced. Refuse-and-stay: on any error nothing under the image
// directory has changed except that the temp dir is gone.
func (cfg Config) stageGuestImage(ctx context.Context, f *install.Fetcher, rel install.Manifest, logf func(string, ...any)) error {
	if cfg.GuestImage == "" {
		return errors.New("no GUEST_IMAGE configured: this node's launch does not name the image it boots")
	}
	var art *install.Entry
	for i := range rel.Artifacts {
		if rel.Artifacts[i].Name == guestImageArtifact {
			art = &rel.Artifacts[i]
		}
	}
	if art == nil {
		return fmt.Errorf("%w: release %s ships no %s", install.ErrManifest, rel.Version, guestImageArtifact)
	}
	dir := filepath.Dir(cfg.GuestImage)
	tmp, err := os.MkdirTemp(dir, ".stage-")
	if err != nil {
		return fmt.Errorf("stage dir beside %s: %w", cfg.GuestImage, err)
	}
	defer os.RemoveAll(tmp)
	if err := f.Artifact(ctx, rel.Version, tmp, *art); err != nil {
		return err
	}
	expanded := filepath.Join(tmp, strings.TrimSuffix(guestImageArtifact, ".zst"))
	// Flushed before the rename, and the rename flushed after: an unflushed image under
	// the .next name is a truncated OS that ImageUpgrade would install as verified.
	if err := atomicfile.SyncTree(expanded); err != nil {
		return fmt.Errorf("flush the staged image: %w", err)
	}
	if err := os.Rename(expanded, nextImage(cfg.GuestImage)); err != nil {
		return fmt.Errorf("place the staged image: %w", err)
	}
	if err := atomicfile.SyncDir(dir); err != nil {
		return fmt.Errorf("flush the image directory: %w", err)
	}
	logf("guest update: %s's image staged at %s", rel.Version, nextImage(cfg.GuestImage))
	return nil
}

// decideGuest is the vm chain's comparison, pure so it can be enumerated. Order matters:
// a release naming no closure cannot be applied at all; a briard below the release's min_briard
// is refused regardless of anything else (the one direction that can go wrong); a guest
// already running the release's closure is at it whatever the record says; and only then do the
// briard chain's ordering rules apply -- stable forward on the date, latest/exact on the full id,
// an exact pin floored at stable. A record that names this very release while the guest runs
// another closure is disbelieved (the cloud moved the node by closure since), so the release
// is applied rather than reported as already there.
func decideGuest(target string, want install.Manifest, have, stable *install.Manifest, running, briardVersion string) (install.Decision, error) {
	if want.System == "" {
		return install.Decision{}, fmt.Errorf("%w: guest release %s names no system closure", install.ErrManifest, want.Version)
	}
	if err := install.BriardSatisfies(want.MinBriard, briardVersion); err != nil {
		return install.Decision{}, err
	}
	if running != "" && running == want.System {
		return install.Decision{Reason: fmt.Sprintf("already running %s (%s)", want.Version, want.System)}, nil
	}
	if have != nil && have.Version == want.Version {
		have = nil
	}
	return install.Decide(target, want, have, stable)
}

// cachedGuestRelease reads the node-local record of the last applied guest release; nil when
// absent or unreadable (the safe default: install).
func (cfg Config) cachedGuestRelease(logf func(string, ...any)) *install.Manifest {
	if cfg.GuestReleaseCache == "" {
		return nil
	}
	b, err := os.ReadFile(cfg.GuestReleaseCache)
	if err != nil {
		logf("guest update: no recorded guest release at %s (%v)", cfg.GuestReleaseCache, err)
		return nil
	}
	var m install.Manifest
	if err := json.Unmarshal(b, &m); err != nil || m.Version == "" {
		logf("guest update: recorded guest release at %s unreadable (%v)", cfg.GuestReleaseCache, err)
		return nil
	}
	return &m
}

// rememberGuestRelease writes the record: the exact signed bytes, durably (a fact whose only
// copy is this file). Best-effort -- the OS already moved, and a lost record costs one extra
// manifest fetch and compare next time, never a wrong upgrade.
func (cfg Config) rememberGuestRelease(raw []byte, logf func(string, ...any)) {
	if cfg.GuestReleaseCache == "" {
		return
	}
	if err := atomicfile.Write(cfg.GuestReleaseCache, raw, 0o644, 0o755); err != nil {
		logf("guest update: could not record the applied release at %s: %v", cfg.GuestReleaseCache, err)
	}
}

// osUpdatesNotOurs says why this node does not move its own OS, "" when it does. Only a
// standalone node does, at the end of its update run (DirectiveUpdateVMAuto), and the rule is
// here, in the agent, because only the agent knows the node's role now:
//
//   - A managed node's OS is the cloud's to move (it sequences a flock one node at a time,
//     serving node last, inside the home's window); a second driver on the node would race it.
//     So the presence of a controller switches this off, with no knob.
//   - A node with peers and no orchestrator: each node deciding to reboot on its own is a
//     correlated outage. Automatic OS updates disable themselves and say so (announceOSUpdates),
//     derived from the mesh like every other statement about role.
func (cfg Config) osUpdatesNotOurs() string {
	switch {
	case cfg.ControllerURL != "":
		return "automatic OS updates are the orchestrator's on this machine"
	case len(cfg.Resource.Peers) > 1:
		return "automatic OS updates are off on this machine: it has peers and no orchestrator"
	}
	return ""
}

// announceOSUpdates says at start whether this node's OS is updated without a human: off, and
// why, on a node with peers and no orchestrator; on again (which ends that alert) on a lone node
// or one with an orchestrator.
func (cfg Config) announceOSUpdates(ctx context.Context, n notify.Notifier, logf func(string, ...any)) {
	if cfg.ControllerURL == "" && cfg.osUpdatesNotOurs() != "" {
		fireAlert(ctx, n, logf, notify.Alert{
			Key:      "os-updates",
			Kind:     notify.Open,
			Severity: notify.Info,
			Title:    "Briard: automatic OS updates are off on this machine",
			Body:     fmt.Sprintf("%s has %d peers and no orchestrator: machines updating their OS independently would reboot together. Run `briard update --vm` on one machine at a time.", cfg.Node, len(cfg.Resource.Peers)-1),
		})
		return
	}
	fireAlert(ctx, n, logf, notify.Alert{
		Key:   "os-updates",
		Kind:  notify.Resolved,
		Title: "Briard: OS updates are on again on this machine",
		Body:  fmt.Sprintf("%s's OS is updated again, on its own schedule or by its orchestrator.", cfg.Node),
	})
}
