package reportcard

import (
	"errors"
	"strings"
	"testing"

	"briard.io/agent/nic"
)

// capable is the baseline "green" host: everything present, plenty of RAM, wired ethernet.
func capable() HostFacts {
	return HostFacts{
		DevKVM: true, VirtFlags: true, DevNetTun: true, TunModule: true,
		HasIP: true, SystemdBooted: true, MemTotalMB: 16 * 1024,
		// The device the guest's L2 hangs off: wired, holding the default route, and proven able
		// to parent a macvtap (the probe ran and passed) -- the machine we expect to admit, not
		// merely one we could not fault.
		NIC:        nic.Selection{Dev: "eth0", Candidates: []string{"eth0"}},
		DiskFreeMB: 64 * 1024,
		// A capable host on a home network holds a DHCP lease, which makes the default install
		// (no BRIARD_VIP_ADDR, address from the router) pass rather than warn.
		HostLeased: true,
		// It resolves the .local name the install hands over: a desktop (Ubuntu, Fedora) ships
		// nss-mdns or systemd-resolved; a bare server may not, a warning, never a refusal.
		HasMDNSResolver: true,
		// It encrypts its data volume without paying on every write: every x86-64 CPU since ~2010
		// and every Pi 5 can.
		CPUAES: true,
	}
}

// find returns the check with the given name (fatal if absent -- the gate asked for must exist).
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

// Each refusal path must fire with an actionable fix and block admission: the card refuses the
// unfit with the fix named.
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
		{"not booted with systemd", func(f *HostFacts) { f.SystemdBooted = false }, "systemd", "systemd units"},
		{"below RAM floor", func(f *HostFacts) { f.MemTotalMB = 2048 }, "memory", "4 GB"},
		// The three ways the guest's L2 has nowhere to hang. Each names the override: a user who
		// disagrees with our selection needs the way to say so on the refusal's screen.
		{"no network at all", func(f *HostFacts) {
			f.NIC = nic.Selection{Err: nic.ErrNoInterfaces}
		}, "network", "connect this machine to your network"},
		{"no default route", func(f *HostFacts) {
			f.NIC = nic.Selection{Err: nic.ErrNoDefaultRoute, Candidates: []string{"eth0", "wlan0"}}
		}, "network", "BRIARD_NIC -- this machine has: eth0"},
		{"the default route is a full-tunnel VPN", func(f *HostFacts) {
			f.NIC = nic.Selection{Dev: "tun0", Candidates: []string{"eth0", "tun0"},
				Err: errors.New("a macvtap could not be created on it (argument \"tun0\" is wrong: Device does not support macvlan)")}
		}, "network", "BRIARD_NIC -- this machine has: eth0"},
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

// Warns steer honestly but still admit: below-recommended RAM, no local mDNS resolver.
func TestAssessWarnsStillAdmit(t *testing.T) {
	// A host with no mDNS resolver is told before the install that its name will not resolve on
	// this box, and admitted anyway: the address always works, the rest of the LAN resolves the
	// name, and refusing over the household's own software would block a good server for a
	// convenience.
	t.Run("no mdns resolver", func(t *testing.T) {
		f := capable()
		f.HasMDNSResolver = false
		r := Assess(f)
		c := find(t, r, "mdns")
		if c.Status != Warn || c.Fix == "" {
			t.Fatalf("no mDNS resolver = %+v, want warn+fix", c)
		}
		if !strings.Contains(c.Fix, "address") {
			t.Errorf("the fix %q does not point at the address, which is what still works", c.Fix)
		}
		if !r.Admit() {
			t.Error("a host without an mDNS resolver warns but is still admitted")
		}
	})
	// A wireless station is the ipvtap substrate and works fully: a Warn, with the advice, and
	// admitted.
	t.Run("wifi only", func(t *testing.T) {
		f := capable()
		f.NIC = nic.Selection{Dev: "wlan0", Wireless: true, Candidates: []string{"wlan0"}}
		r := Assess(f)
		c := find(t, r, "network")
		if c.Status != Warn || c.Fix != WifiFix {
			t.Fatalf("wireless = %+v, want warn with the advice", c)
		}
		if !r.Admit() {
			t.Error("a wireless station warns but is admitted")
		}
	})
	t.Run("below recommended disk", func(t *testing.T) {
		f := capable()
		f.DiskFreeMB = 12 * 1024 // >= what the node uses, < that + a Home Assistant + the advised free space
		r := Assess(f)
		if c := find(t, r, "disk"); c.Status != Warn || c.Fix == "" {
			t.Fatalf("12 GB free = %+v, want warn+fix", c)
		}
		if !r.Admit() {
			t.Error("12 GB free (>= floor) warns but is still admitted")
		}
	})
	// An unreadable statfs invents no verdict either way: no disk check, the host admitted on
	// its other merits. Refusing over a fact we could not read would be the worst of both.
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

// The macvtap advisories never Refuse -- macvtap is never worse than the bridge substrate, so a
// caveat must not block a host the bridge path admits. On a USB NIC the promiscuous-fallback
// warns with a fix; on a PCIe NIC it passes; the MAC/port-security advisory always warns.
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

// A node's VIP is an address on the user's LAN. Without this check the card admits a
// 192.168.9.0/24 home for an install claiming 192.168.1.100, and the node then reports ready while
// unreachable (the readiness probe runs in-guest, against an address the guest owns). The facts
// are such a machine's -- an 8 GB Ubuntu desktop at 192.168.9.100 -- and the card must refuse it.
func TestVIPOffLANIsRefused(t *testing.T) {
	f := capable()
	f.MemTotalMB = 7873 // as reported by an 8 GB box after firmware reservation
	f.HostCIDR = "192.168.9.100/24"
	f.VIPAddr = "192.168.1.100/24" // an address on another LAN
	r := Assess(f)
	if r.Admit() {
		t.Fatal("a VIP outside the host's LAN must be refused; the card admitted it")
	}
	c := find(t, r, "vip")
	if c.Status != Refuse {
		t.Errorf("vip = %s, want refuse", c.Status)
	}
	if !strings.Contains(c.Fix, "BRIARD_VIP_ADDR") || !strings.Contains(c.Fix, "192.168.9.0/24") {
		t.Errorf("the fix must name the knob AND the LAN to pick from; got %q", c.Fix)
	}
}

// The host's own address is the other way this bites: on a 192.168.1.0/24 home whose desktop is
// already .100, a VIP of .100 collides with the very machine being installed on.
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

// The third refusal, after off-LAN and host's-own: an address already somebody's -- the only one
// of the three that needs the network rather than arithmetic.
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
	if !strings.Contains(c.Fix, "BRIARD_VIP_ADDR") {
		t.Errorf("the fix must name the way out, got %q", c.Fix)
	}
}

// Unset is not silence: it means DHCP, a claim about the network the card can speak to -- a
// machine holding its own lease is evidence this LAN hands addresses out.
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

	// Absent evidence warns and must never refuse: a deliberately-static host on a DHCP-serving
	// network reads false here, and refusing it would refuse a machine over a fact we failed to
	// gather -- the stance the disk and host-address checks take.
	f.HostLeased = false
	r = Assess(f)
	if !r.Admit() {
		t.Error("absent DHCP evidence must warn, never refuse -- it is not proof of anything")
	}
	c := find(t, r, "vip")
	if c.Status != Warn {
		t.Errorf("vip = %s, want warn", c.Status)
	}
	if !strings.Contains(c.Fix, "BRIARD_VIP_ADDR") {
		t.Errorf("the fix must name the way out, got %q", c.Fix)
	}
}

// An unreadable host address must not become a refusal: the card refuses hosts for facts it has,
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
// refused for meeting the stated floor.
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

// A CPU without AES acceleration still admits -- a perfectly good node -- but the card says its
// data volume will be created unencrypted, before the install rather than discovered after.
func TestAssessAESlessWarnsButAdmits(t *testing.T) {
	f := capable()
	f.CPUAES = false
	r := Assess(f)
	if got := find(t, r, "encryption").Status; got != Warn {
		t.Errorf("AES-less host: encryption = %s, want warn", got)
	}
	for _, c := range r.Checks {
		if c.Status == Refuse {
			t.Errorf("an AES-less host must still be admitted; %q refused: %s", c.Name, c.Detail)
		}
	}
}

// The card counts what the node uses: the guest image, the data volume and the guest's disk --
// 6 GB on a fresh host, refused below. Nothing temporary is a line: an update's staging fits in
// the free space the card advises keeping (2 GB), and a Home Assistant is the 5 GB its disk grows
// by. Asserted at the edges, so a line that moves moves the card visibly.
func TestDiskCheckCountsWhatTheNodeUses(t *testing.T) {
	if got := diskUse(capable()); got != 6144 {
		t.Fatalf("a fresh install uses %d MB, want 6144 -- if a line moved, say why here", got)
	}
	for _, c := range []struct {
		mb   int
		want Status
	}{{6143, Refuse}, {6144, Warn}, {13311, Warn}, {13312, Pass}} {
		f := capable()
		f.DiskFreeMB = c.mb
		if got := find(t, Assess(f), "disk"); got.Status != c.want {
			t.Errorf("%d MB free = %+v, want %v", c.mb, got, c.want)
		}
	}
	f := capable()
	f.DiskFreeMB = 5 * 1024
	c := find(t, Assess(f), "disk")
	if !strings.Contains(c.Detail, "uses 6 GB") || !strings.Contains(c.Fix, "free at least 6 GB for the VM image, a 4 GB data volume, a 1 GB VM disk, plus about 2 GB") {
		t.Errorf("the refusal does not say what the node uses and what to keep free: %+v", c)
	}
	f.DiskFreeMB = 8 * 1024
	if c := find(t, Assess(f), "disk"); !strings.Contains(c.Fix, "Home Assistant adds about 5 GB") || !strings.Contains(c.Fix, "about 2 GB free") {
		t.Errorf("the warning does not name what an app adds and what to keep free: %+v", c)
	}
}

// A reinstall keeps its pet volumes, so it is not charged for them again: the data volume and the
// state disk are already on this host's disk. Charged a fresh host's lines, install-macvtap's
// reinstall (8.6 GB free, both kept) is refused -- for space its own disks hold.
func TestDiskCheckCreditsTheVolumesAReinstallKeeps(t *testing.T) {
	f := capable()
	f.KeptDataDisk, f.KeptStateDisk = true, true
	if got := diskUse(f); got != 1024 {
		t.Fatalf("a reinstall keeping both volumes uses %d MB, want 1024", got)
	}
	f.DiskFreeMB = 8806 // 8.6 GB
	if c := find(t, Assess(f), "disk"); c.Status == Refuse {
		t.Errorf("a reinstall with 8.6 GB free and its volumes kept was refused: %+v", c)
	}
	f.DiskFreeMB = 512
	c := find(t, Assess(f), "disk")
	if c.Status != Refuse || strings.Contains(c.Fix, "data volume") || strings.Contains(c.Fix, "VM disk") {
		t.Errorf("below what a reinstall uses = %+v; want a refusal that charges only what is not already there", c)
	}
}
