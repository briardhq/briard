// Package quadlet renders a service manifest into podman quadlet source files.
//
// This is the SWAPPABLE layer of the three-part stack the manifest decision named: manifest
// (published, signed, runtime-neutral) -> renderer (here, podman/quadlet-specific) -> systemd
// units. shared/manifest deliberately knows nothing about podman; everything podman-shaped lives
// in this package, so replacing the container runtime is replacing this file rather than
// re-cutting the published catalog.
//
// IT RUNS IN THE GUEST, and the host/guest line runs through the MANIFEST rather than through
// this package ([V3b.3](f)). The host owns identity: it fetches the manifest from the catalog,
// verifies its signature against the release keyring, and writes those exact bytes to the
// replicated volume. The guest never chooses what to run — it renders what the volume already
// says, at promotion, from a manifest whose content hash IS the service identity.
//
// Rendering has to happen there because a promoting node is alone: the volume is only readable
// once mounted, i.e. after drbd-reactor has promoted, and drbd-reactor promotes on a quorum event
// with nothing asking the host. It is also the better place on its own terms — the renderer
// belongs next to the podman it renders for, which is the same reason Dir gives for re-rendering
// rather than replicating the rendered units.
//
// This does not soften [[logic-on-host-by-default]]: Render is a pure total function of the
// manifest bytes, holding no state, no identity and no decision. What moved is which binary
// executes it, not who decides.
//
// Quadlet is podman's own systemd generator: files under /run/containers/systemd become real
// units at daemon-reload, which is what moves unit generation from BUILD time to RUN time. The
// generator needs no integration — podman ships it and virtualisation.podman.enable
// already pulls it in.
package quadlet

import (
	"fmt"
	"sort"
	"strings"

	"briard.io/agent/services"
	"briard.io/shared/manifest"
)

// Dir is where quadlet reads generated units from. /run, so it is node-local and tmpfs-backed:
// rendered units are NOT replicated and do not survive a reboot by themselves. That is
// deliberate — the DRBD volume holds the MANIFEST as the service's identity, and every node
// re-renders from it. Storing rendered units instead would mean replaying one podman version's
// output onto a node that may run another.
const Dir = "/run/containers/systemd"

// Rendered is one service's quadlet source plus the promoter chain that drives it.
type Rendered struct {
	// Files maps a filename under Dir to its content.
	Files map[string]string
	// Units is the ordered list of generated systemd units the drbd-reactor promoter must start,
	// between the data mount and the VIP. Order matters: the pod must exist before its members.
	Units []string
	// ContainerUnits are the per-container .service units WITHOUT the pod. They are what holds the
	// data Volume bind, and the ONLY units a maintenance op may stop to release it — never the pod.
	//
	// Why never the pod: `systemctl stop <pod>` makes podman `pod stop`, which kills the member
	// CONTAINERS out from under their systemd units, so each container unit exits non-zero and
	// lands in `failed` — NOT a clean stop. Stopping a container directly is a graceful stop
	// (systemd → `podman stop` → clean exit), which is what leaves the data bind released and
	// everything else undisturbed.
	//
	// A CRASHED CONTAINER NO LONGER DEMOTES THE NODE, and this comment used to say the opposite —
	// that `Requires=`-propagates-failure "IS the failover trigger; a container that crashes
	// SHOULD demote the node". [V3b.3](f) reverses that, for two reasons a pod stop's blast
	// radius had obscured: a code fault is DETERMINISTIC, so the peer running the identical
	// closure hits it identically and the failover only flaps; and one broken service must not
	// take the other N-1 down with it. The mechanism is non-membership — these units are started
	// by briard-services, not by the promoter, so `Requires=` never sees them and there is no
	// propagation left to fire. Recovery is the unit's own Restart= (see Render); the household
	// hears about it through per-service health, which reports but never gates.
	ContainerUnits []string
	// ImageUnits are the .image pre-warm units, which are NOT promoter members and NOT started
	// by systemd either — every warm goes through a caller that guards the pull (see Render).
	ImageUnits []string
	// ImageRefs maps each of those warm units to the image REF it obtains. It exists because
	// starting the unit is a `podman image pull` — measured, not assumed: quadlet generates
	// `Wants=network-online.target` + `ExecStart=podman image pull <ref>`, with no
	// already-present short-circuit. A caller that must not touch the network (bring-up, which
	// runs after every guest reboot) needs the ref to ask whether the pull is needed at all.
	ImageRefs map[string]string
	// Address is the host this service's pod answers on — a bare host, no port. A service is ONE
	// pod and a pod is ONE network namespace, so one address covers every port it listens on, and
	// the consumers (the front door's routes, the health probe) carry only the port.
	//
	// IT COMES FROM THE RENDERER BECAUSE THE ADDRESS IS THE RENDERER'S DECISION ([B.48]). The
	// manifest names a port; what host that port answers on is decided by the networking this file
	// writes into the .pod. Under host networking it is the guest's own loopback, which is why
	// every caller could get away with assembling `127.0.0.1:<port>` for itself — and why they all
	// break the moment a service asks for a private pod network ([B.48](a)), where the port is not
	// on the guest's loopback at all. Returning it from the one place that chose it is what makes
	// that a one-file change: the pod's address reaches the probe and the door together, with no
	// caller left to update.
	//
	// It never reaches the catalog. A published, signed, node-independent document cannot carry a
	// fact that differs per node and per render — and since the manifest's content hash IS the
	// service identity, putting one there would remint the identity of every entry.
	Address string
}

// prefix keeps every generated unit in one obvious namespace, so a `systemctl list-units
// 'briard-*'` shows exactly what we put on the box and nothing of the user's collides.
const prefix = "briard-"

// Render turns a manifest into quadlet source files.
//
// Two traps are avoided here on purpose, both recorded before the build:
//
//  1. The .container does NOT reference its image as `Image=foo.image`. That form gives the
//     container an AUTOMATIC dependency on the pull unit, which turns PROMOTION INTO A PULL —
//     the multi-GB cold load on the failover-critical path that warm-standby exists to prevent
//     . The container names the digest directly and sets Pull=never, so a cold node
//     FAILS FAST instead of fetching. Defer, don't pull, on the failover path: what a promoting
//     node may do about a missing image is converge's decision (it pulls, by digest), never a
//     side-effect of starting a unit.
//
//  2. AutoUpdate is never set. Podman would change image identity behind our back, breaking
//     announce-before-act and the health gate. Our upgrade path owns image identity.
//
// NOTHING carries an [Install] section, the .image pre-warm units included. Warm standby is
// still a node fact rather than a promoter one, but systemd is the wrong thing to entrust it to,
// and that is what the 2026-08-29 nightly caught: starting an .image unit IS an unconditional
// `podman image pull` (see ImageRefs), so a `WantedBy=multi-user.target` on one pulls even when
// the image is already local.
// MEASURED on a fleet node 2026-08-29, in the same millisecond: briard-dummy-app.service started
// its container off localhost/briard-dummy@sha256:9c63… while briard-dummy-app-image.service was
// failing to fetch that exact digest from a registry the node does not have. One failed unit is
// all it takes for `switch-to-configuration switch` to exit 4, which the agent reads as a failed
// commit — so this line turned every OS upgrade on a node with an installed service into a
// rollback, and took os-reboot + os-upgrade down with it.
//
// The warm itself is unaffected, because it never came from here. Install, prewarm, bring-up and
// converge each ensure their images through ServiceWarm/warmImage, which asks `podman image
// exists` before starting anything. [Install] was the one path that skipped that guard, and it
// could not even do the job it claimed: these units live on tmpfs under /run/containers/systemd,
// so at boot — the moment it was meant to fire — they do not exist yet. It only ever fired when
// something re-ran the generators after converge had written them, which is precisely the
// switch-to-configuration case above.
func Render(m manifest.Manifest, addr string) (Rendered, error) {
	if err := m.Validate(); err != nil {
		// Rendering an unvalidated manifest is how an injected line reaches a unit file. The
		// caller has normally validated already (Parse does); this is the belt to that braces.
		return Rendered{}, err
	}
	base := prefix + m.Name
	out := Rendered{Files: map[string]string{}}

	// The pod, and the ONE decision this file makes that everything else derives from.
	//
	// HOST NETWORKING IS NOW ASKED FOR, not assumed. The manifest's silence means private
	// (shared/manifest property 2), so a service reaches nothing but itself unless a catalog entry
	// deliberately says otherwise and carries that word in its identity hash.
	//
	// The address is the caller's, because allocation needs to see every service on the node and
	// this function sees one manifest. Under host networking it is the guest's own loopback and
	// nothing is written into the pod; under a private network it is this pod's address on the
	// node's pod pool, and both podman and the front door are told the same value from here — the
	// property that keeps "where it answers" a single fact rather than an agreement.
	pod := []string{"[Pod]", "PodName=" + base}
	switch {
	case m.HostNetwork():
		pod = append(pod, "Network=host")
	case addr != "":
		// A named network with an address WE chose, never one podman assigns. Two reasons, and the
		// second is why it is not merely tidier: an assigned address has to be READ BACK after the
		// pod starts, which is a second source of truth and a value that moves when a container is
		// recreated -- so the routing table would go stale with the service healthy and nothing
		// watching. Writing it means a recreated container returns to the same address.
		pod = append(pod, "Network="+PodNetwork, "IP="+addr)
		for _, p := range m.Ports {
			// Published as itself on every interface the guest holds, which is what puts a
			// non-HTTP service on the household's service address. The front door is not involved
			// and cannot be: it speaks HTTP, and this exists for the services that do not.
			pod = append(pod, fmt.Sprintf("PublishPort=%d:%d", p, p))
		}
	default:
		// UNPINNED, and legitimately so: a caller with no address is one that needs unit names and
		// image digests rather than something to start -- the host's prewarm and its node-local
		// spec cache, neither of which is serving. The rendering is a true description of the
		// service, just a less specific one, and nothing starts from it: converge re-renders with
		// the allocated address before any pod is started, on every promotion and every install.
		pod = append(pod, "Network="+PodNetwork)
	}
	out.Files[base+".pod"] = join(pod...)
	out.Units = append(out.Units, base+"-pod.service")

	for _, c := range m.Containers {
		unit := base + "-" + c.Name
		lines := []string{
			"[Container]",
			"ContainerName=" + unit,
			"Image=" + c.Image,
			"Pod=" + base + ".pod",
			"Pull=never",
		}
		if c.Mount != "" {
			// The service's data is ONE subvolume with per-container plain SUBDIRECTORIES. Not a
			// preference: data.restore runs `btrfs subvolume delete`, which refuses on a
			// subvolume containing nested subvolumes, so per-container storage cannot itself be
			// a subvolume or rollback breaks outright.
			lines = append(lines, "Volume="+DataPath(m.Name, c.Name)+":"+c.Mount)
		}
		// The binds one CATALOGUED SERVICE needs beyond its own data — Home Assistant's control
		// channel, mosquitto's config (agent/services). They cannot come from the manifest — the
		// schema refuses host binds on purpose, so that a catalog entry cannot ask for the host —
		// so they come from the product, keyed on the service's name. Render stays a pure
		// function of the manifest: the same manifest still renders the same units on every node.
		for _, v := range services.Volumes(m, c) {
			lines = append(lines, "Volume="+v)
		}
		for _, k := range sortedKeys(c.Env) {
			lines = append(lines, "Environment="+k+"="+escape(c.Env[k]))
		}
		// THE CONTAINER SUPERVISES ITSELF. Quadlet passes a [Service] section through verbatim
		// into the generated unit (measured against podman 5.8.2's own generator), and it emits
		// no Restart= of its own for a container — only for the pod.
		//
		// It is required because the service units are NOT promoter chain members ([V3b.3](f)):
		// drbd-reactor neither starts, restarts nor watches them, so without a restart policy a
		// container that dies stays dead with nothing to bring it back. That non-membership is
		// what makes "a service error alerts but never demotes" mechanically true, and this line
		// is the other half of it — the recovery the promoter used to provide, put where it
		// belongs.
		//
		// `always` rather than `on-failure`: a service container has no legitimate exit. Its job
		// is to keep serving, so exiting 0 is exactly as dead as exiting 1, and the pod unit's
		// generated `on-failure` would silently accept the first. Restarts systemd itself asked
		// for are unaffected — a `systemctl stop` (quiesce, demote) is never restarted whatever
		// the policy says.
		//
		// RestartSec spaces the retries past systemd's default start-rate limit
		// (StartLimitBurst=5 per StartLimitIntervalSec=10s), so a crash-loop retries indefinitely
		// instead of latching to `failed` and giving up on a transient cause. The loop stays
		// VISIBLE either way: NRestarts climbs, and the resource telemetry reads it per service.
		lines = append(lines, "", "[Service]", "Restart=always", "RestartSec=5")
		out.Files[unit+".container"] = join(lines...)
		out.Units = append(out.Units, unit+".service")
		out.ContainerUnits = append(out.ContainerUnits, unit+".service")

		// One .image unit per container. Two containers sharing a ref yield two oneshot units
		// that ensure the same image — idempotent and harmless, and cheaper to read than a
		// dedup table keyed on a digest.
		//
		// No [Install]: this unit exists to BE started by a guarded caller, never to start
		// itself. See Render's doc comment.
		out.Files[unit+".image"] = join(
			"[Image]",
			"Image="+c.Image,
		)
		out.ImageUnits = append(out.ImageUnits, unit+"-image.service")
		if out.ImageRefs == nil {
			out.ImageRefs = map[string]string{}
		}
		out.ImageRefs[unit+"-image.service"] = c.Image
	}
	// Where the pod above can be reached. HOST NETWORKING IS WHY IT IS LOOPBACK: the containers
	// share the guest's network namespace, so the primary's port is the guest's own port. This is
	// the one line that changes when a service asks for a private network ([B.48](a)) — and it is
	// deliberately the ONLY place that knows, so that changing it moves the front door's route and
	// the health probe's target together rather than leaving callers to agree.
	out.Address = addr
	return out, nil
}

// DataRoot is the service's single btrfs subvolume on the replicated volume.
func DataRoot(service string) string { return "/var/lib/briard/" + service }

// SnapshotPath is the pre-upgrade rollback point for a service — a read-only sibling of its data
// subvolume under the btrfs root's .snapshots dir (created by briard-data at mount), so it
// replicates with the volume. One in-flight upgrade per service, so a fixed name — and a leftover
// from a crashed upgrade needs no sweeper, because `data.snapshot` deletes an existing rollback
// point before taking the new one. A rollback point is one replaceable fact, not a series.
func SnapshotPath(service string) string {
	return "/var/lib/briard/.snapshots/" + service + "-preupgrade"
}

// DataPath is one container's plain subdirectory inside that subvolume.
func DataPath(service, container string) string { return DataRoot(service) + "/" + container }

// Subdirs lists the per-container directories the install step must create inside the subvolume,
// for the containers that actually keep state.
func Subdirs(m manifest.Manifest) []string {
	var dirs []string
	for _, c := range m.Containers {
		if c.Mount != "" {
			dirs = append(dirs, c.Name)
		}
	}
	return dirs
}

func join(lines ...string) string { return strings.Join(lines, "\n") + "\n" }

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	// Deterministic output: the rendered bytes are compared across nodes and across re-renders,
	// so map iteration order must not make two identical manifests render differently.
	sort.Strings(keys)
	return keys
}

// escape neutralises systemd's specifier syntax. A literal '%' in a value would otherwise be
// read as a specifier (%n, %i, ...) and expanded to something the catalog never wrote —
// harmless-looking in an env var, and a silent corruption of what the service was told. The
// manifest already forbids newlines (that check is the anti-injection boundary); this is the
// remaining way a legal value can mean something other than itself.
func escape(v string) string { return strings.ReplaceAll(v, "%", "%%") }

// String renders the file set in a stable order, for logs and test diffing.
func (r Rendered) String() string {
	names := make([]string, 0, len(r.Files))
	for n := range r.Files {
		names = append(names, n)
	}
	sort.Strings(names)
	var b strings.Builder
	for _, n := range names {
		fmt.Fprintf(&b, "--- %s ---\n%s", n, r.Files[n])
	}
	return b.String()
}

// ContainerName is the podman container (and unit prefix) for one container of one service. The
// renderer decides it, so everything that needs to name a running container asks here rather than
// rebuilding the string — the host's service spec does, and so does the guest when it has to
// exec into one ([V3b.4]).
func ContainerName(service, container string) string { return prefix + service + "-" + container }

// PodNetwork is the podman network every private service's pod joins. One network for the node
// rather than one per service: the pool is a /24 and the addresses inside it are what separate
// services, while a network per service would spend a subnet each to express the same thing.
//
// It is created by the node before the first private service starts, never by quadlet: an
// [Network] unit would make network creation a side-effect of starting a pod, which is the same
// trap `Image=foo.image` sets for pulls.
const PodNetwork = "briard"

// Images renders ONLY the pre-warm units — the part of a service that has no networking in it.
//
// It exists because warming is something a node does when it is NOT serving, and a node that is
// not serving has not allocated this service an address. Rendering the whole set would demand one,
// and the honest answer would be a placeholder written into a .pod file that converge is about to
// overwrite. The image units depend on nothing but the manifest's digests, so this is the whole
// truth for a prewarm rather than a subset of a lie.
func Images(m manifest.Manifest) (Rendered, error) {
	if err := m.Validate(); err != nil {
		return Rendered{}, err
	}
	base := prefix + m.Name
	out := Rendered{Files: map[string]string{}, ImageRefs: map[string]string{}}
	for _, c := range m.Containers {
		unit := base + "-" + c.Name
		out.Files[unit+".image"] = join("[Image]", "Image="+c.Image)
		out.ImageUnits = append(out.ImageUnits, unit+"-image.service")
		out.ImageRefs[unit+"-image.service"] = c.Image
	}
	return out, nil
}
