package host

import (
	"context"
	"slices"
	"testing"
	"time"

	"briard.io/agent/nic"
	"briard.io/agent/platform"
)

// macvtapCfg is what install.sh writes: the shape every Linux node ships in ([B.150](c)).
func macvtapCfg() Config {
	return Config{
		SystemTap: "briard-drbd0", ServiceTap: "briard0", WitnessTap: "briard-priv0",
		SystemHostCIDR: "10.42.7.129/32", WitnessCIDR: "10.11.203.2/24",
		PrivHostCIDR: "10.11.203.1/24",
	}
}

// The macvtap substrate: two children on the parent, a private link addressed at the host end,
// and this node's system-subnet /32 on that link rather than on the parent.
func TestNetSpecMacvtap(t *testing.T) {
	s := macvtapCfg().netSpec("eth1", false)
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

// A BRIDGE PARENT IS A DIFFERENT NODE, and the narrowing is what makes the config true rather
// than merely quiet: a second tap and a private link do not exist on this substrate, so naming
// them would render the guest a NIC it cannot use. ONE port, the guest makes its own service
// identity on top ([V3b.26c]), and the host's address is on the bridge because it is genuinely on
// that segment -- which is also why the /32 has to become a /24.
func TestNetSpecAndSubstrateOnABridge(t *testing.T) {
	s := macvtapCfg().netSpec("br0", true)
	if !s.Bridge || s.SystemTap != "briard-drbd0" {
		t.Fatalf("spec = %+v", s)
	}
	if s.ServiceTap != "" || s.PrivTap != "" {
		t.Errorf("a bridge parent has one port and no private link, got %+v", s)
	}
	// THE PREFIX IS THE ASSERTION. netSpec narrows before it reads, so the address on the bridge
	// is the /24 -- not the /32 the config was written with, which on a segment the host is
	// really on claims no on-link route and black-holes the guest's peers, silently.
	want := []nic.Addr{{CIDR: "10.42.7.129/24", Dev: "br0"}}
	if !slices.Equal(s.Addrs, want) {
		t.Errorf("addrs = %+v, want %+v", s.Addrs, want)
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
	// The prefix, which is the easiest half to forget and the one that black-holes the guest's
	// peers if it is wrong: a /32 on a segment the host is really on claims no on-link route.
	if cfg.SystemHostCIDR != "10.42.7.129/24" {
		t.Errorf("SystemHostCIDR = %q, want the on-link prefix", cfg.SystemHostCIDR)
	}
}

// ...and on anything that is not a bridge, nothing is narrowed and the fd-passing substrate is
// selected. install.sh no longer writes NET_MODE at all, so this default is the only thing
// standing between a shipped node and qemu opening taps by name, which would silently give the
// guest a NIC with no wire behind it.
func TestApplySubstrateMacvtapDefaultsTheMode(t *testing.T) {
	cfg := macvtapCfg().applySubstrate(false)
	if cfg.NetMode != platform.NetMacvtap {
		t.Errorf("NetMode = %q, want macvtap", cfg.NetMode)
	}
	if cfg.ServiceTap != "briard0" || cfg.WitnessTap != "briard-priv0" {
		t.Errorf("nothing should have been narrowed: %+v", cfg)
	}
	// A rig that set NET_MODE itself keeps it -- the environment still wins where the substrate
	// is not the thing deciding ([B.150](a)). Only a NON-EMPTY value can say so, and that is not
	// a gap: platform.NetBridge IS the empty string, so "configured as by-name" and "not
	// configured" are the same value and always were. Unset now means DERIVE, and on a
	// non-bridge parent the derivation is macvtap -- which is what the assertion above pins.
	pinned := macvtapCfg()
	pinned.NetMode = platform.NetMacvtap
	if got := pinned.applySubstrate(false).NetMode; got != platform.NetMacvtap {
		t.Errorf("NetMode = %q, want the configured value kept", got)
	}
}

// AN AGENT WITH NO DEVICE NAMES OWNS NO NETWORK, and must say so immediately rather than wait.
//
// This is a failable assertion about an UNBOUNDED LOOP, which is why it carries a deadline of its
// own: awaitNetwork's exit condition is a successful Converge, and Converge on a spec with no
// SystemTap can never succeed -- so without the guard this call never returns. It hung every
// agent/host test that drives Run(), and it would hang any real node configured that way.
func TestAwaitNetworkReturnsWhenThereIsNoNetworkToOwn(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan Config, 1)
	go func() {
		cfg, err := Config{}.awaitNetwork(ctx, nil, func(string, ...any) {})
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

// An unparseable address is passed through rather than mangled. onLink is a narrowing, not a
// validator -- the address is checked where it is used, and a silent rewrite here would be a
// worse failure than the one it was trying to prevent.
func TestOnLinkLeavesNonsenseAlone(t *testing.T) {
	for _, in := range []string{"", "not-a-cidr", "10.0.0.1"} {
		if got := onLink(in); got != in {
			t.Errorf("onLink(%q) = %q, want it untouched", in, got)
		}
	}
}
