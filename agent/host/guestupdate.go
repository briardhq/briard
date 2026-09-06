package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"time"

	"briard.io/agent/install"
	"briard.io/agent/selfupdate"
	"briard.io/shared/api"
	"briard.io/shared/atomicfile"
	"briard.io/shared/notify"
)

// THE GUEST CHAIN ([B.86d]). The guest OS is its own release line -- `guest.<date>.<rev>` beside
// the host's `v3.<date>.<rev>` -- and its signed manifest names the CLOSURE the image boots
// (Manifest.System) and the oldest host that tolerates it (Manifest.MinHost). Resolving a guest
// release is therefore: fetch and verify the target's manifest, refuse it if this host is older
// than it needs, compare its closure with what the guest runs, and hand the closure to the
// existing OS-upgrade path (stage from the cache, switch or reboot, health-gate, commit or
// revert). Nothing below the manifest changes: the cloud still names closures in
// `upgrade-system`, the guest's os.* verbs are untouched; what this adds is a node that can move
// its own OS from a channel -- which an OSS install could not do at all until now.
//
// Three triggers, mirroring the host chain: the cloud (a closure, as before), `briard update
// guest` (this directive, over the admin socket, resolving `latest` by default) and a timer
// INSIDE the agent (this directive with `stable`, nightly). The timer living inside the agent is
// correct and not an inconsistency with the host chain's frozen unit: the bootstrap problem is
// agent-only. A broken agent that cannot upgrade the guest is not bricked, because the host
// timer fixes the agent and the agent then fixes the guest. Only the thing at the bottom needs
// an updater beneath it.

// guestReleaseCacheName is the node-local record of the guest release this node last applied
// (the exact signed manifest bytes), seeded by install.sh from the release it installed. It is
// what the stable path orders against: the guest itself knows only a closure path, not a
// release id. NOT $PREFIX/guest-image/manifest.json -- that one describes the IMAGE on disk,
// which lags the running OS until [B.86f] fetches the new image after an upgrade commits.
const guestReleaseCacheName = "guest-release.json"

// systemReader is the one guest fact the resolver needs: which closure the guest runs. The
// guestReader satisfies it; a test hands in a stub.
type systemReader interface {
	SystemPath(ctx context.Context) (string, error)
}

// applyGuestUpdate is the update-guest directive ([B.86d]): resolve d.Payload (a target:
// `latest`, `stable`, or an exact guest id; "" is latest) on the guest chain and, if due, run
// the OS upgrade to the closure it names. The outcome is the upgrade's own -- done, rolled back,
// failed -- and a refusal before anything moved (an unverifiable manifest, a host too old, a pin
// below the floor) is a failure that names its reason.
func (cfg Config) applyGuestUpdate(ctx context.Context, d api.Directive, r systemReader, up upgrader, n notify.Notifier, logf func(string, ...any)) api.DirectiveOutcome {
	failed := func(detail string) api.DirectiveOutcome {
		return api.DirectiveOutcome{ID: d.ID, State: api.OutcomeFailed, Detail: detail}
	}
	target := d.Payload
	if target == "" {
		target = install.TargetLatest
	}
	if up == nil {
		logf("directive kind=update-guest ignored (no guest on this node)")
		return failed("no guest on this node")
	}
	// Fail closed, as every verifier here does: no key, no release. The same PEM the catalog
	// path verifies with (service.go), parsed the same way.
	kr, err := selfupdate.NewKeyring(cfg.UpdateKeyring)
	if err != nil || kr.Len() == 0 {
		logf("directive kind=update-guest refused: no usable release keyring on this node (%v)", err)
		return failed("no release keyring on this node; a guest release cannot be verified")
	}
	f := &install.Fetcher{BaseURL: cfg.ChannelURL, Chain: install.ChainGuest, Keyring: kr, Logf: logf}
	rctx, cancel := context.WithTimeout(ctx, 2*time.Minute) // two small signed files, at most three
	defer cancel()
	want, raw, err := f.Manifest(rctx, target)
	if err != nil {
		logf("directive update-guest: resolving %s failed: %v", target, err)
		return failed(err.Error())
	}
	var stable *install.Manifest
	if target != install.TargetStable && target != install.TargetLatest {
		s, _, err := f.Manifest(rctx, install.TargetStable)
		if err != nil {
			return failed(fmt.Sprintf("%v: cannot pin %s — reading stable failed: %v", install.ErrBelowFloor, target, err))
		}
		stable = &s
	}
	running, err := r.SystemPath(rctx)
	if err != nil {
		return failed("cannot read the guest's running system: " + err.Error())
	}
	have := cfg.cachedGuestRelease(logf)
	dec, err := decideGuest(target, want, have, stable, running, cfg.Version)
	if err != nil {
		if errors.Is(err, install.ErrHostTooOld) {
			// The one refusal an owner must hear about: it is the support window closing on
			// this node, and the remedy (update the host, or reinstall) is theirs to take.
			escalate(ctx, n, logf, "this node", "guest OS update", want.Version, err)
		}
		logf("directive update-guest refused: %v", err)
		return failed(err.Error())
	}
	if !dec.Install {
		logf("directive update-guest: %s", dec.Reason)
		if running == want.System && (have == nil || have.Version != want.Version) {
			// The guest is on this release's closure but the record did not say so (the cloud
			// moved it by closure, or the record was lost): make the record true.
			cfg.rememberGuestRelease(raw, logf)
		}
		return api.DirectiveOutcome{ID: d.ID, State: api.OutcomeDone, Detail: dec.Reason}
	}
	logf("directive update-guest: %s — upgrading to %s", dec.Reason, want.System)
	o := applySystemUpgrade(ctx, d.ID, want.System, up, n, logf, cfg.UpgradeBudget, cfg.beat)
	if o.State == api.OutcomeDone {
		cfg.rememberGuestRelease(raw, logf)
		o.Detail = "now running " + want.Version
	}
	return o
}

// decideGuest is the guest chain's comparison, pure so it can be enumerated. Order matters:
// a release naming no closure cannot be applied at all; a host below the release's min_host
// is refused regardless of anything else (the one direction that can go wrong); a guest
// already RUNNING the release's closure is at it whatever the record says; and only then do the
// host chain's ordering rules apply -- stable forward on the date, latest/exact on the full id,
// an exact pin floored at stable. A record that names this very release while the guest runs
// another closure is disbelieved (the cloud moved the node by closure since), so the release
// is applied rather than reported as already there.
func decideGuest(target string, want install.Manifest, have, stable *install.Manifest, running, hostVersion string) (install.Decision, error) {
	if want.System == "" {
		return install.Decision{}, fmt.Errorf("%w: guest release %s names no system closure", install.ErrManifest, want.Version)
	}
	if err := install.HostSatisfies(want.MinHost, hostVersion); err != nil {
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

// guestUpdateTimer is the third trigger: once a night, in the small hours and jittered, the
// agent converges its guest to `stable` through the same directive the CLI submits, over the
// same channel (so it serialises with the observe loop like every admin operation). It runs
// ONLY on a standalone node:
//
//   - A managed node's OS is the cloud's to move (Rollout.System sequences a flock one node at
//     a time, serving node last, inside the home's window); a second driver on the node would
//     race it. So the presence of a controller switches this off, with no knob.
//   - A node with peers and no orchestrator is the case the design names: each node deciding
//     to reboot on its own is a CORRELATED outage. Automatic OS updates disable themselves and
//     say so, derived from the mesh like every other statement about role.
//
// Both methods run inside the window. The design allowed a switch-method update at any hour;
// the small hours are where the nightly tick already is, and a daily cadence for an OS is not
// worth a second schedule. Failures are the upgrade path's own (escalated there); this only
// logs the outcome.
func (cfg Config) guestUpdateTimer(ctx context.Context, local chan<- localRequest, n notify.Notifier, logf func(string, ...any)) {
	if cfg.ControllerURL != "" {
		return
	}
	if len(cfg.Resource.Peers) > 1 {
		al := notify.Alert{
			Level: notify.Warning,
			Title: "Briard: automatic OS updates are off on this node",
			Body:  fmt.Sprintf("%s has %d peers and no orchestrator: nodes updating their OS independently would reboot together. Run `briard update guest` on one node at a time.", cfg.Node, len(cfg.Resource.Peers)-1),
		}
		logf("%s", notify.LogLine(al))
		if n != nil {
			nctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			_ = n.Notify(nctx, al)
			cancel()
		}
		return
	}
	loc := time.Local
	if tz := localTimezone("/"); tz != "" {
		if l, err := time.LoadLocation(tz); err == nil {
			loc = l
		}
	}
	for {
		next := nextGuestUpdateTick(time.Now(), loc)
		logf("guest update: next automatic check of guest/stable at %s", next.Format(time.RFC3339))
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Until(next)):
		}
		resp := make(chan api.DirectiveOutcome, 1)
		select {
		case <-ctx.Done():
			return
		case local <- localRequest{d: api.Directive{Kind: install.DirectiveUpdateGuest, Payload: install.TargetStable}, resp: resp}:
		}
		select {
		case <-ctx.Done():
			return
		case o := <-resp:
			logf("guest update (nightly): %s %s", o.State, o.Detail)
		}
	}
}

// nextGuestUpdateTick is the next 03:00 local after now, plus up to two hours of jitter -- the
// host chain's timer window, so one household's two nightly checks land in the same quiet hours
// and a fleet does not hit the channel as one.
func nextGuestUpdateTick(now time.Time, loc *time.Location) time.Time {
	n := now.In(loc)
	at := time.Date(n.Year(), n.Month(), n.Day(), 3, 0, 0, 0, loc)
	if !at.After(n) {
		at = at.AddDate(0, 0, 1)
	}
	return at.Add(time.Duration(rand.Int64N(int64(2 * time.Hour))))
}
