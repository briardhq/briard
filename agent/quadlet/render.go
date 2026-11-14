// Package quadlet renders an app manifest into podman quadlet source files: the swappable layer of
// the stack the manifest decision named, manifest (published, signed, runtime-neutral) -> renderer
// (podman/quadlet-specific) -> systemd units. shared/manifest deliberately knows nothing about
// podman and everything podman-shaped lives in this package, so replacing the container runtime
// replaces the renderer rather than re-cutting the published catalog.
//
// It runs in the guest, but the host/guest line runs through the manifest: the host owns identity
// (it fetches the manifest from the catalog, verifies its signature against the release keyring and
// writes those exact bytes to the replicated volume), and the guest never chooses what to run,
// rendering at promotion what the volume says, from a manifest whose content hash is the app
// identity. Rendering must happen in the guest: a promoting node is alone, and the volume is
// readable only once mounted, after drbd-reactor promotes on a quorum event with nothing asking the
// host; and the renderer belongs next to the podman it renders for (Dir's reason for re-rendering
// rather than replicating units). Logic-on-the-host-by-default still holds: Render is a pure total
// function of the manifest bytes, with no state, identity or decision; what moved is which binary
// executes it, not who decides.
//
// Quadlet is podman's own systemd generator, shipped by podman and pulled in by
// virtualisation.podman.enable: files under /run/containers/systemd become units at daemon-reload,
// moving unit generation from build time to run time.
package quadlet

import (
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	apps "briard.io/agent/apps"
	"briard.io/agent/guestfirmware"
	"briard.io/shared/manifest"
)

// Dir is where quadlet reads generated units from. /run, so it is node-local and tmpfs-backed:
// rendered units are deliberately not replicated and do not survive a reboot by themselves. The
// DRBD volume holds the manifest as the app's identity and every node re-renders from it;
// storing rendered units instead would replay one podman version's output onto a node that may
// run another.
const Dir = "/run/containers/systemd"

// ImagePullTimeout bounds one `.image` unit's pull when the manifest does not say how big the
// image is. It is long on purpose: an expiry throws away the layer in flight, so the bound
// exists to make a hopeless pull loud, not to make failure prompt. The .image emit site in
// Render carries the measurements.
const ImagePullTimeout = 45 * time.Minute

// The bound a sized entry gets: a fixed allowance for everything that is not bytes
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

// MemoryLimitFactor is how far past its declared minimum an app may grow before its own
// container is OOM-killed and restarted: relaxed enough for a real app's swings (a database
// migration, a backup, a heavy integration starting), tight enough that one leak cannot take the
// guest's growth up to the host's ceiling. See the [Service] lines in Render.
const MemoryLimitFactor = 4

// Rendered is one app's quadlet source plus the units that run it.
type Rendered struct {
	// Files maps a filename under Dir to its content.
	Files map[string]string
	// Units is the ordered list of generated systemd units converge starts for this app (run by
	// briard-apps, after the data mount and the VIP). Order matters: the pod must exist before its
	// members.
	Units []string
	// ContainerUnits are the per-container .service units without the pod: they hold the data
	// Volume bind and are the only units a maintenance op may stop to release it. Never the pod:
	// `systemctl stop <pod>` makes podman `pod stop`, which kills the containers out from under
	// their units, so each exits non-zero into `failed`; stopping a container is a graceful stop
	// (systemd → `podman stop` → clean exit) that releases the bind and disturbs nothing else.
	//
	// ⚠️ A crashed container does not demote the node, and must not: a code fault is deterministic,
	// so the peer running the identical closure hits it too and failover only flaps; and one broken
	// app must not take the other N-1 down. The mechanism is non-membership: briard-apps starts
	// these units, not the promoter, so `Requires=` never sees them and nothing propagates.
	// Recovery is the unit's own Restart= (see Render); per-app health tells the household, and
	// reports but never gates.
	ContainerUnits []string
	// ImageUnits are the .image pre-warm units, which are neither promoter members nor started
	// by systemd — every warm goes through a caller that guards the pull (see Render).
	ImageUnits []string
	// ImageRefs maps each of those warm units to the image ref it obtains. It exists because
	// starting the unit is a `podman image pull` — measured, not assumed: quadlet generates
	// `Wants=network-online.target` + `ExecStart=podman image pull <ref>`, with no
	// already-present short-circuit. A caller that must not touch the network (bring-up, which
	// runs after every guest reboot) needs the ref to ask whether the pull is needed at all.
	ImageRefs map[string]string
	// Address is the bare host (no port) this app's pod answers on: the guest's own loopback under
	// host networking, the pod's address on the node's pod pool under a private network. An app is
	// one pod and one network namespace, so the consumers (the reverse proxy's routes, the health
	// probe) add only the port. The caller allocates it (Render's addr); it comes back because the
	// .pod's networking was written with it, so consumers never assemble `127.0.0.1:<port>`, which
	// breaks for a private pod network, where the port is not on the guest's loopback. Empty when
	// the caller passed none. It never reaches the catalog: a signed, node-independent document
	// cannot carry a per-node, per-render fact, and since the manifest's content hash is the app
	// identity, one there would remint every entry's identity.
	Address string
}

// prefix keeps every generated unit in one obvious namespace, so a `systemctl list-units
// 'briard-*'` shows exactly what we put on the box and nothing of the user's collides.
const prefix = "briard-"

// Render turns a manifest into quadlet source files, avoiding two traps on purpose:
//
//  1. The .container names its image digest with Pull=never, not `Image=foo.image`: that form
//     makes the container depend on the pull unit, turning promotion into the multi-GB cold pull
//     on the failover-critical path that warm-standby exists to prevent. A cold node fails fast
//     instead; a missing image is converge's decision (it pulls, by digest), never a side-effect
//     of starting a unit.
//  2. AutoUpdate is never set: podman would change image identity behind our back, breaking
//     announce-before-act and the health gate. Our upgrade path owns image identity.
//
// ⚠️ Nothing carries an [Install] section, the .image pre-warm units included. Starting an .image
// unit is an unconditional `podman image pull` (see ImageRefs), so a `WantedBy=multi-user.target`
// would pull even when the image is local and fail on a node with no registry, and one failed
// unit makes `switch-to-configuration switch` exit 4, which the agent reads as a failed commit:
// every OS upgrade on a node with an installed app would roll back. Warm standby is a node fact,
// but not systemd's: install, prewarm, bring-up and converge ensure images through
// AppWarm/warmImage, which asks `podman image exists` first. [Install] could not do it anyway:
// these units live on tmpfs under Dir, absent at boot, so it fires only when something re-runs
// the generators after converge wrote them — exactly that switch-to-configuration case.
func Render(m manifest.Manifest, addr string) (Rendered, error) {
	if err := m.Validate(); err != nil {
		// Rendering an unvalidated manifest is how an injected line reaches a unit file. The
		// caller has normally validated already (Parse does); this is the belt to that braces.
		return Rendered{}, err
	}
	base := prefix + m.Name
	out := Rendered{Files: map[string]string{}}

	// The pod, and the one decision here everything else derives from: host networking is asked
	// for, never assumed. The manifest's silence means private (shared/manifest property 2), so an
	// app reaches nothing but itself unless a catalog entry deliberately says otherwise and carries
	// that word in its identity hash. The address is the caller's, since allocation sees every app
	// on the node and this sees one manifest: the guest's loopback under host networking (nothing
	// written into the pod), else the pod's address on the node's pod pool, given to podman here
	// and to the reverse proxy through Rendered.Address, so "where it answers" is one fact rather
	// than an agreement.
	//
	// ⚠️ ExitPolicy=continue is what brings a crashed container back. Under quadlet's default
	// `stop`, the last container exiting makes podman stop the pod, systemd stops the container
	// unit as its dependent, and a dependency stop suppresses Restart=always, so a crashed app
	// stays down until something else starts it (nixosTest/hass-health-probe.nix, its custom-crash
	// case). With `continue` the pod outlives its containers, the exit is an ordinary failure, and
	// Restart= does its job. Stopping a pod on purpose is converge's (stopApp, and the stop of what
	// it no longer renders).
	pod := []string{"[Pod]", "PodName=" + base, "ExitPolicy=continue"}
	switch {
	case m.HostNetwork():
		pod = append(pod, "Network=host")
	case addr != "":
		// A named network with an address we chose, never one podman assigns: an assigned address
		// has to be read back after the pod starts (a second source of truth) and, worse, moves
		// when a container is recreated, so the routing table would go stale with the app healthy
		// and nothing watching. Written, a recreated container returns to the same address.
		pod = append(pod, "Network="+PodNetwork, "IP="+addr)
		for _, p := range m.Ports {
			// Published as itself on every interface the guest holds, which is what puts a
			// non-HTTP app on the household's service address. The reverse proxy is not involved
			// and cannot be: it speaks HTTP, and this exists for the apps that do not.
			pod = append(pod, fmt.Sprintf("PublishPort=%d:%d", p, p))
		}
	default:
		// Unpinned, and legitimately so: a caller with no address is one that needs unit names,
		// image refs or file contents rather than something to start -- the host's app paths and
		// its node-local spec cache, and the guest's recorder check. The rendering is a true
		// description of the app, just a less specific one, and nothing starts from it: converge
		// re-renders with the allocated address before any pod is started, on every promotion and
		// every install.
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
		// Each mount is a plain directory of the app's area in its tier, never a subvolume of
		// its own: data.restore runs `btrfs subvolume delete`, which refuses on a subvolume
		// containing nested subvolumes, so rollback would break outright. Parent first, so a
		// mount nested in another lands on top of it rather than under it: a parent's path is a
		// prefix of its child's, so it sorts first.
		mounts := slices.Clone(c.Mounts)
		slices.SortFunc(mounts, func(a, b manifest.Mount) int { return strings.Compare(a.Path, b.Path) })
		for _, mt := range mounts {
			lines = append(lines, "Volume="+Area(mt.Tier, m.Name)+"/"+mt.Dir+":"+mt.Path)
		}
		// The binds one catalogued app needs beyond its own data — Home Assistant's control
		// channel, mosquitto's config (agent/apps). They cannot come from the manifest — the
		// schema refuses host binds on purpose, so that a catalog entry cannot ask for the host —
		// so they come from the product, keyed on the app's name. Render stays a pure
		// function of the manifest: the same manifest still renders the same units on every node.
		for _, v := range apps.Volumes(m, c) {
			lines = append(lines, "Volume="+v)
		}
		// The user, from the product for the same reason: a catalog entry that could pick its uid
		// could pick root.
		if u := apps.User(m, c); u != "" {
			lines = append(lines, "User="+u)
		}
		if cp := apps.Capabilities(m, c); cp != "" {
			lines = append(lines, "AddCapability="+cp)
		}
		for _, k := range sortedKeys(c.Env) {
			lines = append(lines, "Environment="+k+"="+escape(c.Env[k]))
		}
		// The container supervises itself: quadlet passes a [Service] section through verbatim and
		// emits Restart= only for the pod, not a container (measured against podman 5.8.2's own
		// generator). App units are not promoter chain members — drbd-reactor neither starts,
		// restarts nor watches them — so without this a dead container stays dead. That
		// non-membership makes "an app error alerts but never demotes" mechanically true; this line
		// is the other half, recovery in the unit rather than the promoter. `always`, not
		// `on-failure`: an app container's job is to keep serving, so exiting 0 is as dead as
		// exiting 1, and the pod unit's generated `on-failure` would silently accept the first.
		// A stop systemd itself was asked for is unaffected — a `systemctl stop` (quiesce, demote)
		// is never restarted whatever the policy says. RestartSec spaces retries past systemd's
		// default start-rate limit (StartLimitBurst=5 per StartLimitIntervalSec=10s), so a
		// crash-loop retries indefinitely instead of latching to `failed` on a transient cause, and
		// stays visible: NRestarts climbs, and the resource telemetry reads it per app.
		lines = append(lines, "", "[Service]", "Restart=always", "RestartSec=5")
		// A leak stays in its own app. The guest grows when it runs short, so a leaking app would
		// be fed DIMM after DIMM up to the host's ceiling, squeezing everything beside it. The
		// limit is relaxed on purpose (MemoryLimitFactor times the declared minimum, room for real
		// swings) and hard: past it the kernel's OOM killer acts inside this unit only, and
		// Restart= brings the app back fresh, the cure for a leak. Swap is capped at the declared
		// minimum, because zram is shared by the whole guest and a memory limit does not count it.
		// No declared minimum, no limit: an unmeasured entry must not be guessed into a cage. The
		// container's processes are accounted under this unit's cgroup, so the limit is per
		// container -- per app while every app is one container; an app of several would need it on
		// a shared slice.
		if m.MinMemoryMB > 0 {
			lines = append(lines,
				fmt.Sprintf("MemoryMax=%dM", MemoryLimitFactor*m.MinMemoryMB),
				fmt.Sprintf("MemorySwapMax=%dM", m.MinMemoryMB))
		}
		// The ring's generic hook, for every catalogued app: a member of its data, taken while the
		// container is stopped, at the one boundary visible from outside it (Home Assistant adds
		// its internal restarts through the inbound channel, which only it can see). Only a
		// container that mounts the data takes one, since an app's other containers share its area
		// and would each take a member of the same bytes; the same start empties the app's cache if
		// space is short (guestagent.TakeStartMember). ⚠️ The leading `-` is not optional: without
		// it a failed snapshot takes the household's app down, and a ring missing a member is
		// nothing while an app that will not start is an outage. The binary also exits 0
		// regardless; `-` alone hides the reason, and the agent's own journal line is what says it.
		if c.UsesTier(manifest.TierData) {
			lines = append(lines, "ExecStartPre=-"+agentBin()+" --app-starting="+m.Name)
			// The other half: a clean stop leaves a marker saying this app's data was flushed,
			// which the next start reads to say what its member's bytes are; a node that dies
			// writes nothing, so the member taken when a survivor promotes says crash-consistent,
			// which it is. ⚠️ ExecStopPost, never ExecStop: it runs after the container is actually
			// down, on every stop systemd performs, and is the only place $SERVICE_RESULT exists.
			// The `-` for the start hook's reason: a household must never lose an app because a
			// marker could not be written.
			lines = append(lines, "ExecStopPost=-"+agentBin()+" --app-stopped="+m.Name)
		}
		out.Files[unit+".container"] = join(lines...)
		out.Units = append(out.Units, unit+".service")
		out.ContainerUnits = append(out.ContainerUnits, unit+".service")

		// One .image unit per container; two containers sharing a ref get two idempotent, harmless
		// oneshot units, cheaper to read than a dedup table keyed on a digest. No [Install]: a
		// guarded caller starts it, it never starts itself (see Render's doc comment).
		//
		// TimeoutStartSec bounds the pull because nothing else does: quadlet generates Type=oneshot
		// (measured against podman 5.8.2's own generator), for which systemd disables
		// TimeoutStartSec= by default, and converge starts this unit on the promotion path. An
		// unbounded pull from a registry that dribbles bytes holds the promotion open indefinitely:
		// nixosTest/cold-converge-pull.nix measured one holding a node Primary with the volume
		// mounted, no VIP, no apps, restarts=0 and nothing in `failed` for its whole 420 s window —
		// nothing broken enough to alert on. The bound is PullTimeout(m.Size): the allowance plus
		// the download at PullBitrate for an entry that says how many bytes it downloads, else
		// ImagePullTimeout (45 minutes). Size is the app's total, so each image of a
		// multi-container app is bounded by the whole: generous, but the only number the manifest
		// carries.
		//
		// ⚠️ Deliberately not tighter: the bound makes a hopeless pull loud, not failure prompt,
		// since an expiry throws away the layer in flight. nixosTest/image-pull-resume.nix: a
		// SIGTERM (what the timeout sends) at 43% of a 25 MiB single-layer image re-fetched the
		// full 25 MiB (1.43x on the wire), but an image of 5x1 MiB plus one 20 MiB layer,
		// interrupted with the small ones done and the big one at 8/20 MiB, re-requested none of
		// the five (equal layers hide this: podman copies layers concurrently, so none has finished
		// at the interrupt). Progress across attempts is monotonic by whole layer, so the bound has
		// to clear the largest single layer, not the whole image: 45 minutes covers a
		// several-hundred-MB layer at well under 2 Mbit/s (PullAllowance has the sized case). A
		// bound shorter than a household's link needs for one layer would never converge,
		// re-downloading the same blob until something gave up for good. It also clears
		// self-healing faults: a 20 s outage mid-transfer was absorbed by TCP retransmission with
		// no re-fetch (1.00x; podman's own --retry never ran), which a timeout firing then would
		// turn into a re-download.
		//
		// PrivateTmp keeps the bound from becoming a disk leak. A pull downloads every compressed
		// layer into /var/tmp/container_images_storage<random>/, unpacks each into the graph root
		// as it arrives, and deletes the downloads only when the whole pull completes; a killed
		// pull leaves them, and podman never comes back for them (measured: three interrupted pulls
		// left three directories). Each expiry would strand up to the whole download (622 MB for
		// Home Assistant) in /var/tmp, the per-boot scratch on the state disk (the graph root is on
		// the same disk), and the households that hit the timeout hit it repeatedly; a completed pull
		// peaks at both at once (622 MB in /var/tmp beside 2.28 GB unpacked, for Home Assistant).
		// systemd removes the unit's own /var/tmp however it stops, where an ExecStopPost cleanup
		// is skipped by a SIGKILL; measured both ways, an interrupted pull leaves 0 KB and a
		// successful one lands in the graph root untouched. The private /var/tmp is disk-backed
		// (systemd-private-*-briard-…-image.service-* under that same /var/tmp), so it does not trade
		// the leak for an OOM on the first multi-GB image. Only the .image unit gets it, since it
		// pulls and exits; the .container unit runs the app and is deliberately left alone.
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
	// The address the pod above was rendered with (see Rendered.Address).
	out.Address = addr
	return out, nil
}

// Area is the app's area in a storage tier: its one btrfs subvolume in the tier's directory
// of the replicated volume, holding the directories its containers mount.
func Area(tier, app string) string { return "/var/lib/briard/" + tier + "/" + app }

// DataRoot is the app's data area -- what it keeps to run, rolled back with it.
func DataRoot(app string) string { return Area(manifest.TierData, app) }

// SnapshotsDir is where every member of an app's ring (its History) lives: read-only siblings
// of the data areas under the data tier's .snapshots dir (created by briard-primary-storage at
// mount), so they replicate with the volume.
const SnapshotsDir = "/var/lib/briard/data/.snapshots/"

// snapshotStamp is the member name's time format: fixed width and UTC, so lexical order over
// stamps is chronological order. Over whole names it is not (SnapshotMember).
const snapshotStamp = "20060102T150405Z"

// A Trigger says what caused a member to be taken, and is the half of its name a human scans.
// The set is closed because the name parser enumerates it: a trigger nobody listed is a member
// nobody can read back. What a member is kept for is not its trigger but the event it anchors
// (history.go).
type Trigger string

const (
	// TriggerStart is an ordinary start of the app: its container's, or for an app that can tell
	// us about its own (Home Assistant's s6 `run` wrapper), one of those. It is the only trigger
	// a later start may replace, while it is still pending.
	TriggerStart Trigger = "start"
	// TriggerClock is the member taken by the clock rather than by a start: a stable
	// app can run for a month without a restart, and the clock is what still samples it.
	// It is the one member taken against a running app, so it is Crash unless the app held
	// still for it.
	TriggerClock Trigger = "clock"
	// The *-before samples: each is taken with the app stopped, just before an
	// operation, and is the restore point of that operation's event. The app does not run again
	// until the next start, so nothing is ever compared against one (Trigger.Before).
	//
	// TriggerAppUpdateBefore precedes briard moving the app to another version. It is the member
	// the upgrade-point rules say must be taken on a stopped container.
	TriggerAppUpdateBefore Trigger = "app-update-before"
	// TriggerAppUndoBefore precedes briard putting an earlier member back, for any reason,
	// including the automatic revert of a failed update. Undoing it is the redo.
	TriggerAppUndoBefore Trigger = "app-undo-before"
	// TriggerHassRestoreBefore is taken at Home Assistant's first restart of one of its own backup
	// restores, while its marker is present. It is not a start.
	TriggerHassRestoreBefore Trigger = "hass-restore-before"
	// TriggerHassDBRestoreBefore precedes briard putting back Home Assistant's recorder database
	// alone, after the nightly check found the newest copy damaged (agent/hass/dbcheck.go). It
	// holds the damaged database, so undoing its event puts that back.
	TriggerHassDBRestoreBefore Trigger = "hass-db-restore-before"
	// TriggerFromBackup is a member materialised from the household's backup rather than taken
	// off the live data: one night's copy of an app, put into the ring so the ordinary restore can
	// put it back. Its bytes are the backup's, so its sidecar's TakenAt is the night the backup
	// copied (what a restore to it goes back to), while its name is stamped at materialising: it
	// must be the newest member until the restore has used it, or the prune would take it first. It
	// counts as Before(): the app never ran on it, so nothing is compared against it.
	TriggerFromBackup Trigger = "from-backup"
	// TriggerUninstall is the member an uninstall keeps: the app stopped, its data as it stood, and
	// the manifest it ran. It outlives the app's manifest on the volume, so for 30 days it is the
	// app's whole presence there (agent/guestagent/uninstall.go), and installing the app again is
	// the restore of it. A data area's is in the ring; a files area's, of the same name, beside
	// the Versions.
	TriggerUninstall Trigger = "uninstall"
)

// Before reports whether a member was taken just before an operation, with the app stopped --
// or put in place for one (TriggerFromBackup), or kept by an uninstall: either way the app did not
// run on it.
func (t Trigger) Before() bool {
	return t == TriggerAppUpdateBefore || t == TriggerAppUndoBefore || t == TriggerHassRestoreBefore ||
		t == TriggerHassDBRestoreBefore || t == TriggerFromBackup || t == TriggerUninstall
}

// SnapshotMemberApp reads the app out of a member's name, and reports whether the name is
// one of ours at all. The sweep over `.snapshots` has to tell our members from anything else a
// human or a future feature left there, and the name is the only thing it can ask.
func SnapshotMemberApp(name string) (string, bool) {
	svc, _, _, ok := ParseSnapshotMember(name)
	return svc, ok
}

// SnapshotMemberTime reads the moment a member was taken out of its name. It is what the ring's
// ordering and its pruning read, and it is why the stamp is in the name rather than only
// in the sidecar: answering "how old is the newest member" must not cost a file read per member.
func SnapshotMemberTime(name string) (time.Time, bool) {
	_, _, at, ok := ParseSnapshotMember(name)
	return at, ok
}

// ParseSnapshotMember splits `<app>-<trigger>-<stamp>`. The app may itself contain dashes
// ("home-assistant"), so it is parsed from the right: the stamp is fixed width and the trigger
// comes from a closed set, which leaves whatever precedes them as the name.
func ParseSnapshotMember(name string) (app string, trigger Trigger, at time.Time, ok bool) {
	name = strings.TrimPrefix(name, SnapshotsDir)
	for _, t := range []Trigger{TriggerStart, TriggerClock, TriggerAppUpdateBefore, TriggerAppUndoBefore, TriggerHassRestoreBefore, TriggerHassDBRestoreBefore, TriggerFromBackup, TriggerUninstall} {
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

// SnapshotMember is one member's subvolume path: app, trigger and the moment it was taken. The ring
// is a series of members distinct by construction: nothing is replaced, and a take whose name
// already exists is refused rather than resolved. Second precision and UTC, so times compare
// correctly once parsed and nodes in different zones taking a member at the same instant agree on
// its name. ⚠️ The name is not a chronological sort key, and reading it as one is a real bug the
// format invites: the trigger sits between app and stamp, so every `-clock-` member sorts before
// every `-start-` one whatever their times. Readers order by SnapshotMemberTime; the trigger stays
// before the stamp because that makes the directory readable to a human scanning it.
func SnapshotMember(app string, trigger Trigger, at time.Time) string {
	return SnapshotsDir + app + "-" + string(trigger) + "-" + at.UTC().Format(snapshotStamp)
}

// SnapshotSidecar is where a member's metadata sits: beside the subvolume, never inside it.
// Inside is impossible — the member is read-only from the instant it exists — and that is also
// why the sidecar cannot be atomic with it. The guest writes it immediately after the snapshot
// and removes the member if it cannot, so "every member has a sidecar" is an invariant the
// history may rely on rather than a hope.
func SnapshotSidecar(member string) string { return member + ".json" }

// AppSidecar is where a member's app keeps its own state about it, beside the history's sidecar
// and never read by the history. Its existence alone holds the member, for at most RetainApp
// (RetentionPrune); what it says is the app's business (Home Assistant's recorder check,
// agent/hass/dbcheck.go). It ends in .json, so a listing of the ring skips it like the other.
func AppSidecar(member string) string { return member + ".app.json" }

// A Consistency says what a member's bytes are, which its trigger does not answer. Quiesced:
// flushed by a clean stop or held still across the take, so a restore gives the app exactly what it
// had. Crash: whatever a power cut would have left, so a restore bets on the app's own recovery —
// measured-good for Home Assistant (WAL replay, nixosTest/hass-upgrade.nix), measured-bad for
// mosquitto, which lost exactly the retained message somebody would roll back for
// (nixosTest/apps-pair.nix). Which way the bet goes is per-app, so a member carries the answer
// rather than leaving every reader to infer one.
//
// ⚠️ It is the taker's to state, never derived from the Trigger: a stopped container is not flushed
// data. The member taken as an app starts after a promotion has nothing running either, yet the old
// primary never shut the app down, so it is crash-consistent under an ordinary `start` trigger. A
// container start tells the two apart by the clean-stop marker its unit's ExecStopPost leaves (see
// Render); a taker that stopped the app itself knows. Empty means unrecorded (a member from an
// older briard): readers treat it as unknown, never quiesced, since the integrity check may trust
// only what says so.
type Consistency string

const (
	// Quiesced: the data was flushed — a container start after a clean stop, a *-before sample
	// of an app briard stopped first, a clock sample whose quiesce held.
	Quiesced Consistency = "quiesced"
	// Crash: not known to be flushed — a clock sample the app did not hold still for (its
	// quiesce: a truncating WAL checkpoint plus a held transaction), or a container start with
	// no clean stop before it.
	Crash Consistency = "crash"
)

// Note is what a household is told about a point's consistency, in one phrase, or "" when there
// is nothing to say. One sentence in one place, because both front ends render it (`briard app
// history` and the dashboard's history page), and a household that heard two descriptions of the
// same fact would have to work out whether they meant the same thing. It keeps our vocabulary out
// too: "quiesced" and "crash-consistent" are words for this file, not for somebody's kitchen. The
// quiet case is quiet on purpose: most points are clean, and a note on every line is a note
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
// A manifest, not a digest: the restore path re-provisions and re-renders units from the manifest
// text, an app with several containers has several digests, and env, mounts, ports and subdirs are
// identity too; the digests are inside it. Nor can it be read off the member: the volume keeps
// manifests in `.apps/<name>.json`, a sibling of the data subvolume, so a btrfs snapshot of the
// data does not capture the code identity that wrote it. Carrying it here makes a member
// self-contained.
type SnapshotMeta struct {
	App         string      `json:"app"`
	Trigger     Trigger     `json:"trigger"`
	TakenAt     time.Time   `json:"taken_at"`
	Consistency Consistency `json:"consistency"` // what the bytes are; empty means an older member, unrecorded
	Manifest    string      `json:"manifest"`    // the manifest running when it was taken, verbatim
	// Event is the event this member is the restore point of, or nil for a plain sample
	// (history.go). An operation writes it with its *-before member; what an evaluation finds is
	// added to the previous member's sidecar as a reason.
	Event *Event `json:"event,omitempty"`
	// Pending marks a start sample waiting for its boot's verdict before it is evaluated.
	// The long-running guest agent clears it; the ring keeps a pending sample.
	Pending bool `json:"pending,omitempty"`
	// Health is the app's health as of this sample, once it is evaluated: a start's is
	// its boot's verdict, and every other sample carries its predecessor's forward, so the
	// healthy → unhealthy transition survives the pruning of the start that measured it. Empty
	// means unknown.
	Health apps.Health `json:"health,omitempty"`
	// Resets are the renames a start's boot made (apps.Corrupt's form), found at its verdict
	// and landed on the sample before it -- with those of the pending starts it replaced.
	// No member holds them before the next sample does, so a comparison with this
	// member as the earlier side does not find them again.
	Resets []string `json:"resets,omitempty"`
	// BackedUp marks an uninstall's kept copy once a nightly backup that holds it has finished:
	// only then may the files tier's valve shed it for room.
	BackedUp bool `json:"backed_up,omitempty"`
}

// Subdirs lists the directories the install step must create, each `<tier>/<dir>`, once each and
// sorted: the areas they name are the ones the app has, beside the data area every app
// has.
func Subdirs(m manifest.Manifest) []string {
	var dirs []string
	for _, c := range m.Containers {
		for _, mt := range c.Mounts {
			dirs = append(dirs, mt.Tier+"/"+mt.Dir)
		}
	}
	slices.Sort(dirs)
	return slices.Compact(dirs)
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
// harmless-looking in an env var, and a silent corruption of what the app was told. The
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

// ContainerName is the podman container (and unit prefix) for one container of one app. The
// renderer decides it, so everything that needs to name a running container asks here rather than
// rebuilding the string — the host's app spec does, and so does the guest when it has to
// exec into one.
func ContainerName(app, container string) string { return prefix + app + "-" + container }

// PodNetwork is the podman network every private app's pod joins. One network for the node
// rather than one per app: the pool is a /24 and the addresses inside it are what separate
// apps, while a network per app would spend a subnet each to express the same thing.
//
// It is created by the node before the first private app starts, never by quadlet: an
// [Network] unit would make network creation a side-effect of starting a pod, which is the same
// trap `Image=foo.image` sets for pulls.
const PodNetwork = "briard"

// Images renders only the pre-warm units — the part of an app that has no networking in it.
//
// Warming is something a node does when it is not serving, so it has not allocated this app an
// address; rendering the whole set would demand one, and the honest answer would be a placeholder
// in a .pod file that converge is about to overwrite. The image units depend on nothing but the
// manifest's digests, so this is the whole truth for a prewarm rather than a subset of a lie.
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

// agentBin is the guest agent's absolute path, which the rendered ExecStartPre and ExecStopPost
// must name because systemd requires an absolute path for the first word of an Exec line.
//
// It is the one node-local fact this renderer reads, read rather than passed because threading
// it through Render would put it in every caller and every test for a path that is the same on
// every node. Render stays a pure function of the manifest in the sense converge-at-promotion
// needs: the same manifest renders the same units on a node.
func agentBin() string { return guestfirmware.BinDir() + "/briard-guest-agent" }

// SnapshotEntry is one ring member as a reader sees it: where it is, and what its sidecar says.
//
// The two are kept apart deliberately. The sidecar is what the guest wrote when the member was
// taken and must not carry its own path -- a copied or renamed directory would then describe
// somewhere it is not -- so the path comes from the listing that found it.
type SnapshotEntry struct {
	Member string       `json:"member"`
	Meta   SnapshotMeta `json:"meta"`
	// App says the member has an app sidecar (AppSidecar), which holds it. Read off the ring by
	// existence; the contents are the app's.
	App bool `json:"app,omitempty"`
}
