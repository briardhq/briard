package host

import (
	"os"
	"strconv"
	"strings"
	"time"

	"briard.io/agent/drbd"
	"briard.io/shared/model"
)

// promoterUnits is the ordered drbd-reactor promoter chain for a data node: mount the DRBD
// volume -> converge this node to what the volume says it runs -> claim the VIP. The front door
// is not a member: it rides briard-vip (wantedBy + partOf) inside the guest, so it tracks the
// primary role without the host needing to name it.
//
// IT IS STATIC, and that is what makes converge-at-promotion possible ([V3b.3](f)). The chain is
// what drbd-reactor promotes WITH, but the volume is only readable AFTER promotion — so the
// start-list cannot name the services themselves. briard-services is the unit that, once the
// mount exists, reads the manifests, renders and starts them. The runtime-installed services are
// therefore NOT members, which is also what makes "a service error alerts but never demotes"
// mechanically true: drbd-reactor never sees them, so a crashed container cannot deactivate the
// target. A constant chain is what the baked slot's always was; restoring it generalises the
// trick to N services.
//
// Nothing here is conditional on a service existing any more. The old conditional membership
// existed because naming a unit the guest does not define fails the WHOLE ordered chain, and a
// zero-service node has no service unit to name — but briard-services is defined
// unconditionally by the guest image, exactly as briard-data and briard-vip are, so there is
// nothing left to make conditional.
//
// It takes no arguments, and that is the end state [V3b.3](e1) was after: the chain is the same
// three units on every anchor in the fleet, so there is nothing to decide and nothing to pass.
func promoterUnits() []string {
	return []string{"briard-data.service", "briard-services.service", "briard-vip.service"}
}

// buildVersion is the agent's release id, stamped at build time:
//
//	-ldflags "-X briard.io/agent/host.buildVersion=<id>"
//
// It is what a self-update converges TO (NodeStatus.AgentVersion) and the idempotency key for a
// re-offered agent-update. Empty in a plain `go build` / test binary -> self-update
// convergence is unobservable (fine: those binaries aren't the ones the cloud rolls out).
var buildVersion string

// versionBanner is the line the agent logs once at startup so a running install can say
// which build it is. Until this existed the id was stamped at build time, threaded
// into NodeStatus, and used by the self-updater -- but never shown to a human, so the first
// question on every bug report ("which version?") had no answer the reporter could reach.
//
// An unstamped binary says so rather than logging an empty string: "development build" is a
// fact a bug report can act on, whereas a blank version reads as a formatting bug and invites
// the reporter to omit the field entirely.
func versionBanner(version string) string {
	if version == "" {
		return "briard-agent starting (development build — no version stamped)"
	}
	return "briard-agent starting, version " + version
}

// ConfigFromEnv builds a Config from the environment, mirroring the driver's
// single-node contract. Multi-node peer wiring + witness topology is separate; the
// cloud-enrollment config source is the controller.
func ConfigFromEnv() Config {
	node := env("NODE", "guest")
	role := model.Role(env("ROLE", string(model.RoleAnchor)))
	// The full connection mesh comes from PEERS (identical on every node — DRBD
	// matches self by the `on <name>` stanza to the guest hostname). With PEERS
	// unset we keep the single-node self-peer from PEER_ADDR/DATA_DEV.
	peers := parsePeers(os.Getenv("PEERS"))
	if len(peers) == 0 {
		peers = []drbd.Peer{{
			Name: node, NodeID: 0,
			Address: env("PEER_ADDR", "127.0.0.1:7789"),
			Disk:    env("DATA_DEV", "/dev/vdb"),
		}}
	}
	cfg := Config{
		QEMUBinary:  env("QEMU", "qemu-system-x86_64"),
		QEMUDataDir: os.Getenv("QEMU_DATADIR"), // "" -> qemu default; the bundle sets <prefix>/share/qemu
		Accel:       env("ACCEL", "kvm:tcg"),
		// `max` = every feature the accelerator can give the guest, which under KVM is the
		// host's own CPU. The escape hatch (BRIARD_CPU=qemu64 at the installer, CPU= here) is
		// for a host where the passthrough itself is the suspect -- one env line beats a release.
		CPUModel:    env("CPU", "max"),
		MemoryMB:    atoi(os.Getenv("MEMORY_MB"), 2048),
		Cores:       atoi(os.Getenv("CORES"), 2),
		GuestDisk:   os.Getenv("GUEST_DISK"),
		DataDisk:    os.Getenv("DATA_DISK"),
		ControlSock: env("CONTROL_SOCK", "/run/briard-ctl.sock"),
		// QEMU's own control channel -- the VM, not the guest OS inside it. Without
		// it the host's only way to stop a guest is killing qemu, i.e. a power cut to the
		// machine whose job is not losing data. Its own directory, because platform.Launch
		// makes that directory 0700: QMP is unrestricted control of the VM (it can dump guest
		// RAM to a file), so it must not share a directory with a socket meant to be reachable.
		QMPSock: env("QMP_SOCK", "/run/briard/qmp/guest.sock"),
		// On by default, because an admin door only an expert can find is not a door.
		// The CLI defaults to this same path (agent/cli); keep the two literals in step.
		AdminSock:  env("ADMIN_SOCK", "/run/briard/admin.sock"),
		ServiceTap: os.Getenv("SERVICE_TAP"),
		SystemTap:  os.Getenv("SYSTEM_TAP"),
		WitnessTap: os.Getenv("WITNESS_TAP"),  // eth3 private witness link; "" -> no witness NIC
		NetMode:    os.Getenv("NET_MODE"),     // "" (bridge, default) | "macvtap"
		VIPParent:  os.Getenv("VIP_PARENT"),   // bridge substrate only: the NIC the guest builds VIP_DEV on
		NetWrapBin: os.Getenv("NET_WRAP_BIN"), // the fd-passing launch wrapper; required for NET_MODE=macvtap
		SerialLog:  os.Getenv("GUEST_SERIAL"),
		// Host-side witness-forwarder identity. Bin + the anchor cert/key/ca; a managed
		// pairing directive (MeshSpec.Witness) starts the forwarder with these. Unset -> a pairing
		// that needs the cloud witness fails fast (before any DRBD change).
		ForwarderBin: os.Getenv("FORWARDER_BIN"),
		WitnessCert:  os.Getenv("WITNESS_CERT"),
		WitnessKey:   os.Getenv("WITNESS_KEY"),
		WitnessCA:    os.Getenv("WITNESS_CA"),
		Node:         node,
		Role:         role,
		SystemDev:    os.Getenv("SYSTEM_DEV"),  // e.g. eth1 (the system NIC); "" -> leave it unaddressed
		SystemCIDR:   os.Getenv("SYSTEM_CIDR"), // this node's node IP, e.g. 10.0.0.1/24
		// The host's own end of the system subnet, and the guest's name for the private NIC.
		// install.sh sets all three together or none of them: a host address with no device to
		// route it over, or a device with no address, is a half-built path.
		SystemHostCIDR: os.Getenv("SYSTEM_HOST_CIDR"), // e.g. 10.0.0.129/32
		WitnessDev:     env("WITNESS_DEV", "eth3"),
		WitnessCIDR:    os.Getenv("WITNESS_CIDR"), // the guest's end of the private link, e.g. 10.11.9.2/24
		VIPDev:         os.Getenv("VIP_DEV"),      // e.g. eth2 on a data node; "" -> this node claims no VIP (a witness)
		VIPAddr:        os.Getenv("VIP_ADDR"),     // e.g. 192.168.9.50/24; "" -> DHCP (the LAN owns the value)
		FlockID:        os.Getenv("FLOCK_ID"),     // flock-scoped VIP MAC seed; "" -> fall back to the node name
		FlockName:      os.Getenv("FLOCK_NAME"),   // flock-scoped VISIBLE name for mDNS; "" -> publish nothing
		Resource: drbd.Resource{
			Name:   env("RESOURCE", "r0"),
			Device: env("DEVICE", "/dev/drbd0"),
			Peers:  peers,
		},
		// Exactly one node seeds a fresh cluster (skip-initial-sync); the rest sync
		// from it. The first peer is that node by convention (single-node: itself).
		FreshInit: node == peers[0].Name,
		// A diskless node has no payload/VIP to probe (nor a service NIC to reach it), so
		// its health follows quorum ("" -> healthy == quorate); data nodes probe the VIP.
		//
		// The probe target is the FRONT DOOR (:80), not a service's own port. That is what makes
		// it stable across zero and one service: the proxy answers for itself when nothing is
		// installed and forwards the question to the service when something is. Probing a
		// service port directly is what left a freshly installed node reporting unhealthy
		// forever with no reflex able to tell that apart from a broken service.
		//
		// NO DEFAULT, deliberately (V3.19c step 3). It used to be the lab's own
		// `http://192.168.1.100/healthz` -- a guess about someone else's network that agreed
		// with every test we ran, which is precisely how the baked VIP stayed invisible. Unset
		// now means "ask the guest what address it actually holds" (guest.ResolveHealthURL, via
		// VIP_DEV): the only source that can be right on a LAN we have never seen. Setting it
		// explicitly still pins a probe target.
		HealthURL:       disklessOr(role, "", os.Getenv("HEALTH_URL")),
		StatusEvery:     durEnv("STATUS_EVERY", 10*time.Second),
		BringUpBudget:   durEnv("BRINGUP_BUDGET", 5*time.Minute),
		UpgradeBudget:   durEnv("UPGRADE_BUDGET", 15*time.Minute),                   // the OS-upgrade bound, incl. the degraded wait before a revert
		ControllerURL:   os.Getenv("CONTROLLER_URL"),                                // "" -> standalone, no north-bound report
		ControllerToken: os.Getenv("CONTROLLER_TOKEN"),                              // bearer on seam calls; "" -> no auth
		AssignmentCache: env("ASSIGNMENT_CACHE", "/var/lib/briard/assignment.json"), // cold-boot cache
		NotifyURL:       os.Getenv("NOTIFY_URL"),                                    // ntfy topic URL for alerts; "" -> log-only
		TelemetryPath:   os.Getenv("TELEMETRY_PATH"),                                // out-of-band soak collector file; "" -> don't write
		MetricsWindow:   durEnv("METRICS_WINDOW", time.Hour),                        // cloud aggregate rollup bucket; soak shortens it to exercise rollover
		// A TEST FIXTURE, read here so it has one home rather than a stray Getenv in the observe
		// loop. agent-watchdog.nix sets it to wedge that goroutine on purpose; nothing else does,
		// install.sh writes no such variable, and unset is a no-op. See wedgeForTest.
		WedgeFIFO: os.Getenv("BRIARD_WEDGE_FIFO"),
		// Services is NOT read from the environment, and there is nothing here to read it from:
		// what a node runs is installed at runtime and rebuilt from the node-local manifest cache
		// at bring-up (Run -> installedServices), or read off the volume when this node promotes
		// (adoptVolumeServices). The environment described the build-time payload slot, which is
		// gone ([V3b.3](e1)); the empty set is the shipped state and every node starts there.
		// The catalog is published signed static content (OSS §10.1: an apt-mirror, not an API),
		// which is exactly what the release channel already is -- so it lives in the same bucket,
		// under the same trust root (the release keyring verifies manifests and artifacts alike),
		// with one publish credential and one thing for a third party to mirror. briard.io itself
		// is the marketing site; a service catalog is not a web page.
		CatalogURL:     env("CATALOG_URL", "https://get.briard.io/catalog"),
		ServiceCache:   env("SERVICE_CACHE", "/var/lib/briard/services"),
		MeshCache:      env("MESH_CACHE", "/var/lib/briard/mesh.json"),
		ReactorSnippet: os.Getenv("REACTOR_SNIPPET"),
		// Host-agent self-update. UPDATE_KEYRING points at a PEM file of trusted
		// Ed25519 release public keys; unset (or unreadable) -> self-update is OFF (an
		// agent-update directive refuses, fail-closed). Base/RunDir/Unit default in
		// newSelfUpdater. Version is baked at build time (buildVersion), overridable by env for
		// tests -- it is the running binary's own id, so a committed update reports the new one.
		UpdateKeyring: readFileOrNil(os.Getenv("UPDATE_KEYRING")),
		UpdateBase:    os.Getenv("UPDATE_BASE"),
		UpdateRunDir:  os.Getenv("UPDATE_RUN_DIR"),
		UpdateUnit:    os.Getenv("UPDATE_UNIT"),
		Version:       env("AGENT_VERSION", buildVersion),
	}
	if role == model.RoleDiskless {
		cfg.Diskless = true // no metadata, no promoter
	} else {
		cfg.Promoter = promoterUnits()
	}
	return cfg
}

// parsePeers parses the PEERS env into the full DRBD connection mesh. It is the
// same value on every node (DRBD identifies self by matching the guest hostname to
// an `on <name>` stanza), so NodeID is the entry's position and the ordering must
// be identical fleet-wide. Each entry is "name@host[:port]/disk": host defaults to
// port 7789; disk "none" (or empty) is a diskless witness, otherwise it names the
// backing device by bare name under /dev (e.g. "vdb" -> /dev/vdb). Malformed entries
// are skipped. Returns nil for an empty value -- caller keeps the single self-peer.
func parsePeers(s string) []drbd.Peer {
	var peers []drbd.Peer
	for _, entry := range strings.Split(s, ",") {
		entry = strings.TrimSpace(entry)
		name, rest, ok := strings.Cut(entry, "@")
		if !ok || name == "" {
			continue
		}
		addr, disk, _ := strings.Cut(rest, "/")
		if !strings.Contains(addr, ":") {
			addr += ":7789"
		}
		p := drbd.Peer{Name: name, NodeID: len(peers), Address: addr}
		if disk != "" && disk != "none" {
			p.Disk = "/dev/" + disk
		}
		peers = append(peers, p)
	}
	return peers
}

// disklessOr returns w when the role is diskless, else d.
func disklessOr(role model.Role, w, d string) string {
	if role == model.RoleDiskless {
		return w
	}
	return d
}

// disklessOrSpecs empties the service set on a diskless node (no payload to upgrade).
func disklessOrSpecs(role model.Role, s []model.ServiceSpec) []model.ServiceSpec {
	if role == model.RoleDiskless {
		return nil
	}
	return s
}

// readFileOrNil returns the contents of path, or nil when path is empty or unreadable -- a
// missing/unreadable release keyring simply leaves self-update off (fail-closed), not a crash.
func readFileOrNil(path string) []byte {
	if path == "" {
		return nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return b
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func atoi(s string, def int) int {
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	return def
}

func durEnv(k string, def time.Duration) time.Duration {
	if d, err := time.ParseDuration(os.Getenv(k)); err == nil {
		return d
	}
	return def
}
