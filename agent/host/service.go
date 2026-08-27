package host

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"briard.io/agent/drbd"
	"briard.io/agent/guestagent"
	"briard.io/agent/quadlet"
	"briard.io/agent/selfupdate"
	"briard.io/shared/api"
	"briard.io/shared/atomicfile"
	"briard.io/shared/manifest"
	"briard.io/shared/model"
)

// Runtime service install. The host orchestrates; the guest is dumb hands.
//
// THE SHAPE, and why it is a maintenance bracket: a shipped node mounts, PROMOTES and
// serves the landing page with zero services, so by the time anyone installs one the resource is
// already Primary with drbd-reactor actively driving it. Installing means rewriting the promoter's
// start-list on a live promoted resource — which is an upgrade, from "empty service" to the
// current version, and belongs on the same rails as one rather than getting its own.
//
//	fetch+verify manifest -> render -> render units (this node) -> provision (Primary only)
//	  -> [ pause -> rewrite chain -> resume ] -> health-gate -> revert the chain on failure
//
// The bracket has more than one caller and no interlock. The interim guard is an
// assertion: refuse to start if the promoter is already paused, so an overlap fails loudly at the
// beginning instead of corrupting someone else's bracket from the middle. It cannot PREVENT the
// race — a pause can still land between the check and ours — and is not claimed to.

// serviceInstaller is the slice of the guest client an install drives. *guestagent.Client
// satisfies it; a fake drives the test. Narrow interface for DI, not a seam.
type serviceInstaller interface {
	Status(ctx context.Context, resource string) (model.QuorumState, error)
	ServiceRender(ctx context.Context, files map[string]string, stale []string) error
	ServiceProvision(ctx context.Context, dataDir string, subdirs []string, manifest string) error
	ServiceManifest(ctx context.Context) (string, error)
	ReactorActive(ctx context.Context) (bool, error)
	ReactorPause(ctx context.Context, snippet string) error
	ReactorResume(ctx context.Context, snippet string) error
	PayloadStart(ctx context.Context, unit string) error
	// ServiceWarm ensures an image is present, pulling only if it is missing -- see BringUp for
	// why "start the .image unit" is not the same operation.
	ServiceWarm(ctx context.Context, unit, ref string) error
	PayloadStop(ctx context.Context, unit string) error
	Adjust(ctx context.Context, req guestagent.ProvisionRequest) error
	PayloadHealth(ctx context.Context, url string) (bool, error)
	// Snapshot/Restore are the {data} half of the rollback: a broken UPGRADE must put
	// the service's data subvolume back to its pre-upgrade point, not only take the service out
	// of the promoter chain. Fresh installs (no prior data) never call them.
	Snapshot(ctx context.Context, dataDir, dest string) error
	Restore(ctx context.Context, dataDir, src string) error
}

// installBudget bounds the whole operation. Generous, because a first install legitimately pulls
// images; bounded, because a wedged install must not hold the bracket open forever — an
// unresumed promoter is a node that will not fail over.
const installBudget = 15 * time.Minute

// healthGate is how long the service gets to come up before the install is judged failed and the
// chain reverted. A container start plus an application's own boot; HA takes tens of seconds.
const healthGate = 5 * time.Minute

// revertBudget bounds the rollback, on its own DETACHED deadline. The health gate's most likely
// failure is the install budget expiring, and a revert inheriting that dead context could neither
// restore data nor resume the promoter — leaving the node mid-bracket exactly when the install
// went wrong. So the undo path never shares a deadline with the thing it undoes.
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
	ctx, cancel := cfg.beat.budget(ctx, installBudget)
	defer cancel()

	m, _, err := cfg.fetchManifest(ctx, d.Payload)
	if err != nil {
		logf("service prewarm %s: %v", d.Payload, err)
		return failed(err.Error())
	}
	rendered, err := quadlet.Render(m)
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
// payload, returning the terminal outcome (announce-before-act). Install and upgrade are the
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
	ctx, cancel := cfg.beat.budget(ctx, installBudget)
	defer cancel()

	m, raw, err := cfg.fetchManifest(ctx, d.Payload)
	if err != nil {
		logf("service install %s: %v", d.Payload, err)
		return failed(err.Error())
	}
	rendered, err := quadlet.Render(m)
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

	// ONE RUNTIME SERVICE AT A TIME, until [V3b.3](b). The node can now HOLD a set — the cache is
	// per-service, bring-up assembles the chain from all of it — but everything that DESCRIBES a
	// service through the seam is still a scalar: NodeStatus.{Image,Healthy,System} and telemetry's
	// Payload* fields. Letting a second service in before those are widened would not fail, which is
	// the problem: the node would run both and report one, so the cloud would confirm a rollout
	// against an arbitrary member and a crash-loop in the other would be invisible. A loud refusal
	// here is the honest half of "wire it, don't open it"; (b) deletes this check as part of making
	// the contract able to name WHICH service it means.
	//
	// Scoped to RUNTIME-installed services (the cache), not the build-time baked slot, which an
	// install still replaces exactly as it did before.
	if other, err := cfg.otherInstalled(m.Name); err != nil {
		return failed(fmt.Sprintf("could not read the installed-service cache: %v", err))
	} else if other != "" {
		return failed(fmt.Sprintf("%s is already installed and this node runs one service at a time; uninstall it first", other))
	}

	// What is installed NOW — the rollback target. nil on a fresh install (the shipped
	// zero-service node) or an idempotent re-install of the same manifest. Read BEFORE
	// ServiceProvision overwrites the volume's manifest, so a failed upgrade can put the prior back.
	prior, priorSubdirs, priorRaw := cfg.priorService(ctx, g, raw, logf)

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
	if !qs.Primary {
		logf("service install %s: units rendered; not Primary, so no provisioning here", m.Name)
		return api.DirectiveOutcome{ID: d.ID, State: api.OutcomeDone, Detail: "units rendered (secondary)"}
	}

	dataDir := quadlet.DataRoot(m.Name)

	// Snapshot the rollback point BEFORE the switch, whenever a service is already installed — its
	// data is what a broken upgrade can poison, and the read-only snapshot on the replicated
	// volume is what a failed gate restores. The snapshot is taken live; the data is quiesced on the
	// rollback path, where `data.restore` needs the subvolume's bind released.
	var snap string
	if prior != nil {
		snap = quadlet.SnapshotPath(m.Name)
		if err := g.Snapshot(ctx, dataDir, snap); err != nil {
			return failed(fmt.Sprintf("snapshot rollback point: %v", err))
		}
	}

	// Provision writes the NEW manifest to the volume + ensures the subvolume/subdirs. On an
	// upgrade this overwrites the prior manifest on the volume; the rollback re-writes the prior.
	if err := g.ServiceProvision(ctx, dataDir, quadlet.Subdirs(m), string(raw)); err != nil {
		return failed(fmt.Sprintf("provision storage: %v", err))
	}

	// The bracket. Everything past here must return the node to the prior service, hence the
	// explicit revert on each failure path rather than a defer that would also fire on success.
	previous := cfg.Promoter
	next := promoterUnits(rendered.Units)
	revert := func(cause error) api.DirectiveOutcome {
		return cfg.revert(ctx, g, d, dataDir, rendered, prior, priorSubdirs, priorRaw, snap, previous, logf, cause)
	}
	if err := cfg.switchService(ctx, g, next, rendered.ContainerUnits, logf); err != nil {
		return revert(fmt.Errorf("install %s: %w", m.Name, err))
	}
	logf("service install %s: promoter chain now %v; waiting for health", m.Name, next)

	// Gate on the SERVICE's own endpoint (in-guest), not the front door — see awaitHealthy. The
	// primary container's port + healthPath are manifest-guaranteed (Validate requires both).
	primary := m.Primary()
	healthURL := fmt.Sprintf("http://127.0.0.1:%d%s", primary.Port, primary.HealthPath)
	if err := cfg.awaitHealthy(ctx, g, healthURL); err != nil {
		logf("service install %s failed its health gate (%v); reverting", m.Name, err)
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
	logf("service install %s: healthy, serving", m.Name)
	// WHERE TO REACH IT, carried back as the outcome Detail. The node holds both halves and the
	// operator holds neither: the port is the manifest's (never typed by a human) and the name is
	// the one the guest publishes over mDNS. Without it the verb reports success and leaves the
	// address to be guessed, which walks the user to the front door's "nothing is routed here"
	// page and reads a working install as a broken one.
	//
	// Lead with the NAME, the doctrine install.sh already prints under: the name stays true if
	// the address moves. Only the name is offered, because it is the only half this code can
	// state truthfully -- under DHCP the VIP is acquired in-guest and rebuilt per cycle, and
	// printing a plausible-but-wrong address is the failure [V3.17] exists to end. No published
	// name (a witness, or FLOCK_NAME unset) means no URL to promise: say the port and stop.
	reach := fmt.Sprintf("it answers on port %d", primary.Port)
	if cfg.FlockName != "" {
		reach = fmt.Sprintf("reach it at http://briard-%s.local:%d/", cfg.FlockName, primary.Port)
	}
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

// otherInstalled names a runtime-installed service on this node that is NOT `name`, or "" if
// there is none. It reads the cache DIRECTORY rather than cfg.Services because the directives that
// changed it may not have been adopted into this cfg copy yet — the file is the authority on what
// is installed, which is the same reason installedServices reads it at bring-up.
//
// An absent directory is the shipped state and not an error. A directory that cannot be read IS
// one, and is reported rather than swallowed: "I could not tell" must not be spelled the same way
// as "nothing is installed" on the path whose whole job is to refuse a second service.
func (cfg Config) otherInstalled(name string) (string, error) {
	if cfg.ServiceCache == "" {
		return "", nil
	}
	entries, err := os.ReadDir(cfg.ServiceCache)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil // nothing installed yet, the shipped state
	}
	if err != nil {
		return "", err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		if got := strings.TrimSuffix(e.Name(), ".json"); got != name {
			return got, nil
		}
	}
	return "", nil
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
// installed one stopped reporting the guest's telemetry ENTIRELY -- not merely the payload
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
	specs, chain, rendered, ok := cfg.installedServices(logf)
	if !ok {
		return
	}
	cfg.Services, cfg.Promoter, cfg.ServiceRendered = specs, chain, rendered
	for _, spec := range specs {
		logf("installed service %q adopted into the running config (serving unit %s)", spec.Name, spec.ServingUnit())
	}
}

// InstalledServices reads the node-local manifest DIRECTORY and returns the service specs, the
// promoter chain they imply, and the UNION of their RENDERED UNITS. Called at bring-up so a
// restarted agent rebuilds the chain it had, rather than reverting to whatever the environment
// describes.
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
func (cfg Config) installedServices(logf func(string, ...any)) ([]model.ServiceSpec, []string, quadlet.Rendered, bool) {
	if cfg.ServiceCache == "" {
		return nil, nil, quadlet.Rendered{}, false
	}
	entries, err := os.ReadDir(cfg.ServiceCache)
	if err != nil {
		return nil, nil, quadlet.Rendered{}, false // absent = nothing installed, the shipped state
	}
	var (
		specs []model.ServiceSpec
		units []string
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
		m, _, err := manifest.Parse(raw)
		if err != nil {
			logf("installed-service cache %s is unusable (%v); bringing up without it", path, err)
			continue
		}
		rendered, err := quadlet.Render(m)
		if err != nil {
			logf("installed-service cache %s does not render (%v); bringing up without it", path, err)
			continue
		}
		primary := m.Primary()
		specs = append(specs, model.ServiceSpec{
			Name:    m.Name,
			Image:   primary.Image,
			DataDir: quadlet.DataRoot(m.Name),
			Units:   rendered.Units,
			Unit:    "briard-" + m.Name + "-" + primary.Name + ".service",
		})
		units = append(units, rendered.Units...)
		mergeRendered(&all, rendered)
	}
	if len(specs) == 0 {
		return nil, nil, quadlet.Rendered{}, false
	}
	return specs, promoterUnits(units), all, true
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

// PriorService reads the manifest currently on the volume and renders it — the rollback target for
// an upgrade. Returns (nil, nil, "") for a fresh install, an idempotent re-install of the same
// manifest, or a prior that no longer parses/renders (no usable rollback target → treated as
// fresh; the gate still guards the new service, so the worst case is a rollback to empty rather
// than to the broken new one, never to it). The raw bytes and subdirs come back too, because the
// rollback re-provisions them as the volume's identity.
func (cfg Config) priorService(ctx context.Context, g serviceInstaller, incoming []byte, logf func(string, ...any)) (*quadlet.Rendered, []string, string) {
	raw, err := g.ServiceManifest(ctx)
	if err != nil {
		logf("service install: could not read the installed manifest (%v); treating as a fresh install", err)
		return nil, nil, ""
	}
	if raw == "" || raw == string(incoming) {
		return nil, nil, ""
	}
	pm, _, perr := manifest.Parse([]byte(raw))
	if perr != nil {
		logf("service install: installed manifest does not parse (%v); no rollback target", perr)
		return nil, nil, ""
	}
	pr, rerr := quadlet.Render(pm)
	if rerr != nil {
		logf("service install: installed manifest does not render (%v); no rollback target", rerr)
		return nil, nil, ""
	}
	return &pr, quadlet.Subdirs(pm), raw
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

// SwitchService rewrites the promoter start-list to `chain` inside the bracket, quiescing the
// service's CONTAINER units first so the ones drbd-reactor restarts on resume pick up the freshly
// rendered /run content. On a fresh install the containers are not running and the stop is a
// harmless no-op; on an upgrade (same service name ⇒ same unit names) it is what makes the new
// version actually take effect, since systemd does not restart an already-active unit on
// daemon-reload alone. containerUnits, NEVER the pod — see quiesce.
func (cfg Config) switchService(ctx context.Context, g serviceInstaller, chain, containerUnits []string, logf func(string, ...any)) error {
	if err := g.ReactorPause(ctx, cfg.ReactorSnippet); err != nil {
		return fmt.Errorf("pause promoter: %w", err)
	}
	cfg.quiesce(ctx, g, containerUnits, logf)
	if err := cfg.adjustChain(ctx, g, chain); err != nil {
		// Resume regardless: a paused promoter is a node that will not fail over, worse than a
		// failed rewrite.
		if rerr := g.ReactorResume(ctx, cfg.ReactorSnippet); rerr != nil {
			logf("service install: promoter left PAUSED after a failed rewrite: %v", rerr)
		}
		return err
	}
	if err := g.ReactorResume(ctx, cfg.ReactorSnippet); err != nil {
		return fmt.Errorf("resume promoter: %w", err)
	}
	return nil
}

// Quiesce stops the given CONTAINER units, best-effort — a unit that is not running is not a
// failure worth aborting for. Called with the promoter PAUSED, so the stop is not read as a fault.
//
// It must be handed CONTAINER units, never the pod. The container holds the data Volume bind, so a
// clean `systemctl stop` of it releases the bind (what data.restore needs) AND leaves the
// drbd-reactor target up. Stopping the POD instead makes podman kill the containers ungracefully →
// their units go to `failed` → the target `Requires=` a failed member → the target deactivates →
// `briard-data` (PartOf) unmounts the SHARED DRBD volume, taking every other service with it. The
// full trace is on quadlet.Rendered.ContainerUnits. The pod carries no data bind, so a data op
// never needs to stop it.
func (cfg Config) quiesce(ctx context.Context, g serviceInstaller, containerUnits []string, logf func(string, ...any)) {
	for i := len(containerUnits) - 1; i >= 0; i-- {
		if err := g.PayloadStop(ctx, containerUnits[i]); err != nil {
			logf("service switch: stop %s: %v (continuing)", containerUnits[i], err)
		}
	}
}

// AdjustChain writes the reactor start-list. Adjust writes the .res file UNCONDITIONALLY, so the
// full resource config must ride along or the definition is truncated; `drbdadm adjust` against an
// unchanged .res is a no-op, so passing it costs nothing and omitting it would be catastrophic.
func (cfg Config) adjustChain(ctx context.Context, g serviceInstaller, chain []string) error {
	req := guestagent.ProvisionRequest{
		Resource:      cfg.Resource.Name,
		ResConfig:     cfg.Resource.Config(),
		ReactorConfig: drbd.ReactorConfig(cfg.Resource.Name, chain),
	}
	if err := g.Adjust(ctx, req); err != nil {
		return fmt.Errorf("rewrite promoter chain: %w", err)
	}
	return nil
}

// Revert returns the node to the service it ran before this install — the prior manifest and its
// data (an upgrade), or the empty zero-service chain (a fresh install) — and reports the terminal
// outcome. It runs on a DETACHED, freshly-budgeted context (revertBudget): the most likely trigger
// is the health gate's deadline expiring, and a revert inheriting that dead deadline could restore
// nothing.
//
// The whole rollback runs under ONE pause so the promoter cannot restart the broken service between
// steps: stop the broken units → restore the data → put the prior units + manifest back → point the
// chain at the prior → resume, which starts the prior service fresh. Data is restored BEFORE code,
// the order the guest manager's rollback uses — and if the data cannot be rolled back the
// promoter is deliberately left PAUSED rather than resumed onto poisoned data.
func (cfg Config) revert(ctx context.Context, g serviceInstaller, d api.Directive, dataDir string, next quadlet.Rendered, prior *quadlet.Rendered, priorSubdirs []string, priorRaw, snap string, previous []string, logf func(string, ...any), cause error) api.DirectiveOutcome {
	rctx, rcancel := cfg.beat.budget(context.WithoutCancel(ctx), revertBudget)
	defer rcancel()

	bothFailed := func(what string, err error) api.DirectiveOutcome {
		// The install AND its revert failed — the node needs a human. Name the exact failed step.
		return api.DirectiveOutcome{ID: d.ID, State: api.OutcomeFailed,
			Detail: fmt.Sprintf("%v; AND the revert failed to %s: %v", cause, what, err)}
	}

	if err := g.ReactorPause(rctx, cfg.ReactorSnippet); err != nil {
		return bothFailed("pause the promoter", err)
	}
	// Stop the just-installed CONTAINERS (not the pod — that would unmount the shared volume): releases the data subvolume's bind (restore needs it) and lets them start fresh on
	// resume.
	cfg.quiesce(rctx, g, next.ContainerUnits, logf)

	if snap != "" {
		if err := g.Restore(rctx, dataDir, snap); err != nil {
			// Data could not be rolled back. Do NOT resume onto the prior units — they would run on
			// the poisoned data. Leave the promoter paused for a human: a node that will not fail
			// over is recoverable, silent data corruption is not.
			return bothFailed("restore the data subvolume (promoter left PAUSED)", err)
		}
	}
	if prior != nil {
		if err := g.ServiceRender(rctx, prior.Files, filesToRemove(next.Files, prior.Files)); err != nil {
			return bothFailed("re-render the prior units", err)
		}
		if err := g.ServiceProvision(rctx, dataDir, priorSubdirs, priorRaw); err != nil {
			return bothFailed("re-record the prior manifest", err)
		}
	}
	if err := cfg.adjustChain(rctx, g, previous); err != nil {
		if rerr := g.ReactorResume(rctx, cfg.ReactorSnippet); rerr != nil {
			logf("service install: promoter left PAUSED after a failed revert rewrite: %v", rerr)
		}
		return bothFailed("rewrite the promoter chain", err)
	}
	if err := g.ReactorResume(rctx, cfg.ReactorSnippet); err != nil {
		return bothFailed("resume the promoter", err)
	}
	return api.DirectiveOutcome{ID: d.ID, State: api.OutcomeRolledBack, Detail: cause.Error()}
}

// AwaitHealthy polls the SERVICE's OWN health endpoint (its port + healthPath from the manifest,
// probed in-guest at 127.0.0.1) until it comes up, or the gate expires.
//
// It does NOT gate on the front door. The reverse-proxy's /healthz reports NODE readiness — on a
// shipped zero-service node it answers "no backend configured" (200) and never learns about a
// runtime-installed service (its backend is baked at guest-build time from cfg.image) — so gating
// on it passes a broken install. Probing the service directly is also the
// shape multi-service needs: each service gated on its own endpoint, not one VIP /healthz that
// cannot reflect N of them. The front door's per-domain routing to runtime services (and the
// matching steady-state health) is deferred with the routing work.
func (cfg Config) awaitHealthy(ctx context.Context, g serviceInstaller, healthURL string) error {
	if healthURL == "" {
		return nil // no probe endpoint (a witness never installs; a valid manifest always has one)
	}
	deadline := time.Now().Add(healthGate)
	for {
		ok, err := g.PayloadHealth(ctx, healthURL)
		if err == nil && ok {
			return nil
		}
		if time.Now().After(deadline) {
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
