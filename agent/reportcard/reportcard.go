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
}

// 4 GB is the dedicated-node floor; 8 GB recommended.
const (
	memFloorMB       = 4 * 1024
	memRecommendedMB = 8 * 1024
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
		cs = append(cs, Check{"memory", Warn, fmt.Sprintf("%d MB RAM (below the %d MB recommended)", f.MemTotalMB, memRecommendedMB),
			"8 GB is recommended; a cheap SO-DIMM upgrade pays off if this box hosts more than the basics"})
	default:
		cs = append(cs, Check{"memory", Refuse, fmt.Sprintf("%d MB RAM (below the %d MB floor)", f.MemTotalMB, memFloorMB),
			"add RAM to at least 4 GB (often a cheap SO-DIMM); below this a node can't run the guest reliably"})
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

	return Report{Checks: cs}
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
