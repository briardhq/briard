// Package host is the host-side Briard agent (the privileged product daemon): it
// boots the guest VM, drives the agent-owned DRBD bring-up over the control
// channel, and then continuously observes the node's state into an api.NodeStatus.
//
// This is the real host path the test `driver` prototyped. The
// difference from the driver is the standing observe loop: the driver brings up and
// then just holds the guest alive, whereas the agent keeps reading the node's status.
// Config comes from the environment (host.ConfigFromEnv); status is reported up the
// north-bound shared/api seam.
package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"briard.io/agent/cloud"
	"briard.io/agent/drbd"
	"briard.io/agent/guest"
	"briard.io/agent/guestagent"
	"briard.io/agent/overlay"
	"briard.io/agent/platform"
	"briard.io/agent/quadlet"
	"briard.io/shared/api"
	"briard.io/shared/model"
	"briard.io/shared/notify"
	"briard.io/shared/sdnotify"
	"briard.io/shared/telemetry"
)

// orNode returns id when set, else the node name. It is what makes the flock-scoped VIP MAC a
// pure addition: a node installed before FlockID existed, and every agent-less harness, has no id
// and keeps the exact MAC it has always had rather than silently drawing a new DHCP lease.
func orNode(id, node string) string {
	if id != "" {
		return id
	}
	return node
}

// setNodeRoute installs the host's standing path to its guest's node IP over the private link.
// Skipped -- not an error -- when any of the three facts it needs is absent: a substrate with no
// private link (the host is on-link there and needs no route), a harness that gives the host no
// system address, or a node with no node IP. The setter is a parameter so the skip conditions can
// be tested without a live network.
func (cfg Config) setNodeRoute(ctx context.Context, set func(context.Context, platform.NodeRoute) error) error {
	if cfg.WitnessTap == "" || cfg.hostNodeIP() == "" || cfg.guestNodeIP() == "" {
		return nil
	}
	return set(ctx, platform.NodeRoute{
		GuestIP: cfg.guestNodeIP(),
		Dev:     cfg.WitnessTap,
		Src:     cfg.hostNodeIP(),
		LLAddr:  deriveMAC(cfg.Node, "wit"),
	})
}

// hostNodeIP is the host's own address on the system subnet, bare. Empty when this host takes
// none -- then nothing is routed and the guest is told no private host address, which is the
// agent-less harnesses and any node whose substrate puts the host on the LAN anyway.
func (cfg Config) hostNodeIP() string { return bareIP(cfg.SystemHostCIDR) }

// guestNodeIP is the guest's address on the system subnet, bare -- the node IP, and what anything
// reaching this node dials (DESIGN §4).
func (cfg Config) guestNodeIP() string { return bareIP(cfg.SystemCIDR) }

// bareIP strips the prefix from a CIDR. "" in, "" out: an unset address is not an error here,
// it is a node that has none.
func bareIP(cidr string) string {
	if i := strings.IndexByte(cidr, '/'); i >= 0 {
		return cidr[:i]
	}
	return cidr
}

// deriveMAC returns a stable, unique MAC for a guest NIC from a seed (the node name, or the flock
// id for the VIP's NIC) + NIC role. QEMU's default MAC is identical across guests, so a fleet on a
// shared bridge needs distinct MACs or the NICs collide (ARP never resolves). Keeps QEMU's 52:54:00
// OUI and fills the low 3 bytes from a hash -- deterministic, so a NIC's MAC is stable across
// restarts.
func deriveMAC(node, role string) string {
	h := fnv.New32a()
	fmt.Fprintf(h, "%s/%s", node, role)
	s := h.Sum32()
	return fmt.Sprintf("52:54:00:%02x:%02x:%02x", byte(s>>16), byte(s>>8), byte(s))
}

// Config is the host agent's runtime configuration (from ConfigFromEnv today, the
// cloud enrollment seam later). It carries the guest VM spec, the DRBD resource +
// bring-up, and this node's identity.
type Config struct {
	// Guest VM (mapped onto platform.QEMUSpec).
	QEMUBinary  string
	QEMUDataDir string // qemu `-L` firmware dir; "" = qemu's default (set for the relocatable bundle)
	Accel       string
	CPUModel    string // qemu `-cpu`; "" = qemu's default (qemu64, x86-64-v1). See platform.QEMUSpec.
	MemoryMB    int
	Cores       int
	GuestDisk   string
	DataDisk    string
	ControlSock string
	// QMPSock is the host end of QEMU's monitor -- the channel to the VM itself, as opposed
	// to ControlSock, which reaches the guest OS inside it. It is what makes a
	// clean shutdown possible when the guest agent cannot be reached, and what a forced reset
	// goes through. Empty = launch with no monitor (the older behaviour, and what every
	// test that never stops a VM passes).
	QMPSock string
	// AdminSock is the local admin door `briard` submits directives to; "" disables it.
	// Distinct from ControlSock in both direction and peer: that one is the agent DIALING the
	// guest, this one is the agent LISTENING for an operator.
	AdminSock  string
	ServiceTap string
	SystemTap  string
	WitnessTap string // host tap for the guest's private witness NIC (eth3); "" = no witness link
	SerialLog  string
	// Net substrate for the service + system NICs. NetMode "" (NetBridge) = host
	// taps on a bridge, opened by name (today's install.sh substrate); "macvtap" (NetMacvtap) =
	// macvtap chardevs, which the NetWrapBin fd-passing wrapper attaches to qemu. NetWrapBin is
	// required in macvtap mode and unused otherwise.
	NetMode    string
	NetWrapBin string

	// Host-side cloud-witness forwarder. A managed pairing directive (MeshSpec.Witness)
	// starts the witness-forwarder here -- host-side, so the mTLS identity + WAN hop stay off the
	// guest ([[logic-on-host-by-default]]). ForwarderBin is the witness-forwarder binary; the
	// Cert/Key/CA are the anchor's mTLS identity (the cert path on the replicated volume).
	// Empty ForwarderBin -> a pairing that needs a forwarder fails (the node can't reach the cloud
	// witness), which is caught before any DRBD change.
	ForwarderBin string
	WitnessCert  string
	WitnessKey   string
	WitnessCA    string

	// Node identity + DRBD bring-up.
	Node string // this node's name
	// FlockID is the identity of the FLOCK rather than the node -- generated once at install into
	// pet state, and (later) handed to a joiner by the pairing directive. Only the VIP's MAC
	// derives from it, because the VIP is the one thing that must survive moving between nodes:
	// a MAC derived from the node name means node B draws a different DHCP lease, and the address
	// changes on failover, which is the one thing a VIP exists not to do. "" falls back to the node
	// name, which keeps every agent-less harness and every already-installed node exactly as it was.
	FlockID string
	// FlockName is the flock's HUMAN-VISIBLE name (`brave-elf`), minted once at install into pet
	// state and published as `briard-<name>.local`. It is flock-scoped for the same reason the VIP
	// is: the name resolves TO the VIP, so a node-scoped name would change identity on failover
	// while the thing it points at did not.
	//
	// It carries NO mechanism -- not the MAC, not the DHCP client-id, not DRBD's `on <name>` --
	// and that is the entire point: a rename is a label change, so it can never move the address.
	// "" means publish nothing rather than publish a guess.
	FlockName string
	Role      model.Role    // anchor / diskless ("" reserved for plain machines)
	Resource  drbd.Resource // the DRBD resource this node serves
	Promoter  []string      // drbd-reactor units in start order; nil on a witness
	// ServiceRendered is the runtime-installed service's units, re-derived from the node-local
	// manifest cache at start-up and handed to bring-up so they exist before the promoter looks
	// for them. Set alongside Promoter by installedService, for the same reason and from
	// the same source; zero when no service is installed.
	ServiceRendered quadlet.Rendered
	Diskless        bool // this node is a diskless witness
	FreshInit       bool // seed a fresh cluster (skip initial sync); exactly one node

	// System/DRBD NIC address (the private subnet). When SystemDev is set, the
	// agent configures it on the guest before bring-up so DRBD can use it; "" for a
	// single-node guest that replicates over loopback.
	SystemDev  string // guest NIC to address, e.g. "eth1" (the system NIC -- this node's node IP)
	SystemCIDR string // its address/prefix, e.g. "10.0.0.1/24"
	// SystemHostCIDR is the HOST's own address on the same system subnet, carried on its end of
	// the private link. It exists because a standby has no LAN presence of its own and still has
	// to be dialable by its guest (the witness forwarder) and able to answer on-link -- and
	// because the guest needs a source address to reply to when the host dials IT (the reboot
	// gate). "" = this host takes no system-subnet address, which is every agent-less harness.
	SystemHostCIDR string
	// WitnessDev is the GUEST's name for the NIC facing the private link -- the far end of
	// WitnessTap. Named by the host rather than assumed by the guest, which bakes no positional
	// knowledge of its own NIC layout ([V3b.16a]).
	WitnessDev string
	VIPDev     string // guest NIC the promoter claims the VIP on; "" = the guest's baked default
	// VIPAddr is the service address the promoter claims, in CIDR form ("192.168.9.50/24").
	// It is the LAN's address, not ours: baking it made the product work only on the one
	// subnet our lab happened to use, and fail GREEN everywhere else (the readiness probe
	// runs in-guest against an address the guest itself owns). "" = the guest's baked
	// fallback, which the agent-less harnesses (nixosTest/lib.nix) still rely on.
	VIPAddr string

	// Observability + north-bound control.
	HealthURL       string        // payload /healthz at the VIP; "" -> health follows quorum
	StatusEvery     time.Duration // observe cadence
	BringUpBudget   time.Duration // bounds the launch -> converge phase
	ControllerURL   string        // fleet controller base URL; "" -> don't report up (standalone)
	ControllerToken string        // bearer presented on every seam call; "" -> no auth
	AssignmentCache string        // where the cloud Assignment is cached for cold-boot; "" -> no persistence
	NotifyURL       string        // alert endpoint (ntfy topic URL); "" -> log-only notifier
	TelemetryPath   string        // where resource telemetry is written for the out-of-band soak collector; "" -> don't write
	MetricsWindow   time.Duration // rollup bucket for the cloud aggregate pipeline; 0 -> 1h (production)
	// UpgradeBudget bounds a whole-OS upgrade: stage, activate, health-gate, and — if the gate
	// never passes — the wait before it reverts. It is therefore also THE NUMBER THAT DECIDES
	// HOW LONG A BROKEN UPDATE LEAVES A NODE DEGRADED, which is why it is worth naming rather
	// than burying (will need to reason about exactly this interval).
	//
	// Configurable so the lab does not have to spend the production interval to observe a
	// rollback: os-rollback-demo.sh has three gated acts, and at the default that is ~48 minutes
	// of an hour-long run spent watching a countdown. The production default is unchanged and
	// pinned by a test — see TestUpgradeBudgetDefault for why a default, once configurable, needs
	// asserting rather than assuming.
	//
	// ⚠️ It also bounds STAGING, the one step that goes to the network. That is the reason the
	// default is generous and must stay so: a slow link fetching tens of MB has to fit inside it.
	// And a value below boot-plus-settle makes a gate expire before the node is even up, which
	// turns a broken-generation test into a timeout test that passes for the wrong reason.
	UpgradeBudget time.Duration

	// Payload upgrade: what an `upgrade` directive acts on. Service names the
	// payload + its data subvolume; ReactorSnippet enables promoter-coordinated quiesce
	//; SnapshotRetention bounds pre-upgrade snapshots. Empty Service.Name
	// -> the node doesn't drive upgrades (witness / no payload).
	Service           model.ServiceSpec
	ReactorSnippet    string
	SnapshotRetention int

	// Runtime service install. CatalogURL is the signed catalog root;
	// ServiceCache is where the installed manifest is kept NODE-LOCALLY so the agent can rebuild
	// the promoter chain at bring-up, before promotion, when the replicated copy on the volume is
	// not yet mountable. "" disables either.
	CatalogURL   string
	ServiceCache string

	// MeshCache is where a runtime pairing's MeshSpec is kept NODE-LOCALLY, for the same reason
	// ServiceCache exists and to close the same hole one step further out ([V3b.16b]).
	//
	// The mesh arrives as a cloud directive and is applied to the guest, which writes it into the
	// guest's /etc. But bring-up REWRITES the guest's .res from cfg.Resource on every pass, and
	// cfg.Resource comes from the PEERS env — which install.sh never sets. So without this a
	// runtime-paired anchor whose guest rebooted came back rewritten to the single-node self-peer,
	// against on-disk DRBD metadata recording real peers: exactly what RescueGuest refuses to do,
	// happening silently. The host must durably own what it told the guest.
	//
	// The SPEC is cached rather than the derived drbd.Resource: it is what the cloud actually said,
	// and the resource is re-derived from it the same way applyPair derives it. "" disables.
	MeshCache string

	// Mesh is the last pairing this node applied, restored from MeshCache at startup. The zero
	// value is "never paired" -- the shipped single-node state -- and reads correctly as such,
	// since a nil Mesh.Witness is exactly "no cloud witness in this mesh".
	//
	// It is kept whole, rather than only the drbd.Resource derived from it, because bring-up needs
	// the witness block too: the host-side forwarder is a transient unit that a host reboot ends,
	// and re-creating it is what makes a restored `.res` naming the witness true rather than
	// aspirational (restoreWitnessHop).
	Mesh api.MeshSpec

	// Overlay remote-reach; nil = LAN-only/standalone (the default). The agent
	// brings it up *after* the guest converges -- it's parallel to the failover path, so
	// a failure warns but never fails bring-up -- and reports its health through
	// NodeStatus.Overlay. Enrollment is coordinator-mediated: the setup key lives in the
	// provider's own config, minted by the controller (the node never holds the token).
	Overlay overlay.OverlayProvider

	// Host-agent self-update. UpdateKeyring is the PEM bundle of trusted Ed25519
	// release public keys the agent verifies an agent-update artifact against -- empty means
	// self-update is OFF (an agent-update directive refuses, fail-closed). UpdateBase /
	// UpdateRunDir locate the flat update slots (default selfupdate.Default*); UpdateUnit is the
	// systemd unit restarted to trial a staged binary (default "briard-agent.service"). Version
	// is this agent's running release id -- reported as NodeStatus.AgentVersion and used to make
	// a re-offer of the running version an idempotent no-op.
	UpdateKeyring []byte
	UpdateBase    string
	UpdateRunDir  string
	UpdateUnit    string
	Version       string

	// beat is the systemd watchdog keep-alive (V3.32), NOT configuration: Run builds it from the
	// environment systemd sets and it rides here for the same reason Overlay does -- so every
	// method that already holds a cfg can reach it without threading one more argument down a
	// call chain that is deep by design. nil is the ordinary state outside systemd (dev runs, the
	// lab fleet, tests) and every method on it is nil-safe, so a zero Config works everywhere.
	beat *beat

	// telemetry is the goroutine that owns TelemetryPath, and like beat it is machinery rather
	// than configuration: Run builds it and it rides here so writeTelemetry can reach it without
	// threading one more argument through observe. nil = telemetry off (the shipped state, and
	// every unit test); writeTelemetry is nil-safe. See telemetryWriter for why it is a
	// goroutine and not a deadline (B.87).
	telemetry *telemetryWriter

	// WedgeFIFO is a TEST FIXTURE, not a product knob: a path the observe loop opens each cycle
	// so agent-watchdog.nix can wedge that goroutine on purpose. Empty everywhere but that test.
	// See wedgeForTest for why this is explicit rather than borrowed from a defect.
	WedgeFIFO string
}

// statusReader is the DRBD/quorum slice of the guest client the snapshot needs -- kept
// small so it's unit-testable without a live control channel.
type statusReader interface {
	// Cluster reads this node's whole DRBD view -- its own quorum state AND its peers -- from
	// one sample. The peers are what let the redundancy alert tell "a peer is gone" from "the
	// only other copy of the data is gone" ([B.102]); they stop here and do not ride the cloud
	// wire (shared/model.Cluster). It is the same drbd.status verb the QuorumState summary
	// rides, so this reads strictly more from the same call rather than costing another.
	Cluster(ctx context.Context, resource string) (model.Cluster, error)

	// PayloadHealth probes the payload's /healthz from INSIDE the guest (macvtap-safe: the
	// host may not be able to reach the VIP). An error (e.g. an old guest that predates the
	// verb) falls the caller back to the legacy host-side probeHealth.
	PayloadHealth(ctx context.Context, url string) (bool, error)
	// VIP reads the address the service NIC actually holds, so the loop can probe an address
	// the host did not choose (a lease acquired by DHCP inside the guest). Resolution is
	// guest.ResolveHealthURL's — the observe loop and the readiness gate must answer "what do
	// we probe?" the same way, and one rule in two packages is two rules waiting to disagree.
	guest.VIPReader
	// MDNSPublished reads the name avahi ACTUALLY published, for exactly the reason VIP reads the
	// address off the interface: the value we asked for is not evidence of the value in force.
	// avahi conflict-renames on a collision silently, so two flocks in one house can even SWAP
	// names across a reboot with nothing telling anyone. "" when this node publishes none.
	MDNSPublished(ctx context.Context) (string, error)
}

// guestReader is what the observe loop reads each cycle: quorum state, the payload image
// the guest actually serves (the replicated pin), and the running system closure. Reading
// image + system from the guest -- not tracking them in-memory -- is what makes them
// correct across a failover, where a survivor converges to the pinned code without ever
// applying the upgrade directive.
type guestReader interface {
	statusReader
	PayloadImage(ctx context.Context) (string, error)
	SystemPath(ctx context.Context) (string, error)
	Resources(ctx context.Context, unit, dataDir string) (telemetry.NodeResources, error)
}

// Run boots the guest, drives bring-up to quorate primary (a witness just comes
// up, it never promotes), then observes the node's status until ctx is cancelled.
// The guest is always stopped on return.
func Run(ctx context.Context, cfg Config, logf func(string, ...any)) error {
	logf("%s", versionBanner(cfg.Version)) // name the running build before anything else
	// The watchdog keep-alive, built before anything that could block so every operation below is
	// covered. nil outside systemd. Set on the local cfg, which is the copy every call below takes.
	cfg.beat = newBeat(logf)
	// The telemetry writer, built once here and for the same reason the beat is: it must outlive
	// any single observe() call, since the re-dial loop below runs many of them and a second
	// writer on the same path would be two goroutines racing one file. nil when telemetry is off.
	cfg.telemetry = cfg.newTelemetryWriter(ctx, logf)
	// A service installed at RUNTIME is not described by the environment, so rebuild it
	// from the node-local manifest cache before anything derives from cfg. Without this an agent
	// restart would re-derive the chain from env alone and silently drop the installed service
	// out of the promoter — the node would come back serving nothing.
	if spec, chain, rendered, ok := cfg.installedService(logf); ok {
		logf("installed service %q restored from cache; promoter chain %v", spec.Name, chain)
		cfg.Service, cfg.Promoter, cfg.ServiceRendered = spec, chain, rendered
	}
	// A mesh joined at RUNTIME is not described by the environment either, and it is the more
	// dangerous of the two to forget: bring-up REWRITES the guest's .res from cfg.Resource every
	// time, so a paired anchor whose guest rebooted was rewritten back to the single-node self-peer
	// on 127.0.0.1 — against on-disk DRBD metadata recording real peers and a different node-id.
	// That is what RescueGuest refuses to do, happening on an ordinary reboot ([V3b.16b]).
	//
	// After installedService and for the same reason it is where it is: everything below derives
	// from cfg, so both restores must land before anything reads it.
	if spec, res, ok := cfg.cachedMesh(logf); ok {
		logf("paired mesh restored from cache: %s, %d peers (cloud witness: %t)",
			res.Name, len(res.Peers), spec.Witness != nil)
		cfg.Resource, cfg.Mesh = res, spec
	}
	// READY: the agent has started — config read, about to enter its loop. Deliberately NOT
	// "the node is healthy", which is what this used to mean and what a supervisor's readiness
	// must not mean; see the sdnotify package doc for the two costs of that coupling, the fatal
	// one being that an agent whose guest never converges would never arm its watchdog. Sent
	// BEFORE bringUp so the watchdog covers bring-up and the recovery ladder, which are the
	// stretches that most need it. No-op when not under Type=notify.
	if err := sdnotify.Ready(); err != nil {
		logf("sd_notify READY failed (non-fatal): %v", err)
	}
	g, client, err := cfg.bringUp(ctx, cfg.guestSpec(), logf)
	if err != nil {
		if errors.Is(err, errNoChannel) {
			return nil // never heard from the guest before the budget ran out -> clean shutdown
		}
		return err
	}
	defer client.Close()
	// NOTE: no `defer g.Stop()` — an agent shutdown/restart must be transparent to the
	// guest. g.Stop() is the explicit self-fence VM-destroy backstop,
	// invoked only on a wedged guest (the host-side reboot ladder) or by the reboot
	// path's rollback, which is about to revert the disk out from under it anyway.

	// Remote reach: bring the overlay up after convergence. It's off the
	// failover-critical path, so a failure warns but never fails bring-up -- LAN
	// operation is unaffected. Enrollment is idempotent (netbird `up` on an already-
	// enrolled node just reconnects).
	if cfg.Overlay != nil {
		if id, err := cfg.Overlay.EnrollNode(ctx, api.EnrollRequest{NodeName: cfg.Node}); err != nil {
			logf("overlay bring-up failed (non-fatal): %v", err)
		} else {
			logf("overlay up: name=%s addr=%s", id.Name, id.Address)
		}
	}

	// The guest binding an `upgrade` directive drives: a guest.Manager for the health-gated
	// payload/system upgrades, plus the VM handle the reboot path needs.
	// nil-safe: a witness / payload-less node just won't act on those directives. guestCfg is
	// hoisted so the reconnect loop can rebuild the Manager on a fresh connection.
	guestCfg := guest.Config{
		HealthURL: cfg.HealthURL,
		// The gate resolves its own probe target per call, for the same reason the observe
		// loop does: under DHCP the address is acquired inside the guest, and this gate is a
		// ROLLBACK TRIGGER — probing a stale address does not fail loudly, it reverts a
		// healthy node.
		VIPDev:            cfg.VIPDev,
		Diskless:          cfg.Diskless,
		Resource:          cfg.Resource.Name, // what OSReady asks about this node
		ReactorSnippet:    cfg.ReactorSnippet,
		SnapshotRetention: cfg.SnapshotRetention,
		// The Manager's own step-by-step lines went NOWHERE until now: with no Logf, NewManager
		// installs a no-op, so every "enter maintenance / quiesce / pin image / health-gate
		// tripped" line has been discarded in the field while looking, in the source,
		// like observability. Found while instrumenting the activation verdict and
		// wondering why the new line never appeared.
		Logf: logf,
	}
	// EVERY node with a guest gets this, not only one running a service. Gating construction on
	// `cfg.Service.Name != ""` would leave `up == nil` on a zero-service node, and applyDirective
	// would then refuse every OS upgrade with "no target/upgrader on this node" — i.e. the shipped
	// state, and the whole free tier, could not take an OS update. A system closure is a
	// property of the NODE; what happens to run on top of it cannot decide whether the node can
	// be updated. Payload directives stay correctly refused: their own guard still reads spec.Name.
	mgr := newOSUpgrade(cfg, g, client, guestCfg, logf)
	// Redundancy alerting: a data node warns when it loses a replica connection
	// while still serving. Ntfy if a topic URL is configured, else a log-only notifier
	// (the standalone fallback). A witness / single-node cluster has no redundancy signal.
	var n notify.Notifier
	var alerter *redundancyAlerter
	if cfg.Role != model.RoleDiskless {
		if cfg.NotifyURL != "" {
			n = notify.Ntfy(cfg.NotifyURL)
		} else {
			n = notify.Nop() // no endpoint: the fire() logf is the local trail, don't double-log
		}
		if peers := len(cfg.Resource.Peers) - 1; peers > 0 {
			alerter = newRedundancyAlerter(n, cfg.Node, peers, logf)
		}
	}
	// The one degradation the alerter above CANNOT report, because the same condition is what stops
	// it being built: a guest replicating to peers this host has no record of. Checked once, here,
	// now that both the channel and the notifier exist — see warnIfMeshForgotten.
	cfg.warnIfMeshForgotten(ctx, client, n, logf)
	// Resolve this node's identity once at boot -- register with the cloud and
	// cache the Assignment, or cold-boot from the cache during a cloud/WAN outage
	// (degrade-to-local; identity is never a boot dependency). The client is built once
	// here (nil = standalone) and the resolved tenant is tagged onto every report.
	var rep cloud.CloudClient
	if cfg.ControllerURL != "" {
		rep = cloud.NewHTTP(cfg.ControllerURL, cfg.ControllerToken)
	}
	// The node tells the cloud its IANA zone at registration, so its home can be
	// given an update window in LOCAL time. "" when the host does not say -- the cloud then
	// cannot schedule this home and treats that as a fault, which is the honest outcome.
	assignment := cloud.Resolve(ctx, rep, cfg.AssignmentCache,
		api.NodeInfo{NodeName: cfg.Node, Role: cfg.Role, Timezone: localTimezone("/")}, logf)

	// Roll this node's per-cycle resource samples into hourly aggregates and upload
	// them up the cloud seam (never raw). Only a payload node reporting to a cloud has
	// dashboard metrics (volume/load); a witness or standalone node has none, so the
	// aggregator stays nil and observe skips the upload. Built once here so it survives the
	// reconnect loop (like the alerter).
	var agg *metricsAggregator
	if rep != nil && cfg.Service.Name != "" {
		agg = newMetricsAggregator(cfg.MetricsWindow, logf)
	}

	// The local admin door. Started ONCE, outside the re-dial loop below, so a bounced
	// guest channel doesn't take the operator's CLI down with it — the socket outlives any
	// single observe() call, which is the whole point of an out-of-band admin surface.
	local := make(chan localRequest)
	go serveLocal(ctx, cfg.AdminSock, local, logf)

	// Observe with reconnect. The guest agent serves one connection then exits
	// (systemd restarts it), and a per-call deadline also closes the channel — either way
	// observe returns ErrChannelDown, and we re-dial + re-handshake rather than go blind
	// forever (the older behaviour: one drop and the host never sees the guest again).
	// A warm guest just re-attaches and observe resumes; a guest that stays unreachable past
	// the recovery window gets its VM restarted, and after K of those the host stops and says
	// so (guestrecover.go, B.22b). A cancelled ctx is a clean shutdown.
	//
	// TERMINAL OUTCOMES LIVE HERE, NOT IN observe, and is why. observe returns on a dead
	// channel, so anything held in its frame goes with it — and an OS upgrade ALWAYS bounces the
	// channel, since both of its methods restart the guest. So the directive whose outcome most
	// needed reporting was the one guaranteed to lose it: the next observe reported no outcomes,
	// the controller still saw the directive in flight, and re-delivered it. The observed-state
	// backstop cannot cover the gap either — it closes a directive when the reported
	// system MATCHES the target, which is precisely what a rolled-back upgrade does not do. Both
	// closure mechanisms failing on the same case is what made this a deterministic loop on a
	// broken release rather than an occasional retry.
	var pendingOutcomes []api.DirectiveOutcome
	var recovery guestRecovery
	for {
		served := time.Now()
		err := cfg.observe(ctx, client, mgr, alerter, n, rep, agg, assignment.Tenant, local, &pendingOutcomes, logf)
		if ctx.Err() != nil {
			return nil
		}
		if !errors.Is(err, guestagent.ErrChannelDown) {
			return err // observe only returns nil (handled above) or ErrChannelDown
		}
		// How long the channel that just died had been up is the only evidence available for
		// whether this is a NEW incident or the same guest failing again, and it has to be
		// taken here — recover() is called once per drop and cannot see the stretch before it.
		recovery.served(time.Since(served))
		logf("control channel down (%v); reconnecting", err)
		_ = client.Close()
		// An OS upgrade that rebooted the guest has already re-established the
		// channel on its way through, so adopt that one rather than dial. Dialling would not
		// merely duplicate it: the guest agent serves a single connection at a time, so a
		// second dial would block until this one was dropped.
		if mgr != nil && mgr.channel() != client {
			client = mgr.channel()
			logf("adopting the channel the upgrade path re-established")
			continue
		}
		// Wait for the guest to come back, and restart its VM if it does not. Rebinding the
		// Manager is part of the swap recover() owns — a reboot replaces the channel and the VM
		// together, so Run cannot rebind to a fresh channel without also being told about a
		// fresh VM, which is why this is no longer a bare reconnect + rebind here.
		client, err = mgr.recover(ctx, &recovery, n)
		if err != nil {
			return nil // ctx cancelled while recovering — clean shutdown
		}
	}
}

// GuestSpec describes this node's guest VM. It is derived from cfg alone, so any launch can
// rebuild it identically — which is what the reboot path needs: it relaunches the
// same guest with the boot selector armed, and arming the selector must be the *only*
// difference between the two launches.
func (cfg Config) guestSpec() platform.QEMUSpec {
	return platform.QEMUSpec{
		Binary:      cfg.QEMUBinary,
		DataDir:     cfg.QEMUDataDir,
		Accel:       cfg.Accel,
		CPUModel:    cfg.CPUModel,
		MemoryMB:    cfg.MemoryMB,
		Cores:       cfg.Cores,
		DiskImage:   cfg.GuestDisk,
		DataDisk:    cfg.DataDisk,
		ControlSock: cfg.ControlSock,
		QMPSock:     cfg.QMPSock,
		ServiceTap:  cfg.ServiceTap,
		// The service NIC carries the VIP and nothing else, so its MAC is the VIP's identity --
		// flock-scoped, not node-scoped (see Config.FlockID). The DRBD and witness NICs stay
		// node-derived: those MUST differ per node or the NICs collide and ARP never resolves.
		ServiceMAC: deriveMAC(orNode(cfg.FlockID, cfg.Node), "svc"),
		SystemTap:  cfg.SystemTap,
		SystemMAC:  deriveMAC(cfg.Node, "sys"),
		WitnessTap: cfg.WitnessTap,
		WitnessMAC: deriveMAC(cfg.Node, "wit"),
		NetMode:    cfg.NetMode,
		NetWrapBin: cfg.NetWrapBin,
		SerialLog:  cfg.SerialLog,
	}
}

// logAcceleration records, once per bring-up, whether this guest is being accelerated by KVM
// or emulated by TCG -- asking QEMU (`query-kvm`) rather than trusting the argv, because
// Accel is a fallback list and the fall back to emulation is silent.
//
// It exists for a question that gets asked LATER: "why is this node slow?". Emulation is a
// supported configuration, not a fault (a host with no virtualisation extensions still gets a
// working briard, which is the point of the tcg fallback), so nothing fails and nothing alerts
// -- and that is exactly what makes it invisible. Without this line the first honest answer
// costs someone a bisect; with it, the answer is in the journal from the first boot.
//
// The healthy case is logged too, deliberately. A check that only speaks up when something is
// wrong leaves a reader unable to tell "accelerated" from "the check never ran", and this one
// is a one-liner per bring-up.
//
// Never fatal: a guest that boots and serves is not made worse by an unanswered question about
// its accelerator. The install-time counterpart is the report card's virt-flags advisory --
// that one is a prediction about a host, this is an observation about a running VM.
func logAcceleration(ctx context.Context, g *platform.Guest, spec platform.QEMUSpec, logf func(string, ...any)) {
	if spec.QMPSock == "" {
		return // no monitor to ask (the tests that launch without one); not a finding
	}
	cpu := spec.CPUModel
	if cpu == "" {
		cpu = "qemu default"
	}
	enabled, present, err := g.Accelerated(ctx)
	switch {
	case err != nil:
		logf("could not ask qemu whether KVM is in use (%v); accel was requested as %q", err, spec.Accel)
	case enabled:
		logf("guest accelerated by KVM (accel=%q cpu=%q)", spec.Accel, cpu)
	default:
		// Spelled out at length because the reader of this line is, by construction, someone
		// who did not know it applied to them -- and the two causes have different fixes.
		why := "this host has no virtualisation extensions available (check the firmware, or nested virt if this host is itself a VM)"
		if present {
			why = "the host HAS KVM but qemu could not use it (check /dev/kvm permissions and that the kvm module is loaded)"
		}
		logf("WARNING: this guest is EMULATED, not accelerated — qemu fell back to TCG: %s. "+
			"It runs correctly but roughly an order of magnitude slower, and disk/crypto-heavy work "+
			"(sha256, TLS, checksums) suffers most. Requested accel=%q cpu=%q. "+
			"This is a supported configuration, so nothing else will report it: start any performance "+
			"question here.", why, spec.Accel, cpu)
	}
}

// errNoChannel reports that the guest never spoke: qemu never bound the control socket, or
// an adopted guest's agent never re-served, before the bring-up budget ran out. It is kept
// distinct from a bring-up *failure* because the two deserve opposite treatment — never
// having reached the guest is a clean give-up (the agent exits, systemd restarts it, and the
// whole sequence runs again from scratch), whereas a bring-up that got as far as speaking to
// the guest and then failed is a real error the caller must report and act on.
var errNoChannel = errors.New("host: control channel never came up")

// BringUp takes the guest from nothing to converged: launch it (or re-adopt one that
// outlived an agent restart), establish the control channel, apply the runtime identity the
// image does not carry, and converge DRBD.
//
// It is ONE call because a reboot has to redo ALL of it. A guest that comes back
// from a reboot is the image's baked `guest` again, with unaddressed NICs: SetHostname and
// ConfigureNet are applied at runtime and never baked, so a relaunch that re-established
// only the control channel would leave DRBD unable to match its `on <name>` stanzas. Every
// step here is idempotent precisely so it can be re-driven — which is also why
// re-adopting an already-converged guest costs nothing.
//
// On success the client is live and the caller owns closing it. On any error it is closed
// here, and the guest is left running: bring-up failing is not grounds to destroy a VM (the
// self-fence is a separate, explicit decision).
func (cfg Config) bringUp(ctx context.Context, qspec platform.QEMUSpec, logf func(string, ...any)) (*platform.Guest, *guestagent.Client, error) {
	// Launch the guest as a transient systemd service, or RE-ADOPT one already
	// running. The guest's lifecycle is decoupled from the agent's, so a
	// `systemctl restart briard-agent` (e.g. an agent self-update) leaves qemu
	// serving; the restarted agent finds it active and re-adopts over the persisted
	// control socket instead of booting a second VM.
	// Bound the whole launch -> converge phase, including the wait for qemu to bind the control
	// socket below. HOISTED ABOVE THE LAUNCH (V3.32): the comment always claimed to bound "the
	// whole launch -> converge phase" while the deadline actually started after the launch, so
	// the systemd calls that start the VM ran under the caller's unbounded context. Bounding them
	// matches what this always said it did, and it is what lets the watchdog lease cover bring-up
	// rather than only its tail.
	bringup, cancel := context.WithTimeout(ctx, cfg.BringUpBudget)
	defer cancel()
	// The watchdog rides this operation's own budget: pings until bring-up finishes (the cancel
	// above) or overruns it. See beat.go for why the lease is the budget rather than a flag.
	cfg.beat.Lease(bringup)

	var g *platform.Guest
	adopted := platform.Running(bringup, qspec)
	if adopted {
		logf("re-adopting running guest (%s)", platform.GuestUnit)
		g = platform.Adopt(qspec)
	} else {
		var err error
		if g, err = platform.Launch(bringup, qspec); err != nil {
			return nil, nil, fmt.Errorf("host: launch guest: %w", err)
		}
	}

	spec := guestagent.BringUpSpec{
		Resource:  cfg.Resource,
		Diskless:  cfg.Diskless,
		FreshInit: cfg.FreshInit, // exactly one node seeds; the rest sync from it
		Promoter:  cfg.Promoter,
		// The guest's /run is tmpfs, so a reboot took these with it while the host's cache kept
		// the manifest. Re-rendered here, before ReactorStart, or the promoter starts a chain
		// whose units are gone.
		ServiceUnits:  cfg.ServiceRendered.Files,
		ServiceImages: cfg.ServiceRendered.ImageUnits,
	}
	// Establish the control channel. On a fresh launch, wait patiently for qemu to bind the
	// socket rather than exiting on the first "connection refused": exiting crash-loops
	// the agent -- systemd restarts it, the guest-disk reformat (briard-fleet-pre) re-runs, and
	// the guest never boots, so the node stays down. The mid-run channel is already this
	// resilient; the initial connection must be too. On a RE-ADOPT, the prior
	// host's disconnect bounced the in-guest agent (it serves one connection then exits,
	// systemd restarts it), so the handshake must RETRY with a bounded per-attempt deadline
	// until the guest re-serves — a single blocking handshake would eat the whole bring-up
	// budget. reconnect() is exactly that retry loop; use it for adopt, and the patient
	// one-shot dial for a fresh launch.
	var client *guestagent.Client
	var err error
	if adopted {
		if client, err = reconnect(bringup, cfg.ControlSock, logf); err != nil {
			return nil, nil, fmt.Errorf("%w: %w", errNoChannel, err)
		}
	} else {
		conn, derr := dialControl(bringup, cfg.ControlSock)
		if derr != nil {
			return nil, nil, fmt.Errorf("%w: %w", errNoChannel, derr)
		}
		client = guestagent.NewClient(conn)
		// Handshake first: learn the guest's protocol version + capabilities and refuse
		// a guest this host can't drive, before sending any real verb -- a skewed guest (e.g.
		// a survivor on a newer OS generation after a rolling update) is a safe deferral, not
		// silent misbehaviour.
		hello, herr := client.Handshake(bringup)
		if herr != nil {
			err = herr
		} else {
			logf("guest protocol v%d, %d capabilities", hello.Version, len(hello.Capabilities))
		}
	}
	// Say what the VM actually got, now that qemu is provably up. See logAcceleration.
	logAcceleration(bringup, g, qspec, logf)
	if err == nil {
		// Rename the guest to this node first: DRBD matches the running hostname against
		// the `on <name>` stanzas, so create-md fails until the (baked "guest") image is
		// renamed to cfg.Node. Then address the system NIC -- DRBD binds/connects on it,
		// so it must be up before drbd@<res>.target starts.
		err = client.SetHostname(bringup, cfg.Node)
	}
	// Configure the guest's NICs when either a system NIC is given -- which since [V3b.26b] is
	// EVERY installed node, lone ones included: eth1 carries this node's node IP, the one address
	// anything uses to reach it (DESIGN §4), and DRBD binds there -- OR a VIP device is, which is
	// what the agent-less harnesses send (then ConfigureNet records VIP_DEV/VIP_ADDR and skips
	// addressing).
	if err == nil && (cfg.SystemDev != "" || cfg.VIPDev != "" || cfg.VIPAddr != "") {
		err = client.ConfigureNet(bringup, guestagent.NetConfig{
			Dev: cfg.SystemDev, CIDR: cfg.SystemCIDR, VIPDev: cfg.VIPDev, VIPAddr: cfg.VIPAddr,
			PrivDev: cfg.WitnessDev, PrivHostIP: cfg.hostNodeIP(),
		})
	}
	// ...and the host's own path back to it. The guest just installed its half; this is the
	// mirror, and it is what makes the node IP mean the same thing from outside the guest as
	// from inside it. Warn rather than fail: a host that cannot reach its own guest is degraded
	// (the reboot gate reads unreachable, which it treats as "allowed" -- the pre-gate
	// behaviour), not broken, and the rest of bring-up is what the household actually needs.
	if err == nil {
		if rerr := cfg.setNodeRoute(bringup, platform.SetNodeRoute); rerr != nil {
			logf("node route: %v -- this host cannot reach its guest at %s", rerr, cfg.guestNodeIP())
		}
	}
	// Put this anchor's host-side hop to the cloud witness back, if its mesh has one. HERE, before
	// DRBD comes up and long before WaitQuorate, because a witness that is not reachable when quorum
	// is being counted is a witness that does not vote -- see restoreWitnessHop for why it is placed
	// at this point and why it warns rather than fails.
	if err == nil {
		cfg.restoreWitnessHop(bringup, client, platformWitness{}, logf)
	}
	// Hand the guest the flock's VISIBLE name, separately from addressing and separately from the
	// hostname above -- three identifiers, three calls, because that is what makes any one of them
	// changeable without the others. "" is a node with no minted name (every agent-less harness,
	// and any node installed before V3.20): the verb publishes nothing rather than a guess.
	if err == nil && cfg.FlockName != "" {
		err = client.SetMDNSName(bringup, cfg.FlockName)
	}
	if err == nil {
		err = client.BringUp(bringup, spec)
	}
	if err == nil && len(spec.Promoter) > 0 {
		// A data node converges when quorate: drbd-reactor then promotes exactly one
		// of them to Primary (VIP + payload), the rest settle Secondary. Gating on
		// quorate (not Primary) is what lets a multi-node cluster's secondaries
		// converge instead of hanging. A witness has no promoter -- it just comes up.
		err = client.WaitQuorate(bringup, cfg.Resource.Name, guestagent.DefaultPollInterval)
	}
	if err != nil {
		_ = client.Close()
		return nil, nil, fmt.Errorf("host: bring-up: %w", err)
	}
	logf("CONVERGED node=%s role=%s", cfg.Node, cfg.Role)
	return g, client, nil
}

// reconnect re-dials the control socket and re-handshakes, retrying with backoff until it
// succeeds or ctx is cancelled. It recovers a bounced guest agent (it serves one
// connection then exits, and systemd restarts it) without disturbing a warm guest — the
// host just re-attaches. The first attempt waits one backoff so systemd has a moment to
// restart the agent. A guest that stays unreachable keeps us here; escalating to a VM
// reboot is B.22b.
func reconnect(ctx context.Context, sock string, logf func(string, ...any)) (*guestagent.Client, error) {
	backoff := time.Second
	for attempt := 1; ; attempt++ {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(backoff):
		}
		client, err := connectAndHandshake(ctx, sock)
		if err == nil {
			logf("control channel reconnected (attempt %d)", attempt)
			return client, nil
		}
		logf("reconnect attempt %d failed: %v", attempt, err)
		if backoff < 15*time.Second {
			backoff *= 2
		}
	}
}

// dialControl connects to the guest control socket, retrying until qemu binds it or ctx ends.
// The initial connection must be as patient as the mid-run reconnect: on a container
// restart qemu may not have bound the socket when the host agent starts, and returning on the
// first "connection refused" exits the agent -> systemd restarts it -> the guest-disk reformat
// re-runs -> the guest never boots (a crash-loop that leaves the node down). Staying in
// this loop keeps the agent process alive so the qemu it just launched can finish binding.
func dialControl(ctx context.Context, sock string) (net.Conn, error) {
	backoff := 200 * time.Millisecond
	for {
		conn, err := net.Dial("unix", sock)
		if err == nil {
			return conn, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(backoff):
		}
		if backoff < 2*time.Second {
			backoff *= 2
		}
	}
}

// connectAndHandshake dials the control socket and negotiates the protocol — the
// handshake both proves the channel is live and re-learns the (possibly restarted) guest's
// capabilities. Bounded so a mute guest doesn't hang the dial.
func connectAndHandshake(ctx context.Context, sock string) (*guestagent.Client, error) {
	dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	conn, err := net.Dial("unix", sock)
	if err != nil {
		return nil, err
	}
	client := guestagent.NewClient(conn)
	if _, err := client.Handshake(dctx); err != nil {
		_ = client.Close()
		return nil, err
	}
	return client, nil
}

// Observe reads the node's status on a fixed cadence until ctx is cancelled or the control
// channel drops. A cancelled ctx is a clean shutdown (nil); a dropped channel returns
// ErrChannelDown so Run reconnects.
// pending is OWNED BY Run, not by this call: an outcome collected here must survive the channel
// re-dial that an OS upgrade always causes, and a local would not.
func (cfg Config) observe(ctx context.Context, r guestReader, up upgrader, alerter *redundancyAlerter, n notify.Notifier, rep cloud.CloudClient, agg *metricsAggregator, tenant string, local <-chan localRequest, pending *[]api.DirectiveOutcome, logf func(string, ...any)) error {
	t := time.NewTicker(cfg.StatusEvery)
	defer t.Stop()
	cr := &certRequester{}         // node-side CSR handshake state, lives for the observe loop
	su := cfg.newSelfUpdater(logf) // signed host-agent self-update; nil when no keyring provisioned
	// The host's own route to the VIP its guest holds, over the private link -- the one address
	// macvtap hides from the machine running the guest and from nobody else ([V3b.19]). Lives for
	// the observe loop because it remembers what it installed; see viproute.go.
	vr := newVIPRouter(cfg.WitnessTap, cfg.VIPDev)
	// EVERY cfg.beat.Beat() below sits in front of one ctx-BOUNDED call, and that is the whole
	// rule: the watchdog threshold is the longest gap between two pings, so a ping goes wherever
	// a gap would otherwise open. It is not one ping per cycle -- these calls carry 5s deadlines
	// each, so a slow-but-entirely-healthy iteration can run ~35s and would force a threshold
	// four times larger than it needs to be. A datagram costs nothing; a gap costs detection
	// latency. See beat.go.
	for {
		// The served image + running system are read from the guest each cycle (the
		// replicated pin / current-system), so they're correct even on a survivor that
		// converged-at-promotion. The image is also the payload-upgrade baseline.
		cfg.beat.Beat()
		img := cfg.currentImage(ctx, r)
		cfg.beat.Beat()
		sys := cfg.currentSystem(ctx, r)
		cfg.beat.Beat()
		st, cl, probe, err := cfg.snapshot(ctx, r, img, sys)
		// AHEAD of the channel-down return, and that placement is the load-bearing part. A dead
		// channel is precisely when the local guest may have stopped serving and a PEER may have
		// taken the VIP over -- the case where a route left pointing at our own guest replaces a
		// working LAN path with a black hole. Reconciling here withdraws it; reconciling after the
		// return would keep it exactly when it is most wrong.
		//
		// The address is asked for separately from the snapshot's health resolution, and
		// deliberately: that one prefers the CONFIGURED address when there is one, which on a node
		// that is not currently serving is an address this guest does not hold. A route may only
		// follow ground truth.
		cfg.beat.Beat()
		vr.reconcile(ctx, r, logf)
		if errors.Is(err, guestagent.ErrChannelDown) {
			return err // channel dead -> Run re-dials; a verb error just reports degraded
		}
		cfg.beat.Beat()
		st.Overlay = cfg.overlayStatus(ctx) // remote-reach signal (nil when no overlay)
		st.Tenant = tenant                  // tag the report with the assigned tenant
		// Resource telemetry is a soak leak-instrument, not product-health: it does NOT ride
		// the report. Measured each cycle and written to the out-of-band collector
		// file the soak reads L0-side; best-effort, never gates the observe loop.
		cfg.beat.Beat()
		res := cfg.resources(ctx, r)
		cfg.writeTelemetry(res, logf) // a handoff, never a write: see telemetryWriter (B.87)
		// The deliberate wedge point, off unless a test arms it. It sits HERE, where the
		// un-ctx'd write used to be, so what agent-watchdog.nix measures is a stall at the same
		// place in the same loop. See wedgeForTest.
		cfg.wedgeForTest(logf)
		// Fold this cycle's sample into the hourly rollup and upload the aggregates
		// (never raw) up the cloud seam -- the product-health subset re-added deliberately
		//. Best-effort like the rest of telemetry: a failed upload keeps the buckets and
		// Retries next cycle; only a success prunes the completed ones.
		if agg != nil && rep != nil {
			agg.add(time.Now(), res)
			cfg.beat.Beat()
			mctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			if err := rep.ReportMetrics(mctx, cfg.Node, agg.snapshot()); err != nil {
				logf("metrics upload failed: %v", err)
			} else {
				agg.prune(time.Now())
			}
			cancel()
		}
		logf("status node=%s role=%s primary=%t quorate=%t connected=%d healthy=%t probe=%s image=%s%s",
			st.NodeName, st.Role, st.Quorum.Primary, st.Quorum.Quorate, st.Quorum.Connected, st.Healthy,
			orDash(probe), st.Image, resourceLog(res))
		alerter.observe(ctx, cl) // edge-triggered redundancy warning (nil-safe on witness/single-node)
		if rep != nil {
			cfg.beat.Beat()
			rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			csr := cr.pendingCSR // ride a queued CSR up (nil on every ordinary report)
			directives, err := rep.Report(rctx, api.ReportRequest{Status: st, CSR: csr, Outcomes: *pending})
			cancel()
			if err != nil {
				logf("report to controller failed: %v", err) // transient: keep observing (outcomes retry)
			} else {
				if csr != nil {
					cr.pendingCSR = nil // uploaded; keep the key stashed until the cert returns
				}
				*pending = nil // acked -- collect this cycle's fresh outcomes below
				for _, d := range directives {
					// Per directive, not per batch: a batch of slow ones would otherwise open
					// exactly the gap this rule exists to close. The legs that block for minutes
					// (the upgrade path, the recovery ladder) take their own lease.
					cfg.beat.Beat()
					o := cfg.dispatch(ctx, d, r, up, img, n, cr, su, logf)
					cfg.adoptInstalledService(d, o, logf)
					if o.ID != "" {
						*pending = append(*pending, o)
					}
				}
				// Once a staged agent-update's outcome has drained (this report acked the
				// prior cycle's outcomes and none are pending), restart the unit so the
				// pivot trials the new binary. Deferring past the outcome ack keeps announce-
				// before-act intact; the restart is detached, so the guest is undisturbed.
				if su != nil && su.Armed() && len(*pending) == 0 {
					logf("agent-update: armed and acked -- restarting to trial the staged binary")
					if err := su.Restart(ctx); err != nil {
						logf("agent-update: restart request failed (will retry next cycle): %v", err)
					}
				}
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case rq := <-local:
			// A directive submitted through the local admin door. Applied HERE, in the
			// observe loop, for exactly the reason cloud directives are: the agent owns the guest
			// control channel, so an admin op must never run concurrently with a cycle. Arriving
			// on the select also WAKES the loop, so the CLI isn't left waiting out a StatusEvery
			// tick before anything happens.
			//
			// Its outcome goes back to the CLI and NOWHERE ELSE — deliberately not appended to
			// pendingOutcomes. Outcomes close the loop on an intent the cloud announced;
			// reporting one for an ID the cloud never issued would be, at best, noise in a ledger
			// whose whole value is that every row answers a question someone asked.
			cfg.beat.Beat()
			o := cfg.dispatch(ctx, rq.d, r, up, img, n, cr, su, logf)
			rq.resp <- o // answer the CLI first; adopting is bookkeeping it need not wait on
			cfg.adoptInstalledService(rq.d, o, logf)
		case <-t.C:
		}
	}
}

// Dispatch routes one directive to the subsystem that can act on it, and is the single place
// that decision is made — the local door and the cloud's down-channel both come
// through here, so "what does this node do with a directive" cannot drift between them.
func (cfg Config) dispatch(ctx context.Context, d api.Directive, r guestReader, up upgrader, img string, n notify.Notifier, cr *certRequester, su selfUpdater, logf func(string, ...any)) api.DirectiveOutcome {
	if d.Kind == api.DirectiveServiceInstall || d.Kind == api.DirectiveServicePrewarm {
		// A service install needs the render/provision/bracket verbs, none of which the narrow upgrader has.
		i, ok := r.(serviceInstaller)
		if !ok {
			return api.DirectiveOutcome{ID: d.ID, State: api.OutcomeFailed, Detail: "guest client cannot install a service"}
		}
		if d.Kind == api.DirectiveServicePrewarm {
			return cfg.applyServicePrewarm(ctx, i, d, logf)
		}
		return cfg.applyServiceInstall(ctx, i, d, logf)
	}
	if d.Kind == api.DirectiveHandover {
		// A planned handover needs the guest's promoter verb, not the upgrader.
		e, ok := r.(guestEvictor)
		if !ok {
			return api.DirectiveOutcome{ID: d.ID, State: api.OutcomeFailed, Detail: "guest client cannot hand over"}
		}
		return cfg.applyHandover(ctx, e, d, logf)
	}
	if d.Kind == api.DirectiveSync {
		// The pre-eviction flush, standalone: the same narrow slice of the guest as the
		// handover it precedes.
		e, ok := r.(guestEvictor)
		if !ok {
			return api.DirectiveOutcome{ID: d.ID, State: api.OutcomeFailed, Detail: "guest client cannot sync"}
		}
		return cfg.applySync(ctx, e, d, logf)
	}
	if d.Kind == api.DirectivePair {
		// Runtime anchor pairing needs the guest client's mesh verbs
		// (adjust/bring-up), not the narrow upgrader; r is that client.
		m, ok := r.(guestMesher)
		if !ok {
			return api.DirectiveOutcome{ID: d.ID, State: api.OutcomeFailed, Detail: "guest client cannot reconcile a mesh"}
		}
		return cfg.applyPair(ctx, m, platformWitness{}, d, logf)
	}
	return applyDirective(ctx, d, up, cfg.Service, img, n, cr, su, logf, cfg.UpgradeBudget, cfg.beat)
}

// OverlayStatus reads the overlay's health for the status snapshot. Nil when no
// overlay is configured; on a read error it reports Up=false (a signal) rather
// than nil -- a transient overlay hiccup must not break the observe loop, since
// the overlay is off the failover-critical path.
func (cfg Config) overlayStatus(ctx context.Context) *api.OverlayStatus {
	if cfg.Overlay == nil {
		return nil
	}
	hctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	h, err := cfg.Overlay.Health(hctx)
	if err != nil {
		return &api.OverlayStatus{Up: false}
	}
	return &api.OverlayStatus{Up: h.Up, Relayed: h.Relayed, PeersUp: h.PeersUp}
}

// resourceLog renders the trend inputs for the status line so a soak's own logs carry the
// growth signals at a glance. Empty when unmeasured.
func resourceLog(r *telemetry.NodeResources) string {
	if r == nil {
		return ""
	}
	return fmt.Sprintf(" agentRSS=%dk payloadRSS=%dk fds=%d/%d vol=%dk snaps=%d load1=%.2f restarts=%d kerr=%d",
		r.AgentRSSKB, r.PayloadRSSKB, r.AgentFDs, r.PayloadFDs, r.VolumeUsedKB, r.SnapshotCount, r.Load1,
		r.PayloadRestarts, len(r.KernelErrors))
}

// Resources gathers the node's resource telemetry for the soak trend oracle: the
// host agent's own footprint (measured in-process, so it's present on every node -- witness
// included -- as the "does the product daemon leak" surface) plus the appliance's, read from
// the guest via the sys.resources verb. Best-effort throughout: a witness or payload-less
// node reports agent-only telemetry, and a guest read hiccup degrades to agent-only rather
// than blanking the report or breaking the observe loop. Never nil (the agent footprint
// always measures), so the trend oracle always has a numeric sample.
func (cfg Config) resources(ctx context.Context, r guestReader) *telemetry.NodeResources {
	var res telemetry.NodeResources
	if cfg.Service.Name != "" {
		rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		// ServingUnit, not a name rebuilt here: a runtime-installed service's units come from the
		// quadlet renderer (briard-<service>-<container>.service), so "podman-<name>.service" named
		// nothing, systemd answered with no MainPID, and PayloadRSS/PayloadFDs/PayloadRestarts sat
		// at zero for every catalog-installed service. Restarts is the crash-loop signal, so a
		// service that crash-looped read exactly like one that never restarted.
		if app, err := r.Resources(rctx, cfg.Service.ServingUnit(), cfg.Service.DataDir); err == nil {
			res = app // appliance fields; agent fields (zero here) filled below
		}
		cancel()
	}
	res.AgentRSSKB, res.AgentFDs = selfResources()
	return &res
}

// telemetryWriter owns the ONLY goroutine that touches TelemetryPath, and it exists because the
// least important thing in the observe loop was the one thing able to stop it (B.87). Every
// other call in that loop carries a 5s deadline. This one could not: there is no ctx-aware file
// write in the standard library, so a deadline handed to os.WriteFile would be decoration --
// open(2) on a hung mount (an unresponsive NFS or FUSE path under TELEMETRY_PATH) blocks
// uninterruptibly and no timer reaches it. The answer is therefore isolation rather than a
// bound: the write happens on a goroutine of its own and the loop only ever does a
// non-blocking send, so the worst a stuck path can cost is the samples themselves.
//
// Latest-wins, depth 1, drop when the writer is busy -- and that is not a concession forced by
// the channel, it is what the file already meant. writeTelemetryFile publishes the newest
// sample by atomic rename and keeps no history, so a sample the writer never got to is
// indistinguishable from one it overwrote a cycle later.
type telemetryWriter struct {
	ch chan *telemetry.NodeResources
	// dropped edge-triggers the log below. Touched ONLY by the sender (observe is the sole
	// caller of writeTelemetry, and there is one observe at a time), never by the goroutine, so
	// it needs no lock.
	dropped bool
}

// newTelemetryWriter starts that goroutine, or returns nil when no path is configured -- the
// shipped state, since install.sh sets no TELEMETRY_PATH. writeTelemetry is nil-safe, so a
// Config built without one (every unit test, every standalone node) still works.
//
// The goroutine is deliberately NOT joined at shutdown. A writer wedged in open(2) cannot be
// cancelled by anything short of the mount coming back, so waiting for it would reintroduce the
// hang one call further out; the process exits over it instead, which is exactly what a
// best-effort instrument should be allowed to cost.
func (cfg Config) newTelemetryWriter(ctx context.Context, logf func(string, ...any)) *telemetryWriter {
	if cfg.TelemetryPath == "" {
		return nil
	}
	w := &telemetryWriter{ch: make(chan *telemetry.NodeResources, 1)}
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case res := <-w.ch:
				cfg.writeTelemetryFile(res, logf)
			}
		}
	}()
	return w
}

// WriteTelemetry hands this cycle's resource sample to the writer goroutine, and never waits.
// Nil-safe: no TelemetryPath (or a Config that never started a writer) means telemetry is off.
func (cfg Config) writeTelemetry(res *telemetry.NodeResources, logf func(string, ...any)) {
	w := cfg.telemetry
	if w == nil {
		return
	}
	select {
	case w.ch <- res:
		if w.dropped {
			logf("telemetry writer caught up; sampling again")
			w.dropped = false
		}
	default:
		// The writer is still inside the previous write: a slow or hung TELEMETRY_PATH. Drop
		// this sample and carry on -- the loop this instruments must never wait on it. Edge-
		// triggered, so a path that never comes back says so once instead of every cycle.
		if !w.dropped {
			logf("telemetry write is not keeping up (%s); dropping samples until it does", cfg.TelemetryPath)
			w.dropped = true
		}
	}
}

// WriteTelemetryFile publishes one resource sample to the out-of-band collector file the soak
// reads L0-side -- the internal host→lab channel that replaces putting telemetry on the cloud
// report. Latest-wins via atomic write-rename (a reader never sees a torn sample); no history is
// kept (the soak samples at rest each cycle). Best-effort: a write miss just logs.
// Restart-robustness is irrelevant (scratch file).
//
// Runs on the writer goroutine, never on the observe loop -- see telemetryWriter for why the
// distinction is the whole point of this file existing at all.
func (cfg Config) writeTelemetryFile(res *telemetry.NodeResources, logf func(string, ...any)) {
	b, err := json.Marshal(res)
	if err != nil {
		logf("telemetry marshal failed: %v", err)
		return
	}
	tmp := cfg.TelemetryPath + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		logf("telemetry write failed: %v", err)
		return
	}
	if err := os.Rename(tmp, cfg.TelemetryPath); err != nil {
		logf("telemetry rename failed: %v", err)
	}
}

// WedgeForTest blocks the calling goroutine for as long as WedgeFIFO names a FIFO with no
// reader, and does nothing at all otherwise. It is a FIXTURE, named so, and it is in the product
// deliberately.
//
// The watchdog (V3.32) exists to catch an agent that is alive but has one goroutine stuck in an
// uninterruptible syscall, and the test that proves it has to produce exactly that. It used to
// get it for free from writeTelemetry's un-ctx'd write -- point TELEMETRY_PATH's .tmp sibling at
// a reader-less FIFO and the observe loop stopped while the runtime, the timers and every other
// goroutine kept running perfectly. Fixing that (B.87) took the lever away, and no bounded call
// can replace it: the shape the watchdog must catch is precisely the shape the rest of this file
// now makes unreachable. So the lever is explicit rather than borrowed from a defect.
//
// Unset everywhere but that test. install.sh writes no such variable, and an unreadable or
// absent path is a silent no-op -- which is the fail-safe direction, because a fixture that
// quietly fails to engage makes agent-watchdog.nix go RED (no trip) rather than green.
func (cfg Config) wedgeForTest(logf func(string, ...any)) {
	if cfg.WedgeFIFO == "" {
		return
	}
	f, err := os.OpenFile(cfg.WedgeFIFO, os.O_WRONLY, 0)
	if err != nil {
		return // armed but not engaged: nothing there to block on
	}
	logf("wedge fixture: %s got a reader; the observe loop is moving again", cfg.WedgeFIFO)
	_ = f.Close()
}

// selfResources measures this agent process's own resident set (KB) and open fd count from
// /proc/self -- the host-side half of the leak signal. Both best-effort: a read miss leaves
// the field zero (read as "unmeasured"), never an error on the observe path.
func selfResources() (rssKB int64, fds int) {
	if b, err := os.ReadFile("/proc/self/status"); err == nil {
		rssKB = parseSelfVmRSSKB(b)
	}
	if ents, err := os.ReadDir("/proc/self/fd"); err == nil {
		fds = len(ents)
	}
	return
}

// parseSelfVmRSSKB pulls the VmRSS kB value out of /proc/self/status. 0 if absent.
func parseSelfVmRSSKB(status []byte) int64 {
	for _, line := range strings.Split(string(status), "\n") {
		if rest, ok := strings.CutPrefix(line, "VmRSS:"); ok {
			if f := strings.Fields(rest); len(f) > 0 {
				n, _ := strconv.ParseInt(f[0], 10, 64)
				return n
			}
		}
	}
	return 0
}

// Snapshot reads the node's current state into an api.NodeStatus (the payload reported up the shared/api seam). A failed Status read surfaces as a
// non-quorate, unhealthy snapshot rather than a hard error -- the observe loop
// must ride out transient control-channel hiccups. The read error is returned
// alongside so the loop can tell a dead channel (ErrChannelDown -> reconnect,
// B.22a) from a mere degraded read (verb error -> keep observing, report degraded).
// It also returns the probe target it resolved, so the caller can SAY it. The agent knew this
// address all along and never printed it, which made "the node reports healthy and nobody can
// reach it" -- the exact shape of V3.19 -- undiagnosable from a journal. Under DHCP the address is
// not in any config file either, so the log is the only place a human can find it.
func (cfg Config) snapshot(ctx context.Context, r statusReader, served, system string) (api.NodeStatus, model.Cluster, string, error) {
	st := api.NodeStatus{NodeName: cfg.Node, Role: cfg.Role, Image: served, System: system, AgentVersion: cfg.Version}
	rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cl, err := r.Cluster(rctx, cfg.Resource.Name)
	if err != nil {
		return st, cl, "", err // zero QuorumState, Healthy=false
	}
	st.Quorum = cl.QuorumState
	// The name this node is REALLY publishing, not the one it was configured with. A read error
	// leaves it empty rather than falling back to cfg.FlockName: echoing the requested name would
	// make a silent conflict-rename permanently invisible, which is the whole failure being
	// closed here. Empty is honest -- "we do not currently know of a published name".
	if name, merr := r.MDNSPublished(rctx); merr == nil {
		st.PublishedName = name
	}
	// A WITNESS is what "healthy == participating" belongs to, and role is how we know one --
	// not an empty URL. Under DHCP a data node has no configured address either, and the two
	// answers must stay apart: a witness with nothing to probe is healthy when quorate, a data
	// node with no address is a node nobody in the house can reach.
	var probe string
	if cfg.Diskless {
		st.Healthy = cl.Quorate
	} else if url := guest.ResolveHealthURL(rctx, r, cfg.Diskless, cfg.VIPDev, cfg.HealthURL); url == "" {
		// No address to probe means two opposite things, and telling them apart is the whole
		// of this branch:
		//
		//   - a SECONDARY has no address because the VIP is promoter-driven and it was not
		//     promoted. That is the system working. Its health is participation, exactly as a
		//     witness's is -- replicating, quorate and up to date is a secondary doing its
		//     whole job, and there is nothing else to ask it. Reporting it unhealthy made
		//     every correct HA pair permanently DEGRADED in the cloud's view, with a standing
		//     "1 node unhealthy" nobody could act on.
		//   - a PRIMARY has no address because the thing that should have given it one did
		//     not. That is the node the house cannot reach, which is the defect V3.19 exists
		//     for and which B.90 caught in the flesh (briard-vip timing out on DHCP took the
		//     whole promoter chain down). It stays unhealthy, and must: a primary that reads
		//     healthy because it is quorate is the zombie V3.19 was written to abolish.
		//
		// Nor does the front door offer a way out of the distinction: it is partOf
		// briard-vip.service, so on a secondary it is not running to answer a /healthz at all.
		st.Healthy = !cl.Primary && cl.Quorate && cl.UpToDate
	} else {
		probe = url
		// Prefer the in-guest probe (payload.health) so the health signal survives a substrate
		// where the host can't reach the VIP (macvtap). Fall back to the legacy host->VIP GET only
		// when the verb errors — an old guest built before it existed, which is always tap-based, so
		// the LAN probe still works there.
		healthy, herr := r.PayloadHealth(rctx, url)
		if herr != nil {
			healthy = probeHealth(rctx, url)
		}
		st.Healthy = healthy
	}
	return st, cl, probe, nil
}

// CurrentImage reports the payload image this node actually serves: the replicated pin
// read from the guest (ground truth across a failover), falling back to the configured
// baked default when no pin is set. Empty for a witness (no payload). A read error falls
// back to the default rather than blanking the report; the loop re-reads next cycle.
func (cfg Config) currentImage(ctx context.Context, r guestReader) string {
	if cfg.Service.Name == "" {
		return "" // witness / no payload
	}
	rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	pin, err := r.PayloadImage(rctx)
	if err != nil || pin == "" {
		return cfg.Service.Image // no pin set (yet), or a transient read hiccup
	}
	return pin
}

// CurrentSystem reports the NixOS system closure this node is running (readlink -f
// /run/current-system, via the guest) -- ground truth for the whole-OS rollout, correct
// across a failover (a converged survivor reports the switched-to closure). Empty for a
// witness (no payload/upgrade) or on a read hiccup; the rollout just re-reads next cycle.
func (cfg Config) currentSystem(ctx context.Context, r guestReader) string {
	if cfg.Service.Name == "" {
		return "" // witness / no payload
	}
	rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	sys, err := r.SystemPath(rctx)
	if err != nil {
		return ""
	}
	return sys
}

// probeHealth GETs the payload health endpoint; a 200 is healthy, anything else
// (including any error) is not.
func probeHealth(ctx context.Context, url string) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// orDash renders an empty probe target as "-" rather than nothing, so a witness (health follows
// quorum) and a data node that resolved no address are both visible in the log line instead of
// leaving a blank the reader has to interpret.
func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
