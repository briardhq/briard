package reportcard

import (
	"strings"
	"testing"
)

// capable is the baseline "green" host: everything present, plenty of RAM, wired ethernet.
func capable() HostFacts {
	return HostFacts{
		DevKVM: true, VirtFlags: true, DevNetTun: true, TunModule: true,
		HasIP: true, MemTotalMB: 16 * 1024, WiredEthernet: true, AnyEthernet: true,
	}
}

// find returns the check with the given name (fatal if absent -- every Assess must emit all gates).
func find(t *testing.T, r Report, name string) Check {
	t.Helper()
	for _, c := range r.Checks {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("report has no %q check; checks=%+v", name, r.Checks)
	return Check{}
}

// A fully capable host is admitted with every gate Pass.
func TestAssessCapableAdmits(t *testing.T) {
	r := Assess(capable())
	if !r.Admit() {
		t.Fatalf("capable host must be admitted; report=%+v", r)
	}
	for _, c := range r.Checks {
		if c.Status != Pass {
			t.Errorf("capable host: %s = %s, want pass", c.Name, c.Status)
		}
	}
}

// Each refusal path must fire with an actionable fix, and each must block admission. These are the
// load-bearing failable assertions: the card refuses the unfit WITH the fix named.
func TestAssessRefusalsCarryFixes(t *testing.T) {
	cases := []struct {
		name    string // scenario
		mutate  func(*HostFacts)
		check   string // which check should refuse
		fixHint string // substring the fix must contain (the actionable remedy)
	}{
		{"virt disabled in BIOS", func(f *HostFacts) { f.DevKVM = false; f.VirtFlags = false }, "kvm", "BIOS"},
		{"kvm module not loaded", func(f *HostFacts) { f.DevKVM = false; f.VirtFlags = true }, "kvm", "modprobe"},
		{"no tun driver", func(f *HostFacts) { f.DevNetTun = false; f.TunModule = false }, "tun", "CONFIG_TUN"},
		{"no iproute2", func(f *HostFacts) { f.HasIP = false }, "iproute2", "iproute2"},
		{"below RAM floor", func(f *HostFacts) { f.MemTotalMB = 2048 }, "memory", "4 GB"},
		{"no network at all", func(f *HostFacts) { f.WiredEthernet = false; f.AnyEthernet = false }, "network", "network"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := capable()
			tc.mutate(&f)
			r := Assess(f)
			c := find(t, r, tc.check)
			if c.Status != Refuse {
				t.Fatalf("%s: %s = %s, want refuse", tc.name, tc.check, c.Status)
			}
			if c.Fix == "" || !strings.Contains(c.Fix, tc.fixHint) {
				t.Errorf("%s: fix = %q, want it to mention %q", tc.name, c.Fix, tc.fixHint)
			}
			if r.Admit() {
				t.Errorf("%s: host must NOT be admitted", tc.name)
			}
		})
	}
}

// Warns steer honestly but still admit: WiFi-only (green wants wired) and below-recommended RAM.
func TestAssessWarnsStillAdmit(t *testing.T) {
	t.Run("wifi only", func(t *testing.T) {
		f := capable()
		f.WiredEthernet, f.AnyEthernet = false, true
		r := Assess(f)
		if c := find(t, r, "network"); c.Status != Warn || c.Fix == "" {
			t.Fatalf("wifi-only network = %+v, want warn+fix", c)
		}
		if !r.Admit() {
			t.Error("a wifi-only host warns but is still admitted (yellow tier)")
		}
	})
	t.Run("below recommended RAM", func(t *testing.T) {
		f := capable()
		f.MemTotalMB = 4096 // >= floor, < recommended
		r := Assess(f)
		if c := find(t, r, "memory"); c.Status != Warn {
			t.Fatalf("4 GB memory = %s, want warn", c.Status)
		}
		if !r.Admit() {
			t.Error("4 GB (>= floor) warns but is still admitted")
		}
	})
}

// Find over a bare check slice (the macvtap advisories aren't a full Report).
func findChecks(t *testing.T, cs []Check, name string) Check {
	t.Helper()
	for _, c := range cs {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("checks have no %q; checks=%+v", name, cs)
	return Check{}
}

// The macvtap advisories NEVER Refuse -- macvtap is never worse than the bridge substrate, so
// a caveat must not block a host the bridge path admits. On a USB NIC the promiscuous-fallback warns
// with a fix; on a PCIe NIC that check passes; the MAC/port-security advisory always warns.
func TestMacvtapAdvisories(t *testing.T) {
	t.Run("usb NIC warns on promiscuous fallback, never refuses", func(t *testing.T) {
		f := capable()
		f.PrimaryNICBus = "usb"
		cs := MacvtapAdvisories(f)
		nic := findChecks(t, cs, "macvtap-nic")
		if nic.Status != Warn || nic.Fix == "" {
			t.Errorf("usb NIC macvtap-nic = %+v, want warn+fix", nic)
		}
		for _, c := range cs {
			if c.Status == Refuse {
				t.Errorf("macvtap advisory %q must never refuse (macvtap <= bridge): %+v", c.Name, c)
			}
		}
	})
	t.Run("pci NIC passes the promiscuous check", func(t *testing.T) {
		f := capable()
		f.PrimaryNICBus = "pci"
		if c := findChecks(t, MacvtapAdvisories(f), "macvtap-nic"); c.Status != Pass {
			t.Errorf("pci NIC macvtap-nic = %s, want pass", c.Status)
		}
	})
	t.Run("the L2/port-security advisory always warns", func(t *testing.T) {
		if c := findChecks(t, MacvtapAdvisories(capable()), "macvtap-l2"); c.Status != Warn || c.Fix == "" {
			t.Errorf("macvtap-l2 = %+v, want warn+fix", c)
		}
	})
	t.Run("advisories are appended only under macvtap mode (Run wiring is by NET_MODE)", func(t *testing.T) {
		// The core Assess never emits macvtap checks; they come only from MacvtapAdvisories.
		for _, c := range Assess(capable()).Checks {
			if c.Name == "macvtap-nic" || c.Name == "macvtap-l2" {
				t.Errorf("core Assess must not emit macvtap advisory %q", c.Name)
			}
		}
	})
}
