package guestagent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"briard.io/agent/drbd"
	"briard.io/agent/guestagent/deadman"
	"briard.io/shared/api"
	"briard.io/shared/backup"
	"briard.io/shared/model"
	"briard.io/shared/telemetry"
)

// ControlPort is the virtio-serial port name for the host<->guest channel;
// the guest device is ControlPortDev. The host launches QEMU with a virtserialport
// of this name; the guest agent opens ControlPortDev.
const (
	ControlPort    = "briard.control"
	ControlPortDev = "/dev/virtio-ports/" + ControlPort
)

// DefaultPollInterval is how often BringUpGuest polls for convergence.
const DefaultPollInterval = 500 * time.Millisecond

// Control verbs on the channel. The host owns bring-up: it renders
// the DRBD config and drives create-md / target / reactor through these; the
// guest just executes. None of them promote/demote -- drbd-reactor does that.
const (
	verbProvision = "drbd.provision"     // write configs + create-md
	verbUp        = "drbd.up"            // start drbd@<res>.target (attach+connect)
	verbInitUUID  = "drbd.init-uptodate" // new-current-uuid: declare a fresh resource UpToDate
	verbReactor   = "drbd.reactor.start" // start drbd-reactor (it then promotes) -- the ONLY thing that does
	verbStatus    = "drbd.status"        // drbdsetup status --json -> model.Cluster (QuorumState + peers)
	verbAdjust    = "drbd.adjust"        // rewrite the .res + `drbdadm adjust` (runtime mesh growth)
)

// Net.configure sets a static address on a guest NIC -- the system/DRBD NIC on the
// private subnet, so DRBD can bind/connect there. Host-driven: the agent knows
// the per-node address (it renders the same into the .res). Idempotent (addr replace),
// since bring-up may retry.
const verbNetConfigure = "net.configure"

// verbNetVIP reads the address the VIP device ACTUALLY holds, in CIDR form; "" when it holds none
// (a Secondary, or a promotion still in flight). It exists because under DHCP the host stops
// deciding the address: it is acquired inside the guest at promotion, so the host has to be told
// what it turned out to be -- to probe health at it, to report it, and to print it at a human.
//
// Ground truth is the INTERFACE, not whatever was last written to vip.env: an address that was
// recorded but failed to apply must not be reportable as live.
const verbNetVIP = "net.vip"

// verbNetMDNSName records the flock's human-visible name and republishes it: briard-mdns publishes
// `briard-<name>.local` pointing at the VIP.
//
// It is its OWN verb rather than another field on net.configure, and the reason is the design it
// belongs to (V3.20): the name is a LABEL and the address is an IDENTITY, so a rename must be
// possible without re-running network configuration. Folding it in would have made every rename an
// addressing call -- re-asserting VIP_DEV/VIP_ADDR to change a string -- which is exactly the
// coupling the three-way identifier split exists to remove.
//
// The name is FLOCK-scoped, not node-scoped, because it resolves to the VIP: a node-scoped name
// would change identity on failover while the address it points at did not.
const verbNetMDNSName = "net.mdnsname"

// verbNetMDNSPublished reads back the name avahi ACTUALLY published, which is not always the name
// it was asked for: on a collision avahi conflict-renames to `<name>-2` and tells nobody. Same
// doctrine as verbNetVIP, and for the same reason -- V3.19 was a name that was present, plausible,
// and not what anyone thought it was. Reporting back the REQUESTED name would reproduce that
// failure precisely, in the item that exists to end it.
const verbNetMDNSPublished = "net.mdnspublished"

// Sys.hostname sets the guest's hostname to this node's name. DRBD matches the
// running hostname against the `on <name>` stanzas in the .res, so every fleet
// guest (one baked image, hostname "guest") must be renamed to its node name
// before create-md, or DRBD reports "not defined for this host". The agent sets it
// uniformly at bring-up (a no-op rename to "guest" on the single-node/legacy path).
//
// NOTE this is the NODE id (`briard-node-3f9a2c`), which is hidden, and NOT the flock name a
// household sees -- see verbNetMDNSName. They were the same string until V3.20, which is why
// nothing visible could be renamed without breaking DRBD's self-match.
const verbSetHostname = "sys.hostname"

// Upgrade/rollback verbs, driven by the host's guest.Manager. Data
// snapshot/restore cut at the payload's btrfs subvolume (per-service scope, not the
// whole volume). os.system reads the code identity (the closure store path, node-
// independent -- NOT a generation number); os.switch is the whole-VM code half.
const (
	verbPayloadStart  = "payload.start"  // systemctl start <unit>
	verbPayloadStop   = "payload.stop"   // systemctl stop <unit> (quiesce before snapshot)
	verbPayloadActive = "payload.active" // systemctl is-active <unit> -> bool
	verbPayloadHealth = "payload.health" // in-guest GET of the payload health URL -> bool (the probe done from inside the guest, so it survives a substrate — e.g. macvtap — where the host can't reach the VIP)
	verbPayloadSince  = "payload.since"  // ActiveEnterTimestampMonotonic -> usec (0=inactive); adopt-not-bounce proof
	verbDataSnapshot  = "data.snapshot"  // btrfs subvolume snapshot -r <DataDir> <dest>
	verbDataRestore   = "data.restore"   // replace the live subvolume with a snapshot
	verbOSSystem      = "os.system"      // readlink -f /run/current-system -> closure store path
	verbOSStage       = "os.stage"       // nix-store --realise: pull a closure INTO the store
	verbOSComponents  = "os.components"  // read a closure's boot-critical parts, for the reboot decision
	verbOSSwitch      = "os.switch"      // point the system profile at a closure + activate it
	verbOSStageBoot   = "os.stageboot"   // make a closure BOOTABLE without making it the default
	verbOSPowerOff    = "os.poweroff"    // ask the guest OS to shut itself down cleanly
	verbOSGC          = "os.gc"          // drop old profile generations, then collect the store
)

// There is no `os.pin` / `os.reqsystem` verb and no `.code-system` file: the
// whole-OS closure was never a property of the data. The data's identity is per-service — the
// payload image and the service manifest, both here on the replicated volume — while a system
// closure is a property of the NODE. Storing one per service volume did not survive the
// multi-service shape (N volumes, one running OS => N assertions about one system), and the
// compatibility case for keeping it does not hold either: a newer kernel makes btrfs features
// available rather than enabled, and DRBD metadata version is fixed at create-md. Format
// migrations, if one ever lands, are a manual procedure with their own gating.

// Runtime service install. Two verbs, not one, because the halves live in DIFFERENT
// DURABILITY DOMAINS and folding them together would hide that:
//
//   - service.render writes quadlet source to /run (node-local, tmpfs) and reloads systemd so the
//     generated units exist. EVERY node runs this, secondaries included — a survivor can only
//     start the pod because it was rendered there too.
//   - service.provision creates the service subvolume + its per-container subdirectories and
//     records the manifest, all on the REPLICATED VOLUME, which only the Primary has mounted. A
//     secondary cannot run this at all.
//
// Both are dumb hands ([[logic-on-host-by-default]]): the host renders the unit text
// (agent/quadlet), decides which node gets which verb, and owns the ordering. The guest writes
// bytes and makes directories.
const (
	verbServiceRender    = "service.render"    // write quadlet units to /run + daemon-reload (every node)
	verbServiceProvision = "service.provision" // create the service subvolume + record the manifest (Primary only)
	// service.installed READS ONE NAMED SERVICE's manifest off the volume, or "". It REPLACES
	// the unnamed service.manifest rather than widening it, because a verb whose meaning changes
	// under an unchanged name is what a protocol bump exists to police -- and a bump is the
	// expensive instrument here: the host agent self-updates independently of the guest OS closure
	// ([V3.4]), so raising MinGuestProtocol makes every host refuse every not-yet-rolled guest
	// fleet-wide, and its own health gate then reverts the self-update. A NEW verb is refused by
	// exactly the one path that needs it (Supports), which is the instrument service.warm already
	// set the precedent for ([V3b.3](e1), no api.go change).
	verbServiceInstalled = "service.installed" // read one named service's manifest from the volume, or ""
	// service.list NAMES the services the volume carries. It is what makes a converged node able
	// to say what it runs: converge-at-promotion renders from the volume, so a survivor that never
	// installed anything runs services the HOST was never told about -- and the host reports from
	// its node-local cache, which only an install on a Primary writes. Measured on a fleet run
	// 2026-08-28: the survivor served the upgraded fixture at the VIP (its tick counter moved)
	// while reporting no services at all, so the cloud saw a node running nothing and could not
	// have confirmed any rollout on it. Names only, because the manifest for each is what
	// service.installed already returns; a verb that returned both would duplicate that one.
	verbServiceList = "service.list" // list the services recorded on the volume
	verbServiceWarm = "service.warm" // ensure an image is present, starting its .image unit ONLY if it is missing
	// service.converge re-runs converge-at-promotion IN PLACE, on a node that is already Primary
	// -- render every manifest on the volume, warm, start ([V3b.3](f), converge.go). It is what an
	// install calls once it has written the new manifest, and it exists as a VERB rather than a
	// `systemctl restart briard-services` because briard-services is a promoter CHAIN MEMBER:
	// stopping one deactivates drbd-reactor's target, which unmounts the volume and demotes the
	// node. Same code, same unit, no unit lifecycle touched.
	verbServiceConverge = "service.converge"
	// service.forget REMOVES one service's manifest from the volume. It exists because converge
	// made the volume the truth ([V3b.3](f)): a FRESH install that fails its health gate used to
	// be undone by putting the node-local promoter chain back, which simply did not mention the
	// new service -- but the manifest it wrote to the volume stayed, and under converge every
	// future promotion anywhere in the flock would render and start it again. Reverting a fresh
	// install therefore has to remove the identity, not just stop the units.
	verbServiceForget = "service.forget"
)

// manifestDir holds the installed services' identities on the replicated volume — one file per
// service, `<name>.json`, holding the manifest's own bytes, whose content hash IS that identity.
// It superseded the payload slot's single OCI digest, which is now deleted ([V3b.3](e1)): one
// manifest transitively pins the whole container set. The VOLUME holds the manifests, never the rendered
// units — a survivor re-renders rather than replaying units that a different podman version may
// have produced.
//
// A DIRECTORY rather than one file, mirroring the host's node-local cache and for the same reason
// ([V3b.3](b)): the volume must be able to say which service it means. With one file, installing
// a SECOND service read the first as its own "prior" — so it snapshotted against the wrong
// service, and `filesToRemove` deleted the first service's rendered units as a renamed prior's
// orphans. That is the concrete defect the one-service-at-a-time gate stood in for.
//
// A node that installed a service BEFORE this split has its manifest at the old single path and
// nothing reads it: priorService then finds no prior, treats the next install as fresh, and skips
// the rollback target. That is priorService's own documented fresh-install case, its worst outcome
// is a rollback to empty rather than to a broken new service, and the first install after the
// upgrade writes the new location — so this carries no migration step ([[alpha-reinstall-only-policy]]).
const manifestDir = "/var/lib/briard/.services"

// quadletDir is where podman's generator reads unit source from. Mirrors agent/quadlet.Dir; the
// guest is told the filenames but not the directory, so this path is the guest's own.
const quadletDir = "/run/containers/systemd"

const (
	// TLS cert/key on the DRBD volume: replicated, so a failover serves the same
	// cert; the terminator hot-reloads them. Pairs with the guest image's tlsDir.
	tlsDir      = "/var/lib/briard/tls"
	tlsCertPath = tlsDir + "/fullchain.pem"
	tlsKeyPath  = tlsDir + "/key.pem"
)

// Maintenance-mode verbs: a planned upgrade must hold drbd-reactor's promoter
// so it doesn't treat a deliberate payload stop as a failure (demote/re-promote). Pause
// stops the drbd-reactor daemon (stop-services-on-exit defaults false -> DRBD Primary +
// services stay up); Resume restarts it (re-adopts, no restart/demote). The payload is
// then cycled surgically (payload.stop is ignore-dependencies, so the VIP/data/target
// stay up), so no target re-raise is needed.
const (
	verbReactorPause  = "reactor.pause"  // systemctl stop drbd-reactor.service
	verbReactorResume = "reactor.resume" // systemctl start drbd-reactor.service
	verbReactorActive = "reactor.active" // systemctl is-active drbd-reactor.service -> bool (interim guard)
	verbReactorEvict  = "reactor.evict"  // drbd-reactorctl evict: hand the work to a peer
	verbHello         = "hello"          // protocol handshake: version + capabilities
	verbCertWrite     = "cert.write"     // write a renewed cert/key to the DRBD volume
	verbResources     = "sys.resources"  // read appliance resource telemetry
	verbBackupSave    = "backup.save"    // tar+age-encrypt .storage/config to an off-site path
	verbBackupRestore = "backup.restore" // age-decrypt+extract a backup into the data dir
	verbFsSync        = "fs.sync"        // flush the data volume's dirty pages (pre-eviction pre-copy)
)

// dataMountRoot is where briard-data mounts the replicated volume — the guest image's
// `btrfsRoot` (guest-image/configuration.nix), restated here the way manifestDir restates a
// path under it. fs.sync carries no path on the wire on purpose: the verb has exactly one
// meaning ("flush the replicated volume"), and the node that mounts the volume is the one that
// knows where it lives.
const dataMountRoot = "/var/lib/briard"

// bootIDPath is the kernel's per-boot identifier, which the handshake reports so the host can
// recognise a guest that rebooted underneath it ([B.102]). The kernel mints it once per boot and
// it survives every in-guest agent restart, which is exactly the line the host needs drawn --
// and unlike a hostname or an address it is not something bring-up sets, so it cannot be
// confused with the convergence it is used to trigger.
const bootIDPath = "/proc/sys/kernel/random/boot_id"

// guestCapabilities is the verb set the guest advertises in its handshake -- the honest
// capability list a host negotiates against (Client.Supports). Keep in sync with the
// dispatch switch; a verb absent here is invisible to a capability-checking host even if
// the switch handles it. (A drift guard test asserts a representative subset is present.)
var guestCapabilities = []string{
	verbSetHostname, verbProvision, verbUp, verbReactor, verbStatus, verbNetConfigure, verbNetVIP,
	verbNetMDNSName, verbNetMDNSPublished,
	verbPayloadStart, verbPayloadStop, verbPayloadActive, verbPayloadHealth, verbPayloadSince,
	verbDataSnapshot, verbDataRestore,
	verbServiceRender, verbServiceProvision, verbServiceInstalled, verbServiceList, verbServiceWarm, verbServiceConverge, verbServiceForget, verbReactorActive,
	verbOSSystem, verbOSStage, verbOSComponents, verbOSSwitch, verbOSStageBoot, verbOSPowerOff,
	verbOSGC,
	verbReactorPause, verbReactorResume, verbReactorEvict,
	verbCertWrite,
	verbResources,
	verbBackupSave, verbBackupRestore,
	verbFsSync,
}

const (
	// CurrentSystem resolves to the running system's closure store path (the code identity).
	currentSystem = "/run/current-system"
	// BootedSystem is the generation this kernel actually booted -- NOT currentSystem, which a
	// switch-only update moves out from under it. It is the honest reference for "would this
	// take a reboot?": the kernel in the machine is the booted one.
	bootedSystem = "/run/booted-system"
	// SystemProfile is the profile os.switch repoints to roll the whole-VM generation.
	systemProfile = "/nix/var/nix/profiles/system"
	// StagingProfile is where a REBOOT-path upgrade parks its target. It is a
	// second system profile, NOT the system one: install-grub.pl globs
	// /nix/var/nix/profiles/system-profiles/* at run time and gives each a submenu of its
	// own, so registering here makes the closure bootable WITHOUT touching the default
	// entry. The name is load-bearing -- it becomes the grub submenu title the SMBIOS
	// selector names (guest-image/disk-image.nix) -- and must stay \w+ or install-grub.pl
	// skips the profile entirely (it also globs the `staging-N-link` generations, which is
	// what that filter exists to reject).
	stagingProfileDir = "/nix/var/nix/profiles/system-profiles"
	stagingProfile    = stagingProfileDir + "/staging"
	// JournalDir + containerStore are the appliance growth surfaces the resource telemetry
	// measures: the systemd journal and the podman code/layer store. Both are the
	// unmanaged-growth suspects the soak's disk trend names.
	journalDir     = "/var/log/journal"
	containerStore = "/var/lib/containers"
	// KmsgCursorPath tracks how far the kernel-error report has read, so each poll returns only
	// NEW warning+ lines. In /run so a guest reboot re-baselines cleanly.
	kmsgCursorPath = "/run/briard/.kmsg-cursor"
)

// ProvisionRequest carries the host-rendered configs (drbd.Resource.Config and
// drbd.ReactorConfig) to drop in the guest, then create-md the resource.
// ReactorConfig is empty on a witness (no promoter there).
type ProvisionRequest struct {
	Resource      string `json:"resource"`
	ResConfig     string `json:"res_config"`
	ReactorConfig string `json:"reactor_config,omitempty"`
	Diskless      bool   `json:"diskless,omitempty"` // witness: no metadata, so no create-md
}

// ProvisionResult reports whether Provision created fresh DRBD metadata. It is false
// when the data disk already held a valid replica (a node returning from a reboot) -- so the
// caller knows NOT to declare the node UpToDate (skip-initial-sync), which is only ever correct
// on a true first init. False for a diskless witness too (it has no metadata).
type ProvisionResult struct {
	CreatedMetadata bool `json:"created_metadata"`
}

// resourceRequest names the resource for up/reactor/status.
type resourceRequest struct {
	Resource string `json:"resource"`
}

// unitRequest names a systemd unit (payload start/stop/is-active).
type unitRequest struct {
	Unit string `json:"unit"`
}

// healthRequest carries the payload health URL the guest GETs from inside itself
// (payload.health). The host owns the URL (its HealthURL config); the guest just probes it.
type healthRequest struct {
	URL string `json:"url"`
}

// snapshotRequest is a btrfs subvolume op: DataDir is the live rw subvolume, Path is
// the RO snapshot subvolume (the dest on snapshot, the source on restore).
type snapshotRequest struct {
	DataDir string `json:"data_dir"`
	Path    string `json:"path"`
}

// systemRequest names a closure store path to switch the system to (os.switch).
type systemRequest struct {
	Path string `json:"path"`
}

// stageRequest names a closure to realise INTO the guest store (os.stage), and
// optionally the one binary cache to realise it from.
//
// From/FromKey are empty in production: the guest substitutes from the caches baked
// into its image (cache.nixos.org + cache.briard.io, guest-image/configuration.nix),
// which is the whole point of the cache. When set, they OVERRIDE that list for this one
// call — fetch only from From, and accept only narinfos signed by FromKey.
//
// Letting the host name the source grants it no authority it lacks: it already dictates
// WHICH closure the guest activates (os.switch, Principle 8 — the host owns every
// generation switch), so naming WHERE the bytes come from and WHOSE signature to accept
// cannot widen that. What it must never do is *weaken* the check, so `require-sigs` is
// never relaxed — an override swaps the trusted key, it does not remove the gate. The
// nixosTest harness uses this to point a guest at a cache served inside the test; a future flock peer-cache (one node downloads, then serves the rest over the LAN) is the same shape.
type stageRequest struct {
	Path    string `json:"path"`
	From    string `json:"from,omitempty"`
	FromKey string `json:"from_key,omitempty"`
}

// SystemComponents are the parts of a system closure that a running kernel cannot be
// talked out of: swapping them takes a boot, not a `switch-to-configuration switch`
// . Every field is a resolved store path except KernelParams, which is the
// literal command line — a params change is invisible in the store paths and still needs a
// boot to take effect.
//
// This is FACTS, not a verdict: the guest reads, the host decides
// ([[logic-on-host-by-default]]). That split is what makes the decision exhaustively
// unit-testable on the host without a live guest, and it keeps the policy — which
// differences are worth a reboot — in one place instead of baked into a guest binary that
// updates on its own schedule.
type SystemComponents struct {
	Kernel        string `json:"kernel"`
	Initrd        string `json:"initrd"`
	KernelModules string `json:"kernel_modules"`
	Systemd       string `json:"systemd"`
	KernelParams  string `json:"kernel_params"`
}

// StageSource overrides where one Stage call fetches from: a binary cache URL and the
// public key its narinfos must carry. The zero value means "use the guest's baked
// caches", which is production. See stageRequest for why the host may name this.
type StageSource struct {
	URL string
	Key string
}

// serviceRenderRequest carries the quadlet source the host rendered: filename -> content, to be
// written under quadletDir. Stale is the set of filenames to remove first, so swapping which
// service occupies the slot leaves nothing of the previous one behind.
type serviceRenderRequest struct {
	Files map[string]string `json:"files"`
	Stale []string          `json:"stale,omitempty"`
}

// serviceProvisionRequest creates the service's single data subvolume with per-container plain
// SUBDIRECTORIES inside it (never nested subvolumes — `btrfs subvolume delete` refuses on a
// subvolume containing them, which would break data.restore outright), and records Manifest as
// the service identity on the replicated volume.
type serviceProvisionRequest struct {
	// Name is which service this is, and therefore which identity file on the volume the
	// manifest lands in. The host owns the naming because it owns the manifest; deriving it
	// from DataDir here would make the guest re-implement a layout it is only ever told.
	Name     string   `json:"name"`
	DataDir  string   `json:"data_dir"`
	Subdirs  []string `json:"subdirs,omitempty"`
	Manifest string   `json:"manifest"`
}

// serviceInstalledRequest names the service whose recorded manifest to read (service.installed).
// The NAME is the whole request, and it is what makes the volume able to say which service it
// means: with an unnamed read, installing a second service saw the first as its own prior.
type serviceInstalledRequest struct {
	Name string `json:"name"`
}

// manifestPath is one service's identity file on the replicated volume. The name is a manifest
// slug — shared/manifest's Validate refuses anything else, precisely because these become path
// elements — and the dispatch re-checks it with safeUnitName before it gets here, because the
// guest must not depend on the host having validated its input.
func manifestPath(name string) string { return manifestDir + "/" + name + ".json" }

// serviceWarmRequest names one image to ensure is present (service.warm), and the .image unit
// that would obtain it. Both halves are needed because the CHECK is on the ref and the ACTION is
// on the unit -- and the host, which owns the manifest, is the only side that knows the pairing.
type serviceWarmRequest struct {
	Unit string `json:"unit"`
	Ref  string `json:"ref"`
}

// certWriteRequest carries a renewed cert + key to land on the DRBD volume (cert.write).
type certWriteRequest struct {
	Cert string `json:"cert"`
	Key  string `json:"key"`
}

// backupSaveRequest seals the home's sacred config to an off-site path (backup.save). Base is the payload's data-dir mount (== /config in the container); Includes are
// paths under it (".storage", "configuration.yaml", …); Recipient is the household's age
// public key (seal only — the private key never reaches the guest); Dest is where the
// encrypted blob lands (a mounted off-site target).
type backupSaveRequest struct {
	Base      string   `json:"base"`
	Includes  []string `json:"includes"`
	Recipient string   `json:"recipient"`
	Dest      string   `json:"dest"`
}

// backupRestoreRequest decrypts a backup blob and extracts it into the data dir
// (backup.restore). Identity is the household's age private key (restore only, a
// rare recovery op; the blob at rest stays encrypted).
type backupRestoreRequest struct {
	Base     string `json:"base"`
	Src      string `json:"src"`
	Identity string `json:"identity"`
}

// reactorRequest names a drbd-reactor promoter snippet to pause/resume.
type reactorRequest struct {
	Snippet string `json:"snippet"`
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

// netConfigureRequest sets a static CIDR on a guest interface (net.configure) and,
// optionally, records which NIC the promoter should claim the VIP on. VIPDev is set
// only on a data node whose VIP is not on the baked-default NIC (the two-subnet
// layout puts DRBD on eth1, so the VIP moves to eth2 --); "" leaves the baked
// default (single-node/legacy: the lone NIC is eth1).
//
// VIPAddr is the service address itself, in CIDR form ("192.168.9.50/24"). Like VIPDev it is
// RECORDED here, not applied -- the promoter chain claims it when this node wins the primary
// role. "" leaves the guest's baked fallback, which the agent-less harnesses still run on.
// mdnsNameRequest carries the flock's visible name (net.mdnsname). Name is the BARE flock name
// ("brave-elf"); the `briard-` prefix and the `.local` suffix are the guest's to add, so the one
// place that knows the published label's shape is the unit that publishes it.
type mdnsNameRequest struct {
	Name string `json:"name"`
}

type netConfigureRequest struct {
	Dev     string `json:"dev"`
	CIDR    string `json:"cidr"`
	VIPDev  string `json:"vip_dev,omitempty"`
	VIPAddr string `json:"vip_addr,omitempty"`
	// The private host<->guest link, when the substrate has one. PrivDev is the guest NIC facing
	// it and PrivHostIP is the host's own address on the SYSTEM subnet, reached over that NIC by
	// a /32 -- which must beat the on-link /24 the system address itself installs, because under
	// macvtap that on-link path leads out of eth1 to a host the substrate isolates us from. Both
	// empty = no private link (the bridge substrate, where the host shares our L2 and is simply
	// on-link) and nothing is routed. Named by the host, never inferred here: the guest bakes no
	// positional knowledge of which NIC is which ([V3b.16a]).
	PrivDev     string `json:"priv_dev,omitempty"`
	PrivCIDR    string `json:"priv_cidr,omitempty"`
	PrivHostIP  string `json:"priv_host_ip,omitempty"`
	PrivHostMAC string `json:"priv_host_mac,omitempty"`
	// THE SERVICE IDENTITY, WHEN THE GUEST HAS TO MAKE IT ITSELF ([V3b.26c]). Under the bridge
	// substrate the host gives us ONE tap -- all Windows can express -- so VIPDev is not a NIC
	// anyone handed us; VIPParent names the NIC to build it on as a macvlan child, and VIPMAC is
	// the flock-scoped MAC that child must carry, because failover moves a MAC and never just an
	// address. Both empty = the host built the device (macvtap), and nothing here creates anything.
	VIPParent string `json:"vip_parent,omitempty"`
	VIPMAC    string `json:"vip_mac,omitempty"`
}

// NetConfig is what the host tells the guest about its own networking, in one call. A struct
// rather than a seventh positional argument: these are five independent facts about three
// different NICs, and at that width a caller transposing two strings produces a node that
// configures itself confidently and wrongly.
type NetConfig struct {
	Dev         string // the system NIC -- this node's node IP lands here, and DRBD binds it
	CIDR        string // that address, with prefix; "" leaves the NIC unaddressed
	VIPDev      string // the NIC the promoter claims the VIP on; "" = this node claims none
	VIPAddr     string // the VIP itself, CIDR form; "" = DHCP decides
	PrivDev     string // the guest NIC facing the private host link; "" = no such link
	PrivCIDR    string // this end's own address on it -- SUBSTRATE, addressed by nobody; see the handler
	PrivHostIP  string // the host's system-subnet address, routed over PrivDev
	PrivHostMAC string // that end's link address, pinned as a permanent neighbour -- see the handler
	VIPParent   string // the NIC to build VIPDev on as a macvlan child; "" = the host already built it
	VIPMAC      string // the flock MAC that child carries; meaningless without VIPParent
}

// netVIPRequest names the device to read the live VIP from (net.vip). Empty Dev = this node has
// no VIP device, which answers "" rather than erroring.
type netVIPRequest struct {
	Dev string `json:"dev"`
}

// firstCIDR pulls the address out of one `ip -o -4 addr show` line:
//
//	2: eth2    inet 192.168.9.50/24 brd 192.168.9.255 scope global eth2\       valid_lft ...
//
// i.e. the field after "inet". Returns "" when there is no such field, which is the honest
// answer for an unaddressed device -- never a guess, and never a partially-parsed string.
//
// IPv4LL (169.254.0.0/16) is SKIPPED, and that is not tidiness. A link-local address is one the
// machine gave ITSELF when nobody answered, so it can never be the flock's service address -- yet
// it reads as an address to everything downstream. Measured: with no DHCP server on the segment,
// dhcpcd self-assigned 169.254.57.250, this reported it as the VIP, the health probe hit it from
// INSIDE the guest and passed, and the node published HEALTHY while nothing on the LAN could reach
// it. That is precisely the defect V3.19 exists to end, arriving through V3.19's own replacement.
//
// The guest is separately told not to invent one (dhcpcd -L), so this is the second line of
// defence rather than the first -- deliberately, because the cost of being wrong here is a node
// that looks fine. Ground truth that reports an unreachable address is not ground truth.
func firstCIDR(out string) string {
	for line := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		f := strings.Fields(line)
		for i, tok := range f {
			if tok == "inet" && i+1 < len(f) {
				if addr := f[i+1]; !strings.HasPrefix(addr, "169.254.") {
					return addr
				}
			}
		}
	}
	return ""
}

// allCIDRs returns every global v4 address in `ip -o -4 addr show ... scope global` output, in
// CIDR form. firstCIDR above answers "what does this NIC hold" for the VIP probe and stops at the
// first; this answers "what must be pruned", so it cannot stop -- a NIC carrying three stale
// addresses must give up all three. Link-local is excluded for the same reason it is there: a
// self-assigned 169.254 is not an address we put on and not one we take off.
func allCIDRs(out string) []string {
	var found []string
	for line := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		f := strings.Fields(line)
		for i, tok := range f {
			if tok == "inet" && i+1 < len(f) {
				if addr := f[i+1]; !strings.HasPrefix(addr, "169.254.") {
					found = append(found, addr)
				}
			}
		}
	}
	return found
}

// hostnameRequest carries this node's name (sys.hostname).
type hostnameRequest struct {
	Name string `json:"name"`
}

// resourcesRequest is what the host must tell the guest to measure the appliance: each service's
// systemd unit (for its cgroup -> RSS/fds/restarts) and the data volume (for used space +
// snapshot count). Both empty on a witness -> only the volume-independent metrics come back, which
// is exactly why the host asks REGARDLESS of whether anything is installed.
type resourcesRequest struct {
	// Services pairs each service's name with the unit whose footprint IS that service's. The
	// host owns the pairing because it owns the manifest; the guest measures what it is handed
	// and names nothing itself ([[logic-on-host-by-default]]).
	Services []resourceService `json:"services,omitempty"`
	DataDir  string            `json:"data_dir,omitempty"` // the DRBD data volume mount
}

// resourceService is one service to measure: its name, and its serving unit.
type resourceService struct {
	Name string `json:"name"`
	Unit string `json:"unit"`
}

// Executor runs commands and writes files inside the guest. The real impl shells
// out (NewOSExecutor); tests supply a fake.
type Executor interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
	WriteFile(path string, data []byte) error
	// ReadFile returns a file's contents. A MISSING file must come back as an error the caller
	// can recognise with os.IsNotExist rather than as empty content: "no name is published" and
	// "the name is the empty string" are different answers, and only one of them is normal.
	ReadFile(path string) ([]byte, error)
	Sethostname(name string) error
}

// dispatch is the guest-side verb switch: verb -> Executor calls (+ ParseStatus).
func dispatch(x Executor) dispatchFunc {
	return func(ctx context.Context, verb string, payload json.RawMessage) (any, error) {
		// Run executes a guest command whose output is only wanted on failure, and
		// wraps a non-zero exit with the command + its output -- otherwise the host
		// sees a bare "exit status 1" and bring-up failures are undiagnosable.
		run := func(name string, args ...string) error {
			out, err := x.Run(ctx, name, args...)
			if err == nil {
				return nil
			}
			if o := bytes.TrimSpace(out); len(o) > 0 {
				return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, o)
			}
			return fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
		}
		switch verb {
		case verbHello:
			// Report our protocol version + capabilities so the host can negotiate/refuse
			//. No side effects; safe to call before anything else.
			//
			// The boot_id rides along because the host cannot otherwise tell a bounced agent
			// from a rebooted guest ([B.102]). Read best-effort: a hello that FAILED would be
			// a guest the host refuses to drive, and no host has ever needed this field to
			// drive one -- so an unreadable boot_id is reported as absent, not as an error.
			hello := api.GuestHello{Version: api.GuestProtocol, Capabilities: guestCapabilities}
			if b, err := x.ReadFile(bootIDPath); err == nil {
				hello.BootID = strings.TrimSpace(string(b))
			}
			return hello, nil
		case verbSetHostname:
			var req hostnameRequest
			if err := json.Unmarshal(payload, &req); err != nil {
				return nil, err
			}
			// IN MEMORY AND NOWHERE ELSE, which is the whole of it now. This used to also persist
			// the name to /etc/briard/node-id for briard-identity.service to restore at boot,
			// because the `.res` it must match survived a reboot and syscall.Sethostname did not:
			// the guest came back as the baked "guest" against a persistent `on briard-node-<id>`,
			// the boot-started reactor promoted into the mismatch, and a failed promote is never
			// retried -- quorate but never Primary, no VIP, no address (V3.20, on the L0 runner;
			// invisible before it because the two were the SAME LITERAL).
			//
			// Two facts that must agree need one LIFETIME, and V3.20 gave them one by making the
			// NAME persistent. [V3b.16b] gives them one the other way -- the `.res` is ephemeral
			// too, both are re-derived from the host at every bring-up, and [V3b.16a] means nothing
			// can promote before that bring-up. The file, the unit and its silent
			// keep-the-baked-name fallback are all deleted: a third copy on disk is a third thing
			// that can be wrong.
			return nil, x.Sethostname(req.Name)
		case verbProvision:
			var req ProvisionRequest
			if err := json.Unmarshal(payload, &req); err != nil {
				return nil, err
			}
			if err := x.WriteFile(resPath(req.Resource), []byte(req.ResConfig)); err != nil {
				return nil, err
			}
			if req.ReactorConfig != "" {
				if err := x.WriteFile(reactorPath, []byte(req.ReactorConfig)); err != nil {
					return nil, err
				}
			}
			if req.Diskless {
				return ProvisionResult{}, nil // a diskless witness has no metadata to create
			}
			// Idempotent bring-up: `create-md` WITHOUT --force is itself the metadata
			// probe. A node returning from a reboot already holds its replica on the persisted
			// data disk -- a blind create-md --force would WIPE it and re-seed, split-braining
			// against the peer that kept serving. On a fresh/blank disk create-md writes new
			// metadata (exit 0 -> CreatedMetadata); on a disk that already holds metadata DRBD
			// refuses to overwrite -- the confirm prompt hits EOF (the Executor gives commands
			// /dev/null stdin) and aborts non-zero, which we read as "metadata already present,
			// attach it, never wipe". This is more reliable than a dump-md pre-check, which at
			// provision time (resource written but not yet defined in the kernel) reported no
			// metadata on a disk that still had it. A non-metadata failure (bad config/disk)
			// also lands here as "attach"; Up then fails loudly, so bring-up still stops rather
			// than silently wiping. Use x.Run (not run): a non-zero here is expected, not fatal.
			if _, err := x.Run(ctx, "drbdadm", "create-md", req.Resource); err != nil {
				return ProvisionResult{CreatedMetadata: false}, nil // existing replica / refusal: attach, don't wipe
			}
			return ProvisionResult{CreatedMetadata: true}, nil
		case verbAdjust:
			var req ProvisionRequest
			if err := json.Unmarshal(payload, &req); err != nil {
				return nil, err
			}
			// Runtime mesh growth: rewrite the resource config with the new peer set and
			// apply the delta to the ALREADY-RUNNING resource -- `drbdadm adjust` brings up the
			// connection(s) to the joining node(s) with no restart and no create-md, so the serving
			// primary's local disk stays attached and UpToDate. The joining anchor/witness themselves
			// come up fresh via Provision+Up (FreshInit=false) and resync as SyncTarget; this side is
			// the primary learning it now has peers. Idempotent: adjust to an unchanged config is a
			// no-op, so a retried pair directive is safe.
			if err := x.WriteFile(resPath(req.Resource), []byte(req.ResConfig)); err != nil {
				return nil, err
			}
			if req.ReactorConfig != "" {
				if err := x.WriteFile(reactorPath, []byte(req.ReactorConfig)); err != nil {
					return nil, err
				}
			}
			return nil, run("drbdadm", "adjust", req.Resource)
		case verbUp:
			req, err := resourceReq(payload)
			if err != nil {
				return nil, err
			}
			return nil, run("systemctl", "start", "drbd@"+req.Resource+".target")
		case verbInitUUID:
			req, err := resourceReq(payload)
			if err != nil {
				return nil, err
			}
			// Declare a brand-new resource's local data current, skipping the initial
			// sync (there is no peer to sync from on first init). One-time; NOT a
			// force-promotion. The agent does this only when it first creates a resource.
			return nil, run("drbdadm", "new-current-uuid", "--clear-bitmap", req.Resource+"/0")
		case verbReactor:
			if _, err := resourceReq(payload); err != nil {
				return nil, err
			}
			// ARMING THE PROMOTER, and since [V3b.16a] this is the only thing that ever does: the
			// guest unit is `wantedBy = [ ]`, so drbd-reactor does not start at boot. Everything
			// bring-up does above -- hostname, NIC addressing, vip.env, the .res, the promoter
			// snippet, a runtime service's units -- is therefore GUARANTEED to be in place before
			// anything can promote, on a reboot as well as on a first install. It used to be
			// guaranteed only on a first install, and losing that race is [V3b.16].
			//
			// Idempotent, deliberately: `systemctl start` on a running daemon is a no-op, so
			// re-converging a healthy node costs nothing, and an agent that died inside a
			// maintenance bracket re-arms the promoter it left stopped ([V3b.15]).
			return nil, run("systemctl", "start", "drbd-reactor.service")
		case verbStatus:
			req, err := resourceReq(payload)
			if err != nil {
				return nil, err
			}
			out, err := x.Run(ctx, "drbdsetup", "status", "--json")
			if err != nil {
				return nil, err
			}
			// The fuller view. QuorumState is embedded, so a host that only knows
			// the three summary fields reads this response unchanged.
			return drbd.ParseCluster(out, req.Resource)
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
			// Flush so the cert replicates before a failover relies on it (as payload.pin).
			_, err := x.Run(ctx, "sync", "-f", tlsCertPath)
			return nil, err
		case verbBackupSave:
			var req backupSaveRequest
			if err := json.Unmarshal(payload, &req); err != nil {
				return nil, err
			}
			// Direct file I/O (not via Executor): the archive/encrypt is Go-level work over
			// the mounted volume, like the shared/backup unit tests. Bulk never crosses the
			// control channel — the guest seals + writes the blob locally (data.snapshot idiom).
			recip, err := backup.ParseRecipient(req.Recipient)
			if err != nil {
				return nil, err
			}
			if err := os.MkdirAll(filepath.Dir(req.Dest), 0o755); err != nil {
				return nil, err
			}
			f, err := os.Create(req.Dest)
			if err != nil {
				return nil, err
			}
			if err := backup.Save(req.Base, req.Includes, recip, f); err != nil {
				f.Close()
				return nil, err
			}
			if err := f.Close(); err != nil {
				return nil, err
			}
			_, err = x.Run(ctx, "sync", "-f", req.Dest) // durability: the blob is the off-site copy
			return nil, err
		case verbBackupRestore:
			var req backupRestoreRequest
			if err := json.Unmarshal(payload, &req); err != nil {
				return nil, err
			}
			id, err := backup.ParseIdentity(req.Identity)
			if err != nil {
				return nil, err
			}
			f, err := os.Open(req.Src)
			if err != nil {
				return nil, err
			}
			defer f.Close()
			return nil, backup.Load(f, id, req.Base)
		case verbPayloadStart:
			req, err := unitReq(payload)
			if err != nil {
				return nil, err
			}
			return nil, run("systemctl", "start", req.Unit)
		case verbPayloadStop:
			req, err := unitReq(payload)
			if err != nil {
				return nil, err
			}
			// Surgical stop: --job-mode=ignore-dependencies stops ONLY this unit, so a
			// planned payload quiesce can't cascade to the promoter target / data mount /
			// VIP (the promoter is separately paused). Correct by construction --
			// no dependence on drbd-reactor's exact systemd dependency directives.
			return nil, run("systemctl", "--job-mode=ignore-dependencies", "stop", req.Unit)
		case verbPayloadActive:
			req, err := unitReq(payload)
			if err != nil {
				return nil, err
			}
			// Is-active exits non-zero when the unit is not active, printing the state
			// word regardless -- so trust the word, not the exit code.
			out, _ := x.Run(ctx, "systemctl", "is-active", req.Unit)
			return strings.TrimSpace(string(out)) == "active", nil
		case verbPayloadHealth:
			var req healthRequest
			if err := json.Unmarshal(payload, &req); err != nil {
				return nil, err
			}
			// The readiness probe, done from INSIDE the guest (not host->VIP over the LAN):
			// the guest owns the VIP on its own NIC, so this works regardless of the host
			// networking substrate (macvtap blocks host->guest, but not guest->itself). A raw
			// net.Dial HTTP/1.0 GET keeps the guest binary free of net/http + the TLS stack the
			// -tags guest build deliberately trims. 200 == healthy; any error == not.
			return probeHTTPOK(ctx, req.URL), nil
		case verbPayloadSince:
			req, err := unitReq(payload)
			if err != nil {
				return nil, err
			}
			// ActiveEnterTimestampMonotonic (usec since boot) changes ONLY when the unit
			// re-enters the active state -- i.e. on a (re)start. It is stable across a
			// promoter pause/resume that merely re-adopts the already-running payload, so an
			// unchanged value is ground truth for "adopt, don't bounce" (the maintenance
			// contract; reused by the per-snippet disable). `--value` prints the raw usec;
			// 0 when the unit is inactive (never entered active), which parseUint yields for "".
			out, err := x.Run(ctx, "systemctl", "show", "-p", "ActiveEnterTimestampMonotonic", "--value", req.Unit)
			if err != nil {
				return nil, err
			}
			return parseUint(out), nil
		case verbDataSnapshot:
			req, err := snapshotReq(payload)
			if err != nil {
				return nil, err
			}
			// REPLACE an existing rollback point rather than snapshotting into it. The path is
			// fixed per service (quadlet.SnapshotPath), so the second upgrade of a service finds
			// the first one's snapshot already sitting there -- and `btrfs subvolume snapshot`
			// given an existing directory creates the new snapshot INSIDE it, which on a
			// read-only snapshot fails with "Read-only file system". Measured on a soak run
			// 2026-08-28: every upgrade after the first failed, the fleet stopped converging, and
			// the error named the filesystem rather than the collision it actually was.
			//
			// A rollback point is one replaceable fact, not a series: the host asks for "the
			// pre-upgrade state of this service", and the previous upgrade's copy is exactly what
			// that supersedes. Deleting it here is what makes this verb idempotent, which is what
			// its caller assumes.
			if _, err := x.Run(ctx, "btrfs", "subvolume", "show", req.Path); err == nil {
				if err := run("btrfs", "subvolume", "delete", req.Path); err != nil {
					return nil, err
				}
			}
			return nil, run("btrfs", "subvolume", "snapshot", "-r", req.DataDir, req.Path)
		case verbDataRestore:
			req, err := snapshotReq(payload)
			if err != nil {
				return nil, err
			}
			// Precondition: the host has stopped the payload (bind released). Swap the
			// live rw subvolume for a fresh rw snapshot of the RO restore point.
			if err := run("btrfs", "subvolume", "delete", req.DataDir); err != nil {
				return nil, err
			}
			return nil, run("btrfs", "subvolume", "snapshot", req.Path, req.DataDir)
		case verbServiceRender:
			var req serviceRenderRequest
			if err := json.Unmarshal(payload, &req); err != nil {
				return nil, err
			}
			return nil, renderService(ctx, x, run, req)
		case verbServiceProvision:
			var req serviceProvisionRequest
			if err := json.Unmarshal(payload, &req); err != nil {
				return nil, err
			}
			return nil, provisionService(ctx, x, run, req)
		case verbServiceWarm:
			var req serviceWarmRequest
			if err := json.Unmarshal(payload, &req); err != nil {
				return nil, err
			}
			// One implementation, shared with converge (converge.go): an installing Primary and a
			// converging survivor must not disagree about what "the image is already here" means.
			// The exists-or-pull rule and why it is safe are argued there.
			return nil, warmImage(ctx, x, req.Unit, req.Ref)
		case verbServiceConverge:
			// No request body: converge takes its whole input from the volume, which is the point
			// -- a caller that could name what to converge TO would be the node-was-told model
			// this replaces.
			return nil, Converge(ctx, x)
		case verbServiceForget:
			var req serviceInstalledRequest
			if err := json.Unmarshal(payload, &req); err != nil {
				return nil, err
			}
			if err := safeUnitName(req.Name); err != nil { // the name is a path element
				return nil, err
			}
			// `rm -f`: an absent manifest is the desired end state, not an error. Then flush the
			// DIRECTORY, because the durable fact here is the entry's REMOVAL -- the same reason
			// provisionService syncs after writing one.
			if err := run("rm", "-f", manifestPath(req.Name)); err != nil {
				return nil, err
			}
			_, err := x.Run(ctx, "sync", "-f", manifestDir)
			return nil, err
		case verbServiceList:
			// The same listing converge itself renders from, so what the host learns a node runs
			// and what the node actually renders cannot drift: one reader, one directory. Names
			// carry the `.json` suffix on disk; the caller wants service names.
			names, err := manifestNames(ctx, x)
			if err != nil {
				return nil, err
			}
			out := make([]string, 0, len(names))
			for _, n := range names {
				out = append(out, strings.TrimSuffix(n, ".json"))
			}
			return out, nil
		case verbServiceInstalled:
			var req serviceInstalledRequest
			if err := json.Unmarshal(payload, &req); err != nil {
				return nil, err
			}
			if err := safeUnitName(req.Name); err != nil { // the name becomes a path element
				return nil, err
			}
			// "" when THIS service is not installed -- the zero-service node and a node running
			// only other services are both legitimate states, not errors, so an absent file must
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
		case verbOSStage:
			var req stageRequest
			if err := json.Unmarshal(payload, &req); err != nil {
				return nil, err
			}
			if req.Path == "" {
				return nil, fmt.Errorf("os.stage: empty system closure")
			}
			// The delivery half of, and the only verb that pulls bytes: realise the
			// closure into the local store, substituting whatever is missing. Everything
			// downstream (os.switch and os.stageboot's staged checks, a promoting peer's converge)
			// assumes the closure is ALREADY local -- the failover path must never fetch
			// -- so this is what makes that assumption true ahead of time.
			//
			// Idempotent: a closure already present realises to a no-op, so re-staging the
			// running system costs nothing and a retry after a partial fetch resumes.
			args := []string{"--realise", req.Path}
			if req.From != "" {
				// Override, not extend: fetch ONLY from the named cache. Extending would
				// leave the baked caches in the list, and in a hermetic test they are
				// unreachable -- so nix would stall on them before trying the one cache
				// that actually has the closure.
				args = append(args, "--option", "substituters", req.From)
				if req.FromKey != "" {
					args = append(args, "--option", "trusted-public-keys", req.FromKey)
				}
			}
			if err := run("nix-store", args...); err != nil {
				return nil, err
			}
			// Make what was just staged DURABLE, not merely written. Nix registers the paths
			// with a synced db transaction but does not fsync their data (fsync-store-paths
			// defaults off), so for a window after staging the store db says "valid" about
			// files whose pages exist only in memory. Anything that captures the disk in that
			// window — the switch path's crash-consistent SnapshotCreateLive moments from now,
			// or a host power cut — then restores a REGISTERED-but-TORN closure, which nix
			// will never re-fetch. Measured ([B.65]): a restored target's kernel-params read
			// back empty, flipping the activation verdict to reboot for a switch-only pair.
			return nil, run("sync")
		case verbOSComponents:
			req, err := systemReq(payload)
			if err != nil {
				return nil, err
			}
			// Empty path = the BOOTED generation, the reference the host diffs against.
			root := req.Path
			if root == "" {
				root = bootedSystem
			}
			var c SystemComponents
			for _, f := range []struct {
				name string
				out  *string
			}{
				{"kernel", &c.Kernel},
				{"initrd", &c.Initrd},
				{"kernel-modules", &c.KernelModules},
				{"systemd", &c.Systemd},
			} {
				out, err := x.Run(ctx, "readlink", "-f", root+"/"+f.name)
				if err != nil {
					return nil, fmt.Errorf("os.components: %s/%s: %w", root, f.name, err)
				}
				*f.out = strings.TrimSpace(string(out))
			}
			// Kernel-params is a FILE, not a link: the command line is not a store path, so a
			// params-only change leaves every other field identical while still needing a boot.
			out, err := x.Run(ctx, "cat", root+"/kernel-params")
			if err != nil {
				return nil, fmt.Errorf("os.components: %s/kernel-params: %w", root, err)
			}
			c.KernelParams = strings.TrimSpace(string(out))
			// An empty read is damage, never a value. No bootable generation has an empty
			// command line (ours always carry console/root/loglevel), but a closure whose
			// data pages were lost to a crash-consistent capture reads exactly this way —
			// registered in the store db, symlinks intact, file content gone ([B.65]: a
			// restored rollback snapshot). Returning "" would hand the caller a diffable
			// value, and ActivationFor would then route a switch-only change down the reboot
			// path — into the very generation whose files are torn. Refuse instead, naming
			// the likely cause, so the directive fails diagnosably.
			if c.KernelParams == "" {
				return nil, fmt.Errorf("os.components: %s/kernel-params read EMPTY — the closure's data is damaged (torn snapshot restore or power cut after staging?)", root)
			}
			return c, nil
		case verbOSSwitch:
			req, err := systemReq(payload)
			if err != nil {
				return nil, err
			}
			// The closure must ALREADY be staged, and this guard is load-bearing rather
			// than defensive: with substituters configured `nix-env --set` will
			// happily substitute a missing closure, so without it a switch could silently
			// fetch -- breaking's "converge is select, never build" in the one place
			// that cannot afford to wait on a network. os.stage is what puts it there, and
			// os.stageboot carries the same guard for the same reason. (It lived on os.pin
			// once the code pin was removed, and belonged here all along: the rule is
			// about switching, not about recording.)
			if err := run("test", "-e", req.Path); err != nil {
				return nil, fmt.Errorf("os.switch: system %s not staged: %w", req.Path, err)
			}
			// Point the profile at the closure (a new generation -- a roll-forward even
			// when reverting) then activate it.
			if err := run("nix-env", "-p", systemProfile, "--set", req.Path); err != nil {
				return nil, err
			}
			return nil, run(req.Path+"/bin/switch-to-configuration", "switch")
		case verbOSStageBoot:
			req, err := systemReq(payload)
			if err != nil {
				return nil, err
			}
			if req.Path == "" {
				return nil, fmt.Errorf("os.stageboot: empty system closure")
			}
			// Same warm-standby rule as os.switch: the closure must ALREADY be local. This verb
			// arms a boot; it never fetches. os.stage is what puts it there.
			if err := run("test", "-e", req.Path); err != nil {
				return nil, fmt.Errorf("os.stageboot: system %s not staged: %w", req.Path, err)
			}
			// Register the closure as a generation of the `staging` profile. This both
			// materialises it as a gcroot and gets it a grub submenu of its own, and it is
			// the ONLY write -- the bootloader default still points at the running system.
			if err := run("mkdir", "-p", stagingProfileDir); err != nil {
				return nil, err
			}
			if err := run("nix-env", "-p", stagingProfile, "--set", req.Path); err != nil {
				return nil, err
			}
			// Regenerate grub.cfg from the RUNNING system, not the staged one. This is the
			// whole trick: install-grub.pl takes its default entry from the toplevel whose
			// switch-to-configuration invoked it ($defaultConfig = $ARGV[1]), so running the
			// current system's copy lists staging while leaving the default on current.
			// (`nixos-rebuild boot --profile-name staging` runs the STAGED system's copy and
			// would hand the default over -- the exact thing this must not do.) Nothing on
			// disk selects staging; only the host's SMBIOS flag at launch does.
			if err := run(currentSystem+"/bin/switch-to-configuration", "boot"); err != nil {
				return nil, err
			}
			// Flush it. The next thing that happens to this guest is a shutdown, and the
			// staging entry is worthless if it is still in page cache when the VM stops.
			return nil, run("sync")
		case verbOSGC:
			// Delete old generations of every profile, then collect. Both halves matter and
			// the first is the one that does the work: each /nix/var/nix/profiles/system-N-link
			// is itself a gcroot pinning a whole closure, so a bare `nix-collect-garbage`
			// frees nothing however many have piled up. -d does the generation sweep first,
			// recursing into system-profiles/ so the reboot path's `staging` profile is
			// included (nix-collect-garbage.cc's removeOldGenerations walks subdirectories).
			//
			// It never deletes what the node might need: nix keeps each profile's CURRENT
			// generation (profiles.cc deleteOldGenerations skips curGen), and NixOS roots
			// current-system + booted-system. os.hold covers the one case those miss.
			return nil, run("nix-collect-garbage", "-d")
		case verbOSPowerOff:
			// The FIRST-CHOICE clean shutdown: ask the guest OS directly, over the channel
			// the host already has, instead of rattling its virtual power button and hoping
			// something inside is listening. QMP's ACPI path (platform.Guest.Shutdown) stays
			// as the fallback for a guest whose agent is gone -- the two fail independently,
			// which is the whole reason to have both.
			//
			// --no-block because the reply must be written BEFORE systemd starts tearing the
			// machine down: without it the shutdown races the response and the host sees a
			// dead channel, which is indistinguishable from a guest that crashed. The host
			// confirms the outcome by watching the VM disappear, not by this return value
			// (platform.Guest.WaitStopped).
			return nil, run("systemctl", "poweroff", "--no-block")
		case verbResources:
			var req resourcesRequest
			if err := json.Unmarshal(payload, &req); err != nil {
				return nil, err
			}
			// Best-effort: telemetry is a signal, never a gate, so a failed sub-read leaves
			// its field zero rather than failing the verb (which would break the host's
			// observe loop). The struct always comes back; nil error.
			return gatherResources(ctx, x, req), nil
		case verbReactorActive:
			// Interim guard: lets a bracket user refuse to start when the promoter is ALREADY
			// paused — i.e. when someone else is mid-operation. It cannot prevent the race (a
			// pause can still land between this check and ours) and is not claimed to; it turns
			// the likely overlap from silent corruption of the bracket into a loud refusal.
			out, err := x.Run(ctx, "systemctl", "is-active", "drbd-reactor.service")
			if err != nil {
				// Is-active exits non-zero for every not-active state, which is an ANSWER
				// ("paused"), not a failure. Reporting an error here would make a correctly
				// paused node look broken.
				return false, nil
			}
			return strings.TrimSpace(string(out)) == "active", nil
		case verbReactorEvict:
			// A PLANNED HANDOVER: give the work to a peer while perfectly
			// healthy, so this node can reboot into a new generation without taking the house
			// down with it. drbd-reactor's own `evict` runtime-masks the promoter target, stops
			// it so a peer promotes, and unmasks -- writing our own stop-and-hope against the
			// promoter would reimplement that with less knowledge of its state machine.
			//
			// PROVEN, not assumed (nixosTest/reactor-evict.nix): the work moves in ~6s, the data
			// comes with it, and the same CLI's `disable` was deferred in this file as flaky --
			// which is why `evict` got a mechanism test before anything sequenced it.
			//
			// ⚠️ IT SAYS "NOT ME", NOT "YOU". The destination is drbd-reactor's election, not
			// ours; on a flock with three diskful anchors the work may land on the third node.
			// Our shipped topology (2 anchors + a diskless witness, which cannot hold the
			// resource) makes it deterministic -- but that is a property of the topology, so a
			// caller that needs the work to land somewhere specific must ASSERT where it went.
			//
			// KeepMasked is for the reboot path: without it the node is immediately eligible
			// again, so an ex-primary rebooting for its own upgrade could take the work back
			// before anyone has verified its new generation. The mask is `--runtime` and so does
			// NOT survive the reboot -- carrying it across is the sequencer's problem, not this
			// verb's. Unmask releases it (the deliberate hand-back) and runs no eviction.
			var req evictRequest
			if err := json.Unmarshal(payload, &req); err != nil {
				return nil, err
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
			// Flush the replicated volume's dirty pages BEFORE an eviction, so the unmount's
			// writeback — which runs inside the demote path, under the peer's promotion and
			// DRBD's ping deadline — moves only the last seconds of writes instead of
			// everything since the last natural writeback. Protocol C acks a write once it is
			// on both disks, but dirty PAGE CACHE is on no disk yet: for a write-heavy payload
			// the unmount flush is unbounded, and this verb is the pre-copy that bounds it.
			//
			// `stat -c %m` names the mount holding the path; equal to the path itself means the
			// data volume is mounted here (a Primary). Anything else — a Secondary or witness
			// that never mounted it — has nothing of ours to flush, which is an ANSWER, not an
			// error: the caller asked "make sure your dirty data is small", and it already is.
			out, err := x.Run(ctx, "stat", "-c", "%m", dataMountRoot)
			if err != nil || strings.TrimSpace(string(out)) != dataMountRoot {
				return "skipped: data volume not mounted here", nil
			}
			if err := run("sync", "-f", dataMountRoot); err != nil {
				return nil, err
			}
			return "synced", nil
		case verbReactorPause:
			// Pause the promoter by stopping the drbd-reactor daemon: stop-services-on-exit
			// defaults false, so the promoted services + DRBD Primary stay up while it is
			// down -- deterministic maintenance mode. (v0 is single-resource; per-snippet
			// `drbd-reactorctl disable` is deferred -- its systemctl reload proved flaky.
			// The snippet arg is reserved for that future per-resource path.)
			//
			// A BARE STOP, and the promote-vs-stop deadlock it used to have to dodge is now
			// defused by the unit itself. This verb once removed drbd-reactor's own
			// `Before=drbd-reactor.service` override (reactor-50-before.conf) and reloaded
			// first, because a stopping reactor fires one last `systemctl start
			// drbd-services@r0.target` that the override sequences BEHIND this very stop ->
			// neither completes -> 90s TimeoutStopSec SIGKILL. That defusal moved onto
			// drbd-reactor.service's ExecStop ([B.85], guest-image/configuration.nix), because
			// the same deadlock hangs every OTHER stop too -- a shutdown, the deadman's reboot,
			// a host reboot -- none of which come through here.
			//
			// So the steps are gone rather than kept as belt-and-braces: this verb and that unit
			// ship in the same closure (the guest agent is built INTO the guest image,
			// disk-image.nix), so there is no version in which one is present without the other,
			// and duplicating it only buys a second place to have to keep correct. Measured
			// after the move: a bare stop of a promoted reactor completes in 401ms
			// (nixosTest/reactor-pause-deadlock.nix, which now drives exactly this line).
			return nil, run("systemctl", "stop", "drbd-reactor.service")
		case verbReactorResume:
			// Restart the daemon; it re-reads config and adopts the already-Primary services,
			// with no restart/demote. (No maintenance marker to clear -- briard-converge is a switch-free gate, so
			// nothing autonomous races a managed op.)
			return nil, run("systemctl", "start", "drbd-reactor.service")
		case verbNetConfigure:
			var req netConfigureRequest
			if err := json.Unmarshal(payload, &req); err != nil {
				return nil, err
			}
			// Address the system NIC: this node's NODE IP, which is how anything reaches it
			// (DESIGN §4) and where DRBD binds. `addr replace` is idempotent (no-op if already
			// set), then ensure the link is up. Both take an explicit `dev` (portable across
			// iproute2 versions). An empty Dev/CIDR still skips addressing -- the agent-less
			// harnesses send that, and a witness has no VIP device either.
			if req.Dev != "" && req.CIDR != "" {
				if err := run("ip", "addr", "replace", req.CIDR, "dev", req.Dev); err != nil {
					return nil, err
				}
				// PRUNE any OTHER global v4 address on this NIC. `addr replace` adds and updates
				// but never removes, so a RENUMBER would leave the old address alongside the new
				// one -- and a renumber is exactly what an adoption does to the joiner, whose
				// island subnet gives way to the adopter's (DESIGN §1.2). Two addresses on the
				// DRBD NIC is two plausible sources for it to bind, which is the ambiguity
				// [B.101] spent a fork's worth of debugging on at the ARP layer.
				//
				// Add first, prune second, never flush-then-add: the flush form leaves the NIC
				// momentarily addressless, and this call runs on a node that may have a peer
				// connected. Nothing here could bite until [V3b.26b] gave a lone node an
				// install-time address; before that there was never an old one to leave behind.
				if out, err := x.Run(ctx, "ip", "-o", "-4", "addr", "show", "dev", req.Dev, "scope", "global"); err == nil {
					for _, held := range allCIDRs(string(out)) {
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
			// The route back to our own host, over the private link, plus a PERMANENT neighbour
			// entry for the host's end of it. Both halves are load-bearing.
			//
			// The route is a /32 so it beats the on-link /24 the system address above installs:
			// under macvtap that on-link path leaves by eth1 towards a host the substrate isolates
			// us from, so every reply to a host-originated packet would go out the wrong door.
			//
			// ⚠️ THE NEIGHBOUR ENTRY IS WHY THIS LINK CAN BE UNNUMBERED AT ALL, and it was found
			// by capture, not by reasoning ([V3b.26b]). This NIC has no address, so when the
			// kernel needs to ARP for the host it has no source address on the outgoing interface
			// to put in the request -- and arp_announce=2 then makes it BORROW one from another
			// NIC. What it borrows is eth0's slirp address, 10.0.2.15, which is also what every
			// other qemu user-net picks: the request goes out as "who-has <host> tell 10.0.2.15",
			// the host sees an ARP request apparently from its own address, and never answers.
			// Measured on the wire in agent-deadman. Pinning the MAC means we never ask.
			//
			// So the link is symmetric: neither end can ARP usefully across an unnumbered wire, so
			// both ends pin. The host's mirror is platform.SetNodeRoute, and it pins for a
			// different reason -- our node IP lives on eth1 while its request would arrive on this
			// NIC, which arp_ignore=1 refuses ([B.101]).
			if req.PrivDev != "" && req.PrivHostIP != "" {
				if err := run("ip", "link", "set", "dev", req.PrivDev, "up"); err != nil {
					return nil, err
				}
				// This end's own address on the link. It is SUBSTRATE and nothing addresses it --
				// the gate answers at the node IP, the forwarder is dialled at the node IP, the
				// VIP is routed via the node IP. It exists for one reason, measured: avahi joins
				// the IPv4 mDNS group on an interface only if that interface HAS a v4 address, so
				// without it this NIC answers mDNS over IPv6 only -- and the host's end of that
				// conversation is a stranger's machine, which may have IPv6 off. [V3b.19]'s name
				// half then breaks silently: the household's own machine cannot find its own node
				// while everything else works. install-macvtap runs with v6 disabled on the host
				// precisely so nothing can pass for a reason we do not control.
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
			// THE SERVICE IDENTITY, MADE HERE. Under the bridge substrate the host hands us one
			// NIC, so the second MAC this node must present -- the flock-scoped one the VIP rides
			// and a failover MOVES -- has to be created inside the guest as a macvlan child
			// ([V3b.26c]). Under macvtap this block does nothing: the host built the device and
			// VIPParent is empty.
			//
			// `mode bridge` so the child and its parent can talk to each other as well as to the
			// LAN. The MAC is set AT CREATION rather than after, so the device never exists
			// holding a random one -- a macvlan comes up with a kernel-random MAC, and a frame
			// emitted in that window teaches the switch a port for an address nobody owns.
			//
			// ⚠️ CREATED DOWN, AND LEFT DOWN. `ip link add` leaves a device administratively down
			// and that is exactly the standby discipline: briard-vip.service brings VIP_DEV up on
			// promotion and takes it down on demotion, because a Secondary holding the flock MAC
			// up teaches the switch the wrong port for the VIP the moment it emits anything at
			// all ([B.100]/[B.101]). Bringing it up here would hand every standby that defect.
			if req.VIPParent != "" && req.VIPDev != "" {
				// The parent, up. Usually redundant with the system-NIC block above -- VIPParent IS
				// req.Dev in the shipped shape -- but this block must stand on its own: a caller may
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
					// It already exists -- re-assert the MAC rather than assume it. The flock MAC
					// is FLOCK-scoped, so an adoption changes it under a device that outlives the
					// change ([V3b.26b]), and this call is the one that would otherwise leave the
					// joiner presenting its old flock's identity.
					if err := run("ip", "link", "set", "dev", req.VIPDev, "address", req.VIPMAC); err != nil {
						return nil, err
					}
				}
			}
			// The VIP's device AND address are agent-determined: record them where
			// briard-vip.service reads them, which since [V3b.16a] is a REQUIRED EnvironmentFile
			// with nothing baked behind it. Idempotent (whole-file write).
			//
			// An unset VIP_ADDR is the ordinary DHCP case and the unit expects it. An unset
			// VIP_DEV now means the node claims no VIP at all -- a witness, which has no promoter
			// and never starts briard-vip. A data node reaching promotion without one is a broken
			// configuration and fails loudly there, rather than guessing a NIC ([V3b.16] guessed
			// the replication NIC and took a second DHCP lease on it).
			var env []byte
			if req.VIPDev != "" {
				env = append(env, "VIP_DEV="+req.VIPDev+"\n"...)
			}
			// SYSTEM_DEV beside it, and it is not decoration: the VIP unit's STOP path needs to
			// know whether VIP_DEV is a dedicated service NIC or the DRBD NIC. A standby must bring
			// a dedicated one DOWN -- the flock MAC is shared with the peer, and a Secondary
			// emitting from it teaches the switch the wrong port ([B.100]/[B.101]) -- while a
			// link-down on the DRBD NIC would take replication with it. Only the agent knows which
			// is which; the guest bakes no positional knowledge ([V3b.16a]). See the ⚠️ on vipDown
			// in guest-image/configuration.nix for the proxy this replaced and why it was false.
			//
			// Inside the VIPDev branch on purpose: a node with no VIP device is a WITNESS, which
			// has no promoter and never starts briard-vip, and writing this file for it would
			// hand a unit that must not run a REQUIRED EnvironmentFile it now satisfies.
			if req.VIPDev != "" && req.Dev != "" {
				env = append(env, "SYSTEM_DEV="+req.Dev+"\n"...)
			}
			if req.VIPAddr != "" {
				env = append(env, "VIP_ADDR="+req.VIPAddr+"\n"...)
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
			// `scope global` excludes link-local, so a device that is up but unaddressed reads as
			// "" rather than as an fe80:: the caller would have to know to discard. A non-zero exit
			// means the device is absent; that is "no address", not a failure to report one --
			// the host asks this every cycle and a transient absence must not read as a dead channel.
			out, err := x.Run(ctx, "ip", "-o", "-4", "addr", "show", "dev", req.Dev, "scope", "global")
			if err != nil {
				return "", nil
			}
			return firstCIDR(string(out)), nil
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
			// try-restart, NOT restart: republish only where a name is already published. A
			// Secondary holds no VIP, so briard-mdns is stopped there (it is partOf briard-vip)
			// and starting it would publish a name for an address this node does not hold.
			//
			// The rename applies WITHOUT touching addressing -- no VIP re-assert, no MAC, no DHCP
			// client-id, no DRBD state. That is the property the three-way identifier split was
			// for, and this line is where it is cashed in.
			return nil, run("systemctl", "try-restart", mdnsUnit)
		case verbNetMDNSPublished:
			// The name avahi ESTABLISHED, which is not always the name we asked for: on a
			// collision it conflict-renames to `<name>-2` and tells nobody. Reporting the
			// requested name here would rebuild V3.19 -- a name present, plausible, and not what
			// anyone thinks it is -- inside the item that exists to end that.
			out, err := x.ReadFile(mdnsPublishedPath)
			if err != nil {
				// Absent is the normal answer on a Secondary (nothing published), so it is "" and
				// not an error. Any OTHER read failure is also "": the host asks every cycle, and
				// a transient unreadable file must not read as a dead channel.
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
	return serve(ctx, conn, dispatch(x))
}

// ContactStampPath is the last-seen-host-agent stamp: the guest agent bumps its mtime on every
// served request, and the SEPARATE briard-deadman process (RunDeadman) reads that mtime to tell
// how long the host has been silent. It lives on tmpfs (/run) so it re-baselines cleanly each
// boot — the deadman disarms until the host talks once (StampMtime → zero → not armed). It is a
// stamp file, NOT in-process state, precisely because the per-connection guest agent crash-loops
// while the host is down (the reopened virtio-serial port EOFs), which would reset any in-process
// timer before it could fire.
const ContactStampPath = "/run/briard/.host-contact"

// ServeStamped serves the dispatch loop and bumps the contact stamp on every request (the
// production runGuest entry). The deadman itself runs in its own process (RunDeadman) — decoupled
// from this connection lifecycle, so a crash-looping agent can't reset it. Plain Serve stays for
// tests (no stamp side effect).
func ServeStamped(ctx context.Context, conn io.ReadWriteCloser, x Executor) error {
	d := dispatch(x)
	hooked := func(ctx context.Context, verb string, payload json.RawMessage) (any, error) {
		touchStamp(ContactStampPath) // the host agent just talked to us — freshen the deadman's stamp
		return d(ctx, verb, payload)
	}
	return serve(ctx, conn, hooked)
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

// deadmanStatePath holds the deadman's backoff episode — a NODE-LOCAL path (a sibling of the DRBD
// mount at /var/lib/briard, NOT on the replicated volume), so it survives a reboot without being
// shared/overwritten across nodes.
const deadmanStatePath = "/var/lib/briard-deadman/episode.json"

// RunDeadman runs the host-agent deadman as its OWN long-running process — the briard-deadman
// guest service. It watches the contact stamp ServeStamped bumps and, once the host agent has
// been silent past T_deadman, reboots the guest — gracefully (so the promoter teardown demotes
// cleanly and the reboot IS the failover trigger), and ONLY when deadman.RebootAllowed says no
// peer loses quorum over it. It is a separate process from the guest agent precisely so a
// crash-looping agent (while the host is down) can't reset the timer.
// BRIARD_DEADMAN[/_JITTER/_TICK] tune the threshold.
//
// It also SERVES that same gate on the private host<->guest link, so the host's own rung can ask
// the one question it cannot answer from outside (gate.go). The listener is started only when the
// link has an address, and a bind failure is logged rather than fatal: losing the gate costs the
// host its guard, while exiting here would cost the node its reflex entirely.
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
			out, err := x.Run(ctx, "drbdsetup", "status", "--json")
			if err != nil {
				return deadman.Fabric{}, err
			}
			peers, connected, quorate, err := drbd.PeerCounts(out)
			return deadman.Fabric{Peers: peers, Connected: connected, Quorate: quorate}, err
		},
		Reboot: func(ctx context.Context) error {
			out, err := x.Run(ctx, "systemctl", "reboot") // GRACEFUL — never `reboot -f`
			if err != nil {
				return fmt.Errorf("systemctl reboot: %w: %s", err, bytes.TrimSpace(out))
			}
			return nil
		},
		// The deadman's owner-facing channel, and the ONLY alert on the node that is not written
		// by the host agent -- so it is also the one most easily missed. It goes to this process's
		// stderr, which systemd puts in the GUEST's journal, which the guest's ttyS0 console
		// carries out to the host's /var/log/briard-guest-console.log. Under macvtap that file is
		// the sole witness to anything inside the VM (install.sh), and it shares no logger with
		// the host agent -- so `briard alerts` reads both surfaces and cannot merely tail one.
		//
		// The "alert [<level>] " prefix is notify.LogLine's shape, hand-written because the guest
		// binary must not link shared/notify (see deadman.LevelWarning). It is what makes this
		// line findable amid the kernel and systemd traffic sharing the console.
		Alert: func(level, msg string) { fmt.Fprintf(os.Stderr, "briard-deadman: alert [%s] %s\n", level, msg) },
		Logf:  func(f string, a ...any) { fmt.Fprintf(os.Stderr, "briard-deadman: "+f+"\n", a...) },
		State: deadman.FileState{Path: deadmanStatePath},
	}
	return mon.Run(ctx)
}

// reactorPath is where the agent drops drbd-reactor's promoter snippet. TMPFS since [V3b.16b]:
// the host re-derives it from cfg.Promoter at every bring-up, so a persistent copy could only ever
// be a stale one that outlived the agent that wrote it -- and a reactor with no snippet is idle,
// which is a second, independent backstop to [V3b.16a]'s gate.
//
// PAIRED with guest-image/configuration.nix's reactorSnippetDir, which points drbd-reactor.toml's
// `snippets` at this directory. Different languages, so no shared import.
const reactorPath = "/run/briard/drbd-reactor.d/briard.toml"

// (The drop-in drbd-reactor writes over its own promoter target --
// /run/systemd/system/drbd-services@r0.target.d/reactor-50-before.conf -- was named here while
// reactor.pause removed it by hand. That defusal is drbd-reactor.service's ExecStop now
// ([B.85], guest-image/configuration.nix), so the path belongs to the unit that acts on it and
// nothing in Go needs to know it.)

// vipEnvPath is the REQUIRED EnvironmentFile briard-vip.service reads its VIP_DEV and VIP_ADDR
// from; the agent writes it via net.configure at every bring-up. Nothing is baked behind it
// ([V3b.16a]) -- which is safe only because the promoter that starts briard-vip is itself
// agent-started, so nothing can read this file before bring-up has written it.
const vipEnvPath = "/run/briard/vip.env"

const (
	// mdnsEnvPath is the EnvironmentFile briard-mdns.service reads the flock's visible name from.
	// Written by net.mdnsname, never baked: it is PET identity reaching a CATTLE image, and baking
	// identity into a shared image is the mistake V3.19 was.
	mdnsEnvPath = "/run/briard/mdns.env"
	// mdnsPublishedPath is where briard-mdns records the name avahi actually ESTABLISHED, parsed
	// from avahi-publish's own output. Absent means nothing is published -- the normal state of a
	// Secondary, which holds no VIP and therefore publishes no name.
	mdnsPublishedPath = "/run/briard/mdns.published"
	// mdnsUnit is restarted to republish after a rename. try-restart, so a node that is not
	// serving stays quiet: there is no name to correct where no name is published.
	mdnsUnit = "briard-mdns.service"
)

// resDir is where the agent drops the DRBD resource config. TMPFS, and it is the LAST of the four
// node-scoped facts to get there ([V3b.16b]) because it was the only one the host could not
// re-derive: applyPair used to apply the cloud's mesh and forget it, so an ephemeral `.res` would
// have returned a runtime-paired node un-meshed on every reboot -- what RescueGuest refuses to do.
// The mesh cache (agent/host's cacheMesh) is what earned this move: the host durably owns the mesh
// it writes now, so the guest's copy can have the one lifetime everything else here has.
//
// With this there is nothing node-scoped left on the guest's overlay, so a rebooted guest cannot
// act on configuration nobody has just restated -- the promoter is gated ([V3b.16a]) and every
// input is re-pushed at bring-up.
//
// PAIRED with guest-image/disk-image.nix's `include` glob in /etc/drbd.conf: drbdadm looks for that
// one file at a path we do not choose, and it is what points at this directory.
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
		return fmt.Errorf("service: unsafe unit filename %q", n)
	}
	return nil
}

// renderService materialises the host's quadlet source under quadletDir and reloads systemd —
// the moment podman's generator turns those files into real units, which is what moves unit
// generation from build time to run time.
//
// Node-local by design: quadletDir is on /run, so this must run on EVERY node, not just the
// Primary. A survivor can start the pod only because the units were rendered there too.
func renderService(ctx context.Context, x Executor, run func(string, ...string) error, req serviceRenderRequest) error {
	if len(req.Files) == 0 {
		return fmt.Errorf("service.render: no files to write")
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
	// Remove the outgoing service's units first: swapping which service occupies the slot must
	// not leave a stale .container behind for the promoter to trip over. Absent is fine.
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

// provisionService creates the service's single btrfs subvolume with plain per-container
// subdirectories, and records the manifest as the service's identity.
//
// Primary-only: everything it touches is on the replicated volume, which is mounted nowhere else.
// Idempotent — a re-install onto an existing service must keep its data, so an existing subvolume
// is reused rather than re-created (re-creating it would silently delete the user's state).
func provisionService(ctx context.Context, x Executor, run func(string, ...string) error, req serviceProvisionRequest) error {
	if req.Name == "" || req.DataDir == "" || req.Manifest == "" {
		return fmt.Errorf("service.provision: need a name, a data dir and a manifest")
	}
	if err := safeUnitName(req.Name); err != nil { // the name becomes a path element
		return err
	}
	if out, err := x.Run(ctx, "btrfs", "subvolume", "show", req.DataDir); err != nil || len(out) == 0 {
		if err := run("btrfs", "subvolume", "create", req.DataDir); err != nil {
			return err
		}
	}
	for _, d := range req.Subdirs {
		if err := safeUnitName(d); err != nil { // same escape check; a subdir is a bare name
			return err
		}
		if err := run("mkdir", "-p", req.DataDir+"/"+d); err != nil {
			return err
		}
	}
	if err := run("mkdir", "-p", manifestDir); err != nil {
		return err
	}
	pin := manifestPath(req.Name)
	if err := x.WriteFile(pin, []byte(req.Manifest)); err != nil {
		return err
	}
	// Flush to the DRBD backing so the identity actually replicates BEFORE a failover relies on
	// it — protocol C acks a device write only once the peer holds it. Without this, a crash
	// inside the btrfs writeback window loses the manifest and a survivor promotes with no idea
	// what it is meant to be running. Same reasoning as payload.pin's sync.
	_, err := x.Run(ctx, "sync", "-f", pin)
	return err
}

// gatherResources measures the appliance's resource footprint for the soak trend oracle
// . Every sub-metric is best-effort: a failed read leaves its field zero (the host
// reads 0 as "unread this cycle"), so a hiccup in one probe never sinks the whole report or
// the observe loop. It fills only the appliance (guest) fields; the host adds its own
// Agent* footprint on top. Zero DataDir/Unit (a witness) simply skips the volume/payload
// metrics.
func gatherResources(ctx context.Context, x Executor, req resourcesRequest) telemetry.NodeResources {
	var r telemetry.NodeResources
	// One entry per service, in the order the host sent them -- so the oracle can name WHICH
	// service leaked or crash-looped. A service whose unit is unnamed is skipped rather than
	// measured as zero: an unmeasured service and an idle one must not read the same.
	for _, s := range req.Services {
		if s.Unit == "" {
			continue
		}
		sr := telemetry.ServiceResources{Name: s.Name}
		// The service's memory is the UNIT'S CGROUP, not its main process. A service runs as a
		// container, so the unit's MainPID is podman's supervisor and the workload lives in a child
		// cgroup: /proc/<MainPID>/status reported ~2.5 MB for a Home Assistant, a number that is
		// stable, believable-looking and about a hundredfold wrong. cgroup v2 accounts
		// hierarchically, so reading the unit's cgroup catches the container's processes too.
		sr.RSSKB = cgroupAnonKB(ctx, x, s.Unit)
		// MainPID=0 means the unit isn't running; skip rather than read /proc/0.
		if out, err := x.Run(ctx, "systemctl", "show", "-p", "MainPID", "--value", s.Unit); err == nil {
			if pid := strings.TrimSpace(string(out)); pid != "" && pid != "0" {
				// NOTE: this is the SUPERVISING process's fd count, not the workload's -- there is
				// no cgroup fd counter to read, and walking every pid in the cgroup each cycle is
				// a lot of exec for a signal that mostly catches conmon/podman leaks. An fd leak
				// INSIDE the container is not visible here.
				if fds, err := x.Run(ctx, "ls", "/proc/"+pid+"/fd"); err == nil {
					sr.FDs = countLines(fds)
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
		if out, err := x.Run(ctx, "btrfs", "subvolume", "list", "-s", req.DataDir); err == nil {
			r.SnapshotCount = countLines(out)
		}
	}
	// -x: stay on one filesystem, so the number is the surface itself, not whatever is
	// mounted beneath it.
	if out, err := x.Run(ctx, "du", "-skx", journalDir); err == nil {
		r.LogSizeKB = parseDuKB(out)
	}
	if out, err := x.Run(ctx, "du", "-skx", containerStore); err == nil {
		r.PodmanStoreKB = parseDuKB(out)
	}
	// The guest kernel log (no-bad-kernel-log): where the guest-internal DRBD/btrfs/OOM signals
	// land, unreachable from the L1 container (which shares the host kernel). Report only lines
	// NEW since the last poll (a journal cursor), so a transient error from a past cycle ages
	// out instead of re-failing every sample, and a fresh error is attributable to the current
	// cycle. Mechanism only: the guest returns the lines, the oracle scans + scopes them.
	r.KernelErrors = recentKernelErrors(ctx, x)
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
// trailing "-- cursor: <c>" marker (absent when no entries were shown, e.g. nothing new).
//
// journalctl speaks in its OWN voice too -- "-- No entries --", "-- Reboot --", "-- Journal begins
// at ... --" -- and none of those are log lines. Dropping only the cursor left "-- No entries --"
// counted as one kernel error on every healthy poll, so kerr sat at a permanent 1: worse than
// noise, because this is the signal the soak oracle scans and a constant 1 hides the step up to a
// real error. A journal line is dated ("Aug 06 ... guest kernel: ..."); nothing real is bracketed
// in "-- ... --".
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

// probeHTTPOK does a minimal plaintext HTTP/1.0 GET of rawURL and reports whether the response
// status is 200. It is deliberately net/http-free (raw net.Dial + a hand-written request) so the
// `-tags guest` binary stays clear of net/http and the TLS stack that trim removed. Plaintext
// is all the local /healthz needs. Any parse/dial/read error, non-http scheme, or non-200 status is
// "not healthy" — the same fail-closed semantics as the old host-side probe.
func probeHTTPOK(ctx context.Context, rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "http" || u.Host == "" {
		return false
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
	}
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

// cgroupAnonKB is the payload's resident memory: the ANON bytes of the unit's cgroup, in KB.
//
// Two deliberate choices. The CGROUP rather than the main process, because a containerised
// service's workload is not the unit's MainPID (that is podman's supervisor) -- cgroup v2 accounts
// hierarchically, so the unit's own file sums its container children. And ANON rather than
// memory.current, because memory.current includes the PAGE CACHE: a service writing a database
// would show steadily climbing "memory" that is really just cache the kernel will drop under
// pressure, and payload_rss_kb carries a SLOPE check in the trend oracle -- cache growth would
// read as a leak and fail a soak run for a healthy node.
//
// 0 on anything unreadable (no cgroup, cgroup v1, accounting off), which the host already treats
// as "unread this cycle" -- never a fallback to a different quantity, because a series that
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

func systemReq(payload json.RawMessage) (systemRequest, error) {
	var req systemRequest
	err := json.Unmarshal(payload, &req)
	return req, err
}

// Client is the host end: typed calls to the guest agent over the channel.
type Client struct {
	c       *conn
	version int             // negotiated guest protocol version (0 until Handshake)
	caps    map[string]bool // verbs the guest advertised (nil until Handshake)
	bootID  string          // which BOOT of the guest answered (empty until Handshake, or from a guest too old to say)
}

// NewClient wraps a connection to the guest (virtio-serial in prod, net.Pipe in tests).
func NewClient(rw io.ReadWriteCloser) *Client { return &Client{c: newConn(rw)} }

// Handshake negotiates the host<->guest protocol: it reads the guest's version +
// capabilities and refuses a guest the host can't drive (version outside
// [MinGuestProtocol, GuestProtocol]) -- a safe deferral, since bring-up/upgrade then
// fails rather than the host sending verbs a skewed guest might misinterpret. On success
// the version + capabilities are recorded (see ProtocolVersion / Supports). The host
// should call this once, right after connecting (and after any reconnect).
func (g *Client) Handshake(ctx context.Context) (api.GuestHello, error) {
	var h api.GuestHello
	// Resync=true: on a reconnect, a stale in-flight reply from the dropped session can sit
	// ahead of ours in the still-open virtio-serial stream -- skip it, don't fail.
	if err := g.c.callResync(ctx, verbHello, nil, &h, true); err != nil {
		return h, fmt.Errorf("guestagent: handshake: %w", err)
	}
	if !compatibleGuest(h.Version) {
		return h, fmt.Errorf("guestagent: incompatible guest protocol v%d (host drives v%d..v%d)",
			h.Version, api.MinGuestProtocol, api.GuestProtocol)
	}
	g.version = h.Version
	g.bootID = h.BootID
	g.caps = make(map[string]bool, len(h.Capabilities))
	for _, c := range h.Capabilities {
		g.caps[c] = true
	}
	return h, nil
}

// compatibleGuest reports whether the host can drive a guest speaking protocol v. Newer
// than the host knows, or older than it still supports, is refused.
func compatibleGuest(v int) bool {
	return v >= api.MinGuestProtocol && v <= api.GuestProtocol
}

// Supports reports whether the guest advertised verb in its handshake. Before a handshake
// (caps nil) it returns true -- optimistic, preserving the older "just try the verb"
// behaviour for callers that don't negotiate.
func (g *Client) Supports(verb string) bool {
	if g.caps == nil {
		return true
	}
	return g.caps[verb]
}

// ProtocolVersion is the negotiated guest protocol version (0 before a handshake).
func (g *Client) ProtocolVersion() int { return g.version }

// BootID identifies the guest BOOT this channel reached, from the handshake. Empty before a
// handshake, and empty from a guest too old to report one -- so a caller comparing two of them
// must treat an empty side as no evidence rather than as a difference ([B.102]).
func (g *Client) BootID() string { return g.bootID }

// SetHostname renames the guest to this node's name so DRBD's `on <name>` matching
// works (see verbSetHostname). Idempotent.
func (g *Client) SetHostname(ctx context.Context, name string) error {
	return g.c.call(ctx, verbSetHostname, hostnameRequest{Name: name}, nil)
}

// ConfigureNet sets a static CIDR on a guest NIC (the system/DRBD NIC) and brings
// it up, so DRBD can use the private subnet. vipDev, when non-empty, records the NIC
// the promoter should claim the VIP on (the two-subnet layout moves it to eth2);
// "" leaves the guest's baked default. vipAddr does the same for the address itself, in
// CIDR form -- the LAN decides it, so it cannot be baked. Idempotent.
func (g *Client) ConfigureNet(ctx context.Context, n NetConfig) error {
	return g.c.call(ctx, verbNetConfigure, netConfigureRequest{
		Dev: n.Dev, CIDR: n.CIDR, VIPDev: n.VIPDev, VIPAddr: n.VIPAddr,
		PrivDev: n.PrivDev, PrivCIDR: n.PrivCIDR,
		PrivHostIP: n.PrivHostIP, PrivHostMAC: n.PrivHostMAC,
		VIPParent: n.VIPParent, VIPMAC: n.VIPMAC,
	}, nil)
}

// SetMDNSName records the flock's visible name and republishes it if this node is serving.
// name is BARE ("brave-elf"); the guest builds `briard-<name>.local` from it. "" publishes
// nothing. Idempotent, and deliberately independent of ConfigureNet -- a rename must never be an
// addressing call.
func (g *Client) SetMDNSName(ctx context.Context, name string) error {
	return g.c.call(ctx, verbNetMDNSName, mdnsNameRequest{Name: name}, nil)
}

// MDNSPublished reports the name avahi actually established, bare and without the `.local`
// suffix, or "" when this node publishes none (a Secondary, or a publish still in flight).
//
// Read it rather than assume it: avahi conflict-renames on a collision without telling anyone, so
// the requested name and the published name can differ, and which flock gets the plain name
// depends on who probed first. See verbNetMDNSPublished.
func (g *Client) MDNSPublished(ctx context.Context) (string, error) {
	var name string
	if err := g.c.call(ctx, verbNetMDNSPublished, struct{}{}, &name); err != nil {
		return "", err
	}
	return name, nil
}

// VIP reports the address dev actually holds, in CIDR form, or "" when it holds none -- this node
// is Secondary, or its promotion has not claimed the address yet. Under DHCP this is how the host
// learns an address it no longer chooses. An unaddressed device is "" rather than an error,
// because the host asks every cycle and a Secondary answering "" is the normal case, not a fault.
func (g *Client) VIP(ctx context.Context, dev string) (string, error) {
	var cidr string
	if err := g.c.call(ctx, verbNetVIP, netVIPRequest{Dev: dev}, &cidr); err != nil {
		return "", err
	}
	return cidr, nil
}

// Provision drops the rendered DRBD + reactor configs and create-md's the resource -- but only
// if it has no valid metadata yet: it returns whether it created fresh metadata, so the
// caller declares UpToDate only on a true first init (never on a reboot, which would wipe the
// persisted replica). ReactorConfig is dropped regardless (idempotent).
func (g *Client) Provision(ctx context.Context, req ProvisionRequest) (ProvisionResult, error) {
	var res ProvisionResult
	err := g.c.call(ctx, verbProvision, req, &res)
	return res, err
}

// Up starts drbd@<res>.target (attach + connect; leaves the node Secondary).
func (g *Client) Up(ctx context.Context, resource string) error {
	return g.c.call(ctx, verbUp, resourceRequest{Resource: resource}, nil)
}

// Adjust rewrites the resource config and runs `drbdadm adjust` -- apply a peer-set change to the
// already-running resource without a restart (runtime mesh growth). Called on the serving
// primary to add a joining anchor/witness; the joining nodes come up via Provision+Up
// (FreshInit=false) and resync. No create-md here, so the primary's disk is never touched.
func (g *Client) Adjust(ctx context.Context, req ProvisionRequest) error {
	return g.c.call(ctx, verbAdjust, req, nil)
}

// InitUpToDate declares a brand-new resource's data current (skips the initial
// sync). One-time, first-init only -- never on an existing resource.
func (g *Client) InitUpToDate(ctx context.Context, resource string) error {
	return g.c.call(ctx, verbInitUUID, resourceRequest{Resource: resource}, nil)
}

// ReactorStart starts drbd-reactor, which then drives promotion (not us).
func (g *Client) ReactorStart(ctx context.Context, resource string) error {
	return g.c.call(ctx, verbReactor, resourceRequest{Resource: resource}, nil)
}

// Status reads the guest's DRBD/quorum ground truth into a QuorumState — the
// summary the node reports up (shared/api's closed allowlist).
func (g *Client) Status(ctx context.Context, resource string) (model.QuorumState, error) {
	c, err := g.Cluster(ctx, resource)
	return c.QuorumState, err
}

// Cluster reads the same sample whole: this node's QuorumState plus its peers'
// roles and disk states, the input to "could someone else take over?".
// Stays home — the peer half never rides the cloud wire.
func (g *Client) Cluster(ctx context.Context, resource string) (model.Cluster, error) {
	var c model.Cluster
	err := g.c.call(ctx, verbStatus, resourceRequest{Resource: resource}, &c)
	return c, err
}

// PayloadStart starts the payload's systemd unit inside the guest.
func (g *Client) PayloadStart(ctx context.Context, unit string) error {
	return g.c.call(ctx, verbPayloadStart, unitRequest{Unit: unit}, nil)
}

// ServiceRender writes the host-rendered quadlet source into the guest's /run and reloads
// systemd, so the generated units exist. Node-local: run this on EVERY node, or a
// survivor has nothing to start. Stale names the outgoing service's files, removed first.
func (g *Client) ServiceRender(ctx context.Context, files map[string]string, stale []string) error {
	return g.c.call(ctx, verbServiceRender, serviceRenderRequest{Files: files, Stale: stale}, nil)
}

// ServiceProvision creates the service's data subvolume + per-container subdirectories and
// records the manifest as its identity. PRIMARY ONLY — it all lives on the replicated volume.
// Idempotent: an existing subvolume is reused, never re-created, so a re-install keeps the data.
func (g *Client) ServiceProvision(ctx context.Context, name, dataDir string, subdirs []string, manifest string) error {
	req := serviceProvisionRequest{Name: name, DataDir: dataDir, Subdirs: subdirs, Manifest: manifest}
	return g.c.call(ctx, verbServiceProvision, req, nil)
}

// ServiceWarm ensures `ref` is present in the guest's image store, starting `unit` (its .image
// warm unit) ONLY if it is missing. The distinction is the whole point: starting the unit is a
// registry pull, so an unconditional warm makes every path that calls it require WAN.
func (g *Client) ServiceWarm(ctx context.Context, unit, ref string) error {
	return g.c.call(ctx, verbServiceWarm, serviceWarmRequest{Unit: unit, Ref: ref}, nil)
}

// ServiceConverge makes the guest match the volume, in place: every manifest under the replicated
// `.services/` rendered, warmed and started ([V3b.3](f), converge.go). PRIMARY ONLY in effect --
// the volume is mounted nowhere else, and a Secondary converges when it promotes, which is the
// whole design.
//
// It takes no arguments deliberately. A caller that could name what to converge TO would be the
// node-was-told model this replaces; the volume is the only input, and the install path's job is
// to have written it first.
//
// SupportsServiceConverge gates it: an older guest has no briard-services unit to converge, so a
// host must refuse rather than call a verb that guest cannot honour.
func (g *Client) ServiceConverge(ctx context.Context) error {
	return g.c.call(ctx, verbServiceConverge, struct{}{}, nil)
}

// SupportsServiceConverge reports whether the guest can converge itself to the volume.
func (g *Client) SupportsServiceConverge() bool { return g.Supports(verbServiceConverge) }

// ServiceForget removes one service's manifest from the replicated volume -- what a failed FRESH
// install must do, because under converge the volume is what every future promotion renders from.
// Idempotent: an absent manifest is the end state this asks for.
func (g *Client) ServiceForget(ctx context.Context, name string) error {
	return g.c.call(ctx, verbServiceForget, serviceInstalledRequest{Name: name}, nil)
}

// ServiceInstalled reads the manifest recorded on the replicated volume for ONE service, or ""
// when that service is not installed there. "" is a legitimate answer — the shipped zero-service
// node, and equally a node running other services — never an error.
//
// SupportsServiceInstalled is the gate the install path checks first: a guest too old to
// advertise this verb cannot tell one service's identity from another's, so it must be refused
// rather than driven into overwriting the manifest of whatever it already runs.
func (g *Client) ServiceInstalled(ctx context.Context, name string) (string, error) {
	var s string
	err := g.c.call(ctx, verbServiceInstalled, serviceInstalledRequest{Name: name}, &s)
	return s, err
}

// SupportsServiceInstalled reports whether the guest can name a service when reading or recording
// its identity on the volume — the per-service split of what used to be one file ([V3b.3](b)).
func (g *Client) SupportsServiceInstalled() bool { return g.Supports(verbServiceInstalled) }

// ServiceList names the services recorded on the replicated volume. Empty is a legitimate answer
// (the shipped zero-service node), never an error.
//
// It is how a node that never installed anything can still say what it runs: converge renders from
// the volume at promotion, so on a survivor the volume is the only place the truth exists.
func (g *Client) ServiceList(ctx context.Context) ([]string, error) {
	var names []string
	err := g.c.call(ctx, verbServiceList, nil, &names)
	return names, err
}

// SupportsServiceList reports whether the guest can name what the volume carries. A guest without
// it leaves a converged survivor reporting only what this host installed itself -- which is
// nothing, on a node that promoted into a service someone else installed.
func (g *Client) SupportsServiceList() bool { return g.Supports(verbServiceList) }

// Resources reads the appliance's resource telemetry -- per-service RSS/fds/restarts, load, and
// the disk sub-series -- for the soak's trend oracle. services pairs each service's name with the
// unit whose footprint is its own, and dataDir is the DRBD volume; both empty on a witness and on
// a zero-service node, which still get every node-scoped series. Best-effort on the guest, so a
// partial read comes back as a struct with the unread fields zero, not an error.
func (g *Client) Resources(ctx context.Context, services map[string]string, dataDir string) (telemetry.NodeResources, error) {
	var r telemetry.NodeResources
	req := resourcesRequest{DataDir: dataDir}
	// Sorted, so a run's forensics read in the same order every cycle -- map order would shuffle
	// the series between samples for no reason.
	names := make([]string, 0, len(services))
	for name := range services {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		req.Services = append(req.Services, resourceService{Name: name, Unit: services[name]})
	}
	err := g.c.call(ctx, verbResources, req, &r)
	return r, err
}

// WriteCert lands a renewed cert/key on the DRBD volume, where the TLS terminator
// hot-reloads it -- so a cloud-scheduled renewal is applied with no restart. Replicated +
// synced, so a failover serves the same cert.
func (g *Client) WriteCert(ctx context.Context, cert, key string) error {
	return g.c.call(ctx, verbCertWrite, certWriteRequest{Cert: cert, Key: key}, nil)
}

// BackupSave has the guest seal the home's sacred config (base/includes) to an
// encrypted blob at dest, using the household's age public recipient. The guest
// does the tar+encrypt locally and writes the blob itself — no bulk over the channel.
func (g *Client) BackupSave(ctx context.Context, base string, includes []string, recipient, dest string) error {
	return g.c.call(ctx, verbBackupSave, backupSaveRequest{Base: base, Includes: includes, Recipient: recipient, Dest: dest}, nil)
}

// BackupRestore has the guest decrypt the blob at src with the household identity and
// extract it into base. A recovery op; the caller supplies the private key.
func (g *Client) BackupRestore(ctx context.Context, base, src, identity string) error {
	return g.c.call(ctx, verbBackupRestore, backupRestoreRequest{Base: base, Src: src, Identity: identity}, nil)
}

// PayloadStop stops the payload's unit -- the quiesce step before a snapshot.
func (g *Client) PayloadStop(ctx context.Context, unit string) error {
	return g.c.call(ctx, verbPayloadStop, unitRequest{Unit: unit}, nil)
}

// PayloadActive reports whether the payload's unit is active (the Running half of the
// health-gate; readiness is probed host-side against the payload's health endpoint).
func (g *Client) PayloadActive(ctx context.Context, unit string) (bool, error) {
	var active bool
	err := g.c.call(ctx, verbPayloadActive, unitRequest{Unit: unit}, &active)
	return active, err
}

// PayloadHealth asks the guest to GET the payload health URL from inside itself and report 200
// (the Ready half of the health-gate). Probing in-guest — not host->VIP over the LAN — keeps the
// readiness check working under a networking substrate where the host can't reach the guest's VIP
// (macvtap). Callers that need the legacy host-side behaviour on an OLD guest fall back when this
// returns an error (an unknown-verb error from a guest built before this verb existed).
func (g *Client) PayloadHealth(ctx context.Context, url string) (bool, error) {
	var ok bool
	err := g.c.call(ctx, verbPayloadHealth, healthRequest{URL: url}, &ok)
	return ok, err
}

// PayloadActiveSince reports the unit's ActiveEnterTimestampMonotonic (usec since boot),
// which advances only when the unit (re)enters active -- 0 while inactive. Unchanged across
// a maintenance pause/resume proves the promoter re-adopted the running payload rather than
// bouncing it (the contract's no-restart check).
func (g *Client) PayloadActiveSince(ctx context.Context, unit string) (uint64, error) {
	var usec uint64
	err := g.c.call(ctx, verbPayloadSince, unitRequest{Unit: unit}, &usec)
	return usec, err
}

// Snapshot takes a read-only btrfs snapshot of dataDir at dest (a subvolume on the
// same DRBD volume, so it replicates with it).
func (g *Client) Snapshot(ctx context.Context, dataDir, dest string) error {
	return g.c.call(ctx, verbDataSnapshot, snapshotRequest{DataDir: dataDir, Path: dest}, nil)
}

// Restore replaces the live dataDir subvolume with a fresh rw snapshot of src. The
// caller must have stopped the payload first (bind released).
func (g *Client) Restore(ctx context.Context, dataDir, src string) error {
	return g.c.call(ctx, verbDataRestore, snapshotRequest{DataDir: dataDir, Path: src}, nil)
}

// SystemPath reads the guest's current system closure store path -- the code identity
// recorded on a snapshot (node-independent, unlike a generation number) so code and
// data roll back together.
func (g *Client) SystemPath(ctx context.Context) (string, error) {
	var path string
	err := g.c.call(ctx, verbOSSystem, nil, &path)
	return path, err
}

// Stage realises closure into the guest's nix store, substituting whatever is missing --
// the delivery half of, and the ONLY call that pulls bytes. Everything after
// it (Switch's and StageBoot's staged checks, a promoting peer's converge) requires the closure
// to be local already, because the failover path must never fetch.
//
// src is the zero StageSource in production, where the guest uses the caches baked into
// its image; a caller sets it to override that for one call (see StageSource).
func (g *Client) Stage(ctx context.Context, closure string, src StageSource) error {
	return g.c.call(ctx, verbOSStage, stageRequest{Path: closure, From: src.URL, FromKey: src.Key}, nil)
}

// Components reads a closure's boot-critical parts. An empty closure reads the
// BOOTED generation -- the reference to diff a staged target against, since that is the
// kernel actually running. The host compares the two and picks the activation method;
// this call has no opinion.
func (g *Client) Components(ctx context.Context, closure string) (SystemComponents, error) {
	var c SystemComponents
	err := g.c.call(ctx, verbOSComponents, systemRequest{Path: closure}, &c)
	return c, err
}

// Switch points the system profile at closure (a store path) and activates it -- the
// whole-VM code half of an upgrade/rollback. Reverting is the same call with an
// earlier closure (a roll-forward). The closure must already be in the store -- Stage
// is what puts it there.
func (g *Client) Switch(ctx context.Context, closure string) error {
	return g.c.call(ctx, verbOSSwitch, systemRequest{Path: closure}, nil)
}

// StageBoot makes closure bootable without making it the default: it registers the closure
// as a generation of the `staging` system profile and reinstalls the bootloader from the
// RUNNING system, so grub gains a staging submenu while its default entry does not move
// . Nothing on the guest's disk then decides which of the two boots -- the host
// does, per launch, with the SMBIOS selector (platform.QEMUSpec.BootStaging).
//
// That split is deliberate: the arming lives OUTSIDE the disk, so an OS-disk snapshot taken
// after this call contains nothing armed, and restoring it cannot re-run the generation the
// restore was undoing. (The rejected alternative, `grub-reboot`, writes the arming into the
// guest's own grubenv -- inside the snapshot.) It also fails safe: an unset or unrecognised
// selector leaves grub on its default, so a bug boots the OLD system.
//
// The closure must already be staged; this call never fetches.
func (g *Client) StageBoot(ctx context.Context, closure string) error {
	return g.c.call(ctx, verbOSStageBoot, systemRequest{Path: closure}, nil)
}

// PowerOff asks the guest OS to shut itself down cleanly. It returns as soon as the request
// is accepted -- the shutdown then proceeds without us, and the control channel dies with
// it, which is expected rather than an error. Confirm completion by watching the VM stop
// (platform.Guest.WaitStopped), never by this call.
//
// Preferred over the ACPI power button (platform.Guest.Shutdown) whenever the agent is
// reachable: it asks the OS in the OS's own terms instead of relying on something inside
// being subscribed to a virtual button. Keep both -- this one needs a live agent, and the
// ACPI one is what remains when the agent is the thing that died.
func (g *Client) PowerOff(ctx context.Context) error {
	return g.c.call(ctx, verbOSPowerOff, nil, nil)
}

// CollectGarbage drops old generations of the guest's profiles and collects the store
// -- the counterweight to incremental updates, which add a closure per release and
// never removed one. Host-scheduled and never inside a maintenance bracket; see the observe
// loop, where being on the same goroutine as the upgrade dispatch makes that structural.
func (g *Client) CollectGarbage(ctx context.Context) error {
	return g.c.call(ctx, verbOSGC, nil, nil)
}

// ReactorPause suspends drbd-reactor's promoter for a snippet (maintenance mode):
// the resource stays Primary and its services keep running, but the promoter stops
// reacting -- so a planned payload stop isn't mistaken for a failure.
func (g *Client) ReactorPause(ctx context.Context, snippet string) error {
	return g.c.call(ctx, verbReactorPause, reactorRequest{Snippet: snippet}, nil)
}

// ReactorEvict hands this node's work to a peer -- a PLANNED handover, the thing an upgrade
// needs so a healthy node can reboot while its peer serves. keepMasked leaves
// the node ineligible to take it back (the reboot path); unmask releases that and evicts
// nothing. It reports only that the eviction RAN: which peer took over is drbd-reactor's
// election, so a caller that cares must read the roles afterwards.
func (g *Client) ReactorEvict(ctx context.Context, keepMasked, unmask bool) error {
	return g.c.call(ctx, verbReactorEvict, evictRequest{KeepMasked: keepMasked, Unmask: unmask}, nil)
}

// ReactorActive reports whether the promoter daemon is running — false meaning someone has it
// paused, i.e. a maintenance bracket is already open. The interim guard: a second bracket user
// refuses to start rather than corrupting the first one's bracket from the middle. Advisory by
// construction, since a pause can still land between this answer and the caller's own.
func (g *Client) ReactorActive(ctx context.Context) (bool, error) {
	var active bool
	err := g.c.call(ctx, verbReactorActive, struct{}{}, &active)
	return active, err
}

// FsSync flushes the replicated data volume's dirty pages — the pre-copy that bounds an
// eviction's unmount flush ([B.100a]). Returns the guest's one-word account ("synced", or
// "skipped: ..." on a node with nothing mounted — a Secondary answering honestly, not failing).
func (g *Client) FsSync(ctx context.Context) (string, error) {
	var detail string
	err := g.c.call(ctx, verbFsSync, struct{}{}, &detail)
	return detail, err
}

// ReactorResume re-adopts the promoter (drbd-reactor enable); it re-runs the initial
// target start, which is a no-op on an already-Primary node with services up.
func (g *Client) ReactorResume(ctx context.Context, snippet string) error {
	return g.c.call(ctx, verbReactorResume, reactorRequest{Snippet: snippet}, nil)
}

// BringUpSpec is one DRBD resource to bring up on this node.
type BringUpSpec struct {
	Resource  drbd.Resource
	Diskless  bool     // this node is a diskless witness (no create-md, no promoter)
	FreshInit bool     // first-ever bring-up of this resource: declare it UpToDate (skip initial sync)
	Promoter  []string // drbd-reactor promoter units in start order; nil on a witness
	// ServiceUnits re-materialises a runtime-installed service's quadlet files before the
	// promoter is started. They live on tmpfs inside the guest, so a reboot erases them
	// while the host's manifest cache survives — and Promoter above then names units that do not
	// exist. Empty on a node with no runtime-installed service, which is the shipped state.
	//
	// It has to happen HERE rather than after BringUp returns, and that is the whole reason this
	// field exists: ReactorStart is the last thing below, so by the time the caller has its
	// channel back the promoter is already trying to start the chain. Units must exist before
	// anything can be asked to start them.
	ServiceUnits map[string]string
	// ServiceImages maps each .image warm unit to the image REF it obtains, for every installed
	// service. Their WantedBy=multi-user.target already fired during boot, so units written now
	// would never run otherwise, and the containers' Pull=never would fail the chain at promotion
	// instead of fetching.
	//
	// A MAP, not a unit list, because bring-up must not pull. This comment used to claim "on a
	// reboot the images are still in local storage, so this is a no-op, not a pull" -- and that was
	// simply false: quadlet generates `Wants=network-online.target` +
	// `ExecStart=podman image pull <ref>` with no already-present short-circuit (measured with
	// podman's own generator), so every guest reboot re-pulled from a registry and a node without
	// WAN failed bring-up outright. That is the RUNNING half of [V3.17]'s doctrine -- installing
	// needs network, running and failover never do -- broken on the path that runs after every
	// reboot. The ref is what lets the guest answer "is it already here?" before pulling.
	ServiceImages map[string]string
}

// BringUp performs the agent-owned bring-up: render + drop the DRBD
// config and (on a data node) create-md, start drbd@<res>.target, then -- on a data
// node -- start drbd-reactor. Promotion is drbd-reactor's, done asynchronously once
// quorum forms; the caller observes convergence via Status. A witness (Diskless,
// no Promoter) provisions the config and comes up, but creates no metadata and
// runs no promoter.
//
// THE ORDER IS THE GUARANTEE, not merely the sequence ([V3b.16a]). Nothing else starts
// drbd-reactor -- not boot, not a target -- so every step above happens-before any promotion can,
// on a reboot exactly as on a first install. Quorum does not wait on the reactor either (drbd.up
// attaches DRBD itself), and the caller gates on QUORATE rather than Primary, so there is no
// cycle: bring-up never waits for a promotion it is the precondition of.
func (g *Client) BringUp(ctx context.Context, spec BringUpSpec) error {
	res := spec.Resource
	var reactorCfg string
	if len(spec.Promoter) > 0 {
		reactorCfg = drbd.ReactorConfig(res.Name, spec.Promoter)
	}
	req := ProvisionRequest{
		Resource:      res.Name,
		ResConfig:     res.Config(),
		ReactorConfig: reactorCfg,
		Diskless:      spec.Diskless,
	}
	prov, err := g.Provision(ctx, req)
	if err != nil {
		return err
	}
	if err := g.Up(ctx, res.Name); err != nil {
		return err
	}
	// Declare UpToDate (skip-initial-sync) only on a TRUE first init: the designated seed AND a
	// disk we actually just created metadata on. On a reboot the metadata already existed, so
	// Provision skipped create-md and we attach + resync from peers instead of re-declaring
	// UpToDate -- which would split-brain against the replica that kept serving.
	if spec.FreshInit && prov.CreatedMetadata {
		if err := g.InitUpToDate(ctx, res.Name); err != nil {
			return err
		}
	}
	// Put the service's units back before the promoter can look for them. A failure here
	// fails bring-up, like every other step: the alternative is a node that comes up with a
	// promoter chain naming units that do not exist, which fails later and less legibly. (The
	// soft-failure rule lives one level up, in reading the cache at all — an unusable cache means
	// "nothing installed", and this code never runs.)
	if len(spec.ServiceUnits) > 0 {
		if err := g.ServiceRender(ctx, spec.ServiceUnits, nil); err != nil {
			return fmt.Errorf("re-render installed service units: %w", err)
		}
		// Sorted so the log reads the same on every boot; the images are independent, so the
		// order is presentation, not semantics.
		units := make([]string, 0, len(spec.ServiceImages))
		for u := range spec.ServiceImages {
			units = append(units, u)
		}
		sort.Strings(units)
		for _, u := range units {
			if err := g.ServiceWarm(ctx, u, spec.ServiceImages[u]); err != nil {
				return fmt.Errorf("warm image unit %s: %w", u, err)
			}
		}
	}
	if len(spec.Promoter) > 0 {
		return g.ReactorStart(ctx, res.Name)
	}
	return nil
}

// WaitPrimary polls Status until this node is the quorate primary -- i.e. bring-up
// has converged: drbd-reactor promoted once quorum formed. interval is the poll
// cadence; the caller bounds the total wait via ctx. Transient Status errors
// (guest still coming up) are ignored until ctx expires.
func (g *Client) WaitPrimary(ctx context.Context, resource string, interval time.Duration) error {
	return g.waitStatus(ctx, resource, interval, func(qs model.QuorumState) bool {
		return qs.Primary && qs.Quorate
	})
}

// WaitQuorate polls Status until this node is quorate -- the convergence gate for a
// promoter-capable data node in a *multi-node* cluster, where drbd-reactor promotes
// exactly one node and the others settle Secondary. Gating those on WaitPrimary would
// hang the secondaries; quorate means "participating in a healthy cluster" regardless
// of which node won the primary role (the VIP/payload run on whoever did).
func (g *Client) WaitQuorate(ctx context.Context, resource string, interval time.Duration) error {
	return g.waitStatus(ctx, resource, interval, func(qs model.QuorumState) bool {
		return qs.Quorate
	})
}

// WaitStatus polls Status until ready(qs) or ctx expires; transient Status errors
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
func (g *Client) Close() error { return g.c.close() }

// BringUpGuest is the host-side entry point once the guest VM is running: dial its
// control socket (the virtio-serial chardev's host end), bring the resource up, and
// wait for the node to converge to quorate primary.
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
	return g.WaitPrimary(ctx, spec.Resource.Name, DefaultPollInterval)
}

// osExecutor is the real guest Executor: shell out + write files.
type osExecutor struct{}

// NewOSExecutor returns the production Executor used by `briard run --guest`.
func NewOSExecutor() Executor { return osExecutor{} }

func (osExecutor) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

func (osExecutor) WriteFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func (osExecutor) ReadFile(path string) ([]byte, error) { return os.ReadFile(path) }

func (osExecutor) Sethostname(name string) error {
	return setHostname(name)
}
