// Package quadlet renders a service manifest into podman quadlet source files.
//
// This is the SWAPPABLE layer of the three-part stack the manifest decision named: manifest
// (published, signed, runtime-neutral) -> renderer (here, podman/quadlet-specific) -> systemd
// units. shared/manifest deliberately knows nothing about podman; everything podman-shaped lives
// in this package, so replacing the container runtime is replacing this file rather than
// re-cutting the published catalog.
//
// It runs on the HOST, not in the guest ([[logic-on-host-by-default]]): the guest verb that
// consumes this output only writes bytes to disk and reloads systemd. That keeps the logic
// unit-testable without a VM and the guest dumb-handed.
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
	// Why never the pod: `systemctl stop <pod>` makes podman `pod stop`,
	// which kills the member CONTAINERS out from under their systemd units, so each container unit
	// exits non-zero and lands in `failed` — NOT a clean stop. drbd-reactor's promoter has the
	// target `Requires=` each member, and `Requires=` propagates a FAILURE (though not a clean
	// stop). So a pod stop => container `failed` => the target deactivates => `briard-data` (which
	// is `PartOf` the target) stops => the shared DRBD volume UNMOUNTS, taking every other service
	// on the node with it. Stopping a container directly is a graceful stop (systemd → `podman
	// stop` → clean exit), so `Requires=` stays quiet and the target — and the mount — stay up.
	// (That Requires-propagates-failure edge is not a bug to route around: it IS the failover
	// trigger — a container that crashes SHOULD demote the node. Maintenance just must not fire it.)
	ContainerUnits []string
	// ImageUnits are the .image pre-warm units, which are NOT promoter members — they are
	// boot-time and run on every node (see Render).
	ImageUnits []string
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
//     FAILS FAST instead of fetching, exactly as briard-converge already refuses rather than
//     pulling. Defer, don't pull, on the failover path.
//
//  2. AutoUpdate is never set. Podman would change image identity behind our back, breaking
//     announce-before-act and the health gate. Our upgrade path owns image identity.
//
// The .image units carry [Install] WantedBy=multi-user.target: boot-time, on every node, NOT
// promoter-gated — structurally identical to today's briard-payload-warm. Nothing else gets an
// [Install] section, because the promoter decides what runs.
func Render(m manifest.Manifest) (Rendered, error) {
	if err := m.Validate(); err != nil {
		// Rendering an unvalidated manifest is how an injected line reaches a unit file. The
		// caller has normally validated already (Parse does); this is the belt to that braces.
		return Rendered{}, err
	}
	base := prefix + m.Name
	out := Rendered{Files: map[string]string{}}

	// The pod. Host networking is OUR uniform substrate decision, not a per-service capability —
	// which is why the manifest cannot ask for it (or refuse it). It matches what the baked
	// payload slot already does (--network=host) and what service discovery on a home LAN needs.
	out.Files[base+".pod"] = join(
		"[Pod]",
		"PodName="+base,
		"Network=host",
	)
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
		for _, k := range sortedKeys(c.Env) {
			lines = append(lines, "Environment="+k+"="+escape(c.Env[k]))
		}
		out.Files[unit+".container"] = join(lines...)
		out.Units = append(out.Units, unit+".service")
		out.ContainerUnits = append(out.ContainerUnits, unit+".service")

		// One .image unit per container. Two containers sharing a ref yield two oneshot units
		// that ensure the same image — idempotent and harmless, and cheaper to read than a
		// dedup table keyed on a digest.
		out.Files[unit+".image"] = join(
			"[Image]",
			"Image="+c.Image,
			"",
			"[Install]",
			"WantedBy=multi-user.target",
		)
		out.ImageUnits = append(out.ImageUnits, unit+"-image.service")
	}
	return out, nil
}

// DataRoot is the service's single btrfs subvolume on the replicated volume.
func DataRoot(service string) string { return "/var/lib/briard/" + service }

// SnapshotPath is the pre-upgrade rollback point for a service — a read-only sibling of its data
// subvolume under the btrfs root's .snapshots dir (created by briard-data at mount), so it
// replicates with the volume. One in-flight upgrade per service, so a fixed name; a leftover
// from a crashed upgrade is pruned by the retention GC (data.gc).
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

// Chain returns the promoter start-list for a node running this service: the data mount, then
// the pod, then its members, then the VIP. Naming the members explicitly is what the quadlet
// spike proved is required — starting the pod service does not start its containers.
func Chain(r Rendered) []string {
	units := append([]string{"briard-data.service"}, r.Units...)
	return append(units, "briard-vip.service")
}

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
