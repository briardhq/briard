package host

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"briard.io/agent/drbd"
	"briard.io/agent/guestagent"
	"briard.io/agent/overlay"
	"briard.io/agent/platform"
	"briard.io/shared/api"
	"briard.io/shared/model"
	"briard.io/shared/telemetry"
)

// fakeOverlay is an overlay.OverlayProvider stand-in for the observe-loop wiring.
type fakeOverlay struct {
	health   overlay.Health
	healthEr error
}

func (f fakeOverlay) EnrollNode(context.Context, api.EnrollRequest) (api.NodeIdentity, error) {
	return api.NodeIdentity{}, nil
}
func (fakeOverlay) NodeName(context.Context) (string, error) { return "", nil }
func (fakeOverlay) Up(context.Context) error                 { return nil }
func (f fakeOverlay) Health(context.Context) (overlay.Health, error) {
	return f.health, f.healthEr
}
func (fakeOverlay) Teardown(context.Context) error { return nil }

func TestOverlayStatus_nilWhenUnconfigured(t *testing.T) {
	if got := (Config{}).overlayStatus(context.Background()); got != nil {
		t.Errorf("no overlay configured -> want nil, got %+v", got)
	}
}

func TestOverlayStatus_mirrorsHealth(t *testing.T) {
	cfg := Config{Overlay: fakeOverlay{health: overlay.Health{Up: true, Relayed: true, PeersUp: 2}}}
	got := cfg.overlayStatus(context.Background())
	if got == nil || !got.Up || !got.Relayed || got.PeersUp != 2 {
		t.Errorf("overlayStatus = %+v, want Up+Relayed+PeersUp=2", got)
	}
}

func TestOverlayStatus_downOnError(t *testing.T) {
	cfg := Config{Overlay: fakeOverlay{healthEr: errors.New("daemon down")}}
	got := cfg.overlayStatus(context.Background())
	if got == nil || got.Up {
		t.Errorf("read error -> want non-nil Up=false signal, got %+v", got)
	}
}

// fakeStatus is a guestReader stand-in so snapshot() is testable without
// a live control channel.
type fakeStatus struct {
	qs        model.QuorumState
	err       error
	system    string // SystemPath return (running system closure)
	sysErr    error
	res       telemetry.NodeResources // Resources return (appliance telemetry)
	resErr    error
	health    bool            // ServiceHealth return (the in-guest probe result)
	hlthErr   error           // when set, ServiceHealth errors -> snapshot falls back to the host-side probe
	svcHealth map[string]bool // ServiceHealthOf: service name -> answers; absent = unresolvable
	// vip is what net.vip answers: the address the service NIC ACTUALLY holds, in CIDR form,
	// "" for a device that holds none. probed records the URL snapshot resolved from it, so a
	// test can assert WHAT was probed and not merely that something was.
	vip    string
	vipErr error
	probed *string
	// active is what service.active answers per unit, and activeErr makes it fail -- the two
	// halves per-service state needs, since "the unit is down" and "we could not ask" must not
	// report the same thing (api.ServiceStatus.State).
	active    map[string]bool
	activeErr error
	// mdns is what net.mdnspublished answers: the name avahi ACTUALLY established, which a
	// silent conflict-rename can make differ from the one we asked for. "" = nothing published.
	mdns    string
	mdnsErr error
	// volume is what the replicated volume says this node runs (name -> manifest bytes), which on a
	// node that promoted into somebody else.s install is the only place that truth exists.
	volume        map[string]string
	volumeErr     error
	noServiceList bool // a guest too old to list: the host must fall back, not fail
}

// The fake answers the whole-cluster read the snapshot makes. Its peer list stays empty: these
// tests are about the node's own quorum/health fields, and an empty list is a real reading (a
// guest too old to report peers) rather than an unset one.
func (f fakeStatus) Cluster(context.Context, string) (model.Cluster, error) {
	return model.Cluster{QuorumState: f.qs}, f.err
}

func (f fakeStatus) MDNSPublished(context.Context) (string, error) { return f.mdns, f.mdnsErr }

func (f fakeStatus) ServiceActive(_ context.Context, unit string) (bool, error) {
	return f.active[unit], f.activeErr
}

func (f fakeStatus) ServiceHealth(_ context.Context, url string) (bool, error) {
	if f.probed != nil {
		*f.probed = url
	}
	return f.health, f.hlthErr
}

// ServiceHealthOf is the PER-SERVICE probe ([B.48]), keyed on the service name rather than on a
// URL: `svcHealth` maps a name to what the guest would answer, and an absent entry stands for the
// service the guest cannot resolve -- which must leave the field empty, not report it unhealthy.
func (f fakeStatus) ServiceHealthOf(_ context.Context, service string) (bool, error) {
	ok, known := f.svcHealth[service]
	if !known {
		return false, errors.New("not in the routing table")
	}
	return ok, nil
}

func (f fakeStatus) VIP(context.Context, string) (string, error) { return f.vip, f.vipErr }

func (f fakeStatus) SystemPath(context.Context) (string, error) {
	return f.system, f.sysErr
}

func (f fakeStatus) Resources(context.Context, map[string]string, string) (telemetry.NodeResources, error) {
	return f.res, f.resErr
}

// What the VOLUME carries: `volume` maps a service name to its manifest bytes, so a test can put
// this node in the state a converged survivor is in -- running what somebody else installed.
func (f fakeStatus) ServiceList(context.Context) ([]string, error) {
	names := make([]string, 0, len(f.volume))
	for n := range f.volume {
		names = append(names, n)
	}
	sort.Strings(names)
	return names, f.volumeErr
}

func (f fakeStatus) ServiceInstalled(_ context.Context, name string) (string, error) {
	return f.volume[name], nil
}

func (f fakeStatus) SupportsServiceList() bool { return !f.noServiceList }

func TestConfigFromEnv_DefaultsAndAnchor(t *testing.T) {
	// Clear the knobs so we exercise defaults deterministically.
	for _, k := range []string{"QEMU", "ACCEL", "MEMORY_MB", "NODE", "ROLE", "RESOURCE", "HEALTH_URL"} {
		t.Setenv(k, "")
	}
	t.Setenv("NODE", "n1")
	t.Setenv("ROLE", string(model.RoleAnchor))

	cfg := ConfigFromEnv()
	if cfg.Node != "n1" {
		t.Errorf("Node = %q, want n1", cfg.Node)
	}
	if cfg.Role != model.RoleAnchor {
		t.Errorf("Role = %q, want anchor", cfg.Role)
	}
	if cfg.Diskless {
		t.Error("anchor must not be diskless")
	}
	if len(cfg.Promoter) == 0 {
		t.Error("anchor must have a promoter chain")
	}
	if cfg.Resource.Name != "r0" || cfg.Resource.Device != "/dev/drbd0" {
		t.Errorf("resource defaults wrong: %+v", cfg.Resource)
	}
	if len(cfg.Resource.Peers) != 1 || cfg.Resource.Peers[0].Name != "n1" {
		t.Errorf("single self-peer expected, got %+v", cfg.Resource.Peers)
	}
	if cfg.MemoryMB != 2048 || cfg.QEMUBinary != "qemu-system-x86_64" {
		t.Errorf("VM defaults wrong: mem=%d bin=%q", cfg.MemoryMB, cfg.QEMUBinary)
	}
}

// The shipped agent must launch its guest WITH a monitor. V3.17c2-ii built the clean
// shutdown (ACPI over QMP, and a forced reset for a guest past talking to) but nothing
// outside the test driver ever set QMPSock, so in the field every path that needed to stop
// a guest still fell through to killing qemu -- a power cut. The mechanism existed and was
// unreachable, which is the failure mode a default is for.
//
// It also gets its OWN directory, asserted here because platform.Launch chmods that
// directory 0700: QMP is unrestricted control of the VM, so sharing a directory with a
// socket meant to be reachable (AdminSock) would either widen QMP or break the admin door.
func TestConfigFromEnv_GuestGetsAMonitor(t *testing.T) {
	t.Setenv("QMP_SOCK", "")
	cfg := ConfigFromEnv()
	if cfg.QMPSock == "" {
		t.Fatal("QMPSock empty by default -- the guest would launch with no monitor and could only be power-cut")
	}
	if dir := path.Dir(cfg.QMPSock); dir == path.Dir(cfg.AdminSock) {
		t.Errorf("QMP socket shares directory %q with the admin socket; Launch makes it 0700", dir)
	}
}

// THE ENVIRONMENT CANNOT INSTALL A SERVICE, which is the end state [V3b.3](e1) was after: there
// is no SERVICE_IMAGE (or any sibling) left to read, so an anchor comes up with the empty set and
// fills it from what it was actually installed with -- the node-local cache, or the volume once it
// promotes. Setting the old variables must therefore do NOTHING, which is what this asserts: a
// re-introduced env path would give a node a service nobody installed.
func TestConfigFromEnv_NoServiceComesFromTheEnvironment(t *testing.T) {
	for _, k := range []string{"SERVICE_NAME", "SERVICE_IMAGE", "SERVICE_DATADIR"} {
		t.Setenv(k, "briard-dummy:v0")
	}
	t.Setenv("ROLE", string(model.RoleAnchor))
	if services := ConfigFromEnv().Services; len(services) != 0 {
		t.Fatalf("Services = %+v, want none: a service is installed at runtime, never configured", services)
	}
}

// The chain is STATIC — data -> services -> vip — on every data node, whatever is installed
// ([V3b.3](f)). That is what makes converge-at-promotion possible: the chain is what drbd-reactor
// promotes WITH, but the volume it must converge to is only readable AFTER promotion, so the
// start-list cannot name the services themselves.
//
// The old assertion here was the opposite, and its reason was real: naming a unit the guest does
// not define fails the whole ordered chain and takes the VIP down on every fresh install, which is
// precisely the state `curl | sh` lands in. briard-services is why it can now be unconditional —
// the guest image defines it always, exactly as it defines briard-data and briard-vip.
//
// Nothing is conditional any more ([V3b.3](e1) took the last member out): the chain is the same
// units on every anchor, whatever the node is running and whatever the environment says. The
// SHAPE is what this guards, not the membership -- [B.125] added the two mDNS publishers, and the
// property that must survive any such change is that nothing about the environment or the
// installed services can alter the list.
func TestConfigFromEnv_TheChainIsStatic(t *testing.T) {
	t.Setenv("ROLE", string(model.RoleAnchor))
	cfg := ConfigFromEnv()
	if len(cfg.Services) != 0 {
		t.Errorf("Services = %+v with nothing installed, want the empty set", cfg.Services)
	}
	want := []string{
		"briard-data.service",
		"briard-services.service",
		"briard-vip.service",
		"briard-reverse-proxy.service",
		"briard-mdns.service",
		"briard-mdns-services.service",
	}
	if !slices.Equal(cfg.Promoter, want) {
		t.Errorf("promoter chain = %v, want %v", cfg.Promoter, want)
	}
	// A runtime-installed service does NOT join it — converge starts those, which is what keeps a
	// crashed container from deactivating the promoter's target and demoting the node.
	t.Setenv("SERVICE_IMAGE", "briard-dummy:v0")
	if got := ConfigFromEnv().Promoter; !slices.Equal(got, want) {
		t.Errorf("promoter chain = %v, want the same static %v whatever the environment carries", got, want)
	}
}

// There is NO baked probe target any more (V3.19c step 3). The old default was the lab's own
// http://192.168.1.100/healthz, and the reason it survived so long is that every test agreed with
// it — a guess about someone else's network that nothing in our own could contradict. Unset now
// means "resolve it from the address the guest actually holds", which is the only source that can
// be right on a LAN we have never seen.
//
// The probe target is still the front door rather than a service's own port; that part was never
// the defect, and healthURLAt is what keeps it true once the address is dynamic.
func TestConfigFromEnv_NoBakedHealthTarget(t *testing.T) {
	t.Setenv("HEALTH_URL", "")
	t.Setenv("ROLE", string(model.RoleAnchor))
	t.Setenv("VIP_DEV", "eth2")
	cfg := ConfigFromEnv()
	if cfg.HealthURL != "" {
		t.Errorf("HealthURL = %q, want empty: no address may be baked into the product path", cfg.HealthURL)
	}
	// Empty must NOT read as "nothing to probe" on a data node — that meaning belongs to the
	// witness alone. What makes it resolvable instead is the device the agent was told about.
	if cfg.Diskless {
		t.Fatal("precondition: an anchor is not diskless")
	}
	if cfg.VIPDev == "" {
		t.Error("with no baked target, VIP_DEV is what makes the probe resolvable at all")
	}
}

// An explicitly set HEALTH_URL still pins the probe, which is what makes the resolution an
// override rather than a replacement — and is how a node with an unusual front door stays
// configurable without reintroducing a default for everyone.
func TestConfigFromEnv_ExplicitHealthURLIsKept(t *testing.T) {
	t.Setenv("ROLE", string(model.RoleAnchor))
	t.Setenv("HEALTH_URL", "http://10.1.2.3/healthz")
	if got := ConfigFromEnv().HealthURL; got != "http://10.1.2.3/healthz" {
		t.Errorf("HealthURL = %q, want the explicitly configured target", got)
	}
}

func TestConfigFromEnv_WitnessHasNoPromoter(t *testing.T) {
	t.Setenv("ROLE", string(model.RoleDiskless))
	cfg := ConfigFromEnv()
	if !cfg.Diskless {
		t.Error("witness must be diskless")
	}
	if cfg.Promoter != nil {
		t.Errorf("witness must have no promoter, got %v", cfg.Promoter)
	}
	if cfg.HealthURL != "" {
		t.Errorf("witness health follows quorum, HealthURL must be empty, got %q", cfg.HealthURL)
	}
}

func TestConfigFromEnv_SystemNIC(t *testing.T) {
	// Unset -> single-node: no system NIC to configure, no VIP-device override.
	t.Setenv("SYSTEM_TAP", "")
	t.Setenv("SYSTEM_DEV", "")
	t.Setenv("SYSTEM_CIDR", "")
	t.Setenv("VIP_DEV", "")
	if c := ConfigFromEnv(); c.SystemTap != "" || c.SystemDev != "" || c.SystemCIDR != "" || c.VIPDev != "" {
		t.Errorf("expected empty system NIC fields, got %+v", c)
	}
	// Set -> carried through for the launcher (tap) + the configure step (DRBD NIC on
	// eth1, VIP moved to eth2).
	t.Setenv("SYSTEM_TAP", "sys0")
	t.Setenv("SYSTEM_DEV", "eth1")
	t.Setenv("SYSTEM_CIDR", "10.0.0.3/24")
	t.Setenv("VIP_DEV", "eth2")
	c := ConfigFromEnv()
	if c.SystemTap != "sys0" || c.SystemDev != "eth1" || c.SystemCIDR != "10.0.0.3/24" || c.VIPDev != "eth2" {
		t.Errorf("system NIC fields not mapped: %+v", c)
	}
}

func TestParsePeers(t *testing.T) {
	got := parsePeers("n1@10.0.0.2/vdb, n2@10.0.0.3:7000/sdb , w@10.0.0.4/none")
	want := []drbd.Peer{
		{Name: "n1", NodeID: 0, Address: "10.0.0.2:7789", Disk: "/dev/vdb"}, // bare host -> default port; bare disk -> /dev/
		{Name: "n2", NodeID: 1, Address: "10.0.0.3:7000", Disk: "/dev/sdb"}, // explicit port kept; disk under /dev
		{Name: "w", NodeID: 2, Address: "10.0.0.4:7789"},                    // witness: "none" disk -> diskless
	}
	if len(got) != len(want) {
		t.Fatalf("parsePeers len = %d, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("peer[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
	if parsePeers("") != nil {
		t.Error("empty PEERS must yield nil (single-node self-peer path)")
	}
	if p := parsePeers("garbage,,n1@10.0.0.2/vdb"); len(p) != 1 || p[0].NodeID != 0 {
		t.Errorf("malformed entries must be skipped without NodeID gaps, got %+v", p)
	}
}

func TestConfigFromEnv_MultiPeerMesh(t *testing.T) {
	t.Setenv("PEERS", "n1@10.0.0.2/vdb,n2@10.0.0.3/vdb,w@10.0.0.4/none")

	// The first peer seeds the fresh cluster; every other node syncs from it.
	t.Setenv("NODE", "n1")
	if cfg := ConfigFromEnv(); !cfg.FreshInit {
		t.Error("first peer (n1) must FreshInit")
	}
	t.Setenv("NODE", "n2")
	if cfg := ConfigFromEnv(); cfg.FreshInit {
		t.Error("non-first peer (n2) must not FreshInit")
	}

	// The rendered mesh carries every peer, identical regardless of self.
	cfg := ConfigFromEnv()
	if len(cfg.Resource.Peers) != 3 {
		t.Fatalf("expected 3 peers in the mesh, got %+v", cfg.Resource.Peers)
	}
}

func TestConfigFromEnv_SingleNodeFreshInit(t *testing.T) {
	// No PEERS -> single self-peer, always FreshInit (the single-node contract).
	t.Setenv("PEERS", "")
	t.Setenv("NODE", "guest")
	cfg := ConfigFromEnv()
	if !cfg.FreshInit {
		t.Error("single-node must FreshInit")
	}
	if len(cfg.Resource.Peers) != 1 || cfg.Resource.Peers[0].Name != "guest" {
		t.Errorf("single self-peer expected, got %+v", cfg.Resource.Peers)
	}
}

func TestDeriveMAC(t *testing.T) {
	// Stable, QEMU-OUI, and distinct per node and per NIC role -- the property that
	// keeps a fleet's NICs from colliding on a shared bridge.
	if got := deriveMAC("n1", "sys"); got != deriveMAC("n1", "sys") {
		t.Errorf("not stable: %q", got)
	}
	seen := map[string]string{}
	for _, node := range []string{"n1", "n2", "w"} {
		for _, role := range []string{"sys", "svc"} {
			mac := deriveMAC(node, role)
			if len(mac) != 17 || mac[:9] != "52:54:00:" {
				t.Errorf("deriveMAC(%s,%s) = %q, want 52:54:00:xx:xx:xx", node, role, mac)
			}
			if prev, ok := seen[mac]; ok {
				t.Errorf("MAC collision: %s/%s and %s both -> %s", node, role, prev, mac)
			}
			seen[mac] = node + "/" + role
		}
	}
}

// The status carries the name the node is REALLY publishing, never the one it was configured
// with. avahi conflict-renames on a collision and tells nobody, so two flocks in one house can
// even swap names across a reboot -- and if the report echoed cfg.FlockName, nothing anywhere
// (household, journal, cloud) would ever notice. Same doctrine as reading the VIP off the
// interface instead of trusting vip.env, and for the same reason: what we asked for is not
// evidence of what is in force.
func TestSnapshot_ReportsThePublishedNameNotTheConfiguredOne(t *testing.T) {
	cfg := Config{Node: "briard-node-3f9a2c", Role: model.RoleAnchor, FlockName: "brave-elf"}
	cfg.Resource.Name = "r0"

	qs := model.QuorumState{Primary: true, Quorate: true, Connected: 2}
	r := fakeStatus{qs: qs, vip: "192.168.9.50/24", health: true, mdns: "brave-elf-2"}
	st, _, _, err := cfg.snapshot(context.Background(), r, "/nix/store/sys")
	if err != nil {
		t.Fatal(err)
	}
	if st.PublishedName != "brave-elf-2" {
		t.Errorf("PublishedName = %q, want the established brave-elf-2 (configured: %q)",
			st.PublishedName, cfg.FlockName)
	}
	if st.PublishedName == cfg.FlockName {
		t.Error("the report echoed the configured name -- a silent avahi rename would be invisible")
	}
}

// A node publishing nothing reports nothing, and specifically does NOT fall back to the name it
// was configured with. Empty means "we do not currently know of a published name", which is the
// honest answer for a Secondary (the name is bound to the VIP) and for a failed read alike.
func TestSnapshot_UnknownPublishedNameIsEmptyNotTheConfiguredOne(t *testing.T) {
	cfg := Config{Node: "briard-node-3f9a2c", Role: model.RoleAnchor, FlockName: "brave-elf"}
	cfg.Resource.Name = "r0"

	qs := model.QuorumState{Quorate: true, Connected: 2}
	for _, c := range []struct {
		what string
		r    fakeStatus
	}{
		{"a Secondary publishes no name", fakeStatus{qs: qs, vip: "192.168.9.50/24", health: true}},
		{"the read failed", fakeStatus{qs: qs, vip: "192.168.9.50/24", health: true, mdnsErr: errors.New("channel hiccup")}},
	} {
		st, _, _, err := cfg.snapshot(context.Background(), c.r, "/nix/store/sys")
		if err != nil {
			t.Fatalf("%s: %v", c.what, err)
		}
		if st.PublishedName != "" {
			t.Errorf("%s: PublishedName = %q, want empty -- never the configured %q",
				c.what, st.PublishedName, cfg.FlockName)
		}
	}
}

// A WITNESS's health follows quorum, and the thing that says so is its ROLE. This used to be
// keyed on an empty HealthURL, which read the same but meant something else: V3.19 gives a data
// node an address it acquires by DHCP, so "no configured URL" stops being witness-shaped. The
// two now answer differently on purpose -- see the data-node case below.
func TestSnapshot_HealthFollowsQuorumOnAWitness(t *testing.T) {
	cfg := Config{Node: "n1", Role: model.RoleDiskless, Diskless: true, HealthURL: ""}
	cfg.Resource.Name = "r0"

	qs := model.QuorumState{Primary: true, Quorate: true, Connected: 2}
	st, _, _, _ := cfg.snapshot(context.Background(), fakeStatus{qs: qs}, "/nix/store/sys")
	if st.NodeName != "n1" || st.Role != model.RoleDiskless {
		t.Errorf("identity not preserved: %+v", st)
	}
	if st.Quorum != qs {
		t.Errorf("Quorum = %+v, want %+v", st.Quorum, qs)
	}
	if !st.Healthy {
		t.Error("a quorate witness should read healthy: it has nothing to probe")
	}
	if len(st.Services) != 0 {
		t.Errorf("Services = %+v, want none: a witness runs nothing", st.Services)
	}
	if st.System != "/nix/store/sys" {
		t.Errorf("System = %q, want the running system reported through the seam", st.System)
	}
}

// CurrentSystem reports the guest's running closure, and "" for a witness or a read
// error -- ground truth for the whole-OS rollout across a failover.
//
// The zero-service case is the one that matters and the one this test used to get wrong: it
// asserted an anchor with no service reports "", calling that config "witness". It is not a
// witness, it is the SHIPPED state -- what install.sh leaves behind and what the whole free tier
// runs -- and reporting "" for it made the cloud's systemTargets skip the node forever ([V3b.3](d)).
// A witness is diskless; that is now what the code and this test both say.
func TestCurrentSystem(t *testing.T) {
	cfg := Config{Services: []model.ServiceSpec{{Name: "dummy"}}}
	if got := cfg.currentSystem(context.Background(), fakeStatus{system: "/nix/store/v1"}); got != "/nix/store/v1" {
		t.Errorf("currentSystem = %q, want the running /nix/store/v1", got)
	}
	if got := cfg.currentSystem(context.Background(), fakeStatus{sysErr: errors.New("down")}); got != "" {
		t.Errorf("read error: currentSystem = %q, want empty", got)
	}
	// The shipped zero-service anchor. It runs a closure like any other node, so it must SAY so
	// -- a node the rollout cannot see is a node the rollout cannot update.
	shipped := Config{}
	if got := shipped.currentSystem(context.Background(), fakeStatus{system: "/nix/store/v1"}); got != "/nix/store/v1" {
		t.Errorf("zero-service anchor: currentSystem = %q, want /nix/store/v1 -- the shipped node must be visible to an OS roll", got)
	}
	// A real witness: diskless, no guest of its own to read a closure from.
	witness := Config{Diskless: true}
	if got := witness.currentSystem(context.Background(), fakeStatus{system: "/nix/store/v1"}); got != "" {
		t.Errorf("witness currentSystem = %q, want empty", got)
	}
}

// Observe returns ErrChannelDown so Run re-dials — the fix for the older gap
// where a single dropped channel blinded the host forever.
func TestObserveReturnsOnChannelDown(t *testing.T) {
	cfg := Config{Node: "n1", Role: model.RoleAnchor, StatusEvery: time.Millisecond}
	cfg.Resource.Name = "r0"
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second) // safety net
	defer cancel()
	err := cfg.observe(ctx, fakeStatus{err: guestagent.ErrChannelDown}, nil, nil, nil, nil, nil, "", nil, &[]api.DirectiveOutcome{}, func(string, ...any) {})
	if !errors.Is(err, guestagent.ErrChannelDown) {
		t.Errorf("observe = %v, want ErrChannelDown (to trigger reconnect)", err)
	}
}

// A verb-level read error (channel alive, guest degraded) must NOT trigger a reconnect —
// observe keeps observing (reporting degraded) until ctx, so a cold DRBD can't spin the
// reconnect loop.
func TestObserveRidesOutVerbError(t *testing.T) {
	cfg := Config{Node: "n1", Role: model.RoleAnchor, StatusEvery: time.Millisecond}
	cfg.Resource.Name = "r0"
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	r := fakeStatus{err: errors.New("drbdsetup: no such resource r0")} // verb error, channel fine
	if err := cfg.observe(ctx, r, nil, nil, nil, nil, nil, "", nil, &[]api.DirectiveOutcome{}, func(string, ...any) {}); err != nil {
		t.Errorf("observe on a verb error = %v, want nil (keep observing until ctx)", err)
	}
}

// captureClient records the last reported status and stops observe after one report.
type captureClient struct {
	last   *api.NodeStatus
	cancel context.CancelFunc
}

func (c *captureClient) Register(context.Context, api.NodeInfo) (api.Assignment, error) {
	return api.Assignment{}, nil
}
func (c *captureClient) Report(_ context.Context, req api.ReportRequest) ([]api.Directive, error) {
	c.last = &req.Status
	c.cancel() // one report is enough; end the observe loop
	return nil, nil
}
func (c *captureClient) ReportMetrics(context.Context, string, []api.MetricAggregate) error {
	return nil
}

// Observe tags each report with the resolved tenant, so the cloud sees which
// tenant a node claims.
func TestObserveTagsReportWithTenant(t *testing.T) {
	cfg := Config{Node: "n1", Role: model.RoleAnchor, StatusEvery: time.Millisecond}
	cfg.Resource.Name = "r0"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cc := &captureClient{cancel: cancel}

	if err := cfg.observe(ctx, fakeStatus{}, nil, nil, nil, cc, nil, "default", nil, &[]api.DirectiveOutcome{}, func(string, ...any) {}); err != nil {
		t.Fatalf("observe: %v", err)
	}
	if cc.last == nil || cc.last.Tenant != "default" {
		t.Errorf("reported status = %+v, want tenant=default", cc.last)
	}
}

// Operation-class partition: a planned op (upgrade) is cloud-gated -- it runs only via a
// delivered directive (applyDirective), so a node with no cloud reachable never starts one
// autonomously. Reactive ops (failover, converge) stay autonomous, but those aren't directives.
func TestObserveNoCloudNoPlannedOp(t *testing.T) {
	up := &fakeUpgrader{}
	cfg := Config{Node: "n1", Role: model.RoleAnchor, StatusEvery: time.Millisecond}
	cfg.Resource.Name = "r0"
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Millisecond)
	defer cancel()
	// Rep=nil (no cloud): several cycles, then ctx ends.
	if err := cfg.observe(ctx, fakeStatus{}, up, nil, nil, nil, nil, "", nil, &[]api.DirectiveOutcome{}, func(string, ...any) {}); err != nil {
		t.Fatalf("observe: %v", err)
	}
	if up.sysCalled {
		t.Errorf("no cloud reachable -> no planned op, but the upgrader ran: %+v", up)
	}
}

func TestSnapshot_StatusErrorIsUnhealthy(t *testing.T) {
	cfg := Config{Node: "n1", Role: model.RoleAnchor}
	sentinel := errors.New("channel down")
	st, _, _, err := cfg.snapshot(context.Background(), fakeStatus{err: sentinel}, "")
	if !errors.Is(err, sentinel) {
		t.Errorf("snapshot must return the read error (for the reconnect gate), got %v", err)
	}
	if st.NodeName != "n1" {
		t.Errorf("identity must survive a status error, got %+v", st)
	}
	if st.Healthy || st.Quorum.Quorate {
		t.Errorf("a failed status read must be non-quorate + unhealthy, got %+v", st)
	}
}

func TestSnapshot_HealthURLProbedNotQuorum(t *testing.T) {
	// Health comes from the probe, not the quorum bit, AND it prefers the in-guest verb
	// (fakeStatus.health) over the host-side LAN probe.
	cfg := Config{Node: "n1", Role: model.RoleAnchor, HealthURL: "http://unused.invalid/healthz"}

	// Quorate but the in-guest probe says sick -> unhealthy (health != quorum).
	st, _, _, _ := cfg.snapshot(context.Background(), fakeStatus{qs: model.QuorumState{Quorate: true}, health: false}, "")
	if !st.Quorum.Quorate {
		t.Fatal("precondition: node is quorate")
	}
	if st.Healthy {
		t.Error("in-guest probe false must read unhealthy despite quorum")
	}
	// Non-quorate but the in-guest probe says healthy -> healthy (health != quorum).
	st, _, _, _ = cfg.snapshot(context.Background(), fakeStatus{qs: model.QuorumState{Quorate: false}, health: true}, "")
	if !st.Healthy {
		t.Error("in-guest probe true must read healthy")
	}
}

// With no address of our own choosing, the loop probes the one the GUEST reports -- the DHCP
// lease it acquired at promotion. This is the whole point of V3.19c: the host stops deciding the
// service address, so it has to be told what the address turned out to be, every cycle.
func TestSnapshot_HealthProbesTheAddressTheGuestReports(t *testing.T) {
	var probed string
	cfg := Config{Node: "n1", Role: model.RoleAnchor, HealthURL: "", VIPDev: "eth2"}
	r := fakeStatus{qs: model.QuorumState{Quorate: true}, health: true, vip: "192.168.9.50/24", probed: &probed}

	st, _, _, _ := cfg.snapshot(context.Background(), r, "")
	if want := "http://192.168.9.50/healthz"; probed != want {
		t.Errorf("probed %q, want the front door at the REPORTED lease %q", probed, want)
	}
	if !st.Healthy {
		t.Error("a node answering at its acquired address is healthy")
	}
}

// An address WE set is the address we probe, even though the guest could be asked. The device
// can hold more than one address (dhcpcd still serves the service NIC), and preferring what it
// reports would reintroduce V3.19's own failure shape at the worst moment: a node-local lease
// that keeps answering after the VIP has moved to the peer -- healthy while not serving.
func TestSnapshot_ConfiguredAddressWinsOverTheReportedOne(t *testing.T) {
	var probed string
	cfg := Config{Node: "n1", Role: model.RoleAnchor, HealthURL: "http://192.168.9.7/healthz", VIPDev: "eth2"}
	r := fakeStatus{qs: model.QuorumState{Quorate: true}, health: true, vip: "192.168.9.50/24", probed: &probed}

	if _, _, _, _ = cfg.snapshot(context.Background(), r, ""); probed != "http://192.168.9.7/healthz" {
		t.Errorf("probed %q, want the CONFIGURED address", probed)
	}
}

// A PRIMARY with no address -- neither configured nor reported -- is not healthy. It is the node
// nobody in the house can reach, which is the defect V3.19 exists for; answering it like a witness
// ("healthy == quorate") is how that defect stayed invisible. B.90 is what it looks like in the
// flesh: briard-vip timed out waiting for DHCP, took drbd-services@r0 down with it, and the node
// went on being quorate the whole time.
func TestSnapshot_PrimaryWithNoAddressIsUnhealthy(t *testing.T) {
	var probed string
	cfg := Config{Node: "n1", Role: model.RoleAnchor, HealthURL: "", VIPDev: "eth2"}
	// Quorate AND up to date -- everything a secondary needs to read healthy below. The primary
	// bit is the only difference, and it has to be enough on its own.
	r := fakeStatus{
		qs:     model.QuorumState{Primary: true, Quorate: true, Diskful: true, UpToDate: true},
		health: true, vip: "", probed: &probed,
	}

	st, _, _, _ := cfg.snapshot(context.Background(), r, "")
	if st.Healthy {
		t.Error("a quorate primary holding no service address must NOT read healthy")
	}
	if probed != "" {
		t.Errorf("nothing to probe, yet it probed %q", probed)
	}
}

// A SECONDARY holds no service address because the VIP is promoter-driven, not because anything
// is wrong with it -- so its health is participation, the same rule the witness follows. Reporting
// it unhealthy made every correct HA pair read DEGRADED in the cloud's view forever, with a
// standing "1 node unhealthy" nobody could act on (B.91).
func TestSnapshot_SecondaryWithNoAddressIsHealthyWhenParticipating(t *testing.T) {
	cfg := Config{Node: "n2", Role: model.RoleAnchor, HealthURL: "", VIPDev: "eth2"}
	participating := model.QuorumState{Primary: false, Quorate: true, Diskful: true, UpToDate: true}

	st, _, probe, _ := cfg.snapshot(context.Background(), fakeStatus{qs: participating, vip: ""}, "")
	if !st.Healthy {
		t.Error("a quorate, up-to-date secondary is doing its whole job and must read healthy")
	}
	if probe != "" {
		t.Errorf("a secondary has nothing to probe, got %q", probe)
	}

	// Not up to date == not a viable failover target. This is the node-local fact the OS-upgrade
	// gate already leans on (model.QuorumState.UpToDate exists because that gate had nothing else
	// to ask a Secondary); health must not be softer than the gate.
	syncing := participating
	syncing.UpToDate = false
	if st, _, _, _ := cfg.snapshot(context.Background(), fakeStatus{qs: syncing, vip: ""}, ""); st.Healthy {
		t.Error("a secondary still syncing must not read healthy")
	}

	// Non-quorate is the partitioned survivor: participating in nothing.
	isolated := participating
	isolated.Quorate = false
	if st, _, _, _ := cfg.snapshot(context.Background(), fakeStatus{qs: isolated, vip: ""}, ""); st.Healthy {
		t.Error("a non-quorate secondary must not read healthy")
	}
}

// When the guest predates service.health (the verb errors), snapshot falls back to the legacy
// host-side probe of the VIP -- so an old, tap-based guest keeps a correct health signal.
func TestSnapshot_HealthFallsBackToHostProbe(t *testing.T) {
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ok.Close()
	sick := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer sick.Close()

	verbErr := errors.New("guestagent: unknown verb \"service.health\"")

	cfg := Config{Node: "n1", Role: model.RoleAnchor, HealthURL: ok.URL}
	st, _, _, _ := cfg.snapshot(context.Background(), fakeStatus{qs: model.QuorumState{Quorate: true}, hlthErr: verbErr}, "")
	if !st.Healthy {
		t.Error("verb error must fall back to the host probe (200 -> healthy)")
	}
	cfg.HealthURL = sick.URL
	st, _, _, _ = cfg.snapshot(context.Background(), fakeStatus{qs: model.QuorumState{Quorate: true}, hlthErr: verbErr}, "")
	if st.Healthy {
		t.Error("verb error + host probe 500 -> unhealthy")
	}
}

// The OS-upgrade budget is now configurable (so the lab need not spend the production interval to
// watch a rollback), and that is exactly why its DEFAULT has to be asserted. It is not a timeout
// detail: AwaitOSReady polls until this context ends, so this number IS how long a broken update
// leaves a node degraded before it reverts itself, and it is also the bound on staging — the one
// step that goes to the network. A silent drift downward would false-roll-back slow-but-healthy
// upgrades on a slow link; upward, it would leave a node degraded longer than anyone agreed to.
func TestConfigFromEnv_UpgradeBudgetDefault(t *testing.T) {
	t.Setenv("NODE_NAME", "n1")
	t.Setenv("RESOURCE_NAME", "r0")

	cfg := ConfigFromEnv()
	if cfg.UpgradeBudget != 15*time.Minute {
		t.Errorf("UpgradeBudget default = %v, want 15m — see Config.UpgradeBudget before changing this", cfg.UpgradeBudget)
	}

	// And it is overridable, which is the point of the change.
	t.Setenv("UPGRADE_BUDGET", "90s")
	if got := ConfigFromEnv().UpgradeBudget; got != 90*time.Second {
		t.Errorf("UPGRADE_BUDGET=90s gave %v", got)
	}
}

// The VIP's MAC is the VIP's identity, so it must be FLOCK-scoped: two nodes of one flock present
// the same service MAC, draw the same DHCP lease, and therefore keep the same address when the VIP
// moves between them. The DRBD and witness MACs must stay NODE-scoped -- those have to differ per
// node or the NICs collide and ARP never resolves (V3.19b).
func TestServiceMACIsFlockScopedAndOthersAreNot(t *testing.T) {
	a := Config{Node: "anchorA", FlockID: "flock-1"}
	b := Config{Node: "anchorB", FlockID: "flock-1"}

	if got, want := deriveMAC(orNode(a.FlockID, a.Node), "svc"), deriveMAC(orNode(b.FlockID, b.Node), "svc"); got != want {
		t.Errorf("service MAC must be equal across a flock's nodes: %s != %s", got, want)
	}
	if deriveMAC(a.Node, "sys") == deriveMAC(b.Node, "sys") {
		t.Error("DRBD MACs must differ per node, or the NICs collide")
	}
	if deriveMAC(a.Node, "wit") == deriveMAC(b.Node, "wit") {
		t.Error("witness MACs must differ per node, or the NICs collide")
	}
	// Two different flocks in one house must not collide on the service MAC either.
	other := Config{Node: "anchorA", FlockID: "flock-2"}
	if deriveMAC(orNode(a.FlockID, a.Node), "svc") == deriveMAC(orNode(other.FlockID, other.Node), "svc") {
		t.Error("distinct flocks must present distinct service MACs")
	}
}

// No flock id -> the node name, unchanged. This is what keeps every already-installed node and
// every agent-less harness on exactly the MAC it has always had instead of silently drawing a new
// DHCP lease the first time it restarts on a newer agent.
func TestServiceMACFallsBackToNodeWithoutFlockID(t *testing.T) {
	if got, want := deriveMAC(orNode("", "guest"), "svc"), deriveMAC("guest", "svc"); got != want {
		t.Errorf("without a flock id the service MAC must be the node-derived one: %s != %s", got, want)
	}
}

// The TCG fallback is the silent one. `-machine accel=kvm:tcg` is a request, not a
// declaration: a host with no virtualisation extensions gets a guest that boots, converges and
// serves — correctly, and roughly an order of magnitude slower. Nothing fails, so nothing
// reports it, and the cost lands on whoever asks "why is this node slow?" months later. This
// asserts the line that answers them, and that it comes from ASKING qemu
// (`query-accelerators`) rather than from re-reading the argv we chose.
//
// The table asks for a NAME, which is what [V3b.28] changed. The case that decides it is the
// third: an accelerated guest whose accelerator is not KVM must not log a warning — under the
// old `query-kvm` it did, because the question was KVM-shaped and WHPX answers it "no".
func TestLogAccelerationSaysWhatTheVMActuallyGot(t *testing.T) {
	for _, tc := range []struct {
		name    string
		accel   string   // the query-accelerators payload qemu returns
		want    []string // substrings the operator must be able to find
		notWant string
	}{
		{
			name:  "emulated: the fallback fired and says so",
			accel: `{"return":{"enabled":"tcg","present":["qtest","tcg"]}}`,
			want:  []string{"WARNING", "EMULATED", "firmware", "/dev/kvm", "order of magnitude"},
		},
		{
			name:    "accelerated: said out loud, so silence never has to be interpreted",
			accel:   `{"return":{"enabled":"kvm","present":["kvm","qtest","tcg"]}}`,
			want:    []string{"accelerated by kvm", "cpu=\"max\""},
			notWant: "WARNING",
		},
		{
			// The case the swap exists for: a hypervisor that is not KVM is still a
			// hypervisor. Asking `query-kvm` here answered {"enabled":false} and this line
			// called a correctly accelerated node emulated (measured on Windows, [V3b.27](a)).
			name:    "accelerated by something that is not KVM",
			accel:   `{"return":{"enabled":"whpx","present":["qtest","tcg","whpx"]}}`,
			want:    []string{"accelerated by whpx"},
			notWant: "WARNING",
		},
		{
			// An accelerator we have never heard of must read as acceleration, not as a
			// fault: the enum grows with QEMU and this code should not need editing when it
			// does. Only tcg means emulation.
			name:    "an accelerator this code has never heard of",
			accel:   `{"return":{"enabled":"mshv","present":["kvm","mshv","qtest","tcg"]}}`,
			want:    []string{"accelerated by mshv"},
			notWant: "WARNING",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, sock := startQMPWith(t, map[string]string{"query-accelerators": tc.accel})
			spec := platform.QEMUSpec{QMPSock: sock, Accel: "kvm:tcg", CPUModel: "max"}
			var log string
			logAcceleration(context.Background(), platform.Adopt(spec), spec,
				func(f string, a ...any) { log += fmt.Sprintf(f, a...) + "\n" })
			for _, want := range tc.want {
				if !strings.Contains(log, want) {
					t.Errorf("the log must mention %q, so it is greppable later:\n%s", want, log)
				}
			}
			if tc.notWant != "" && strings.Contains(log, tc.notWant) {
				t.Errorf("a healthy guest must not log %q:\n%s", tc.notWant, log)
			}
		})
	}
}

// No monitor, nothing to ask: the tests and harnesses that launch without QMP must not be told
// their guest might be emulated. An unanswerable question is not a finding.
func TestLogAccelerationSilentWithoutQMP(t *testing.T) {
	spec := platform.QEMUSpec{Accel: "kvm:tcg"}
	logged := false
	logAcceleration(context.Background(), platform.Adopt(spec), spec, func(string, ...any) { logged = true })
	if logged {
		t.Error("without a QMP socket there is nothing to report; the line must be omitted, not guessed")
	}
}

// A cancellation during bring-up is the shutdown the agent was ASKED to perform, not a failure of
// it ([B.133]). Run must return nil, because main hands a non-nil return to log.Fatalf: without
// this, a `systemctl restart` (or a host reboot) landing in the bring-up window makes systemd
// record `Failed with result 'exit-code'` against a unit that did exactly what it was told, and a
// false fault in the journal is worth as much as a missing one.
//
// Cancelled BEFORE the call rather than raced against it: the guard is `ctx.Err() != nil` after
// bring-up returns, so a pre-cancelled context exercises it deterministically, with nothing to
// race on a loaded runner.
func TestRunTreatsACancellationDuringBringUpAsAShutdown(t *testing.T) {
	// NON-VACUITY, and it is the whole reason this test can fail: with a LIVE context this same
	// config cannot bring a guest up (no QEMU, no sockets), so Run must report an error. If it
	// returned nil here, the nil asserted below would prove nothing at all.
	if err := Run(context.Background(), Config{}, func(string, ...any) {}); err == nil {
		t.Fatal("Run with a live context and an unusable config = nil, want an error — " +
			"the cancellation check below would be vacuous")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // asked to stop before the guest could ever come up

	var logged []string
	logf := func(f string, a ...any) { logged = append(logged, fmt.Sprintf(f, a...)) }
	if err := Run(ctx, Config{}, logf); err != nil {
		t.Errorf("Run cancelled during bring-up = %v, want nil — a stop is not a failure", err)
	}

	// The error is reported, not swallowed: it still says how far bring-up had got, which is the
	// only thing distinguishing "stopped while launching" from "stopped while handshaking".
	var found bool
	for _, l := range logged {
		if strings.Contains(l, "shutting down during bring-up") {
			found = true
		}
	}
	if !found {
		t.Errorf("no 'shutting down during bring-up' line; the bring-up error was dropped silently.\ngot: %v", logged)
	}
}
