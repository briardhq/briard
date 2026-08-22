package host

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"briard.io/agent/drbd"
	"briard.io/agent/guestagent"
	"briard.io/agent/platform"
	"briard.io/shared/api"
)

// fakeMesher records which guest verb the pairing reconcile drove. netCalls captures every
// ConfigureNet (the DRBD NIC + the witness NIC in a forwarded-witness pairing) in order.
type fakeMesher struct {
	netCalled               bool
	netDev, netCIDR, vipDev string
	netCalls                []netCall
	adjusted                *guestagent.ProvisionRequest
	broughtUp               *guestagent.BringUpSpec
}

type netCall struct{ dev, cidr, vipDev, vipAddr string }

func (f *fakeMesher) ConfigureNet(_ context.Context, dev, cidr, vipDev, vipAddr string) error {
	f.netCalled, f.netDev, f.netCIDR, f.vipDev = true, dev, cidr, vipDev
	f.netCalls = append(f.netCalls, netCall{dev, cidr, vipDev, vipAddr})
	return nil
}
func (f *fakeMesher) Adjust(_ context.Context, req guestagent.ProvisionRequest) error {
	f.adjusted = &req
	return nil
}
func (f *fakeMesher) BringUp(_ context.Context, spec guestagent.BringUpSpec) error {
	f.broughtUp = &spec
	return nil
}

// fakeWitness records the witness-forwarder the reconcile started (nil = never started).
type fakeWitness struct{ started *platform.ForwarderSpec }

func (w *fakeWitness) StartForwarder(_ context.Context, s platform.ForwarderSpec) error {
	w.started = &s
	return nil
}

// threePeerMesh is anchorA (seed/primary) + anchorB (blank joiner) + a diskless witness.
func threePeerMesh() []api.MeshPeer {
	return []api.MeshPeer{
		{Name: "anchorA", NodeID: 0, Address: "10.0.0.1:7789", Disk: "/dev/vdb"},
		{Name: "anchorB", NodeID: 1, Address: "10.0.0.2:7789", Disk: "/dev/vdb"},
		{Name: "witness", NodeID: 2, Address: "10.0.0.3:7789"}, // diskless
	}
}

// The serving primary adjusts the RUNNING resource in place (keeps its data): ConfigureNet to its
// replication address, then Adjust with the full 3-peer config -- never BringUp (which would
// create-md and could re-seed).
func TestReconcileMeshPrimaryAdjustsInPlace(t *testing.T) {
	cfg := Config{Node: "anchorA", Promoter: []string{"briard-vip.service"}, VIPDev: "eth2"}
	spec := api.MeshSpec{Resource: "r0", Device: "/dev/drbd0", Peers: threePeerMesh(),
		Join: false, SystemDev: "eth1", SystemCIDR: "10.0.0.1/24"}
	f := &fakeMesher{}
	if err := cfg.reconcileMesh(context.Background(), f, &fakeWitness{}, spec, func(string, ...any) {}); err != nil {
		t.Fatal(err)
	}
	if f.broughtUp != nil {
		t.Error("the primary must NOT bring-up (no create-md on the serving replica)")
	}
	if f.adjusted == nil {
		t.Fatal("the primary must adjust in place")
	}
	if !f.netCalled || f.netCIDR != "10.0.0.1/24" || f.vipDev != "eth2" {
		t.Errorf("configure-net = %+v, want its replication CIDR + the VIP dev", f)
	}
	// The adjusted .res carries the whole widened mesh, and the promoter snippet is (re)written.
	for _, want := range []string{"on anchorA", "on anchorB", "on witness", "connection-mesh"} {
		if !strings.Contains(f.adjusted.ResConfig, want) {
			t.Errorf("adjusted .res missing %q:\n%s", want, f.adjusted.ResConfig)
		}
	}
	if !strings.Contains(f.adjusted.ReactorConfig, "briard-vip.service") {
		t.Errorf("adjusted reactor config = %q, want the promoter unit", f.adjusted.ReactorConfig)
	}
}

// A blank second anchor joins: fresh bring-up as a NON-seed (FreshInit=false), so it attaches and
// resyncs as SyncTarget rather than declaring itself UpToDate (which would split-brain).
func TestReconcileMeshBlankAnchorJoinsAndResyncs(t *testing.T) {
	cfg := Config{Node: "anchorB", Promoter: []string{"briard-vip.service"}, VIPDev: "eth2"}
	spec := api.MeshSpec{Resource: "r0", Device: "/dev/drbd0", Peers: threePeerMesh(),
		Join: true, SystemDev: "eth1", SystemCIDR: "10.0.0.2/24"}
	f := &fakeMesher{}
	if err := cfg.reconcileMesh(context.Background(), f, &fakeWitness{}, spec, func(string, ...any) {}); err != nil {
		t.Fatal(err)
	}
	if f.adjusted != nil {
		t.Error("a blank joiner must NOT adjust (it has no running resource to adjust)")
	}
	if f.broughtUp == nil {
		t.Fatal("a blank joiner must bring up")
	}
	if f.broughtUp.FreshInit {
		t.Error("a joiner must NOT FreshInit (never declare UpToDate -- it resyncs from the primary)")
	}
	if f.broughtUp.Diskless {
		t.Error("a disk-bearing anchor must not be diskless")
	}
	if len(f.broughtUp.Promoter) == 0 {
		t.Error("a disk-bearing anchor runs the promoter (it can be promoted on failover)")
	}
	if len(f.broughtUp.Resource.Peers) != 3 {
		t.Errorf("joiner bring-up mesh has %d peers, want 3", len(f.broughtUp.Resource.Peers))
	}
	if f.netCIDR != "10.0.0.2/24" {
		t.Errorf("joiner configure-net CIDR = %q, want 10.0.0.2/24", f.netCIDR)
	}
}

// The diskless witness joins for the quorum vote only: bring up diskless, no promoter.
func TestReconcileMeshWitnessJoinsDisklessNoPromoter(t *testing.T) {
	cfg := Config{Node: "witness", Promoter: []string{"briard-vip.service"}}
	spec := api.MeshSpec{Resource: "r0", Device: "/dev/drbd0", Peers: threePeerMesh(),
		Join: true, SystemDev: "eth1", SystemCIDR: "10.0.0.3/24"}
	f := &fakeMesher{}
	if err := cfg.reconcileMesh(context.Background(), f, &fakeWitness{}, spec, func(string, ...any) {}); err != nil {
		t.Fatal(err)
	}
	if f.broughtUp == nil {
		t.Fatal("the witness must bring up")
	}
	if !f.broughtUp.Diskless {
		t.Error("the witness must bring up diskless (no metadata, quorum vote only)")
	}
	if len(f.broughtUp.Promoter) != 0 {
		t.Error("the witness runs no promoter (it never serves)")
	}
}

// forwardedWitnessSpec is a managed pairing whose diskless voter is the cloud witness reached
// through each anchor's host forwarder: the witness peer's mesh Address is the host-forwarder listen
// addr, and the MeshWitness block carries the private guest↔host link + the cloud proxy target.
func forwardedWitnessSpec(self string, join bool, cidr string) api.MeshSpec {
	peers := []api.MeshPeer{
		{Name: "anchorA", NodeID: 0, Address: "10.7.0.1:7789", Disk: "/dev/vdb"},
		{Name: "anchorB", NodeID: 1, Address: "10.7.0.2:7789", Disk: "/dev/vdb"},
		{Name: "cloud-witness", NodeID: 2, Address: "10.9.9.1:7789"}, // the host forwarder, not the LAN
	}
	_ = self
	return api.MeshSpec{
		Resource: "r0", Device: "/dev/drbd0", Peers: peers,
		Join: join, SystemDev: "eth1", SystemCIDR: cidr,
		Witness: &api.MeshWitness{
			Dev: "eth3", CIDR: "10.9.9.2/24", LocalAddr: "10.9.9.2:7789",
			Target: "witness.briard.example:7788", ServerName: "witness.briard.example",
		},
	}
}

// witnessCfg is a node configured with the host-held forwarder identity (bin + anchor cert/key/ca).
func witnessCfg(node string) Config {
	return Config{
		Node: node, Promoter: []string{"briard-vip.service"}, VIPDev: "eth2",
		ForwarderBin: "/opt/briard/bin/witness-forwarder",
		WitnessCert:  "/var/lib/briard/pki/node.crt",
		WitnessKey:   "/var/lib/briard/pki/node.key",
		WitnessCA:    "/var/lib/briard/pki/ca.crt",
	}
}

// A forwarded-witness pairing addresses the private witness NIC (eth3), starts the host forwarder on
// the witness peer's mesh address, and renders the explicit-connection .res (WitnessLocal per anchor).
func TestReconcileMeshForwardedWitnessStartsForwarder(t *testing.T) {
	cfg := witnessCfg("anchorA")
	spec := forwardedWitnessSpec("anchorA", false, "10.7.0.1/24")
	f, w := &fakeMesher{}, &fakeWitness{}
	if err := cfg.reconcileMesh(context.Background(), f, w, spec, func(string, ...any) {}); err != nil {
		t.Fatal(err)
	}
	// Both NICs were addressed: eth1 (DRBD) and eth3 (witness link, no VIP dev).
	var eth1, eth3 *netCall
	for i := range f.netCalls {
		switch f.netCalls[i].dev {
		case "eth1":
			eth1 = &f.netCalls[i]
		case "eth3":
			eth3 = &f.netCalls[i]
		}
	}
	if eth1 == nil || eth1.cidr != "10.7.0.1/24" {
		t.Errorf("eth1 configure-net = %+v, want the DRBD CIDR", eth1)
	}
	if eth3 == nil || eth3.cidr != "10.9.9.2/24" || eth3.vipDev != "" {
		t.Errorf("eth3 configure-net = %+v, want the private witness CIDR + no VIP dev", eth3)
	}
	// The forwarder was started at the witness peer's mesh address, tunnelling to the cloud proxy
	// with the host-held anchor cert.
	if w.started == nil {
		t.Fatal("the host witness-forwarder was not started")
	}
	if w.started.Listen != "10.9.9.1:7789" || w.started.Target != "witness.briard.example:7788" {
		t.Errorf("forwarder = %+v, want listen 10.9.9.1:7789 -> the cloud proxy", w.started)
	}
	if w.started.Cert != cfg.WitnessCert || w.started.ServerName != "witness.briard.example" {
		t.Errorf("forwarder identity = %+v, want the host-held anchor cert + proxy SAN", w.started)
	}
	// The .res is the explicit-connection form: each anchor carries its witness-side local address.
	if !strings.Contains(f.adjusted.ResConfig, "10.9.9.2:7789") ||
		strings.Contains(f.adjusted.ResConfig, "connection-mesh") {
		t.Errorf("forwarded-witness .res should be explicit-connection with the witness local:\n%s", f.adjusted.ResConfig)
	}
}

// A forwarded-witness pairing on a node missing the forwarder identity fails BEFORE any DRBD change
// -- a mis-provisioned anchor never half-applies a pairing it can't complete.
func TestReconcileMeshForwardedWitnessFailsWithoutIdentity(t *testing.T) {
	cfg := Config{Node: "anchorA", Promoter: []string{"briard-vip.service"}, VIPDev: "eth2"} // no forwarder bin/cert
	spec := forwardedWitnessSpec("anchorA", false, "10.7.0.1/24")
	f, w := &fakeMesher{}, &fakeWitness{}
	if err := cfg.reconcileMesh(context.Background(), f, w, spec, func(string, ...any) {}); err == nil {
		t.Fatal("a forwarded-witness pairing without forwarder identity must fail")
	}
	if f.adjusted != nil || f.broughtUp != nil {
		t.Error("DRBD must not be touched when the witness path can't be brought up")
	}
	if w.started != nil {
		t.Error("the forwarder must not start without its identity")
	}
}

// The diskless cloud witness itself is provisioned out of band, so it never starts a
// forwarder even when the mesh carries a MeshWitness block.
func TestReconcileMeshForwardedWitnessNodeStartsNoForwarder(t *testing.T) {
	cfg := Config{Node: "cloud-witness"}
	spec := forwardedWitnessSpec("cloud-witness", true, "10.9.9.9/24")
	f, w := &fakeMesher{}, &fakeWitness{}
	if err := cfg.reconcileMesh(context.Background(), f, w, spec, func(string, ...any) {}); err != nil {
		t.Fatal(err)
	}
	if w.started != nil {
		t.Error("the diskless witness node must not start a forwarder (it IS the witness)")
	}
}

// A mesh that doesn't list this node is a mistake, not a silent no-op.
func TestReconcileMeshRefusesWhenSelfAbsent(t *testing.T) {
	cfg := Config{Node: "stranger"}
	spec := api.MeshSpec{Resource: "r0", Device: "/dev/drbd0", Peers: threePeerMesh(), Join: true}
	if err := cfg.reconcileMesh(context.Background(), &fakeMesher{}, &fakeWitness{}, spec, func(string, ...any) {}); err == nil {
		t.Fatal("a mesh without this node must error, not proceed")
	}
}

// ApplyPair parses the directive payload and reports the terminal outcome.
func TestApplyPairOutcome(t *testing.T) {
	cfg := Config{Node: "anchorA", Promoter: []string{"briard-vip.service"}}
	payload, _ := json.Marshal(api.MeshSpec{Resource: "r0", Device: "/dev/drbd0",
		Peers: threePeerMesh(), Join: false, SystemDev: "eth1", SystemCIDR: "10.0.0.1/24"})
	f := &fakeMesher{}
	o := cfg.applyPair(context.Background(), f, &fakeWitness{}, api.Directive{ID: "p1", Kind: api.DirectivePair, Payload: string(payload)}, func(string, ...any) {})
	if o.State != api.OutcomeDone || o.ID != "p1" {
		t.Errorf("good pair outcome = %+v, want done/p1", o)
	}
	if f.adjusted == nil {
		t.Error("applyPair did not drive the reconcile")
	}

	bad := cfg.applyPair(context.Background(), f, &fakeWitness{}, api.Directive{ID: "p2", Kind: api.DirectivePair, Payload: "{not json"}, func(string, ...any) {})
	if bad.State != api.OutcomeFailed {
		t.Errorf("malformed payload outcome = %+v, want failed", bad)
	}
}

// THE DEFECT THIS EXISTS FOR, asserted end-to-end at the host seam ([V3b.16b]): a node that was
// paired at RUNTIME must still be in that mesh after its agent restarts, because bring-up rewrites
// the guest's .res from cfg.Resource on every pass.
//
// Failable by construction rather than by timing. The "after the reboot" half rebuilds Config the
// way ConfigFromEnv does on a fresh process -- PEERS unset, so the single-node self-peer -- and
// then applies the restore. Without cacheMesh/cachedMesh the restore is a no-op and the assertions
// below see 127.0.0.1 and one peer, which is exactly what the field would have seen.
func TestPairedMeshSurvivesAnAgentRestart(t *testing.T) {
	dir := t.TempDir()
	cache := filepath.Join(dir, "mesh.json")

	// Before: this node knows only what install.sh gave it -- no PEERS, so a mesh of one on
	// loopback. This is the shipped single-node state, and the thing that must NOT come back.
	lone := drbd.Resource{Name: "r0", Device: "/dev/drbd0", Peers: []drbd.Peer{
		{Name: "anchorA", NodeID: 0, Address: "127.0.0.1:7789", Disk: "/dev/vdb"},
	}}
	cfg := Config{Node: "anchorA", Promoter: []string{"briard-vip.service"}, VIPDev: "eth2",
		MeshCache: cache, Resource: lone}

	// The cloud pairs it into the 3-voter mesh.
	spec := api.MeshSpec{Resource: "r0", Device: "/dev/drbd0", Peers: threePeerMesh(),
		Join: false, SystemDev: "eth1", SystemCIDR: "10.0.0.1/24"}
	if err := cfg.reconcileMesh(context.Background(), &fakeMesher{}, &fakeWitness{}, spec, func(string, ...any) {}); err != nil {
		t.Fatal(err)
	}

	// The reboot: a brand-new Config, as a restarted agent builds it -- the pairing is nowhere in
	// its environment. Only the cache can put the mesh back.
	fresh := Config{Node: "anchorA", MeshCache: cache, Resource: lone}
	_, res, ok := fresh.cachedMesh(func(string, ...any) {})
	if !ok {
		t.Fatal("a node paired at runtime came back with no cached mesh: bring-up would rewrite " +
			"its .res to the single-node self-peer, against metadata that records real peers")
	}
	fresh.Resource = res

	// What bring-up would now write into the guest -- the assertion that matters, since the .res is
	// the artefact the defect corrupted.
	got := fresh.Resource.Config()
	for _, want := range []string{"on anchorA", "on anchorB", "on witness", "connection-mesh"} {
		if !strings.Contains(got, want) {
			t.Errorf("post-restart .res missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "127.0.0.1") {
		t.Errorf("post-restart .res fell back to the single-node loopback peer:\n%s", got)
	}
	if n := len(fresh.Resource.Peers); n != 3 {
		t.Errorf("post-restart mesh has %d peers, want the 3 it was paired into", n)
	}
	// The node's own node-id must survive too: coming back as node-id 0 when the pairing made it
	// something else is what makes DRBD refuse the metadata rather than merely lose a peer.
	for _, p := range fresh.Resource.Peers {
		if p.Name == "anchorA" && p.Address != "10.0.0.1:7789" {
			t.Errorf("this node's own stanza = %+v, want its replication address", p)
		}
	}
}

// A cache that cannot be used is not a failure mode of its own: the node falls back to the mesh its
// environment describes, which is what it did before a cache existed. Loud, never fatal.
func TestCachedMeshFallsBackWhenUnusable(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct{ name, body string }{
		{"unparseable", "{not json"},
		// A spec that does not name this node -- a rename, or a cache carried onto another node.
		{"does not name this node", `{"resource":"r0","peers":[{"name":"someone-else","node_id":0,"address":"10.0.0.9:7789","disk":"/dev/vdb"}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cache := filepath.Join(dir, tc.name+".json")
			if err := os.WriteFile(cache, []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			var said int
			cfg := Config{Node: "anchorA", MeshCache: cache}
			if _, _, ok := cfg.cachedMesh(func(string, ...any) { said++ }); ok {
				t.Error("an unusable mesh cache must read as absent, not as a mesh")
			}
			if said != 1 {
				t.Errorf("logged %d times, want exactly one line naming the cache", said)
			}
		})
	}
	// Never paired: absent is the shipped single-node state and says nothing at all.
	var said int
	cfg := Config{Node: "anchorA", MeshCache: filepath.Join(dir, "nope.json")}
	if _, _, ok := cfg.cachedMesh(func(string, ...any) { said++ }); ok || said != 0 {
		t.Errorf("absent cache: ok=%v logged=%d, want a silent false (nothing was ever paired)", ok, said)
	}
}

// THE HOST WITNESS-FORWARDER IS RE-CREATED AT BRING-UP ([V3b.16b]). It is a transient systemd unit
// on purpose, so a HOST reboot ends it -- and applyPair was the only thing that ever started one,
// while the cloud sends a pair directive on demand rather than on registration. A mesh restored from
// the cache would then name a witness address nothing was listening on.
//
// Driven through restoreWitnessHop rather than bringUp, which launches qemu: this is the decision
// (does this node need a hop, and what does it start), and it is the whole of what changed.
func TestWitnessHopIsRestoredAtBringUp(t *testing.T) {
	dir := t.TempDir()
	cache := filepath.Join(dir, "mesh.json")

	// A forwarded-witness pairing lands, exactly as the cloud would send it.
	cfg := witnessCfg("anchorA")
	cfg.MeshCache = cache
	spec := forwardedWitnessSpec("anchorA", false, "10.7.0.1/24")
	if err := cfg.reconcileMesh(context.Background(), &fakeMesher{}, &fakeWitness{}, spec, func(string, ...any) {}); err != nil {
		t.Fatal(err)
	}

	// The host reboots: a brand-new Config that knows nothing but its environment, plus the cache.
	fresh := witnessCfg("anchorA")
	fresh.MeshCache = cache
	cached, res, ok := fresh.cachedMesh(func(string, ...any) {})
	if !ok {
		t.Fatal("no cached mesh after a forwarded-witness pairing")
	}
	fresh.Resource, fresh.Mesh = res, cached
	if fresh.Mesh.Witness == nil {
		t.Fatal("the cached mesh dropped its witness block: the .res would name a hop nothing " +
			"re-creates, which is the whole defect")
	}

	f, w := &fakeMesher{}, &fakeWitness{}
	fresh.restoreWitnessHop(context.Background(), f, w, func(string, ...any) {})
	if w.started == nil {
		t.Fatal("bring-up did not re-create the witness hop: after a host reboot the transient unit " +
			"is gone and nothing else ever starts one")
	}
	// The SAME hop the pairing established -- listening where the .res says the witness peer is.
	if w.started.Listen != "10.9.9.1:7789" || w.started.Target != "witness.briard.example:7788" {
		t.Errorf("restored forwarder = %+v, want the witness peer's mesh address -> the cloud proxy", w.started)
	}
	if w.started.Cert != fresh.WitnessCert || w.started.ServerName != "witness.briard.example" {
		t.Errorf("restored forwarder identity = %+v, want the host-held anchor cert + expected SAN", w.started)
	}
	// The guest's private witness NIC is re-asserted too (idempotent over a baked address).
	var eth3 bool
	for _, c := range f.netCalls {
		if c.dev == "eth3" && c.cidr == "10.9.9.2/24" {
			eth3 = true
		}
	}
	if !eth3 {
		t.Error("the private witness link was not re-asserted on eth3")
	}
}

// The two nodes that must NOT start a hop, so the restore cannot become a thing every node does.
func TestWitnessHopSkippedWhereThereIsNone(t *testing.T) {
	t.Run("a mesh with no cloud witness", func(t *testing.T) {
		cfg := witnessCfg("anchorA") // Mesh is the zero value: never paired, so Witness is nil
		w := &fakeWitness{}
		cfg.restoreWitnessHop(context.Background(), &fakeMesher{}, w, func(string, ...any) {})
		if w.started != nil {
			t.Errorf("started a forwarder for a mesh with no cloud witness: %+v", w.started)
		}
	})
	t.Run("the diskless voter itself", func(t *testing.T) {
		cfg := witnessCfg("cloud-witness")
		cfg.Diskless = true
		cfg.Mesh = forwardedWitnessSpec("cloud-witness", false, "10.7.0.3/24")
		w := &fakeWitness{}
		cfg.restoreWitnessHop(context.Background(), &fakeMesher{}, w, func(string, ...any) {})
		if w.started != nil {
			t.Error("the diskless witness started a forwarder to itself")
		}
	})
}

// A hop that cannot start WARNS and lets bring-up continue -- deliberately the opposite of
// [V3b.16a]'s "absence of configuration is an error", because this node can still serve on 2/3 with
// its peer and failing bring-up would leave it serving nothing at all (the promoter is gated).
func TestWitnessHopFailureIsLoudNotFatal(t *testing.T) {
	cfg := witnessCfg("anchorA")
	cfg.ForwarderBin = "" // a node without the forwarder binary: bringUpWitness refuses
	cfg.Mesh = forwardedWitnessSpec("anchorA", false, "10.7.0.1/24")
	var lines []string
	cfg.restoreWitnessHop(context.Background(), &fakeMesher{}, &fakeWitness{},
		func(f string, a ...any) { lines = append(lines, fmt.Sprintf(f, a...)) })
	var warned bool
	for _, l := range lines {
		if strings.Contains(l, "WARNING") && strings.Contains(l, "witness") {
			warned = true
		}
	}
	if !warned {
		t.Errorf("a hop that could not start said nothing actionable: %v", lines)
	}
}
