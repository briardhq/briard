package guestagent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	apps "briard.io/agent/apps"
	"briard.io/agent/drbd"
	"briard.io/agent/guestagent/deadman"
	"briard.io/agent/guestfirmware"
	"briard.io/agent/hass"
	"briard.io/agent/mosquitto"
	"briard.io/agent/quadlet"
	"briard.io/shared/dashboard"
	"briard.io/shared/manifest"
	"briard.io/shared/model"
	"briard.io/shared/nodestorage"
	"briard.io/shared/notify"
	"briard.io/shared/telemetry"
)

// The protocol this package speaks is the firmware's: the framing, the handshake, the three push
// verbs and the Executor they shell out through live in agent/guestfirmware, the one Go package
// the guest image bakes and so the only one whose change has to move the guest chain. Everything
// here is what the host pushes on top of it as `briard-guest-agent`.
//
// Executor is the firmware's, embedded rather than re-declared so the two halves cannot drift,
// plus what the pushed half asks of the filesystem and the network. Those are answered in-process,
// never by spawning `test`, `ls` or `ip`: a spawned command costs the guest a few milliseconds of
// CPU, and the host asks some of these at every tick of its loop, so on an idle node the spawning
// was a large share of everything the guest did. They live here, not in the firmware, because the
// firmware is baked into the guest image and an edit there would move the image.
type Executor interface {
	guestfirmware.Executor
	// Stat describes the file at path, following symlinks (as `test -e` does). A missing file is
	// an error os.IsNotExist recognises, the same contract as ReadFile.
	Stat(path string) (fs.FileInfo, error)
	// ListDir names a directory's entries as `ls -1` does: sorted, without the ones whose name
	// starts with a dot. A missing directory is an error os.IsNotExist recognises.
	ListDir(path string) ([]string, error)
	// IPv4Addrs is a device's global IPv4 addresses as CIDRs ("192.168.1.5/24"), in the kernel's
	// order (globalIPv4 says which count). A device that does not exist is an error.
	IPv4Addrs(dev string) ([]string, error)
}

// osExecutor is the real guest Executor: the firmware's, and the standard library for the rest.
type osExecutor struct{ guestfirmware.Executor }

// NewOSExecutor returns the production Executor: what the guest agent's main runs with.
func NewOSExecutor() Executor { return osExecutor{guestfirmware.NewOSExecutor()} }

func (osExecutor) Stat(path string) (fs.FileInfo, error) { return os.Stat(path) }

func (osExecutor) ListDir(path string) ([]string, error) {
	entries, err := os.ReadDir(path) // sorted by name
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), ".") {
			names = append(names, e.Name())
		}
	}
	return names, nil
}

func (osExecutor) IPv4Addrs(dev string) ([]string, error) {
	ifi, err := net.InterfaceByName(dev)
	if err != nil {
		return nil, err
	}
	addrs, err := ifi.Addrs()
	if err != nil {
		return nil, err
	}
	return globalIPv4(addrs), nil
}

// globalIPv4 keeps the IPv4 addresses that are the machine's on a network, as CIDRs: not the
// loopback, and not IPv4 link-local (169.254.0.0/16). A link-local address is one the machine
// gave itself when nobody answered, so it is never an address we put on a NIC nor one to take
// off, and it can never be the flock's service address -- yet it reads as an address to
// everything downstream: reported as the VIP, it passes the in-guest health probe and the node
// publishes healthy while nothing on the LAN can reach it. The guest is separately told not to
// invent one (dhcpcd -L), so this is deliberately the second line of defence, not the first,
// because the cost of being wrong here is a node that looks fine.
func globalIPv4(addrs []net.Addr) []string {
	var cidrs []string
	for _, a := range addrs {
		n, ok := a.(*net.IPNet)
		if !ok || n.IP.To4() == nil || n.IP.IsLoopback() || n.IP.IsLinkLocalUnicast() {
			continue
		}
		cidrs = append(cidrs, n.String())
	}
	return cidrs
}

// exists is `test -e`: the path names something, a symlink counting by what it points to.
func exists(x Executor, path string) bool {
	_, err := x.Stat(path)
	return err == nil
}

// mountinfoPath lists this process's mounts, one per line; field 5 is the mount point.
const mountinfoPath = "/proc/self/mountinfo"

// isMountPoint answers what `stat -c %m p` compared with p answered: whether a filesystem is
// mounted at p itself, rather than p being a directory of the one beneath it. An unreadable
// mount table is "not mounted": every caller reads that as "not ours here".
func isMountPoint(x Executor, p string) bool {
	raw, err := x.ReadFile(mountinfoPath)
	if err != nil {
		return false
	}
	for line := range strings.SplitSeq(string(raw), "\n") {
		if f := strings.Fields(line); len(f) > 4 && unescapeMountPath(f[4]) == p {
			return true
		}
	}
	return false
}

// unescapeMountPath undoes the kernel's octal escaping of space, tab, newline and backslash in a
// mountinfo path ("\040" for a space).
func unescapeMountPath(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+4 <= len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// DefaultPollInterval is how often BringUpGuest polls for convergence.
const DefaultPollInterval = 500 * time.Millisecond

// Control verbs on the channel. The host owns bring-up: it renders
// the DRBD config and drives create-md / target / reactor through these; the
// guest just executes. None of them promote/demote -- drbd-reactor does that.
const (
	verbNodeStorage = "storage.node"       // write the storage spec + start briard-node-storage.service
	verbReactor     = "drbd.reactor.start" // write the promoter snippet + start drbd-reactor -- the only thing that promotes
	verbStatus      = "drbd.status"        // the node status: model.Cluster (QuorumState + peers); on a flock, drbdsetup status --json
	verbAdjust      = "drbd.adjust"        // rewrite the .res + `drbdadm adjust` (runtime mesh growth)
	verbChainStart  = "chain.start"        // start the lone node's chain target -- its promotion, with no DRBD
)

// nodeStorageUnit is the guest unit this verb starts. The agent renders it with no [Install]
// section (units.go), so it runs when the host says so and never at boot; and because the agent it
// execs is pushed, storage bring-up cannot run before the host has dressed the guest.
//
// ⚠️ "Dressed" has to mean committed, not staged. The unit names <binDir>/briard-guest-agent
// directly (units.go says why it must not go through the picker), so it can start only once the
// commit has moved the staged file onto that name. The commit is the last thing the agent does
// before serving anything (guestfirmware.BinCommit); a commit after the port opens lets this verb
// exec a path that does not exist yet (203/EXEC, a failed bring-up).
const nodeStorageUnit = "briard-node-storage.service"

// verbNetConfigure sets a static address on a guest NIC -- the system/DRBD NIC on the
// private subnet, so DRBD can bind/connect there. Host-driven: the agent knows
// the per-node address (it renders the same into the .res). Idempotent (addr replace),
// since bring-up may retry.
const verbNetConfigure = "net.configure"

// verbNetVIP reads the address the VIP device actually holds, in CIDR form; "" when it holds none
// (a Secondary, or a promotion still in flight). Under DHCP the host does not decide the address:
// it is acquired inside the guest at promotion, so the host has to be told what it turned out to
// be, to probe health at it, report it, and print it at a human. Ground truth is the interface, not
// whatever was last written to vip.env: an address recorded but failed to apply must not be
// reportable as live.
const verbNetVIP = "net.vip"

// verbNetVIPForget deletes the flock's remembered DHCP address (`.vip-address` at the volume's
// root) on the node that mounts the volume. The host asks for it when it refuses the address the
// guest was handed: briard-vip-up claims a remembered address optimistically at the next
// promotion, so a refused one left there would be claimed -- and refused -- again.
const verbNetVIPForget = "net.vip.forget"

// vipAddrFile is the image's `vipAddrFile` (guest-image/configuration.nix), restated as
// dataMountRoot is.
const vipAddrFile = dataMountRoot + "/.vip-address"

// verbNetMDNSName records the flock's human-visible name, which the reverse proxy publishes as
// `briard-<name>.local` pointing at the VIP. It is its own verb rather than a field on
// net.configure because the name is a label and the address an identity: a rename must be
// possible without re-running network configuration, and folded in, every rename would be an
// addressing call (re-asserting VIP_DEV/VIP_ADDR to change a string), exactly the coupling the
// three-way identifier split exists to remove. The name is flock-scoped, not node-scoped, because
// it resolves to the VIP: a node-scoped name would change identity on failover while the address
// it points at did not.
const verbNetMDNSName = "net.mdnsname"

// verbNetMDNSPublished reads back the flock name the reverse proxy's mDNS responder is actually
// answering for, as it records it from what it serves (reverse-proxy's writePublished). Same
// doctrine as verbNetVIP: reporting the requested name would report one present, plausible,
// and not necessarily the one being served.
const verbNetMDNSPublished = "net.mdnspublished"

// verbNetMDNSOther reads back the address of another briard the reverse proxy heard answering the
// bare `briard.local` when this node came up -- "" when none was heard or nothing is published.
// The bare name is shared by design; this is the one fact that lets the doctor say so.
const verbNetMDNSOther = "net.mdnsother"

// verbSetHostname sets the guest's hostname to this node's name. DRBD matches the running hostname
// against the `on <name>` stanzas in the .res, so every guest (one baked image, hostname "guest")
// must be renamed to its node name before create-md, or DRBD reports "not defined for this host".
// The agent sets it at every bring-up (a no-op on a node whose name is still the default "guest").
// This is the node id (`briard-node-3f9a2c`), which is hidden, not the flock name a household
// sees (verbNetMDNSName); keeping the two apart lets the visible name change without breaking
// DRBD's self-match.
const verbSetHostname = "sys.hostname"

// verbSetTimezone hands the guest the household's timezone, an IANA name the host read off its own
// configuration (agent/host/timezone.go). A node-scoped fact the host pushes at every bring-up:
// the guest's /etc is a tmpfs-backed overlay, so /etc/localtime lives exactly as long as the boot
// it was pushed into. Its readers are whatever the guest renders for people (the dashboard's
// History above all) and the journal's own display. The image sets no zone, which leaves
// /etc/localtime unmanaged and free to set at runtime.
const verbSetTimezone = "sys.timezone"

// Upgrade/rollback verbs, driven by the host's guest.Manager. Data snapshot/restore cut at the
// app's btrfs subvolume (per-app scope, not the whole volume). os.system reads the code identity
// (the closure store path, node-independent, not a generation number); the image the host boots is
// the whole-VM code half. The app.* verbs act on whichever unit the host names, which is always a
// runtime-installed app's.
//
// ⚠️ Renaming a verb family is the one break a verb set cannot absorb: the guest advertises names,
// so a rolled host meeting an un-rolled guest finds the whole family missing at once, and Supports
// can only route around a verb one path at a time. Only the alpha's reinstall-only policy makes
// that affordable (every node re-runs the installer, so there is no fleet to strand), and it is
// why the channel carries no version number to bump (guestfirmware, VerbHello).
const (
	verbAppStart  = "app.start"  // systemctl start <unit>
	verbAppStop   = "app.stop"   // systemctl stop <unit> (quiesce before snapshot)
	verbAppActive = "app.active" // systemctl is-active <unit> -> bool
	verbAppHealth = "app.health" // in-guest GET of the app's health URL -> bool (the probe done from inside the guest, so it survives a substrate — e.g. macvtap — where the host can't reach the VIP)
	// verbAppHealthOf is app health asked by app name: the guest resolves the address from its
	// own routing table and answers healthy, unhealthy or unknown (appHealth). A separate verb
	// rather than a field on app.health, so an older guest refuses it loudly instead of silently
	// probing an empty URL and reporting the app unhealthy, which on the install gate would revert
	// a perfectly good install. Named for its three-valued answer, which a healthy-or-not verb
	// could not give.
	verbAppHealthOf = "app.health.state"
	// verbDataMember takes one ring member: refuses a collision instead of replacing, and writes
	// the sidecar beside it. A new name rather than a field on an older verb: an old guest does not
	// advertise it, so Client.Supports refuses exactly the one path that needs it, where a version
	// floor would refuse every path on every not-yet-rolled guest.
	verbDataMember = "data.member"
	// verbDataMemberQuiesced takes a member of an app that is running, asking it to hold still
	// across the snapshot (the clock sample), and reports back what it achieved, because "held
	// still" is not something the host can observe from where it sits. A name of its own rather
	// than a flag on data.member, by the same rule: a guest that ignored the flag would take a live
	// snapshot and report the success of a quiesced one, the class lying about the one member it
	// exists for.
	//
	// ⚠️ The whole sequence (hold, snapshot, release) runs in the guest, deliberately: driven from
	// the host, the released-lock guarantee would sit on the far side of a channel that can drop
	// mid-transaction; here the release is a deferred call in the same process as the request that
	// took it.
	verbDataMemberQuiesced = "data.member.quiesced"
	verbDataRestore        = "data.restore" // replace the live subvolume with a snapshot
	// verbDataReplace is that same swap with the sweep: the paths the host names are
	// removed from the staged copy before it goes live, which is what stops a member taken around
	// a household's own backup restore from replaying it. A name of its own rather than a field
	// on data.restore, by the same rule: a field whose absence is silent is a guest reporting
	// success for the old behaviour.
	verbDataReplace = "data.replace"
	verbOSSystem    = "os.system" // readlink -f /run/current-system -> closure store path
	verbOSState     = "os.state"  // systemctl is-system-running, + the failed units when degraded
)

// No verb pins an OS closure to the data: the data's identity is per-app (the app manifest, here
// on the replicated volume) while a system closure is a property of the node, so one per app
// volume would be N assertions about one running system. Nor is there a compatibility case: a
// newer kernel makes btrfs features available rather than enabled, and DRBD metadata version is
// fixed at create-md. A format migration, if one ever lands, is a manual procedure with its own
// gating.

// Runtime app install: two verbs, because the halves live in different durability domains and
// one verb would hide that. app.render writes quadlet source to /run (node-local, tmpfs) and
// reloads systemd so the generated units exist; every node runs it, secondaries included, since a
// survivor can start the pod only because it was rendered there too. app.provision creates the
// app subvolume + its per-container subdirectories and records the manifest, all on the replicated
// volume, which only the Primary has mounted; a secondary cannot run it at all. Both are dumb
// hands (logic lives on the host by default): the host renders the unit text (agent/quadlet),
// decides which node gets which verb and owns the ordering; the guest writes bytes and makes
// directories.
const (
	verbAppRender    = "app.render"    // write quadlet units to /run + daemon-reload (every node)
	verbAppProvision = "app.provision" // create the app subvolume + record the manifest (Primary only)
	// app.installed reads one named app's manifest off the volume, or "". The name is in the
	// request because a verb whose meaning changes under an unchanged name is the one thing
	// nothing on this channel can police: the guest advertises names, and a name that still
	// resolves reports success from a guest doing the old thing. So meaning moves with the name:
	// a new verb is refused by exactly the one path that needs it (Supports), and the same rule
	// covers a new field whose absence would be silent (data.member, data.replace).
	verbAppInstalled = "app.installed" // read one named app's manifest from the volume, or ""
	// app.pulling records (or clears) an app install's pull for the dashboard:
	// the manifest's sizes and the start time, on the dashboard's tmpfs -- written before the
	// warm, because the manifest reaches the volume only after it, and a bar needs its total
	// while the bytes move. The guest keeps a number; the host decided to pull.
	verbAppPulling = "app.pulling"
	// storage.free reports the image store's filesystem (free, total bytes): what the host's
	// free-space gate reads before a pull starts. The guest measures; the host refuses.
	verbStorageFree = "storage.free"
	// deadman.episode hands the host the last stretch the guest spent without it (what the
	// deadman did, from when to when), and forgets it: the guest reports, the host alerts. An
	// empty reply means nothing happened since the host last asked.
	verbDeadmanEpisode = "deadman.episode"
	// storage.grow takes the state disk's filesystem out to the size the host just grew the disk
	// to (a thick file extended, then QEMU's block_resize): it waits for the kernel to see the new
	// size, then resize2fs, online. The host decided and paid; the guest only fills the space.
	verbStorageGrow = "storage.grow"
	// app.list names the apps the volume carries, which lets a converged node say what it runs:
	// converge-at-promotion renders from the volume, so a survivor that never installed anything
	// runs apps the host was never told about, and the host's node-local cache is written only by
	// an install on a Primary, so without this the survivor serves apps while reporting none.
	// Names only, because app.installed already returns each manifest; a verb returning both would
	// duplicate that one.
	verbAppList = "app.list" // list the apps recorded on the volume
	verbAppWarm = "app.warm" // ensure an image is present, starting its .image unit only if it is missing
	// verbImageEnsure is the same question asked without a unit: is this ref resident, and if not,
	// fetch it. app.warm starts a rendered `.image` unit, which exists only for a manifest the node
	// has already rendered, and a restore has to ask about a manifest it has not rendered and may
	// never render, because the answer decides whether it proceeds at all.
	//
	// ⚠️ Its failure means the opposite of converge's: a pull converge cannot do takes the VIP down
	// and says so (the running-never-needs-network doctrine), while a pull the restore cannot do
	// simply cancels the restore and leaves the household exactly as it was. Same operation,
	// opposite blast radius, hence a separate verb rather than a flag on the other one.
	verbImageEnsure = "image.ensure"
	// image.remove deletes one image ref from podman's store: what the host asks once an app has
	// moved off it and nothing on the volume pins it any more. An image that is not here is already
	// gone, not an error; one a container still uses is refused by podman, and the host only logs it.
	verbImageRemove = "image.remove"
	// verbDataMembers lists one app's ring: every member and the sidecar beside it, oldest
	// first. It is what the picker reads, and it is a verb rather than a directory the
	// host could stat because only the guest has the volume mounted.
	verbDataMembers = "data.members"
	// storage.usage answers one app's share of the volume per tier (appusage.go): its group's total,
	// cap and history count fresh, and the split between its current data and its history from
	// the last walk -- starting one in the background when that is stale. A read for the app's page.
	verbStorageUsage = "storage.usage"
	// cache.empty empties one app's cache area (clearCache), the host having stopped the app: the
	// Storage card's Clear.
	verbCacheEmpty = "cache.empty"
	// data.backup starts a backup of the newest member of every app into the repository the
	// request names (backup.go) and answers whether it did; data.backup.result answers whether one
	// is running and hands over a finished one's report once. The recorder check's two-verb shape:
	// a run reads for minutes, so it runs in the background and the host collects the report on
	// a later cycle rather than holding a call open for it.
	verbDataBackup       = "data.backup"
	verbDataBackupResult = "data.backup.result"
	// files.tick starts the files tier's tick in the background (filestier.go) -- an hourly version
	// where one is due, its retention, the allocation rule's limits, the valve -- and
	// answers whether it did; files.tick.result is whether one is running, with a finished one's
	// report once. The same two-verb shape: the valve waits on the cleaner.
	verbFilesTick       = "files.tick"
	verbFilesTickResult = "files.tick.result"
	// data.backup.key answers the household's repository key, minting it on the volume the first
	// time it is asked for (backup.go), and records the household's word that it is saved.
	verbDataBackupKey = "data.backup.key"
	// data.backup.restore puts one app's copy from one night of the backup into the ring as a
	// member (backup.go) and answers the member's path, for the ordinary app restore to put
	// back. An act: it writes the ring, and it reads from the repository for as long as that takes.
	verbDataBackupRestore = "data.backup.restore"
	// data.backup.adopt adds the volume's key to a repository another key opens (backup.go), so
	// the next night can write it. An act, one restic command.
	verbDataBackupAdopt = "data.backup.adopt"
	// data.backup.list answers every backup the repository holds, newest first, with the apps in
	// each (backup.go) -- the Backup page's restore. A read, with no lock on the repository.
	verbDataBackupList = "data.backup.list"
	// app.converge re-runs converge-at-promotion in place, on a node that is already Primary:
	// render every manifest on the volume, warm, start (converge.go). An install calls it once it
	// has written the new manifest. It is a verb rather than `systemctl restart briard-apps`
	// because briard-apps is a promoter chain member: stopping one deactivates drbd-reactor's
	// target, which unmounts the volume and demotes the node. Same code, same unit, no unit
	// lifecycle touched.
	verbAppConverge = "app.converge"
	// ── App-specific verbs: `app.<catalog name>.<action>` ────────────────────────────────
	//
	// The naming rule is a property, not a style. Every other verb here works on any app
	// (app.health probes whatever URL it is handed, app.converge renders whatever the volume
	// says); these do not: each knows one app's API, returns that app's own shape, and refuses
	// every other name. A verb set is a capability list, so it has to say what it grants: a generic
	// `readiness` would promise a power the guest never has, while `app.home-assistant.readiness`
	// grants exactly the one it does. The app in the name rather than in a request field leaves no
	// name to steer from the wire and no unknown-name branch to write.
	//
	// The two gate verbs are in the guest because both halves are: the credential is on this
	// node's tmpfs and the app listens on the guest's loopback, which under macvtap the host
	// cannot reach at all. Both are still dumb hands: how to talk to an app is agent/hass's and
	// agent/mosquitto's, what the answer means is the host's (agent/host/readiness.go).
	//
	// None is guarded by a capability check, which is the alpha policy, not an oversight: the
	// agent and the guest closure are published together and the alpha reinstalls rather than
	// upgrading in place, so no node has a guest older than its agent. A guest that does not know
	// one of these is a mismatched pair, a fault worth seeing as one, not a configuration to
	// tolerate.

	// app.home-assistant.readiness samples Home Assistant's per-config-entry setup states.
	// It is the differential S1 gate's input, one layer above the liveness floor
	// `app.health` answers: the floor says the app replies, this says which of the things
	// it was doing it is still doing.
	verbHassReadiness = "app.home-assistant.readiness"

	// app.mosquitto.probe stores a token in the broker's own retained state and reports what is
	// stored: the S1 signal for an app that publishes no readiness of its own. A separate verb
	// rather than a mosquitto arm of the one above, because it does a different kind of thing:
	// readiness samples, this one writes, and folding a write into a verb whose name promises a
	// read is the other way a capability list stops describing what the host can make the guest do.
	verbMosquittoProbe = "app.mosquitto.probe"

	// app.home-assistant.nudge tells a running Home Assistant that something outside it changed
	// and it should reconsider: the one signal that travels into Home Assistant, where the two
	// verbs above pull facts out of it. It grants exactly one power, to put briard's own no-payload
	// event on Home Assistant's bus; it cannot name an event, carry data, or reach any other app,
	// since all three would have to be on the wire, and none of them is.
	verbHassNudge = "app.home-assistant.nudge"

	// The nightly recorder check (agent/hass/dbcheck.go): three verbs, because it is a read and a
	// write that run differently (this package's dbcheck.go). The host schedules them and tells
	// the household; the work is here because only the guest mounts the volume. The manifest, and
	// so the image and the data path, come off the volume.
	//
	// app.home-assistant.dbcheck starts the check in the background and answers whether it
	// did: `quick_check` on the newest quiesced member, and on damage a search for a held copy
	// that checks clean. Read-only on the data. No request body.
	verbHassDBCheck = "app.home-assistant.dbcheck"
	// app.home-assistant.dbcheck.result answers whether a check is running, and hands over a
	// finished one's report once.
	verbHassDBCheckResult = "app.home-assistant.dbcheck.result"
	// app.home-assistant.dbrestore puts the recorder database back from the member a report
	// named: the gate again, stop, undo point, copy, start -- in line, like every operation that
	// stops an app.
	verbHassDBRestore = "app.home-assistant.dbrestore"
	// dashboard.handoff writes the one-time code + OS account the host minted for the household
	// dashboard, 0600 on tmpfs (shared/dashboard) -- the whole of its bootstrap auth.
	verbDashboardHandoff = "dashboard.handoff"
	// dashboard.casa writes the host's view of the household's casa name for the page to render
	// -- a fact of the host's, pushed on every change and at bring-up, never decided here.
	verbDashboardCasa = "dashboard.casa"
	// dashboard.alerts writes the host's alert store for the page to list -- a copy, pushed when
	// the store records something and at the start of every connection; the store stays the host's.
	verbDashboardAlerts = "dashboard.alerts"
	// dashboard.backup writes the host's view of the nightly backup for the page -- the folder,
	// whether it runs, the last nights -- pushed the same way. Not the key: the page reads that
	// from the volume (dashboard.BackupKeyPath).
	verbDashboardBackup = "dashboard.backup"
	// dashboard.catalog writes the host's view of the catalog's index for the page -- every
	// listed app, which are installable and which the household wished for -- pushed the same
	// way; the index and the wishes stay the host's.
	verbDashboardCatalog = "dashboard.catalog"
)

// manifestDir holds the installed apps' identities on the replicated volume: one `<name>.json` per
// app, the manifest's own bytes, whose content hash is that identity (one manifest transitively
// pins the whole container set). The volume holds the manifests, never the rendered units: a
// survivor re-renders rather than replaying units a different podman version may have produced.
// A directory rather than one file, mirroring the host's node-local cache and for the same reason:
// the volume must be able to say which app it means. With one file, installing a second app reads
// the first as its own "prior", snapshots against the wrong app and deletes the first app's
// rendered units as a renamed prior's orphans.
const manifestDir = "/var/lib/briard/.apps"

// quadletDir is where podman's generator reads unit source from. Mirrors agent/quadlet.Dir; the
// guest is told the filenames but not the directory, so this path is the guest's own.
const quadletDir = "/run/containers/systemd"

const (
	// TLS cert/key on the DRBD volume: replicated, so a failover serves the same
	// cert; the terminator hot-reloads them. The reverse proxy's unit (units.go) is rendered
	// with these paths.
	tlsDir      = "/var/lib/briard/tls"
	tlsCertPath = tlsDir + "/fullchain.pem"
	tlsKeyPath  = tlsDir + "/key.pem"
)

// Maintenance-mode verbs: a planned upgrade must hold drbd-reactor's promoter so it doesn't treat
// a deliberate app stop as a failure (demote/re-promote). Pause stops the drbd-reactor daemon
// (stop-services-on-exit defaults false -> DRBD Primary + apps stay up); Resume restarts it
// (re-adopts, no restart/demote). The app is then cycled surgically (app.stop is
// ignore-dependencies, so the VIP/data/target stay up), so no target re-raise is needed.
const (
	verbReactorPause  = "reactor.pause"  // systemctl stop drbd-reactor.service
	verbReactorResume = "reactor.resume" // systemctl start drbd-reactor.service
	verbReactorActive = "reactor.active" // systemctl is-active drbd-reactor.service -> bool (bracket guard)
	verbReactorEvict  = "reactor.evict"  // drbd-reactorctl evict: hand the work to a peer
	verbCertWrite     = "cert.write"     // write a renewed cert/key to the DRBD volume
	verbCertRead      = "cert.read"      // read the cert (never the key) back: `briard doctor`'s expiry check
	verbResources     = "sys.resources"  // read appliance resource telemetry
	verbFsSync        = "fs.sync"        // flush the data volume's dirty pages (pre-eviction pre-copy)
)

// dataMountRoot is where briard-primary-storage mounts the replicated volume — the guest image's
// `btrfsRoot` (guest-image/configuration.nix), restated here the way manifestDir restates a
// path under it. fs.sync carries no path on the wire on purpose: the verb has exactly one
// meaning ("flush the replicated volume"), and the node that mounts the volume is the one that
// knows where it lives.
const dataMountRoot = "/var/lib/briard"

// guestCapabilities is the verb set the guest advertises in its handshake -- the honest
// capability list a host negotiates against (Client.Supports). Keep in sync with the
// dispatch switch; a verb absent here is invisible to a capability-checking host even if
// the switch handles it. (A drift guard test asserts a representative subset is present.)
var guestCapabilities = []string{
	verbSetHostname, verbSetTimezone, verbNodeStorage, verbAdjust, verbReactor, verbChainStart, verbStatus, verbNetConfigure, verbNetVIP, verbNetVIPForget,
	verbNetMDNSName, verbNetMDNSPublished, verbNetMDNSOther,
	verbAppStart, verbAppStop, verbAppActive, verbAppHealth, verbAppHealthOf,
	verbFilesTick, verbFilesTickResult,
	verbDataMember, verbDataMemberQuiesced, verbDataMembers, verbStorageUsage, verbCacheEmpty, verbDataBackup, verbDataBackupResult, verbDataBackupKey, verbDataBackupRestore, verbDataBackupAdopt, verbDataBackupList, verbDataRestore, verbDataReplace, verbImageEnsure, verbImageRemove,
	verbAppRender, verbAppProvision, verbAppInstalled, verbAppList, verbAppWarm, verbAppConverge, verbAppStage, verbAppCommit, verbAppDiscard, verbAppPending, verbAppUninstall, verbAppReinstate, verbHassReadiness, verbHassNudge, verbHassDBCheck, verbHassDBCheckResult, verbHassDBRestore, verbMosquittoProbe, verbReactorActive,
	verbAppPulling, verbStorageFree, verbStorageGrow, verbDeadmanEpisode,
	verbOSSystem, verbOSState, guestfirmware.VerbOSPowerOff,
	verbReactorPause, verbReactorResume, verbReactorEvict,
	verbCertWrite, verbCertRead,
	verbDashboardHandoff, verbDashboardCasa, verbDashboardAlerts, verbDashboardBackup, verbDashboardCatalog,
	verbResources,
	verbFsSync,
	guestfirmware.VerbHello, guestfirmware.VerbBinStage, guestfirmware.VerbBinTest, guestfirmware.VerbBinActivate,
}

// besideActs is every verb the serve loop answers without the act mutex, beside whatever act is
// in flight (guestfirmware.ServeFrames). A verb absent here is an act by default, the safe default
// for one added later. Two kinds qualify:
//
//   - reads change nothing, so there is nothing for an act to interleave with; they are what let
//     the host tell a slow guest from a dead one while an install pulls its image.
//   - pushes of the host's own file (the alert copy, the pull marker, the dashboard's code, a
//     renewed cert): each is written whole and renamed into place, nothing reads it mid-flight,
//     and each must land during an act, because an install is exactly when the household looks.
//     The one HTTP push (the integration nudge) is a single POST.
//
// Everything that touches the chain, the store, the volume layout or the ring is an act, one at a
// time in arrival order. The groups inside that set touch disjoint state, but nothing issues them
// concurrently (the host sends one directive at a time), so one mutex is the whole of the ordering.
var besideActs = map[string]bool{
	// reads
	guestfirmware.VerbHello: true, verbStatus: true, verbAppActive: true,
	verbAppHealth: true, verbAppHealthOf: true, verbAppInstalled: true,
	verbAppList: true, verbDataMembers: true, verbNetVIP: true, verbNetMDNSPublished: true,
	verbNetMDNSOther: true, verbResources: true, verbStorageFree: true, verbCertRead: true,
	verbReactorActive: true, verbOSSystem: true, verbOSState: true, verbDeadmanEpisode: true,
	verbHassReadiness: true, verbMosquittoProbe: true, verbHassDBCheck: true,
	verbHassDBCheckResult: true, verbAppPending: true, verbDataBackup: true,
	verbDataBackupResult: true, verbDataBackupKey: true, verbDataBackupList: true,
	// started in the background like the backup, whose shape it shares: the start returns at once,
	// and what the tick writes (the versions, the files tier's limit) no act touches
	verbFilesTick: true, verbFilesTickResult: true,
	// a read that may start its walk in the background, the same way
	verbStorageUsage: true,
	// pushes
	verbDashboardHandoff: true, verbDashboardCasa: true, verbDashboardAlerts: true, verbDashboardBackup: true,
	verbDashboardCatalog: true,
	verbAppPulling:       true, verbCertWrite: true, verbNetMDNSName: true, verbHassNudge: true,
}

// isAct is ServeFrames' lane predicate: anything not declared to run beside acts is one.
func isAct(verb string) bool { return !besideActs[verb] }

const (
	// currentSystem resolves to the running system's closure store path (the code identity).
	currentSystem = "/run/current-system"
	// bootedSystem, systemProfile and the staging profile are read by nothing in this package:
	// the guest runs no in-place OS upgrade (the host swaps the whole image), so there is no
	// generation to compare or park.
	bootedSystem = "/run/booted-system"
	// The system profile.
	systemProfile = "/nix/var/nix/profiles/system"
	// The staging profile, a second system profile.
	stagingProfileDir = "/nix/var/nix/profiles/system-profiles"
	stagingProfile    = stagingProfileDir + "/staging"
	// journalDir + containerStore are the appliance growth surfaces the resource telemetry
	// measures: the systemd journal and the podman code/layer store. Both are the
	// unmanaged-growth suspects the soak's disk trend names.
	journalDir     = "/var/log/journal"
	containerStore = "/var/lib/containers"
	// kmsgCursorPath tracks how far the kernel-error report has read, so each poll returns only
	// new warning+ lines. In /run so a guest reboot re-baselines cleanly.
	kmsgCursorPath = "/run/briard/.kmsg-cursor"
)

// ProvisionRequest is drbd.adjust's payload: the host-rendered .res (drbd.Resource.Config) and,
// when set, the promoter snippet (drbd.ReactorConfig) to rewrite before `drbdadm adjust`.
type ProvisionRequest struct {
	Resource      string `json:"resource"`
	ResConfig     string `json:"res_config"`
	ReactorConfig string `json:"reactor_config,omitempty"`
	Diskless      bool   `json:"diskless,omitempty"` // read by nothing: drbd.adjust runs no create-md
}

// ProvisionResult is returned by no verb: storage.node decides first init inside its own unit
// (nodestorage.go), so created-metadata never crosses the channel.
type ProvisionResult struct {
	CreatedMetadata bool `json:"created_metadata"`
}

// resourceRequest names the resource for drbd.status.
type resourceRequest struct {
	Resource string `json:"resource"`
}

// unitRequest names a systemd unit (systemctl start/stop/is-active).
type unitRequest struct {
	Unit string `json:"unit"`
}

// healthRequest carries the app health URL the guest GETs from inside itself
// (app.health). The host owns the URL (its HealthURL config); the guest just probes it.
type healthRequest struct {
	URL string `json:"url"`
}

// appRequest names an app by its catalog slug — the form every verb takes once the guest
// resolves what it needs from its own state rather than from the caller's.
type appRequest struct {
	App string `json:"app"`
}

// snapshotRequest is a btrfs subvolume op: DataDir is the live rw subvolume, Path is
// the RO snapshot subvolume (the dest on snapshot, the source on restore).
type snapshotRequest struct {
	DataDir string `json:"data_dir"`
	Path    string `json:"path"`
	// Sidecar is the member's metadata, rendered by the host (quadlet.SnapshotMeta) and written
	// here verbatim beside the subvolume. Empty on data.restore, which addresses a member rather
	// than making one. A guest that ignored this field would leave unlabelled members behind,
	// which is why it rides a verb of its own (data.member) rather than an optional field.
	Sidecar string `json:"sidecar,omitempty"`
	// Sweep is what data.replace removes from the staged copy before it goes live: paths relative
	// to the subvolume root, named by the host out of the apps registry. Only
	// data.replace reads it, and that is the whole reason that verb has a name of its own — a
	// guest taking this as an optional extra on data.restore would put the data back and keep the
	// marker, reporting success while replaying the household's own restore.
	Sweep []string `json:"sweep,omitempty"`
	// App is the catalog name, for the one verb that must reach into the app before it
	// snapshots (data.member.quiesced): the manifest on the volume is what says how.
	App string `json:"app,omitempty"`
}

// appRenderRequest carries the quadlet source the host rendered: filename -> content, to be
// written under quadletDir. Stale is the set of filenames to remove first -- the outgoing
// render's -- so a re-render leaves nothing of the previous one behind.
type appRenderRequest struct {
	Files map[string]string `json:"files"`
	Stale []string          `json:"stale,omitempty"`
}

// appProvisionRequest creates the app's single data subvolume with per-container plain
// subdirectories inside it (never nested subvolumes — `btrfs subvolume delete` refuses on a
// subvolume containing them, which would break data.restore outright), and records Manifest as
// the app identity on the replicated volume.
type appProvisionRequest struct {
	// Name is which app this is, and therefore which identity file on the volume the
	// manifest lands in. The host owns the naming because it owns the manifest; deriving it
	// from DataDir here would make the guest re-implement a layout it is only ever told.
	Name     string   `json:"name"`
	DataDir  string   `json:"data_dir"`
	Subdirs  []string `json:"subdirs,omitempty"`
	Manifest string   `json:"manifest"`
}

// appInstalledRequest names the app whose recorded manifest to read (app.installed).
// The name is the whole request, and it is what makes the volume able to say which app it
// means: with an unnamed read, installing a second app sees the first as its own prior.
type appInstalledRequest struct {
	Name string `json:"name"`
}

// manifestPath is one app's identity file on the replicated volume. The name is a manifest
// slug — shared/manifest's Validate refuses anything else, precisely because these become path
// elements — and the dispatch re-checks it with safeUnitName before it gets here, because the
// guest must not depend on the host having validated its input.
func manifestPath(name string) string { return manifestDir + "/" + name + ".json" }

// sidecarPath mirrors quadlet.SnapshotSidecar. Restated here rather than imported because the
// guest is dumb hands for this verb -- it is handed a member path and some bytes, and deriving
// the companion name is the only thing it does with either. The two must agree, and
// TestSidecarPathMatchesTheRenderer is what keeps them in step.
func sidecarPath(member string) string { return member + ".json" }

// appWarmRequest names one image to ensure is present (app.warm), and the .image unit
// that would obtain it. Both halves are needed because the check is on the ref and the action is
// on the unit -- and the host, which owns the manifest, is the only side that knows the pairing.
type appWarmRequest struct {
	Unit string `json:"unit"`
	Ref  string `json:"ref"`
}

// hassReadinessRequest carries the port Home Assistant serves on (app.home-assistant.readiness).
// The port comes from the host because the host holds the manifest, where an app's port is
// declared once (the same field the liveness floor builds its URL from); the guest supplies the
// rest, because the rest is knowledge about that one app. No name field: the verb names the app
// (see the verb block), so one less thing on the wire and one less unknown-name branch that could
// only ever mean the two sides disagree.
type hassReadinessRequest struct {
	Port int `json:"port"`
}

// mosquittoProbeRequest carries the token to store, if any (app.mosquitto.probe). An empty
// token makes it a pure read. No port either: the port a client of the broker uses is not the
// manifest's, which names the endpoint the liveness floor probes -- so it is product knowledge,
// and it stays with the rest of that app's knowledge rather than travelling on the wire.
type mosquittoProbeRequest struct {
	Token string `json:"token,omitempty"`
}

// certWriteRequest carries a renewed cert + key to land on the DRBD volume (cert.write).
type certWriteRequest struct {
	Cert string `json:"cert"`
	Key  string `json:"key"`
}

// reactorRequest names a drbd-reactor promoter snippet to pause/resume.
type reactorRequest struct {
	Snippet string `json:"snippet"`
}

// reactorStartRequest arms the promoter: the snippet to land and the resource it promotes. It is
// not reactorRequest with a field added, because the two are different acts on different
// documents: pause/resume name a snippet already on disk by its file name, and this one carries
// the snippet's content. One struct would make "snippet" mean two things depending on the verb,
// the kind of thing that reads fine and drops a promoter chain.
type reactorStartRequest struct {
	Resource string `json:"resource"`
	Snippet  string `json:"snippet"`
}

// evictRequest asks this node to give the work to a peer (reactor.evict).
// KeepMasked leaves the node ineligible afterwards -- the reboot path, so it cannot reclaim the
// resource before its new generation has been verified. Unmask is the release, and runs no
// eviction of its own. Both false = a plain evict, which unmasks on its way out and is what
// makes a later hand-back possible.
type evictRequest struct {
	KeepMasked bool `json:"keep_masked,omitempty"`
	Unmask     bool `json:"unmask,omitempty"`
}

// mdnsNameRequest carries the flock's visible name (net.mdnsname). Name is the bare flock name
// ("brave-elf"); the `briard-` prefix and the `.local` suffix are the guest's to add, so the one
// place that knows the published label's shape is the unit that publishes it.
type mdnsNameRequest struct {
	Name string `json:"name"`
}

// netConfigureRequest sets a static CIDR on a guest interface (net.configure) and records
// which NIC the promoter claims the VIP on; VIPDev "" = this node claims no VIP (a witness).
// VIPAddr is the service address itself, in CIDR form ("192.168.9.50/24"); like VIPDev it is
// recorded here, not applied (the promoter chain claims it when this node wins the primary role),
// and "" = DHCP decides.
type netConfigureRequest struct {
	Dev     string `json:"dev"`
	CIDR    string `json:"cidr"`
	VIPDev  string `json:"vip_dev,omitempty"`
	VIPAddr string `json:"vip_addr,omitempty"`
	// VIPLease: the host holds the VIP's lease (a single-anchor substrate, where the guest's own
	// frames carry the host's MAC), so the guest claims VIP_ADDR when it is given one and never
	// starts a client of its own -- with none given, it claims nothing.
	VIPLease bool `json:"vip_lease,omitempty"`
	// The private host<->guest link, when the substrate has one. PrivDev is the guest NIC facing
	// it and PrivHostIP is the host's own address on the system subnet, reached over that NIC by
	// a /32 -- which must beat the on-link /24 the system address itself installs, because under
	// macvtap that on-link path leads out of eth1 to a host the substrate isolates us from. Both
	// empty = no private link (the bridge substrate, where the host shares our L2 and is simply
	// on-link) and nothing is routed. Named by the host, never inferred here: the guest bakes no
	// positional knowledge of which NIC is which.
	PrivDev     string `json:"priv_dev,omitempty"`
	PrivCIDR    string `json:"priv_cidr,omitempty"`
	PrivHostIP  string `json:"priv_host_ip,omitempty"`
	PrivHostMAC string `json:"priv_host_mac,omitempty"`
	// PodSubnet is the pool this guest allocates its private app networks from, as the first
	// three octets ("10.12.7"). Drawn on the host (agent/subnet) because the draw must see the
	// household's address space, which only the host can; used only in here, because a pod network
	// exists nowhere else. Empty = this node cannot render a private app, and converge says so
	// rather than quietly handing it the guest's own namespace.
	PodSubnet string `json:"pod_subnet,omitempty"`
	// The service identity, when the guest has to make it itself. Under the bridge substrate the
	// host gives us one tap (all Windows can express), so VIPDev is not a NIC anyone handed us;
	// VIPParent names the NIC to build it on as a macvlan child, and VIPMAC is the flock-scoped MAC
	// that child must carry, because failover moves a MAC and never just an address. VIPParent
	// empty = the host built the device (macvtap) and nothing here creates anything; VIPMAC is
	// still the VIP's DHCP identity (VIP_MAC in vip.env).
	VIPParent string `json:"vip_parent,omitempty"`
	VIPMAC    string `json:"vip_mac,omitempty"`
	// VIPRouter: the household's router on the VIP's LAN, which replies to clients on other
	// subnets leave through while the VIP is held (VIP_ROUTER, briard-vip-reply-route). The
	// host's own gateway there: it knows one however the address was set, and the guest knows
	// one only when it holds the lease itself.
	VIPRouter string `json:"vip_router,omitempty"`
}

// NetConfig is what the host tells the guest about its own networking, in one call. A struct
// rather than positional arguments: these are independent facts about several NICs, and at
// that width a caller transposing two strings produces a node that configures itself
// confidently and wrongly.
type NetConfig struct {
	Dev         string // the system NIC -- this node's node IP lands here, and DRBD binds it
	CIDR        string // that address, with prefix; "" leaves the NIC unaddressed
	VIPDev      string // the NIC the promoter claims the VIP on; "" = this node claims none
	VIPAddr     string // the VIP itself, CIDR form; "" = DHCP decides
	VIPLease    bool   // the host holds the VIP's lease: the guest never asks DHCP itself
	PrivDev     string // the guest NIC facing the private host link; "" = no such link
	PrivCIDR    string // this end's own address on it -- substrate, addressed by nobody; see the handler
	PrivHostIP  string // the host's system-subnet address, routed over PrivDev
	PrivHostMAC string // that end's link address, pinned as a permanent neighbour -- see the handler
	PodSubnet   string // the pool private app networks are allocated from, "10.12.7"; "" = none drawn
	VIPParent   string // the NIC to build VIPDev on as a macvlan child; "" = the host already built it
	VIPMAC      string // the flock MAC: the VIP's DHCP identity, and the MAC a VIPParent child carries
	VIPRouter   string // the router on the VIP's LAN, for replies to other subnets; "" = none routed
}

// netVIPRequest names the device to read the live VIP from (net.vip). Empty Dev = this node has
// no VIP device, which answers "" rather than erroring.
type netVIPRequest struct {
	Dev string `json:"dev"`
}

// hostnameRequest carries this node's name (sys.hostname).
type hostnameRequest struct {
	Name string `json:"name"`
}

// resourcesRequest is what the host must tell the guest to measure the appliance: each app's
// systemd unit (for its cgroup -> RSS/fds/restarts) and the data volume (for used space +
// snapshot count). Both empty on a witness -> only the volume-independent metrics come back, which
// is exactly why the host asks regardless of whether anything is installed.
type resourcesRequest struct {
	// Apps pairs each app's name with the unit whose footprint IS that app's. The
	// host owns the pairing because it owns the manifest; the guest measures what it is handed
	// and names nothing itself (logic lives on the host by default).
	Apps    []resourceApp `json:"apps,omitempty"`
	DataDir string        `json:"data_dir,omitempty"` // the DRBD data volume mount
	// Soak adds the reads that only the soak's oracle consumes and that cost real I/O: the two
	// du walks and the snapshot listing (and the quota tree, which only the oracle reads). A
	// shipped node never sets it: the host reads this verb under a 5 s deadline (agent/host's
	// resources), and a du over a freshly pulled multi-GB container store can outlast that.
	Soak bool `json:"soak,omitempty"`
}

// resourceApp is one app to measure: its name, and its serving unit.
type resourceApp struct {
	Name string `json:"name"`
	Unit string `json:"unit"`
}

// dispatch is the guest-side verb switch: verb -> Executor calls (+ ParseStatus).
func dispatch(x Executor) guestfirmware.DispatchFunc {
	return func(ctx context.Context, verb string, payload json.RawMessage) (any, error) {
		// runIn executes a guest command whose output is only wanted on failure, and
		// wraps a non-zero exit with the command + its output -- otherwise the host
		// sees a bare "exit status 1" and bring-up failures are undiagnosable.
		runIn := func(ctx context.Context, name string, args ...string) error {
			out, err := x.Run(ctx, name, args...)
			if err == nil {
				return nil
			}
			if o := bytes.TrimSpace(out); len(o) > 0 {
				return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, o)
			}
			return fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
		}
		// The dispatch context, which is what almost every verb wants: the command dies with
		// the agent that was asked to stop.
		run := func(name string, args ...string) error { return runIn(ctx, name, args...) }
		switch verb {
		case guestfirmware.VerbHello:
			// The dressed agent's answer: the same handshake the firmware serves, with the full
			// verb set in place of the firmware's five. Which list comes back is how the host
			// tells a dressed guest from an undressed one.
			return guestfirmware.HelloReply(x, guestCapabilities), nil
		case guestfirmware.VerbBinStage, guestfirmware.VerbBinTest, guestfirmware.VerbBinActivate:
			return guestfirmware.HandleBin(ctx, x, verb, payload)
		case verbSetTimezone:
			var req timezoneRequest
			if err := json.Unmarshal(payload, &req); err != nil {
				return nil, err
			}
			return nil, setTimezone(ctx, x, run, req.Zone)
		case verbSetHostname:
			var req hostnameRequest
			if err := json.Unmarshal(payload, &req); err != nil {
				return nil, err
			}
			// In memory and nowhere else: the hostname and the `.res` it must match need one
			// lifetime. A name that reverts to the baked "guest" under a persistent
			// `on briard-node-<id>` lets the reactor promote into the mismatch, and a failed
			// promote is never retried (quorate but never Primary, no VIP). Both are ephemeral and
			// re-derived from the host at every bring-up, and the agent-armed promoter means
			// nothing can promote before that bring-up; a copy on disk would be a third thing that
			// can be wrong.
			return nil, x.Sethostname(req.Name)
		case verbNodeStorage:
			// The host's storage spec, landed and carried out. Parsed here rather
			// than written through blindly so a spec this build cannot understand is refused at
			// the verb, with the host still listening, instead of inside a unit whose only
			// report is an exit code.
			spec, err := nodestorage.Parse(payload)
			if err != nil {
				return nil, err
			}
			raw, err := spec.Marshal()
			if err != nil {
				return nil, err
			}
			if err := x.WriteFile(nodestorage.Path, raw); err != nil {
				return nil, err
			}
			// The work is the unit's, not this handler's (the shape the storage spec chose):
			// systemd owns the result (its status, journal, exit code), and the ExecStart is the
			// pushed agent, so storage code moves with the host bundle instead of being frozen in
			// the image. `systemctl start` on a Type=oneshot returns when ExecStart has finished,
			// so a failure here is a failed bring-up.
			return nil, run("systemctl", "start", nodeStorageUnit)
		case verbAdjust:
			var req ProvisionRequest
			if err := json.Unmarshal(payload, &req); err != nil {
				return nil, err
			}
			// Runtime mesh growth: rewrite the resource config with the new peer set and apply the
			// delta to the already-running resource. `drbdadm adjust` brings up the connection(s)
			// to the joining node(s) with no restart and no create-md, so the serving primary's
			// local disk stays attached and UpToDate; the joining anchor/witness come up via
			// storage.node (FreshInit=false) and resync as SyncTarget, and this side is the primary
			// learning it now has peers. Idempotent: adjust to an unchanged config is a no-op, so a
			// retried pair directive is safe.
			if err := x.WriteFile(resPath(req.Resource), []byte(req.ResConfig)); err != nil {
				return nil, err
			}
			if req.ReactorConfig != "" {
				if err := x.WriteFile(reactorPath, []byte(req.ReactorConfig)); err != nil {
					return nil, err
				}
			}
			return nil, run("drbdadm", "adjust", req.Resource)
		case verbReactor:
			var req reactorStartRequest
			if err := json.Unmarshal(payload, &req); err != nil {
				return nil, err
			}
			// The promoter snippet lands here, one step before the daemon that reads it starts:
			// writing it beside the start keeps the chain a node will be promoted with and the
			// moment it may be promoted one call apart. Empty on a witness, which runs no
			// promoter and never reaches this verb.
			if req.Snippet != "" {
				if err := x.WriteFile(reactorPath, []byte(req.Snippet)); err != nil {
					return nil, err
				}
			}
			// Arming the promoter; this is the only thing that ever does. The guest unit is
			// `wantedBy = [ ]`, so drbd-reactor does not start at boot, and everything bring-up does
			// above (hostname, NIC addressing, vip.env, the .res, the promoter snippet, a runtime
			// app's units) is therefore guaranteed in place before anything can promote, on a
			// reboot as well as on a first install. Idempotent, deliberately: `systemctl start` on
			// a running daemon is a no-op, so re-converging a healthy node costs nothing, and an
			// agent that died inside a maintenance bracket re-arms the promoter it left stopped.
			return nil, run("systemctl", "start", "drbd-reactor.service")
		case verbChainStart:
			// The lone node's promotion: the same five members in the same order,
			// started by a static target instead of drbd-reactor's generated one. Nothing else
			// starts it (no [Install], units.go), so every step of bring-up happens-before it exactly
			// as for the reactor -- and like the reactor start it is idempotent, so re-converging
			// a healthy node costs nothing.
			return nil, run("systemctl", "start", chainTarget)
		case verbStatus:
			req, err := resourceReq(payload)
			if err != nil {
				return nil, err
			}
			// The one place the serving state is filled in (model.QuorumState.Serving reads it),
			// with its two branches: a node that runs no DRBD answers from its chain target, a
			// flock from DRBD. Only a spec that says alone takes the first branch; no spec yet
			// (a guest the host has not brought up) asks DRBD, which answers "no resource" --
			// the same not-serving the host always read before bring-up.
			if alone(x) {
				return loneCluster(ctx, x), nil
			}
			out, err := x.Run(ctx, "drbdsetup", "status", "--json")
			if err != nil {
				return nil, err
			}
			// The fuller view. QuorumState is embedded, so a reader that wants only the
			// summary reads this response unchanged.
			c, err := drbd.ParseCluster(out, req.Resource)
			if err != nil {
				return nil, err
			}
			// The chain is the reactor's target for the resource, which it reaches only once
			// every member has started: DRBD turns Primary first, so the role alone would answer
			// "serving" for as long as the chain takes to mount, claim and converge.
			c.ChainDown = !unitActive(ctx, x, "drbd-services@"+req.Resource+".target")
			return c, nil
		case verbCertWrite:
			var req certWriteRequest
			if err := json.Unmarshal(payload, &req); err != nil {
				return nil, err
			}
			// Land the renewed cert/key on the DRBD volume (replicated). A torn write (cert
			// updated, key not yet) is safe: the terminator's LoadX509KeyPair fails on a
			// mismatched pair and keeps the last good cert until both land, then hot-reloads.
			if err := run("mkdir", "-p", tlsDir); err != nil {
				return nil, err
			}
			if err := x.WriteFile(tlsKeyPath, []byte(req.Key)); err != nil {
				return nil, err
			}
			if err := x.WriteFile(tlsCertPath, []byte(req.Cert)); err != nil {
				return nil, err
			}
			// Flush so the cert replicates before a failover relies on it, the way app.provision does.
			_, err := x.Run(ctx, "sync", "-f", tlsCertPath)
			return nil, err
		case verbCertRead:
			// The PEM as it sits on the volume; the host parses it, so the guest links no x509.
			// Absent is "" -- a node with no Briard account is never issued one -- and any other
			// read failure is an error, so "could not read" never reads as "has none".
			out, err := x.ReadFile(tlsCertPath)
			if os.IsNotExist(err) {
				return "", nil
			}
			if err != nil {
				return nil, err
			}
			return string(out), nil
		case verbAppStart:
			req, err := unitReq(payload)
			if err != nil {
				return nil, err
			}
			return nil, run("systemctl", "start", req.Unit)
		case verbAppStop:
			req, err := unitReq(payload)
			if err != nil {
				return nil, err
			}
			// Surgical stop: --job-mode=ignore-dependencies stops only this unit, so a
			// planned app quiesce can't cascade to the promoter target / data mount /
			// VIP (the promoter is separately paused). Correct by construction --
			// no dependence on drbd-reactor's exact systemd dependency directives.
			return nil, run("systemctl", "--job-mode=ignore-dependencies", "stop", req.Unit)
		case verbAppActive:
			req, err := unitReq(payload)
			if err != nil {
				return nil, err
			}
			// Is-active exits non-zero when the unit is not active, printing the state
			// word regardless -- so trust the word, not the exit code.
			out, _ := x.Run(ctx, "systemctl", "is-active", req.Unit)
			return strings.TrimSpace(string(out)) == "active", nil
		case verbAppHealth:
			var req healthRequest
			if err := json.Unmarshal(payload, &req); err != nil {
				return nil, err
			}
			// The readiness probe, done from inside the guest (not host->VIP over the LAN):
			// the guest owns the VIP on its own NIC, so this works regardless of the host
			// networking substrate (macvtap blocks host->guest, but not guest->itself). A raw
			// net.Dial HTTP/1.0 GET; 200 == healthy; any error == not.
			return probeHTTPOK(ctx, req.URL), nil
		case verbAppHealthOf:
			var req appRequest
			if err := json.Unmarshal(payload, &req); err != nil {
				return nil, err
			}
			// The address comes from the routing table, not from the caller: the manifest names a
			// port, but what host answers on it is the renderer's decision (`127.0.0.1` is right
			// only for a host-networked pod), and resolving here makes the install gate and the
			// steady-state probe follow that decision instead of restating it. An unresolvable app
			// is an error rather than `false`: "I cannot find where this app listens" and "this app
			// is not answering" have opposite consequences on the install gate (the first is worth
			// retrying until the deadline, the second is the thing being measured), and collapsing
			// them would revert an install because a table was written a moment late.
			return appHealth(ctx, x, req.App)
		case verbDataMember:
			req, err := snapshotReq(payload)
			if err != nil {
				return nil, err
			}
			// One implementation, two callers. The host drives this verb for the members only it
			// can title (the upgrade point, where only the host knows a switch happened); the
			// inbound channel calls the same function for the ones only the guest can see
			// (inbound.go). A second copy of "take a member" would be a second place for the
			// collision rule and the sidecar invariant to drift.
			if err := takeSnapshot(ctx, x, run, req.DataDir, req.Path, req.Sidecar); err != nil {
				return nil, err
			}
			var meta quadlet.SnapshotMeta
			if req.Sidecar != "" && json.Unmarshal([]byte(req.Sidecar), &meta) == nil {
				recordMember(ctx, x, req.Path, meta)
			}
			return nil, nil
		case verbDataMemberQuiesced:
			req, err := snapshotReq(payload)
			if err != nil {
				return nil, err
			}
			return quiescedMember(ctx, x, run, req)
		case verbDataRestore, verbDataReplace:
			req, err := snapshotReq(payload)
			if err != nil {
				return nil, err
			}
			ringMu.Lock() // the listener or the evaluator must not prune the member being restored from
			defer ringMu.Unlock()
			// Precondition: the host has stopped the app (bind released). Swap the
			// live rw subvolume for a fresh rw snapshot of the RO restore point.
			//
			// One implementation, two names: data.replace is data.restore plus the sweep below, a
			// separate name because the sweep's absence is silent. An older guest handed a sweep
			// list on data.restore would put the data back, ignore the field and report success,
			// leaving a household's own restore to replay. The host calls data.replace and lets
			// Supports refuse; data.restore stays for the one caller that must put data back even
			// on a guest too old to sweep (the failed-upgrade revert, RestoreWithoutSweep).
			//
			// ⚠️ Verify, materialise, then destroy, in that order. Deleting the live subvolume first
			// means a missing or unreadable restore point, or a failed snapshot, leaves the
			// household with the live data gone and nothing in its place, on the app rollback path
			// after a failed upgrade, when their data is the only thing left worth having. Nothing
			// irreversible happens until its replacement exists on disk.
			if _, err := x.Run(ctx, "btrfs", "subvolume", "show", req.Path); err != nil {
				return nil, fmt.Errorf("restore point %s is not a usable subvolume (%w) -- refusing to delete %s",
					req.Path, err, req.DataDir)
			}
			// The cache goes with every rollback: a newer version's cache beside older data is how
			// a downgrade breaks an app, and nothing in a cache is worth that. Emptied before the
			// data is touched, so a failure here costs only what the app can make again.
			_, app, err := areaOf(req.DataDir)
			if err != nil {
				return nil, err
			}
			if err := clearCache(ctx, x, run, app); err != nil {
				return nil, fmt.Errorf("empty %s's cache: %w", app, err)
			}
			// A staging name beside the live one, on the same btrfs so the rename below is a
			// metadata operation. Cleared first: `btrfs subvolume snapshot` given an existing
			// directory creates the new snapshot inside it rather than replacing it.
			staged := req.DataDir + ".restoring"
			if _, err := x.Run(ctx, "btrfs", "subvolume", "show", staged); err == nil {
				if err := run("btrfs", "subvolume", "delete", staged); err != nil {
					return nil, err
				}
			}
			// Born into the live area's group, so a limit there keeps counting across the swap.
			group, err := areaGroup(ctx, x, req.DataDir)
			if err != nil {
				return nil, err
			}
			if err := run("btrfs", "subvolume", "snapshot", "-i", group, req.Path, staged); err != nil {
				return nil, err
			}
			// The sweep, on the staged copy and nowhere else. A member taken around a household's
			// own backup restore carries the request that started it, and HA's wipe deliberately
			// keeps the tar, so putting that member back unswept would replay the restore and land
			// the household exactly where they were trying to leave. Here the live data is never
			// touched: the rename below is what makes any of this visible, so a crash leaves either
			// the old subvolume or a swept one, never a half-swept live one. Unconditional, because
			// a member that happens to carry a marker nobody anticipated (a clock sample landing in
			// the seconds between HA writing one and consuming it) needs the same treatment and
			// cannot be recognised as special. Relative paths only, resolved against the staged
			// root: an absolute path from the host would make this a verb that can unlink anywhere
			// on the volume.
			for _, rel := range req.Sweep {
				if rel == "" || strings.Contains(rel, "..") || strings.HasPrefix(rel, "/") {
					return nil, fmt.Errorf("data.replace: %q is not a path inside the member", rel)
				}
				if err := run("rm", "-f", staged+"/"+rel); err != nil {
					return nil, fmt.Errorf("sweep %s from the staged copy: %w", rel, err)
				}
			}
			// Only now is the live copy expendable: its replacement is already on disk, so a
			// crash from here leaves something to finish rather than nothing to recover.
			if err := run("btrfs", "subvolume", "delete", req.DataDir); err != nil {
				return nil, err
			}
			return nil, run("mv", staged, req.DataDir)
		case verbAppRender:
			var req appRenderRequest
			if err := json.Unmarshal(payload, &req); err != nil {
				return nil, err
			}
			return nil, renderApp(ctx, x, run, req)
		case verbAppProvision:
			var req appProvisionRequest
			if err := json.Unmarshal(payload, &req); err != nil {
				return nil, err
			}
			return nil, provisionApp(ctx, x, run, req)
		case verbAppWarm:
			var req appWarmRequest
			if err := json.Unmarshal(payload, &req); err != nil {
				return nil, err
			}
			// One implementation, shared with converge (converge.go): an installing Primary and a
			// converging survivor must not disagree about what "the image is already here" means.
			// The exists-or-pull rule and why it is safe are argued there.
			return nil, warmImage(ctx, x, req.Unit, req.Ref)
		case verbDataMembers:
			var req appRequest
			if err := json.Unmarshal(payload, &req); err != nil {
				return nil, err
			}
			return listMembers(ctx, x, req.App)
		case verbStorageUsage:
			var req appRequest
			if err := json.Unmarshal(payload, &req); err != nil {
				return nil, err
			}
			return appUsage(ctx, x, req.App, time.Now())
		case verbCacheEmpty:
			var req appRequest
			if err := json.Unmarshal(payload, &req); err != nil {
				return nil, err
			}
			if err := safeUnitName(req.App); err != nil {
				return nil, err
			}
			return nil, clearCache(ctx, x, run, req.App)
		case verbImageRemove:
			var req appWarmRequest
			if err := json.Unmarshal(payload, &req); err != nil {
				return nil, err
			}
			if req.Ref == "" {
				return nil, fmt.Errorf("image.remove: need an image ref")
			}
			if _, err := x.Run(ctx, "podman", "image", "exists", req.Ref); err != nil {
				return nil, nil
			}
			// An image this OS carries (briard-stage, configuration.nix) is kept: it comes back at
			// every boot, and on a node with no registry it is the only copy there is.
			if staged, err := x.ReadFile(stagedImagesPath); err == nil {
				if id, err := x.Run(ctx, "podman", "image", "inspect", "--format", "{{.Id}}", req.Ref); err == nil &&
					slices.Contains(strings.Fields(string(staged)), strings.TrimSpace(string(id))) {
					return imageRemoveReply{Kept: true}, nil
				}
			}
			if out, err := x.Run(ctx, "podman", "rmi", req.Ref); err != nil {
				return nil, fmt.Errorf("image.remove %s: %w: %s", req.Ref, err, strings.TrimSpace(string(out)))
			}
			return nil, nil
		case verbImageEnsure:
			var req appWarmRequest
			if err := json.Unmarshal(payload, &req); err != nil {
				return nil, err
			}
			if req.Ref == "" {
				return nil, fmt.Errorf("image.ensure: need an image ref")
			}
			// Resident is the whole answer when it is true, and it is the cheap half: a manifest's
			// ref is a digest, so an image that is here is the image asked for.
			if _, err := x.Run(ctx, "podman", "image", "exists", req.Ref); err == nil {
				return nil, nil
			}
			// Otherwise fetch it directly rather than through a `.image` unit. That unit is a
			// property of a rendered manifest, and this caller is asking about one the node has
			// not rendered and may never render -- a ring member's pinned identity, possibly
			// months old. Pulling by ref needs nothing rendered and is exactly as safe.
			if out, err := x.Run(ctx, "podman", "pull", req.Ref); err != nil {
				return nil, fmt.Errorf("image.ensure %s: %w: %s", req.Ref, err, strings.TrimSpace(string(out)))
			}
			return nil, nil
		case verbHassReadiness:
			var req hassReadinessRequest
			if err := json.Unmarshal(payload, &req); err != nil {
				return nil, err
			}
			return hass.Readiness(ctx, x, req.Port)
		case verbHassDBCheck:
			return startDBCheck(x), nil
		case verbHassDBCheckResult:
			return dbCheckResult(), nil
		case verbDataBackup:
			var req backupRequest
			if err := json.Unmarshal(payload, &req); err != nil {
				return nil, err
			}
			return startBackup(x, req)
		case verbDataBackupResult:
			return backupResult(), nil
		case verbFilesTick:
			return startFilesTick(x), nil
		case verbFilesTickResult:
			return filesTickResult(), nil
		case verbDataBackupKey:
			var req backupKeyRequest
			if err := json.Unmarshal(payload, &req); err != nil {
				return nil, err
			}
			return backupKeyState(ctx, x, req)
		case verbDataBackupRestore:
			var req backupRestoreRequest
			if err := json.Unmarshal(payload, &req); err != nil {
				return nil, err
			}
			return restoreBackupMember(ctx, x, req, time.Now())
		case verbDataBackupList:
			var req backupListRequest
			if err := json.Unmarshal(payload, &req); err != nil {
				return nil, err
			}
			return listBackups(ctx, x, req)
		case verbDataBackupAdopt:
			var req backupAdoptRequest
			if err := json.Unmarshal(payload, &req); err != nil {
				return nil, err
			}
			return nil, adoptBackup(ctx, x, req)
		case verbHassDBRestore:
			var req dbRestoreRequest
			if err := json.Unmarshal(payload, &req); err != nil {
				return nil, err
			}
			return nil, hassDBRestore(ctx, x, run, req.Member)
		case verbHassNudge:
			// No request body, and the port comes off the volume rather than the wire: the
			// opposite choice from app.home-assistant.readiness, for the same reason the probe
			// below reads its manifest. The app this verb talks to is not the one its caller is
			// installing, so the host is not holding its manifest when it calls. The volume's copy
			// is the document the renderer named the container from, so the port cannot drift
			// from what is actually running and cannot be steered from the wire.
			raw, err := x.ReadFile(manifestPath(hass.Name))
			if err != nil {
				// Not an error: this is the answer, not the absence of one. A node with no Home
				// Assistant is the ordinary case (a broker-only node, the shipped zero-app one),
				// and its callers fire this at every install and uninstall without knowing which.
				// `false` says there was nobody to tell.
				return false, nil
			}
			m, _, err := manifest.Parse(raw)
			if err != nil {
				return nil, fmt.Errorf("%s: %s does not parse: %w", verbHassNudge, hass.Name, err)
			}
			if err := hass.Nudge(ctx, x, m.Primary().Port); err != nil {
				return nil, err
			}
			return true, nil
		case verbDashboardHandoff:
			var h dashboard.Handoff
			if err := json.Unmarshal(payload, &h); err != nil {
				return nil, err
			}
			if h.Code == "" {
				return nil, fmt.Errorf("%s: no code", verbDashboardHandoff)
			}
			if out, err := x.Run(ctx, "mkdir", "-p", "-m", "0700", dashboard.Dir); err != nil {
				return nil, fmt.Errorf("%s: %w: %s", verbDashboardHandoff, err, strings.TrimSpace(string(out)))
			}
			raw, err := json.Marshal(h)
			if err != nil {
				return nil, err
			}
			// Written beside its final name and moved in, so the dashboard never reads half a code;
			// 0600 before the move, so no reader but root ever sees it at all.
			tmp := dashboard.HandoffPath + ".new"
			if err := x.WriteFile(tmp, raw); err != nil {
				return nil, fmt.Errorf("%s: %w", verbDashboardHandoff, err)
			}
			if out, err := x.Run(ctx, "chmod", "0600", tmp); err != nil {
				return nil, fmt.Errorf("%s: %w: %s", verbDashboardHandoff, err, strings.TrimSpace(string(out)))
			}
			if out, err := x.Run(ctx, "mv", "-f", tmp, dashboard.HandoffPath); err != nil {
				return nil, fmt.Errorf("%s: %w: %s", verbDashboardHandoff, err, strings.TrimSpace(string(out)))
			}
			return nil, nil
		case verbDashboardCasa, verbDashboardAlerts, verbDashboardBackup, verbDashboardCatalog:
			// Four of the host's facts for the page, written the same way; the payload is parsed
			// as its own shape, so a malformed one never reaches the page.
			// The alert copy's directory is readable by more than the page (dashboard.AlertsDir);
			// the backup view carries the key, so it stays in the page's own.
			var v any = &dashboard.Casa{}
			path, dir, mode := dashboard.CasaPath, dashboard.Dir, "0700"
			switch verb {
			case verbDashboardAlerts:
				v, path, dir, mode = &[]notify.Record{}, dashboard.AlertsPath, dashboard.AlertsDir, "0755"
			case verbDashboardBackup:
				v, path = &dashboard.Backup{}, dashboard.BackupPath
			case verbDashboardCatalog:
				v, path = &dashboard.Catalog{}, dashboard.CatalogPath
			}
			if err := json.Unmarshal(payload, v); err != nil {
				return nil, err
			}
			if out, err := x.Run(ctx, "mkdir", "-p", "-m", mode, dir); err != nil {
				return nil, fmt.Errorf("%s: %w: %s", verb, err, strings.TrimSpace(string(out)))
			}
			raw, err := json.Marshal(v)
			if err != nil {
				return nil, err
			}
			// Beside its final name and moved in, like the handoff: the page never reads half a state.
			tmp := path + ".new"
			if err := x.WriteFile(tmp, raw); err != nil {
				return nil, fmt.Errorf("%s: %w", verb, err)
			}
			if out, err := x.Run(ctx, "mv", "-f", tmp, path); err != nil {
				return nil, fmt.Errorf("%s: %w: %s", verb, err, strings.TrimSpace(string(out)))
			}
			if c, ok := v.(*dashboard.Casa); ok {
				// The same view, as Home Assistant's integration reads it: the external URL, named
				// only once the household's network resolves the name (hass.ExternalURL). 0644 in
				// HA's app directory, beside a 0600 token, so its own mode is set.
				if out, err := x.Run(ctx, "mkdir", "-p", hass.Dir); err != nil {
					return nil, fmt.Errorf("%s: %w: %s", verb, err, strings.TrimSpace(string(out)))
				}
				tmp := hass.ExternalURLPath + ".new"
				if err := x.WriteFile(tmp, []byte(hass.ExternalURL(*c))); err != nil {
					return nil, fmt.Errorf("%s: %w", verb, err)
				}
				if out, err := x.Run(ctx, "chmod", "0644", tmp); err != nil {
					return nil, fmt.Errorf("%s: %w: %s", verb, err, strings.TrimSpace(string(out)))
				}
				if out, err := x.Run(ctx, "mv", "-f", tmp, hass.ExternalURLPath); err != nil {
					return nil, fmt.Errorf("%s: %w: %s", verb, err, strings.TrimSpace(string(out)))
				}
			}
			if c, ok := v.(*dashboard.Catalog); ok {
				// The same view, as Home Assistant's integration reads it: the installed apps'
				// releases, beside the alert copy in the directory its container sees.
				raw, err := json.Marshal(c.Releases())
				if err != nil {
					return nil, err
				}
				if out, err := x.Run(ctx, "mkdir", "-p", "-m", "0755", dashboard.AlertsDir); err != nil {
					return nil, fmt.Errorf("%s: %w: %s", verb, err, strings.TrimSpace(string(out)))
				}
				tmp := dashboard.AppsPath + ".new"
				if err := x.WriteFile(tmp, raw); err != nil {
					return nil, fmt.Errorf("%s: %w", verb, err)
				}
				if out, err := x.Run(ctx, "mv", "-f", tmp, dashboard.AppsPath); err != nil {
					return nil, fmt.Errorf("%s: %w: %s", verb, err, strings.TrimSpace(string(out)))
				}
			}
			return nil, nil
		case verbMosquittoProbe:
			var req mosquittoProbeRequest
			if err := json.Unmarshal(payload, &req); err != nil {
				return nil, err
			}
			// The container is derived from the manifest on the volume. The name becomes a podman
			// argument, and the volume's copy is the same document the renderer named the
			// container from -- so it cannot be steered from the wire and cannot drift from what
			// is actually running. Which app it is, is the verb's to say.
			raw, err := x.ReadFile(manifestPath(mosquitto.Name))
			if err != nil {
				return nil, fmt.Errorf("%s: %s is not installed on this volume: %w", verbMosquittoProbe, mosquitto.Name, err)
			}
			m, _, err := manifest.Parse(raw)
			if err != nil {
				return nil, fmt.Errorf("%s: %s does not parse: %w", verbMosquittoProbe, mosquitto.Name, err)
			}
			return mosquitto.Probe(ctx, x, quadlet.ContainerName(m.Name, m.Primary().Name), req.Token)
		case verbAppConverge:
			// The input is the volume, which is the point: a caller that could name what to
			// converge to would put the node back on being told what it runs. The one thing a
			// caller may name is the install it is running right now, whose staged manifest is then
			// the one rendered for that app (staging.go); every other staged install is held back.
			// It answers with the apps it could not prepare, having started every other one (see
			// Converge on why the two callers need opposite things from that list).
			var req appConvergeRequest
			if len(payload) > 0 {
				if err := json.Unmarshal(payload, &req); err != nil {
					return nil, err
				}
			}
			return Converge(ctx, x, req.Live)
		case verbAppStage:
			var req appStageRequest
			if err := json.Unmarshal(payload, &req); err != nil {
				return nil, err
			}
			return nil, stageApp(ctx, x, run, req)
		case verbAppUninstall:
			var req appUninstallRequest
			if err := json.Unmarshal(payload, &req); err != nil {
				return nil, err
			}
			return nil, uninstallApp(ctx, x, run, req)
		case verbAppReinstate:
			var req appReinstateRequest
			if err := json.Unmarshal(payload, &req); err != nil {
				return nil, err
			}
			return nil, reinstateApp(ctx, x, run, req)
		case verbAppCommit, verbAppDiscard:
			var req appInstalledRequest
			if err := json.Unmarshal(payload, &req); err != nil {
				return nil, err
			}
			if verb == verbAppCommit {
				return nil, commitApp(ctx, x, run, req.Name)
			}
			return nil, discardApp(ctx, x, run, req.Name)
		case verbAppPending:
			return pendingInstalls(ctx, x)
		case verbAppList:
			// The same listing converge itself renders from, so what the host learns a node runs
			// and what the node actually renders cannot drift: one reader, one directory. Names
			// carry the `.json` suffix on disk; the caller wants app names.
			names, err := manifestNames(ctx, x)
			if err != nil {
				return nil, err
			}
			out := make([]string, 0, len(names))
			for _, n := range names {
				out = append(out, strings.TrimSuffix(n, ".json"))
			}
			return out, nil
		case verbAppPulling:
			var req appPullingRequest
			if err := json.Unmarshal(payload, &req); err != nil {
				return nil, err
			}
			if err := safeUnitName(req.App); err != nil { // the name becomes a path element
				return nil, err
			}
			path := dashboard.PullPath(req.App)
			if req.Done {
				// Absent is the state; a file that was never there is not an error.
				if _, err := x.Run(ctx, "rm", "-f", path); err != nil {
					return nil, fmt.Errorf("%s: %w", verbAppPulling, err)
				}
				return nil, nil
			}
			raw, err := json.Marshal(dashboard.Pull{App: req.App, Size: req.Size, InstalledSize: req.InstalledSize, Started: time.Now().UTC()})
			if err != nil {
				return nil, err
			}
			if _, err := x.Run(ctx, "mkdir", "-p", "-m", "0700", dashboard.Dir); err != nil {
				return nil, fmt.Errorf("%s: %w", verbAppPulling, err)
			}
			if err := x.WriteFile(path, raw); err != nil {
				return nil, fmt.Errorf("%s: %w", verbAppPulling, err)
			}
			return nil, nil
		case verbDeadmanEpisode:
			// Read, then remove: the host records what it is told, so a second read must say
			// nothing. A missing file is the common answer and not an error.
			path := deadmanLastEpisodePath
			b, err := os.ReadFile(path)

			if errors.Is(err, os.ErrNotExist) {
				return deadmanEpisodeReply{}, nil
			}
			if err != nil {
				return nil, fmt.Errorf("%s: %w", verbDeadmanEpisode, err)
			}
			var r deadmanEpisodeReply
			if err := json.Unmarshal(b, &r.Episode); err != nil {
				_ = os.Remove(path) // unreadable: nothing the host can record; do not serve it forever
				return nil, fmt.Errorf("%s: %s: %w", verbDeadmanEpisode, path, err)
			}
			r.Found = true
			if err := os.Remove(path); err != nil {
				return nil, fmt.Errorf("%s: forget %s: %w", verbDeadmanEpisode, path, err)
			}
			return r, nil
		case verbStorageFree:
			// The image store's filesystem, in bytes, from df -- the one tool that answers the
			// question the same way on every root filesystem the guest might have.
			out, err := x.Run(ctx, "df", "-B1", "--output=avail,size", storageRoot)
			if err != nil {
				return nil, fmt.Errorf("%s: %w: %s", verbStorageFree, err, strings.TrimSpace(string(out)))
			}
			lines := strings.Split(strings.TrimSpace(string(out)), "\n")
			if len(lines) < 2 {
				return nil, fmt.Errorf("%s: df said %q", verbStorageFree, strings.TrimSpace(string(out)))
			}
			f := strings.Fields(lines[len(lines)-1])
			if len(f) != 2 {
				return nil, fmt.Errorf("%s: df said %q", verbStorageFree, lines[len(lines)-1])
			}
			free, err1 := strconv.ParseInt(f[0], 10, 64)
			total, err2 := strconv.ParseInt(f[1], 10, 64)
			if err1 != nil || err2 != nil {
				return nil, fmt.Errorf("%s: df said %q", verbStorageFree, lines[len(lines)-1])
			}
			return storageFreeReply{Free: free, Total: total, Path: storageRoot}, nil
		case verbStorageGrow:
			var req storageGrowRequest
			if err := json.Unmarshal(payload, &req); err != nil {
				return nil, err
			}
			return nil, growStateDisk(ctx, x, req.Size)
		case verbAppInstalled:
			var req appInstalledRequest
			if err := json.Unmarshal(payload, &req); err != nil {
				return nil, err
			}
			if err := safeUnitName(req.Name); err != nil { // the name becomes a path element
				return nil, err
			}
			// "" when this app is not installed -- the zero-app node and a node running
			// only other apps are both legitimate states, not errors, so an absent file must
			// not read as a failure.
			out, err := x.Run(ctx, "cat", manifestPath(req.Name))
			if err != nil {
				return "", nil
			}
			return string(out), nil
		case verbOSSystem:
			out, err := x.Run(ctx, "readlink", "-f", currentSystem)
			if err != nil {
				return nil, err
			}
			return strings.TrimSpace(string(out)), nil
		case verbOSState:
			return systemState(ctx, x), nil
		case guestfirmware.VerbOSPowerOff:
			// The first-choice clean shutdown, and the firmware's: the host may have to stop a
			// guest it has never dressed, so the verb lives with the half the image bakes. Why it
			// is detached from the dispatch context, and why the command's exit status is not the
			// answer, is at guestfirmware.PowerOff.
			return nil, guestfirmware.PowerOff(ctx, x)
		case verbResources:
			var req resourcesRequest
			if err := json.Unmarshal(payload, &req); err != nil {
				return nil, err
			}
			// Best-effort: telemetry is a signal, never a gate, so a failed sub-read leaves
			// its field zero rather than failing the verb and losing every other field with it.
			// The struct always comes back; nil error.
			return gatherResources(ctx, x, req), nil
		case verbReactorActive:
			// A guard: lets a bracket user refuse to start when the promoter is already paused,
			// i.e. when someone else is mid-operation. It cannot prevent the race (a pause can
			// still land between this check and ours) and is not claimed to; it turns the likely
			// overlap from silent corruption of the bracket into a loud refusal. A lone node runs
			// no promoter, so nothing can be paused: the honest answer to "is anyone
			// mid-operation" is no.
			if alone(x) {
				return true, nil
			}
			out, err := x.Run(ctx, "systemctl", "is-active", "drbd-reactor.service")
			if err != nil {
				// Is-active exits non-zero for every not-active state, which is an answer
				// ("paused"), not a failure. Reporting an error here would make a correctly
				// paused node look broken.
				return false, nil
			}
			return strings.TrimSpace(string(out)) == "active", nil
		case verbReactorEvict:
			// A planned handover: give the work to a peer while perfectly healthy, so this node can
			// reboot into a new generation without taking the house down with it. drbd-reactor's
			// own `evict` runtime-masks the promoter target, stops it so a peer promotes, and
			// unmasks; our own stop-and-hope against the promoter would reimplement that with less
			// knowledge of its state machine. Proven, not assumed (nixosTest/reactor-evict.nix):
			// the work moves in ~6s and the data comes with it.
			//
			// ⚠️ It says "not me", not "you": the destination is drbd-reactor's election, not ours,
			// and on a flock with three diskful anchors the work may land on the third node. Our
			// shipped topology (2 anchors + a diskless witness, which cannot hold the resource)
			// makes it deterministic, but that is a property of the topology, so a caller that
			// needs the work to land somewhere specific must assert where it went.
			//
			// KeepMasked is for the reboot path: without it the node is immediately eligible again,
			// so an ex-primary rebooting for its own upgrade could take the work back before anyone
			// has verified its new generation. The mask is `--runtime` and so does not survive the
			// reboot; carrying it across is the sequencer's problem, not this verb's. Unmask
			// releases it (the deliberate hand-back) and runs no eviction.
			var req evictRequest
			if err := json.Unmarshal(payload, &req); err != nil {
				return nil, err
			}
			// The lone node's evict is the same gesture on the chain target, not a refusal: "not
			// me" is a stop of the chain (the house goes dark: there is nobody to take it, and the
			// install rigs assert exactly that, off-box), keep-masked masks the target so nothing
			// starts it back (the hold's own release tries and is refused, as the reactor's
			// re-promotion is on a flock), and unmask releases it and starts the chain, which is
			// what the reactor's election does two seconds after an unmask. A plain evict is
			// therefore a restart. The stop names the first member beside the target: every other
			// member Requires= it transitively, so that one stop waits for all of them, where a
			// target's own stop job returns as soon as the target is down.
			//
			// ⚠️ The mask and the unit file are the same path: this agent renders the target into
			// the directory `mask --runtime` writes its /dev/null symlink into. Both directions go
			// through the renderer so there is one owner: masking clears the file first (systemctl
			// refuses outright over an existing one), and unmasking re-renders, because dropping
			// the symlink otherwise leaves no unit at all.
			if alone(x) {
				switch {
				case req.Unmask:
					if err := UnmaskRendered(ctx, x, chainTarget); err != nil {
						return nil, err
					}
				default:
					if err := run("systemctl", "stop", chainTarget, chainRoot); err != nil {
						return nil, err
					}
					if req.KeepMasked {
						return nil, MaskRendered(ctx, x, chainTarget)
					}
				}
				return nil, run("systemctl", "start", chainTarget)
			}
			args := []string{"evict"}
			switch {
			case req.Unmask:
				args = append(args, "-u")
			case req.KeepMasked:
				args = append(args, "--keep-masked")
			}
			return nil, run("drbd-reactorctl", args...)
		case verbFsSync:
			// Flush the replicated volume's dirty pages before an eviction, so the unmount's
			// writeback (inside the demote path, under the peer's promotion and DRBD's ping
			// deadline) moves only the last seconds of writes instead of everything since the last
			// natural writeback. Protocol C acks a write once it is on both disks, but dirty page
			// cache is on no disk yet: for a write-heavy payload the unmount flush is unbounded,
			// and this verb is the pre-copy that bounds it.
			//
			// A filesystem mounted at the data root itself means the data volume is mounted here
			// (a Primary). Anything else (a Secondary or witness that never mounted it) has nothing
			// of ours to flush, which is an answer, not an error: the caller asked "make sure your
			// dirty data is small", and it already is.
			if !isMountPoint(x, dataMountRoot) {
				return "skipped: data volume not mounted here", nil
			}
			if err := run("sync", "-f", dataMountRoot); err != nil {
				return nil, err
			}
			return "synced", nil
		case verbNetVIPForget:
			// Only where the volume is mounted, read as fs.sync reads it: anywhere else the path
			// is the empty mountpoint, and "forgotten" would be a claim about a file nobody saw.
			if !isMountPoint(x, dataMountRoot) {
				return nil, fmt.Errorf("the data volume is not mounted here, so its remembered address cannot be forgotten")
			}
			// Synced, for the reason it was written synced: the next promotion may follow a
			// power cut, and a deletion lost to the page cache brings the address back.
			if err := run("rm", "-f", vipAddrFile); err != nil {
				return nil, err
			}
			return nil, run("sync", "-f", dataMountRoot)
		case verbReactorPause:
			// Pause the promoter by stopping the drbd-reactor daemon: stop-services-on-exit
			// defaults false, so the promoted apps + DRBD Primary stay up while it is down
			// (deterministic maintenance mode). The snippet arg is unread: the resource is single,
			// and per-snippet `drbd-reactorctl disable` is not used (its systemctl reload proved
			// flaky). A bare stop: a stopping reactor fires one last `systemctl start
			// drbd-services@r0.target` that its own `Before=` override (reactor-50-before.conf)
			// sequences behind the stop -> neither completes -> 90s TimeoutStopSec SIGKILL.
			// drbd-reactor.service's ExecStop (guest-image/configuration.nix) defuses that
			// deadlock, not this verb, because it hangs every other stop too (a shutdown, the
			// deadman's reboot, a host reboot), none of which come through here; a second copy
			// here would be a second place to keep correct.
			//
			// A lone node has no promoter to pause: its chain is a static target that
			// nothing re-promotes, so the bracket's "hold still" is already the steady state.
			if alone(x) {
				return nil, nil
			}
			return nil, run("systemctl", "stop", "drbd-reactor.service")
		case verbReactorResume:
			// Restart the daemon; it re-reads config and adopts the already-Primary apps,
			// with no restart/demote. (No maintenance marker to clear -- nothing in the guest
			// switches the OS on its own, so nothing autonomous races a managed op.)
			if alone(x) {
				return nil, nil // nothing was paused
			}
			return nil, run("systemctl", "start", "drbd-reactor.service")
		case verbNetConfigure:
			var req netConfigureRequest
			if err := json.Unmarshal(payload, &req); err != nil {
				return nil, err
			}
			// Address the system NIC: this node's node IP, which is how anything reaches it
			// and where DRBD binds. `addr replace` is idempotent (no-op if already
			// set), then ensure the link is up. Both take an explicit `dev` (portable across
			// iproute2 versions). An empty Dev/CIDR still skips addressing -- the agent-less
			// harnesses send that, and a witness has no VIP device either.
			if req.Dev != "" && req.CIDR != "" {
				if err := run("ip", "addr", "replace", req.CIDR, "dev", req.Dev); err != nil {
					return nil, err
				}
				// Prune any other global v4 address on this NIC. `addr replace` adds and updates but
				// never removes, so a renumber (exactly what an adoption does to the joiner, whose
				// island subnet gives way to the adopter's) would leave the old address alongside
				// the new one: two plausible sources for DRBD to bind. Add first, prune second,
				// never flush-then-add: the flush form leaves the NIC momentarily addressless, and
				// this call runs on a node that may have a peer connected.
				if held, err := x.IPv4Addrs(req.Dev); err == nil {
					for _, held := range held {
						if held == req.CIDR {
							continue
						}
						if err := run("ip", "addr", "del", held, "dev", req.Dev); err != nil {
							return nil, err
						}
					}
				}
				if err := run("ip", "link", "set", "dev", req.Dev, "up"); err != nil {
					return nil, err
				}
			}
			// The pod pool, recorded rather than configured; nothing is created here. The pool is
			// the range converge allocates each private app's network from, and those networks
			// come into being when an app is rendered, not at bring-up; writing it to /run makes it
			// readable by converge, which runs later and in another process. Absent is a real state
			// and stays absent (a witness draws no pool): converge then refuses a private app, the
			// honest failure. The alternative, the guest's own namespace, is host networking under
			// another name and would grant by accident exactly the capability the manifest field
			// exists to make deliberate.
			if req.PodSubnet != "" {
				if err := x.WriteFile(podSubnetPath, []byte(req.PodSubnet+"\n")); err != nil {
					return nil, err
				}
			}
			// The route back to our own host over the private link, plus a permanent neighbour
			// entry for the host's end of it; both halves are load-bearing. The route is a /32 so
			// it beats the on-link /24 the system address above installs: under macvtap that
			// on-link path leaves by eth1 towards a host the substrate isolates us from, so every
			// reply to a host-originated packet would go out the wrong door.
			//
			// ⚠️ The neighbour entry is why this link can be unnumbered at all, found by capture,
			// not by reasoning. When this NIC has no address (PrivCIDR below is optional), an ARP
			// for the host has no source address on the outgoing interface, and arp_announce=2
			// makes the kernel borrow one from another NIC: eth0's slirp address, 10.0.2.15,
			// which is also what every other qemu user-net picks. The request goes out as
			// "who-has <host> tell 10.0.2.15", the host sees an ARP request apparently from its
			// own address, and never answers. Pinning the MAC means we never ask. The link is
			// symmetric: neither end can ARP usefully across an unnumbered wire, so both ends
			// pin. The host's mirror is platform.SetNodeRoute, which pins for a different reason:
			// our node IP lives on eth1 while its request would arrive on this NIC, which
			// arp_ignore=1 refuses.
			if req.PrivDev != "" && req.PrivHostIP != "" {
				if err := run("ip", "link", "set", "dev", req.PrivDev, "up"); err != nil {
					return nil, err
				}
				// This end's own address on the link: substrate, and nothing addresses it (the gate
				// answers at the node IP, the forwarder is dialled at the node IP, the VIP is routed
				// via the node IP). It exists for one measured reason: avahi joins the IPv4 mDNS
				// group on an interface only if that interface has a v4 address, so without it this
				// NIC answers mDNS over IPv6 only, and the host's end of that conversation is a
				// stranger's machine, which may have IPv6 off. The mDNS name half then breaks
				// silently: the household's own machine cannot find its own node while everything
				// else works. install-macvtap runs with v6 disabled on the host precisely so nothing
				// can pass for a reason we do not control.
				if req.PrivCIDR != "" {
					if err := run("ip", "addr", "replace", req.PrivCIDR, "dev", req.PrivDev); err != nil {
						return nil, err
					}
				}
				if req.PrivHostMAC != "" {
					if err := run("ip", "neigh", "replace", req.PrivHostIP,
						"lladdr", req.PrivHostMAC, "dev", req.PrivDev, "nud", "permanent"); err != nil {
						return nil, err
					}
				}
				if err := run("ip", "route", "replace", req.PrivHostIP+"/32", "dev", req.PrivDev); err != nil {
					return nil, err
				}
			}
			// The service identity, made here. Under the bridge substrate the host hands us one NIC,
			// so the second MAC this node must present (the flock-scoped one the VIP rides and a
			// failover moves) has to be created inside the guest as a macvlan child. Under macvtap
			// this block does nothing: the host built the device and VIPParent is empty. `mode
			// bridge` so the child and its parent can talk to each other as well as to the LAN. The
			// MAC is set at creation rather than after, so the device never exists holding a random
			// one: a macvlan comes up with a kernel-random MAC, and a frame emitted in that window
			// teaches the switch a port for an address nobody owns.
			//
			// ⚠️ Created down, and left down: `ip link add` leaves a device administratively down,
			// which is exactly the standby discipline. briard-vip.service brings VIP_DEV up on
			// promotion and down on demotion, because a Secondary holding the flock MAC up teaches
			// the switch the wrong port for the VIP the moment it emits anything at all; bringing
			// it up here would hand every standby that defect.
			if req.VIPParent != "" && req.VIPDev != "" {
				// The parent, up. Usually redundant with the system-NIC block above (VIPParent is
				// req.Dev in the shipped shape), but this block must stand on its own: a caller may
				// name a parent that block never touched, and a macvlan on a down parent is a device
				// that exists and carries nothing.
				if err := run("ip", "link", "set", "dev", req.VIPParent, "up"); err != nil {
					return nil, err
				}
				if _, err := x.Run(ctx, "ip", "link", "show", "dev", req.VIPDev); err != nil {
					add := []string{"link", "add", req.VIPDev, "link", req.VIPParent}
					if req.VIPMAC != "" {
						add = append(add, "address", req.VIPMAC)
					}
					if err := run("ip", append(add, "type", "macvlan", "mode", "bridge")...); err != nil {
						return nil, err
					}
				} else if req.VIPMAC != "" {
					// It already exists (a pairing re-runs this call on a running guest) --
					// re-assert the MAC rather than assume the device still carries it.
					if err := run("ip", "link", "set", "dev", req.VIPDev, "address", req.VIPMAC); err != nil {
						return nil, err
					}
				}
			}
			// The VIP's device and address are agent-determined: record them where
			// briard-vip.service reads them, a required EnvironmentFile with nothing baked behind
			// it. Idempotent (whole-file write). An unset VIP_ADDR is the ordinary DHCP case and
			// the unit expects it. An unset VIP_DEV means the node claims no VIP at all (a witness,
			// which has no promoter and never starts briard-vip); a data node reaching promotion
			// without one is a broken configuration and fails loudly there, rather than guessing a
			// NIC (a guess lands on the replication NIC and takes a second DHCP lease on it).
			var env []byte
			if req.VIPDev != "" {
				env = append(env, "VIP_DEV="+req.VIPDev+"\n"...)
			}
			// SYSTEM_DEV beside it, not decoration: the VIP unit's stop path needs to know whether
			// VIP_DEV is a dedicated service NIC or the DRBD NIC. A standby must bring a dedicated
			// one down (the flock MAC is shared with the peer, and a Secondary emitting from it
			// teaches the switch the wrong port), while a link-down on the DRBD NIC would take
			// replication with it. Only the agent knows which is which; the guest bakes no
			// positional knowledge (briard-vip-down, vipDown, in guest-image/configuration.nix).
			// Gated on VIPDev on purpose: a node with no VIP device is a witness, which
			// has no promoter and never starts briard-vip, and writing this file for it would hand
			// a unit that must not run a required EnvironmentFile it now satisfies.
			if req.VIPDev != "" && req.Dev != "" {
				env = append(env, "SYSTEM_DEV="+req.Dev+"\n"...)
			}
			// VIP_MAC, the flock MAC, is the VIP's DHCP identity: its client-id and hostname. Stated
			// rather than read off the NIC: the identity is the flock's, never a property of the NIC
			// carrying it, and it is the one the host asks under where it holds the lease (VIPLease).
			if req.VIPDev != "" && req.VIPMAC != "" {
				env = append(env, "VIP_MAC="+req.VIPMAC+"\n"...)
			}
			if req.VIPAddr != "" {
				env = append(env, "VIP_ADDR="+req.VIPAddr+"\n"...)
			}
			if req.VIPDev != "" && req.VIPLease {
				env = append(env, "VIP_LEASE=host\n"...)
			}
			if req.VIPDev != "" && req.VIPRouter != "" {
				env = append(env, "VIP_ROUTER="+req.VIPRouter+"\n"...)
			}
			if len(env) > 0 {
				return nil, x.WriteFile(vipEnvPath, env)
			}
			return nil, nil
		case verbNetVIP:
			var req netVIPRequest
			if err := json.Unmarshal(payload, &req); err != nil {
				return nil, err
			}
			if req.Dev == "" {
				return "", nil // no VIP device on this node (a witness) -- not an error
			}
			// Only global IPv4 addresses count, so a device that is up but unaddressed reads as ""
			// rather than as an fe80:: or a self-assigned 169.254 the caller would have to know to
			// discard. An error means the device is absent; that is "no address", not a failure to
			// report one -- the host asks this every cycle and a transient absence must not read as
			// a dead channel. The first address is the NIC's own; any later one is a secondary.
			held, err := x.IPv4Addrs(req.Dev)
			if err != nil || len(held) == 0 {
				return "", nil
			}
			return held[0], nil
		case verbNetMDNSName:
			var req mdnsNameRequest
			if err := json.Unmarshal(payload, &req); err != nil {
				return nil, err
			}
			if req.Name == "" {
				// Publish nothing rather than publish a guess. An empty name is what a node with
				// no minted flock name sends, and `briard-.local` is worse than silence.
				return nil, nil
			}
			if err := x.WriteFile(mdnsEnvPath, []byte("FLOCK_NAME="+req.Name+"\n")); err != nil {
				return nil, err
			}
			// Nothing is restarted, and nothing needs to be: the reverse proxy publishes the names
			// and re-reads this file on its own tick, so a rename reaches the wire by itself, on a
			// Primary, the only node whose reverse proxy is running and so the only node that
			// could publish a name for an address it holds. The rename applies without touching
			// addressing (no VIP re-assert, no MAC, no DHCP client-id, no DRBD state): the property
			// the three-way identifier split was for, cashed in by these two writes. The per-app
			// names are derived from this one, so they move with it, in the routing table the
			// reverse proxy matches on and in the records it answers, which are the same list read
			// once, so a rename cannot leave a name published that nothing routes.
			renameRoutes(x, req.Name)
			return nil, nil
		case verbNetMDNSPublished:
			// The name the reverse proxy is really answering for, as it records it from what it
			// serves -- never the requested name, which would report a name present, plausible,
			// and not necessarily the one being served.
			out, err := x.ReadFile(mdnsPublishedPath)
			if err != nil {
				// Absent is the normal answer on a Secondary (nothing published), so it is "" and
				// not an error. Any other read failure is also "": the host asks every cycle, and
				// a transient unreadable file must not read as a dead channel.
				return "", nil
			}
			return strings.TrimSpace(string(out)), nil
		case verbNetMDNSOther:
			// Absent is "none heard", the common answer; any other read failure is "" for the
			// reason verbNetMDNSPublished gives.
			out, err := x.ReadFile(mdnsOtherPath)
			if err != nil {
				return "", nil
			}
			return strings.TrimSpace(string(out)), nil
		default:
			return nil, fmt.Errorf("guestagent: unknown verb %q", verb)
		}
	}
}

// Serve runs the guest dispatch loop over conn until it closes or ctx is done.
func Serve(ctx context.Context, conn io.ReadWriteCloser, x Executor) error {
	return guestfirmware.ServeFrames(ctx, conn, dispatch(x), isAct)
}

// ContactStampPath is the last-seen-host-agent stamp: the guest agent bumps its mtime on every
// served request, and the separate briard-deadman process (RunDeadman) reads that mtime to tell
// how long the host has been silent. On tmpfs (/run), so it re-baselines each boot: the deadman
// stays disarmed until the host talks once (StampMtime → zero → not armed). A stamp file, not
// in-process state, because the per-connection guest agent restarts in a loop while the host is
// down (the reopened virtio-serial port EOFs), which would reset any in-process timer.
const ContactStampPath = dashboard.ContactStampPath

// ServeStamped serves the dispatch loop and bumps the contact stamp on every request (the
// production entry, briard-guest-agent's runGuest). The deadman reads the stamp from its own
// process (RunDeadman). Plain Serve is for tests (no stamp side effect).
func ServeStamped(ctx context.Context, conn io.ReadWriteCloser, x Executor) error {
	d := dispatch(x)
	hooked := func(ctx context.Context, verb string, payload json.RawMessage) (any, error) {
		touchStamp(ContactStampPath) // the host agent just talked to us — freshen the deadman's stamp
		if verb == guestfirmware.VerbHello {
			log.Printf("control: the host connected") // the one request per session worth a line
		}
		return d(ctx, verb, payload)
	}
	return guestfirmware.ServeFrames(ctx, conn, hooked, isAct)
}

// touchStamp bumps path's mtime to now (creating it, and its dir, if absent). Best-effort: a
// failure just means the deadman sees a slightly staler stamp, never a crash.
func touchStamp(path string) {
	now := time.Now()
	if err := os.Chtimes(path, now, now); err != nil {
		_ = os.MkdirAll(filepath.Dir(path), 0o755)
		if f, e := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o644); e == nil {
			f.Close()
		}
		_ = os.Chtimes(path, now, now)
	}
}

// StampMtime returns the contact stamp's mtime, or the zero time when it doesn't exist yet (no
// host contact this boot → the deadman treats that as "not armed").
func StampMtime(path string) time.Time {
	fi, err := os.Stat(path)
	if err != nil {
		return time.Time{}
	}
	return fi.ModTime()
}

// deadmanStatePath holds the deadman's backoff episode, node-local (the state disk's bind at
// /var/lib/briard-deadman, not the replicated volume at /var/lib/briard), so it survives a reboot
// without being shared or overwritten across nodes.
const deadmanStatePath = "/var/lib/briard-deadman/episode.json"

// deadmanLastEpisodePath is where the deadman leaves a finished episode for the host
// (deadman.episode). A variable so a test can point the verb at a file of its own.
var deadmanLastEpisodePath = deadman.FileState{Path: deadmanStatePath}.LastPath()

// RunDeadman runs the host-agent deadman as its own long-running process, the briard-deadman
// guest unit, so the guest agent's restart loop while the host is down cannot reset the timer.
// Once the contact stamp ServeStamped bumps shows the host agent silent past T_deadman
// (BRIARD_DEADMAN[/_JITTER/_TICK] tune the threshold), it reboots the guest: gracefully, so the
// promoter teardown demotes cleanly and the reboot is the failover trigger, and only when
// deadman.RebootAllowed says no peer loses quorum over it.
//
// It also serves that same gate at BRIARD_GATE_ADDR (deadman/gate.go; the unit sets a bare port, so
// it answers on the node IP), so the host's own rung can ask the one question it cannot answer
// from outside.
// No address, no listener. A bind failure is logged, not fatal: losing the gate costs the host its
// guard, while exiting here would cost the node its reflex entirely.
func RunDeadman(ctx context.Context) error {
	node, _ := os.Hostname()
	x := NewOSExecutor()
	gate := &deadman.Gate{
		Logf: func(f string, a ...any) { fmt.Fprintf(os.Stderr, "briard-deadman: "+f+"\n", a...) },
	}
	if addr := os.Getenv("BRIARD_GATE_ADDR"); addr != "" {
		go func() {
			if err := gate.Serve(ctx, addr); err != nil && ctx.Err() == nil {
				fmt.Fprintf(os.Stderr, "briard-deadman: gate listener down, the host rung is blind: %v\n", err)
			}
		}()
	}
	mon := &deadman.Monitor{
		Node:        node,
		Base:        durationEnv("BRIARD_DEADMAN", deadman.DefaultDeadman),
		Window:      durationEnv("BRIARD_DEADMAN_JITTER", deadman.DefaultJitter),
		Tick:        durationEnv("BRIARD_DEADMAN_TICK", 0),
		LastContact: func() time.Time { return StampMtime(ContactStampPath) },
		Gate:        gate,
		Fabric: func(ctx context.Context) (deadman.Fabric, error) {
			// A lone node has no DRBD to read: one voter, quorum guaranteed, nobody to protect.
			// Reading the spec the storage was built from is the price of knowing whether it is alone. No
			// spec reads as a flock, and the read below then fails, as it does before bring-up:
			// not knowing means holding.
			if alone(x) {
				return deadman.Fabric{Quorate: true}, nil
			}
			out, err := x.Run(ctx, "drbdsetup", "status", "--json")
			if err != nil {
				return deadman.Fabric{}, err
			}
			peers, connected, quorate, err := drbd.PeerCounts(out)
			return deadman.Fabric{Peers: peers, Connected: connected, Quorate: quorate}, err
		},
		Reboot: func(ctx context.Context) error {
			out, err := x.Run(ctx, "systemctl", "reboot") // graceful, never `reboot -f`
			if err != nil {
				return fmt.Errorf("systemctl reboot: %w: %s", err, bytes.TrimSpace(out))
			}
			return nil
		},
		// The deadman raises no alert of its own: its only way out of the house is the host
		// agent, the thing that is down when it acts. It logs (this process's stderr, the guest's
		// journal, the serial console) and leaves the finished episode beside its state file
		// for the host to collect (the deadman.episode verb) and turn into the owner's alert.
		Logf:  func(f string, a ...any) { fmt.Fprintf(os.Stderr, "briard-deadman: "+f+"\n", a...) },
		State: deadman.FileState{Path: deadmanStatePath},
	}
	return mon.Run(ctx)
}

// reactorPath is where the agent drops drbd-reactor's promoter snippet. On tmpfs: the host
// re-derives it from cfg.Promoter at every bring-up, so a persistent copy could only ever be a
// stale one that outlived the agent that wrote it, and a reactor with no snippet is idle, a
// second, independent backstop to the arm-at-bring-up gate. Paired with
// guest-image/configuration.nix's reactorSnippetDir, which points drbd-reactor.toml's `snippets`
// at this directory (different languages, so no shared import).
const reactorPath = "/run/briard/drbd-reactor.d/briard.toml"

// vipEnvPath is the required EnvironmentFile briard-vip.service reads its VIP_DEV and VIP_ADDR
// from; the agent writes it via net.configure at every bring-up. Nothing is baked behind it
// -- which is safe only because the promoter that starts briard-vip is itself
// agent-started, so nothing can read this file before bring-up has written it.
const vipEnvPath = "/run/briard/vip.env"

const (
	// mdnsEnvPath carries the flock's visible name to the reverse proxy, which publishes it.
	// Written by net.mdnsname, never baked: it is pet identity reaching a cattle image, and a
	// shared image must carry no identity. ⚠️ Paired with reverse-proxy/mdns.go's const of the
	// same path, which re-reads it on a tick, so a rename needs no unit restarted and no ordering
	// between the two.
	mdnsEnvPath = "/run/briard/mdns.env"
	// mdnsPublishedPath is where the reverse proxy records the name it is really answering for,
	// bare. Absent means nothing is published -- the normal state of a Secondary, which holds no
	// VIP, runs no reverse proxy, and therefore publishes no name.
	mdnsPublishedPath = "/run/briard/mdns.published"
	// mdnsOtherPath is where the reverse proxy records another briard heard answering the bare
	// name at bring-up. Paired with reverse-proxy/mdns.go's const of the same path.
	mdnsOtherPath = "/run/briard/mdns.other"
)

// podSubnetPath records the pool this guest allocates private app networks from, written by
// net.configure and read by converge. /run, like every other node-scoped fact the host hands down:
// it is re-sent at every bring-up, so nothing durable can go stale.
const podSubnetPath = "/run/briard/pod.subnet"

// resDir is where the agent drops the DRBD resource config. On tmpfs, like every node-scoped fact
// here: the host re-derives the `.res` at every bring-up (a runtime pairing from its durable mesh
// cache, agent/host's cacheMesh), so the guest keeps nothing node-scoped on its disks, and a
// rebooted guest cannot act on configuration nobody has just restated: the promoter is gated and
// every input is re-pushed at bring-up. Paired with guest-image/disk-image.nix's `include` glob in
// /etc/drbd.conf: drbdadm looks for that one file at a path we do not choose, and it points here.
const resDir = "/run/briard/drbd.d"

func resPath(resource string) string { return filepath.Join(resDir, resource+".res") }

// durationEnv reads a time.Duration from env key k, or returns def when unset/unparseable.
func durationEnv(k string, def time.Duration) time.Duration {
	if d, err := time.ParseDuration(os.Getenv(k)); err == nil && d > 0 {
		return d
	}
	return def
}

// safeUnitName rejects any filename that could escape quadletDir. The host renders these from a
// validated manifest, so this is defence in depth rather than the primary boundary — but the
// guest writes them as root into a directory systemd reads, and a second cheap check beats
// trusting that no host-side regression ever produces a "../" .
func safeUnitName(n string) error {
	if n == "" || strings.ContainsAny(n, `/\`) || strings.Contains(n, "..") {
		return fmt.Errorf("app: unsafe unit filename %q", n)
	}
	return nil
}

// renderApp materialises the host's quadlet source under quadletDir and reloads systemd, the
// moment podman's generator turns those files into real units. Node-local: quadletDir is on /run,
// so each node renders its own, the host at install and at every bring-up (BringUpSpec.AppUnits),
// and converge from the volume at promotion.
func renderApp(ctx context.Context, x Executor, run func(string, ...string) error, req appRenderRequest) error {
	if len(req.Files) == 0 {
		return fmt.Errorf("app.render: no files to write")
	}
	for _, n := range req.Stale {
		if err := safeUnitName(n); err != nil {
			return err
		}
	}
	names := make([]string, 0, len(req.Files))
	for n := range req.Files {
		if err := safeUnitName(n); err != nil {
			return err
		}
		names = append(names, n)
	}
	sort.Strings(names) // deterministic write order, so a failure part-way is reproducible
	if err := run("mkdir", "-p", quadletDir); err != nil {
		return err
	}
	// Remove the prior render's orphans first, so a re-render leaves nothing of the previous one
	// behind. Absent is fine.
	for _, n := range req.Stale {
		_, _ = x.Run(ctx, "rm", "-f", quadletDir+"/"+n)
	}
	for _, n := range names {
		if err := x.WriteFile(quadletDir+"/"+n, []byte(req.Files[n])); err != nil {
			return err
		}
	}
	return run("systemctl", "daemon-reload")
}

// provisionApp creates the app's single btrfs subvolume with plain per-container subdirectories
// and records the manifest as the app's identity. Primary-only: everything it touches is on the
// replicated volume, which is mounted nowhere else. Idempotent: a re-install onto an existing app
// must keep its data, so an existing subvolume is reused (re-creating it would silently delete the
// user's state).
func provisionApp(ctx context.Context, x Executor, run func(string, ...string) error, req appProvisionRequest) error {
	if req.Name == "" || req.DataDir == "" || req.Manifest == "" {
		return fmt.Errorf("app.provision: need a name, a data dir and a manifest")
	}
	if err := safeUnitName(req.Name); err != nil { // the name becomes a path element
		return err
	}
	if err := ensureAppData(ctx, x, run, req.DataDir, req.Subdirs); err != nil {
		return err
	}
	pin := manifestPath(req.Name)
	if err := x.WriteFile(pin, []byte(req.Manifest)); err != nil {
		return err
	}
	// This data is exactly what we just put there, so the next member may say so. Every caller
	// of this verb has the app stopped: a restore, which has just made the app's empty place or
	// materialised a member over it (app.stage records the same for an install or upgrade).
	// Without this the next member would read as crash-consistent: untrue, and loudest on the
	// one path where a household is already anxious.
	if err := RecordAppStop(ctx, x, req.Name, "success"); err != nil {
		log.Printf("app.provision %s: could not record the volume as flushed (%v); its next member will read as crash-consistent", req.Name, err)
	}
	// Flush to the DRBD backing so the identity actually replicates before a failover relies on
	// it (protocol C acks a device write only once the peer holds it). Without this, a crash in
	// the btrfs writeback window loses the manifest and a survivor promotes with no idea what it
	// is meant to be running.
	_, err := x.Run(ctx, "sync", "-f", pin)
	return err
}

// ensureAppData creates an app's areas and the directories its containers mount in them, each
// subdir `<tier>/<dir>` (quadlet.Subdirs), reusing an existing area (re-creating it would silently
// delete the household's state), and the manifest directory beside them. An area exists only in a
// tier the app mounts, and in the data tier always, where its History and its manifest's record
// hang. Each is born into its group of the quota tree (qgroup.go).
func ensureAppData(ctx context.Context, x Executor, run func(string, ...string) error, dataDir string, subdirs []string) error {
	_, app, err := areaOf(dataDir)
	if err != nil {
		return err
	}
	if err := makeArea(ctx, x, run, dataDir); err != nil {
		return err
	}
	for _, s := range subdirs {
		tier, d, _ := strings.Cut(s, "/")
		if !slices.Contains(manifest.Tiers, tier) {
			return fmt.Errorf("subdir %q names no storage tier", s)
		}
		if err := safeUnitName(d); err != nil { // same escape check; a subdir is a bare name
			return err
		}
		area := quadlet.Area(tier, app)
		if err := makeArea(ctx, x, run, area); err != nil {
			return err
		}
		if err := run("mkdir", "-p", area+"/"+d); err != nil {
			return err
		}
	}
	return run("mkdir", "-p", manifestDir)
}

// makeArea makes an app's area in its group of the quota tree, unless it exists.
func makeArea(ctx context.Context, x Executor, run func(string, ...string) error, area string) error {
	if out, err := x.Run(ctx, "btrfs", "subvolume", "show", area); err == nil && len(out) > 0 {
		return nil
	}
	if err := run("mkdir", "-p", filepath.Dir(area)); err != nil {
		return err
	}
	group, err := areaGroup(ctx, x, area)
	if err != nil {
		return err
	}
	return run("btrfs", "subvolume", "create", "-i", group, area)
}

// clearCache empties an app's cache area, the app stopped: the subvolume deleted -- at once, where
// removing its files one by one is minutes for a large cache -- and made again with the
// directories it held, which its containers mount. An app with no cache area has nothing to
// empty. The delete is gated on the area being a subvolume, said positively by btrfs.
func clearCache(ctx context.Context, x Executor, run func(string, ...string) error, app string) error {
	area := quadlet.Area(manifest.TierCache, app)
	if out, err := x.Run(ctx, "btrfs", "subvolume", "show", area); err != nil || len(out) == 0 {
		return nil
	}
	dirs, err := x.ListDir(area)
	if err != nil {
		return fmt.Errorf("list %s: %w", area, err)
	}
	if err := run("btrfs", "subvolume", "delete", area); err != nil {
		return err
	}
	if err := makeArea(ctx, x, run, area); err != nil {
		return err
	}
	for _, d := range dirs {
		if err := run("mkdir", "-p", area+"/"+d); err != nil {
			return err
		}
	}
	return nil
}

// cacheAtStart empties the app's cache as it starts, when its space is short (spaceShort). Only
// at a start that happens anyway -- no restart is forced for it -- and only for an app of one
// container: a second may be running, holding the cache open.
func cacheAtStart(ctx context.Context, x Executor, app string) (string, error) {
	raw, err := x.ReadFile(manifestPath(app))
	if err != nil {
		return "", fmt.Errorf("read the running manifest: %w", err)
	}
	m, _, err := manifest.Parse(raw)
	if err != nil {
		return "", err
	}
	if !m.UsesTier(manifest.TierCache) || len(m.Containers) != 1 {
		return "", nil
	}
	tree, err := readQgroups(ctx, x)
	if err != nil {
		return "", err
	}
	if g, ok := tree[appQgroup(tierData, app)]; !ok || !spaceShort(g) {
		return "", nil
	}
	run := func(name string, args ...string) error {
		if out, err := x.Run(ctx, name, args...); err != nil {
			return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
		}
		return nil
	}
	if err := clearCache(ctx, x, run, app); err != nil {
		return "", err
	}
	return "its space is short: emptied its cache", nil
}

// spaceShort: the app's data group -- its data, cache and unversioned areas, and its History --
// has less than a tenth of its limit left.
func spaceShort(g qgroup) bool { return g.limit >= 0 && g.limit-g.referenced < g.limit/10 }

// gatherResources measures the appliance's resource footprint for the soak trend oracle, filling
// only the appliance (guest) fields; the host adds its own Agent* footprint on top. Every
// sub-metric is best-effort: a failed read leaves its field zero (the host reads 0 as "unread this
// cycle"), so a hiccup in one probe never sinks the whole report or the observe loop. Zero
// DataDir/Unit (a witness) skips the volume/app metrics.
func gatherResources(ctx context.Context, x Executor, req resourcesRequest) telemetry.NodeResources {
	var r telemetry.NodeResources
	// One entry per app, in the order the host sent them -- so the oracle can name which
	// app leaked or crash-looped. An app whose unit is unnamed is skipped rather than
	// measured as zero: an unmeasured app and an idle one must not read the same.
	for _, s := range req.Apps {
		if s.Unit == "" {
			continue
		}
		sr := telemetry.AppResources{Name: s.Name}
		// The app's memory is the unit's cgroup, not its main process (cgroupAnonKB says why):
		// /proc/<MainPID>/status reads ~2.5 MB for a Home Assistant -- stable, believable-looking
		// and about a hundredfold wrong.
		sr.RSSKB = cgroupAnonKB(ctx, x, s.Unit)
		// MainPID=0 means the unit isn't running; skip rather than read /proc/0.
		if out, err := x.Run(ctx, "systemctl", "show", "-p", "MainPID", "--value", s.Unit); err == nil {
			if pid := strings.TrimSpace(string(out)); pid != "" && pid != "0" {
				// The supervising process's fd count, not the workload's: there is no cgroup fd
				// counter, and walking every pid in the cgroup each cycle is a lot of exec for a
				// signal that mostly catches conmon/podman leaks. An fd leak inside the container
				// is not visible here.
				if fds, err := x.ListDir("/proc/" + pid + "/fd"); err == nil {
					sr.FDs = len(fds)
				}
			}
		}
		// NRestarts is the crash-loop signal (no-silent-restarts): it climbs each time the
		// unit died and was restarted, even though a between-restarts is-active reads green.
		if out, err := x.Run(ctx, "systemctl", "show", "-p", "NRestarts", "--value", s.Unit); err == nil {
			sr.Restarts = parseUint(out)
		}
		r.Payloads = append(r.Payloads, sr)
	}
	if out, err := x.Run(ctx, "cat", "/proc/loadavg"); err == nil {
		r.Load1 = parseLoad1(out)
	}
	if req.DataDir != "" {
		if out, err := x.Run(ctx, "df", "-kP", req.DataDir); err == nil {
			r.VolumeUsedKB = parseDfUsedKB(out)
		}
		// -s: snapshots only (the pre-upgrade RO subvolumes an upgrade accumulates), so this
		// tracks snapshot growth, not the live subvolume set.
		if req.Soak {
			if out, err := x.Run(ctx, "btrfs", "subvolume", "list", "-s", req.DataDir); err == nil {
				r.SnapshotCount = countLines(out)
			}
		}
	}
	// Memory: four kernel files, each best-effort like the rest. No zram device (a guest built
	// without it) leaves the zram pair zero, which reads as "nothing parked" -- true either way.
	if out, err := x.Run(ctx, "cat", "/proc/meminfo"); err == nil {
		r.MemTotalKB, r.MemAvailableKB, r.AnonKB = parseMeminfo(out)
	}
	if out, err := x.Run(ctx, "cat", "/sys/block/zram0/mm_stat"); err == nil {
		r.ZramOrigKB, r.ZramUsedKB = parseZramMMStat(out)
	}
	if out, err := x.Run(ctx, "cat", "/proc/vmstat"); err == nil {
		r.PswpIn, r.PswpOut = parseVmstatSwap(out)
	}
	if out, err := x.Run(ctx, "cat", "/proc/pressure/memory"); err == nil {
		r.MemPSISome60, r.MemPSIFull60 = parsePSI60(out)
	}
	// -x: stay on one filesystem, so the number is the surface itself, not whatever is
	// mounted beneath it.
	if req.Soak {
		if out, err := x.Run(ctx, "du", "-skx", journalDir); err == nil {
			r.LogSizeKB = parseDuKB(out)
		}
		if out, err := x.Run(ctx, "du", "-skx", containerStore); err == nil {
			r.PodmanStoreKB = parseDuKB(out)
		}
	}
	// The guest kernel log (no-bad-kernel-log) carries the guest-internal DRBD/btrfs/OOM signals,
	// unreachable from the L1 container (which shares the host kernel). Only lines new since the
	// last poll (a journal cursor), so a past cycle's transient error ages out instead of
	// re-failing every sample and a fresh one is attributable to this cycle. Mechanism only: the
	// guest returns the lines, the oracle scans and scopes them.
	r.KernelErrors = recentKernelErrors(ctx, x)
	// `dmsetup table` prints one `<name>: <table>` line per dm device, exactly the seam
	// invariant's input: the data LV must still be one `linear` segment and nothing unexpected may
	// have appeared beside it. Best-effort like every other field: no dmsetup, or no dm device,
	// yields no lines.
	if out, err := x.Run(ctx, "dmsetup", "table"); err == nil {
		r.DMTargets = dmTableLines(out)
	}
	// The quota tree, where the volume is mounted: the mode the kernel states and every group
	// with its parents (qgroup.go). Mechanism only, like the dm tables; soak-only, because only
	// the oracle reads it. A node without the volume mounted reports neither.
	if req.Soak {
		if mode, err := quotaMode(ctx, x); err == nil {
			r.QuotaMode = mode
			if tree, err := readQgroups(ctx, x); err == nil {
				r.Qgroups = tree.rows()
			}
		}
	}
	return r
}

// recentKernelErrors returns the guest kernel warning+ lines added since the last call,
// advancing a stored journal cursor. The first call (no cursor) returns a bounded recent tail.
// Best-effort: any failure yields no lines (never fatal to the observe loop).
func recentKernelErrors(ctx context.Context, x Executor) []string {
	base := []string{"-k", "-p", "warning", "--no-pager", "--show-cursor"}
	args := append(base, "-n", "100") // first poll: a bounded recent tail
	if cur, err := x.Run(ctx, "cat", kmsgCursorPath); err == nil {
		if c := strings.TrimSpace(string(cur)); c != "" {
			args = append(base, "--after-cursor", c)
		}
	}
	out, err := x.Run(ctx, "journalctl", args...)
	if err != nil {
		return nil
	}
	lines, cursor := splitJournalCursor(out)
	if cursor != "" {
		_ = x.WriteFile(kmsgCursorPath, []byte(cursor+"\n"))
	}
	return lines
}

// splitJournalCursor separates journalctl --show-cursor output into the log lines and the
// trailing "-- cursor: <c>" marker (absent when no entries were shown, e.g. nothing new), and
// drops journalctl's own voice ("-- No entries --", "-- Reboot --", "-- Journal begins at ... --"),
// none of which are log lines. Kept, "-- No entries --" would count as one kernel error on every
// healthy poll: a constant 1 in the signal the soak oracle scans, hiding the step up to a real
// error. A journal line is dated ("Aug 06 ... guest kernel: ..."); nothing real is bracketed in
// "-- ... --".
func splitJournalCursor(out []byte) (lines []string, cursor string) {
	for _, raw := range strings.Split(string(out), "\n") {
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" {
			continue
		}
		if rest, ok := strings.CutPrefix(trimmed, "-- cursor:"); ok {
			cursor = strings.TrimSpace(rest)
			continue
		}
		if strings.HasPrefix(trimmed, "-- ") && strings.HasSuffix(trimmed, " --") {
			continue // journalctl's own marker, not an entry
		}
		lines = append(lines, raw)
	}
	return
}

// parseUint reads a non-negative integer (e.g. systemd's NRestarts value). 0 on empty/junk.
func parseUint(b []byte) uint64 {
	n, _ := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
	return n
}

// healthBudget bounds every answer to "is this app working". The host asks it inside a status
// cycle whose calls share one five-second deadline, and the host's request carries no deadline
// of its own, so the guest keeps the answer well inside it: a slow app must come back as a
// verdict (unhealthy, or unknown from an override), never as silence. Silence is what the host
// counts toward declaring the whole guest dead.
const healthBudget = 3 * time.Second

// probeHTTPOK does a minimal plaintext HTTP/1.0 GET of rawURL (raw net.Dial + a hand-written
// request) and reports whether the response status is 200. Plaintext is all the local /healthz
// needs. Any parse/dial/read error, non-http scheme, or non-200 status is "not healthy"
// (fail-closed). It gives up after healthBudget, or at the caller's deadline if that is sooner,
// so an app slow to answer reads "not healthy" too.
func probeHTTPOK(ctx context.Context, rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "http" || u.Host == "" {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, healthBudget)
	defer cancel()
	addr := u.Host
	if u.Port() == "" {
		addr = net.JoinHostPort(u.Hostname(), "80")
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return false
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	if _, err := fmt.Fprintf(conn, "GET %s HTTP/1.0\r\nHost: %s\r\nConnection: close\r\n\r\n", u.RequestURI(), u.Host); err != nil {
		return false
	}
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		return false
	}
	// Status line: "HTTP/1.0 200 OK" — match the 200 code token, not a substring elsewhere.
	return strings.HasPrefix(line, "HTTP/") && strings.Contains(line, " 200 ")
}

// cgroupAnonKB is an app's resident memory, in KB: deliberately the anon bytes of the unit's
// cgroup. The cgroup, not the main process, because a containerised app's workload is not the
// unit's MainPID (podman's supervisor), and cgroup v2 accounts hierarchically, so the unit's own
// file sums its container children. Anon, not memory.current, because memory.current includes the
// page cache: an app writing a database would show steadily climbing "memory" that is really cache
// the kernel will drop under pressure, and the trend oracle checks this series' slope, so cache
// growth would read as a leak and fail a soak run for a healthy node.
//
// 0 on anything unreadable (no cgroup, cgroup v1, accounting off), which the host already treats
// as "unread this cycle"; never a fallback to a different quantity, because a series that
// silently changes meaning is worse than a gap in it.
func cgroupAnonKB(ctx context.Context, x Executor, unit string) int64 {
	out, err := x.Run(ctx, "systemctl", "show", "-p", "ControlGroup", "--value", unit)
	if err != nil {
		return 0
	}
	cg := strings.TrimSpace(string(out))
	if cg == "" || cg == "/" { // not running, or no cgroup of its own
		return 0
	}
	st, err := x.Run(ctx, "cat", "/sys/fs/cgroup"+cg+"/memory.stat")
	if err != nil {
		return 0
	}
	return parseCgroupAnonKB(st)
}

// parseCgroupAnonKB pulls "anon <bytes>" out of a cgroup v2 memory.stat and returns KB. 0 if the
// field is absent or unparseable -- including a v1 memory.stat, whose fields differ.
func parseCgroupAnonKB(stat []byte) int64 {
	for _, line := range strings.Split(string(stat), "\n") {
		f := strings.Fields(line)
		if len(f) != 2 || f[0] != "anon" {
			continue
		}
		n, err := strconv.ParseInt(f[1], 10, 64)
		if err != nil || n < 0 {
			return 0
		}
		return n / 1024
	}
	return 0
}

// parseLoad1 reads the 1-minute load average (the first field of /proc/loadavg). 0 on junk.
func parseLoad1(loadavg []byte) float64 {
	if f := strings.Fields(string(loadavg)); len(f) > 0 {
		n, _ := strconv.ParseFloat(f[0], 64)
		return n
	}
	return 0
}

// parseDfUsedKB takes the Used column (index 2) from `df -kP` output: a header line then a
// single POSIX-guaranteed-unwrapped data line. 0 if the shape is unexpected.
func parseDfUsedKB(df []byte) int64 {
	lines := nonEmptyLines(df)
	if len(lines) < 2 {
		return 0
	}
	if f := strings.Fields(lines[len(lines)-1]); len(f) >= 3 {
		n, _ := strconv.ParseInt(f[2], 10, 64)
		return n
	}
	return 0
}

// parseMeminfo takes MemTotal, MemAvailable and AnonPages (all kB) from /proc/meminfo. A missing
// line leaves its value 0.
func parseMeminfo(b []byte) (total, avail, anon int64) {
	for _, l := range nonEmptyLines(b) {
		f := strings.Fields(l)
		if len(f) < 2 {
			continue
		}
		n, _ := strconv.ParseInt(f[1], 10, 64)
		switch f[0] {
		case "MemTotal:":
			total = n
		case "MemAvailable:":
			avail = n
		case "AnonPages:":
			anon = n
		}
	}
	return total, avail, anon
}

// parseZramMMStat takes orig_data_size and mem_used_total (fields 0 and 2, in bytes) from a zram
// device's mm_stat and returns them in kB. 0, 0 if the shape is unexpected.
func parseZramMMStat(b []byte) (origKB, usedKB int64) {
	f := strings.Fields(string(b))
	if len(f) < 3 {
		return 0, 0
	}
	o, _ := strconv.ParseInt(f[0], 10, 64)
	u, _ := strconv.ParseInt(f[2], 10, 64)
	return o / 1024, u / 1024
}

// parseVmstatSwap takes the cumulative pswpin/pswpout page counters from /proc/vmstat.
func parseVmstatSwap(b []byte) (in, out uint64) {
	for _, l := range nonEmptyLines(b) {
		f := strings.Fields(l)
		if len(f) != 2 {
			continue
		}
		switch f[0] {
		case "pswpin":
			in = parseUint([]byte(f[1]))
		case "pswpout":
			out = parseUint([]byte(f[1]))
		}
	}
	return in, out
}

// parsePSI60 takes avg60 from the `some` and `full` lines of /proc/pressure/memory.
func parsePSI60(b []byte) (some, full float64) {
	for _, l := range nonEmptyLines(b) {
		f := strings.Fields(l)
		if len(f) < 3 || !strings.HasPrefix(f[2], "avg60=") {
			continue
		}
		v, _ := strconv.ParseFloat(strings.TrimPrefix(f[2], "avg60="), 64)
		switch f[0] {
		case "some":
			some = v
		case "full":
			full = v
		}
	}
	return some, full
}

// parseDuKB takes the size (first field, KB under `du -sk`) from du's summary line. 0 on junk.
func parseDuKB(du []byte) int64 {
	if f := strings.Fields(string(du)); len(f) > 0 {
		n, _ := strconv.ParseInt(f[0], 10, 64)
		return n
	}
	return 0
}

func nonEmptyLines(b []byte) []string {
	var out []string
	for _, l := range strings.Split(string(b), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

func countLines(b []byte) int { return len(nonEmptyLines(b)) }

// dmTableLines turns `dmsetup table` output into one entry per dm device. With nothing to report
// dmsetup prints "No devices found" and exits 0, so that line is dropped rather than reported as a
// device (the trap splitJournalCursor exists for): a witness, or any node whose data disk carries
// no seam, reports nothing at all rather than one phantom target.
func dmTableLines(b []byte) []string {
	var out []string
	for _, l := range nonEmptyLines(b) {
		if !strings.Contains(l, ":") {
			continue // "No devices found", and anything else dmsetup says about itself
		}
		out = append(out, l)
	}
	return out
}

func resourceReq(payload json.RawMessage) (resourceRequest, error) {
	var req resourceRequest
	err := json.Unmarshal(payload, &req)
	return req, err
}

func unitReq(payload json.RawMessage) (unitRequest, error) {
	var req unitRequest
	err := json.Unmarshal(payload, &req)
	return req, err
}

func snapshotReq(payload json.RawMessage) (snapshotRequest, error) {
	var req snapshotRequest
	err := json.Unmarshal(payload, &req)
	return req, err
}

// Client is the host end: typed calls to the guest agent over the channel.
type Client struct {
	c      *guestfirmware.Conn
	caps   map[string]bool // verbs the guest advertised (nil until Handshake)
	bootID string          // which boot of the guest answered (empty until Handshake, or from a guest that does not say)
	bundle string          // the guest bundle the guest runs ("" = the image's firmware), from the handshake
}

// NewClient wraps a connection to the guest (virtio-serial in prod, net.Pipe in tests).
func NewClient(rw io.ReadWriteCloser) *Client { return &Client{c: guestfirmware.NewConn(rw)} }

// Handshake opens the channel, recording the verb set the guest advertises, the boot answering and
// the bundle it runs. It refuses nothing: a guest is driven by what it says it serves, one path at
// a time (Supports), because a handshake could refuse only the whole channel, and the channel is
// what fixes a node (see the note at guestfirmware.VerbHello). Call it once right after connecting
// and after any reconnect: dressing restarts the guest agent, changing the far side's verb set
// underneath the host.
func (g *Client) Handshake(ctx context.Context) (guestfirmware.Hello, error) {
	var h guestfirmware.Hello
	// On a reconnect a stale reply from the dropped session can sit ahead of ours in the
	// still-open virtio-serial stream; the Conn drops a reply nothing waits on, so it never fails.
	if err := g.c.Call(ctx, guestfirmware.VerbHello, nil, &h); err != nil {
		return h, fmt.Errorf("guestagent: handshake: %w", err)
	}
	g.bootID = h.BootID
	g.bundle = h.Bundle
	g.caps = make(map[string]bool, len(h.Capabilities))
	for _, c := range h.Capabilities {
		g.caps[c] = true
	}
	return h, nil
}

// Supports reports whether the guest advertised verb in its handshake. It is the only negotiation: a
// caller whose path needs a verb an older guest cannot serve asks here and does that one thing
// differently, leaving every other path working. Before a handshake (caps nil) it optimistically
// returns true, preserving "just try the verb" for callers that don't negotiate.
func (g *Client) Supports(verb string) bool {
	if g.caps == nil {
		return true
	}
	return g.caps[verb]
}

// BootID identifies the guest boot this channel reached, from the handshake. Empty before a
// handshake, and empty from a guest too old to report one -- so a caller comparing two of them
// must treat an empty side as no evidence rather than as a difference.
func (g *Client) BootID() string { return g.bootID }

// SetHostname renames the guest to this node's name so DRBD's `on <name>` matching
// works (see verbSetHostname). Idempotent.
func (g *Client) SetHostname(ctx context.Context, name string) error {
	return g.c.Call(ctx, verbSetHostname, hostnameRequest{Name: name}, nil)
}

// ConfigureNet sets a static CIDR on a guest NIC (the system/DRBD NIC) and brings
// it up, so DRBD can use the private subnet. VIPDev records the NIC the promoter claims the VIP
// on ("" = this node claims none, a witness); VIPAddr the address itself, in CIDR form ("" = DHCP
// decides). Nothing is baked behind either: the LAN decides them. Idempotent.
func (g *Client) ConfigureNet(ctx context.Context, n NetConfig) error {
	return g.c.Call(ctx, verbNetConfigure, netConfigureRequest{
		Dev: n.Dev, CIDR: n.CIDR, VIPDev: n.VIPDev, VIPAddr: n.VIPAddr, VIPLease: n.VIPLease,
		PrivDev: n.PrivDev, PrivCIDR: n.PrivCIDR,
		PrivHostIP: n.PrivHostIP, PrivHostMAC: n.PrivHostMAC,
		PodSubnet: n.PodSubnet,
		VIPParent: n.VIPParent, VIPMAC: n.VIPMAC, VIPRouter: n.VIPRouter,
	}, nil)
}

// SetMDNSName records the flock's visible name and republishes it if this node is serving.
// name is bare ("brave-elf"); the reverse proxy builds `briard-<name>.local` from it. "" publishes
// nothing. Idempotent, and deliberately independent of ConfigureNet -- a rename must never be an
// addressing call.
func (g *Client) SetMDNSName(ctx context.Context, name string) error {
	return g.c.Call(ctx, verbNetMDNSName, mdnsNameRequest{Name: name}, nil)
}

// MDNSPublished reports the flock name the reverse proxy's mDNS responder is actually answering
// for, bare (no `briard-` prefix, no `.local`), or "" when this node publishes none (a Secondary,
// or a publish still in flight). Read it rather than assume it: the requested name is present and
// plausible, and not necessarily the one being served. See verbNetMDNSPublished.
func (g *Client) MDNSPublished(ctx context.Context) (string, error) {
	var name string
	if err := g.c.Call(ctx, verbNetMDNSPublished, struct{}{}, &name); err != nil {
		return "", err
	}
	return name, nil
}

// MDNSOther reports the address of another briard the reverse proxy heard answering the bare
// `briard.local` when this node came up, or "" when none was heard. See verbNetMDNSOther.
func (g *Client) MDNSOther(ctx context.Context) (string, error) {
	var addr string
	if err := g.c.Call(ctx, verbNetMDNSOther, struct{}{}, &addr); err != nil {
		return "", err
	}
	return addr, nil
}

// VIP reports the address dev actually holds, in CIDR form, or "" when it holds none -- this node
// is Secondary, or its promotion has not claimed the address yet. Under DHCP this is how the host
// learns an address it does not choose. An unaddressed device is "" rather than an error,
// because the host asks every cycle and a Secondary answering "" is the normal case, not a fault.
func (g *Client) VIP(ctx context.Context, dev string) (string, error) {
	var cidr string
	if err := g.c.Call(ctx, verbNetVIP, netVIPRequest{Dev: dev}, &cidr); err != nil {
		return "", err
	}
	return cidr, nil
}

// NodeStorage hands the guest this node's storage spec and runs briard-node-storage.service:
// every tier built or opened, the `.res` landed, metadata created if there is none, and the
// resource attached. It never promotes. One call, one process: whether the volume is brand new
// cannot be inferred from outside (a blank dm-crypt device reads as ciphertext, not blankness), so
// the unit that runs `lvcreate` runs `create-md` too, and knows.
func (g *Client) NodeStorage(ctx context.Context, spec nodestorage.Spec) error {
	if err := spec.Validate(); err != nil {
		return err
	}
	return g.c.Call(ctx, verbNodeStorage, spec, nil)
}

// Adjust rewrites the resource config and runs `drbdadm adjust` -- apply a peer-set change to the
// already-running resource without a restart. Called on the serving primary to add a joining
// anchor/witness (the joiners come up via BringUp, FreshInit=false, and resync), and on a
// surviving anchor whose flock shrank but is still a flock. No create-md here, so the local disk
// is never touched.
func (g *Client) Adjust(ctx context.Context, req ProvisionRequest) error {
	return g.c.Call(ctx, verbAdjust, req, nil)
}

// ReactorStart lands the promoter snippet and starts drbd-reactor, which then drives promotion
// (not us). The snippet travels with the start because the chain a node is promoted with and the
// moment it may be promoted belong one call apart, not one verb apart.
func (g *Client) ReactorStart(ctx context.Context, resource, snippet string) error {
	return g.c.Call(ctx, verbReactor, reactorStartRequest{Resource: resource, Snippet: snippet}, nil)
}

// ChainStart starts the lone node's chain target: what ReactorStart is on a node that runs no
// DRBD. The target and its members are the guest agent's own render (units.go, shared/chain), so
// there is no snippet to land.
func (g *Client) ChainStart(ctx context.Context) error {
	return g.c.Call(ctx, verbChainStart, struct{}{}, nil)
}

// Status reads the node's serving state into a QuorumState — the summary the node reports
// up (shared/api's closed allowlist). Ask qs.Serving() for "is this node serving"; on a
// flock the guest fills it from DRBD.
func (g *Client) Status(ctx context.Context, resource string) (model.QuorumState, error) {
	c, err := g.Cluster(ctx, resource)
	return c.QuorumState, err
}

// Cluster reads the same sample whole: this node's QuorumState plus its peers'
// roles and disk states, the input to "could someone else take over?".
// Stays home — the peer half never rides the cloud wire.
func (g *Client) Cluster(ctx context.Context, resource string) (model.Cluster, error) {
	var c model.Cluster
	err := g.c.Call(ctx, verbStatus, resourceRequest{Resource: resource}, &c)
	return c, err
}

// AppStart starts the app's systemd unit inside the guest.
func (g *Client) AppStart(ctx context.Context, unit string) error {
	return g.c.Call(ctx, verbAppStart, unitRequest{Unit: unit}, nil)
}

// AppRender writes the host-rendered quadlet source into the guest's /run and reloads
// systemd, so the generated units exist. Node-local (renderApp). stale names the prior render's
// files this one drops, removed first.
func (g *Client) AppRender(ctx context.Context, files map[string]string, stale []string) error {
	return g.c.Call(ctx, verbAppRender, appRenderRequest{Files: files, Stale: stale}, nil)
}

// AppProvision creates the app's data subvolume + per-container subdirectories and
// records the manifest as its identity. Primary only: it all lives on the replicated volume.
// Idempotent: an existing subvolume is reused, never re-created, so a re-install keeps the data.
func (g *Client) AppProvision(ctx context.Context, name, dataDir string, subdirs []string, manifest string) error {
	req := appProvisionRequest{Name: name, DataDir: dataDir, Subdirs: subdirs, Manifest: manifest}
	return g.c.Call(ctx, verbAppProvision, req, nil)
}

// AppWarm ensures `ref` is present in the guest's image store, starting `unit` (its .image
// warm unit) only if it is missing, which is the whole point: starting the unit is a registry
// pull, so an unconditional warm makes every path that calls it require WAN.
func (g *Client) AppWarm(ctx context.Context, unit, ref string) error {
	return g.c.Call(ctx, verbAppWarm, appWarmRequest{Unit: unit, Ref: ref}, nil)
}

// HassReadiness samples Home Assistant's per-config-entry setup states from inside the guest, the
// differential S1 gate's whole input. An empty list is a legitimate answer, not a failure: an app
// that has answered its health URL but not finished setting its integrations up has none yet, and
// the gate handles that by construction, judging only entries loaded before the change. Named for
// the app, like the verb: it works for exactly one, so it takes no app name it would only ignore.
func (g *Client) HassReadiness(ctx context.Context, port int) ([]hass.Entry, error) {
	var out []hass.Entry
	err := g.c.Call(ctx, verbHassReadiness, hassReadinessRequest{Port: port}, &out)
	return out, err
}

// HassDBCheck starts the nightly recorder check in the guest's background and answers whether it
// did; false means a check or a restore is already running there.
func (g *Client) HassDBCheck(ctx context.Context) (bool, error) {
	var started bool
	err := g.c.Call(ctx, verbHassDBCheck, nil, &started)
	return started, err
}

// HassDBCheckResult answers whether a check is running, with a finished one's report, once.
func (g *Client) HassDBCheckResult(ctx context.Context) (hass.DBCheckState, error) {
	var s hass.DBCheckState
	err := g.c.Call(ctx, verbHassDBCheckResult, nil, &s)
	return s, err
}

// DataBackup starts a backup in the guest's background into the REST repository at url, under
// the household's group, with the host's own facts beside the volume's, and answers whether it
// did; false means one is running or its report is uncollected.
func (g *Client) DataBackup(ctx context.Context, url, group string, host map[string][]byte) (bool, error) {
	var started bool
	err := g.c.Call(ctx, verbDataBackup, backupRequest{Repository: url, Group: group, Host: host}, &started)
	return started, err
}

// DataBackupRestore puts one app's copy from one night of the backup (a restic snapshot id,
// or "latest") into the ring as a member, and answers the member's path.
func (g *Client) DataBackupRestore(ctx context.Context, url, snapshot, app string) (string, error) {
	var member string
	err := g.c.Call(ctx, verbDataBackupRestore, backupRestoreRequest{Repository: url, Snapshot: snapshot, App: app}, &member)
	return member, err
}

// DataBackupAdopt adds the volume's key to the repository at url, opened once with password.
func (g *Client) DataBackupAdopt(ctx context.Context, url, password string) error {
	return g.c.Call(ctx, verbDataBackupAdopt, backupAdoptRequest{Repository: url, Password: password}, nil)
}

// SupportsBackupAdopt says whether this guest can adopt a repository.
func (g *Client) SupportsBackupAdopt() bool { return g.Supports(verbDataBackupAdopt) }

// DataBackupList answers every backup the repository at url holds, newest first, with the apps in
// each; group is this household's, so the list says which backups are its own.
func (g *Client) DataBackupList(ctx context.Context, url, group string) ([]dashboard.BackupListing, error) {
	var list []dashboard.BackupListing
	err := g.c.Call(ctx, verbDataBackupList, backupListRequest{Repository: url, Group: group}, &list)
	return list, err
}

// SupportsBackupList says whether this guest can list the backup.
func (g *Client) SupportsBackupList() bool { return g.Supports(verbDataBackupList) }

// SupportsBackupRestore says whether this guest can materialise a backup's night as a member.
func (g *Client) SupportsBackupRestore() bool { return g.Supports(verbDataBackupRestore) }

// DataBackupKey reads the household's repository key off the volume, minting it the first time,
// and with saved records that the household has a copy of it elsewhere.
func (g *Client) DataBackupKey(ctx context.Context, saved bool) (BackupKey, error) {
	var k BackupKey
	err := g.c.Call(ctx, verbDataBackupKey, backupKeyRequest{Saved: saved}, &k)
	return k, err
}

// DataBackupResult answers whether a backup is running, with a finished one's report, once.
func (g *Client) DataBackupResult(ctx context.Context) (BackupState, error) {
	var s BackupState
	err := g.c.Call(ctx, verbDataBackupResult, nil, &s)
	return s, err
}

// FilesTick starts the files tier's tick in the guest's background and answers whether it did:
// false while one runs, or while a finished one's report waits to be collected.
func (g *Client) FilesTick(ctx context.Context) (bool, error) {
	var started bool
	err := g.c.Call(ctx, verbFilesTick, nil, &started)
	return started, err
}

// FilesTickResult answers whether a tick is running, with a finished one's report, once.
func (g *Client) FilesTickResult(ctx context.Context) (FilesTickState, error) {
	var s FilesTickState
	err := g.c.Call(ctx, verbFilesTickResult, nil, &s)
	return s, err
}

// SupportsFilesTick reports whether the guest runs the files tier's tick.
func (g *Client) SupportsFilesTick() bool { return g.Supports(verbFilesTick) }

// HassDBRestore puts the recorder database back from the member a report named.
func (g *Client) HassDBRestore(ctx context.Context, member string) error {
	return g.c.Call(ctx, verbHassDBRestore, dbRestoreRequest{Member: member}, nil)
}

// dbRestoreRequest names the member to restore from; the guest reads it back from the ring.
type dbRestoreRequest struct {
	Member string `json:"member"`
}

// HassNudge tells a running Home Assistant to reconsider what briard has offered it, and answers
// whether there was a Home Assistant on this volume to tell. That is a fact, not a verdict: `false`
// on a broker-only node is the ordinary case, because the caller fires this after every install
// and uninstall without first working out which apps the node runs, and deciding that here, from
// the volume, is cheaper than a host-side check that could be stale. An error means Home Assistant
// is installed and would not take the event. Best-effort by contract: the caller logs it and
// carries on, because the fallback is Home Assistant picking the change up at its next start.
func (g *Client) HassNudge(ctx context.Context) (bool, error) {
	var told bool
	err := g.c.Call(ctx, verbHassNudge, nil, &told)
	return told, err
}

// DashboardHandoff hands the guest the one-time code the household dashboard admits a browser on,
// with what the host knows about the OS account. The guest writes it 0600 on tmpfs;
// the dashboard consumes it once.
func (g *Client) DashboardHandoff(ctx context.Context, h dashboard.Handoff) error {
	return g.c.Call(ctx, verbDashboardHandoff, h, nil)
}

// DashboardCasa tells the guest what the host knows about the household's casa name, for the
// dashboard to render. Pushed on every change and at bring-up.
func (g *Client) DashboardCasa(ctx context.Context, c dashboard.Casa) error {
	return g.c.Call(ctx, verbDashboardCasa, c, nil)
}

// SupportsDashboardCasa reports whether this guest takes the casa view (an older image does not;
// the name still works there, the page just cannot show it).
func (g *Client) SupportsDashboardCasa() bool { return g.Supports(verbDashboardCasa) }

// DashboardBackup tells the guest what the host knows about the nightly backup, for the page.
func (g *Client) DashboardBackup(ctx context.Context, b dashboard.Backup) error {
	return g.c.Call(ctx, verbDashboardBackup, b, nil)
}

// DashboardCatalog tells the guest what the host knows of the catalog's index, for the page's
// cards: every listed app, which are installable, which the household wished for.
func (g *Client) DashboardCatalog(ctx context.Context, c dashboard.Catalog) error {
	return g.c.Call(ctx, verbDashboardCatalog, c, nil)
}

// SupportsDashboardCatalog reports whether this guest takes the catalog view (an older one does
// not; its page shows what is installed and nothing it could add).
func (g *Client) SupportsDashboardCatalog() bool { return g.Supports(verbDashboardCatalog) }

// DashboardAlerts hands the guest a copy of the host's alert store, for the dashboard to list.
func (g *Client) DashboardAlerts(ctx context.Context, recs []notify.Record) error {
	return g.c.Call(ctx, verbDashboardAlerts, recs, nil)
}

// SupportsDashboardAlerts reports whether this guest takes the alert copy (an older one does
// not; its page simply has no alert list).
func (g *Client) SupportsDashboardAlerts() bool { return g.Supports(verbDashboardAlerts) }

// MosquittoProbe stores `token` in the broker's own retained state (an empty token makes it a pure
// read) and returns what it holds: the S1 signal for an app whose work is invisible to a sample. A
// not-serving sample is an answer, not an error: it says no client can use this broker, exactly
// what the gate is asked to notice. Errors here are the probe itself failing.
func (g *Client) MosquittoProbe(ctx context.Context, token string) (mosquitto.Sample, error) {
	var out mosquitto.Sample
	err := g.c.Call(ctx, verbMosquittoProbe, mosquittoProbeRequest{Token: token}, &out)
	return out, err
}

// AppConverge makes the guest match the volume, in place: every manifest under the replicated
// `.apps/` rendered, warmed and started (converge.go). Primary only in effect: the volume is
// mounted nowhere else, and a Secondary converges when it promotes, which is the whole design. The
// volume is the only input, and the install path's job is to have written it first. live names the
// install the caller is running now ("" for none): its staged manifest is rendered for that app,
// and every other staged install is held back (staging.go). SupportsAppConverge gates it: an older
// guest has no briard-apps unit to converge, so a host must refuse rather than call a verb that
// guest cannot honour.
func (g *Client) AppConverge(ctx context.Context, live string) ([]string, error) {
	var skipped []string
	// A guest built before converge reported anything answers with no value at all, which decodes
	// to an empty list -- correctly, since such a guest also has nothing that can be skipped.
	err := g.c.Call(ctx, verbAppConverge, appConvergeRequest{Live: live}, &skipped)
	return skipped, err
}

// SupportsAppConverge reports whether the guest can converge itself to the volume.
func (g *Client) SupportsAppConverge() bool { return g.Supports(verbAppConverge) }

// storageRoot is podman's image store in the guest -- the filesystem a pull fills and the
// free-space gate measures. Rootful podman's default; the guest image sets no other.
const storageRoot = "/var/lib/containers/storage"

// appPullingRequest is app.pulling's argument: record a pull (its sizes), or with Done
// clear the record.
type appPullingRequest struct {
	App           string `json:"app"`
	Size          int64  `json:"size,omitempty"`
	InstalledSize int64  `json:"installedSize,omitempty"`
	Done          bool   `json:"done,omitempty"`
}

// storageFreeReply is storage.free's answer.
type storageFreeReply struct {
	Free  int64  `json:"free"`
	Total int64  `json:"total"`
	Path  string `json:"path"`
}

type storageGrowRequest struct {
	Size int64 `json:"size"`
}

// stateDiskDev is the state disk as the guest finds it: by the serial the host attaches it with.
const stateDiskDev = "/dev/disk/by-id/virtio-briard-state"

// stateGrowWait bounds how long the kernel may take to report the size the host resized the disk
// to -- a virtio capacity change is an interrupt away, so this is generous.
const stateGrowWait = 10 * time.Second

// growStateDisk waits until the state disk is at least size bytes as the kernel sees it, then grows
// its ext4 filesystem to fill it. Online: ext4 grows while mounted, and the disk is in use the
// whole time. Waiting on the size rather than sleeping is what makes the order certain -- a
// resize2fs that ran before the kernel saw the new capacity would find nothing to do and say so.
func growStateDisk(ctx context.Context, x Executor, size int64) error {
	if size <= 0 {
		return fmt.Errorf("%s: need a size", verbStorageGrow)
	}
	deadline := time.Now().Add(stateGrowWait)
	for {
		out, err := x.Run(ctx, "blockdev", "--getsize64", stateDiskDev)
		if err != nil {
			return fmt.Errorf("%s: %w: %s", verbStorageGrow, err, strings.TrimSpace(string(out)))
		}
		have, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
		if err != nil {
			return fmt.Errorf("%s: blockdev said %q", verbStorageGrow, strings.TrimSpace(string(out)))
		}
		if have >= size {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s: the state disk is still %d bytes, not %d, after %s", verbStorageGrow, have, size, stateGrowWait)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
	if out, err := x.Run(ctx, "resize2fs", stateDiskDev); err != nil {
		return fmt.Errorf("%s: resize2fs: %w: %s", verbStorageGrow, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// AppPulling records an app install's pull for the dashboard -- the manifest's sizes,
// written before the first byte moves.
func (g *Client) AppPulling(ctx context.Context, app string, size, installed int64) error {
	return g.c.Call(ctx, verbAppPulling, appPullingRequest{App: app, Size: size, InstalledSize: installed}, nil)
}

// AppPulled clears that record: the image is present (or the pull is over either way).
func (g *Client) AppPulled(ctx context.Context, app string) error {
	return g.c.Call(ctx, verbAppPulling, appPullingRequest{App: app, Done: true}, nil)
}

// SupportsAppPulling reports whether the guest keeps the pull record; an older guest shows
// the household an install with no bar, which is not a reason to refuse it.
func (g *Client) SupportsAppPulling() bool { return g.Supports(verbAppPulling) }

// StorageFree reports the image store's filesystem: free and total bytes.
func (g *Client) StorageFree(ctx context.Context) (free, total int64, err error) {
	var r storageFreeReply
	if err := g.c.Call(ctx, verbStorageFree, struct{}{}, &r); err != nil {
		return 0, 0, err
	}
	return r.Free, r.Total, nil
}

// SupportsStorageFree reports whether the guest can measure its image store.
func (g *Client) SupportsStorageFree() bool { return g.Supports(verbStorageFree) }

// deadmanEpisodeReply is deadman.episode's answer: Found false when nothing waited.
type deadmanEpisodeReply struct {
	Found   bool                `json:"found"`
	Episode deadman.LastEpisode `json:"episode"`
}

// DeadmanEpisode collects (and makes the guest forget) the last stretch it spent without the
// host agent. ok is false when there was none.
func (g *Client) DeadmanEpisode(ctx context.Context) (deadman.LastEpisode, bool, error) {
	var r deadmanEpisodeReply
	if err := g.c.Call(ctx, verbDeadmanEpisode, struct{}{}, &r); err != nil {
		return deadman.LastEpisode{}, false, err
	}
	return r.Episode, r.Found, nil
}

// SupportsDeadmanEpisode reports whether the guest hands over its deadman episodes.
func (g *Client) SupportsDeadmanEpisode() bool { return g.Supports(verbDeadmanEpisode) }

// StorageGrow has the guest fill its state disk, which the host has just grown to size bytes.
func (g *Client) StorageGrow(ctx context.Context, size int64) error {
	return g.c.Call(ctx, verbStorageGrow, storageGrowRequest{Size: size}, nil)
}

// SupportsStorageGrow reports whether the guest can fill a grown state disk.
func (g *Client) SupportsStorageGrow() bool { return g.Supports(verbStorageGrow) }

// AppInstalled reads the manifest recorded on the replicated volume for one app, or "" when that
// app is not installed there: a legitimate answer (the shipped zero-app node, and equally a node
// running other apps), never an error. SupportsAppInstalled is the gate the install path checks
// first: a guest that does not advertise this verb cannot tell one app's identity from another's,
// so it must be refused rather than driven into overwriting the manifest of whatever it already
// runs.
func (g *Client) AppInstalled(ctx context.Context, name string) (string, error) {
	var s string
	err := g.c.Call(ctx, verbAppInstalled, appInstalledRequest{Name: name}, &s)
	return s, err
}

// SupportsAppInstalled reports whether the guest can name an app when reading or recording
// its identity on the volume (one manifest per app).
func (g *Client) SupportsAppInstalled() bool { return g.Supports(verbAppInstalled) }

// AppList names the apps recorded on the replicated volume. Empty is a legitimate answer (the
// shipped zero-app node), never an error. It is how a node that never installed anything can still
// say what it runs: converge renders from the volume at promotion, so on a survivor the volume is
// the only place the truth exists.
func (g *Client) AppList(ctx context.Context) ([]string, error) {
	var names []string
	err := g.c.Call(ctx, verbAppList, nil, &names)
	return names, err
}

// SupportsAppList reports whether the guest can name what the volume carries. A guest without
// it leaves a converged survivor reporting only what this host installed itself -- which is
// nothing, on a node that promoted into an app someone else installed.
func (g *Client) SupportsAppList() bool { return g.Supports(verbAppList) }

// Resources reads the appliance's resource telemetry (per-app RSS/fds/restarts, load, the disk
// sub-series) for the soak's trend oracle. apps pairs each app's name with the unit whose footprint
// is its own, and dataDir is the DRBD volume: both empty on a witness and on a zero-app node, which
// still get every node-scoped series. soak adds the soak-only reads (resourcesRequest.Soak).
// Best-effort on the guest: a partial read is a struct with the unread fields zero, not an error.
func (g *Client) Resources(ctx context.Context, apps map[string]string, dataDir string, soak bool) (telemetry.NodeResources, error) {
	var r telemetry.NodeResources
	req := resourcesRequest{DataDir: dataDir, Soak: soak}
	// Sorted, so a run's forensics read in the same order every cycle -- map order would shuffle
	// the series between samples for no reason.
	names := make([]string, 0, len(apps))
	for name := range apps {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		req.Apps = append(req.Apps, resourceApp{Name: name, Unit: apps[name]})
	}
	err := g.c.Call(ctx, verbResources, req, &r)
	return r, err
}

// WriteCert lands a renewed cert/key on the DRBD volume, where the reverse proxy
// hot-reloads it -- so a renewal is applied with no restart. Replicated + synced, so a failover
// serves the same cert.
func (g *Client) WriteCert(ctx context.Context, cert, key string) error {
	return g.c.Call(ctx, verbCertWrite, certWriteRequest{Cert: cert, Key: key}, nil)
}

// ReadCert returns the serving cert chain as PEM, or "" when the volume holds none. Only the
// node that mounts the volume can answer; the key never crosses the channel.
func (g *Client) ReadCert(ctx context.Context) (string, error) {
	var pem string
	err := g.c.Call(ctx, verbCertRead, struct{}{}, &pem)
	return pem, err
}

// AppStop stops the app's unit -- the quiesce step before a snapshot.
func (g *Client) AppStop(ctx context.Context, unit string) error {
	return g.c.Call(ctx, verbAppStop, unitRequest{Unit: unit}, nil)
}

// AppActive reports whether the app's unit is active (the Running half of the
// health-gate; AppHealth is the Ready half).
func (g *Client) AppActive(ctx context.Context, unit string) (bool, error) {
	var active bool
	err := g.c.Call(ctx, verbAppActive, unitRequest{Unit: unit}, &active)
	return active, err
}

// AppHealth asks the guest to GET the app's health URL from inside itself and report 200
// (the Ready half of the health-gate). Probing in-guest, not host->VIP over the LAN, keeps the
// readiness check working under a substrate where the host can't reach the guest's VIP (macvtap).
// On an error (a guest that does not serve the verb) callers fall back to a host-side GET.
func (g *Client) AppHealth(ctx context.Context, url string) (bool, error) {
	var ok bool
	err := g.c.Call(ctx, verbAppHealth, healthRequest{URL: url}, &ok)
	return ok, err
}

// AppHealthOf asks the guest for one named app's health, letting the guest resolve the address
// from its own routing table. Never build `http://127.0.0.1:<port>` from the manifest instead: the
// manifest names a port and the renderer decides which host answers on it, so that URL is correct
// only for a pod sharing the guest's network namespace. Asking by name is what keeps the install
// gate and the steady-state probe pointed wherever the reverse proxy is pointed. An unresolvable
// app returns an error, never a verdict: "I cannot find it" must not be read as "it is broken" by
// a gate that reverts installs.
func (g *Client) AppHealthOf(ctx context.Context, app string) (apps.Health, error) {
	var h apps.Health
	err := g.c.Call(ctx, verbAppHealthOf, appRequest{App: app}, &h)
	return h, err
}

// Snapshot takes one ring member: a read-only btrfs snapshot of dataDir at dest (a subvolume on
// the same DRBD volume, so it replicates with it) with sidecar written beside it. The guest refuses
// a dest that already exists rather than replacing it, and removes the member if the sidecar
// cannot be written, so a member either exists with its metadata or does not exist.
//
// ⚠️ Gate it on SupportsSnapshotMember: an older guest does not advertise this, and taking a plain
// snapshot instead would silently leave an unlabelled member, the exact outcome the sidecar exists
// to prevent.
func (g *Client) Snapshot(ctx context.Context, dataDir, dest, sidecar string) error {
	return g.c.Call(ctx, verbDataMember, snapshotRequest{DataDir: dataDir, Path: dest, Sidecar: sidecar}, nil)
}

// SupportsSnapshotMember reports whether this guest can take a titled ring member.
func (g *Client) SupportsSnapshotMember() bool { return g.Supports(verbDataMember) }

// QuiescedSnapshot takes a member of an app that is running, asking the app to hold still across
// it, and answers whether the app actually did. The guest writes the member's class from that
// answer, because the host cannot see it from where it sits.
//
// ⚠️ The sidecar the host renders must say `crash`: the guest upgrades it when the app held and
// never the other way round, so everything that can go wrong in between leaves the member telling
// the weaker truth.
func (g *Client) QuiescedSnapshot(ctx context.Context, app, dataDir, dest, sidecar string) (bool, string, error) {
	var out struct {
		Held bool   `json:"held"`
		Why  string `json:"why"`
	}
	err := g.c.Call(ctx, verbDataMemberQuiesced,
		snapshotRequest{DataDir: dataDir, Path: dest, Sidecar: sidecar, App: app}, &out)
	return out.Held, out.Why, err
}

// SupportsQuiescedSnapshot reports whether this guest can ask an app to hold still.
func (g *Client) SupportsQuiescedSnapshot() bool { return g.Supports(verbDataMemberQuiesced) }

// Restore replaces the live dataDir subvolume with a fresh rw snapshot of src, minus the relative
// paths in sweep. The caller must have stopped the app first (bind released).
//
// ⚠️ It speaks data.replace, and a guest that does not advertise it must be refused rather than
// fallen back to data.restore: the fallback puts the data back and keeps the marker,
// which is a household's own backup restore replaying itself. SupportsRestoreSweep is the gate,
// and the one caller that may fall back anyway is the failed-upgrade revert, where the
// alternative is no rollback at all — it says so at its call site.
func (g *Client) Restore(ctx context.Context, dataDir, src string, sweep []string) error {
	return g.c.Call(ctx, verbDataReplace, snapshotRequest{DataDir: dataDir, Path: src, Sweep: sweep}, nil)
}

// RestoreWithoutSweep is the frozen old verb, for the one caller that must put data back even on
// a guest too old to sweep (agent/host's failed-upgrade revert). Nothing else may use it.
func (g *Client) RestoreWithoutSweep(ctx context.Context, dataDir, src string) error {
	return g.c.Call(ctx, verbDataRestore, snapshotRequest{DataDir: dataDir, Path: src}, nil)
}

// SupportsRestoreSweep reports whether this guest can materialise a member minus the markers a
// household's own restore leaves behind.
func (g *Client) SupportsRestoreSweep() bool { return g.Supports(verbDataReplace) }

// SystemPath reads the guest's current system closure store path (os.system) -- which image the
// guest is running. The host compares it with a release manifest's System to decide a guest
// image update and to confirm the image it booted is the one it asked for.
func (g *Client) SystemPath(ctx context.Context) (string, error) {
	var path string
	err := g.c.Call(ctx, verbOSSystem, nil, &path)
	return path, err
}

// SystemState reads the guest OS's overall systemd state (os.state).
func (g *Client) SystemState(ctx context.Context) (SystemState, error) {
	var st SystemState
	err := g.c.Call(ctx, verbOSState, nil, &st)
	return st, err
}

// PowerOff asks the guest OS to shut itself down cleanly and returns as soon as the request is
// accepted: the shutdown proceeds without us and the control channel dies with it, which is
// expected, not an error. Confirm completion by watching the VM stop (platform.Guest.WaitStopped),
// never by this call. Preferred over the ACPI power button (platform.Guest.Shutdown) whenever the
// agent is reachable: it asks the OS in its own terms instead of relying on something inside
// being subscribed to a virtual button. Keep both: this one needs a live agent, and the ACPI one
// is what remains when the agent is the thing that died.
func (g *Client) PowerOff(ctx context.Context) error {
	return g.c.Call(ctx, guestfirmware.VerbOSPowerOff, nil, nil)
}

// ReactorPause stops the drbd-reactor daemon (maintenance mode): the resource stays Primary and
// its apps keep running, but the promoter stops reacting -- so a planned app stop isn't mistaken
// for a failure. The guest does not read snippet.
func (g *Client) ReactorPause(ctx context.Context, snippet string) error {
	return g.c.Call(ctx, verbReactorPause, reactorRequest{Snippet: snippet}, nil)
}

// ReactorEvict hands this node's work to a peer: a planned handover, what an upgrade needs so a
// healthy node can reboot while its peer serves. keepMasked leaves the node ineligible to take it
// back (the reboot path); unmask releases that and evicts nothing. It reports only that the
// eviction ran: which peer took over is drbd-reactor's election, so a caller that cares must read
// the roles afterwards.
func (g *Client) ReactorEvict(ctx context.Context, keepMasked, unmask bool) error {
	return g.c.Call(ctx, verbReactorEvict, evictRequest{KeepMasked: keepMasked, Unmask: unmask}, nil)
}

// ReactorActive reports whether the promoter daemon is running — false meaning someone has it
// paused, i.e. a maintenance bracket is already open (a lone node, which runs none, answers
// true). The guard: a second bracket user refuses to start rather than corrupting the first
// one's bracket from the middle. Advisory by construction, since a pause can still land between
// this answer and the caller's own.
func (g *Client) ReactorActive(ctx context.Context) (bool, error) {
	var active bool
	err := g.c.Call(ctx, verbReactorActive, struct{}{}, &active)
	return active, err
}

// ForgetVIP deletes the flock's remembered DHCP address from the volume (net.vip.forget).
func (g *Client) ForgetVIP(ctx context.Context) error {
	return g.c.Call(ctx, verbNetVIPForget, struct{}{}, nil)
}

// FsSync flushes the replicated data volume's dirty pages — the pre-copy that bounds an
// eviction's unmount flush. Returns the guest's one-word account ("synced", or
// "skipped: ..." on a node with nothing mounted — a Secondary answering honestly, not failing).
func (g *Client) FsSync(ctx context.Context) (string, error) {
	var detail string
	err := g.c.Call(ctx, verbFsSync, struct{}{}, &detail)
	return detail, err
}

// ReactorResume starts the drbd-reactor daemon again (a no-op on a lone node); it re-runs the
// initial target start, which is a no-op on an already-Primary node with apps up. The guest does
// not read snippet.
func (g *Client) ReactorResume(ctx context.Context, snippet string) error {
	return g.c.Call(ctx, verbReactorResume, reactorRequest{Snippet: snippet}, nil)
}

// BringUpSpec is one DRBD resource to bring up on this node.
type BringUpSpec struct {
	// Storage is the node's whole block layout and the resource on top of it, rendered by the
	// host (agent/host's StorageSpec): the tiers to build, the `.res`, whether
	// this node is diskless, and whether it is the seed of a new flock.
	Storage  nodestorage.Spec
	Promoter []string // the promoter chain's units in start order (shared/chain); nil on a witness
	// AppUnits re-materialises the installed apps' quadlet files before the chain starts: they
	// live on tmpfs inside the guest, so a reboot erases them while the host's manifest cache
	// survives. Written here, before ReactorStart/ChainStart, so a node (a standby included) has
	// its apps' units in place before it promotes; briard-apps (converge) starts the apps from the
	// volume once the mount exists. Empty on a node with no installed app, the shipped state.
	AppUnits map[string]string
	// AppImages maps each .image warm unit to the image ref it obtains, for every installed app.
	// Nothing else starts these units (they carry no [Install]) and the containers are
	// Pull=never, so an image missing here is one converge must pull at promotion, and a pull
	// that cannot happen there fails converge and takes the VIP down. A map, not a unit list,
	// because bring-up must not pull: the generated .image unit is `Wants=network-online.target` +
	// `ExecStart=podman image pull <ref>` with no already-present short-circuit, so starting it
	// unconditionally re-pulls from a registry on every reboot, and a node without WAN fails
	// bring-up, breaking "installing needs network, running and failover never do". The ref is
	// what lets the guest answer "is it already here?" before pulling.
	AppImages map[string]string
}

// BringUp performs the agent-owned bring-up: the node's storage (NodeStorage: tiers, `.res`,
// create-md where needed, attach), the installed apps' units re-rendered and their images warmed,
// then the chain armed (drbd-reactor on a flock, the chain target on a lone node). Promotion is
// drbd-reactor's, done asynchronously once quorum forms; the caller observes convergence via
// Status. A witness (Diskless, no Promoter) builds its storage and comes up, but creates no
// metadata and runs no promoter.
//
// The order is the guarantee, not merely the sequence: nothing else starts drbd-reactor or the
// chain target (not boot, not a target), so every step above happens-before any promotion can, on
// a reboot exactly as on a first install. There is no cycle: quorum does not wait on the reactor
// (storage.node attaches DRBD itself) and the caller gates on quorate rather than Primary, so
// bring-up never waits for a promotion it is the precondition of.
func (g *Client) BringUp(ctx context.Context, spec BringUpSpec) error {
	res := spec.Storage.Resource.Name
	// Every block-level step in one call: the tiers, the `.res`, create-md, the attach and, on a
	// true first init only, new-current-uuid and the format. One unit holds both
	// "this run created the LVs" and "this node is the seed", so no host-side gate has to infer
	// freshness.
	if err := g.NodeStorage(ctx, spec.Storage); err != nil {
		return err
	}
	// Put the apps' units back and warm their images before the chain can start. A failure here
	// fails bring-up like every other step; the alternative is a node that promotes and then
	// fails converge, later and less legibly. (The soft-failure rule lives one level up, in
	// reading the cache at all: an unusable cache means "nothing installed", and this code never
	// runs.)
	if len(spec.AppUnits) > 0 {
		if err := g.AppRender(ctx, spec.AppUnits, nil); err != nil {
			return fmt.Errorf("re-render installed app units: %w", err)
		}
		// Sorted so the log reads the same on every boot; the images are independent, so the
		// order is presentation, not semantics.
		units := make([]string, 0, len(spec.AppImages))
		for u := range spec.AppImages {
			units = append(units, u)
		}
		sort.Strings(units)
		for _, u := range units {
			if err := g.AppWarm(ctx, u, spec.AppImages[u]); err != nil {
				return fmt.Errorf("warm image unit %s: %w", u, err)
			}
		}
	}
	if len(spec.Promoter) > 0 {
		// The same chain, a different trigger: a lone node's bring-up ends in its
		// static target where a flock's ends in drbd-reactor. The members do not know which
		// started them.
		if !spec.Storage.Resource.Replicated {
			return g.ChainStart(ctx)
		}
		return g.ReactorStart(ctx, res, drbd.ReactorConfig(res, spec.Promoter))
	}
	return nil
}

// SystemState is the guest OS's own answer to "did everything start?": systemd's overall state
// (`running`, `degraded`, `starting`, ...) and, when degraded, the units that failed -- named so
// the host's log and the rollback's reason say what broke, not just that something did.
type SystemState struct {
	State  string   `json:"state"`
	Failed []string `json:"failed,omitempty"`
}

// systemState serves `os.state`. The exit status of `is-system-running` is discarded on purpose:
// it is zero only for `running`, so every other answer -- the ones worth reporting -- would read
// as an error. Stdout is the answer (guestfirmware.systemStopping has the same trap).
func systemState(ctx context.Context, x Executor) SystemState {
	out, _ := x.Run(ctx, "systemctl", "is-system-running")
	st := SystemState{State: strings.TrimSpace(string(out))}
	if st.State != "degraded" {
		return st
	}
	out, _ = x.Run(ctx, "systemctl", "list-units", "--state=failed", "--no-legend", "--plain")
	for _, line := range strings.Split(string(out), "\n") {
		if f := strings.Fields(line); len(f) > 0 {
			st.Failed = append(st.Failed, f[0])
		}
	}
	return st
}

// loneCluster is the node status of a node that runs no DRBD: the second branch of the one
// predicate, and the only place it lives. Every field keeps the meaning it has on a flock:
// Primary is "holds the disk", which a lone node does by construction (nobody else could);
// Quorate is "allowed to write" (always: there is no peer to lose a majority to); ChainDown is
// its chain target, the reactor's as the flock branch reads it (a hold stops it, and so does a
// bring-up that has not started it yet); Diskful and UpToDate are what the node carries (its
// only copy, current by definition); and Connected and Peers are nobody, so a standby rule that
// asks PeerCanTakeOver() hears no, the truth about a lone node that is not serving.
func loneCluster(ctx context.Context, x Executor) model.Cluster {
	return model.Cluster{QuorumState: model.QuorumState{Primary: true, Quorate: true, ChainDown: !unitActive(ctx, x, chainTarget), Diskful: true, UpToDate: true}}
}

// unitActive asks systemd whether one unit is active. `is-active` exits non-zero for every other
// state, so the exit status and the word must both agree.
func unitActive(ctx context.Context, x Executor, unit string) bool {
	out, err := x.Run(ctx, "systemctl", "is-active", unit)
	return err == nil && strings.TrimSpace(string(out)) == "active"
}

// WaitPrimary polls Status until this node is serving -- i.e. bring-up has converged and the
// chain is up (on a flock, drbd-reactor promoted once quorum formed). interval is the poll cadence;
// the caller bounds the total wait via ctx. Transient Status errors (guest still
// coming up) are ignored until ctx expires.
func (g *Client) WaitPrimary(ctx context.Context, resource string, interval time.Duration) error {
	return g.waitStatus(ctx, resource, interval, model.QuorumState.Serving)
}

// WaitQuorate polls Status until this node is quorate: the convergence gate for a promoter-capable
// data node in a multi-node cluster, where drbd-reactor promotes exactly one node and the others
// settle Secondary, so gating those on WaitPrimary would hang the secondaries. Quorate means
// "participating in a healthy cluster" whichever node won the primary role (the VIP and the apps
// run on whoever did).
func (g *Client) WaitQuorate(ctx context.Context, resource string, interval time.Duration) error {
	return g.waitStatus(ctx, resource, interval, func(qs model.QuorumState) bool {
		return qs.Quorate
	})
}

// waitStatus polls Status until ready(qs) or ctx expires; transient Status errors
// (guest still coming up) are ignored until then.
func (g *Client) waitStatus(ctx context.Context, resource string, interval time.Duration, ready func(model.QuorumState) bool) error {
	for {
		if qs, err := g.Status(ctx, resource); err == nil && ready(qs) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}

// Close closes the underlying channel.
func (g *Client) Close() error { return g.c.Close() }

// BringUpGuest dials the guest's control socket (the virtio-serial chardev's host end), brings
// the resource up, and waits for the node to serve, then closes the connection. Only a test calls
// it: agent/host keeps one connection for the session and calls BringUp itself.
func BringUpGuest(ctx context.Context, sock string, spec BringUpSpec) error {
	conn, err := net.Dial("unix", sock)
	if err != nil {
		return fmt.Errorf("guestagent: dial %s: %w", sock, err)
	}
	g := NewClient(conn)
	defer g.Close()
	if err := g.BringUp(ctx, spec); err != nil {
		return err
	}
	return g.WaitPrimary(ctx, spec.Storage.Resource.Name, DefaultPollInterval)
}

// EnsureImage makes one image ref resident, pulling only if it is missing.
//
// ⚠️ It is not AppWarm, and the difference is who pays for a failure. A pull converge cannot do
// takes the VIP down and says so (the running-never-needs-network doctrine, upheld by failing); a
// pull a restore cannot do cancels the restore and leaves the household exactly where it was. This
// is the verb for the second kind, which is why it asks by ref and needs nothing rendered.
func (g *Client) EnsureImage(ctx context.Context, ref string) error {
	return g.c.Call(ctx, verbImageEnsure, appWarmRequest{Ref: ref}, nil)
}

// SupportsImageEnsure reports whether this guest can be asked for an image by ref alone.
func (g *Client) SupportsImageEnsure() bool { return g.Supports(verbImageEnsure) }

// RemoveImage deletes one image ref from the guest's store; an absent one is not an error. kept
// says the guest declined because its own OS image carries it (stagedImagesPath).
func (g *Client) RemoveImage(ctx context.Context, ref string) (kept bool, err error) {
	var r imageRemoveReply
	err = g.c.Call(ctx, verbImageRemove, appWarmRequest{Ref: ref}, &r)
	return r.Kept, err
}

// imageRemoveReply says whether image.remove kept a baked image rather than removing it.
type imageRemoveReply struct {
	Kept bool `json:"kept,omitempty"`
}

// stagedImagesPath lists, one per line, the IDs of the images briard-stage loaded from this OS
// image at boot (configuration.nix): the images image.remove keeps.
const stagedImagesPath = "/run/briard/staged-images"

// SupportsImageRemove reports whether this guest can delete an image by ref.
func (g *Client) SupportsImageRemove() bool { return g.Supports(verbImageRemove) }

// Members lists one app's ring, oldest first: every member with the sidecar beside it. A
// member whose sidecar cannot be read is omitted rather than offered.
func (g *Client) Members(ctx context.Context, app string) ([]quadlet.SnapshotEntry, error) {
	var out []quadlet.SnapshotEntry
	err := g.c.Call(ctx, verbDataMembers, appRequest{App: app}, &out)
	return out, err
}

// SupportsMembers reports whether this guest can list an app's ring.
func (g *Client) SupportsMembers() bool { return g.Supports(verbDataMembers) }

// Usage answers one app's share of the volume, starting a walk of its current data when the last
// one is stale.
func (g *Client) Usage(ctx context.Context, app string) (dashboard.AppUsage, error) {
	var u dashboard.AppUsage
	err := g.c.Call(ctx, verbStorageUsage, appRequest{App: app}, &u)
	return u, err
}

// SupportsUsage reports whether this guest can measure an app's share of the volume.
func (g *Client) SupportsUsage() bool { return g.Supports(verbStorageUsage) }

// EmptyCache empties one app's cache; the caller has stopped the app.
func (g *Client) EmptyCache(ctx context.Context, app string) error {
	return g.c.Call(ctx, verbCacheEmpty, appRequest{App: app}, nil)
}

// SupportsEmptyCache reports whether this guest can empty an app's cache.
func (g *Client) SupportsEmptyCache() bool { return g.Supports(verbCacheEmpty) }
