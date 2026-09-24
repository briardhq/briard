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
	"strconv"
	"strings"
	"time"

	"briard.io/agent/guestfirmware"
	"briard.io/agent/services"
	"briard.io/shared/manifest"
)

// Dir is where quadlet reads generated units from. /run, so it is node-local and tmpfs-backed:
// rendered units are NOT replicated and do not survive a reboot by themselves. That is
// deliberate — the DRBD volume holds the MANIFEST as the service's identity, and every node
// re-renders from it. Storing rendered units instead would mean replaying one podman version's
// output onto a node that may run another.
const Dir = "/run/containers/systemd"

// ImagePullTimeout bounds one `.image` unit's pull when the manifest does not say how big the
// image is. See the emit site below for why it is this long and why tightening it is the wrong
// instinct — in short: an expiry throws away the layer in flight ([B.56], measured), so the bound
// exists to make a hopeless pull loud, not to make failure prompt. Named here rather than
// inlined because the acceptance test varies it.
const ImagePullTimeout = 45 * time.Minute

// The bound a SIZED entry gets ([V3b.31k]): a fixed allowance for everything that is not bytes
// (the registry round-trips, the token dance, six layers starting at once, decompression at the
// end), plus the download itself at the slowest link a household is asked to have. 5 Mbit/s is
// deliberately below any broadband plan and above the mobile-tethering floor; a link slower than
// that expires a layer or two per attempt and converges anyway, because finished layers are kept.
// Home Assistant (622 MB) gets ~22 minutes; a 10 MB broker gets the allowance plus 16 seconds.
const (
	PullAllowance = 5 * time.Minute
	PullBitrate   = 5_000_000 // bits per second
)

// PullTimeout is the bound for an image of size bytes: the allowance plus the download at
// PullBitrate, or ImagePullTimeout when the manifest carries no size.
func PullTimeout(size int64) time.Duration {
	if size <= 0 {
		return ImagePullTimeout
	}
	return PullAllowance + time.Duration(size*8/PullBitrate)*time.Second
}

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
		// THE RING'S GENERIC HOOK ([B.143]): a member of this service's data, taken while the
		// container is stopped, at the one boundary visible from outside it. Every catalogued
		// service gets this; Home Assistant adds its own internal restarts through the inbound
		// channel, which only it can see.
		//
		// ⚠️ THE LEADING `-` IS NOT OPTIONAL. Without it a snapshot that fails takes the
		// household's service down with it, and the trade is the wrong way round: a ring missing
		// a member is nothing, a service that will not start is an outage. The binary holds
		// itself to the same bar and exits 0 regardless, so this is belt-and-braces — `-` alone
		// hides the reason, and the agent's own line in the journal is what says it.
		//
		// ONLY the container that holds the data. A service's other containers share its
		// subvolume and would each take a member of the same bytes at every start.
		if c.Mount != "" {
			lines = append(lines, "ExecStartPre=-"+agentBin()+" --service-starting="+m.Name)
			// AND THE OTHER HALF OF THE SAME QUESTION ([B.143]): a stop that ended cleanly leaves a
			// marker saying this service's data was flushed, and the next start reads it to say
			// what its member's bytes ARE. A node that dies writes nothing, so the member taken
			// when a survivor promotes says crash-consistent — which it is.
			//
			// ⚠️ ExecStopPost, never ExecStop: it runs after the container is actually down, on
			// every stop systemd performs, and it is the only place $SERVICE_RESULT exists. The
			// leading `-` for the same reason the start hook has one — a household must never lose
			// a service because a marker could not be written.
			lines = append(lines, "ExecStopPost=-"+agentBin()+" --service-stopped="+m.Name)
		}
		out.Files[unit+".container"] = join(lines...)
		out.Units = append(out.Units, unit+".service")
		out.ContainerUnits = append(out.ContainerUnits, unit+".service")

		// One .image unit per container. Two containers sharing a ref yield two oneshot units
		// that ensure the same image — idempotent and harmless, and cheaper to read than a
		// dedup table keyed on a digest.
		//
		// No [Install]: this unit exists to BE started by a guarded caller, never to start
		// itself. See Render's doc comment.
		//
		// AND IT IS BOUNDED, because otherwise it is not ([B.56]). Quadlet generates a
		// Type=oneshot unit (measured against podman 5.8.2's own generator), and systemd disables
		// TimeoutStartSec= by default for oneshot — so an unbounded pull is the shipped behaviour.
		// converge starts this unit on the PROMOTION path, so a registry that dribbles bytes holds
		// the promotion open indefinitely: nixosTest/cold-converge-pull.nix measured a node sitting
		// Primary with the volume mounted, no VIP, no services, restarts=0 and nothing in `failed`
		// for the full 420 s it was watched. Nothing was broken enough to alert on.
		//
		// 45 MINUTES, AND DELIBERATELY NOT TIGHTER. The bound's job is to make a hopeless pull
		// LOUD, not to make failure prompt, because a retry is expensive in a way that is easy to
		// get wrong: nixosTest/image-pull-resume.nix measured that an interrupted pull does NOT
		// resume — a SIGTERM at 43% of a 25 MiB image cost the full 25 MiB again on the next
		// attempt (1.43x on the wire). Every expiry throws away all progress, so a bound near what
		// a household's link actually needs would never converge; it would re-download the same
		// blob until something gave up for good. Tighter is not safer here, it is worse.
		//
		// WHAT SURVIVES IS WHOLE LAYERS, and that is what makes 45 minutes enough rather than a
		// guess about total image size. A layer that FINISHED before the interrupt is not fetched
		// again: an image of 5x1 MiB plus one 20 MiB layer, interrupted with the small ones done
		// and the big one at 8/20 MiB, re-requested none of the five on the next attempt — the
		// registry never heard about them again (450 B of trivia aside). Only the layer in flight
		// is lost, every time.
		//
		// So progress across attempts is MONOTONIC, and the bound does not have to clear the
		// whole image on a household's worst link — it has to clear the image's LARGEST SINGLE
		// LAYER. That is a far weaker requirement, and 45 minutes covers a several-hundred-MB
		// layer at well under 2 Mbit/s.
		//
		// This corrects an earlier reading of the same rig, recorded because the mistake is easy
		// to repeat: a first cut used six EQUAL layers, saw a full re-fetch, and concluded that
		// nothing is kept. podman copies layers CONCURRENTLY, so equal layers advance in lockstep
		// and all of them were partial at the interrupt — there were no finished layers to keep,
		// which is not the same fact at all. Uneven layers are the shape real images have, and
		// the only shape in which the question has an observable answer.
		//
		// It also sits well clear of the faults that heal themselves. A 20 s outage mid-transfer
		// was absorbed by TCP retransmission with no re-fetch at all (1.00x, a single
		// `Copying blob` line spanning the break) — podman's own --retry never ran. A timeout that
		// fired during one of those would convert a self-healing blip into a full re-download.
		// PrivateTmp KEEPS THE BOUND FROM BECOMING A DISK LEAK, and without it the timeout above
		// would be a slow-motion ENOSPC. A pull stages every byte through
		// /var/tmp/container_images_storage<random>/ and only moves it into the graph root when
		// the copy COMPLETES — so a pull that is killed leaves the whole partial image behind, and
		// podman never comes back for it (measured: a successful pull cleans up, an interrupted
		// one does not; three interrupted pulls left three directories). A 45-minute expiry on a
		// slow link would abandon ~2.7 GB of Home Assistant each time, against the ~11 GB the
		// 16 GiB guest root has spare for a service AND its upgrade (disk-image.nix) — and the
		// households that hit the timeout are exactly the ones that hit it repeatedly.
		//
		// systemd gives the unit its own /var/tmp and removes it when the unit stops, HOWEVER it
		// stops — which an ExecStopPost cleanup would not, since a SIGKILL skips it. Measured
		// both ways: an interrupted pull now leaves 0 KB behind, and a successful one still lands
		// in the graph root untouched.
		//
		// AND IT IS DISK-BACKED, which was the thing worth checking rather than assuming: a
		// tmpfs private /var/tmp would run the copy through RAM and trade this leak for an OOM on
		// the first multi-GB image. Measured — the scratch appears as a real
		// systemd-private-*-briard-…-image.service-* directory on the root filesystem (11 MB of a
		// 25 MiB pull visible there mid-transfer), not in memory.
		//
		// Only on the .image unit: this one pulls and exits. The .container unit runs the service
		// and is deliberately left alone.
		//
		// SIZED BY THE MANIFEST since [V3b.31k]: an entry that says how many bytes it downloads
		// gets PullTimeout(size) -- the fixed allowance plus the download at the floor bitrate --
		// and the 45 minutes above stay for an entry that does not. The manifest's total is the
		// service's, not this container's, so a multi-container service bounds each image by the
		// whole: generous per image, and the only number the manifest carries.
		out.Files[unit+".image"] = join(
			"[Image]",
			"Image="+c.Image,
			"",
			"[Service]",
			"TimeoutStartSec="+strconv.Itoa(int(PullTimeout(m.Size).Seconds())),
			"PrivateTmp=true",
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

// SnapshotsDir is where every member of a service's ring lives: read-only siblings of the data
// subvolume under the btrfs root's .snapshots dir (created by briard-primary-storage at mount), so
// they replicate with the volume.
const SnapshotsDir = "/var/lib/briard/.snapshots/"

// snapshotStamp is the member name's time format: fixed width, UTC, and chosen so that lexical
// order over names IS chronological order. Every reader of the ring leans on that.
const snapshotStamp = "20060102T150405Z"

// A Trigger says what caused a member to be taken, and is the half of its name a human scans.
// The set is closed because the name parser enumerates it: a trigger nobody listed is a member
// nobody can read back. What a member is KEPT for is not its trigger but the event it anchors
// (history.go, [B.167]).
type Trigger string

const (
	// TriggerUpgrade is the pre-upgrade rollback point — the member [B.121] rules must be taken
	// on a STOPPED container. It anchors the update's event.
	TriggerUpgrade Trigger = "upgrade"
	// TriggerStart is an ordinary service start — the container's, or for a service that can tell
	// us about its own (Home Assistant's s6 `run` wrapper), one of those. It is the only trigger
	// the rate limit may skip.
	TriggerStart Trigger = "start"
	// TriggerDaily is the member taken BY THE CLOCK rather than by an event ([B.143]): a stable
	// service can run for a month without a restart, and the clock is what still samples it.
	//
	// It is the one member taken against a RUNNING service, so it is Crash unless the service held
	// still for it.
	TriggerDaily Trigger = "daily"
	// The restore PAIR ([B.143]): the undo taken before a restore commits, and the waypoint taken
	// after it. The first anchors the restore's event; the second is a baseline (Baseline).
	TriggerRestoreBefore Trigger = "restore-before"
	TriggerRestoreAfter  Trigger = "restore-after"
	// TriggerUpgradeAfter is the sample taken once an update has passed its gates ([B.167]): a
	// baseline (Baseline), so the update's own first-boot rewrites are not read as a change a
	// household made. Taken while the service runs, so its class is the quiesce's answer.
	TriggerUpgradeAfter Trigger = "upgrade-after"
)

// SnapshotMemberService reads the service out of a member's name, and reports whether the name is
// one of ours at all. The sweep over `.snapshots` has to tell our members from anything else a
// human or a future feature left there, and the name is the only thing it can ask.
func SnapshotMemberService(name string) (string, bool) {
	svc, _, _, ok := ParseSnapshotMember(name)
	return svc, ok
}

// SnapshotMemberTime reads the moment a member was taken out of its name. It is what the ring's
// rate limit and its pruning order read, and it is why the stamp is in the name rather than only
// in the sidecar: answering "how old is the newest member" must not cost a file read per member.
func SnapshotMemberTime(name string) (time.Time, bool) {
	_, _, at, ok := ParseSnapshotMember(name)
	return at, ok
}

// ParseSnapshotMember splits `<service>-<trigger>-<stamp>`. The service may itself contain dashes
// ("home-assistant"), so it is parsed from the RIGHT: the stamp is fixed width and the trigger
// comes from a closed set, which leaves whatever precedes them as the name.
func ParseSnapshotMember(name string) (service string, trigger Trigger, at time.Time, ok bool) {
	name = strings.TrimPrefix(name, SnapshotsDir)
	for _, t := range []Trigger{TriggerUpgrade, TriggerUpgradeAfter, TriggerStart, TriggerDaily, TriggerRestoreBefore, TriggerRestoreAfter} {
		suffix := "-" + string(t) + "-"
		i := strings.LastIndex(name, suffix)
		if i <= 0 {
			continue
		}
		stamp := name[i+len(suffix):]
		parsed, err := time.Parse(snapshotStamp, stamp)
		if err != nil {
			continue
		}
		return name[:i], t, parsed.UTC(), true
	}
	return "", "", time.Time{}, false
}

// SnapshotMember is one member's subvolume path: service, trigger and the moment it was taken.
//
// A SERIES, WHICH IS THE WHOLE OF [B.143]. This replaced a single fixed `<service>-preupgrade`
// name whose doc read "a rollback point is one replaceable fact, not a series" — true while the
// only member was the one an in-flight upgrade needed, and the reason `data.snapshot` used to
// DELETE an existing point before taking the new one. Both are retired together: members are
// distinct by construction now, so nothing is replaced and the verb refuses a collision instead
// of resolving it.
//
// Second precision and UTC, so two members' TIMES compare correctly once parsed, and so two nodes
// in different zones taking a member at the same instant agree on what it is called.
//
// ⚠️ THE NAME IS NOT A CHRONOLOGICAL SORT KEY, and reading it as one is a real bug this format
// invites: the TRIGGER sits between the service and the stamp, so every `-start-` member sorts
// before every `-upgrade-` one whatever their times. Readers order by SnapshotMemberTime.
// (Trigger-before-stamp is kept because it is what makes the directory readable to a human
// scanning it, which is the other job the name has.)
func SnapshotMember(service string, trigger Trigger, at time.Time) string {
	return SnapshotsDir + service + "-" + string(trigger) + "-" + at.UTC().Format(snapshotStamp)
}

// SnapshotSidecar is where a member's metadata sits: beside the subvolume, never inside it.
// Inside is impossible — the member is read-only from the instant it exists — and that is also
// why the sidecar cannot be atomic with it. The guest writes it immediately after the snapshot
// and removes the member if it cannot, so "every member has a sidecar" is an invariant the
// history may rely on rather than a hope.
func SnapshotSidecar(member string) string { return member + ".json" }

// A Consistency says what a member's bytes ARE, which is not the question its trigger answers
// ([B.143]).
//
// QUIESCED means the data was flushed by a clean stop, or held still across the take: restoring it
// gives the service exactly what it had. CRASH means it was not — the bytes are whatever a power
// cut would have left, and restoring one is a bet on the service's own recovery path. That bet is
// measured-good for Home Assistant (WAL replay, nixosTest/hass-upgrade.nix) and measured-BAD for
// mosquitto, which lost exactly the retained message somebody would roll back FOR
// (nixosTest/services-pair.nix). Per-service, in other words — which is why a member carries the
// answer rather than leaving every reader to infer one.
//
// IT IS THE TAKER'S OWN KNOWLEDGE and needs no crash detection: whoever takes a member knows
// whether it created the stopped window. But ⚠️ A STOPPED CONTAINER IS NOT THE SAME FACT AS
// FLUSHED DATA, which is why this is not derived from the Trigger. The member after a promotion
// has nothing running either, yet the old primary never shut the service down, so its bytes are
// crash-consistent under an ordinary `start` trigger. That case is also the one thing this field
// can still get wrong: the failover trigger it would need does not exist yet, so a promotion start
// is labelled like any other start.
//
// The empty value means UNRECORDED — a member taken before this field existed. A reader must treat
// it as unknown and never as quiesced: [B.32]'s integrity check may trust only what says so.
type Consistency string

const (
	// Quiesced: every member taken with the container stopped — the pre-start hook, the
	// pre-upgrade point, and both halves of the restore pair.
	Quiesced Consistency = "quiesced"
	// Crash: taken against a running service. The nightly is the one member that is meant to be
	// this, and only until its quiesce (a truncating WAL checkpoint plus a held transaction)
	// promotes it.
	Crash Consistency = "crash"
)

// Note is what a household is told about a point's consistency, in one phrase, or "" when there
// is nothing to say ([B.143]).
//
// ONE SENTENCE, ONE PLACE. Both front ends render it — `briard app history` and the dashboard's
// history page — and a household that heard two different descriptions of the same fact would have to
// work out whether they meant the same thing. It stays out of our vocabulary too: "quiesced" and
// "crash-consistent" are words for this file, not for somebody's kitchen ([V3c.10]).
//
// The QUIET case is quiet on purpose: most points are clean, and a note on every line is a note
// nobody reads.
func (c Consistency) Note() string {
	switch c {
	case Quiesced:
		return ""
	case Crash:
		return "taken while the app was running"
	default:
		return "taken by an older briard; unverified"
	}
}

// SnapshotMeta is a member's sidecar — the event it is the point of, and what a restore needs.
//
// MANIFEST, NOT A DIGEST. The restore path re-provisions from the manifest text and re-renders
// units from it, a service with several containers has several digests, and env, mounts, ports
// and subdirs are identity too; the digests are inside it. It cannot be read off the member
// either: the volume keeps manifests in `.services/<name>.json`, a SIBLING of the data subvolume,
// so a btrfs snapshot of the data does not capture the code identity that wrote it. Carrying it
// here is what makes a member self-contained.
type SnapshotMeta struct {
	Service     string      `json:"service"`
	Trigger     Trigger     `json:"trigger"`
	TakenAt     time.Time   `json:"taken_at"`
	Consistency Consistency `json:"consistency"` // what the bytes are; empty means an older member, unrecorded
	Manifest    string      `json:"manifest"`    // the manifest running when it was taken, verbatim
	// Event is the event this member is the RESTORE POINT of, or nil for a plain sample
	// (history.go). A performed act writes it with the member; a detected change or a day
	// boundary writes it into the previous member's sidecar when the next sample finds it.
	Event *Event `json:"event,omitempty"`
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

// agentBin is the guest agent's absolute path, which a rendered ExecStartPre must name because
// systemd requires an absolute path for the first word of an Exec line.
//
// It is the ONE node-local fact this renderer reads, and it is read rather than passed because
// the alternative is worse: threading it through Render would put it in every caller and every
// test for the sake of a path that is the same on every node. Render stays a pure function of the
// manifest in the sense that matters -- the same manifest renders the same units on a node --
// which is the property [V3b.3](f) needs.
func agentBin() string { return guestfirmware.BinDir() + "/briard-guest-agent" }

// SnapshotEntry is one ring member as a reader sees it: where it is, and what its sidecar says.
//
// The two are kept apart deliberately. The sidecar is what the guest WROTE when the member was
// taken and must not carry its own path -- a copied or renamed directory would then describe
// somewhere it is not -- so the path comes from the listing that found it.
type SnapshotEntry struct {
	Member string       `json:"member"`
	Meta   SnapshotMeta `json:"meta"`
}
