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
		DiskFreeMB: 64 * 1024,
		// A capable host on an ordinary home network holds a DHCP lease, and since V3.19c step 3
		// that is what makes the default install -- no BRIARD_VIP, address from the router --
		// pass rather than warn. The fixture describes the machine we expect to admit.
		HostLeased: true,
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
		{"below disk floor", func(f *HostFacts) { f.DiskFreeMB = 5 * 1024 }, "disk", "4 GB data volume"},
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
	t.Run("below recommended disk", func(t *testing.T) {
		f := capable()
		f.DiskFreeMB = 12 * 1024 // >= floor, < recommended
		r := Assess(f)
		if c := find(t, r, "disk"); c.Status != Warn || c.Fix == "" {
			t.Fatalf("12 GB free = %+v, want warn+fix", c)
		}
		if !r.Admit() {
			t.Error("12 GB free (>= floor) warns but is still admitted")
		}
	})
	// An unreadable statfs must not invent a verdict in either direction: no disk check at all,
	// and the host still admitted on its other merits. Refusing over a fact we could not read
	// would be the worst of both.
	t.Run("disk unreadable emits no check", func(t *testing.T) {
		f := capable()
		f.DiskFreeMB = 0
		r := Assess(f)
		for _, c := range r.Checks {
			if c.Name == "disk" {
				t.Fatalf("unknown free space produced a %s verdict: %+v", c.Status, c)
			}
		}
		if !r.Admit() {
			t.Error("a host with unreadable free space is still admitted")
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

// THE V3.19 REGRESSION. A node's VIP is an address on the USER'S LAN, and nothing compared the two
// until this check existed: the card admitted a 192.168.9.0/24 home for an install that would claim
// a baked 192.168.1.100, and the node then reported READY while unreachable (the readiness probe
// runs in-guest, against an address the guest itself owns). These are the exact facts of the machine
// that found it -- an 8 GB Ubuntu desktop at 192.168.9.100 -- and the card must now REFUSE it.
func TestVIPOffLANIsRefused(t *testing.T) {
	f := capable()
	f.MemTotalMB = 7873 // as reported by an 8 GB box after firmware reservation
	f.HostCIDR = "192.168.9.100/24"
	f.VIPAddr = "192.168.1.100/24" // the old baked default
	r := Assess(f)
	if r.Admit() {
		t.Fatal("a VIP outside the host's LAN must be refused; the card admitted it")
	}
	c := find(t, r, "vip")
	if c.Status != Refuse {
		t.Errorf("vip = %s, want refuse", c.Status)
	}
	if !strings.Contains(c.Fix, "BRIARD_VIP") || !strings.Contains(c.Fix, "192.168.9.0/24") {
		t.Errorf("the fix must name the knob AND the LAN to pick from; got %q", c.Fix)
	}
}

// The host's own address is the other way this bites: on a 192.168.1.0/24 home whose desktop is
// already .100, the baked VIP collided with the very machine being installed on.
func TestVIPEqualToHostIsRefused(t *testing.T) {
	f := capable()
	f.HostCIDR = "192.168.1.100/24"
	f.VIPAddr = "192.168.1.100/24"
	r := Assess(f)
	if r.Admit() {
		t.Fatal("a VIP equal to the host's own address must be refused")
	}
	if c := find(t, r, "vip"); c.Status != Refuse {
		t.Errorf("vip = %s, want refuse", c.Status)
	}
}

// A VIP inside the LAN, that nothing already answers for, passes.
func TestVIPOnLANPasses(t *testing.T) {
	f := capable()
	f.HostCIDR = "192.168.9.100/24"
	f.VIPAddr = "192.168.9.50/24"
	r := Assess(f)
	if !r.Admit() {
		t.Fatalf("an on-LAN VIP must be admitted; report=%+v", r)
	}
	if c := find(t, r, "vip"); c.Status != Pass {
		t.Errorf("vip = %s, want pass", c.Status)
	}
}

// The THIRD refusal for this gate, after off-LAN and host's-own: an address that is already
// somebody's. It is the only one of the three that needs the network rather than arithmetic, and
// the only one that could not have existed before step 3 -- until the address became the user's
// choice, there was no candidate to probe that we had not invented ourselves.
func TestVIPAlreadyInUseIsRefused(t *testing.T) {
	f := capable()
	f.HostCIDR = "192.168.9.100/24"
	f.VIPAddr = "192.168.9.50/24"
	f.VIPAnswered = true
	r := Assess(f)
	if r.Admit() {
		t.Fatal("an address something already answers for must be refused")
	}
	c := find(t, r, "vip")
	if c.Status != Refuse {
		t.Errorf("vip = %s, want refuse", c.Status)
	}
	if !strings.Contains(c.Fix, "BRIARD_VIP") {
		t.Errorf("the fix must name the way out, got %q", c.Fix)
	}
}

// UNSET IS NO LONGER SILENCE. Since step 3 it means DHCP, which is a claim about the network that
// the card can actually speak to: a machine holding its own lease is evidence this LAN hands
// addresses out. Silence is what the original defect sounded like.
func TestUnsetVIPReportsWhetherTheLANCanGiveUsOne(t *testing.T) {
	f := capable()
	f.HostCIDR = "192.168.9.100/24"
	f.VIPAddr = ""

	f.HostLeased = true
	r := Assess(f)
	if !r.Admit() {
		t.Fatalf("a DHCP install on a leasing LAN must be admitted; report=%+v", r)
	}
	if c := find(t, r, "vip"); c.Status != Pass {
		t.Errorf("vip = %s, want pass when this machine holds a lease", c.Status)
	}

	// Absent evidence WARNS and must never refuse: a deliberately-static host on a DHCP-serving
	// network reads false here, and refusing it would be refusing a machine over a fact we merely
	// failed to gather -- the stance the disk and host-address checks already take.
	f.HostLeased = false
	r = Assess(f)
	if !r.Admit() {
		t.Error("absent DHCP evidence must warn, never refuse -- it is not proof of anything")
	}
	c := find(t, r, "vip")
	if c.Status != Warn {
		t.Errorf("vip = %s, want warn", c.Status)
	}
	if !strings.Contains(c.Fix, "BRIARD_VIP") {
		t.Errorf("the fix must name the way out, got %q", c.Fix)
	}
}

// An unreadable host address must not become a refusal: the card refuses hosts for facts it HAS,
// never for facts it failed to gather (the same stance the disk check takes on a failed statfs).
func TestVIPUnknownHostCIDRIsSilent(t *testing.T) {
	f := capable()
	f.HostCIDR = ""
	f.VIPAddr = "192.168.1.100/24"
	r := Assess(f)
	if !r.Admit() {
		t.Fatal("an unreadable host address must not refuse the machine")
	}
	for _, c := range r.Checks {
		if c.Name == "vip" {
			t.Errorf("unknown LAN must emit no vip verdict, got %+v", c)
		}
	}
}

// The RAM thresholds describe DIMMs, but the kernel reports what firmware left it. A stock 8 GB
// desktop (7873 MB) must not be told to buy RAM it already has, and a stock 4 GB box must not be
// refused for meeting the stated floor. Both were wrong until V3.19.
func TestMemoryThresholdsAllowForFirmwareReservation(t *testing.T) {
	f := capable()
	f.MemTotalMB = 7873 // a real 8 GB machine
	if c := find(t, Assess(f), "memory"); c.Status != Pass {
		t.Errorf("8 GB host (%d MB) = %s, want pass: %s", f.MemTotalMB, c.Status, c.Detail)
	}
	f.MemTotalMB = 3800 // a real 4 GB machine
	c := find(t, Assess(f), "memory")
	if c.Status != Warn {
		t.Errorf("4 GB host (%d MB) = %s, want warn (it meets the floor)", f.MemTotalMB, c.Status)
	}
	if strings.Contains(c.Detail, "7680") || strings.Contains(c.Detail, "3584") {
		t.Errorf("the detail must quote the DIMM's number, not the kernel's threshold; got %q", c.Detail)
	}
}
