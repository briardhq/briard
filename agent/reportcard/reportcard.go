// Package reportcard is the machine report card: the admission gate the
// free-local installer runs first. It inspects the host for the capabilities a briard node needs
// -- KVM, TUN/TAP, iproute2, RAM, wired ethernet -- and REFUSES the genuinely unfit with the fix
// named, rather than half-installing onto a box that can't serve. ("Microsoft says e-waste; we say
// server" is honest only because the card refuses what truly won't work.)
//
// Assess is PURE (host facts in, verdict out), so every refuse/warn path is unit-tested against
// fabricated facts ([[verification-assertions-must-fail]]); Gather (gather.go) is the thin impure
// reader of the real host, proven once end-to-end.
package reportcard

import (
	"fmt"
	"io"
	"net"
	"strings"
)

// Status is a single check's outcome. Refuse blocks admission; Warn admits but steers honestly.
type Status string

const (
	Pass   Status = "pass"
	Warn   Status = "warn"
	Refuse Status = "refuse"
)

// Check is one gate's verdict. Fix is the actionable remedy shown on Warn/Refuse (empty on Pass).
type Check struct {
	Name   string
	Status Status
	Detail string
	Fix    string
}

// HostFacts is what Gather reads from the host; Assess maps it to a verdict. The split keeps the
// verdict logic pure + fully testable (fabricate any host) and the reading a thin shim.
type HostFacts struct {
	DevKVM        bool // /dev/kvm present
	VirtFlags     bool // vmx (Intel) or svm (AMD) in /proc/cpuinfo
	DevNetTun     bool // /dev/net/tun present (the installer modprobes tun first)
	TunModule     bool // the tun module is loaded or available to load (or built-in)
	HasIP         bool // `ip` (iproute2) on PATH
	MemTotalMB    int
	WiredEthernet bool // a non-loopback, non-wireless interface exists (green needs wired)
	AnyEthernet   bool // any non-loopback interface at all (wired or wireless)
	// PrimaryNICBus is the bus of the default-route NIC ("usb", "pci", or "" if unknown). Used
	// only by the macvtap advisories: a USB NIC (e.g. RTL8153) usually can't program
	// the guest MACs into a hardware unicast filter, so macvtap runs it promiscuous.
	PrimaryNICBus string
	// DiskFreeMB is free space on the filesystem the install lands on. 0 means "could not read",
	// which the disk check treats as unknown (and stays quiet) rather than as an empty disk.
	DiskFreeMB int
	// HostCIDR is the default-route NIC's own IPv4 address in CIDR form ("192.168.9.100/24") --
	// i.e. the LAN this node is actually on. "" means it could not be read, which the VIP check
	// treats as unknown rather than as a fault.
	HostCIDR string
	// VIPAddr is the service address this install intends to claim, in CIDR form. "" no longer
	// means "nothing to check": since V3.19c step 3 it means DHCP, which is a different question
	// with its own answer below. It exists here because the address is the LAN's, not ours, and
	// the card is the last place to say so before a VM boots holding it.
	VIPAddr string
	// VIPAnswered is true when something on the LAN already replies for VIPAddr -- the address a
	// user named is taken. Gathered rather than probed inside the check, so the checks stay pure
	// and testable. False also covers "we could not probe", which reads as no evidence and never
	// as proof the address is free.
	VIPAnswered bool
	// HasMDNSResolver is true when this machine can resolve .local names -- nsswitch names an
	// mDNS module (nss-mdns, or systemd-resolved, which answers mDNS through `resolve`). It is
	// about the HOST's own software, not about us: the name we hand over resolves through
	// whatever resolver the household already has, and a box with none resolves no .local name
	// from any source. False also covers "could not read nsswitch", which warns rather than
	// refuses -- absence of evidence is not evidence of absence.
	HasMDNSResolver bool
	// HostLeased is true when this machine's own address looks DHCP-assigned (a lease file for
	// the default-route NIC, under any of the usual managers). EVIDENCE, not proof: a
	// deliberately static host on a DHCP-serving LAN reads false. So it may warn and must never
	// refuse.
	HostLeased bool
}

// RAM. The thresholds are deliberately BELOW the round numbers they describe, because the number
// a machine reports is not the number on the DIMM: firmware reserves a slice before Linux counts,
// so an 8 GB desktop reports ~7873 MB and a 4 GB one ~3800. Comparing against a literal 8*1024
// told every 8 GB machine it was under-provisioned and advised buying RAM it already had -- and,
// worse, the floor at 4*1024 REFUSED conforming 4 GB hosts outright. Measured on the V3.19
// stranger machine (8 GB installed -> 7873 MB reported).
const (
	memFloorMB       = 3584 // 3.5 GiB: a 4 GB host, after firmware reservation
	memRecommendedMB = 7680 // 7.5 GiB: an 8 GB host, after firmware reservation
)

// Disk. The floor is what an install physically needs on day one: the guest image (2.6 GB) + the
// qemu bundle + the THICK-allocated 4 GB data volume + the guest's own first writes -- call it
// 8 GB, below which the install cannot complete and should refuse rather than half-land. The
// recommendation is what the node needs to keep working: the guest root is a 16 GiB THIN disk that
// fills up as services and their upgrades land (a service image is ~2.7 GB, and an upgrade holds
// two), so a host with less than 25 GB free will eventually meet ENOSPC underneath a running
// guest. Warn, never refuse: plenty of nodes will never install a second service.
const (
	diskFloorMB       = 8 * 1024
	diskRecommendedMB = 25 * 1024
)

// Report is the full verdict. Admit is false if any check Refused.
type Report struct {
	Checks []Check
}

// Admit reports whether the host may host a briard node (no Refuse). Warns still admit.
func (r Report) Admit() bool {
	for _, c := range r.Checks {
		if c.Status == Refuse {
			return false
		}
	}
	return true
}

// Assess maps host facts to the closed set of admission checks. Pure -- the whole point is that
// the refuse-with-fix logic is unit-testable without touching a real host.
func Assess(f HostFacts) Report {
	var cs []Check

	// KVM -- the one thing we can't bundle (kernel side). Distinguish "virt disabled in BIOS"
	// (flags absent) from "module not loaded" (flags present, /dev/kvm absent) so the fix is exact.
	switch {
	case f.DevKVM:
		cs = append(cs, Check{"kvm", Pass, "/dev/kvm available", ""})
	case !f.VirtFlags:
		cs = append(cs, Check{"kvm", Refuse, "no CPU virtualization (vmx/svm) detected",
			"enable VT-x / AMD-V in the BIOS/UEFI (often labelled \"Virtualization\" or \"SVM Mode\"); if this CPU truly lacks it, it can't be a briard node"})
	default:
		cs = append(cs, Check{"kvm", Refuse, "CPU supports virtualization but /dev/kvm is absent",
			"load the KVM module: `sudo modprobe kvm_intel` (Intel) or `sudo modprobe kvm_amd` (AMD)"})
	}

	// TUN/TAP -- kernel side, but we modprobe it, so refuse only if the kernel genuinely lacks it.
	if f.DevNetTun || f.TunModule {
		cs = append(cs, Check{"tun", Pass, "TUN/TAP available", ""})
	} else {
		cs = append(cs, Check{"tun", Refuse, "no TUN/TAP driver (/dev/net/tun absent, tun module unavailable)",
			"this kernel lacks CONFIG_TUN; use a stock distro kernel (Debian/Ubuntu/Fedora/Arch all ship it)"})
	}

	// Iproute2 -- near-universal base; the installer can't conjure it, so refuse with the line.
	if f.HasIP {
		cs = append(cs, Check{"iproute2", Pass, "`ip` present", ""})
	} else {
		cs = append(cs, Check{"iproute2", Refuse, "`ip` (iproute2) not found on PATH",
			"install iproute2: `sudo apt install iproute2` (or your distro's equivalent)"})
	}

	// RAM -- hard floor + recommended warn.
	switch {
	case f.MemTotalMB >= memRecommendedMB:
		cs = append(cs, Check{"memory", Pass, fmt.Sprintf("%d MB RAM", f.MemTotalMB), ""})
	case f.MemTotalMB >= memFloorMB:
		// The human-facing number is the DIMM's, not the kernel's: a user reading "below the 7680
		// MB recommended" on a box they know holds 8 GB reads it as our arithmetic being wrong.
		cs = append(cs, Check{"memory", Warn, fmt.Sprintf("%d MB RAM (below the 8 GB recommended)", f.MemTotalMB),
			"8 GB is recommended; a cheap SO-DIMM upgrade pays off if this box hosts more than the basics"})
	default:
		cs = append(cs, Check{"memory", Refuse, fmt.Sprintf("%d MB RAM (below the 4 GB floor)", f.MemTotalMB),
			"add RAM to at least 4 GB (often a cheap SO-DIMM); below this a node can't run the guest reliably"})
	}

	// Disk -- the install writes a 2.6 GB guest image and thick-allocates the data volume, so an
	// out-of-space host must be refused BEFORE any of that lands, not discovered halfway through.
	// A 0 reading means the statfs failed; say nothing rather than refuse a host over a fact we
	// could not read.
	switch {
	case f.DiskFreeMB == 0:
		// unknown -- no check
	case f.DiskFreeMB >= diskRecommendedMB:
		cs = append(cs, Check{"disk", Pass, fmt.Sprintf("%d GB free", f.DiskFreeMB/1024), ""})
	case f.DiskFreeMB >= diskFloorMB:
		cs = append(cs, Check{"disk", Warn, fmt.Sprintf("%d GB free (below the %d GB recommended)", f.DiskFreeMB/1024, diskRecommendedMB/1024),
			"enough to install; the guest disk grows as services and their updates land, so free some space before adding much"})
	default:
		cs = append(cs, Check{"disk", Refuse, fmt.Sprintf("%d GB free (below the %d GB floor)", f.DiskFreeMB/1024, diskFloorMB/1024),
			fmt.Sprintf("free at least %d GB: the install writes a 2.6 GB guest image and reserves a 4 GB data volume up front", diskFloorMB/1024)})
	}

	// Ethernet -- green requires WIRED (the bridge + service IP live on L2). WiFi-only is the
	// yellow "try-me" tier, not green; no interface at all is a refuse.
	switch {
	case f.WiredEthernet:
		cs = append(cs, Check{"network", Pass, "wired ethernet present", ""})
	case f.AnyEthernet:
		cs = append(cs, Check{"network", Warn, "only wireless networking detected",
			"briard is green on WIRED ethernet (the bridge + service IP need L2); plug in ethernet, or expect the yellow \"try-me\" tier"})
	default:
		cs = append(cs, Check{"network", Refuse, "no usable network interface found",
			"connect the machine to your network (wired ethernet recommended)"})
	}

	// mDNS ON THIS MACHINE. The install ends by handing over `briard-<flock>.local`, and whether
	// that name resolves on the machine reading it is a property of the HOST, not of us: a box
	// with no mDNS resolver resolves no .local name from anywhere, ours included. The guest now
	// answers the query on the private link, so nothing is missing on our side ([V3b.19]) -- but
	// saying so before the install beats a user meeting a dead name after it.
	//
	// Never a Refuse, and not even substrate-scoped: the address always works, the name is the
	// convenience, and this warns about the household's own software.
	if f.HasMDNSResolver {
		cs = append(cs, Check{"mdns", Pass, "this machine can resolve .local names (mDNS)", ""})
	} else {
		cs = append(cs, Check{"mdns", Warn, "no mDNS resolver on this machine -- the briard-<name>.local address will not resolve HERE",
			"the numeric address always works (and other machines on your LAN resolve the name fine); install avahi-daemon + libnss-mdns if you want the name on this box too"})
	}

	cs = append(cs, vipCheck(f)...)

	return Report{Checks: cs}
}

// vipCheck is the gate that was missing when the card admitted a machine it should have refused
// (V3.19). The VIP is an address ON THE USER'S LAN, and until this check existed nothing compared
// the two: a node installed on a 192.168.9.0/24 home claimed a baked 192.168.1.100, booted, and
// reported READY -- because the readiness probe runs in-guest, against an address the guest itself
// owns. The card is the last moment that failure can be turned into a refusal with a reason, which
// is the whole promise of the card.
//
// It refuses rather than warns, because there is no partial success here: an off-LAN VIP is a node
// nobody in the house can reach, and admitting it produces exactly the green-but-useless install
// the card exists to prevent.
//
// SINCE V3.19c STEP 3 THERE IS NO DEFAULT ADDRESS, so this check has two halves rather than one:
//
//   - an address the user NAMED is judged against this LAN (below), and now also probed -- if
//     something already answers for it, we would be putting a second claimant on a live address.
//     That was left out when this check was written because there was no candidate to probe; there
//     is one now, and it is the user's, which is exactly the kind we can check without guessing.
//   - NO address means DHCP, and the question becomes whether this LAN hands them out. That
//     cannot be proven without leasing, so it WARNS on absent evidence and never refuses: a
//     deliberately-static host on a DHCP-serving network is a false negative we must not turn
//     into a refusal.
//
// The empty case used to return nil -- silence. Silence is what the original defect sounded like.
func vipCheck(f HostFacts) []Check {
	if f.VIPAddr == "" {
		return dhcpCheck(f)
	}
	vip, _, err := net.ParseCIDR(f.VIPAddr)
	if err != nil || vip.To4() == nil {
		return []Check{{"vip", Refuse, fmt.Sprintf("the service address %q is not a valid IPv4 address/prefix", f.VIPAddr),
			"set BRIARD_VIP to an address on this machine's LAN, in CIDR form (e.g. BRIARD_VIP=192.168.1.50/24)"}}
	}
	if f.HostCIDR == "" {
		// We could not read the host's own address. Say nothing rather than refuse a host over a
		// fact we failed to gather -- the same stance the disk check takes on an unreadable statfs.
		return nil
	}
	hostIP, lan, err := net.ParseCIDR(f.HostCIDR)
	if err != nil {
		return nil
	}
	switch {
	case !lan.Contains(vip):
		return []Check{{"vip", Refuse,
			fmt.Sprintf("the service address %s is not on this machine's LAN (%s)", vip, lan),
			fmt.Sprintf("this node would claim an address nobody on your network can reach. Pick a free address inside %s and re-run: `BRIARD_VIP=<address>/%s curl -fsSL https://get.briard.io/install.sh | sudo sh`",
				lan, prefixOf(f.HostCIDR))}}
	case vip.Equal(hostIP):
		return []Check{{"vip", Refuse,
			fmt.Sprintf("the service address %s is this machine's own address", vip),
			"the guest claims the VIP as a second machine on your LAN, so it cannot be the host's address; set BRIARD_VIP to a free one"}}
	case f.VIPAnswered:
		// The third refusal for this gate, alongside off-LAN and host's-own. It is the only one
		// that needs the network rather than arithmetic, which is why it is checked last:
		// something out there already owns this address, and claiming it would put two machines
		// on one address.
		return []Check{{"vip", Refuse,
			fmt.Sprintf("the service address %s is already in use on this LAN (something answered for it)", vip),
			fmt.Sprintf("pick a free address inside %s, or leave BRIARD_VIP unset and let your router assign one", lan)}}
	}
	return []Check{{"vip", Pass, fmt.Sprintf("service address %s is on this machine's LAN (%s), and nothing answered for it", vip, lan), ""}}
}

// dhcpCheck answers the question an unset BRIARD_VIP asks: can this LAN give us an address?
//
// It cannot be answered with certainty short of actually leasing one, and the install is not the
// place to take a lease we may not keep. So it reports EVIDENCE: this machine's own address being
// DHCP-assigned means a server answered on this segment recently, which is the best available
// proxy for "one will answer the guest too".
//
// Absent evidence WARNS. A host deliberately configured with a static address on a network that
// does serve DHCP reads false here, and refusing that install would be refusing a machine over a
// fact we merely failed to gather -- the same stance the disk and VIP checks already take.
func dhcpCheck(f HostFacts) []Check {
	if f.HostLeased {
		return []Check{{"vip", Pass, "no service address set, and this network hands out addresses (this machine holds a DHCP lease)", ""}}
	}
	return []Check{{"vip", Warn,
		"no service address set, and this machine's own address does not look DHCP-assigned",
		"briard will ask your router for an address when it starts. If your network has no DHCP server, set one explicitly instead: `BRIARD_VIP=<free-address>/<prefix> curl -fsSL https://get.briard.io/install.sh | sudo sh`"}}
}

// prefixOf returns the "/24" part of a CIDR string, for quoting back in a fix line.
func prefixOf(cidr string) string {
	if i := strings.LastIndex(cidr, "/"); i >= 0 {
		return cidr[i+1:]
	}
	return "24"
}

// MacvtapAdvisories returns the macvtap-substrate caveat checks, layered onto the
// core card only when the install chooses NET_MODE=macvtap. They are advisory (WARN/PASS) and
// NEVER Refuse: the evaluation established macvtap is never *worse* than the bridge substrate
// on any of these axes, so a macvtap caveat can steer but must not block a box the bridge path
// would admit. Pure -- unit-tested against fabricated facts.
func MacvtapAdvisories(f HostFacts) []Check {
	var cs []Check
	// USB-NIC unicast-filter exhaustion. A cheap USB NIC (RTL8153-class) can't hold the two
	// per-guest MACs in a hardware unicast filter, so the kernel falls the parent back to
	// promiscuous mode. Functionally harmless -- a Linux bridge does exactly this on EVERY NIC
	// unconditionally -- but a hair more host CPU, so flag it (not a refuse).
	if f.PrimaryNICBus == "usb" {
		cs = append(cs, Check{"macvtap-nic", Warn, "primary NIC is USB (e.g. RTL8153); macvtap likely runs it promiscuous (no hardware unicast filter for the guest MACs)",
			"harmless -- a bridge does the same on every NIC -- but an onboard/PCIe NIC avoids the extra host CPU; no action needed if this is your only NIC"})
	} else {
		cs = append(cs, Check{"macvtap-nic", Pass, "primary NIC supports macvtap without forcing promiscuous mode", ""})
	}
	// MAC port-security is a switch property, not host-detectable -- surface it as an always-on
	// advisory so a managed-switch LAN isn't surprised (the guest presents its own MAC, exactly
	// as it does under a bridge).
	cs = append(cs, Check{"macvtap-l2", Warn, "the guest presents its own MAC on the LAN (same as a bridge)",
		"if your switch enforces MAC port-security or you're on a hosted/cloud L2, allow the guest MAC on this port"})
	return cs
}

// Print writes a human-readable card to w (the installer console / journal).
func Print(w io.Writer, r Report) {
	mark := map[Status]string{Pass: "PASS", Warn: "WARN", Refuse: "REFUSE"}
	for _, c := range r.Checks {
		fmt.Fprintf(w, "[%-6s] %-9s %s\n", mark[c.Status], c.Name, c.Detail)
		if c.Fix != "" {
			fmt.Fprintf(w, "           -> %s\n", c.Fix)
		}
	}
	if r.Admit() {
		fmt.Fprintln(w, "\nresult: this machine can host a briard node.")
	} else {
		fmt.Fprintln(w, "\nresult: REFUSED -- fix the REFUSE items above, then re-run.")
	}
}

// Run gathers the real host's facts, assesses them, prints the card to w, and returns whether the
// host is admitted -- the one call the installer / `briard-agent --report-card` makes. When macvtap
// is set (NET_MODE=macvtap), the macvtap advisories are appended; they are WARN/PASS only, so
// they never change the admission verdict.
func Run(w io.Writer, macvtap bool) bool {
	f := Gather()
	r := Assess(f)
	if macvtap {
		r.Checks = append(r.Checks, MacvtapAdvisories(f)...)
	}
	Print(w, r)
	return r.Admit()
}
