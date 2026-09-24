package host

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"briard.io/agent/guest"
	"briard.io/agent/hass"
	"briard.io/agent/mosquitto"
	"briard.io/agent/quadlet"
	"briard.io/agent/selfupdate"
	"briard.io/agent/services"
	"briard.io/shared/api"
	"briard.io/shared/atomicfile"
	"briard.io/shared/manifest"
	"briard.io/shared/model"
)

// Runtime service install. The host orchestrates; the guest is dumb hands.
//
// THE SHAPE: a shipped node mounts, PROMOTES and serves the landing page with zero services, so
// by the time anyone installs one the resource is already Primary with drbd-reactor actively
// driving it. Installing means changing what the VOLUME says this node runs — which is an
// upgrade, from "empty service" to the current version, and belongs on the same rails as one
// rather than getting its own.
//
//	fetch+verify manifest -> render units (this node) -> provision (Primary only)
//	  -> converge -> health-gate -> revert on failure (the {code+data} rollback)
//
// The chain is static and service units are not members of it ([V3b.3](f)), so none of this
// pauses the promoter. The one promoter contact left is a guard: refuse to start while the
// OS-upgrade path holds the maintenance bracket, so an overlap fails loudly at the beginning
// instead of mutating a resource mid-bracket. It cannot PREVENT the
// race — a pause can still land between the check and ours — and is not claimed to.

// serviceInstaller is the slice of the guest client an install drives. *guestagent.Client
// satisfies it; a fake drives the test. Narrow interface for DI, not a seam.
type serviceInstaller interface {
	Status(ctx context.Context, resource string) (model.QuorumState, error)
	ServiceRender(ctx context.Context, files map[string]string, stale []string) error
	ServiceProvision(ctx context.Context, name, dataDir string, subdirs []string, manifest string) error
	ServiceInstalled(ctx context.Context, name string) (string, error)
	// SupportsServiceInstalled gates the whole install: a guest that cannot NAME a service when
	// recording its identity would overwrite whatever it already runs. See applyServiceInstall.
	SupportsServiceInstalled() bool
	// ServiceConverge makes the node match the VOLUME -- render, warm and start every manifest
	// recorded there ([V3b.3](f)). It is what an install does instead of rewriting the promoter
	// chain, and SupportsServiceConverge gates it the same way.
	// ServiceConverge returns the services the node could not PREPARE, having started every other
	// one. An install must fail when its own service is in that list: the node is still serving
	// the prior version, and reporting success would leave the volume, the cache and the cloud
	// naming a version nothing is running.
	ServiceConverge(ctx context.Context) ([]string, error)
	SupportsServiceConverge() bool
	// StorageFree measures the guest's image store ([V3b.31j]): the free-space gate reads it
	// before a pull starts. An older guest that cannot measure gets no gate, not a refusal.
	StorageFree(ctx context.Context) (free, total int64, err error)
	SupportsStorageFree() bool
	// ServicePulling / ServicePulled bracket the pull for the dashboard's bar: the manifest's
	// sizes on the guest's tmpfs before the first byte, gone once the image is present. An older
	// guest shows the install with no bar.
	ServicePulling(ctx context.Context, service string, size, installed int64) error
	ServicePulled(ctx context.Context, service string) error
	SupportsServicePulling() bool
	// ServiceForget removes one service's manifest from the volume -- what reverting a FRESH
	// install requires, now that the volume is what every future promotion renders from.
	ServiceForget(ctx context.Context, name string) error
	// ReactorActive is the interim overlap guard, and the ONLY promoter verb left here: an
	// install no longer takes the maintenance bracket ([V3b.3](f)), it only refuses to start
	// while somebody else holds it. Pausing and resuming belong to the OS upgrade path
	// (agent/guest), which still changes what a live promoted resource runs.
	ReactorActive(ctx context.Context) (bool, error)
	ServiceStart(ctx context.Context, unit string) error
	// ServiceWarm ensures an image is present, pulling only if it is missing -- see BringUp for
	// why "start the .image unit" is not the same operation.
	ServiceWarm(ctx context.Context, unit, ref string) error
	ServiceStop(ctx context.Context, unit string) error
	ServiceHealth(ctx context.Context, url string) (bool, error)
	// ServiceHealthOf is the same floor asked BY NAME, so the guest resolves the address from the
	// routing table it converged rather than from a URL the host assembled ([B.48]).
	ServiceHealthOf(ctx context.Context, service string) (bool, error)
	// The S1 gate.s input, one layer above the liveness floor ServiceHealth answers -- one method
	// per service that has a signal, named for it (see readinessProbe, which is the same set).
	// Not on `upgrader`, deliberately: an OS upgrade must not be able to name a service, and its
	// interface carries nothing that can.
	HassReadiness(ctx context.Context, port int) ([]hass.Entry, error)
	MosquittoProbe(ctx context.Context, token string) (mosquitto.Sample, error)
	// HassNudge is the one call here that travels INTO a service rather than sampling it: a
	// Home Assistant that is already running has to be told that the node's offering changed,
	// because nothing else will restart it ([B.131]).
	HassNudge(ctx context.Context) (bool, error)
	// Snapshot/Restore are the {data} half of the rollback: a broken UPGRADE must put
	// the service's data subvolume back to its pre-upgrade point, not only take the service out
	// of the promoter chain. Fresh installs (no prior data) never call them.
	Snapshot(ctx context.Context, dataDir, dest, sidecar string) error
	// SupportsSnapshotMember gates the RING ([B.143]): an older guest advertises data.snapshot but
	// not data.member, and taking the old verb instead would leave an unlabelled member -- the one
	// thing the sidecar exists to prevent. Refused loudly, never worked around.
	SupportsSnapshotMember() bool
	// The QUIESCED take, for the one member taken while the service still RUNS ([B.143]): the
	// guest asks the service to hold still, snapshots, releases, and answers whether it held --
	// which is what the class is written from, by the guest, since the host cannot see it.
	QuiescedSnapshot(ctx context.Context, service, dataDir, dest, sidecar string) (bool, string, error)
	SupportsQuiescedSnapshot() bool
	// The RESTORE's three ([B.143]): list a service's ring, make one member's images resident
	// before anything is stopped, and put the data back. All capability-gated, because a guest
	// older than the ring can do none of them and a fallback would be worse than a refusal.
	Members(ctx context.Context, service string) ([]quadlet.SnapshotEntry, error)
	SupportsMembers() bool
	EnsureImage(ctx context.Context, ref string) error
	SupportsImageEnsure() bool
	// Restore materialises a member over the live subvolume, MINUS the markers a household's own
	// backup restore leaves in it ([B.143]) -- the guest's data.replace. RestoreWithoutSweep is
	// the frozen old verb, for the revert's one case where no rollback is worse than a stale
	// marker; nothing else may call it.
	Restore(ctx context.Context, dataDir, src string, sweep []string) error
	RestoreWithoutSweep(ctx context.Context, dataDir, src string) error
	SupportsRestoreSweep() bool
}

// installBudget bounds the whole operation. Generous, because a first install legitimately pulls
// images; bounded, because a wedged install must reach its revert rather than hang forever with
// the service half-installed.
const installBudget = 15 * time.Minute

// healthGate is how long the service gets to come up before the install is judged failed and the
// chain reverted. A container start plus an application's own boot; HA takes tens of seconds.
const healthGate = 5 * time.Minute

// revertBudget bounds the rollback, on its own DETACHED deadline. The health gate's most likely
// failure is the install budget expiring, and a revert inheriting that dead context could neither
// restore data nor put the prior manifest back — leaving the node half-reverted exactly when the
// install went wrong. So the undo path never shares a deadline with the thing it undoes.
const revertBudget = 3 * time.Minute

// ApplyServicePrewarm renders a catalogued service's units and pulls its images, and stops there
// . It is the warm-standby half on its own: no provisioning, no promoter change, no
// health gate — nothing that alters what this node is currently serving.
//
// It runs the same fetch/verify/render/warm prefix as an install, deliberately sharing the code
// rather than approximating it: a peer that warmed from a different rendering than the one the
// Primary installs is a peer that cannot take over, which is the exact failure this exists to
// prevent.
//
// ROLE-INDEPENDENT, and that is the point — for safety before speed. A secondary already did
// precisely this as a side effect of being handed an install it could not finish, which made the
// phase a function of the role. Roles are not ours to control (drbd-reactor promotes on quorum
// events), so that directive's MEANING could change between enqueue and execution: a node sent a
// "download" while Secondary, promoted before it got round to executing, would instead provision
// the volume, rewrite the promoter chain and health-gate itself — mid-failover, the worst possible
// moment to be handed an install nobody asked for. Naming the phase removes the race outright.
//
// The parallelism is the second reason, not the first: the Primary needs warming too, so every
// anchor pulls at once instead of the Primary pulling again after the peers have finished.
func (cfg Config) applyServicePrewarm(ctx context.Context, g serviceInstaller, d api.Directive, logf func(string, ...any)) api.DirectiveOutcome {
	failed := func(detail string) api.DirectiveOutcome {
		return api.DirectiveOutcome{ID: d.ID, State: api.OutcomeFailed, Detail: detail}
	}
	if d.Payload == "" {
		return failed("no service named")
	}
	parent := ctx
	ctx, cancel := cfg.beat.budget(parent, installBudget)
	defer func() { cancel() }()

	m, _, err := cfg.fetchManifest(ctx, d.Payload)
	if err == nil {
		// Same re-budget as the install ([V3b.31k]): a prewarm is the pull and nothing else.
		if b := installBudgetFor(m); b > installBudget {
			cancel()
			ctx, cancel = cfg.beat.budget(parent, b)
		}
	}
	if err != nil {
		logf("service prewarm %s: %v", d.Payload, err)
		return failed(err.Error())
	}
	// Images only: a prewarm is done by a node that is not serving, which has not allocated this
	// service an address — and does not need one to hold its images.
	rendered, err := quadlet.Images(m)
	if err != nil {
		return failed(err.Error())
	}
	// No `stale` list: a prewarm must not remove another service's units. Cleaning up a renamed
	// service's orphans is the install's job, on the node that is actually changing what serves.
	if err := g.ServiceRender(ctx, rendered.Files, nil); err != nil {
		return failed(fmt.Sprintf("render units: %v", err))
	}
	for _, u := range rendered.ImageUnits {
		// Ensure-present, not start: starting an .image unit is a registry pull, so an image that
		// is already here (prewarmed, or staged into the guest at build time) must not be fetched
		// again -- and for a locally-staged one there is no registry to fetch it FROM. Same verb,
		// same reason, as bring-up ([V3b.3](e1)).
		if err := g.ServiceWarm(ctx, u, rendered.ImageRefs[u]); err != nil {
			return failed(fmt.Sprintf("warm image (%s): %v", u, err))
		}
	}
	logf("service prewarm %s: %d units rendered, %d images warmed; nothing promoted", m.Name, len(rendered.Files), len(rendered.ImageUnits))
	return api.DirectiveOutcome{ID: d.ID, State: api.OutcomeDone, Detail: "prewarmed"}
}

// ApplyServiceInstall installs — or UPGRADES — the catalogued service named by the directive
// service, returning the terminal outcome (announce-before-act). Install and upgrade are the
// SAME path: install is an upgrade from "empty" to the target. Idempotent by construction —
// re-installing the same manifest re-renders identical units and reuses the existing subvolume.
//
// The failure contract is the point of the path: a target that never
// becomes healthy leaves the node serving what it served before. For a fresh install that is the
// empty zero-service chain; for an UPGRADE it is the prior manifest AND its data — the service-level
// twin of the {code+data} OS rollback. So the path snapshots the data before the switch and, on
// a tripped gate, restores both the subvolume and the prior manifest, not just the promoter chain.
func (cfg Config) applyServiceInstall(ctx context.Context, g serviceInstaller, d api.Directive, logf func(string, ...any)) api.DirectiveOutcome {
	failed := func(detail string) api.DirectiveOutcome {
		return api.DirectiveOutcome{ID: d.ID, State: api.OutcomeFailed, Detail: detail}
	}
	if d.Payload == "" {
		return failed("no service named")
	}
	// FIRST, before the fetch, so a refusal costs nothing and leaves the node untouched. A guest
	// that does not advertise service.installed keeps the services' identities in ONE file on the
	// volume, so it cannot say which service a manifest belongs to — installing here would record
	// this service's identity over whatever that node already runs, and a survivor would then
	// promote against the wrong manifest. Refusing loudly is the answer rather than a compat path
	// ([[alpha-reinstall-only-policy]]); the guest OS rolls and the verb appears.
	if !g.SupportsServiceInstalled() {
		return failed("this guest is too old to name a service's identity on the volume (no service.installed); update the guest OS before installing")
	}
	// Same instrument, same reason, for converge ([V3b.3](f)). A guest without service.converge
	// has no briard-services unit either, so nothing there ever reads the volume — this would
	// write the manifest, report success, and leave the node serving exactly what it served
	// before. No compat path ([[alpha-reinstall-only-policy]]); the guest OS rolls and the verb
	// appears.
	if !g.SupportsServiceConverge() {
		return failed("this guest is too old to converge itself to the volume (no service.converge); update the guest OS before installing")
	}
	// Same instrument again, for the ring ([B.143]). It is the capability rather than a protocol
	// floor DELIBERATELY: a floor makes every host refuse every not-yet-rolled guest fleet-wide
	// and its own health gate then reverts the self-update, which gate 3 measured on 2026-09-22 --
	// the node could not reach the image that would have fixed it, because reaching it needs the
	// channel the floor had just closed. This refuses exactly the one path that needs the verb.
	if !g.SupportsSnapshotMember() {
		return failed("this guest is too old to take a titled rollback point (no data.member); update the guest OS before installing")
	}
	parent := ctx
	ctx, cancel := cfg.beat.budget(parent, installBudget)
	defer func() { cancel() }()

	m, raw, err := cfg.fetchManifest(ctx, d.Payload)
	if err != nil {
		logf("service install %s: %v", d.Payload, err)
		return failed(err.Error())
	}
	// The manifest says how long its pull may take ([V3b.31k]); an install must not expire
	// before the pull it waits on is allowed to. Re-budgeted from the parent, and the deferred
	// cancel is a closure so it releases whichever lease is current.
	if b := installBudgetFor(m); b > installBudget {
		cancel()
		ctx, cancel = cfg.beat.budget(parent, b)
	}
	// THE FREE-SPACE GATE ([V3b.31j]), before the first byte moves: the manifest says what the
	// image store will hold once pulled, the guest says what its store's filesystem has free,
	// and a pull that could only end in a full disk is refused here with both numbers -- rather
	// than at 90 % of the download with a unit that failed for a reason nobody can read. The
	// margin is the headroom the guest OS itself needs to keep working next to the image.
	if m.InstalledSize > 0 && g.SupportsStorageFree() {
		free, total, err := g.StorageFree(ctx)
		if err != nil {
			logf("service install %s: cannot measure the image store (%v); installing unmeasured", d.Payload, err)
		} else if free < m.InstalledSize+storageHeadroom {
			return failed(fmt.Sprintf("not enough space on the node: %s needs %s installed and the image store has %s free of %s (keeping %s for the system)",
				m.Name, gb(m.InstalledSize), gb(free), gb(total), gb(storageHeadroom)))
		}
	}
	// No address: this render supplies unit names and image digests, and converge re-renders with
	// the allocated one before anything starts (see Render).
	rendered, err := quadlet.Render(m, "")
	if err != nil {
		return failed(err.Error())
	}

	// Interim guard. Checked BEFORE anything is written, so a refusal leaves the node untouched.
	active, err := g.ReactorActive(ctx)
	if err != nil {
		return failed(fmt.Sprintf("could not read promoter state: %v", err))
	}
	if !active {
		return failed("the promoter is already paused — another maintenance operation is in progress")
	}

	// N SERVICES, and everything below is per-service by construction: the node-local cache is a
	// directory ([V3b.3](a)), the volume's identity file is one per name, `priorService` asks
	// about THIS service, `filesToRemove` compares THIS service's renderings, and the chain is
	// assembled from all of them. What used to stand here was a refusal of a second distinct
	// service, and it was load-bearing for exactly as long as nothing could NAME a service through
	// a seam: a node running two and describing one would have had the cloud confirm a rollout
	// against whichever came first and a crash-loop in the other go unseen ([V3b.3](b)).
	//
	// It is gone because all three of those are now plural: `NodeStatus.Services` carries one
	// manifest identity per service, the volume's `.services/<name>.json` says which service a
	// manifest belongs to, and `telemetry.NodeResources.Payloads` measures each service's own
	// footprint and restart count. `Healthy` and `System` deliberately stayed node-scoped — a
	// closure is a property of the node, and the front door is what "is this node serving" means.

	// What is installed NOW — the rollback target. nil on a fresh install (the shipped
	// zero-service node) or an idempotent re-install of the same manifest. Read BEFORE
	// ServiceProvision overwrites the volume's manifest, so a failed upgrade can put the prior back.
	prior, priorSubdirs, priorRaw, priorVersion := cfg.priorService(ctx, g, m.Name, raw, logf)

	// Units are node-local (/run), so this node renders its own. A multi-node home has the
	// directive delivered to EVERY node, and each renders locally — that is what lets a survivor
	// start the pod. The volume holds the manifest as the identity they all render from. Stale
	// names a renamed prior service's orphan files, removed so a survivor cannot resurrect them.
	var stale []string
	if prior != nil {
		stale = filesToRemove(prior.Files, rendered.Files)
	}
	if err := g.ServiceRender(ctx, rendered.Files, stale); err != nil {
		return failed(fmt.Sprintf("render units: %v", err))
	}

	// WARM THE IMAGES, synchronously, here. The .image units are WantedBy=multi-user.target —
	// boot-time — so a service installed AFTER boot would never have them run, and the
	// containers' Pull=never would then fail the whole chain at promotion. Install time is also
	// the right moment on its own terms: it is where a human is watching and the CLI can report
	// the failure, whereas the failover path must never pull. The boot-time unit
	// stays what it is — a RE-warm on subsequent boots, not the primary fetch.
	//
	// This is also the one point where an offline node legitimately fails: `briard service
	// install` needs the network, while running an installed service never does.
	//
	// The pull record for the dashboard's bar ([V3b.31j]): the totals before the first byte,
	// cleared after the last -- on every exit, because a stale record would draw a bar over an
	// install that ended. Best-effort: a bar is a courtesy, never a gate on the install.
	if m.Size > 0 && g.SupportsServicePulling() {
		if err := g.ServicePulling(ctx, m.Name, m.Size, m.InstalledSize); err != nil {
			logf("service install %s: pull record: %v (no progress bar)", m.Name, err)
		}
		defer func() {
			if err := g.ServicePulled(context.WithoutCancel(ctx), m.Name); err != nil {
				logf("service install %s: clear pull record: %v", m.Name, err)
			}
		}()
	}
	for _, u := range rendered.ImageUnits {
		// Ensure-present, not start: starting an .image unit is a registry pull, so an image that
		// is already here (prewarmed, or staged into the guest at build time) must not be fetched
		// again -- and for a locally-staged one there is no registry to fetch it FROM. Same verb,
		// same reason, as bring-up ([V3b.3](e1)).
		if err := g.ServiceWarm(ctx, u, rendered.ImageRefs[u]); err != nil {
			return failed(fmt.Sprintf("warm image (%s): %v", u, err))
		}
	}

	// The subvolume and the manifest live on the replicated volume, which only the Primary has
	// mounted. A secondary legitimately stops here: it has its units and will get the data by
	// replication.
	qs, err := g.Status(ctx, cfg.Resource.Name)
	if err != nil {
		return failed(fmt.Sprintf("read node status: %v", err))
	}
	if !qs.Serving() {
		logf("service install %s: units rendered; not serving, so no provisioning here", m.Name)
		return api.DirectiveOutcome{ID: d.ID, State: api.OutcomeDone, Detail: "units rendered (not serving)"}
	}

	dataDir := quadlet.DataRoot(m.Name)

	// CAPTURE THE S1 BASELINE FIRST, while the OLD version is still serving and still untouched.
	//
	// It is a DIFFERENTIAL gate, so this sample is the whole confounder control: an integration
	// already broken before the upgrade is excluded, and only what this change breaks can trip it.
	// A sample taken any later is a sample of something already disturbed.
	//
	// ⚠️ THIS LINE MUST STAY ABOVE THE STOP AND THE SNAPSHOT that [B.143] put below it: a baseline
	// captured after a stop is a baseline of a service that is not running, which reads as every
	// integration having regressed. TestUpgradeCapturesTheBaselineBeforeTheSnapshot asserts the
	// order by name.
	//
	// ONLY ON AN UPGRADE. A fresh install has nothing to differ against — the service was not
	// running, so there is no "was loaded" to regress from — and the floor is the honest gate
	// there. Deliberate rather than incidental: it is also the case where a Baseline call would
	// be asking a service that does not exist yet.
	gate := guest.Gate{Assessor: cfg.assessorFor(m, g, logf), Logf: logf}
	var readiness guest.Readiness
	if prior != nil {
		readiness = gate.Capture(ctx)
	}

	// STOP THE SERVICE, THEN SNAPSHOT THE ROLLBACK POINT — [B.143], landing [B.121]'s ruling.
	// Its data is what a broken upgrade can poison, and the read-only snapshot on the replicated
	// volume is what a failed gate restores.
	//
	// IT USED TO BE TAKEN LIVE, and `services-pair.nix` measured what that costs: mosquitto holds
	// retained state in memory and writes it on a clean stop, so a snapshot of a running broker
	// did not contain the message it had already accepted — the upgrade carried that message
	// across and the rollback then restored a broker with nothing in it ([V3b.4](c), 2026-08-30).
	// A live snapshot is only crash-consistent, and Home Assistant surviving one (btrfs snapshots
	// atomically, HA replays its WAL on open) is a measured fact about HA, not a guarantee the
	// catalog can make. Stopping first makes the point application-consistent by construction,
	// for every service, and it is the same stop the rollback path needs anyway — `data.restore`
	// cannot run until the subvolume's bind is released.
	//
	// CONTAINER UNITS, never the pod: quiesce's own comment carries the full trace of why. Only
	// the upgraded service stops, so the node keeps serving every other one.
	//
	// THE UNDO IS A RE-CONVERGE, and it needs no new mechanism. Until ServiceProvision below
	// overwrites the volume's manifest, the volume still names the PRIOR service — so a converge
	// re-renders from it and starts exactly what the stop above stopped (converge starts every
	// unit of every service it does not skip, whether or not its bytes changed, so a unit stopped
	// out from under it comes back). That is why the two steps below undo rather than abandon:
	// B.121's ruling is that a failed upgrade snapshot INTERRUPTS the service, and an interruption
	// the household is never brought out of is just an outage with a directive attached.
	var snap string
	stoppedFail := failed
	if prior != nil {
		cfg.quiesce(ctx, g, prior.ContainerUnits, logf)
		stoppedFail = func(detail string) api.DirectiveOutcome {
			if _, err := g.ServiceConverge(ctx); err != nil {
				return failed(fmt.Sprintf("%s; AND the service could not be restarted: %v", detail, err))
			}
			return failed(detail + " (the service was restarted)")
		}
		// THE MEMBER IS PINNED TO THE VERSION IT IS TAKEN UNDER, which is v1 by construction —
		// it is taken before the switch, so the manifest recorded here is the one still on the
		// volume and the one a revert re-provisions. v2's own first member will pin v2, leaving
		// two members seconds apart with near-identical data and unambiguous meanings: this one
		// goes back to v1, that one goes to v2 as of then. Both are kept; the duplication is not
		// fought, and no "who wrote the data" bookkeeping is needed ([B.143]).
		//
		// The title is what the picker shows, so it names the two VERSIONS rather than the
		// trigger the name already carries.
		at := cfg.takenAt()
		snap = quadlet.SnapshotMember(m.Name, quadlet.TriggerUpgrade, at)
		meta := quadlet.SnapshotMeta{
			Service: m.Name,
			Trigger: quadlet.TriggerUpgrade,
			Title:   fmt.Sprintf("%s, before upgrading to %s", priorVersion, m.Version),
			TakenAt: at,
			// QUIESCED BY THE STOP ABOVE, which is what [B.121] bought and the reason that
			// stop is not an optimisation somebody may reorder away.
			Consistency: quadlet.Quiesced,
			Manifest:    priorRaw,
			// THE UPDATE'S EVENT, on the point that undoes it ([B.167]). Written before the act
			// because the point is; a revert below records its own undo rather than rewriting this.
			Event: &quadlet.Event{Kind: quadlet.EventUpdate, At: at, What: "Updated to " + m.Version},
		}
		sidecar, err := json.Marshal(meta)
		if err != nil {
			return stoppedFail(fmt.Sprintf("render the rollback point's sidecar: %v", err))
		}
		if err := g.Snapshot(ctx, dataDir, snap, string(sidecar)); err != nil {
			return stoppedFail(fmt.Sprintf("snapshot rollback point: %v", err))
		}
	}

	// Provision writes the NEW manifest to the volume + ensures the subvolume/subdirs. On an
	// upgrade this overwrites the prior manifest on the volume; the rollback re-writes the prior.
	//
	// A FAILURE HERE ALSO CONVERGES, and which service comes back depends on how far it got: the
	// manifest is written last (provisionService, agent/guestagent), so a failure before that
	// leaves the prior one on the volume and converge restores it exactly. A failure after it
	// brings up the NEW service, ungated — which is why the outcome is still `failed` and says
	// only that the service was restarted, never that the install stood. Either way the household
	// ends with a running service, which is the property worth having; before the stop above
	// existed this path could afford to report and leave everything alone.
	if err := g.ServiceProvision(ctx, m.Name, dataDir, quadlet.Subdirs(m), string(raw)); err != nil {
		return stoppedFail(fmt.Sprintf("provision storage: %v", err))
	}

	// CONVERGE, in place. Everything past here must return the node to the prior service, hence
	// the explicit revert on each failure path rather than a defer that would also fire on
	// success.
	//
	// THERE IS NO MAINTENANCE BRACKET ANY MORE, and its absence is the point ([V3b.3](f)). This
	// used to pause the promoter, quiesce the containers, rewrite the start-list and resume —
	// because the services WERE chain members and changing what a live promoted resource runs
	// meant editing the list it was promoted with. With a static chain there is nothing to
	// rewrite: an install is provision + tell the node to re-read the volume. So installing a
	// service no longer stops the promoter for every OTHER service on the node, and a failure
	// here can no longer demote it.
	//
	// A VERB, not a `systemctl restart briard-services`: briard-services is itself a chain
	// member, so stopping it would deactivate drbd-reactor's target, unmount the volume and
	// demote the node — the exact accident the bracket existed to prevent, re-created by the
	// obvious way of triggering a re-converge. Same code, same unit, no unit lifecycle touched.
	revert := func(cause error) api.DirectiveOutcome {
		return cfg.revert(ctx, g, d, m.Name, dataDir, rendered, prior, priorSubdirs, priorRaw, snap, logf, cause)
	}
	skipped, err := g.ServiceConverge(ctx)
	if err != nil {
		return revert(fmt.Errorf("install %s: %w", m.Name, err))
	}
	// CONVERGE CONTAINS A PREPARATION FAILURE TO THE ONE SERVICE, so it succeeds while declining
	// to start ours — the right answer for a promoting node, and the wrong one to report as an
	// install. Without this the old container would still be serving, the health gate below would
	// pass on it, and the install would report the new version as running.
	if slices.Contains(skipped, m.Name) {
		return revert(fmt.Errorf("install %s: the node could not prepare it and did not start it (see the guest log)", m.Name))
	}
	logf("service install %s: converged from the volume; waiting for health", m.Name)

	// Gate on the SERVICE's own endpoint (in-guest), not the front door — see awaitHealthy. BY
	// NAME: the converge above wrote the routing table, so the guest already knows where this
	// service listens, and the host no longer assembles an address it can only guess stays true.
	if err := cfg.awaitHealthy(ctx, g, m.Name); err != nil {
		logf("service install %s failed its health gate (%v); reverting", m.Name, err)
		return revert(err)
	}
	// THE FLOOR HELD; now ask whether the service still does what it did. The floor's whole
	// blind spot is a service that answers while its work is broken — Home Assistant returning
	// 200 for /manifest.json with half the house's integrations dead — and this is the layer
	// that sees it. A Rollback verdict reverts {code + data} exactly as a failed floor does; a
	// Hold keeps the upgrade and says so; a Pass, a missing assessor, a fresh install or a
	// failed sample all keep it silently, because S1 must never revert a household's service on
	// the strength of its own telemetry breaking.
	//
	// ⚠️ A Hold currently reaches only the log. On the free tier that is nobody ([B.119] owns
	// the surface it needs); the rollback window and the user remain the backstop until then.
	if err := gate.Judge(ctx, readiness); err != nil {
		logf("service install %s failed the readiness gate (%v); reverting", m.Name, err)
		return revert(err)
	}
	// Record the manifest NODE-LOCALLY as well as on the volume. Both copies are needed and they
	// do different jobs: the volume's is the replicated identity (what the service IS), while
	// this one is what lets the agent rebuild the promoter chain at BRING-UP — before promotion,
	// when the volume is not yet mounted and the replicated copy is therefore unreadable. Without
	// it, an agent restart would re-derive the chain from the environment, silently dropping the
	// installed service back out of it. Same pattern, and the same reason, as AssignmentCache.
	if err := cfg.cacheService(m.Name, raw); err != nil {
		// Not fatal: the service IS installed and serving. Say so loudly though — the node will
		// lose it from the chain on the next agent restart.
		logf("service install %s: WARNING: could not cache the manifest (%v); an agent restart will drop it from the promoter chain", m.Name, err)
	}
	// TELL A HOME ASSISTANT THAT IS ALREADY RUNNING ([B.131]). Everything briard's integration
	// does, it does at an HA start, and converge restarts only the services whose bytes changed
	// ([V3b.3](f)) -- so installing the broker beside a live Home Assistant leaves it running and
	// unwired until something happens to restart it. Nothing else in the product will.
	//
	// UNCONDITIONAL, and that is the cheap half of the design: no "which service implies what"
	// table here, no check of what else is installed. The guest answers `false` when there is no
	// Home Assistant to tell, and the signal carries nothing, so firing it after an install that
	// Home Assistant does not care about costs one loopback request and changes nothing.
	//
	// AFTER THE GATES, so a reverted install never fires it, and after the health gate the broker
	// is already accepting clients -- the integration's own socket check is what would otherwise
	// have to lose that race.
	//
	// BEST-EFFORT, NEVER FATAL: the service IS installed and serving. A failure here costs the
	// household exactly what they had before this existed, which is a Home Assistant that picks
	// the change up at its next start.
	if told, err := g.HassNudge(ctx); err != nil {
		logf("service install %s: could not tell Home Assistant to reconsider (%v); it will pick this up at its next start", m.Name, err)
	} else if told {
		logf("service install %s: Home Assistant was told to reconsider what the node offers", m.Name)
	}
	logf("service install %s: healthy, serving", m.Name)
	// WHERE TO REACH IT, carried back as the outcome Detail. The node holds both halves and the
	// operator holds neither: the port is the manifest's (never typed by a human) and the name is
	// the one the guest publishes over mDNS. Without it the verb reports success and leaves the
	// address to be guessed, which walks the user to the front door's "nothing is routed here"
	// page and reads a working install as a broken one.
	//
	// The sentence itself is the registry's (agent/services), because it is not the same sentence
	// for every service: the manifest's port is what the liveness floor probes, which for a
	// broker is a management endpoint on the guest's loopback rather than anything a household
	// opens.
	reach := services.Reach(m, cfg.FlockName)
	return api.DirectiveOutcome{ID: d.ID, State: api.OutcomeDone, Detail: reach}
}

// CacheService writes one service's manifest into the node-local cache directory, named for the
// service. Written only after the health gate passes, so a failed install is never the thing a
// restart converges to.
//
// One file per service, holding the manifest's bytes VERBATIM ([V3b.3](a)). The alternative --
// one file holding the set -- cannot work: a manifest's content hash IS the service identity
// (shared/manifest), so re-encoding a set would give every member a new identity on every write.
// Writing per file also makes install and uninstall a file operation instead of a read-modify-write
// of a shared document, which is what keeps two installs from losing each other's service.
//
// Atomic and durable (shared/atomicfile), which it was not: a bare WriteFile over an existing file
// truncates first, so a crash inside the write left half a manifest, and an unflushed one left no
// manifest at all for a commit interval after the install returned. Neither is loud.
// installedServices reads an unparseable file as "this service is not installed" and says so once
// to the log, while the volume's manifests still name the services the node is meant to be
// running -- bring-up never consults those (only the install path does, via priorService), so the
// node just quietly comes back without it. Same defect as [V3.23], node-local instead of replicated.
//
// The service specs used to be described here as "the one input to BringUp that comes from a file
// instead of being re-derived by the host". They were not: the MESH is the other one, and it had no
// cache at all, so a runtime-paired node's guest reboot rewrote its .res from the stale PEERS env
// ([V3b.16b]). Two node-scoped facts, two caches, one rule -- see cacheMesh.
func (cfg Config) cacheService(name string, raw []byte) error {
	if cfg.ServiceCache == "" {
		return nil
	}
	return atomicfile.Write(cfg.manifestPath(name), raw, 0o600, 0o700)
}

// manifestPath is one service's file in the cache directory. The name is a manifest slug --
// shared/manifest's Validate refuses anything else, and refuses it precisely because these become
// path elements -- so there is no traversal to strip here; the check upstream is the one that
// matters and this would only duplicate it badly.
func (cfg Config) manifestPath(name string) string {
	return filepath.Join(cfg.ServiceCache, name+".json")
}

// adoptInstalledServices refreshes the LIVE config from the node-local manifest cache after a
// directive that changed what is installed. Run() does this once at startup; without it here, the
// very process that performed the install kept its old (usually empty) view of cfg.Services until
// something restarted it.
//
// That is not cosmetic: cfg.resources() is gated on there being a service, so a node that had just
// installed one stopped reporting the guest's telemetry ENTIRELY -- not merely the service
// footprint but volume usage, snapshot count, load average, journal size and podman-store size,
// every one of them a soak trend input or a growth surface. The node ran perfectly and described
// itself as empty.
//
// Pointer receiver on purpose: it mutates the observe loop's own cfg, the copy every later cycle
// reads. A failed re-read leaves the previous view in place -- the cache is the authority on what
// is installed, and a transient read error is not evidence of an uninstall.
func (cfg *Config) adoptInstalledServices(d api.Directive, o api.DirectiveOutcome, logf func(string, ...any)) {
	if d.Kind != api.DirectiveServiceInstall || o.State != api.OutcomeDone {
		return
	}
	specs, rendered, ok := cfg.installedServices(logf)
	if !ok {
		return
	}
	// Not cfg.Promoter: the chain is static ([V3b.3](f)), and an install that changed it would be
	// the node-was-told model this item removed.
	cfg.Services, cfg.ServiceRendered = specs, rendered
	for _, spec := range specs {
		logf("installed service %q adopted into the running config (serving unit %s)", spec.Name, spec.ServingUnit())
	}
}

// InstalledServices reads the node-local manifest DIRECTORY and returns the service specs plus the
// UNION of their RENDERED UNITS. Called at bring-up so a restarted agent knows what it runs,
// rather than reverting to whatever the environment describes.
//
// NO CHAIN COMES BACK, because a service is not a chain member: the promoter chain is the same
// three units everywhere and `briard-services` starts the services from the volume once the mount
// exists ([V3b.3](f)). It used to return one, assembled from the services' units, and nothing had
// read it since that landed.
//
// The rendered output is returned rather than discarded because the chain alone is not enough
// . The units it names live under /run/containers/systemd — tmpfs — so a guest reboot
// erases them while this cache survives, leaving a promoter chain pointing at unit files that do
// not exist. restoreService puts them back; see there for why re-rendering is the canonical
// answer rather than persisting them somewhere durable.
//
// ORDER IS BY FILENAME, which is by service name, which makes it deterministic and nothing more.
// The chain is a start ORDER and a set of services has a real one — [V3b.3](c) turns it into a
// dependency order once there is a second real service to decide against. Until then, "stable
// across restarts" is the only property this owes, and alphabetical is the cheapest way to owe it.
//
// Every failure is a soft "not installed", per service and for the set: a cache directory that is
// absent (the shipped zero-service node), unreadable, or holding a file that no longer parses must
// leave the node bringing up with what remains rather than refusing to start. A node that will not
// boot is worse than a node that lost a service — and losing ONE service must not cost the others,
// which is why a bad file is skipped rather than failing the read.
func (cfg Config) installedServices(logf func(string, ...any)) ([]model.ServiceSpec, quadlet.Rendered, bool) {
	if cfg.ServiceCache == "" {
		return nil, quadlet.Rendered{}, false
	}
	entries, err := os.ReadDir(cfg.ServiceCache)
	if err != nil {
		return nil, quadlet.Rendered{}, false // absent = nothing installed, the shipped state
	}
	var (
		specs []model.ServiceSpec
		all   = quadlet.Rendered{Files: map[string]string{}}
	)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		path := filepath.Join(cfg.ServiceCache, e.Name())
		raw, err := os.ReadFile(path)
		if err != nil {
			logf("installed-service cache %s is unreadable (%v); bringing up without it", path, err)
			continue
		}
		spec, rendered, err := specOf(raw)
		if err != nil {
			logf("installed-service cache %s is unusable (%v); bringing up without it", path, err)
			continue
		}
		specs = append(specs, spec)
		mergeRendered(&all, rendered)
	}
	if len(specs) == 0 {
		return nil, quadlet.Rendered{}, false
	}
	return specs, all, true
}

// specOf turns one manifest's bytes into the spec the node reports and the units it runs. ONE
// implementation, because the two readers must agree about what a manifest means: the node-local
// cache (installedServices, what this host was told) and the replicated volume
// (volumeServices, what this node actually promoted into).
func specOf(raw []byte) (model.ServiceSpec, quadlet.Rendered, error) {
	m, id, err := manifest.Parse(raw)
	if err != nil {
		return model.ServiceSpec{}, quadlet.Rendered{}, err
	}
	rendered, err := quadlet.Render(m, "")
	if err != nil {
		return model.ServiceSpec{}, quadlet.Rendered{}, err
	}
	primary := m.Primary()
	return model.ServiceSpec{
		Name:    m.Name,
		DataDir: quadlet.DataRoot(m.Name),
		Units:   rendered.Units,
		Unit:    quadlet.ContainerName(m.Name, primary.Name) + ".service",
		// The identity of the bytes as read, not of a re-marshalling of them -- Parse returns it
		// here for free, which is why the report reads it from the spec.
		Manifest: string(id),
	}, rendered, nil
}

// volumeReader is the slice of the guest adoptVolumeServices needs: what the replicated volume
// says this node runs. Narrow interface for DI, not a seam.
type volumeReader interface {
	ServiceList(ctx context.Context) ([]string, error)
	ServiceInstalled(ctx context.Context, name string) (string, error)
	SupportsServiceList() bool
}

// adoptVolumeServices makes this host's view of what it runs match the VOLUME, and caches it.
//
// WHY IT EXISTS, measured rather than reasoned (a fleet run, 2026-08-28): converge-at-promotion
// ([V3b.3](f)) renders and starts from the volume, so a survivor that never installed anything
// runs services this host was never told about. The report is built from the node-local cache,
// which only a completed install ON A PRIMARY writes -- so the survivor served the upgraded
// fixture at the VIP, with its tick counter moving, while telling the cloud it ran no services at
// all. The cloud could not have confirmed any rollout on it, and a household degraded to "no
// services" in every view while being perfectly healthy. Converge made the volume the truth about
// what runs; this makes the host READ that truth instead of remembering what it was told.
//
// ON THE PROMOTION EDGE, not every cycle: the volume is only readable by whoever holds it, and
// what it says changes only when someone installs. Polling it would add a verb round-trip per
// status cycle to answer a question whose answer just changed exactly once.
//
// It CACHES what it finds, which is what keeps the node's own restart correct: bring-up reads the
// node-local cache, and a node that promoted into somebody else's install would otherwise come
// back empty. The cache becomes a projection of the volume rather than a record of what this host
// did -- which is what converge already made true of the units themselves.
//
// Every failure is soft. A guest too old to list (no service.list) leaves the previous view
// standing, as does a read error or a manifest that will not parse: this is a REPORT, and a
// wrong-but-quiet report is worse than a stale one only if it is silently wrong, which the log
// prevents. The node keeps serving either way -- what runs is the guest's business.
func (cfg *Config) adoptVolumeServices(ctx context.Context, g volumeReader, logf func(string, ...any)) {
	if !g.SupportsServiceList() {
		return // an older guest; the node-local cache is all this host can honestly report
	}
	names, err := g.ServiceList(ctx)
	if err != nil {
		logf("could not read the volume's services (%v); still reporting what this node installed", err)
		return
	}
	var (
		specs []model.ServiceSpec
		all   = quadlet.Rendered{Files: map[string]string{}}
	)
	for _, name := range names {
		raw, err := g.ServiceInstalled(ctx, name)
		if err != nil || raw == "" {
			logf("the volume names %q but its manifest could not be read (%v); reporting without it", name, err)
			continue
		}
		spec, rendered, err := specOf([]byte(raw))
		if err != nil {
			logf("the volume's manifest for %q is unusable (%v); reporting without it", name, err)
			continue
		}
		specs = append(specs, spec)
		mergeRendered(&all, rendered)
		// Durable, so this node's own restart brings up what it is actually running rather than
		// what it once installed. A cache write that fails is not fatal to the report.
		if err := cfg.cacheService(name, []byte(raw)); err != nil {
			logf("could not cache the volume's manifest for %q (%v); this node's next bring-up will not know about it", name, err)
		}
	}
	if same(cfg.Services, specs) {
		return
	}
	cfg.Services, cfg.ServiceRendered = specs, all
	logf("adopted %d service(s) from the volume: %s", len(specs), names)
}

// same reports whether two service sets name the same services at the same identities -- the
// question "did anything change?", not deep equality of the rendered units, which are derived.
func same(a, b []model.ServiceSpec) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Name != b[i].Name || a[i].Manifest != b[i].Manifest {
			return false
		}
	}
	return true
}

// mergeRendered folds one service's rendering into the union restoreService replays. Unit
// filenames are service-prefixed by the renderer (briard-<service>-…), so the Files merge cannot
// collide; the unit lists concatenate in the order their services were read.
func mergeRendered(all *quadlet.Rendered, r quadlet.Rendered) {
	for name, body := range r.Files {
		all.Files[name] = body
	}
	all.Units = append(all.Units, r.Units...)
	all.ImageUnits = append(all.ImageUnits, r.ImageUnits...)
	for unit, ref := range r.ImageRefs {
		if all.ImageRefs == nil {
			all.ImageRefs = map[string]string{}
		}
		all.ImageRefs[unit] = ref
	}
	all.ContainerUnits = append(all.ContainerUnits, r.ContainerUnits...)
}

// PriorService reads the manifest currently on the volume FOR THIS SERVICE and renders it — the
// rollback target for an upgrade. Naming the service is what makes it correct at N>1: reading the
// volume's single unnamed manifest meant installing a second service found the FIRST one's
// manifest, called it this install's prior, and then had filesToRemove delete that service's
// rendered units as a renamed prior's orphans ([V3b.3](b)).
//
// Returns (nil, nil, "") for a fresh install, an idempotent re-install of the same
// manifest, or a prior that no longer parses/renders (no usable rollback target → treated as
// fresh; the gate still guards the new service, so the worst case is a rollback to empty rather
// than to the broken new one, never to it). The raw bytes and subdirs come back too, because the
// rollback re-provisions them as the volume's identity.
// It also returns the prior manifest's VERSION, which is not derivable from the rendering: it is
// what the rollback point's title names ("2026.7.1, before upgrading to 2026.8.0"), and the
// picker shows titles rather than paths ([B.143]).
func (cfg Config) priorService(ctx context.Context, g serviceInstaller, name string, incoming []byte, logf func(string, ...any)) (*quadlet.Rendered, []string, string, string) {
	raw, err := g.ServiceInstalled(ctx, name)
	if err != nil {
		logf("service install: could not read the installed manifest (%v); treating as a fresh install", err)
		return nil, nil, "", ""
	}
	if raw == "" || raw == string(incoming) {
		return nil, nil, "", ""
	}
	pm, _, perr := manifest.Parse([]byte(raw))
	if perr != nil {
		logf("service install: installed manifest does not parse (%v); no rollback target", perr)
		return nil, nil, "", ""
	}
	pr, rerr := quadlet.Render(pm, "")
	if rerr != nil {
		logf("service install: installed manifest does not render (%v); no rollback target", rerr)
		return nil, nil, "", ""
	}
	return &pr, quadlet.Subdirs(pm), raw, pm.Version
}

// takenAt is the moment a ring member is named for. See Config.clock.
func (cfg Config) takenAt() time.Time {
	if cfg.clock != nil {
		return cfg.clock()
	}
	return time.Now()
}

// filesToRemove lists filenames in `have` that `want` does not also write. Used both ways: forward
// (drop a renamed prior service's orphans) and on revert (drop the new service's orphans). A
// same-name upgrade shares every filename, so the list is empty and the content is simply
// overwritten.
func filesToRemove(have, want map[string]string) []string {
	var out []string
	for name := range have {
		if _, ok := want[name]; !ok {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// Quiesce stops the given CONTAINER units, best-effort — a unit that is not running is not a
// failure worth aborting for.
//
// NO PROMOTER PAUSE. It used to be called inside the maintenance bracket so the stop was not read
// as a fault; with the service units out of the chain ([V3b.3](f)) the promoter has no opinion
// about them at all, and `revert` below says the same thing at more length. The stop is also what
// QUIESCES: it is not a flush or a checkpoint, it is SIGTERM through podman, and a well-behaved
// service writes what it was holding on the way down — which is exactly what mosquitto's retained
// message needed and did not get while the rollback point was taken live ([B.143]).
//
// It must be handed CONTAINER units, never the pod. The container holds the data Volume bind, so a
// clean `systemctl stop` of it releases the bind (what data.restore needs) AND leaves the
// drbd-reactor target up. Stopping the POD instead makes podman kill the containers ungracefully →
// their units go to `failed` → the target `Requires=` a failed member → the target deactivates →
// `briard-primary-storage` (PartOf) unmounts the SHARED DRBD volume, taking every other service with it. The
// full trace is on quadlet.Rendered.ContainerUnits. The pod carries no data bind, so a data op
// never needs to stop it.
func (cfg Config) quiesce(ctx context.Context, g serviceInstaller, containerUnits []string, logf func(string, ...any)) {
	for i := len(containerUnits) - 1; i >= 0; i-- {
		if err := g.ServiceStop(ctx, containerUnits[i]); err != nil {
			logf("service switch: stop %s: %v (continuing)", containerUnits[i], err)
		}
	}
}

// Revert returns the node to the service it ran before this install — the prior manifest and its
// data (an upgrade), or nothing at all (a fresh install) — and reports the terminal outcome. It
// runs on a DETACHED, freshly-budgeted context (revertBudget): the most likely trigger is the
// health gate's deadline expiring, and a revert inheriting that dead deadline could restore
// nothing.
//
// Stop the broken service -> restore the data -> put the prior manifest back on the volume ->
// converge, which re-renders from that manifest and starts the prior version fresh. Data is
// restored BEFORE code, the order the guest manager's rollback uses.
//
// NO PROMOTER PAUSE, and the safety it used to buy is bought more cheaply now ([V3b.3](f)). The
// old rollback ran under one pause so the promoter could not restart the broken service between
// steps; with the services out of the chain the promoter has no opinion about them at all, and
// the units are stopped here by name. The failed-restore case improves outright: it used to
// leave the promoter PAUSED — a node that will not fail over — rather than resume onto poisoned
// data. Now it simply does not converge, so the one service stays stopped and the node keeps
// serving everything else and keeps its ability to fail over. Smaller blast radius, same refusal.
func (cfg Config) revert(ctx context.Context, g serviceInstaller, d api.Directive, name, dataDir string, next quadlet.Rendered, prior *quadlet.Rendered, priorSubdirs []string, priorRaw, snap string, logf func(string, ...any), cause error) api.DirectiveOutcome {
	rctx, rcancel := cfg.beat.budget(context.WithoutCancel(ctx), revertBudget)
	defer rcancel()

	bothFailed := func(what string, err error) api.DirectiveOutcome {
		// The install AND its revert failed — the node needs a human. Name the exact failed step.
		return api.DirectiveOutcome{ID: d.ID, State: api.OutcomeFailed,
			Detail: fmt.Sprintf("%v; AND the revert failed to %s: %v", cause, what, err)}
	}

	// Stop the just-installed CONTAINERS (not the pod — that would make podman kill them out
	// from under their units): releases the data subvolume's bind, which restore needs.
	cfg.quiesce(rctx, g, next.ContainerUnits, logf)
	if snap != "" {
		// SWEPT LIKE ANY OTHER MATERIALISED MEMBER ([B.143]): the pre-upgrade point can carry a
		// household's in-flight backup restore like any other, and putting it back unswept would
		// replay it. The markers come from the PRIOR manifest, which is what this member is
		// pinned to.
		//
		// ⚠️ AND THIS IS THE ONE CALLER THAT FALLS BACK when the guest cannot sweep. Refusing here
		// would leave a failed upgrade with no data rollback at all, which is a worse outcome than
		// a marker nobody may ever have written; the restore path, whose alternative is simply
		// leaving the household as they are, refuses instead.
		var sweep []string
		if pm, _, err := manifest.Parse([]byte(priorRaw)); err == nil {
			sweep = services.RestoreMarkers(pm)
		}
		// THE REVERT IS AN UNDO ([B.167]) and records itself as one: an event on a point of the
		// failed version's data, quiesced by the stop above, so the history shows the update and
		// then its undoing -- and the update can still be redone. Best-effort: the rollback matters
		// more than its entry.
		at := cfg.takenAt()
		if err := cfg.takeMember(rctx, g, name, quadlet.SnapshotMember(name, quadlet.TriggerRestoreBefore, at),
			quadlet.TriggerRestoreBefore, quadlet.Quiesced, "before reverting a failed update",
			&quadlet.Event{Kind: quadlet.EventUndo, At: at, What: "The update did not work and was undone"},
			at, logf); err != nil {
			logf("revert %s: could not record the undo point (%v); reverting anyway", name, err)
		}
		restore := g.Restore
		if len(sweep) > 0 && !g.SupportsRestoreSweep() {
			logf("revert %s: this guest cannot sweep a restored member; rolling the data back anyway", name)
			restore = func(ctx context.Context, dataDir, src string, _ []string) error {
				return g.RestoreWithoutSweep(ctx, dataDir, src)
			}
		}
		if err := restore(rctx, dataDir, snap, sweep); err != nil {
			// Data could not be rolled back. Do NOT converge — that would start the prior units
			// on the poisoned data. Leaving this one service stopped is the safe end state:
			// silent data corruption is not recoverable, a stopped service is.
			return bothFailed("restore the data subvolume (the service is left stopped)", err)
		}
	}
	if prior != nil {
		if err := g.ServiceProvision(rctx, name, dataDir, priorSubdirs, priorRaw); err != nil {
			return bothFailed("re-record the prior manifest", err)
		}
		// THE WAYPOINT, the baseline the next sample is compared with (quadlet.Baseline) --
		// otherwise the rollback itself would read as a detected change. After the provision, so
		// it is pinned to the manifest it now runs.
		if snap != "" {
			after := cfg.takenAt()
			if err := cfg.takeMember(rctx, g, name, quadlet.SnapshotMember(name, quadlet.TriggerRestoreAfter, after),
				quadlet.TriggerRestoreAfter, quadlet.Quiesced, "after reverting a failed update", nil, after, logf); err != nil {
				logf("revert %s: could not take the waypoint (%v); continuing", name, err)
			}
		}
	} else if err := g.ServiceForget(rctx, name); err != nil {
		// A FRESH install that failed: the volume must not keep naming a service this node could
		// not bring up, or the next promotion anywhere in the flock converges to it and fails the
		// same way. There is no prior manifest to put back, so the identity is removed instead.
		return bothFailed("remove the failed service's manifest from the volume", err)
	}
	// The revert's skip list is not consulted, and that is not an oversight: the prior manifest is
	// one this node already prepared and ran, so a preparation failure here is a new fault on the
	// undo path, and converge has logged it. What matters to the caller is the same either way —
	// the node is on the prior service, and the outcome below says rolled-back.
	if _, err := g.ServiceConverge(rctx); err != nil {
		return bothFailed("converge back to the prior service", err)
	}
	return api.DirectiveOutcome{ID: d.ID, State: api.OutcomeRolledBack, Detail: cause.Error()}
}

// AwaitHealthy polls the SERVICE's OWN health endpoint until it comes up, or the gate expires.
//
// IT ASKS BY NAME, and the guest resolves where that is ([B.48]). It used to assemble
// `http://127.0.0.1:<port>` from the manifest, which is correct only while every pod shares the
// guest's network namespace — the manifest names a port, and which host answers on it is the
// renderer's decision. Handing the guest a name means the gate probes wherever the front door
// routes, by construction, instead of two places agreeing by coincidence.
//
// It does NOT gate on the front door. That /healthz reports NODE readiness — on a node with
// nothing routed it answers 200 because a node with no services is ready, not sick — so gating on
// it would pass a broken install. The distinction is not a workaround: node health and service
// health have different consequences (one can cost a failover, the other must only alert), and the
// front door answering for the node is what keeps them separable.
//
// An ERROR IS NOT AN UNHEALTHY VERDICT here: a service the guest cannot resolve yet is retried
// until the deadline, exactly like one that is not answering yet, because the two are
// indistinguishable from outside and only one of them is worth reverting an install over.
func (cfg Config) awaitHealthy(ctx context.Context, g serviceInstaller, service string) error {
	if service == "" {
		return nil // nothing to probe (a witness never installs)
	}
	deadline := time.Now().Add(healthGate)
	for {
		ok, err := g.ServiceHealthOf(ctx, service)
		if err == nil && ok {
			return nil
		}
		if time.Now().After(deadline) {
			if err != nil {
				return fmt.Errorf("service did not become healthy within %s (%w)", healthGate, err)
			}
			return fmt.Errorf("service did not become healthy within %s", healthGate)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

// FetchManifest pulls the named service's signed manifest from the catalog and returns it with
// the exact bytes that were signed — the bytes are what gets recorded on the volume, because the
// content hash of THOSE is the service identity.
func (cfg Config) fetchManifest(ctx context.Context, name string) (manifest.Manifest, []byte, error) {
	kr, err := selfupdate.NewKeyring(cfg.UpdateKeyring)
	if err != nil {
		return manifest.Manifest{}, nil, fmt.Errorf("release keyring: %w", err)
	}
	cat := &manifest.Catalog{BaseURL: cfg.CatalogURL, Verifier: kr}
	m, _, raw, err := cat.Fetch(ctx, name)
	return m, raw, err
}

// storageHeadroom is what the free-space gate keeps for the guest OS itself: an image store
// filled to the last byte takes the journal, the units and every next upgrade down with it.
const storageHeadroom = 1 << 30

// gb renders bytes for a sentence a household reads: two decimals of GB, MB below that.
func gb(n int64) string {
	switch {
	case n >= 1e9:
		return fmt.Sprintf("%.2f GB", float64(n)/1e9)
	case n >= 1e6:
		return fmt.Sprintf("%.0f MB", float64(n)/1e6)
	}
	return fmt.Sprintf("%d bytes", n)
}

// installBudgetFor is the whole-operation bound for a manifest ([V3b.31k]): the fixed
// installBudget for an entry that does not say how big it is, and otherwise at least the pull's
// own bound plus the health gate -- an install must not give up before the pull it is waiting
// on is allowed to finish.
func installBudgetFor(m manifest.Manifest) time.Duration {
	if m.Size <= 0 {
		return installBudget
	}
	if b := quadlet.PullTimeout(m.Size) + healthGate; b > installBudget {
		return b
	}
	return installBudget
}

// applyServiceRestore puts one service back to a ring member: its data, and its CODE with it when
// the member's pinned manifest differs from what is running ([B.143]).
//
// ⚠️ THE ORDER INVERTS revert's, AND THAT IS THE WHOLE DESIGN. A revert runs from a BROKEN state
// and is allowed to commit: the household already lost the thing it is undoing. A restore runs
// from a HEALTHY one, chosen by somebody looking at a list, so every step that could fail is
// placed BEFORE the first step that costs anything:
//
//	ensure the images -> take the undo member -> stop -> restore data -> record the manifest
//	 -> take the "after" member -> converge
//
// The images come first because they are the one thing that can be simply GONE: a member from
// three months ago pins a digest the registry may no longer serve, and a household whose rollback
// target cannot be fetched must be told while its service is still running. Everything before the
// stop leaves the node exactly as it was.
func (cfg Config) applyServiceRestore(ctx context.Context, g serviceInstaller, d api.Directive, logf func(string, ...any)) api.DirectiveOutcome {
	failed := func(detail string) api.DirectiveOutcome {
		return api.DirectiveOutcome{ID: d.ID, State: api.OutcomeFailed, Detail: detail}
	}
	if d.Payload == "" {
		return failed("no member named")
	}
	if !g.SupportsMembers() || !g.SupportsImageEnsure() || !g.SupportsSnapshotMember() {
		return failed("this guest is too old to restore a ring member; update the guest OS first")
	}
	service, _, _, ok := quadlet.ParseSnapshotMember(d.Payload)
	if !ok {
		return failed(fmt.Sprintf("%q is not a ring member's name", d.Payload))
	}
	ctx, cancel := cfg.beat.budget(ctx, installBudget)
	defer cancel()

	// THE MEMBER IS READ BACK FROM THE RING, never trusted from the payload. The caller names a
	// path; what that path IS -- its title, and the manifest it was taken under -- is the guest's
	// answer, so a stale picker cannot describe a member into something it is not.
	members, err := g.Members(ctx, service)
	if err != nil {
		return failed(fmt.Sprintf("read %s's ring: %v", service, err))
	}
	var target quadlet.SnapshotEntry
	for _, m := range members {
		if m.Member == d.Payload {
			target = m
		}
	}
	if target.Member == "" {
		return failed(fmt.Sprintf("%s has no member %q (it may have been pruned)", service, d.Payload))
	}
	pm, _, err := manifest.Parse([]byte(target.Meta.Manifest))
	if err != nil {
		return failed(fmt.Sprintf("the member's pinned manifest does not parse: %v", err))
	}
	rendered, err := quadlet.Render(pm, "")
	if err != nil {
		return failed(fmt.Sprintf("the member's pinned manifest does not render: %v", err))
	}

	// (1) THE IMAGES, BEFORE ANYTHING IS TOUCHED. Not through converge's warm, whose failure is
	// required to take the VIP down ([V3.17]) -- here a failure must cost nothing at all.
	for _, ref := range rendered.ImageRefs {
		if err := g.EnsureImage(ctx, ref); err != nil {
			return failed(fmt.Sprintf("%s is not available on this node and could not be fetched (%v); nothing was changed", ref, err))
		}
	}

	dataDir := quadlet.DataRoot(service)
	running, _, _, runningVersion := cfg.priorService(ctx, g, service, nil, logf)

	// (2) THE STOP. Container units only -- the pod would unmount the shared volume under every
	// other service (quiesce's own comment carries the trace).
	//
	// ABOVE THE UNDO, so the undo is QUIESCED like every other member this product takes
	// deliberately ([B.143], owner 2026-09-23). Taking it first was the earlier shape and it made
	// the one member a mis-click depends on the one member whose bytes are crash-consistent — a
	// bet mosquitto is measured to lose. Nothing is risked by the swap: the images are already
	// local (step 1, which is the step that must run before anything stops), and a failure below
	// puts the service back with a converge and says the data was never touched.
	stopped := failed
	if running != nil {
		cfg.quiesce(ctx, g, running.ContainerUnits, logf)
		stopped = func(detail string) api.DirectiveOutcome {
			// The volume still names the running service -- ServiceProvision below has not run --
			// so a converge re-renders from it and starts exactly what the stop above stopped.
			// The same undo applyServiceInstall uses; no new mechanism.
			if _, err := g.ServiceConverge(ctx); err != nil {
				return failed(fmt.Sprintf("%s; AND the service could not be restarted: %v", detail, err))
			}
			return failed(detail + " (the service was restarted)")
		}
	}

	// (3) THE UNDO, and it is the only undo a mis-click has. It exists before anything is torn
	// down: the data below is untouched until it is on disk.
	at := cfg.takenAt()
	undo := quadlet.SnapshotMember(service, quadlet.TriggerRestoreBefore, at)
	if err := cfg.takeMember(ctx, g, service, undo, quadlet.TriggerRestoreBefore, quadlet.Quiesced,
		fmt.Sprintf("before restoring %q (%s)", target.Meta.Title, target.Meta.TakenAt.UTC().Format("2006-01-02 15:04")),
		// THE UNDO IS ITSELF AN EVENT ([B.167]), on the point that undoes it -- which is the redo.
		&quadlet.Event{Kind: quadlet.EventUndo, At: at, What: "Undid changes back to " + target.Meta.TakenAt.Local().Format("Mon 2 Jan, 15:04")},
		at, logf); err != nil {
		return stopped(fmt.Sprintf("take the undo point: %v; nothing was changed", err))
	}
	// (4) THE DATA, keeping [B.126]'s verify -> materialise -> destroy order, and SWEPT of the
	// markers a household's own backup restore leaves inside a member ([B.143]): a "before
	// restoring backup" point put back unswept would hand HA the request and the tar again, and
	// the household would land straight back where they were trying to leave. Refused rather than
	// worked around on a guest that cannot sweep — this path's alternative is leaving them exactly
	// as they are, which is no loss at all.
	if !g.SupportsRestoreSweep() {
		return stopped("this node's guest cannot put a point back safely (it predates the restore sweep); nothing was changed")
	}
	if err := g.Restore(ctx, dataDir, target.Member, services.RestoreMarkers(pm)); err != nil {
		// Do NOT converge: that would start the service on data that is neither what it was nor
		// what was asked for. A stopped service is recoverable; silent corruption is not.
		return failed(fmt.Sprintf("restore the data (the service is left stopped): %v", err))
	}
	// (5) THE CODE, when the member was taken under a different one. Same rule the failed-upgrade
	// revert uses: {manifest + data} move together or the node is describing something it is not.
	if err := g.ServiceProvision(ctx, service, dataDir, quadlet.Subdirs(pm), target.Meta.Manifest); err != nil {
		return failed(fmt.Sprintf("record the member's manifest (the service is left stopped): %v", err))
	}
	// (6) THE WAYPOINT. Taken here rather than at the service's next start: nothing is running, so
	// it is quiesced by construction, correctly titled and pinned with no state carried across the
	// converge below. It duplicates the member's bytes and costs nothing for it -- copy-on-write,
	// nothing has diverged -- and what it buys is a timeline that describes itself, since the
	// member it came from may sit months back in the list.
	after := cfg.takenAt()
	if err := cfg.takeMember(ctx, g, service, quadlet.SnapshotMember(service, quadlet.TriggerRestoreAfter, after),
		quadlet.TriggerRestoreAfter, quadlet.Quiesced,
		fmt.Sprintf("after restoring %q (%s)", target.Meta.Title, target.Meta.TakenAt.UTC().Format("2006-01-02 15:04")),
		nil, // a baseline (quadlet.Baseline): the undo's own event already says what changed
		after, logf); err != nil {
		logf("service restore %s: could not take the waypoint (%v); continuing", service, err)
	}
	// (7) CONVERGE, which re-renders from the volume and starts what it now names.
	if _, err := g.ServiceConverge(ctx); err != nil {
		return failed(fmt.Sprintf("converge onto the restored member (the service is left stopped): %v", err))
	}
	moved := "data only"
	if runningVersion != "" && runningVersion != pm.Version {
		moved = fmt.Sprintf("code too (%s -> %s)", runningVersion, pm.Version)
	}
	logf("service restore %s: back to %q, %s", service, target.Meta.Title, moved)
	return api.DirectiveOutcome{ID: d.ID, State: api.OutcomeDone,
		Detail: fmt.Sprintf("restored %q, %s", target.Meta.Title, moved)}
}

// takeMember renders a member's sidecar and asks the guest for it. The host titles the members
// only it can title; the guest's own (agent/guestagent/inbound.go) never come through here.
//
// THE CONSISTENCY IS THE CALLER'S TO STATE, not this function's to guess: it is the call site that
// knows whether it stopped the service first, and a member taken by the clock against a running
// one will come through here saying so.
func (cfg Config) takeMember(ctx context.Context, g memberTaker, service, member string, tr quadlet.Trigger, cons quadlet.Consistency, title string, ev *quadlet.Event, at time.Time, logf func(string, ...any)) error {
	raw, err := g.ServiceInstalled(ctx, service)
	if err != nil {
		return fmt.Errorf("read the running manifest: %w", err)
	}
	sidecar, err := json.Marshal(quadlet.SnapshotMeta{
		Service: service, Trigger: tr, Title: title, TakenAt: at, Consistency: cons, Manifest: raw, Event: ev,
	})
	if err != nil {
		return err
	}
	return g.Snapshot(ctx, quadlet.DataRoot(service), member, string(sidecar))
}

// applyServiceMembers answers one service's ring as JSON ([B.143]). Read-only: it changes
// nothing, and the caller — the CLI's `app history`, the dashboard's picker — renders it.
func (cfg Config) applyServiceMembers(ctx context.Context, g serviceInstaller, d api.Directive, logf func(string, ...any)) api.DirectiveOutcome {
	failed := func(detail string) api.DirectiveOutcome {
		return api.DirectiveOutcome{ID: d.ID, State: api.OutcomeFailed, Detail: detail}
	}
	if d.Payload == "" {
		return failed("no app named")
	}
	if !g.SupportsMembers() {
		return failed("this guest is too old to list an app's history; update the guest OS first")
	}
	members, err := g.Members(ctx, d.Payload)
	if err != nil {
		return failed(fmt.Sprintf("read %s's history: %v", d.Payload, err))
	}
	// EMPTY IS AN ANSWER, not a failure: a service installed a minute ago has no history yet, and
	// saying so is what lets the picker show "nothing to go back to" instead of an error.
	body, err := json.Marshal(members)
	if err != nil {
		return failed(err.Error())
	}
	return api.DirectiveOutcome{ID: d.ID, State: api.OutcomeDone, Detail: string(body)}
}
