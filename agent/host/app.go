package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	apps "briard.io/agent/apps"
	"briard.io/agent/guest"
	"briard.io/agent/guestagent"
	"briard.io/agent/guestfirmware"
	"briard.io/agent/hass"
	"briard.io/agent/mosquitto"
	"briard.io/agent/platform"
	"briard.io/agent/quadlet"
	"briard.io/agent/reportcard"
	"briard.io/agent/selfupdate"
	"briard.io/shared/api"
	"briard.io/shared/atomicfile"
	"briard.io/shared/dashboard"
	"briard.io/shared/manifest"
	"briard.io/shared/model"
	"briard.io/shared/notify"
)

// Runtime app install. The host orchestrates; the guest is dumb hands.
//
// A shipped node mounts, promotes and serves the landing page with zero apps, so any install
// finds the resource Primary with drbd-reactor actively driving it. Installing changes what the
// volume says this node runs: an upgrade from "empty app" to the current version, on an
// upgrade's rails rather than its own.
//
//	fetch+verify manifest -> render units + warm images (this node) -> snapshot + stage
//	  (Primary only) -> converge -> health-gate -> commit, or undo on failure (the {code+data} rollback)
//
// The undo reads only the volume: the install stages the new manifest and its rollback point
// beside the accepted one, and the gate's pass is a rename (agent/guestagent/staging.go), so one
// procedure undoes a failed gate and a crash inside the gate's window from the same files. The
// install calls it on failure; the observe loop calls it for any install still staged when the
// node starts serving (undoPending), after an agent restart, a guest relaunch or a failover.
//
// App units are not members of the static chain, so none of this pauses the promoter. The
// install's one promoter contact is a guard: refuse to start while the OS-upgrade path holds the
// maintenance bracket, so an overlap fails loudly at the start instead of mutating a resource
// mid-bracket. It cannot prevent the race (a pause can still land between the check and ours)
// and is not claimed to.

// appInstaller is the slice of the guest client an install drives. *guestagent.Client
// satisfies it; a fake drives the test. Narrow interface for DI, not a seam.
type appInstaller interface {
	Status(ctx context.Context, resource string) (model.QuorumState, error)
	AppRender(ctx context.Context, files map[string]string, stale []string) error
	AppProvision(ctx context.Context, name, dataDir string, subdirs []string, manifest string) error
	AppInstalled(ctx context.Context, name string) (string, error)
	// SupportsAppInstalled gates the whole install: a guest that cannot name an app when
	// recording its identity would overwrite whatever it already runs. See applyOneAppInstall.
	SupportsAppInstalled() bool
	// AppConverge makes the node match the volume -- render, warm and start every manifest
	// recorded there -- and SupportsAppConverge gates it the same way. It returns the apps the
	// node could not prepare, having started every other one. An install must fail when its own
	// app is in that list: the node is still serving the prior version, and reporting success
	// would leave the volume, the cache and the cloud naming a version nothing is running. live
	// names the install being run now ("" for none): its staged manifest is what renders, and
	// every other staged install is held back.
	AppConverge(ctx context.Context, live string) ([]string, error)
	SupportsAppConverge() bool
	// StorageFree measures the guest's image store: the free-space gate reads it
	// before a pull starts. An older guest that cannot measure gets no gate, not a refusal.
	StorageFree(ctx context.Context) (free, total int64, err error)
	SupportsStorageFree() bool
	// StorageGrow fills a state disk the host just grew (growStateDisk).
	StorageGrow(ctx context.Context, size int64) error
	SupportsStorageGrow() bool
	// AppPulling / AppPulled bracket the pull for the dashboard's bar: the manifest's
	// sizes on the guest's tmpfs before the first byte, gone once the image is present. An older
	// guest shows the install with no bar.
	AppPulling(ctx context.Context, app string, size, installed int64) error
	AppPulled(ctx context.Context, app string) error
	SupportsAppPulling() bool
	// Stage, then commit (agent/guestagent/staging.go): an install is recorded on the volume beside
	// the accepted manifest, with its rollback point, and becomes the identity only at the commit.
	// AppPending reads back every install that never committed, with all its undo needs.
	AppStage(ctx context.Context, name, dataDir string, subdirs []string, manifest, rollback string) error
	AppCommit(ctx context.Context, name string) error
	AppDiscard(ctx context.Context, name string) error
	AppPending(ctx context.Context) ([]guestagent.PendingInstall, error)
	// ReactorActive is the interim overlap guard, and the only promoter verb here: an install
	// takes no maintenance bracket, it only refuses to start while somebody else holds one.
	// Pausing and resuming belong to the OS upgrade path (agent/guest), which changes what a live
	// promoted resource runs.
	ReactorActive(ctx context.Context) (bool, error)
	AppStart(ctx context.Context, unit string) error
	// AppWarm ensures an image is present, pulling only if it is missing -- see the guest's
	// warmImage for why "start the .image unit" is not the same operation.
	AppWarm(ctx context.Context, unit, ref string) error
	AppStop(ctx context.Context, unit string) error
	AppHealth(ctx context.Context, url string) (bool, error)
	// AppHealthOf is the same floor asked by name, so the guest resolves the address from the
	// routing table it converged rather than from a URL the host assembled.
	AppHealthOf(ctx context.Context, app string) (apps.Health, error)
	// The S1 gate's input, one layer above the liveness floor AppHealth answers -- one method
	// per app that has a signal, named for it (see readinessProbe, which is the same set).
	// Not on `upgrader`, deliberately: an OS upgrade must not be able to name an app, and its
	// interface carries nothing that can.
	HassReadiness(ctx context.Context, port int) ([]hass.Entry, error)
	MosquittoProbe(ctx context.Context, token string) (mosquitto.Sample, error)
	// HassNudge is the one call here that travels into an app rather than sampling it: a
	// Home Assistant that is already running has to be told that the node's offering changed,
	// because nothing else will restart it.
	HassNudge(ctx context.Context) (bool, error)
	// Snapshot/Restore are the {data} half of the rollback: a broken upgrade must put
	// the app's data subvolume back to its pre-upgrade point, not only put the prior manifest
	// back. Fresh installs (no prior data) never call them.
	Snapshot(ctx context.Context, dataDir, dest, sidecar string) error
	// SupportsSnapshotMember gates the ring: an older guest does not advertise
	// data.member, and taking a plain snapshot instead would leave an unlabelled member -- the one
	// thing the sidecar exists to prevent. Refused loudly, never worked around.
	SupportsSnapshotMember() bool
	// The quiesced take, for the one member taken while the app still runs: the
	// guest asks the app to hold still, snapshots, releases, and answers whether it held --
	// which is what the class is written from, by the guest, since the host cannot see it.
	QuiescedSnapshot(ctx context.Context, app, dataDir, dest, sidecar string) (bool, string, error)
	SupportsQuiescedSnapshot() bool
	// The restore's three: list an app's ring, make one member's images resident
	// before anything is stopped, and put the data back. All capability-gated, because a guest
	// older than the ring can do none of them and a fallback would be worse than a refusal.
	Members(ctx context.Context, app string) ([]quadlet.SnapshotEntry, error)
	SupportsMembers() bool
	EnsureImage(ctx context.Context, ref string) error
	SupportsImageEnsure() bool
	// The volume's apps and RemoveImage: what dropSuperseded needs to delete an image the
	// app moved off once nothing on the volume pins it.
	AppList(ctx context.Context) ([]string, error)
	SupportsAppList() bool
	RemoveImage(ctx context.Context, ref string) (kept bool, err error)
	SupportsImageRemove() bool
	// Restore materialises a member over the live subvolume, minus the markers a household's own
	// backup restore leaves in it -- the guest's data.replace. RestoreWithoutSweep is
	// the frozen unswept verb, for undo's one case where no rollback is worse than a stale
	// marker; nothing else may call it.
	Restore(ctx context.Context, dataDir, src string, sweep []string) error
	RestoreWithoutSweep(ctx context.Context, dataDir, src string) error
	SupportsRestoreSweep() bool
}

// installBudget bounds the whole operation. Generous, because a first install legitimately pulls
// images; bounded, because a wedged install must reach its revert rather than hang forever with
// the app half-installed.
const installBudget = 15 * time.Minute

// healthGate is how long the app gets to become healthy (apps.Health, the one
// answer) before the install is judged failed and reverted. HA takes tens of seconds.
const healthGate = apps.HealthGate

// revertBudget bounds the rollback on its own detached deadline, so the undo path never shares a
// deadline with the thing it undoes: the health gate's most likely failure is the install budget
// expiring, and a revert inheriting that dead context could neither restore data nor put the prior
// manifest back, leaving the node half-reverted exactly when the install went wrong.
const revertBudget = 3 * time.Minute

// applyAppPrewarm renders a catalogued app's units and pulls its images, and stops there: the
// warm-standby half on its own, with no provisioning, no promoter change, no health gate --
// nothing that alters what this node is currently serving. It deliberately shares the install's
// fetch/verify/render/warm code rather than approximating it: a peer that warmed from a different
// rendering than the one the Primary installs cannot take over, the exact failure this exists to
// prevent.
//
// It is role-independent, and that is the point, for safety before speed. An install handed to a
// node that is not serving does this much and stops, which makes the phase a function of the
// role, and roles are not ours to control (drbd-reactor promotes on quorum events), so the
// directive's meaning can change between enqueue and execution: a node sent a "download" while
// Secondary and promoted before executing it would instead stage onto the volume, converge and
// health-gate itself -- mid-failover, the worst possible moment to be handed an install nobody
// asked for. Naming the phase removes the race outright. The parallelism is the second reason,
// not the first: the Primary needs warming too, so every anchor pulls at once instead of the
// Primary pulling again after the peers have finished.
func (cfg Config) applyAppPrewarm(ctx context.Context, g appInstaller, d api.Directive, logf func(string, ...any)) api.DirectiveOutcome {
	failed := func(detail string) api.DirectiveOutcome {
		return api.DirectiveOutcome{ID: d.ID, State: api.OutcomeFailed, Detail: detail}
	}
	if d.Payload == "" {
		return failed("no app named")
	}
	parent := ctx
	ctx, cancel := cfg.beat.budget(parent, installBudget)
	defer func() { cancel() }()

	m, _, err := cfg.fetchManifest(ctx, d.Payload)
	if err == nil {
		// Same re-budget as the install: a prewarm is the pull and nothing else.
		if b := installBudgetFor(m); b > installBudget {
			cancel()
			ctx, cancel = cfg.beat.budget(parent, b)
		}
	}
	if err != nil {
		logf("app prewarm %s: %v", d.Payload, err)
		return failed(err.Error())
	}
	// Images only: a prewarm may run on a node that is not serving, which has not allocated this
	// app an address — and does not need one to hold its images.
	rendered, err := quadlet.Images(m)
	if err != nil {
		return failed(err.Error())
	}
	// No `stale` list: a prewarm must not remove another app's units. Cleaning up a renamed
	// app's orphans is the install's job, on the node that is actually changing what serves.
	if err := g.AppRender(ctx, rendered.Files, nil); err != nil {
		return failed(fmt.Sprintf("render units: %v", err))
	}
	for _, u := range rendered.ImageUnits {
		// Ensure-present, not start: starting an .image unit is a registry pull, so an image that
		// is already here (prewarmed, or staged into the guest at build time) must not be fetched
		// again -- and for a locally-staged one there is no registry to fetch it from. Same verb,
		// same reason, as bring-up.
		if err := g.AppWarm(ctx, u, rendered.ImageRefs[u]); err != nil {
			return failed(fmt.Sprintf("warm image (%s): %v", u, err))
		}
	}
	logf("app prewarm %s: %d units rendered, %d images warmed; nothing promoted", m.Name, len(rendered.Files), len(rendered.ImageUnits))
	return api.DirectiveOutcome{ID: d.ID, State: api.OutcomeDone, Detail: "prewarmed"}
}

// applyAppInstall installs the space-separated apps the directive names, in order, as one act: a
// second act while one runs is refused "busy", not queued, so a household that picked three apps
// hears about all three together and no queue anywhere holds its intent for minutes with nothing
// to show. Each app takes the whole install path below, health gate and rollback included; the
// first that does not end healthy ends the act, with the ones before it installed and said to be.
func (cfg Config) applyAppInstall(ctx context.Context, g appInstaller, d api.Directive, n notify.Notifier, logf func(string, ...any)) api.DirectiveOutcome {
	var names []string
	for _, name := range strings.Fields(d.Payload) {
		if !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	if len(names) <= 1 {
		return cfg.applyOneAppInstall(ctx, g, d, n, logf)
	}
	var done, reach []string
	for _, name := range names {
		o := cfg.applyOneAppInstall(ctx, g, api.Directive{ID: d.ID, Kind: d.Kind, Payload: name}, n, logf)
		if o.State != api.OutcomeDone {
			o.Detail = name + ": " + o.Detail
			if len(done) > 0 {
				o.Detail = "installed " + strings.Join(done, ", ") + "; " + o.Detail
			}
			return o
		}
		done = append(done, name)
		reach = append(reach, name+": "+o.Detail)
		logf("app install: %s done (%d of %d)", name, len(done), len(names))
	}
	return api.DirectiveOutcome{ID: d.ID, State: api.OutcomeDone, Detail: strings.Join(reach, "\n")}
}

// applyOneAppInstall installs or upgrades the catalogued app the directive names and returns the
// terminal outcome (announce-before-act). Install is the same path, an upgrade from "empty", and
// the path is idempotent by construction: re-installing the same manifest re-renders identical
// units and reuses the existing subvolume.
//
// The failure contract is the point of the path: a target that never becomes healthy leaves the
// node serving what it served before -- no app for a fresh install; for an upgrade the prior
// manifest and its data, the app-level twin of the {code+data} OS rollback. Hence the snapshot
// before the switch and, on a tripped gate, the restore of both subvolume and prior manifest.
func (cfg Config) applyOneAppInstall(ctx context.Context, g appInstaller, d api.Directive, n notify.Notifier, logf func(string, ...any)) api.DirectiveOutcome {
	failed := func(detail string) api.DirectiveOutcome {
		return api.DirectiveOutcome{ID: d.ID, State: api.OutcomeFailed, Detail: detail}
	}
	if d.Payload == "" {
		return failed("no app named")
	}
	// First, before the fetch, so a refusal costs nothing and leaves the node untouched. A guest
	// without app.installed keeps every app's identity in one file on the volume, so it cannot say
	// which app a manifest belongs to: installing would record this app's identity over whatever
	// that node already runs and a survivor would promote against the wrong manifest. Refused
	// loudly, with no compat path: the alpha reinstalls; the guest OS rolls and the verb appears.
	if !g.SupportsAppInstalled() {
		return failed("this VM is too old to name an app's identity on the volume (no app.installed); update the VM before installing")
	}
	// Same for converge: a guest without app.converge has no briard-apps unit either, so nothing
	// there ever reads the volume and this would write the manifest, report success and leave the
	// node serving exactly what it served before. Same refusal, same reason.
	if !g.SupportsAppConverge() {
		return failed("this VM is too old to converge itself to the volume (no app.converge); update the VM before installing")
	}
	// Same for the ring, and deliberately a capability, not a protocol floor: a floor makes every
	// host refuse every not-yet-rolled guest fleet-wide, then its own health gate reverts the
	// self-update, since reaching the image that would fix it needs the channel the floor just
	// closed. This refuses exactly the one path that needs the verb.
	if !g.SupportsSnapshotMember() {
		return failed("this VM is too old to take a titled rollback point (no data.member); update the VM before installing")
	}
	// Installing an uninstalled app again restores its kept copy, at the version it was uninstalled
	// at; a newer catalog version is then the ordinary update offer, on the update's rails. The
	// catalog's version over the kept data would take a fresh install's path, with no rollback point
	// for the first start's migration. An unreadable ring refuses rather than installing empty beside
	// a copy it could not see.
	if kept, err := cfg.keptCopyOf(ctx, g, d.Payload); err != nil {
		return failed(err.Error())
	} else if kept.Member != "" {
		// The install's memory gate, which the restore does not have.
		if km, _, err := manifest.Parse([]byte(kept.Meta.Manifest)); err == nil {
			if refusal := cfg.memoryFor(ctx, km, logf); refusal != "" {
				return failed(refusal)
			}
		}
		o := cfg.applyAppRestore(ctx, g, api.Directive{ID: d.ID, Kind: api.DirectiveAppRestore, Payload: kept.Member}, logf)
		if o.State == api.OutcomeDone {
			if _, err := g.HassNudge(ctx); err != nil {
				logf("app install %s: could not tell Home Assistant to reconsider (%v); it will pick this up at its next start", d.Payload, err)
			}
		}
		return o
	}
	parent := ctx
	ctx, cancel := cfg.beat.budget(parent, installBudget)
	defer func() { cancel() }()

	m, raw, err := cfg.fetchManifest(ctx, d.Payload)
	if err != nil {
		logf("app install %s: %v", d.Payload, err)
		return failed(err.Error())
	}
	// The manifest says how long its pull may take; an install must not expire
	// before the pull it waits on is allowed to. Re-budgeted from the parent, and the deferred
	// cancel is a closure so it releases whichever lease is current.
	if b := installBudgetFor(m); b > installBudget {
		cancel()
		ctx, cancel = cfg.beat.budget(parent, b)
	}
	if refusal := cfg.roomForPull(ctx, g, m, logf); refusal != "" {
		return failed(refusal)
	}
	// The memory gate, beside it: an app's first start is its busiest moment and growth reacts in
	// minutes, so before it the running guest is grown to the system's share plus every app's
	// declared minimum, this one's included, and the install refused only when the host cannot give
	// that much at all. A guest whose size cannot be read installs unmeasured, like the image store.
	if refusal := cfg.memoryFor(ctx, m, logf); refusal != "" {
		return failed(refusal)
	}
	// No address: this render supplies unit names and image digests, and converge re-renders with
	// the allocated one before anything starts (see Render).
	rendered, err := quadlet.Render(m, "")
	if err != nil {
		return failed(err.Error())
	}

	// Interim guard. Checked before anything is written, so a refusal leaves the node untouched.
	active, err := g.ReactorActive(ctx)
	if err != nil {
		return failed(fmt.Sprintf("could not read promoter state: %v", err))
	}
	if !active {
		return failed("the promoter is already paused — another maintenance operation is in progress")
	}

	// N apps: a second distinct app is not refused, because everything below is per-app by
	// construction. The node-local cache is a directory, the volume's `.apps/<name>.json` names a
	// manifest's app, `priorApp` and `filesToRemove` look only at this app, `NodeStatus.Apps` carries
	// an identity per app, and `telemetry.NodeResources.Payloads` measures each app's own footprint
	// and restart count, so the cloud cannot confirm a rollout against the wrong app or miss a
	// crash-loop in another. `Healthy` and `System` deliberately stay node-scoped: a closure is a
	// property of the node, and the reverse proxy is what "is this node serving" means.

	// What is installed now -- the rollback target, the manifest whose data the rollback point
	// below is taken under. nil on a fresh install (the shipped zero-app node) or an idempotent
	// re-install of the same manifest.
	prior, _, priorRaw, _ := cfg.priorApp(ctx, g, m.Name, raw, logf)

	// Units are node-local (/run), so this node renders its own; the volume holds the manifest as
	// the identity every node renders from, and converge re-renders from it at promotion, so a
	// survivor never told about this install still starts it. Stale names a renamed prior app's
	// orphan files, which this render removes.
	var stale []string
	if prior != nil {
		stale = filesToRemove(prior.Files, rendered.Files)
	}
	if err := g.AppRender(ctx, rendered.Files, stale); err != nil {
		return failed(fmt.Sprintf("render units: %v", err))
	}

	// Warm the images here, synchronously. Nothing else starts the .image units (no [Install]) and
	// the containers are Pull=never, so converge would have to pull an image not warmed here at
	// promotion, and a pull that cannot happen there fails converge and takes the VIP down. Install
	// time is right because a human is watching and the CLI can report the failure, while running
	// and failover must never need the network: `briard app install` is the one point where an
	// offline node legitimately fails. Bring-up re-warms after a guest reboot
	// (BringUpSpec.AppImages), a re-warm, not the primary fetch.
	//
	// The pull record draws the dashboard's bar: totals before the first byte, cleared after the
	// last on every exit, since a stale record would draw a bar over an install that ended.
	// Best-effort: a bar is a courtesy, never a gate on the install.
	if m.Size > 0 && g.SupportsAppPulling() {
		if err := g.AppPulling(ctx, m.Name, m.Size, m.InstalledSize); err != nil {
			logf("app install %s: pull record: %v (no progress bar)", m.Name, err)
		}
		defer func() {
			if err := g.AppPulled(context.WithoutCancel(ctx), m.Name); err != nil {
				logf("app install %s: clear pull record: %v", m.Name, err)
			}
		}()
	}
	for _, u := range rendered.ImageUnits {
		// Ensure-present, not start, as in applyAppPrewarm and bring-up.
		if err := g.AppWarm(ctx, u, rendered.ImageRefs[u]); err != nil {
			return failed(fmt.Sprintf("warm image (%s): %v", u, err))
		}
	}
	// The images are in: the record goes now, not at the end, so the dashboard says the app is
	// installing rather than holding a download bar at its last percent while it starts. The
	// deferred clear above stays for the exits before this line.
	if m.Size > 0 && g.SupportsAppPulling() {
		if err := g.AppPulled(ctx, m.Name); err != nil {
			logf("app install %s: clear pull record: %v", m.Name, err)
		}
	}

	// The subvolume and the manifest live on the replicated volume, which only the Primary has
	// mounted. A secondary legitimately stops here: it has its units and will get the data by
	// replication.
	qs, err := g.Status(ctx, cfg.Resource.Name)
	if err != nil {
		return failed(fmt.Sprintf("read machine status: %v", err))
	}
	// A node that holds the disk with its chain down is taking over (or was stopped): the volume
	// may not be mounted, and the promotion's own converge would stop what an install starts
	// now. Refused, never answered as a standby: the admin who asked sees it and asks again.
	if !qs.Serving() && qs.Primary && qs.Quorate {
		return failed("this machine holds your data but is not serving it yet; install again in a moment")
	}
	if !qs.Serving() {
		logf("app install %s: units rendered; not serving, so no provisioning here", m.Name)
		return api.DirectiveOutcome{ID: d.ID, State: api.OutcomeDone, Detail: "units rendered (not serving)"}
	}

	dataDir := quadlet.DataRoot(m.Name)

	// Capture the S1 baseline first, while the old version still serves untouched. The gate is
	// differential, so this sample is the whole confounder control: an integration broken before
	// the upgrade is excluded and only what this change breaks can trip it; any later sample is of
	// something already disturbed.
	//
	// ⚠️ This line must stay above the stop and the snapshot below: a baseline of a stopped app reads
	// as every integration having regressed. TestUpgradeCapturesTheBaselineBeforeTheSnapshot asserts
	// the order by name.
	//
	// Deliberately only on an upgrade: a fresh install has no "was loaded" to regress from (the app
	// was not running), so the floor is the honest gate there, and a Baseline call would ask an app
	// that does not exist yet. Through &g, so a gate built here asks over the channel the health gate
	// may have followed.
	gate := guest.Gate{Assessor: cfg.assessorFor(m, heldProbe{&g}, logf), Logf: logf}
	var readiness guest.Readiness
	if prior != nil {
		readiness = gate.Capture(ctx)
	}

	// Stop the app, then snapshot the rollback point: the data is what a broken upgrade can poison,
	// and this read-only snapshot on the replicated volume is what a failed gate restores. Never
	// live: a live snapshot is only crash-consistent, and `apps-pair.nix` shows the cost --
	// mosquitto writes its in-memory retained state on a clean stop, so a running broker's snapshot
	// misses a message it already accepted and the rollback restores a broker with nothing in it.
	// HA surviving a live snapshot (btrfs snapshots atomically, HA replays its WAL on open) is a fact
	// about HA, not a guarantee the catalog can make. Stopping first makes the point
	// application-consistent for every app, and the rollback needs the same stop anyway:
	// `data.restore` cannot run until the subvolume's bind is released. Container units, never the
	// pod (see quiesce); only the upgraded app stops, so the node keeps serving every other one.
	//
	// A failed snapshot is undone by a re-converge, no new mechanism: until the stage below lands the
	// volume names only the prior app, and converge starts every unit of every app it does not skip,
	// so it starts exactly what the stop stopped. The stop interrupts the app, and an interruption
	// the household is never brought out of is just an outage with a directive attached.
	var snap string
	stoppedFail := failed
	if prior != nil {
		cfg.quiesce(ctx, g, prior.ContainerUnits, logf)
		stoppedFail = func(detail string) api.DirectiveOutcome {
			if _, err := g.AppConverge(ctx, ""); err != nil {
				return failed(fmt.Sprintf("%s; AND the app could not be restarted: %v", detail, err))
			}
			return failed(detail + " (the app was restarted)")
		}
		// The member is pinned to the version it is taken under, v1 by construction: taken before the
		// switch, it records the manifest still on the volume, the one an undo goes back to. v2's first
		// member will pin v2, leaving two near-identical members seconds apart with unambiguous meanings
		// (back to v1; to v2 as of then). Both are kept: the duplication is not fought, and no "who
		// wrote the data" bookkeeping is needed.
		at := cfg.takenAt()
		snap = quadlet.SnapshotMember(m.Name, quadlet.TriggerAppUpdateBefore, at)
		meta := quadlet.SnapshotMeta{
			App:     m.Name,
			Trigger: quadlet.TriggerAppUpdateBefore,
			TakenAt: at,
			// Quiesced by the stop above, which is what that stop buys and the reason it
			// is not an optimisation somebody may reorder away.
			Consistency: quadlet.Quiesced,
			Manifest:    priorRaw,
			// The update's event, on the point that undoes it. Written before the act
			// because the point is; a revert below records its own undo rather than rewriting this.
			Event: &quadlet.Event{At: at, Reasons: []quadlet.Reason{{Kind: quadlet.ReasonAppUpdate, What: "Updated to " + m.Version}}},
		}
		sidecar, err := json.Marshal(meta)
		if err != nil {
			return stoppedFail(fmt.Sprintf("render the rollback point's sidecar: %v", err))
		}
		if err := g.Snapshot(ctx, dataDir, snap, string(sidecar)); err != nil {
			return stoppedFail(fmt.Sprintf("snapshot rollback point: %v", err))
		}
	}

	// Stage the new manifest and its rollback point beside the accepted one, which stays untouched
	// until the commit, and ensure the subvolume/subdirs. From here every failure is the undo, which
	// reads what to undo from the volume: a stage that failed part-way either landed its
	// `.json.next` (undone like any other) or did not (the accepted manifest stands; a converge back
	// to it). The owner is told, as for every upgrade that rolls back: the outcome reaches only
	// whoever watches the directive, and an app update is the one a household meets most.
	kind := "app install"
	if prior != nil {
		kind = "app update"
	}
	revert := func(cause error) api.DirectiveOutcome {
		// A step that died with the channel reverts over the one Run re-establishes: the dead
		// client would fail the revert the same way, leaving the install half undone.
		if errors.Is(cause, guestfirmware.ErrChannelDown) {
			cfg.follow(ctx, &g, logf)
		}
		o := cfg.revert(ctx, g, d, m.Name, logf, cause)
		after := afterRolledBack
		if o.State != api.OutcomeRolledBack {
			after = afterNotUndone // the revert itself failed; o.Detail says which step
		}
		// Not ctx: a gate that ran out of budget is the commonest cause, and its alert must still leave.
		escalate(context.WithoutCancel(ctx), n, logf, m.Name, kind, m.Version, after, errors.New(o.Detail))
		return o
	}
	if err := g.AppStage(ctx, m.Name, dataDir, quadlet.Subdirs(m), string(raw), snap); err != nil {
		return revert(fmt.Errorf("stage the install: %w", err))
	}

	// Converge in place with this install live (its staged manifest is what renders), and
	// deliberately with no maintenance bracket: the chain is static and apps are not members, so an
	// install is stage + tell the node to re-read the volume. It never stops the promoter for every
	// other app on the node, and a failure here cannot demote it. A verb, not `systemctl restart
	// briard-apps`: briard-apps is a chain member, so stopping it would deactivate drbd-reactor's
	// target, unmount the volume and demote the node -- the obvious way to trigger a re-converge, and
	// the wrong one. Same code, same unit, no unit lifecycle touched.
	skipped, err := g.AppConverge(ctx, m.Name)
	if err != nil {
		return revert(fmt.Errorf("install %s: %w", m.Name, err))
	}
	// Converge contains a preparation failure to the one app, so it succeeds while declining to
	// start ours: the right answer for a promoting node, the wrong one to report as an install.
	// Without this the old container would still be serving, the health gate below would pass on
	// it, and the install would report the new version as running.
	if slices.Contains(skipped, m.Name) {
		return revert(fmt.Errorf("install %s: the node could not prepare it and did not start it (see the guest log)", m.Name))
	}
	logf("app install %s: converged from the volume; waiting for health", m.Name)

	// Gate on the app's own endpoint (in-guest), not the reverse proxy (see awaitHealthy), and by
	// name: the converge above wrote the routing table, so the guest already knows where this app
	// listens, rather than the host assembling an address it can only guess stays true.
	if err := cfg.awaitHealthy(ctx, &g, m.Name, logf); err != nil {
		logf("app install %s failed its health gate (%v); reverting", m.Name, err)
		return revert(err)
	}
	// The floor held; now ask whether the app still does what it did. The floor's whole blind spot
	// is an app that answers while its work is broken (HA returning 200 for /manifest.json with half
	// the house's integrations dead), and this layer sees it. Rollback reverts {code + data} exactly
	// as a failed floor does; Hold keeps the upgrade and says so; Pass, a missing assessor, a fresh
	// install or a failed sample keep it silently: S1 must never revert a household's app on the
	// strength of its own telemetry breaking.
	//
	// ⚠️ A Hold reaches only the log. On the free tier that is nobody (no surface carries it
	// further); the rollback window and the user remain the backstop.
	if err := gate.Judge(ctx, readiness); err != nil {
		logf("app install %s failed the readiness gate (%v); reverting", m.Name, err)
		return revert(err)
	}
	// The commit: one rename on the volume, and the install is the identity. A commit that fails
	// leaves it staged, and a staged install is undone -- the same as a gate that failed.
	if err := g.AppCommit(ctx, m.Name); err != nil {
		logf("app install %s could not be committed (%v); reverting", m.Name, err)
		return revert(fmt.Errorf("commit the install: %w", err))
	}
	// Record the manifest node-locally too; both copies are needed. The volume's is the replicated
	// identity (what the app is); this one is what the host reads at start-up, before promotion,
	// while the replicated copy is unreadable: which apps to report and measure, and which units
	// bring-up writes back and re-warms after a guest reboot (installedApps). Same pattern and reason
	// as AssignmentCache.
	if err := cfg.cacheApp(m.Name, raw); err != nil {
		// Not fatal: the app is installed and serving. Say so loudly though: the next agent
		// restart comes up without it until this node next reads the volume at promotion.
		logf("app install %s: WARNING: could not cache the manifest (%v); an agent restart will drop it from the promoter chain", m.Name, err)
	}
	// Tell a Home Assistant that is already running. briard's integration does everything at an HA
	// start, and converge restarts only apps whose bytes changed, so a broker installed beside a live
	// HA leaves it running unwired until something restarts it, and nothing else in the product will.
	// Unconditional, the cheap half of the design: no "which app implies what" table, no check of
	// what else is installed; the guest answers `false` when there is no HA to tell and the signal
	// carries nothing, so a needless nudge costs one loopback request and changes nothing. After the
	// gates, so a reverted install never fires it, and after the health gate the broker already
	// accepts clients, a race the integration's own socket check would otherwise have to lose.
	// Best-effort, never fatal: the app is installed and serving, and a failure costs the household
	// exactly what they had before this existed, an HA that picks the change up at its next start.
	if told, err := g.HassNudge(ctx); err != nil {
		logf("app install %s: could not tell Home Assistant to reconsider (%v); it will pick this up at its next start", m.Name, err)
	} else if told {
		logf("app install %s: Home Assistant was told to reconsider what the node offers", m.Name)
	}
	logf("app install %s: healthy, serving", m.Name)
	// Committed: no gate is left that could revert it, so the image the prior version ran on is
	// no longer anyone's rollback target. Without this every upgrade leaves one behind for good.
	cfg.dropSuperseded(ctx, g, priorRaw, logf)
	// Where to reach it, as the outcome Detail: the node knows the address and the operator does not
	// (the name is the one the guest publishes over mDNS; a port, where one is the address, is the
	// manifest's, never typed by a human). Without it a successful verb leaves the address to be
	// guessed, walking the user to the reverse proxy's "nothing is routed here" page so a working
	// install reads as broken. The registry (agent/apps) writes the sentence because it differs per
	// app: one the reverse proxy does not front, a broker, is reached at a published port, not at its
	// name on the proxy.
	reach := apps.Reach(m, cfg.FlockName)
	return api.DirectiveOutcome{ID: d.ID, State: api.OutcomeDone, Detail: reach}
}

// memoryFor grows the running guest to hold m beside every installed app, and answers why not when
// the host cannot give it that much: the install's memory gate, and installing an uninstalled app
// again.
func (cfg Config) memoryFor(ctx context.Context, m manifest.Manifest, logf func(string, ...any)) string {
	need := cfg.memoryNeeded(m.Name, m.MinMemoryMB)
	if need <= cfg.MemoryMB {
		return ""
	}
	vm := platform.Adopt(cfg.guestSpec())
	host := reportcard.MemTotalMB()
	switch err := growGuestMemoryTo(ctx, need, host, vm.MemoryMB, vm.AddMemory); {
	case errors.Is(err, errMemoryCeiling):
		return fmt.Sprintf("not enough memory on this machine: %s needs the VM to hold %d MB and this host can give it at most %d MB (keeping %d MB for itself)",
			m.Name, need, guestMemoryCeilingMB(host), hostMemoryReserveMB)
	case err != nil:
		logf("app install %s: cannot size the guest's memory (%v); installing unmeasured", m.Name, err)
	}
	return ""
}

// cacheApp writes one app's manifest into the node-local cache directory, named for the app. An
// install writes it only once its gates pass and it commits, so a restart never brings up a
// failed install.
//
// One file per app, the manifest's bytes verbatim: one file holding the set cannot work, because
// a manifest's content hash is the app identity (shared/manifest) and re-encoding the set would
// give every member a new identity on every write. Per file, install and uninstall are file
// operations rather than a read-modify-write of a shared document, which keeps two installs from
// losing each other's app.
//
// Atomic and durable (shared/atomicfile): a bare WriteFile over an existing file truncates first,
// so a crash mid-write leaves half a manifest, and an unflushed one leaves none at all for a
// commit interval after the install returned. Neither is loud: installedApps logs an unparseable
// file and reads it as "not installed", and bring-up reads only this cache, so the node quietly
// comes back without the app until it next reads the volume at promotion (adoptVolumeApps). The
// mesh is the other node-scoped fact bring-up takes from a file rather than re-deriving: two
// facts, two caches, one rule (cacheMesh).
func (cfg Config) cacheApp(name string, raw []byte) error {
	if cfg.AppCache == "" {
		return nil
	}
	return atomicfile.Write(cfg.manifestPath(name), raw, 0o600, 0o700)
}

// manifestPath is one app's file in the cache directory. The name is a manifest slug, and
// shared/manifest's Validate refuses anything else precisely because these become path elements,
// so there is no traversal to strip here: the check upstream is the one that matters and this
// would only duplicate it badly.
func (cfg Config) manifestPath(name string) string {
	return filepath.Join(cfg.AppCache, name+".json")
}

// adoptInstalledApps refreshes the live config from the node-local manifest cache after a
// directive that changed what is installed. Run() does this once at startup; without it here the
// process that performed the install would keep its old (usually empty) cfg.Apps until restarted,
// and everything the loop derives from it (the status report's apps, each app's footprint, the
// clock sample, the recorder check) would leave the new app out: the node would run perfectly and
// describe itself as empty. Pointer receiver on purpose: it mutates the observe loop's own cfg,
// the copy every later cycle reads. A failed re-read keeps the previous view, since the cache is
// the authority on what is installed and a transient read error is not evidence of an uninstall.
//
// It runs after any install's outcome, done or not (a multi-app act can fail on the second with
// the first installed; an install that did nothing leaves the cache as it was), after a restore
// from the history or the backup (which can move an app to its point's version or put back an
// app this node did not have), and after an uninstall.
func (cfg *Config) adoptInstalledApps(d api.Directive, o api.DirectiveOutcome, logf func(string, ...any)) {
	if d.Kind != api.DirectiveAppInstall && d.Kind != api.DirectiveAppRestore && d.Kind != api.DirectiveBackupRestore &&
		d.Kind != api.DirectiveAppUninstall {
		return
	}
	specs, rendered, ok := cfg.installedApps(logf)
	// An uninstall's empty cache is an answer: the last app went.
	if !ok && (d.Kind != api.DirectiveAppUninstall || cfg.AppCache == "") {
		return
	}
	// Not cfg.Promoter: the chain is static, and what a node runs comes from the volume, not
	// from what it was told.
	cfg.Apps, cfg.AppRendered = specs, rendered
	for _, spec := range specs {
		logf("installed app %q adopted into the running config (serving unit %s)", spec.Name, spec.ServingUnit())
	}
}

// installedApps reads the node-local manifest directory and returns the app specs plus the union
// of their rendered units, at start-up (Run, so a restarted agent knows what it runs rather than
// reverting to what the environment describes) and after every act that changes what is
// installed (adoptInstalledApps). No chain comes back: an app is not a chain member, the promoter
// chain is the same units everywhere (shared/chain), and `briard-apps` starts the apps from the
// volume once the mount exists.
//
// The rendering is returned because the units live on tmpfs (/run/containers/systemd), which a
// guest reboot erases while this cache survives; bring-up writes them back and re-warms their
// images (BringUpSpec.AppUnits/AppImages), so a node, a standby included, is warm before it
// promotes. Order is by filename, i.e. app name: deterministic and nothing more, the cheapest way
// to owe the one property owed, stability across restarts.
//
// Every failure is a soft "not installed", per app and for the set: an absent cache directory
// (the shipped zero-app node), an unreadable one, or a file that no longer parses leaves the node
// bringing up with what remains rather than refusing to start. A node that will not boot is worse
// than one that lost an app, and losing one app must not cost the others, so a bad file is
// skipped rather than failing the read.
func (cfg Config) installedApps(logf func(string, ...any)) ([]model.AppSpec, quadlet.Rendered, bool) {
	if cfg.AppCache == "" {
		return nil, quadlet.Rendered{}, false
	}
	entries, err := os.ReadDir(cfg.AppCache)
	if err != nil {
		return nil, quadlet.Rendered{}, false // absent = nothing installed, the shipped state
	}
	var (
		specs []model.AppSpec
		all   = quadlet.Rendered{Files: map[string]string{}}
	)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		path := filepath.Join(cfg.AppCache, e.Name())
		raw, err := os.ReadFile(path)
		if err != nil {
			logf("installed-app cache %s is unreadable (%v); bringing up without it", path, err)
			continue
		}
		spec, rendered, err := specOf(raw)
		if err != nil {
			logf("installed-app cache %s is unusable (%v); bringing up without it", path, err)
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

// specOf turns one manifest's bytes into the spec the node reports and the units it runs. One
// implementation, because the two readers must agree about what a manifest means: the node-local
// cache (installedApps, what this host was told) and the replicated volume
// (adoptVolumeApps, what this node actually promoted into).
func specOf(raw []byte) (model.AppSpec, quadlet.Rendered, error) {
	m, id, err := manifest.Parse(raw)
	if err != nil {
		return model.AppSpec{}, quadlet.Rendered{}, err
	}
	rendered, err := quadlet.Render(m, "")
	if err != nil {
		return model.AppSpec{}, quadlet.Rendered{}, err
	}
	primary := m.Primary()
	return model.AppSpec{
		Name:    m.Name,
		DataDir: quadlet.DataRoot(m.Name),
		Units:   rendered.Units,
		Unit:    quadlet.ContainerName(m.Name, primary.Name) + ".service",
		// The identity of the bytes as read, not of a re-marshalling of them -- Parse returns it
		// here for free, which is why the report reads it from the spec.
		Manifest:    string(id),
		Version:     m.Version,
		MinMemoryMB: m.MinMemoryMB,
	}, rendered, nil
}

// volumeReader is the slice of the guest adoptVolumeApps needs: what the replicated volume
// says this node runs. Narrow interface for DI, not a seam.
type volumeReader interface {
	AppList(ctx context.Context) ([]string, error)
	AppInstalled(ctx context.Context, name string) (string, error)
	SupportsAppList() bool
}

// adoptVolumeApps makes this host's view of what it runs match the volume, and caches it. A
// survivor converges from the volume at promotion and so runs apps this host was never told
// about, while the report comes from the node-local cache, holding only what this node itself
// installed or restored; without this it serves the apps at the VIP while telling the cloud it
// runs none, no rollout can be confirmed on it, and a perfectly healthy household reads "no apps"
// in every view. The volume is the truth about what runs, so the host reads it instead of
// remembering what it was told.
//
// On the promotion edge, not every cycle: only the holder can read the volume, and what it says
// changes only when someone installs, so polling would add a verb round-trip per status cycle
// for an answer that changed exactly once. Caching what it finds keeps the node's own restart
// correct: bring-up reads the cache, and a node that promoted into somebody else's install
// would otherwise come back empty. The cache becomes a projection of the volume, not a record
// of what this host did, which converge already made true of the units.
//
// Every failure is soft: a guest too old to list (no app.list), a read error or an unparseable
// manifest leaves the previous view standing. This is a report, and a wrong-but-quiet report is
// worse than a stale one only if it is silently wrong, which the log prevents. The node keeps
// serving either way; what runs is the guest's business.
func (cfg *Config) adoptVolumeApps(ctx context.Context, g volumeReader, logf func(string, ...any)) {
	if !g.SupportsAppList() {
		return // an older guest; the node-local cache is all this host can honestly report
	}
	names, err := g.AppList(ctx)
	if err != nil {
		logf("could not read the volume's apps (%v); still reporting what this node installed", err)
		return
	}
	var (
		specs []model.AppSpec
		all   = quadlet.Rendered{Files: map[string]string{}}
	)
	for _, name := range names {
		raw, err := g.AppInstalled(ctx, name)
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
		if err := cfg.cacheApp(name, []byte(raw)); err != nil {
			logf("could not cache the volume's manifest for %q (%v); this node's next bring-up will not know about it", name, err)
		}
	}
	if same(cfg.Apps, specs) {
		return
	}
	cfg.Apps, cfg.AppRendered = specs, all
	logf("adopted %d app(s) from the volume: %s", len(specs), names)
}

// same reports whether two app sets name the same apps at the same identities -- the
// question "did anything change?", not deep equality of the rendered units, which are derived.
func same(a, b []model.AppSpec) bool {
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

// mergeRendered folds one app's rendering into the union bring-up writes back. Unit
// filenames are app-prefixed by the renderer (briard-<app>-…), so the Files merge cannot
// collide; the unit lists concatenate in the order their apps were read.
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

// priorApp reads and renders the manifest on the volume for this app: an upgrade's rollback
// target. Naming the app keeps it correct at N>1, where an unnamed read would take another app's
// manifest as this install's prior and have filesToRemove delete that app's units as a renamed
// prior's orphans.
//
// Returns (nil, nil, "", "") for a fresh install, an idempotent re-install of the same manifest,
// a failed read, or a prior that no longer parses/renders (no usable rollback target, so treated
// as fresh: no rollback point is taken). Beside the rendering come the subdirs, the raw bytes
// (what the rollback point is pinned to), and the prior version, which the rendering cannot give
// and a restore reports moving between ("code too (2026.8.0 -> 2026.7.1)").
func (cfg Config) priorApp(ctx context.Context, g appInstaller, name string, incoming []byte, logf func(string, ...any)) (*quadlet.Rendered, []string, string, string) {
	raw, err := g.AppInstalled(ctx, name)
	if err != nil {
		logf("app install: could not read the installed manifest (%v); treating as a fresh install", err)
		return nil, nil, "", ""
	}
	if raw == "" || raw == string(incoming) {
		return nil, nil, "", ""
	}
	pm, _, perr := manifest.Parse([]byte(raw))
	if perr != nil {
		logf("app install: installed manifest does not parse (%v); no rollback target", perr)
		return nil, nil, "", ""
	}
	pr, rerr := quadlet.Render(pm, "")
	if rerr != nil {
		logf("app install: installed manifest does not render (%v); no rollback target", rerr)
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

// filesToRemove lists filenames in `have` that `want` does not also write: an upgrade's renamed
// prior app's orphans. A same-name upgrade shares every filename, so the list is empty and the
// content is simply overwritten.
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

// quiesce stops the given container units, last first, best-effort: a unit that is not running
// is not worth aborting for. No promoter pause, since app units are not chain members (as in
// undo). The stop is the quiesce -- SIGTERM through podman, not a flush or checkpoint -- and a
// well-behaved app writes what it holds on the way down, which is exactly what mosquitto's
// retained message needs and a live snapshot does not give.
//
// Container units, never the pod: the container holds the data Volume bind, so a clean
// `systemctl stop` of it releases the bind data.restore needs, while stopping the pod makes
// podman kill the containers out from under their units, which land in `failed`, not cleanly
// stopped (quadlet.Rendered.ContainerUnits). The pod carries no data bind, so a data op never
// needs to stop it.
func (cfg Config) quiesce(ctx context.Context, g appInstaller, containerUnits []string, logf func(string, ...any)) {
	for i := len(containerUnits) - 1; i >= 0; i-- {
		if err := g.AppStop(ctx, containerUnits[i]); err != nil {
			logf("app switch: stop %s: %v (continuing)", containerUnits[i], err)
		}
	}
}

// revert is the install's undo: undo whatever the volume holds staged for name, and report the
// terminal outcome. It runs on a detached, freshly-budgeted context (revertBudget): the most likely
// trigger is the health gate's deadline expiring, and a revert inheriting that dead deadline could
// restore nothing.
func (cfg Config) revert(ctx context.Context, g appInstaller, d api.Directive, name string, logf func(string, ...any), cause error) api.DirectiveOutcome {
	rctx, rcancel := cfg.beat.budget(context.WithoutCancel(ctx), revertBudget)
	defer rcancel()
	if err := cfg.undoInstall(rctx, g, name, logf); err != nil {
		// The install and its undo failed -- the node needs a human. Name the exact failed step.
		return api.DirectiveOutcome{ID: d.ID, State: api.OutcomeFailed,
			Detail: fmt.Sprintf("%v; AND the revert failed to %v", cause, err)}
	}
	return api.DirectiveOutcome{ID: d.ID, State: api.OutcomeRolledBack, Detail: cause.Error()}
}

// undoInstall undoes the install staged for name, reading what to undo from the volume. Nothing
// staged means the stage never landed: the accepted manifest still stands, and putting the node
// back is a converge to it (which also restarts what the install stopped).
func (cfg Config) undoInstall(ctx context.Context, g appInstaller, name string, logf func(string, ...any)) error {
	all, err := g.AppPending(ctx)
	if err != nil {
		return fmt.Errorf("read the staged install: %w", err)
	}
	for _, p := range all {
		if p.Name == name {
			return cfg.undo(ctx, g, p, logf)
		}
	}
	if _, err := g.AppConverge(ctx, ""); err != nil {
		return fmt.Errorf("converge back to the prior app: %w", err)
	}
	return nil
}

// undo returns the node to what the volume accepted for one staged install (an upgrade's prior
// manifest and data, or nothing for a fresh install) from the record alone, so a failed gate
// and a crash nobody saw are undone identically: stop the staged version -> restore the data ->
// discard the stage -> converge, which renders the accepted manifest and starts the prior
// version fresh. Data is restored before the discard because until then every converge holds
// the app back, so nothing can start the prior version on the staged version's data; an undo
// interrupted anywhere is simply run again.
//
// No promoter pause: apps are not chain members, and the units are stopped here by name. A
// failed restore does not converge, leaving the one app stopped while the node serves everything
// else and keeps its ability to fail over: silent data corruption is not recoverable; a stopped
// app is.
func (cfg Config) undo(ctx context.Context, g appInstaller, p guestagent.PendingInstall, logf func(string, ...any)) error {
	// Stop the staged version's containers (not the pod -- that would make podman kill them out
	// from under their units): releases the data subvolume's bind, which restore needs. A staged
	// manifest a crash tore cannot name its units; the accepted one names the same app's.
	units := containerUnits(p.Staged)
	if units == nil {
		units = containerUnits(p.Accepted)
	}
	cfg.quiesce(ctx, g, units, logf)
	if p.Rollback != "" {
		// Swept like any other materialised member, since the pre-upgrade point can carry a household's
		// in-flight backup restore and putting it back unswept would replay it; the markers come from
		// the accepted manifest this member is pinned to.
		//
		// ⚠️ The one caller that falls back when the guest cannot sweep: refusing would leave a failed
		// upgrade with no data rollback at all, worse than a marker nobody may ever have written. The
		// restore path, whose alternative is simply leaving the household as they are, refuses instead.
		var sweep []string
		if pm, _, err := manifest.Parse([]byte(p.Accepted)); err == nil {
			sweep = apps.RestoreMarkers(pm)
		}
		// The undo records itself as an event on a point of the failed version's data, pinned to the
		// staged manifest it ran under, so the history shows the update then its undoing, and the
		// update can still be redone. Best-effort: the rollback matters more than its entry.
		at := cfg.takenAt()
		if err := cfg.takeMemberOf(ctx, g, p.Name, p.Staged, quadlet.SnapshotMember(p.Name, quadlet.TriggerAppUndoBefore, at),
			quadlet.TriggerAppUndoBefore, quadlet.Quiesced,
			&quadlet.Event{At: at, Reasons: []quadlet.Reason{{Kind: quadlet.ReasonAppUndo, What: "The update did not work and was undone"}}},
			at); err != nil {
			logf("undo %s: could not record the undo point (%v); undoing anyway", p.Name, err)
		}
		restore := g.Restore
		if len(sweep) > 0 && !g.SupportsRestoreSweep() {
			logf("undo %s: this guest cannot sweep a restored member; rolling the data back anyway", p.Name)
			restore = func(ctx context.Context, dataDir, src string, _ []string) error {
				return g.RestoreWithoutSweep(ctx, dataDir, src)
			}
		}
		if err := restore(ctx, quadlet.DataRoot(p.Name), p.Rollback, sweep); err != nil {
			return fmt.Errorf("restore the data subvolume (the app is left stopped): %w", err)
		}
	}
	if err := g.AppDiscard(ctx, p.Name); err != nil {
		return fmt.Errorf("discard the staged install: %w", err)
	}
	// The skip list is deliberately not consulted: the accepted manifest is one this node already
	// prepared and ran, so a preparation failure here is a new fault on the undo path, and converge
	// has logged it.
	if _, err := g.AppConverge(ctx, ""); err != nil {
		return fmt.Errorf("converge back to the prior app: %w", err)
	}
	return nil
}

// undoPending undoes every install the volume holds staged, as the agent's own act, when a node
// starts serving (an agent restart, a guest relaunch, a peer's promotion): an install in
// progress holds the act slot throughout, so a stage found with the slot free has lost its
// installer. Converge has held the app back since (staging.go), so nothing ran on its data.
//
// It reports whether this promotion's check is settled: true when nothing is staged or the undo
// has started; false to ask again next cycle (an act holds the slot, or the read failed). A failed
// undo waits for the next promotion, since a restore retried every cycle would only fail every
// cycle, and leaves the app stopped, which the log names.
func (cfg Config) undoPending(ctx context.Context, g appInstaller, logf func(string, ...any)) bool {
	if cfg.acts.inFlight() {
		return false
	}
	pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	all, err := g.AppPending(pctx)
	cancel()
	if err != nil {
		logf("undo: could not read the staged installs (%v); asking again", err)
		return false
	}
	if len(all) == 0 {
		return true
	}
	return cfg.acts.runInternal("app-undo", func() {
		uctx, ucancel := cfg.beat.budget(ctx, revertBudget*time.Duration(len(all)))
		defer ucancel()
		for _, p := range all {
			logf("undo %s: an install of it was staged and never accepted; putting the node back", p.Name)
			if err := cfg.undo(uctx, g, p, logf); err != nil {
				logf("undo %s: FAILED to %v", p.Name, err)
				continue
			}
			logf("undo %s: back on what the volume accepted", p.Name)
		}
	})
}

// containerUnits names a manifest's container units, nil when it does not parse or render.
func containerUnits(raw string) []string {
	m, _, err := manifest.Parse([]byte(raw))
	if err != nil {
		return nil
	}
	r, err := quadlet.Render(m, "")
	if err != nil {
		return nil
	}
	return r.ContainerUnits
}

// awaitHealthy polls the app's own health endpoint until it comes up, or the gate expires. It
// asks by name and the guest resolves where: which host answers on the manifest's port is the
// renderer's decision, so a host-assembled address is right only by coincidence, while a name
// probes wherever the reverse proxy routes, by construction. It does not gate on the reverse
// proxy, whose /healthz reports node readiness and answers 200 on a node with nothing routed (a
// node with no apps is ready, not sick), so it would pass a broken install. That is no
// workaround: node and app health have different consequences (one can cost a failover, the
// other must only alert), and the proxy answering for the node keeps them separable.
//
// Until the deadline an error is not an unhealthy verdict: an app the guest cannot resolve yet is
// retried exactly like one not answering yet, since the two are indistinguishable from outside
// and only one is worth reverting over. Healthy passes at once; at the deadline the last answer
// settles it: unhealthy, still starting or an error reverts; unknown keeps the install and says
// so. Unknown means the app told us nothing -- our login to it failing, which is not the app --
// and reverting a working app over our own login would be the gate doing the damage it exists to
// prevent.
//
// Nor is a dead channel a verdict while the gate lasts: a guest that stalls long enough for the
// liveness rule to close the channel and then serves again is the observe loop's to reconnect,
// in seconds. The gate waits for that channel and asks over it (follow), re-pointing g so every
// later step -- the readiness verdict, the commit, a revert -- asks over it too.
func (cfg Config) awaitHealthy(ctx context.Context, g *appInstaller, app string, logf func(string, ...any)) error {
	if app == "" {
		return nil // nothing to probe (a witness never installs)
	}
	deadline := time.Now().Add(healthGate)
	for {
		h, err := (*g).AppHealthOf(ctx, app)
		if err == nil && h == apps.Healthy {
			return nil
		}
		if errors.Is(err, guestfirmware.ErrChannelDown) {
			logf("app install %s: the control channel went down during the health gate; waiting for it", app)
			wctx, cancel := context.WithDeadline(ctx, deadline)
			followed := cfg.follow(wctx, g, logf)
			cancel()
			if followed {
				continue
			}
		}
		if time.Now().After(deadline) {
			if err == nil && h == apps.HealthUnknown {
				logf("app install %s: its health was still unknown after %s; keeping it", app, healthGate)
				return nil
			}
			if err != nil {
				return fmt.Errorf("app did not become healthy within %s (%w)", healthGate, err)
			}
			return fmt.Errorf("app did not become healthy within %s (it was %s)", healthGate, h)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

// fetchManifest pulls the named app's signed manifest from the catalog and returns it with
// the exact bytes that were signed — the bytes are what gets recorded on the volume, because the
// content hash of those is the app identity.
func (cfg Config) fetchManifest(ctx context.Context, name string) (manifest.Manifest, []byte, error) {
	kr, err := selfupdate.NewKeyring(cfg.UpdateKeyring)
	if err != nil {
		return manifest.Manifest{}, nil, fmt.Errorf("release keyring: %w", err)
	}
	cat := &manifest.Catalog{URL: cfg.CatalogURL, Verifier: kr}
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

// installBudgetFor is the whole-operation bound for a manifest: the fixed
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

// applyAppRestore puts one app back to a ring member: its data, and its code with it when
// the member's pinned manifest differs from what is running.
//
// ⚠️ The order inverts revert's, and that is the whole design. A revert runs from a broken state
// and may commit, the household having already lost what it undoes; a restore runs from a healthy
// one somebody picked from a list, so every step that could fail comes before the first that
// costs anything:
//
//	ensure the images -> stop -> take the undo member -> restore data -> record the manifest
//	 -> converge
//
// Images first, as the one thing that can be simply gone: a three-month-old member pins a digest
// the registry may no longer serve, and the household must hear that while its app still runs.
// Everything before the stop leaves the node exactly as it was.
func (cfg Config) applyAppRestore(ctx context.Context, g appInstaller, d api.Directive, logf func(string, ...any)) api.DirectiveOutcome {
	failed := func(detail string) api.DirectiveOutcome {
		return api.DirectiveOutcome{ID: d.ID, State: api.OutcomeFailed, Detail: detail}
	}
	if d.Payload == "" {
		return failed("no member named")
	}
	if !g.SupportsMembers() || !g.SupportsImageEnsure() || !g.SupportsSnapshotMember() {
		return failed("this VM is too old to restore a ring member; update the VM first")
	}
	app, _, _, ok := quadlet.ParseSnapshotMember(d.Payload)
	if !ok {
		return failed(fmt.Sprintf("%q is not a ring member's name", d.Payload))
	}
	ctx, cancel := cfg.beat.budget(ctx, installBudget)
	defer cancel()

	// The member is read back from the ring, never trusted from the payload: the caller names a
	// path, and what it is (its event, the manifest it was taken under) is the guest's answer, so a
	// stale history page cannot describe a member into something it is not.
	members, err := g.Members(ctx, app)
	if err != nil {
		return failed(fmt.Sprintf("read %s's ring: %v", app, err))
	}
	var target quadlet.SnapshotEntry
	for _, m := range members {
		if m.Member == d.Payload {
			target = m
		}
	}
	if target.Member == "" {
		return failed(fmt.Sprintf("%s has no member %q (it may have been pruned)", app, d.Payload))
	}
	pm, _, err := manifest.Parse([]byte(target.Meta.Manifest))
	if err != nil {
		return failed(fmt.Sprintf("the member's pinned manifest does not parse: %v", err))
	}
	rendered, err := quadlet.Render(pm, "")
	if err != nil {
		return failed(fmt.Sprintf("the member's pinned manifest does not render: %v", err))
	}

	running, _, runningRaw, runningVersion := cfg.priorApp(ctx, g, app, nil, logf)

	// (1) The images, before anything is touched, and not through converge's warm, whose failure
	// must take the VIP down: here a failure must cost nothing at all. Room first when the member
	// runs on an image the running version does not, since that image went when its version was
	// superseded and a pull is paid for before it starts (roomForPull); a data-only undo pulls
	// nothing and grows nothing.
	if pullsAnew(rendered.ImageRefs, imageRefs(runningRaw)) {
		if refusal := cfg.roomForPull(ctx, g, pm, logf); refusal != "" {
			return failed(refusal + "; nothing was changed")
		}
	}
	for _, ref := range rendered.ImageRefs {
		if err := g.EnsureImage(ctx, ref); err != nil {
			return failed(fmt.Sprintf("%s is not available on this machine and could not be fetched (%v); nothing was changed", ref, err))
		}
	}

	dataDir := quadlet.DataRoot(app)

	// (2) The stop, container units only, never the pod (see quiesce). It comes before the undo so
	// the undo is quiesced like every member this product takes deliberately; otherwise the one
	// member a mis-click depends on would be the one with crash-consistent bytes, a bet mosquitto
	// loses (apps-pair.nix). The order risks nothing: the images are already local (step 1 must run
	// before anything stops), and a failure below puts the app back with a converge and says the
	// data was never touched.
	stopped := failed
	if running != nil {
		cfg.quiesce(ctx, g, running.ContainerUnits, logf)
		stopped = func(detail string) api.DirectiveOutcome {
			// The volume still names the running app -- AppProvision below has not run --
			// so a converge re-renders from it and starts exactly what the stop above stopped.
			// The same undo applyAppInstall uses; no new mechanism.
			if _, err := g.AppConverge(ctx, ""); err != nil {
				return failed(fmt.Sprintf("%s; AND the app could not be restarted: %v", detail, err))
			}
			return failed(detail + " (the app was restarted)")
		}
	}

	// (3) The undo, and it is the only undo a mis-click has. It exists before anything is torn
	// down: the data below is untouched until it is on disk.
	at := cfg.takenAt()
	undo := quadlet.SnapshotMember(app, quadlet.TriggerAppUndoBefore, at)
	// A night from the backup says so: the household chose it from the Backup section, not from
	// the history, and the row should read like what they did.
	back := target.Meta.TakenAt.Local().Format("Mon 2 Jan, 15:04")
	what := "Undid changes back to " + back
	if target.Meta.Trigger == quadlet.TriggerFromBackup {
		what = "Restored the backup from " + back
	}
	// An app not installed here (a new machine putting it back from the backup) gets its place
	// first: app.provision makes the data subvolume, reusing one an earlier failed install left and
	// never re-creating it, and records the night's manifest. The undo point below is then a point
	// like any other, of an empty app or of what that install left, so even this restore can be
	// undone and the swap has a live copy to replace. Decided positively, from a read that answered,
	// never from priorApp's nil, which a failed read returns too.
	unchanged, fresh := "nothing was changed", false
	if running == nil {
		raw, err := g.AppInstalled(ctx, app)
		if err != nil {
			return failed(fmt.Sprintf("read %s's installed manifest: %v; nothing was changed", app, err))
		}
		if raw == "" {
			if err := g.AppProvision(ctx, app, dataDir, quadlet.Subdirs(pm), target.Meta.Manifest); err != nil {
				return failed(fmt.Sprintf("make %s's place on this machine: %v", app, err))
			}
			logf("app restore %s: not installed here; installed empty at %s to restore into", app, pm.Version)
			unchanged = app + " is installed here but empty and stopped; restore it again to finish"
			fresh = true
		}
	}
	// Installing an uninstalled app again takes no undo point: undoing it would only put back the
	// empty app made a moment ago, and its History reads "Uninstalled", whose undo this is.
	reinstall := fresh && target.Meta.Trigger == quadlet.TriggerUninstall
	if reinstall {
		unchanged = app + ` is installed here but empty and stopped; undo "Uninstalled" in its History to finish`
	} else if err := cfg.takeMember(ctx, g, app, undo, quadlet.TriggerAppUndoBefore, quadlet.Quiesced,
		// The undo is itself an event, on the point that undoes it -- which is the redo.
		&quadlet.Event{At: at, Reasons: []quadlet.Reason{{Kind: quadlet.ReasonAppUndo, What: what}}},
		at, logf); err != nil {
		return stopped(fmt.Sprintf("take the undo point: %v; %s", err, unchanged))
	}
	// (4) The data, in verify -> materialise -> destroy order, swept of the markers a household's own
	// backup restore leaves in a member: a "before restoring backup" point put back unswept would hand
	// HA the request and the tar again, landing the household straight back where they were trying
	// to leave. A guest that cannot sweep is refused, not worked around: the alternative here is
	// leaving them exactly as they are, which is no loss at all.
	if !g.SupportsRestoreSweep() {
		return stopped("this node's guest cannot put a point back safely (it predates the restore sweep); " + unchanged)
	}
	if err := g.Restore(ctx, dataDir, target.Member, apps.RestoreMarkers(pm)); err != nil {
		// Do not converge: that would start the app on data that is neither what it was nor
		// what was asked for. A stopped app is recoverable; silent corruption is not.
		return failed(fmt.Sprintf("restore the data (the app is left stopped): %v", err))
	}
	// An uninstall's copy carries the files too, kept beside the Versions under the same name: they
	// come back with the data and the copy goes (files are never rolled back, so no History holds
	// them).
	if target.Meta.Trigger == quadlet.TriggerUninstall {
		u, ok := g.(appUninstaller)
		if !ok || !u.SupportsAppUninstall() {
			return failed("this VM cannot put an uninstalled app's files back (the app is left stopped); update the VM first")
		}
		if err := u.AppReinstate(ctx, app, target.Member); err != nil {
			return failed(fmt.Sprintf("put %s's files back (the app is left stopped): %v", app, err))
		}
	}
	// (5) The code, when the member was taken under a different one. Same rule the failed-upgrade
	// revert uses: {manifest + data} move together or the node is describing something it is not.
	if err := g.AppProvision(ctx, app, dataDir, quadlet.Subdirs(pm), target.Meta.Manifest); err != nil {
		return failed(fmt.Sprintf("record the member's manifest (the app is left stopped): %v", err))
	}
	// And node-locally, as an install's commit records it: bring-up rebuilds from this copy, and
	// the live view the status line reports is adopted from it after the act.
	if err := cfg.cacheApp(app, []byte(target.Meta.Manifest)); err != nil {
		logf("app restore %s: WARNING: could not cache the restored manifest (%v); this node reports the version it undid until it next promotes", app, err)
	}
	// (6) Converge, which re-renders from the volume and starts what it now names. Its start sample
	// is not compared with the undo's point: the app did not run between them.
	if _, err := g.AppConverge(ctx, ""); err != nil {
		return failed(fmt.Sprintf("converge onto the restored member (the app is left stopped): %v", err))
	}
	moved := "data only"
	if fresh {
		moved = "installed at " + pm.Version
	} else if runningVersion != "" && runningVersion != pm.Version {
		moved = fmt.Sprintf("code too (%s -> %s)", runningVersion, pm.Version)
	}
	back = target.Meta.TakenAt.Local().Format("Mon 2 Jan 2006, 15:04:05")
	logf("app restore %s: back to %s, %s", app, back, moved)
	// The code moved too, so the image it ran on is superseded the same way an upgrade's is; the
	// undo point names it, and putting it back re-pulls (step 1 above).
	cfg.dropSuperseded(ctx, g, runningRaw, logf)
	done := "undone back to"
	switch {
	case target.Meta.Trigger == quadlet.TriggerFromBackup:
		done = "restored from the backup, as it was at"
	case reinstall:
		done = "installed again as it was when uninstalled, at"
	}
	return api.DirectiveOutcome{ID: d.ID, State: api.OutcomeDone,
		Detail: fmt.Sprintf("%s %s, %s", done, back, moved)}
}

// takeMember renders a member's sidecar and asks the guest for it. The host records the events
// only it performs; the guest's own (agent/guestagent/inbound.go) never come through here.
//
// The caller states the consistency, never this function: only the call site knows whether it
// stopped the app first, and the clock's member, taken against a running app, says so here.
func (cfg Config) takeMember(ctx context.Context, g memberTaker, app, member string, tr quadlet.Trigger, cons quadlet.Consistency, ev *quadlet.Event, at time.Time, logf func(string, ...any)) error {
	raw, err := g.AppInstalled(ctx, app)
	if err != nil {
		return fmt.Errorf("read the running manifest: %w", err)
	}
	return cfg.takeMemberOf(ctx, g, app, raw, member, tr, cons, ev, at)
}

// takeMemberOf is takeMember with the manifest the member is pinned to named rather than read: the
// undo's point is taken of a version the volume never accepted (undo).
func (cfg Config) takeMemberOf(ctx context.Context, g memberTaker, app, raw, member string, tr quadlet.Trigger, cons quadlet.Consistency, ev *quadlet.Event, at time.Time) error {
	sidecar, err := json.Marshal(quadlet.SnapshotMeta{
		App: app, Trigger: tr, TakenAt: at, Consistency: cons, Manifest: raw, Event: ev,
	})
	if err != nil {
		return err
	}
	return g.Snapshot(ctx, quadlet.DataRoot(app), member, string(sidecar))
}

// applyAppMembers answers one app's ring as JSON. Read-only: it changes
// nothing, and the caller — the CLI's `app history`, the dashboard's history page — renders it.
func (cfg Config) applyAppMembers(ctx context.Context, g appInstaller, d api.Directive, logf func(string, ...any)) api.DirectiveOutcome {
	failed := func(detail string) api.DirectiveOutcome {
		return api.DirectiveOutcome{ID: d.ID, State: api.OutcomeFailed, Detail: detail}
	}
	if d.Payload == "" {
		return failed("no app named")
	}
	if !g.SupportsMembers() {
		return failed("this VM is too old to list an app's history; update the VM first")
	}
	mctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	members, err := g.Members(mctx, d.Payload)
	if err != nil {
		return failed(fmt.Sprintf("read %s's history: %v", d.Payload, err))
	}
	// Empty is an answer, not a failure: an app installed a minute ago has no history yet, so the
	// history can say "nothing yet" instead of an error.
	body, err := json.Marshal(members)
	if err != nil {
		return failed(err.Error())
	}
	return api.DirectiveOutcome{ID: d.ID, State: api.OutcomeDone, Detail: string(body)}
}

// usageReader is the slice of the guest applyAppUsage needs. Narrow interface for DI, not a seam.
type usageReader interface {
	Usage(ctx context.Context, app string) (dashboard.AppUsage, error)
	SupportsUsage() bool
}

// applyAppUsage answers what one app's data and history hold, as JSON. Read-only, and quick:
// the guest answers from its last walk and, when that is stale, starts the next one in its
// background.
func (cfg Config) applyAppUsage(ctx context.Context, g usageReader, d api.Directive) api.DirectiveOutcome {
	failed := func(detail string) api.DirectiveOutcome {
		return api.DirectiveOutcome{ID: d.ID, State: api.OutcomeFailed, Detail: detail}
	}
	if d.Payload == "" {
		return failed("no app named")
	}
	if !g.SupportsUsage() {
		return failed("this VM is too old to measure an app; update the VM first")
	}
	uctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	u, err := g.Usage(uctx, d.Payload)
	if err != nil {
		return failed(fmt.Sprintf("measure %s: %v", d.Payload, err))
	}
	body, err := json.Marshal(u)
	if err != nil {
		return failed(err.Error())
	}
	return api.DirectiveOutcome{ID: d.ID, State: api.OutcomeDone, Detail: string(body)}
}

// cacheEmptier is the slice of the guest applyAppCacheEmpty needs beyond an install's. Narrow
// interface for DI, not a seam.
type cacheEmptier interface {
	EmptyCache(ctx context.Context, app string) error
	SupportsEmptyCache() bool
}

// applyAppCacheEmpty empties one app's cache and starts it again: the Storage card's Clear.
// The app is stopped first -- a running app holds its cache open -- and converge starts what the
// volume names, as a restore's end does, whether or not the emptying worked.
func (cfg Config) applyAppCacheEmpty(ctx context.Context, g appInstaller, d api.Directive, logf func(string, ...any)) api.DirectiveOutcome {
	failed := func(detail string) api.DirectiveOutcome {
		return api.DirectiveOutcome{ID: d.ID, State: api.OutcomeFailed, Detail: detail}
	}
	if !manifest.ValidName(d.Payload) {
		return failed(fmt.Sprintf("%q is not an app's name", d.Payload))
	}
	e, ok := g.(cacheEmptier)
	if !ok || !e.SupportsEmptyCache() {
		return failed("this VM cannot empty an app's cache; update the VM first")
	}
	raw, err := g.AppInstalled(ctx, d.Payload)
	if err != nil {
		return failed(fmt.Sprintf("read %s's installed manifest: %v", d.Payload, err))
	}
	if raw == "" {
		return failed(d.Payload + " is not installed here")
	}
	m, _, err := manifest.Parse([]byte(raw))
	if err != nil {
		return failed(fmt.Sprintf("%s's installed manifest does not parse: %v", d.Payload, err))
	}
	if !m.UsesTier(manifest.TierCache) {
		return failed(m.Name + " keeps no cache")
	}
	r, err := quadlet.Render(m, "")
	if err != nil {
		return failed(fmt.Sprintf("%s's installed manifest does not render: %v", m.Name, err))
	}
	cfg.quiesce(ctx, g, r.ContainerUnits, logf)
	emptied := e.EmptyCache(ctx, m.Name)
	if _, err := g.AppConverge(ctx, ""); err != nil {
		return failed(fmt.Sprintf("start %s again (its cache emptied: %t): %v", m.Name, emptied == nil, err))
	}
	if emptied != nil {
		return failed(fmt.Sprintf("empty %s's cache: %v; it was started again as it was", m.Name, emptied))
	}
	logf("app cache %s: emptied; started again", m.Name)
	return api.DirectiveOutcome{ID: d.ID, State: api.OutcomeDone, Detail: m.Name + "'s cache is empty; it is starting again"}
}

// imageRemover is the slice of the guest dropSuperseded needs. Narrow interface for DI, not a seam.
type imageRemover interface {
	volumeReader
	RemoveImage(ctx context.Context, ref string) (kept bool, err error)
	SupportsImageRemove() bool
}

// dropSuperseded deletes the images `was` pinned that no app on the volume still pins, once a
// version change has committed and the old image is no longer a rollback target: what it frees
// is the "temporary" line of the state disk's accounting, so an upgrade holds two images only
// until it commits, never for good.
//
// A destructive act, gated on a positive answer: every manifest on the volume must be read before
// anything goes, and a failed read removes nothing ("I could not tell whether it is used" must
// never take the branch "it is not"). Past that it is best-effort: a failed removal (podman
// refuses an image a container still uses) is logged and the next tried, and the committed app
// is untouched either way.
func (cfg Config) dropSuperseded(ctx context.Context, g imageRemover, was string, logf func(string, ...any)) {
	old := imageRefs(was)
	if len(old) == 0 || !g.SupportsImageRemove() || !g.SupportsAppList() {
		return
	}
	names, err := g.AppList(ctx)
	if err != nil {
		logf("superseded images: could not list the volume's apps (%v); removing nothing", err)
		return
	}
	pinned := map[string]bool{}
	for _, name := range names {
		raw, err := g.AppInstalled(ctx, name)
		if err != nil || raw == "" {
			logf("superseded images: could not read %s's manifest (%v); removing nothing", name, err)
			return
		}
		refs := imageRefs(raw)
		if refs == nil {
			logf("superseded images: %s's manifest does not render; removing nothing", name)
			return
		}
		for _, ref := range refs {
			pinned[ref] = true
		}
	}
	for _, ref := range old {
		if pinned[ref] {
			continue
		}
		kept, err := g.RemoveImage(ctx, ref)
		switch {
		case err != nil:
			logf("superseded images: could not remove %s: %v", ref, err)
		case kept:
			logf("superseded images: kept %s (the guest's own image carries it)", ref)
		default:
			logf("superseded images: removed %s", ref)
		}
	}
}

// imageRefs is the image refs a manifest's units pin, or nil when it does not parse or render.
func imageRefs(raw string) []string {
	if raw == "" {
		return nil
	}
	m, _, err := manifest.Parse([]byte(raw))
	if err != nil {
		return nil
	}
	r, err := quadlet.Render(m, "")
	if err != nil {
		return nil
	}
	refs := make([]string, 0, len(r.ImageRefs))
	for _, ref := range r.ImageRefs {
		refs = append(refs, ref)
	}
	sort.Strings(refs)
	return refs
}

// roomForPull makes room on the guest's state disk for pulling m and returns why not when it
// cannot; "" means go ahead (also when the guest cannot measure its store: nothing is gated).
//
// The gate runs before the first byte moves: the need (the store's size once pulled, plus what
// the pull stages beside it until the whole pull completes -- the compressed layers -- plus the
// headroom the guest OS needs) against what the guest reports free. A pull that could only end
// in a full disk is refused here, giving both numbers, not at 90 % of the download with a unit
// that failed for a reason nobody can read.
//
// The thick state disk starts small, so it grows first (growStateDisk): the pull is where the
// host pays for what the app brings. A host that cannot pay refuses with the running apps
// untouched; any other grow failure is logged and the gate says whether the space is there
// anyway. Every pull a household asks for (an install, a History restore that changes version)
// shares this one implementation, so the two cannot disagree about room.
func (cfg Config) roomForPull(ctx context.Context, g appInstaller, m manifest.Manifest, logf func(string, ...any)) string {
	if m.InstalledSize == 0 || !g.SupportsStorageFree() {
		return ""
	}
	need := m.InstalledSize + m.Size + storageHeadroom
	if cfg.StateDisk != "" && g.SupportsStorageGrow() {
		vm := platform.Adopt(cfg.guestSpec())
		if err := cfg.growStateDisk(ctx, g, vm.ResizeStateDisk, need, logf); errors.Is(err, errHostDiskFull) {
			return fmt.Sprintf("cannot fetch %s: %v", m.Name, err)
		} else if err != nil {
			logf("%s: growing the guest's disk: %v", m.Name, err)
		}
	}
	free, total, err := g.StorageFree(ctx)
	if err != nil {
		logf("%s: cannot measure the image store (%v); going ahead unmeasured", m.Name, err)
		return ""
	}
	if free < need {
		return fmt.Sprintf("not enough space on the node: %s needs %s installed and %s more while it downloads, and the image store has %s free of %s (keeping %s for the system)",
			m.Name, gb(m.InstalledSize), gb(m.Size), gb(free), gb(total), gb(storageHeadroom))
	}
	return ""
}

// pullsAnew reports whether the member's image refs include one the running version does not
// pin -- the case where a restore has to fetch an image rather than reuse the one it runs on.
func pullsAnew(member map[string]string, running []string) bool {
	for _, ref := range member {
		if !slices.Contains(running, ref) {
			return true
		}
	}
	return false
}
