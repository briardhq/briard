package host

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"briard.io/agent/drbd"
	"briard.io/shared/model"
	"briard.io/shared/nodestorage"
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
// unconditionally by the guest image, exactly as briard-primary-storage and briard-vip are, so there is
// nothing left to make conditional.
//
// It takes no arguments, and that is the end state [V3b.3](e1) was after: the chain is the same
// units on every anchor in the fleet, so there is nothing to decide and nothing to pass.
//
// THE FRONT DOOR IS A MEMBER ([B.125]) AND CARRIES THE HOUSEHOLD'S mDNS NAMES ([B.152]). On a node
// with no `.casa` domain the door routes `byHost` against the service's own `.local` names, so a
// request to the bare VIP matches nothing: the NAME is the only path to a service, and a node that
// cannot publish is unreachable while every other part of it reports healthy. That is what the
// promoter is for. Publishing and answering being one member is also what makes them impossible to
// disagree — the door answers for exactly the names it routes.
// Membership means it never runs on the node that LOST the promotion race: reactor gives every
// member `Requires=drbd-promote@<res>.service`, so its job fails with 'dependency' and never
// executes, where a `wantedBy` binding would start it into a node that had claimed no address.
//
// ORDER IS THE DEPENDENCY: reactor writes `Requires=`/`After=` the PREVIOUS member, so the door
// starts only once briard-vip holds the address its names resolve to. Publishing a name that
// nothing answers is the failure mode the order exists to avoid.
func promoterUnits() []string {
	return []string{
		"briard-primary-storage.service",
		"briard-services.service",
		"briard-vip.service",
		"briard-reverse-proxy.service",
		"briard-dashboard.service",
	}
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

// THE INSTALL LAYOUT, AND WHY IT IS HERE RATHER THAN IN THE INSTALLER ([B.157]).
//
// install.sh is fetched from the channel root and run once, so every value it writes is frozen
// where no release can reach it. It therefore writes only what it COMPUTES about this host -- the
// identifiers it minted, the operator's own overrides -- and every DEFAULT lives here, in a binary
// the channel can fix. A node whose config.env is missing a key is not a broken node; it is a node
// taking the shipped answer.
//
// The three roots are constants because the prefix is: /opt/briard is baked into the qemu bundle's
// own ELF interpreter, so an install anywhere else produces a qemu that cannot execute.
const (
	prefixDir = "/opt/briard"
	stateDir  = "/var/lib/briard"
	runDir    = "/run/briard"

	// defaultConfigFile is where install.sh writes this node's configuration ([B.150](a)). The
	// shipped unit states it in BRIARD_CONFIG so `systemctl cat` answers the question; this is the
	// answer for a hand-run agent, which is told nothing.
	defaultConfigFile = prefixDir + "/config.env"
)

// loadConfigFile makes the file the DEFAULT layer under the environment, by setting only the keys
// the environment has not already spoken for. Every os.Getenv below therefore reads it without
// knowing it exists, and so does every child the agent execs — which is what makes this exactly
// the delivery the unit's `Environment=` lines used to be, rather than a second config mechanism
// standing beside them.
//
// THE POINT IS THAT A FILE CAN BE REWRITTEN AND A UNIT CANNOT. Network decisions are made from
// what this host can see, and what it can see changes — a NIC is replaced, a cable moves, the
// household's router is swapped. Decisions baked into a generated unit are frozen where no
// release can reach them; in a file the agent can converge them ([B.150]).
//
// Deliberately dumber than a .env parser: no quoting, no expansion, no `export`, no multi-line
// values. Every value written here is a path, a device name, an address or a duration, and a
// parser a reader can hold in their head is worth more than one that round-trips a shell string.
// A missing file is not an error — the hermetic tests and a hand-run agent have none.
func loadConfigFile(path string) {
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		// Both halves are trimmed, which costs the ability to write a value with a leading or
		// trailing space and buys the ability to hand-edit this file without counting them. No
		// value here can want one: they are paths, device names, addresses and durations.
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		// LookupEnv, not Getenv: an explicitly EMPTY environment entry is a decision — it is how
		// the substrate fork says "this node has no service tap" ([V3b.26c]) — so it must win
		// over the file exactly as a non-empty one does.
		if _, set := os.LookupEnv(k); k == "" || set {
			continue
		}
		os.Setenv(k, v)
	}
}

// ConfigFromEnv builds a Config from the environment — under which install.sh's config file is
// the default layer (loadConfigFile) — mirroring the driver's single-node contract. Multi-node
// peer wiring + witness topology is separate; the cloud-enrollment config source is the controller.
func ConfigFromEnv() Config {
	loadConfigFile(env("BRIARD_CONFIG", defaultConfigFile))
	// THIS NODE'S OWN NAME, resolved before the struct because the DRBD self-peer below is named
	// after it. Three tiers, the same order every value here uses: the environment, then the record
	// this node minted for itself (identity.go), then the literal every install answered to before
	// [V3.20] gave each one its own.
	id := recordedIdentity(filepath.Dir(env("ASSIGNMENT_CACHE", stateDir+"/assignment.json")))
	node := env("NODE", id.node)
	if node == "" {
		node = defaultNodeName
	}
	role := model.Role(env("ROLE", string(model.RoleAnchor)))
	// The full connection mesh comes from PEERS (identical on every node — DRBD
	// matches self by the `on <name>` stanza to the guest hostname). With PEERS
	// unset we keep the single-node self-peer from PEER_ADDR.
	//
	// ITS DISK IS NOT CONFIGURABLE, and there used to be a DATA_DEV knob here saying otherwise.
	// It was the third leg of V1.0's single-node tripod -- who (NODE), where (PEER_ADDR), which
	// disk (DATA_DEV) -- from the era when the backing really was the raw disk and a node might
	// answer that differently. [V3b.33](b) ended the question: every diskful node runs on the one
	// LV the seam builds, so `Peer.Disk` is the diskful/diskless flag it always was, and the only
	// thing the knob could still express was a disagreement with the seam. Nothing ever set it.
	peers := parsePeers(os.Getenv("PEERS"))
	if len(peers) == 0 {
		peers = []drbd.Peer{{
			Name: node, NodeID: 0,
			Address: env("PEER_ADDR", "127.0.0.1:7789"),
			Disk:    drbd.DataDevice,
		}}
	}
	cfg := Config{
		// ⚠️ THE BUNDLE AND THE DISKS ARE NOT DEFAULTED, and that is the other half of [B.157]'s
		// rule: install.sh writes paths to what it CREATED OR STAGED -- the qemu tree it extracted,
		// the disks it allocated -- because those are facts about this host, not defaults. An empty
		// one is a decision too: it is how every agent-* rig says "no state disk", "no -L", "no
		// backing image", and a default would hand qemu a path to a file that is not there.
		QEMUBinary:  env("QEMU", "qemu-system-x86_64"),
		QEMUDataDir: os.Getenv("QEMU_DATADIR"),
		Accel:       env("ACCEL", "kvm:tcg"),
		// `max` = every feature the accelerator can give the guest, which under KVM is the
		// host's own CPU. The escape hatch (BRIARD_CPU=qemu64 at the installer, CPU= here) is
		// for a host where the passthrough itself is the suspect -- one env line beats a release.
		CPUModel:   env("CPU", "max"),
		MemoryMB:   atoi(os.Getenv("MEMORY_MB"), 2048),
		Cores:      atoi(os.Getenv("CORES"), 2),
		GuestDisk:  os.Getenv("GUEST_DISK"),
		GuestImage: os.Getenv("GUEST_IMAGE"),
		DataDisk:   os.Getenv("DATA_DISK"),
		StateDisk:  os.Getenv("STATE_DISK"),
		// The data volume's size, and the only thing about these disks the agent defaults: it is a
		// number rather than a path, so no rig depends on its absence. Sized for a real service's
		// data -- Home Assistant's `.storage` plus the recorder SQLite outgrows a gigabyte in
		// months, and growing a DRBD-backed volume afterwards is not a one-liner.
		DataSize:    env("DATA_SIZE", "4G"),
		ControlSock: env("CONTROL_SOCK", runDir+"/ctl.sock"),
		// QEMU's own control channel -- the VM, not the guest OS inside it. Without
		// it the host's only way to stop a guest is killing qemu, i.e. a power cut to the
		// machine whose job is not losing data. Its own directory, because platform.Launch
		// makes that directory 0700: QMP is unrestricted control of the VM (it can dump guest
		// RAM to a file), so it must not share a directory with a socket meant to be reachable.
		QMPSock: env("QMP_SOCK", "/run/briard/qmp/guest.sock"),
		// On by default, because an admin door only an expert can find is not a door.
		// The CLI defaults to this same path (agent/cli); keep the two literals in step.
		AdminSock: env("ADMIN_SOCK", "/run/briard/admin.sock"),
		// The guest's admin port ([V3b.31i]), beside the control socket it mirrors.
		AdminPortSock: env("ADMIN_PORT_SOCK", "/run/briard-admin.sock"),
		// THE GUEST'S THREE NICS, by the name of the host device behind each. `declared` and not
		// `env`: an explicitly EMPTY entry is how a node says it has no such NIC, and the default
		// must not overrule it ([V3b.26c]). Unset -- every shipped install, whose config.env names
		// a tap only when the operator did -- takes the shipped names.
		ServiceTap: declared("SERVICE_TAP", "briard0"),      // eth2, where the VIP lives
		SystemTap:  declared("SYSTEM_TAP", "briard-drbd0"),  // eth1, the node IP and DRBD
		WitnessTap: declared("WITNESS_TAP", "briard-priv0"), // eth3, the private host<->guest link
		NetMode:    os.Getenv("NET_MODE"),                   // derived by the agent; "" -> it decides
		VIPParent:  os.Getenv("VIP_PARENT"),                 // bridge substrate only: the NIC the guest builds VIP_DEV on
		NetWrapBin: os.Getenv("NET_WRAP_BIN"),               // the fd-passing launch wrapper install.sh staged
		// The device the guest's L2 hangs off, when an operator named one. The agent asks the same
		// question the report card did, so it reads the same override ([B.150](b)).
		NICOverride:  os.Getenv("NIC"),
		PrivHostCIDR: os.Getenv("PRIV_HOST_CIDR"), // the host's end of the private link, e.g. 10.11.9.1/24
		// The guest's serial console, captured to the host. Under macvtap the host cannot reach the
		// guest over the network at all, so this file is the only witness to anything inside the VM.
		// `declared`, because an empty entry is how BRIARD_GUEST_SERIAL= switches capture off.
		SerialLog: declared("GUEST_SERIAL", "/var/log/briard-guest-console.log"),
		// Host-side witness-forwarder identity. Bin + the anchor cert/key/ca; a managed
		// pairing directive (MeshSpec.Witness) starts the forwarder with these. Unset -> a pairing
		// that needs the cloud witness fails fast (before any DRBD change).
		ForwarderBin: os.Getenv("FORWARDER_BIN"),
		WitnessCert:  os.Getenv("WITNESS_CERT"),
		WitnessKey:   os.Getenv("WITNESS_KEY"),
		WitnessCA:    os.Getenv("WITNESS_CA"),
		Node:         node,
		Role:         role,
		// The guest's own kernel names for those NICs, which follow qemu's -netdev ORDER and are
		// therefore ours rather than an operator's. `declared` for the same reason the taps are:
		// "" is how a node says it addresses no system NIC and claims no VIP.
		SystemDev:  declared("SYSTEM_DEV", "eth1"),
		SystemCIDR: os.Getenv("SYSTEM_CIDR"), // this node's node IP, e.g. 10.0.0.1/24
		// The host's own end of the system subnet, and the guest's name for the private NIC.
		// install.sh sets all three together or none of them: a host address with no device to
		// route it over, or a device with no address, is a half-built path.
		SystemHostCIDR: os.Getenv("SYSTEM_HOST_CIDR"), // e.g. 10.0.0.129/32
		WitnessDev:     env("WITNESS_DEV", "eth3"),
		WitnessCIDR:    os.Getenv("WITNESS_CIDR"),   // the guest's end of the private link, e.g. 10.11.9.2/24
		PodSubnet:      os.Getenv("POD_SUBNET"),     // the pool private service networks come from, e.g. 10.12.7
		VIPDev:         declared("VIP_DEV", "eth2"), // "" -> this node claims no VIP (a witness)
		VIPAddr:        os.Getenv("VIP_ADDR"),       // e.g. 192.168.9.50/24; "" -> DHCP (the LAN owns the value)
		FlockID:        os.Getenv("FLOCK_ID"),       // flock-scoped VIP MAC seed; "" -> fall back to the node name
		FlockName:      os.Getenv("FLOCK_NAME"),     // flock-scoped VISIBLE name for mDNS; "" -> publish nothing
		Resource: drbd.Resource{
			Name:   env("RESOURCE", "r0"),
			Device: env("DEVICE", "/dev/drbd0"),
			Peers:  peers,
		},
		// Exactly one node seeds a fresh cluster (skip-initial-sync); the rest sync
		// from it. The first peer is that node by convention (single-node: itself).
		FreshInit: node == peers[0].Name,
		// The node's storage policy ([V3b.33](d)), which install.sh sets from
		// BRIARD_DATA_ENCRYPTION. Defaulting to auto here rather than to the empty string is
		// what makes every node the installer has never heard the question asked of behave as
		// the shipped default -- and an unparseable value is refused at bring-up (storageSpec),
		// not silently rounded to it.
		DataEncryption: nodestorage.Mode(env("DATA_ENCRYPTION", string(nodestorage.ModeAuto))),
		// A diskless node has no service/VIP to probe (nor a service NIC to reach it), so
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
		HealthURL: disklessOr(role, "", os.Getenv("HEALTH_URL")),
		// 5s, which is what every installed node has run at since the installer started writing it.
		StatusEvery:     durEnv("STATUS_EVERY", 5*time.Second),
		BringUpBudget:   durEnv("BRINGUP_BUDGET", 5*time.Minute),
		UpgradeBudget:   durEnv("UPGRADE_BUDGET", 15*time.Minute),             // the OS-upgrade bound, incl. the degraded wait before a revert
		ControllerURL:   os.Getenv("CONTROLLER_URL"),                          // "" -> standalone, no north-bound report
		ControllerToken: os.Getenv("CONTROLLER_TOKEN"),                        // bearer on seam calls; "" -> no auth
		AssignmentCache: env("ASSIGNMENT_CACHE", stateDir+"/assignment.json"), // cold-boot cache
		NotifyURL:       os.Getenv("NOTIFY_URL"),                              // ntfy topic URL for alerts; "" -> log-only
		TelemetryPath:   os.Getenv("TELEMETRY_PATH"),                          // out-of-band soak collector file; "" -> don't write
		MetricsWindow:   durEnv("METRICS_WINDOW", time.Hour),                  // cloud aggregate rollup bucket; soak shortens it to exercise rollover
		// A TEST FIXTURE, read here so it has one home rather than a stray Getenv in the observe
		// loop. agent-watchdog.nix sets it to wedge that goroutine on purpose; nothing else does, and
		// unset is a no-op. See wedgeForTest.
		WedgeFIFO: os.Getenv("WEDGE_FIFO"),
		// The other test fixture, read here for the same reason and with the same contract:
		// install-macvtap sets it to drive a re-parent without waiting out the shipped tier;
		// 0 is the shipped behaviour. See Config.ReparentTier.
		ReparentTier: durEnv("REPARENT_TIER", 0),
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
		CatalogURL:        env("CATALOG_URL", "https://get.briard.io/catalog"),
		ServiceCache:      env("SERVICE_CACHE", stateDir+"/services"),
		MeshCache:         env("MESH_CACHE", stateDir+"/mesh.json"),
		ChannelURL:        env("CHANNEL_URL", "https://get.briard.io"),
		GuestReleaseCache: env("GUEST_RELEASE_CACHE", stateDir+"/guest-release.json"),
		ReactorSnippet:    os.Getenv("REACTOR_SNIPPET"),
		// UPDATE_KEYRING points at a PEM file of trusted Ed25519 release public keys. It gates
		// the signed CATALOG (service install), not self-update any more: since [B.86a] the
		// agent's own update is fetched and verified by the frozen unit below it, under the same
		// keyring file, and the agent only watches the arm flag. Base/RunDir/Unit default in
		// newSelfUpdater. Version is baked at build time (buildVersion), overridable by env for
		// tests -- it is the running binary's own id, so a committed update reports the new one.
		UpdateKeyring: readFileOrNil(env("UPDATE_KEYRING", prefixDir+"/keyring.pem")),
		UpdateBase:    env("UPDATE_BASE", prefixDir+"/agent"),
		UpdateRunDir:  os.Getenv("UPDATE_RUN_DIR"),
		UpdateUnit:    os.Getenv("UPDATE_UNIT"),
		Version:       env("AGENT_VERSION", buildVersion),
	}
	if role == model.RoleDiskless {
		cfg.Diskless = true // no metadata, no promoter
	} else {
		cfg.Promoter = promoterUnits()
	}
	// The addresses this node numbers itself from, derived from the record it already holds
	// (subnets.go). A PURE READ, so every consumer of a Config -- the CLI, the dashboard, a
	// hand-run agent -- sees the same addresses the running agent does, with no network work and
	// no way for a process with no business numbering anything to draw. The DRAW belongs to
	// convergence and happens in exactly one place.
	return cfg.applyDraw(cfg.recordedSubnets()).applyIdentity(id)
}

// parsePeers parses the PEERS env into the full DRBD connection mesh. It is the
// same value on every node (DRBD identifies self by matching the guest hostname to
// an `on <name>` stanza), so NodeID is the entry's position and the ordering must
// be identical fleet-wide. Each entry is "name@host[:port]/disk": host defaults to
// port 7789; disk "none" (or empty) is a diskless witness, anything else is a DISKFUL
// node. The field is a flag, not a path, and that is what [V3b.33] made of it: a diskful
// node's backing is one value fleet-wide (drbd.DataDevice, the LV the guest's seam unit
// builds), so a mesh string that could name a different device per node would only be a
// way to disagree with the seam. Malformed entries are skipped. Returns nil for an empty
// value -- caller keeps the single self-peer.
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
			p.Disk = drbd.DataDevice
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

// disklessOrSpecs empties the service set on a diskless node (nothing to upgrade).
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

// declared is env() for a key whose EMPTY VALUE IS A DECISION.
//
// env() cannot express one: it falls back to the default for an explicitly empty entry, which is
// right for a URL or a duration and wrong for a device name, where "" is how a node says it has no
// such NIC ([V3b.26c]'s substrate fork). So the default here applies only when the key is not
// declared at all -- which is exactly the shipped install, whose config.env carries a key only when
// the operator named it ([B.157]).
func declared(k, def string) string {
	if v, ok := os.LookupEnv(k); ok {
		return v
	}
	return def
}
