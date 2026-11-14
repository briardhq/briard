package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"briard.io/agent/apps"
	"briard.io/agent/drbd"
	"briard.io/agent/guestagent"
	"briard.io/agent/guestfirmware"
	"briard.io/agent/hass"
	"briard.io/agent/overlay"
	"briard.io/agent/platform"
	"briard.io/agent/quadlet"
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
	peers     []model.PeerState // Cluster.Peers: who else is in the house (empty = a guest that reports none)
	err       error
	system    string // SystemPath return (running system closure)
	sysErr    error
	res       telemetry.NodeResources // Resources return (appliance telemetry)
	resErr    error
	health    bool                   // AppHealth return (the in-guest probe result)
	hlthErr   error                  // when set, AppHealth errors -> snapshot falls back to the host-side probe
	svcHealth map[string]apps.Health // AppHealthOf: app name -> answers; absent = unresolvable
	// vip is what net.vip answers: the address the service NIC actually holds, in CIDR form, ""
	// for none. probed records the URL snapshot resolved from it, so a test can assert what was
	// probed, not merely that something was.
	vip    string
	vipErr error
	probed *string
	// active is what app.active answers per unit and activeErr makes it fail: "the unit is down"
	// and "we could not ask" must not report the same thing (api.AppStatus.State).
	active    map[string]bool
	activeErr error
	// mdns is what net.mdnspublished answers: the name avahi actually established, which a
	// silent conflict-rename can make differ from the one we asked for. "" = nothing published.
	mdns    string
	mdnsErr error
	other   string // what net.mdnsother answers: another briard heard at bring-up
	// mdnsAsked counts both mDNS reads, which the loop must never make (nothing it decides
	// depends on a name).
	mdnsAsked *int
	// clusterAsked, vipAsked and resAsked count the Cluster (one per cycle), VIP and Resources
	// reads, so a test can hold the loop to one net.vip per cycle across every owner of the address.
	clusterAsked, vipAsked, resAsked *int
	// volume is what the replicated volume says this node runs (name -> manifest bytes), the only
	// place that truth exists on a node that promoted into somebody else's install.
	volume    map[string]string
	volumeErr error
	noAppList bool // a guest too old to list: the host must fall back, not fail
	// The ring: what this app's history holds, what the loop asked to take, and the two
	// refusals (a guest too old for the ring, a take that fails).
	members    map[string][]quadlet.SnapshotEntry
	membersErr error
	took       *[]takenMember
	snapErr    error
	noRing     bool
	noQuiesce  bool        // advertises the ring but not the quiesced take
	held       bool        // the app held still across a quiesced take
	quiesceErr error       // the quiesced take failed outright
	db         *fakeDB     // the guest's recorder check; nil is a guest that has none
	backup     *fakeBackup // the guest's backup run; nil is a guest that has none
}

// takenMember is one call to Snapshot: where the member went, and the sidecar that went with it.
// `asked` is the sidecar the host rendered, before the guest had its say: the two differ only on
// a quiesced take, which is the whole point.
type takenMember struct{ member, sidecar, asked string }

// The ring's slice of the guest. The observe loop carries the nightly member, so the
// reader it is handed has to be able to take one.
func (f fakeStatus) Members(_ context.Context, app string) ([]quadlet.SnapshotEntry, error) {
	return f.members[app], f.membersErr
}
func (f fakeStatus) SupportsMembers() bool { return !f.noRing }

func (f fakeStatus) SupportsSnapshotMember() bool { return !f.noRing }

// The quiesced take: `held` is what the fake app reports, and the recorded sidecar carries the
// class the guest would have written, upgraded only when it held, which the host must not be able
// to claim on its own.
func (f fakeStatus) QuiescedSnapshot(_ context.Context, app, _, dest, sidecar string) (bool, string, error) {
	asked := sidecar
	if f.quiesceErr != nil {
		return false, "", f.quiesceErr
	}
	if f.held {
		var meta quadlet.SnapshotMeta
		if err := json.Unmarshal([]byte(sidecar), &meta); err == nil {
			meta.Consistency = quadlet.Quiesced
			up, _ := json.Marshal(meta)
			sidecar = string(up)
		}
	}
	if f.took != nil {
		*f.took = append(*f.took, takenMember{dest, sidecar, asked})
	}
	if f.held {
		return true, "", nil
	}
	return false, app + " did not hold still", nil
}
func (f fakeStatus) SupportsQuiescedSnapshot() bool { return !f.noRing && !f.noQuiesce }
func (f fakeStatus) Snapshot(_ context.Context, _, dest, sidecar string) error {
	if f.took != nil {
		*f.took = append(*f.took, takenMember{dest, sidecar, sidecar})
	}
	return f.snapErr
}

// The fake answers the snapshot's whole-cluster read. `peers` is set only where the rule under
// test reads it; an empty list is a real reading (a guest too old to report peers), not an unset
// one.
func (f fakeStatus) Cluster(context.Context, string) (model.Cluster, error) {
	if f.clusterAsked != nil {
		*f.clusterAsked++
	}
	return model.Cluster{QuorumState: f.qs, Peers: f.peers}, f.err
}

func (f fakeStatus) MDNSPublished(context.Context) (string, error) {
	if f.mdnsAsked != nil {
		*f.mdnsAsked++
	}
	return f.mdns, f.mdnsErr
}
func (f fakeStatus) MDNSOther(context.Context) (string, error) {
	if f.mdnsAsked != nil {
		*f.mdnsAsked++
	}
	return f.other, nil
}

func (f fakeStatus) AppActive(_ context.Context, unit string) (bool, error) {
	return f.active[unit], f.activeErr
}

func (f fakeStatus) AppHealth(_ context.Context, url string) (bool, error) {
	if f.probed != nil {
		*f.probed = url
	}
	return f.health, f.hlthErr
}

// AppHealthOf is the per-app probe, keyed on the app name rather than a URL: `svcHealth` maps a
// name to the guest's answer, and an absent entry is an app the guest cannot resolve, which must
// leave the field empty, not report it unhealthy.
func (f fakeStatus) AppHealthOf(_ context.Context, app string) (apps.Health, error) {
	h, known := f.svcHealth[app]
	if !known {
		return apps.HealthUnknown, errors.New("not in the routing table")
	}
	return h, nil
}

func (f fakeStatus) VIP(context.Context, string) (string, error) {
	if f.vipAsked != nil {
		*f.vipAsked++
	}
	return f.vip, f.vipErr
}

func (f fakeStatus) SystemPath(context.Context) (string, error) {
	return f.system, f.sysErr
}

func (f fakeStatus) Resources(context.Context, map[string]string, string, bool) (telemetry.NodeResources, error) {
	if f.resAsked != nil {
		*f.resAsked++
	}
	return f.res, f.resErr
}

// What the volume carries: `volume` maps an app name to its manifest bytes, so a test can put this
// node in a converged survivor's state, running what somebody else installed.
func (f fakeStatus) AppList(context.Context) ([]string, error) {
	names := make([]string, 0, len(f.volume))
	for n := range f.volume {
		names = append(names, n)
	}
	sort.Strings(names)
	return names, f.volumeErr
}

func (f fakeStatus) AppInstalled(_ context.Context, name string) (string, error) {
	return f.volume[name], nil
}

func (f fakeStatus) SupportsAppList() bool { return !f.noAppList }

// TestMain points every ConfigFromEnv test at a path that does not exist, so a run on a machine
// with an install (where /opt/briard/config.env is real) reads the same nothing a sandbox does,
// and the suite's answers do not depend on the host.
func TestMain(m *testing.M) {
	os.Setenv("BRIARD_CONFIG", filepath.Join(os.TempDir(), "briard-no-such-config.env"))
	os.Exit(m.Run())
}

// The config file is the default layer under the environment: it delivers the node's deployment
// wiring and loses to anything the environment says, including an explicitly empty entry. Empty
// is how the substrate fork says "this node has no service tap", so a file value winning there
// would render a NIC the node does not have.
func TestConfigFromEnv_FileIsTheLayerUnderTheEnvironment(t *testing.T) {
	cfgFile := filepath.Join(t.TempDir(), "config.env")
	if err := os.WriteFile(cfgFile, []byte(
		"# a comment, and the blank line below it\n"+
			"\n"+
			"  SYSTEM_TAP = briard-drbd0  \n"+ // whitespace either side of the `=` is trimmed
			"SERVICE_TAP=briard0\n"+
			"VIP_ADDR=192.168.9.50/24\n"+
			"garbage-with-no-equals\n"+
			"NODE=from-the-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BRIARD_CONFIG", cfgFile)
	t.Setenv("NODE", "from-the-environment") // a set value wins
	t.Setenv("SERVICE_TAP", "")              // and so does an empty one

	cfg := ConfigFromEnv()
	if cfg.Node != "from-the-environment" {
		t.Errorf("Node = %q, want the environment's value", cfg.Node)
	}
	if cfg.ServiceTap != "" {
		t.Errorf("ServiceTap = %q, want the environment's empty value to win", cfg.ServiceTap)
	}
	if cfg.SystemTap != "briard-drbd0" {
		t.Errorf("SystemTap = %q, want the file's value", cfg.SystemTap)
	}
	if cfg.VIPAddr != "192.168.9.50/24" {
		t.Errorf("VIPAddr = %q, want the file's value", cfg.VIPAddr)
	}
}

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
	if cfg.MemoryMB != 1024 || cfg.QEMUBinary != "qemu-system-x86_64" {
		t.Errorf("VM defaults wrong: mem=%d bin=%q", cfg.MemoryMB, cfg.QEMUBinary)
	}
	if cfg.Cores != runtime.NumCPU() {
		t.Errorf("Cores default = %d, want every core (%d)", cfg.Cores, runtime.NumCPU())
	}
}

// ⚠️ Nothing the install laid down is defaulted, and this is the assertion that says so.
// `briard-agent install` writes paths to files the install creates or stages (the qemu tree it
// extracted, the guest image, the disks) because they are facts about this host. The agent must
// not invent them: an empty value is how a harness says "I have no such disk, no -L directory",
// and a default would hand qemu `-drive file=...` for a missing file, so the guest never boots.
// The taps and device names follow the other rule (they default, through `declared`), so both
// are asserted and neither is inferred from the other.
func TestConfigFromEnv_TheInstallsOwnPathsAreNeverInvented(t *testing.T) {
	for _, k := range []string{
		"QEMU_DATADIR", "GUEST_IMAGE", "DATA_DISK", "STATE_DISK", "NET_WRAP_BIN",
	} {
		os.Unsetenv(k)
		t.Cleanup(func() { os.Unsetenv(k) })
	}
	c := ConfigFromEnv()
	for _, g := range []struct{ name, got string }{
		{"QEMUDataDir", c.QEMUDataDir}, {"GuestImage", c.GuestImage},
		{"DataDisk", c.DataDisk}, {"StateDisk", c.StateDisk}, {"NetWrapBin", c.NetWrapBin},
	} {
		if g.got != "" {
			t.Errorf("unset %s = %q, want empty -- the agent invented a path to a file nobody made", g.name, g.got)
		}
	}
}

// The shipped agent must launch its guest with a monitor. The clean shutdown (ACPI over QMP, and
// a forced reset for a guest past talking to) needs QMPSock; unset, every stop falls through to
// killing qemu, a power cut. A mechanism that exists but is unreachable is what a default is for.
// It also gets its own directory, because platform.Launch chmods that directory 0700: QMP is
// unrestricted control of the VM, so sharing a directory with a socket meant to be reachable
// (AdminSock) would either widen QMP or break the admin door.
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

// The environment cannot install an app: no SERVICE_IMAGE (or any sibling) is read, so an anchor
// comes up with the empty set and fills it from what it was actually installed with (the
// node-local cache, or the volume once it promotes). Setting those variables must do nothing: an
// env path would give a node an app nobody installed.
func TestConfigFromEnv_NoAppComesFromTheEnvironment(t *testing.T) {
	for _, k := range []string{"SERVICE_NAME", "SERVICE_IMAGE", "SERVICE_DATADIR"} {
		t.Setenv(k, "briard-dummy:v0")
	}
	t.Setenv("ROLE", string(model.RoleAnchor))
	if apps := ConfigFromEnv().Apps; len(apps) != 0 {
		t.Fatalf("Apps = %+v, want none: an app is installed at runtime, never configured", apps)
	}
}

// The chain is static (data -> vip -> apps) on every data node, whatever is installed. That makes
// converge-at-promotion possible: drbd-reactor promotes with the chain, but the volume to converge
// to is readable only after promotion, so the start-list cannot name the apps. Naming a unit the
// guest does not define fails the whole ordered chain and takes the VIP down on every fresh
// install, precisely the state `curl | sh` lands in; briard-apps can be in it unconditionally
// because the guest agent always renders it, as it does briard-primary-storage and briard-vip.
// Nothing is conditional: the chain is the same units on every anchor. This guards the shape, not
// the membership: whatever the list becomes, nothing about the environment or the installed apps
// may alter it.
func TestConfigFromEnv_TheChainIsStatic(t *testing.T) {
	t.Setenv("ROLE", string(model.RoleAnchor))
	cfg := ConfigFromEnv()
	if len(cfg.Apps) != 0 {
		t.Errorf("Apps = %+v with nothing installed, want the empty set", cfg.Apps)
	}
	want := []string{
		"briard-primary-storage.service",
		"briard-vip.service",
		"briard-apps.service",
		"briard-reverse-proxy.service",
		"briard-dashboard.service",
	}
	if !slices.Equal(cfg.Promoter, want) {
		t.Errorf("promoter chain = %v, want %v", cfg.Promoter, want)
	}
	// A runtime-installed app does not join it: converge starts those, so a crashed container
	// cannot deactivate the promoter's target and demote the node.
	t.Setenv("SERVICE_IMAGE", "briard-dummy:v0")
	if got := ConfigFromEnv().Promoter; !slices.Equal(got, want) {
		t.Errorf("promoter chain = %v, want the same static %v whatever the environment carries", got, want)
	}
}

// There is no baked probe target. A default such as a lab address (http://192.168.1.100/healthz)
// is a guess about someone else's network that every test agrees with and nothing in our own can
// contradict. Unset means "resolve it from the address the guest actually holds", the only source
// that can be right on a LAN we have never seen. The target is the reverse proxy, not an app's
// own port; guest.ResolveHealthURL keeps it so with a dynamic address.
func TestConfigFromEnv_NoBakedHealthTarget(t *testing.T) {
	t.Setenv("HEALTH_URL", "")
	t.Setenv("ROLE", string(model.RoleAnchor))
	t.Setenv("VIP_DEV", "eth2")
	cfg := ConfigFromEnv()
	if cfg.HealthURL != "" {
		t.Errorf("HealthURL = %q, want empty: no address may be baked into the product path", cfg.HealthURL)
	}
	// Empty must not read as "nothing to probe" on a data node (that meaning is the witness's
	// alone); the device the agent was told about makes it resolvable instead.
	if cfg.Diskless {
		t.Fatal("precondition: an anchor is not diskless")
	}
	if cfg.VIPDev == "" {
		t.Error("with no baked target, VIP_DEV is what makes the probe resolvable at all")
	}
}

// An explicitly set HEALTH_URL still pins the probe, so the resolution is an override rather than
// a replacement, and a node with an unusual reverse proxy stays configurable without a default
// for everyone.
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

// ⚠️ The two halves of `declared`, which lets the NIC names default in config.go without losing
// the one meaning an empty value carries. A key nobody named takes the shipped answer (every
// install: the install writes no device names). A key named empty keeps the empty: that is how a
// node says it has no such NIC, which env() cannot express (it reads an explicit empty as unset
// and returns the default). Getting it wrong renders a NIC the node does not have, on the
// substrate where that is precisely the fork.
func TestConfigFromEnv_NICNamesDefaultButAnExplicitEmptyWins(t *testing.T) {
	for _, k := range []string{"SYSTEM_TAP", "SERVICE_TAP", "WITNESS_TAP", "SYSTEM_DEV", "VIP_DEV"} {
		os.Unsetenv(k)
		t.Cleanup(func() { os.Unsetenv(k) })
	}
	c := ConfigFromEnv()
	for _, g := range []struct{ name, got, want string }{
		{"SystemTap", c.SystemTap, "briard-drbd0"},
		{"ServiceTap", c.ServiceTap, "briard0"},
		{"WitnessTap", c.WitnessTap, "briard-priv0"},
		{"SystemDev", c.SystemDev, "eth1"},
		{"VIPDev", c.VIPDev, "eth2"},
	} {
		if g.got != g.want {
			t.Errorf("unset %s = %q, want the shipped %q", g.name, g.got, g.want)
		}
	}
	t.Setenv("SERVICE_TAP", "")
	t.Setenv("WITNESS_TAP", "")
	if c := ConfigFromEnv(); c.ServiceTap != "" || c.WitnessTap != "" {
		t.Errorf("an explicit empty was overruled by the default: service tap=%q witness tap=%q",
			c.ServiceTap, c.WitnessTap)
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
	got := parsePeers("n1@10.0.0.2/disk, n2@10.0.0.3:7000/sdb , w@10.0.0.4/none")
	want := []drbd.Peer{
		// The disk field is a diskful/diskless flag, not a path: every diskful node runs on the one
		// seam LV, so any non-"none" value means that device, and "sdb" below gives no node a
		// backing it can disagree with the flock about.
		{Name: "n1", NodeID: 0, Address: "10.0.0.2:7789", Disk: drbd.DataDevice}, // bare host -> default port
		{Name: "n2", NodeID: 1, Address: "10.0.0.3:7000", Disk: drbd.DataDevice}, // explicit port kept
		{Name: "w", NodeID: 2, Address: "10.0.0.4:7789"},                         // witness: "none" -> diskless
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
	if p := parsePeers("garbage,,n1@10.0.0.2/disk"); len(p) != 1 || p[0].NodeID != 0 {
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

// The status carries the name the node is really publishing, never the configured one. avahi
// conflict-renames on a collision and tells nobody, so two flocks in one house can even swap names
// across a reboot, and a report echoing cfg.FlockName would let nobody (household, journal, cloud)
// ever notice. As with reading the VIP off the interface rather than trusting vip.env: what we
// asked for is not evidence of what is in force.
func TestMDNSNames_ReportsThePublishedNameNotTheConfiguredOne(t *testing.T) {
	cfg := Config{Node: "briard-node-3f9a2c", Role: model.RoleAnchor, FlockName: "brave-elf"}
	r := fakeStatus{mdns: "brave-elf-2", other: "192.168.9.60"}
	st := api.NodeStatus{NodeName: cfg.Node}
	mdnsNames(context.Background(), r, &st)
	if st.OtherBriard != "192.168.9.60" {
		t.Errorf("OtherBriard = %q, want the other briard the reverse proxy heard", st.OtherBriard)
	}
	if st.PublishedName != "brave-elf-2" {
		t.Errorf("PublishedName = %q, want the established brave-elf-2 (configured: %q)",
			st.PublishedName, cfg.FlockName)
	}
	if st.PublishedName == cfg.FlockName {
		t.Error("the report echoed the configured name -- a silent avahi rename would be invisible")
	}
}

// A node publishing nothing reports nothing, never falling back to the configured name. Empty
// means "we do not currently know of a published name", the honest answer for a Secondary (the
// name is bound to the VIP) and for a failed read alike.
func TestMDNSNames_UnknownPublishedNameIsEmptyNotTheConfiguredOne(t *testing.T) {
	cfg := Config{Node: "briard-node-3f9a2c", Role: model.RoleAnchor, FlockName: "brave-elf"}
	for _, c := range []struct {
		what string
		r    fakeStatus
	}{
		{"a Secondary publishes no name", fakeStatus{}},
		{"the read failed", fakeStatus{mdnsErr: errors.New("channel hiccup")}},
	} {
		st := api.NodeStatus{NodeName: cfg.Node}
		mdnsNames(context.Background(), c.r, &st)
		if st.PublishedName != "" {
			t.Errorf("%s: PublishedName = %q, want empty -- never the configured %q",
				c.what, st.PublishedName, cfg.FlockName)
		}
	}
}

// The loop's per-cycle snapshot does not ask for the published name: nothing the loop decides
// depends on it, so it is read when something that carries it asks (the controller report,
// doctor). A cadence is for what the loop acts on; everything else is pulled.
func TestSnapshot_DoesNotAskThePublishedName(t *testing.T) {
	cfg := Config{Node: "briard-node-3f9a2c", Role: model.RoleAnchor, FlockName: "brave-elf"}
	cfg.Resource.Name = "r0"

	asked := 0
	qs := model.QuorumState{Primary: true, Quorate: true, Connected: 2}
	r := fakeStatus{qs: qs, vip: "192.168.9.50/24", health: true, mdns: "brave-elf", mdnsAsked: &asked}
	if _, _, _, err := snap(cfg, r, "/nix/store/sys"); err != nil {
		t.Fatal(err)
	}
	if asked != 0 {
		t.Errorf("the snapshot made %d mDNS reads; the loop must make none", asked)
	}
}

// A witness's health follows quorum, and its role says so, not an empty HealthURL: a data node
// acquires its address by DHCP, so "no configured URL" is not witness-shaped. The two answer
// differently on purpose (see the data-node cases below).
func TestSnapshot_HealthFollowsQuorumOnAWitness(t *testing.T) {
	cfg := Config{Node: "n1", Role: model.RoleDiskless, Diskless: true, HealthURL: ""}
	cfg.Resource.Name = "r0"

	qs := model.QuorumState{Primary: true, Quorate: true, Connected: 2}
	st, _, _, _ := snap(cfg, fakeStatus{qs: qs}, "/nix/store/sys")
	if st.NodeName != "n1" || st.Role != model.RoleDiskless {
		t.Errorf("identity not preserved: %+v", st)
	}
	if st.Quorum != qs {
		t.Errorf("Quorum = %+v, want %+v", st.Quorum, qs)
	}
	if !st.Healthy {
		t.Error("a quorate witness should read healthy: it has nothing to probe")
	}
	if len(st.Apps) != 0 {
		t.Errorf("Apps = %+v, want none: a witness runs nothing", st.Apps)
	}
	if st.System != "/nix/store/sys" {
		t.Errorf("System = %q, want the running system reported through the seam", st.System)
	}
}

// Observe returns ErrChannelDown so Run re-dials — without it a single dropped channel would
// blind the host forever.
func TestObserveReturnsOnChannelDown(t *testing.T) {
	cfg := Config{Node: "n1", Role: model.RoleAnchor, StatusEvery: time.Millisecond}
	cfg.Resource.Name = "r0"
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second) // safety net
	defer cancel()
	err := cfg.observe(ctx, fakeStatus{err: guestfirmware.ErrChannelDown}, nil, nil, nil, nil, nil, nil, "", nil, &[]api.DirectiveOutcome{}, func(string, ...any) {})
	if !errors.Is(err, guestfirmware.ErrChannelDown) {
		t.Errorf("observe = %v, want ErrChannelDown (to trigger reconnect)", err)
	}
}

// A verb-level read error (channel alive, guest degraded) must not trigger a reconnect: observe
// keeps observing (reporting degraded) until ctx, so a cold DRBD can't spin the reconnect loop.
func TestObserveRidesOutVerbError(t *testing.T) {
	cfg := Config{Node: "n1", Role: model.RoleAnchor, StatusEvery: time.Millisecond}
	cfg.Resource.Name = "r0"
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	r := fakeStatus{err: errors.New("drbdsetup: no such resource r0")} // verb error, channel fine
	if err := cfg.observe(ctx, r, nil, nil, nil, nil, nil, nil, "", nil, &[]api.DirectiveOutcome{}, func(string, ...any) {}); err != nil {
		t.Errorf("observe on a verb error = %v, want nil (keep observing until ctx)", err)
	}
}

// The loop asks the guest for its address once per cycle, however many owners want it (the health
// URL, the host's route, the ipvtap copier, casa): the channel serves one verb at a time, and four
// round trips for one fact were four turns nothing else could take. Counted against the cluster
// read, exactly one per cycle.
func TestObserveAsksTheAddressOncePerCycle(t *testing.T) {
	// WitnessTap arms the host-route owner too; with no node IPs its route spec is refused before
	// any exec, so its read runs and nothing touches the network.
	cfg := Config{Node: "n1", Role: model.RoleAnchor, StatusEvery: time.Millisecond, VIPDev: "eth0", WitnessTap: "tap0"}
	cfg.Resource.Name = "r0"
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	var cycles, vips int
	r := fakeStatus{qs: model.QuorumState{Primary: true, Quorate: true}, vip: "192.168.9.50/24", health: true,
		clusterAsked: &cycles, vipAsked: &vips}
	if err := cfg.observe(ctx, r, nil, nil, nil, nil, nil, nil, "", nil, &[]api.DirectiveOutcome{}, func(string, ...any) {}); err != nil {
		t.Fatalf("observe = %v", err)
	}
	if cycles == 0 {
		t.Fatal("the loop never ran a cycle")
	}
	if vips != cycles {
		t.Errorf("net.vip asked %d times over %d cycles; want exactly one per cycle", vips, cycles)
	}
}

// A request is not a cycle. The tray asks for status every 5 s and the page polls while a card
// moves; each is answered from what the last tick saw, never by rerunning its reads, which would
// double the idle agent's cost. With an hour-long tick, ten requests leave the loop at its first
// cycle.
func TestObserveAnswersARequestWithoutACycle(t *testing.T) {
	cfg := Config{Node: "n1", Role: model.RoleAnchor, StatusEvery: time.Hour}
	cfg.Resource.Name = "r0"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var cycles int
	r := fakeStatus{qs: model.QuorumState{Primary: true, Quorate: true}, health: true, clusterAsked: &cycles}
	local := make(chan localRequest)
	done := make(chan error, 1)
	go func() {
		done <- cfg.observe(ctx, r, nil, nil, nil, nil, nil, nil, "", local, &[]api.DirectiveOutcome{}, func(string, ...any) {})
	}()
	for range 10 {
		await(t, "status", submit(local, api.DirectiveStatus), 5*time.Second)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("observe = %v", err)
	}
	if cycles != 1 {
		t.Errorf("ten requests ran %d cycles; want the loop's first alone", cycles)
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

	if err := cfg.observe(ctx, fakeStatus{}, nil, nil, nil, cc, nil, nil, "default", nil, &[]api.DirectiveOutcome{}, func(string, ...any) {}); err != nil {
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
	if err := cfg.observe(ctx, fakeStatus{}, up, nil, nil, nil, nil, nil, "", nil, &[]api.DirectiveOutcome{}, func(string, ...any) {}); err != nil {
		t.Fatalf("observe: %v", err)
	}
	if up.imageTarget.Version != "" || up.rescued {
		t.Errorf("no cloud reachable -> no planned op, but the upgrader ran: %+v", up)
	}
}

func TestSnapshot_StatusErrorIsUnhealthy(t *testing.T) {
	cfg := Config{Node: "n1", Role: model.RoleAnchor}
	sentinel := errors.New("channel down")
	st, _, _, err := snap(cfg, fakeStatus{err: sentinel}, "")
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
	// Health comes from the probe, not the quorum bit, and prefers the in-guest verb
	// (fakeStatus.health) over the host-side LAN probe.
	cfg := Config{Node: "n1", Role: model.RoleAnchor, HealthURL: "http://unused.invalid/healthz"}

	// Quorate but the in-guest probe says sick -> unhealthy (health != quorum).
	st, _, _, _ := snap(cfg, fakeStatus{qs: model.QuorumState{Quorate: true}, health: false}, "")
	if !st.Quorum.Quorate {
		t.Fatal("precondition: node is quorate")
	}
	if st.Healthy {
		t.Error("in-guest probe false must read unhealthy despite quorum")
	}
	// Non-quorate but the in-guest probe says healthy -> healthy (health != quorum).
	st, _, _, _ = snap(cfg, fakeStatus{qs: model.QuorumState{Quorate: false}, health: true}, "")
	if !st.Healthy {
		t.Error("in-guest probe true must read healthy")
	}
}

// With no address of our own choosing, the loop probes the one the guest reports, the DHCP lease
// it acquired at promotion: the host does not decide the service address, so it must be told it,
// every cycle.
func TestSnapshot_HealthProbesTheAddressTheGuestReports(t *testing.T) {
	var probed string
	cfg := Config{Node: "n1", Role: model.RoleAnchor, HealthURL: "", VIPDev: "eth2"}
	r := fakeStatus{qs: model.QuorumState{Quorate: true}, health: true, vip: "192.168.9.50/24", probed: &probed}

	st, _, _, _ := snap(cfg, r, "")
	if want := "http://192.168.9.50/healthz"; probed != want {
		t.Errorf("probed %q, want the reverse proxy at the REPORTED lease %q", probed, want)
	}
	if !st.Healthy {
		t.Error("a node answering at its acquired address is healthy")
	}
}

// An address we set is the address we probe, though the guest could be asked. The device can hold
// more than one address (dhcpcd still serves the service NIC), and preferring what it reports
// would probe, at the worst moment, a node-local lease that keeps answering after the VIP has moved
// to the peer: healthy while not serving.
func TestSnapshot_ConfiguredAddressWinsOverTheReportedOne(t *testing.T) {
	var probed string
	cfg := Config{Node: "n1", Role: model.RoleAnchor, HealthURL: "http://192.168.9.7/healthz", VIPDev: "eth2"}
	r := fakeStatus{qs: model.QuorumState{Quorate: true}, health: true, vip: "192.168.9.50/24", probed: &probed}

	if _, _, _, _ = snap(cfg, r, ""); probed != "http://192.168.9.7/healthz" {
		t.Errorf("probed %q, want the CONFIGURED address", probed)
	}
}

// A Primary with no address, configured or reported, is not healthy: nobody in the house can reach
// it. Answering it like a witness ("healthy == quorate") hides that: briard-vip times out waiting
// for DHCP, takes drbd-services@r0 down with it, and the node stays quorate throughout.
func TestSnapshot_PrimaryWithNoAddressIsUnhealthy(t *testing.T) {
	var probed string
	cfg := Config{Node: "n1", Role: model.RoleAnchor, HealthURL: "", VIPDev: "eth2"}
	// Quorate and up to date, all a secondary needs to read healthy below: the primary bit is the
	// only difference, and must be enough on its own.
	r := fakeStatus{
		qs:     model.QuorumState{Primary: true, Quorate: true, Diskful: true, UpToDate: true},
		health: true, vip: "", probed: &probed,
	}

	st, _, _, _ := snap(cfg, r, "")
	if st.Healthy {
		t.Error("a quorate primary holding no service address must NOT read healthy")
	}
	if probed != "" {
		t.Errorf("nothing to probe, yet it probed %q", probed)
	}
}

// A Secondary holds no service address because the VIP is promoter-driven, not because anything
// is wrong, so its health is participation, the witness's rule. Reporting it unhealthy would make
// every correct HA pair read degraded in the cloud's view forever, a standing "1 node unhealthy"
// nobody could act on.
func TestSnapshot_SecondaryWithNoAddressIsHealthyWhenParticipating(t *testing.T) {
	cfg := Config{Node: "n2", Role: model.RoleAnchor, HealthURL: "", VIPDev: "eth2"}
	participating := model.QuorumState{Primary: false, Quorate: true, Diskful: true, UpToDate: true}
	// ...beside the anchor that holds the house: standing by is a job only while someone serves.
	holder := []model.PeerState{{Name: "n1", Connected: true, Role: "Primary", Diskful: true, UpToDate: true}}

	st, _, probe, _ := snap(cfg, fakeStatus{qs: participating, peers: holder, vip: ""}, "")
	if !st.Healthy {
		t.Error("a quorate, up-to-date secondary is doing its whole job and must read healthy")
	}
	if probe != "" {
		t.Errorf("a secondary has nothing to probe, got %q", probe)
	}

	// Not up to date is not a viable failover target, the node-local fact the OS-upgrade gate leans
	// on (model.QuorumState.UpToDate exists because that gate had nothing else to ask a
	// Secondary); health must not be softer than the gate.
	syncing := participating
	syncing.UpToDate = false
	if st, _, _, _ := snap(cfg, fakeStatus{qs: syncing, peers: holder, vip: ""}, ""); st.Healthy {
		t.Error("a secondary still syncing must not read healthy")
	}

	// Non-quorate is the partitioned survivor: participating in nothing.
	isolated := participating
	isolated.Quorate = false
	if st, _, _, _ := snap(cfg, fakeStatus{qs: isolated, peers: holder, vip: ""}, ""); st.Healthy {
		t.Error("a non-quorate secondary must not read healthy")
	}

	// A lone node not serving (its chain failed, e.g. a converge that could not fetch an image or a
	// member past its start limit, and the hold took it down) stands by for nobody: quorate and up
	// to date on its own, it is a house nobody holds and must read unhealthy.
	if st, _, _, _ := snap(cfg, fakeStatus{qs: participating, vip: ""}, ""); st.Healthy {
		t.Error("a lone node that is not serving has no one to stand by for and must not read healthy")
	}
}

// When the guest predates app.health (the verb errors), snapshot falls back to the legacy
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

	verbErr := errors.New("guestagent: unknown verb \"app.health\"")

	cfg := Config{Node: "n1", Role: model.RoleAnchor, HealthURL: ok.URL}
	st, _, _, _ := snap(cfg, fakeStatus{qs: model.QuorumState{Quorate: true}, hlthErr: verbErr}, "")
	if !st.Healthy {
		t.Error("verb error must fall back to the host probe (200 -> healthy)")
	}
	cfg.HealthURL = sick.URL
	st, _, _, _ = snap(cfg, fakeStatus{qs: model.QuorumState{Quorate: true}, hlthErr: verbErr}, "")
	if st.Healthy {
		t.Error("verb error + host probe 500 -> unhealthy")
	}
}

// The OS-upgrade budget is configurable (so the lab need not spend the production interval to
// watch a rollback), which is why its default is asserted. It is no timeout detail: AwaitOSReady
// polls until this context ends, so it is how long a broken update leaves a node degraded before
// it reverts, and it also bounds staging, the one step that goes to the network. Drifting down
// would falsely roll back slow-but-healthy upgrades on a slow link; up, it would leave a node
// degraded longer than anyone agreed to.
func TestConfigFromEnv_UpgradeBudgetDefault(t *testing.T) {
	t.Setenv("NODE_NAME", "n1")
	t.Setenv("RESOURCE_NAME", "r0")

	cfg := ConfigFromEnv()
	if cfg.UpgradeBudget != 15*time.Minute {
		t.Errorf("UpgradeBudget default = %v, want 15m — see Config.UpgradeBudget before changing this", cfg.UpgradeBudget)
	}

	// And it is overridable, which is the point of the setting.
	t.Setenv("UPGRADE_BUDGET", "90s")
	if got := ConfigFromEnv().UpgradeBudget; got != 90*time.Second {
		t.Errorf("UPGRADE_BUDGET=90s gave %v", got)
	}
}

// The VIP's MAC is its identity, so it is flock-scoped: a flock's nodes present one service MAC,
// draw the same DHCP lease, and keep the same address when the VIP moves between them. The
// DRBD and witness MACs stay node-scoped: they must differ per node or the NICs collide and ARP
// never resolves.
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

// No flock id -> the node name, unchanged, which keeps every installed node and agent-less harness
// on the MAC it has always had rather than silently drawing a new DHCP lease on a newer agent.
func TestServiceMACFallsBackToNodeWithoutFlockID(t *testing.T) {
	if got, want := deriveMAC(orNode("", "guest"), "svc"), deriveMAC("guest", "svc"); got != want {
		t.Errorf("without a flock id the service MAC must be the node-derived one: %s != %s", got, want)
	}
}

// The TCG fallback is the silent one. `-machine accel=kvm:tcg` is a request, not a declaration: a
// host without virtualisation extensions gets a guest that boots, converges and serves correctly,
// roughly an order of magnitude slower. Nothing fails, so nothing reports it, and the cost lands
// on whoever asks "why is this node slow?" months later. This asserts the line that answers them,
// and that it comes from asking qemu (`query-accelerators`), not from re-reading the argv we
// chose. The check asks for a name; the deciding case is the third: a guest accelerated by
// something other than KVM must not log a warning, which `query-kvm` would (the question is
// KVM-shaped, and WHPX answers "no").
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
			// The case asking for a name exists for: a hypervisor that is not KVM is still a
			// hypervisor. `query-kvm` answers {"enabled":false} here, and this line would
			// call a correctly accelerated node emulated.
			name:    "accelerated by something that is not KVM",
			accel:   `{"return":{"enabled":"whpx","present":["qtest","tcg","whpx"]}}`,
			want:    []string{"accelerated by whpx"},
			notWant: "WARNING",
		},
		{
			// An unknown accelerator reads as acceleration, not a fault: the enum grows with
			// QEMU and this code should not need editing when it does. Only tcg is emulation.
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

// A cancellation during bring-up is the shutdown the agent was asked to perform, not a failure.
// Run must return nil because main hands a non-nil return to log.Fatalf: otherwise a `systemctl
// restart` (or a host reboot) in the bring-up window makes systemd record `Failed with result
// 'exit-code'` against a unit that did exactly as told, and a false fault in the journal is worth
// as much as a missing one. Cancelled before the call, not raced: the guard is `ctx.Err() != nil`
// after bring-up returns, so a pre-cancelled context exercises it deterministically on a loaded
// runner.
func TestRunTreatsACancellationDuringBringUpAsAShutdown(t *testing.T) {
	// Non-vacuity, the reason this test can fail at all: with a live context this config cannot
	// bring a guest up (no QEMU, no sockets), so Run must error, or the nil asserted below proves
	// nothing.
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

	// The error is logged, not swallowed: how far bring-up got is all that distinguishes "stopped
	// while launching" from "stopped while handshaking".
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

// The guest's machine UUID is a pure function of the node name: stable across boots
// with no persisted state, distinct per node, and a well-formed version-5 UUID.
func TestDeriveUUID(t *testing.T) {
	a, b := deriveUUID("briard-node-a1b2c3"), deriveUUID("briard-node-d4e5f6")
	if a == b {
		t.Fatal("two nodes derived the same machine UUID")
	}
	if a != deriveUUID("briard-node-a1b2c3") {
		t.Fatal("the derivation is not stable")
	}
	if len(a) != 36 || a[8] != '-' || a[13] != '-' || a[14] != '5' || a[18] != '-' || a[23] != '-' {
		t.Errorf("not a version-5 UUID: %s", a)
	}
	if v := a[19]; v != '8' && v != '9' && v != 'a' && v != 'b' {
		t.Errorf("wrong variant nibble in %s", a)
	}
}

// NodeStatus.System is the guest release this node runs: the host's record of the release whose
// image it booted, not read from the guest, which knows only a closure. The shipped zero-app
// anchor reports it like any node (a node the rollout cannot see, it cannot update); a witness has
// no guest and reports nothing; a node with no record reports nothing rather than guessing.
func TestCurrentSystem(t *testing.T) {
	rec := filepath.Join(t.TempDir(), "guest-release.json")
	os.WriteFile(rec, []byte(`{"chain":"vm","version":"vm.20260906.abc1234","system":"/nix/store/x","artifacts":[{"name":"nixos.qcow2.zst"}]}`), 0o644)
	shipped := Config{GuestReleaseCache: rec}
	if got := shipped.currentSystem(context.Background(), fakeStatus{}); got != "vm.20260906.abc1234" {
		t.Errorf("zero-app anchor: currentSystem = %q, want the recorded release", got)
	}
	witness := Config{Diskless: true, GuestReleaseCache: rec}
	if got := witness.currentSystem(context.Background(), fakeStatus{}); got != "" {
		t.Errorf("witness currentSystem = %q, want empty", got)
	}
	none := Config{GuestReleaseCache: filepath.Join(t.TempDir(), "absent.json")}
	if got := none.currentSystem(context.Background(), fakeStatus{}); got != "" {
		t.Errorf("no record: currentSystem = %q, want empty", got)
	}
}

// A not-Primary anchor with nobody to stand by for is a house nobody holds, not a standby. A lone
// anchor beside a diskless witness, booting a release whose reverse proxy fails, is demoted by its
// own OnFailure, and by participation alone would report healthy=true every five seconds: the
// free-tier owner's whole picture, and wrong.
func TestSnapshot_LoneAnchorThatIsNotPrimaryIsUnhealthy(t *testing.T) {
	cfg := Config{Node: "n1", Role: model.RoleAnchor, HealthURL: "", VIPDev: "eth2"}
	demoted := model.QuorumState{Primary: false, Quorate: true, Diskful: true, UpToDate: true}
	for name, peers := range map[string][]model.PeerState{
		"no peers reported":       nil,
		"only a diskless witness": {{Name: "w", Connected: true, Role: "Secondary", Diskful: false}},
		"peer down":               {{Name: "n2", Connected: false, Role: "Unknown", Diskful: true, UpToDate: true}},
		"peer still syncing":      {{Name: "n2", Connected: true, Role: "Secondary", Diskful: true, UpToDate: false}},
	} {
		if st, _, _, _ := snap(cfg, fakeStatus{qs: demoted, peers: peers, vip: ""}, ""); st.Healthy {
			t.Errorf("%s: a not-Primary anchor with no peer able to hold the house read healthy", name)
		}
	}
	// The same node beside a peer that can hold the house is an ordinary standby.
	holder := []model.PeerState{{Name: "n2", Connected: true, Role: "Primary", Diskful: true, UpToDate: true}}
	if st, _, _, _ := snap(cfg, fakeStatus{qs: demoted, peers: holder, vip: ""}, ""); !st.Healthy {
		t.Error("a quorate, up-to-date standby beside a capable peer must read healthy")
	}
}

// fakeDB is the guest's background recorder check: started, still running or finished with a
// report it hands over once, and the restores it was asked for.
type fakeDB struct {
	starts     int
	running    bool
	report     *hass.DBReport
	restored   []string
	restoreErr error
}

// fakeBackup is the guest's background backup: started (with what), still running or finished
// with a report it hands over once.
type fakeBackup struct {
	starts     int
	url, group string
	host       map[string][]byte
	running    bool
	report     *guestagent.BackupReport
	key        guestagent.BackupKey // what the volume holds
	adopted    string               // the key the guest was handed to adopt with; "" for none
	adoptErr   error
	noAdopt    bool // does not advertise data.backup.adopt
}

func (f fakeStatus) DataBackupAdopt(_ context.Context, url, password string) error {
	if f.backup == nil {
		return errors.New("unknown verb")
	}
	f.backup.adopted = password
	return f.backup.adoptErr
}
func (f fakeStatus) SupportsBackupAdopt() bool { return f.backup != nil && !f.backup.noAdopt }

func (f fakeStatus) DataBackup(_ context.Context, url, group string, host map[string][]byte) (bool, error) {
	if f.backup == nil {
		return false, errors.New("unknown verb")
	}
	if f.backup.running {
		return false, nil
	}
	f.backup.starts++
	f.backup.url, f.backup.group, f.backup.host, f.backup.running = url, group, host, true
	return true, nil
}

func (f fakeStatus) DataBackupKey(_ context.Context, saved bool) (guestagent.BackupKey, error) {
	if f.backup == nil {
		return guestagent.BackupKey{}, errors.New("unknown verb")
	}
	if f.backup.key.Key == "" {
		f.backup.key.Key = "key1-key1-key1-key1-key1-key1-key1-key1"
	}
	if saved {
		f.backup.key.Saved = true
	}
	return f.backup.key, nil
}

func (f fakeStatus) DataBackupResult(context.Context) (guestagent.BackupState, error) {
	if f.backup == nil {
		return guestagent.BackupState{}, errors.New("unknown verb")
	}
	s := guestagent.BackupState{Running: f.backup.running, Report: f.backup.report}
	f.backup.report = nil
	return s, nil
}

func (f fakeStatus) HassDBCheck(context.Context) (bool, error) {
	if f.db == nil {
		return false, errors.New("unknown verb")
	}
	if f.db.running {
		return false, nil
	}
	f.db.starts++
	f.db.running = true
	return true, nil
}

func (f fakeStatus) HassDBCheckResult(context.Context) (hass.DBCheckState, error) {
	if f.db == nil {
		return hass.DBCheckState{}, errors.New("unknown verb")
	}
	s := hass.DBCheckState{Running: f.db.running, Report: f.db.report}
	f.db.report = nil
	return s, nil
}

func (f fakeStatus) HassDBRestore(_ context.Context, member string) error {
	f.db.restored = append(f.db.restored, member)
	return f.db.restoreErr
}

// finish ends the fake's running check with a report.
func (f *fakeDB) finish(rep hass.DBReport) { f.running, f.report = false, &rep }

// snap is a test's snapshot: the fake answers both the status reads and the cycle's one VIP read,
// which in the loop is readVIP's answer handed in.
func snap(cfg Config, r fakeStatus, system string) (api.NodeStatus, model.Cluster, string, error) {
	return cfg.snapshot(context.Background(), r, r, system)
}

// The resource sample is read once a minute on a shipped node (its one reader is the memory
// grower, whose clocks run in minutes, and in the guest the read is a dozen processes), and every
// cycle only where a soak collects it (TelemetryPath, the lab's gate).
func TestObserveSamplesResourcesOnceAMinuteUnlessASoakCollects(t *testing.T) {
	for _, c := range []struct {
		what      string
		telemetry string
		perCycle  bool
	}{
		{"shipped: once a minute", "", false},
		{"lab: every cycle", "/var/lib/briard-fleet/telemetry.json", true},
	} {
		cfg := Config{Node: "n1", Role: model.RoleAnchor, StatusEvery: time.Millisecond, TelemetryPath: c.telemetry}
		cfg.Resource.Name = "r0"
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		var cycles, reads int
		r := fakeStatus{qs: model.QuorumState{Primary: true, Quorate: true}, clusterAsked: &cycles, resAsked: &reads}
		err := cfg.observe(ctx, r, nil, nil, nil, nil, nil, nil, "", nil, &[]api.DirectiveOutcome{}, func(string, ...any) {})
		cancel()
		if err != nil {
			t.Fatalf("%s: observe = %v", c.what, err)
		}
		if cycles < 2 {
			t.Fatalf("%s: only %d cycles ran", c.what, cycles)
		}
		switch {
		case c.perCycle && reads != cycles:
			t.Errorf("%s: sys.resources asked %d times over %d cycles; want one per cycle", c.what, reads, cycles)
		case !c.perCycle && reads != 1:
			t.Errorf("%s: sys.resources asked %d times over %d cycles (30 ms); want one", c.what, reads, cycles)
		}
	}
}

// slowCloud answers every report after a delay, and hands back the directives it was given
// to hand back, once.
type slowCloud struct {
	delay      time.Duration
	directives chan []api.Directive // drained once; later reports bring none
	reports    *int32
	acked      *int32 // outcomes the reports carried up, in total
}

func (c slowCloud) Register(context.Context, api.NodeInfo) (api.Assignment, error) {
	return api.Assignment{}, nil
}
func (c slowCloud) Report(_ context.Context, req api.ReportRequest) ([]api.Directive, error) {
	time.Sleep(c.delay)
	atomic.AddInt32(c.reports, 1)
	atomic.AddInt32(c.acked, int32(len(req.Outcomes)))
	select {
	case ds := <-c.directives:
		return ds, nil
	default:
		return nil, nil
	}
}
func (c slowCloud) ReportMetrics(context.Context, string, []api.MetricAggregate) error { return nil }

// A slow controller costs directive latency, never liveness. The report runs off the loop: with a
// controller that answers in 200 ms the loop cycles many times per report instead of once, one
// report is in flight at a time, and the directives a reply brings are still applied.
func TestObserveDoesNotWaitOnTheReport(t *testing.T) {
	cfg := Config{Node: "n1", Role: model.RoleAnchor, StatusEvery: time.Millisecond}
	cfg.Resource.Name = "r0"
	var cycles, reports, acked int32
	ds := make(chan []api.Directive, 1)
	ds <- []api.Directive{{ID: "d1", Kind: api.DirectiveNoop}}
	r := actingGuest{fakeStatus: fakeStatus{qs: model.QuorumState{Primary: true, Quorate: true}}, cycles: &cycles, handoffs: new(int32)}
	var pending []api.DirectiveOutcome
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if err := cfg.observe(ctx, r, nil, nil, nil, slowCloud{delay: 200 * time.Millisecond, directives: ds, reports: &reports, acked: &acked}, nil, nil, "", nil, &pending, func(string, ...any) {}); err != nil {
		t.Fatal(err)
	}
	n, rp := atomic.LoadInt32(&cycles), atomic.LoadInt32(&reports)
	if rp < 1 || rp > 3 {
		t.Fatalf("reports = %d over 500 ms with a 200 ms controller; want one in flight at a time (1-3)", rp)
	}
	if n < 10*rp {
		t.Fatalf("cycles = %d for %d reports; the loop waited on the controller", n, rp)
	}
	// The first reply's noop was applied: its outcome rode a later report (acked off pending) or
	// is still pending if the run ended first, either way exactly one outcome.
	if a := atomic.LoadInt32(&acked); int(a)+len(pending) != 1 {
		t.Fatalf("acked %d + pending %+v, want the reply's noop directive applied exactly once", a, pending)
	}
}

// A cluster the guest did not answer for is not a cluster this node has left. A failed cluster
// read carries an empty cluster, "not serving" to the promotion edge; it must not move the edge,
// or the next good read looks like a promotion and re-reads the volume for nothing. Counted
// through the volume read the edge makes.
func TestObserveMovesNoPromotionEdgeOnAClusterItCouldNotRead(t *testing.T) {
	cfg := Config{Node: "n1", Role: model.RoleAnchor, StatusEvery: time.Millisecond}
	cfg.Resource.Name = "r0"
	var lists int32
	// Primary and answering on every cycle but one in the middle, which fails as a verb error.
	full := model.QuorumState{Primary: true, Quorate: true, Connected: 2}
	var n int32
	r := edgeGuest{fakeStatus: fakeStatus{qs: full}, lists: &lists, failOn: 3, cycle: &n}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	if err := cfg.observe(ctx, r, nil, nil, nil, nil, nil, nil, "", nil, &[]api.DirectiveOutcome{}, func(string, ...any) {}); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&lists); got != 1 {
		t.Fatalf("the volume was read %d times; want once, at the first Primary cycle -- a failed read must not look like a demotion and the next good one like a promotion", got)
	}
}

// edgeGuest answers Primary and full on every cycle but failOn, where the cluster read is a verb
// error; it counts the volume reads the promotion edge makes.
type edgeGuest struct {
	fakeStatus
	lists  *int32
	failOn int32
	cycle  *int32
}

func (g edgeGuest) Cluster(ctx context.Context, res string) (model.Cluster, error) {
	if atomic.AddInt32(g.cycle, 1) == g.failOn {
		return model.Cluster{}, errors.New("drbdsetup: busy")
	}
	return g.fakeStatus.Cluster(ctx, res)
}
func (g edgeGuest) SupportsAppList() bool { return true }
func (g edgeGuest) AppList(context.Context) ([]string, error) {
	atomic.AddInt32(g.lists, 1)
	return nil, nil
}

// The files tier's tick: a guest that does not run one, unless a test gives it fakeFilesTick.
func (fakeStatus) FilesTick(context.Context) (bool, error) { return false, errors.New("unknown verb") }
func (fakeStatus) FilesTickResult(context.Context) (guestagent.FilesTickState, error) {
	return guestagent.FilesTickState{}, errors.New("unknown verb")
}
func (fakeStatus) SupportsFilesTick() bool { return false }
