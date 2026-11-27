package host

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"briard.io/agent/nic"
	"briard.io/agent/platform"
	"briard.io/shared/api"
	"briard.io/shared/model"
)

// macvtapCfg is the shape every Linux node ships in: the default tap names and its drawn subnets.
func macvtapCfg() Config {
	return Config{
		SystemTap: "briard-drbd0", ServiceTap: "briard0", WitnessTap: "briard-priv0",
		SystemCIDR: "10.42.7.1/24", SystemHostCIDR: "10.42.7.129/32", WitnessCIDR: "10.11.203.2/24",
		PrivHostCIDR: "10.11.203.1/24",
	}
}

// The macvtap substrate: two children on the parent, a private link addressed at the host end,
// and this node's system-subnet /32 on that link rather than on the parent.
func TestNetSpecMacvtap(t *testing.T) {
	s := macvtapCfg().netSpec("eth1", false, false)
	if s.Parent != "eth1" || s.Bridge {
		t.Fatalf("spec = %+v", s)
	}
	if s.SystemTap != "briard-drbd0" || s.ServiceTap != "briard0" || s.PrivTap != "briard-priv0" {
		t.Errorf("devices = %+v, want all three", s)
	}
	want := []nic.Addr{
		{CIDR: "10.11.203.1/24", Dev: "briard-priv0"},
		{CIDR: "10.42.7.129/32", Dev: "briard-priv0"},
	}
	if !slices.Equal(s.Addrs, want) {
		t.Errorf("addrs = %+v, want %+v", s.Addrs, want)
	}
}

// A bridge parent is a different node, and the narrowing makes the config true rather than merely
// quiet: a second tap and a private link do not exist on this substrate, so naming them would
// render the guest a NIC it cannot use. One port, the guest makes its own service identity on
// top, and the host's address is on the bridge: a /32 and one on-link route to its own guest,
// never the /24 that would claim the whole system subnet on the host's primary device.
func TestNetSpecAndSubstrateOnABridge(t *testing.T) {
	s := macvtapCfg().netSpec("br0", true, false)
	if !s.Bridge || s.SystemTap != "briard-drbd0" {
		t.Fatalf("spec = %+v", s)
	}
	if s.ServiceTap != "" || s.PrivTap != "" {
		t.Errorf("a bridge parent has one port and no private link, got %+v", s)
	}
	// The prefix and the route are the assertion: a /32, and the one route that makes it reach the
	// guest -- without the route the host's /32 is on-link to nothing and the guest's node IP is
	// unreachable from the host (the reboot gate, the backup store's replies).
	want := []nic.Addr{{CIDR: "10.42.7.129/32", Dev: "br0"}}
	if !slices.Equal(s.Addrs, want) {
		t.Errorf("addrs = %+v, want %+v", s.Addrs, want)
	}
	wantR := []nic.Route{{Dst: "10.42.7.1", Dev: "br0", Src: "10.42.7.129"}}
	if !slices.Equal(s.Routes, wantR) {
		t.Errorf("routes = %+v, want %+v", s.Routes, wantR)
	}

	cfg := macvtapCfg().applySubstrate(true)
	if cfg.ServiceTap != "" || cfg.WitnessTap != "" || cfg.WitnessCIDR != "" {
		t.Errorf("the fork did not narrow: %+v", cfg)
	}
	if cfg.VIPParent != "eth1" {
		t.Errorf("VIPParent = %q, want the guest told which NIC to build its service identity on", cfg.VIPParent)
	}
	if cfg.NetMode != platform.NetBridge {
		t.Errorf("NetMode = %q, want the by-name substrate", cfg.NetMode)
	}
}

// ...and on anything that is not a bridge, nothing is narrowed and the fd-passing substrate is
// selected. The install writes no NET_MODE, so this default is the only thing standing between a
// shipped node and qemu opening taps by name, which would silently give the guest a NIC with no
// wire behind it.
func TestApplySubstrateMacvtapDefaultsTheMode(t *testing.T) {
	cfg := macvtapCfg().applySubstrate(false)
	if cfg.NetMode != platform.NetMacvtap {
		t.Errorf("NetMode = %q, want macvtap", cfg.NetMode)
	}
	if cfg.ServiceTap != "briard0" || cfg.WitnessTap != "briard-priv0" {
		t.Errorf("nothing should have been narrowed: %+v", cfg)
	}
	// A rig that set NET_MODE itself keeps it: the environment still wins where the substrate is
	// not the thing deciding. Only a non-empty value can say so, which is no gap: platform.NetBridge
	// is the empty string, so "configured as by-name" and "not configured" are the same value.
	// Unset means derive, and on a non-bridge parent that is macvtap, as pinned above.
	pinned := macvtapCfg()
	pinned.NetMode = platform.NetMacvtap
	if got := pinned.applySubstrate(false).NetMode; got != platform.NetMacvtap {
		t.Errorf("NetMode = %q, want the configured value kept", got)
	}
}

// An agent with no device names owns no network, and must say so immediately rather than wait.
// The test carries its own deadline because it is a failable assertion about an unbounded loop:
// awaitNetwork exits on a successful Converge, which a spec with no SystemTap never reaches, so
// without the guard the call never returns, hanging every agent/host test that drives Run() and
// any real node so configured.
func TestAwaitNetworkReturnsWhenThereIsNoNetworkToOwn(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan Config, 1)
	go func() {
		cfg, err := Config{}.awaitNetwork(ctx, nil, nil, func(string, ...any) {})
		if err != nil {
			t.Errorf("awaitNetwork: %v", err)
		}
		done <- cfg
	}()
	select {
	case cfg := <-done:
		// nil, not an empty spec: "this agent owns no network" has to be distinguishable from
		// "this agent owns a network it could not describe", because everything downstream --
		// recordNetwork, the re-parent tick -- branches on it.
		if cfg.net != nil {
			t.Errorf("net = %+v, want nothing recorded for an agent that owns no network", cfg.net)
		}
	case <-ctx.Done():
		t.Fatal("awaitNetwork never returned on a config that names no devices")
	}
}

// A cold start holds while the parent it last served on is missing, ahead of the lease check: at
// boot a NIC can hold a default route before the manager that makes it a bridge port has run, and
// choosing it then takes the host off its own network.
func TestColdStartHold(t *testing.T) {
	present := func(devs ...string) func(string) bool {
		return func(d string) bool { return slices.Contains(devs, d) }
	}
	settling := func(d string) func() string { return func() string { return d } }
	const missing = "vmbr2, the device this node last served on, is not here yet; waiting for it before choosing"
	const leasing = "eno1 has a link and no default route yet; waiting for it before choosing"
	for _, tc := range []struct {
		name, override, prefer string
		exists                 func(string) bool
		settling               func() string
		want                   string
	}{
		{"recorded bridge not built yet, its port holding the route", "", "vmbr2", present("eno1"), settling(""), missing},
		{"recorded parent missing outranks a NIC mid-lease", "", "vmbr2", present("eno1"), settling("eno1"), missing},
		{"recorded parent here, a NIC mid-lease", "", "vmbr2", present("vmbr2", "eno1"), settling("eno1"), leasing},
		{"recorded parent here, nothing settling", "", "vmbr2", present("vmbr2", "eno1"), settling(""), ""},
		{"no record, a NIC mid-lease", "", "", present("eno1"), settling("eno1"), leasing},
		{"no record, nothing settling", "", "", present("eno1"), settling(""), ""},
		{"BRIARD_NIC waives both", "vmbr2", "vmbr2", present("eno1"), settling("eno1"), ""},
	} {
		if got := coldStartHold(tc.override, tc.prefer, tc.exists, tc.settling); got != tc.want {
			t.Errorf("%s: coldStartHold = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// degradedClient records what a degraded node sent and hands back directives to be refused.
type degradedClient struct {
	got  []api.ReportRequest
	give []api.Directive
	err  error
}

func (c *degradedClient) Register(context.Context, api.NodeInfo) (api.Assignment, error) {
	return api.Assignment{}, nil
}
func (c *degradedClient) Report(_ context.Context, req api.ReportRequest) ([]api.Directive, error) {
	c.got = append(c.got, req)
	if c.err != nil {
		return nil, c.err
	}
	give := c.give
	c.give = nil // handed out once, like a real controller's queue
	return give, nil
}
func (c *degradedClient) ReportMetrics(context.Context, string, []api.MetricAggregate) error {
	return nil
}

// A node with no usable guest device still has a voice: it reports the identity half (who it is,
// what it runs, unhealthy) rather than going silent while degraded, the one tier where being told
// matters most, because the household cannot fix it.
func TestReportDegradedSendsIdentityAndRefusesTerminally(t *testing.T) {
	cfg := Config{Node: "n1", Role: model.RoleAnchor, Version: "v3.test"}
	cc := &degradedClient{give: []api.Directive{
		{ID: "d1", Kind: "app-install"},
		{ID: "d2", Kind: "os-upgrade"},
	}}
	dg := &degraded{rep: cc, tenant: "default"}

	cfg.reportDegraded(t.Context(), dg, discard)
	if len(cc.got) != 1 {
		t.Fatalf("reports = %d, want 1", len(cc.got))
	}
	st := cc.got[0].Status
	if st.NodeName != "n1" || st.Role != model.RoleAnchor || st.AgentVersion != "v3.test" || st.Tenant != "default" {
		t.Errorf("status = %+v, want the identity half filled", st)
	}
	// The state half stays empty, which is the honest answer when nothing could be asked -- not a
	// guess, and never Healthy.
	if st.Healthy || st.Quorum != (model.QuorumState{}) || st.System != "" {
		t.Errorf("status = %+v, want no state claimed by a node that is not serving", st)
	}

	// Every directive is refused, and refused terminally: a directive merely ignored is one the
	// controller re-delivers forever, which turns a degraded node into a retry loop against a
	// cloud that cannot help it.
	if len(dg.pending) != 2 {
		t.Fatalf("outcomes = %+v, want one per directive", dg.pending)
	}
	for _, o := range dg.pending {
		if o.State != api.OutcomeFailed || o.Detail == "" {
			t.Errorf("outcome = %+v, want a terminal failure with a reason", o)
		}
	}
	// ...and they ride the next report, so the controller actually learns of them.
	cfg.reportDegraded(t.Context(), dg, discard)
	if len(cc.got) != 2 || len(cc.got[1].Outcomes) != 2 {
		t.Fatalf("second report = %+v, want it to carry both refusals", cc.got)
	}
	if len(dg.pending) != 0 {
		t.Errorf("outcomes = %+v, want them cleared once acked", dg.pending)
	}
}

// A failed report keeps the refusals for the next attempt rather than dropping them -- on this
// path the cloud is often unreachable for the same reason the node is degraded.
func TestReportDegradedKeepsOutcomesWhenTheReportFails(t *testing.T) {
	cc := &degradedClient{err: errors.New("no route to host")}
	dg := &degraded{rep: cc, pending: []api.DirectiveOutcome{{ID: "d1", State: api.OutcomeFailed}}}
	Config{Node: "n1"}.reportDegraded(t.Context(), dg, discard)
	if len(dg.pending) != 1 {
		t.Errorf("outcomes = %+v, want them kept for the next attempt", dg.pending)
	}
}

// A standalone node has nobody to tell, and must not crash discovering that. nil rep is the
// shipped free-tier state and every rig.
func TestReportDegradedIsANoOpWithNoCloud(t *testing.T) {
	Config{Node: "n1"}.reportDegraded(t.Context(), &degraded{}, discard)
	Config{Node: "n1"}.reportDegraded(t.Context(), nil, discard)
}
