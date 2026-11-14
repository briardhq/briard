// Package reportcard is the machine report card: the admission gate `briard-agent install` runs
// first (and `briard-agent --report-card` alone). It inspects the host for what a briard node
// needs (KVM or WHPX, TUN/TAP, iproute2, RAM, disk, a NIC that can carry the guest's L2) and
// refuses the genuinely unfit with the fix named, rather than half-installing onto a box that
// can't serve. ("Microsoft says e-waste; we say server" is honest only because the card refuses
// what truly won't work.) It also holds the installed host's own checks for `briard doctor`
// (doctor.go) and the readers the agent shares with it (disk free, RAM, clock, SMART).
//
// Assess is pure (host facts in, verdict out), so every refuse/warn path is unit-tested against
// fabricated facts; Gather (gather_<os>.go) is the thin impure reader of the real host.
package reportcard

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"

	"briard.io/agent/nic"
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
	DevKVM    bool // /dev/kvm present
	VirtFlags bool // vmx (Intel) or svm (AMD) in /proc/cpuinfo
	// CPUAES is `aes` in /proc/cpuinfo (AES-NI on x86, the ARMv8 crypto extension on aarch64). It
	// decides whether this node's data volume is created encrypted, so the card reports it before
	// the install rather than leaving the split to be discovered.
	CPUAES    bool
	DevNetTun bool // /dev/net/tun present (the agent loads tun before it builds the substrate)
	TunModule bool // the tun module is loaded or available to load (or built-in)
	HasIP     bool // `ip` (iproute2) on PATH
	// SystemdBooted is whether systemd is PID 1 here, not merely whether systemctl is installed:
	// the install registers units, so an OpenRC box with the binary lying around is still a refusal.
	SystemdBooted bool
	MemTotalMB    int
	// NIC is the device the guest's L2 will hang off, and why it is or is not usable. The card
	// judges that one device, never whether the machine has a wired one: a laptop with an unplugged
	// eth0 and the default route on wlan0 has one, and the install would still hang the guest off
	// the wireless station.
	NIC nic.Selection
	// PrimaryNICBus is the selected NIC's bus ("usb", "pci", or "" if unknown), used only by the
	// macvtap advisories: a USB NIC (e.g. RTL8153) usually can't program the guest MACs into a
	// hardware unicast filter, so macvtap runs it promiscuous.
	PrimaryNICBus string
	// DiskFreeMB is free space on the filesystem the install lands on. 0 means "could not read",
	// which the disk check treats as unknown (and stays quiet) rather than as an empty disk.
	DiskFreeMB int
	// KeptDataDisk / KeptStateDisk: the pet volumes a previous install left at their paths, which a
	// reinstall keeps. Their lines are already paid, so the floor does not charge them again: a
	// reinstall is how an alpha node moves across an upgrade floor, and it must not be refused for
	// space its own disks already hold.
	KeptDataDisk, KeptStateDisk bool
	// HostCIDR is the selected NIC's own IPv4 address in CIDR form ("192.168.9.100/24"), i.e. the
	// LAN this node is actually on. "" means unreadable, which the VIP check treats as unknown
	// rather than as a fault.
	HostCIDR string
	// VIPAddr is the service address this install intends to claim, in CIDR form. "" means DHCP, a
	// different question with its own answer (dhcpCheck), not "nothing to check". It is here
	// because the address is the LAN's, not ours, and the card is the last place to say so before a
	// VM boots holding it.
	VIPAddr string
	// VIPAnswered is true when something on the LAN already replies for VIPAddr: the address a user
	// named is taken. Gathered rather than probed inside the check, so the checks stay pure and
	// testable. False also covers "we could not probe", which reads as no evidence and never as
	// proof the address is free.
	VIPAnswered bool
	// HasMDNSResolver is true when this machine can resolve .local names: nsswitch names an mDNS
	// module (nss-mdns, or systemd-resolved, which answers mDNS through `resolve`). It is about the
	// host's own software, not us: the name we hand over resolves through whatever resolver the
	// household has, and a box with none resolves no .local name from any source. False also
	// covers "could not read nsswitch", which warns rather than refuses: absence of evidence is not
	// evidence of absence.
	HasMDNSResolver bool
	// HostLeased is true when this machine's own address looks DHCP-assigned (a lease file for the
	// selected NIC, under any of the usual managers). Evidence, not proof: a deliberately static
	// host on a DHCP-serving LAN reads false, so it may warn and must never refuse.
	HostLeased bool

	// The Windows host's requirements (platform_windows.go); unread on Linux.
	Hypervisor        bool   // the Windows hypervisor is running (WHvGetCapability), so WHPX can accelerate
	HypervisorPending bool   // the Hypervisor Platform feature is enabled but not running: a restart away
	Elevated          bool   // this process holds an administrator's full token
	TAP               bool   // a tap-windows6 adapter exists
	NICSpeedBps       uint64 // the selected NIC's link speed
}

// RAM. The thresholds sit deliberately below the round numbers they describe, because firmware
// reserves a slice before the OS counts: an 8 GB desktop reports ~7873 MB (measured), a 4 GB one
// ~3800. A literal 8*1024 would tell every 8 GB machine to buy RAM it already has, and a floor at
// 4*1024 would refuse conforming 4 GB hosts outright.
const (
	memFloorMB       = 3584 // 3.5 GiB: a 4 GB host, after firmware reservation
	memRecommendedMB = 7680 // 7.5 GiB: an 8 GB host, after firmware reservation
)

// Disk: what a node uses, each line paid for when it is made. Nothing the guest writes charges the
// host later: its OS image is read-only and its state disk thick, grown by an install before the
// pull (agent/host growStateDisk). Only what stays is counted: what an update stages beside the
// running image and frees again is not a line but what the free space every computer keeps for
// itself is for, and the card says to keep that rather than adding it in. Below what the install
// uses the card refuses, rather than half-land; below that plus one Home Assistant plus the
// advised free space, it warns and names each number: a node with no app is a working node.
const (
	// The guest image (~0.86 GB) and the agent, qemu and guest bundles (~0.1 GB), measured.
	diskImageLineMB = 1024
	// The data volume, thick, at DATA_SIZE's default (agent/host/config.go). A larger DATA_SIZE is
	// refused by its own allocation if the host cannot back it.
	diskDataLineMB = 4 * 1024
	// StateDiskInitialMB is the state disk an empty node starts with, thick: the journal (128 MB
	// cap), the boot's scratch and room to spare. The agent makes it at exactly this size
	// (agent/host/disks.go stateDiskInitial).
	StateDiskInitialMB = 1024
	// One Home Assistant: what the state disk grows by, for good (the disk never shrinks), when it
	// installs: 2.49 GB installed + 0.62 GB downloading + the 1 GiB the guest keeps free, padded as
	// the grow pads it.
	diskHomeAssistantLineMB = 5 * 1024
	// HostDiskReserveMB is the free space this computer keeps for its own system, and for an
	// update's staging, which comes and goes inside it. Not a line: the card advises it, and the
	// agent refuses any grow of the state disk that would leave less.
	HostDiskReserveMB = 2 * 1024
)

// diskUse is what installing uses on this host: every line, less the pet volumes a previous
// install left in place (a reinstall keeps them, and they are paid for). 6 GB on a fresh host.
func diskUse(f HostFacts) int {
	mb := diskImageLineMB
	if !f.KeptDataDisk {
		mb += diskDataLineMB
	}
	if !f.KeptStateDisk {
		mb += StateDiskInitialMB
	}
	return mb
}

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

// nicDetail is the one-line statement of what was selected, the "what was picked, and why" half
// of the message; Fix carries the remedy. It says why on the selected path because "eth1 holds
// this machine's default route" is the sentence that lets a user disagree with us.
func nicDetail(s nic.Selection) string {
	switch {
	case errors.Is(s.Err, nic.ErrNoInterfaces):
		return "no network interface found"
	case errors.Is(s.Err, nic.ErrNoDefaultRoute):
		return "no default route, so no device can be identified as the one on your network"
	case s.Override:
		return fmt.Sprintf("%s (named by BRIARD_NIC)", s.Dev)
	default:
		return fmt.Sprintf("%s (it holds this machine's default route)", s.Dev)
	}
}

// Assess maps host facts to the closed set of admission checks. Pure -- the whole point is that
// the refuse-with-fix logic is unit-testable without touching a real host.
func Assess(f HostFacts) Report {
	// What this OS requires of the host before anything else (platform_<os>.go).
	cs := platformChecks(f)

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
			"add RAM to at least 4 GB (often a cheap SO-DIMM); below this a machine can't run the VM reliably"})
	}

	// Disk: the install makes every line above up front (the data volume and the guest's disk
	// thick), so a host without room is refused before any of that lands, not discovered halfway
	// through. A 0 reading means the statfs failed; say nothing rather than refuse a host over a
	// fact we could not read.
	use := diskUse(f)
	comfortable := use + diskHomeAssistantLineMB + HostDiskReserveMB
	lines := "the VM image"
	if !f.KeptDataDisk {
		lines += ", a " + gbOf(diskDataLineMB) + " data volume"
	}
	if !f.KeptStateDisk {
		lines += ", a " + gbOf(StateDiskInitialMB) + " VM disk"
	}
	switch {
	case f.DiskFreeMB == 0:
		// unknown -- no check
	case f.DiskFreeMB >= comfortable:
		cs = append(cs, Check{"disk", Pass, fmt.Sprintf("%s free", gbOf(f.DiskFreeMB)), ""})
	case f.DiskFreeMB >= use:
		cs = append(cs, Check{"disk", Warn, fmt.Sprintf("%s free", gbOf(f.DiskFreeMB)),
			fmt.Sprintf("briard uses %s (%s); a Home Assistant adds about %s, and this computer should keep about %s free for its own system",
				gbOf(use), lines, gbOf(diskHomeAssistantLineMB), gbOf(HostDiskReserveMB))})
	default:
		cs = append(cs, Check{"disk", Refuse, fmt.Sprintf("%s free (briard uses %s)", gbOf(f.DiskFreeMB), gbOf(use)),
			fmt.Sprintf("free at least %s for %s, plus about %s for this computer's own system", gbOf(use), lines, gbOf(HostDiskReserveMB))})
	}

	// The NIC the guest's L2 will hang off, judged as the one device the install will use, not as a
	// survey of the machine's devices. The refusal is the whole safety margin, because every way
	// this goes wrong ends the same way: a guest that boots, reports healthy, and is unreachable
	// from the household. So the unusable cases refuse before anything is written, carrying
	// nic.Selection.Fix (what was picked, why, what failed, the override, the devices to choose
	// from). Wireless is a Warn whose fix says what the household should know: the guest rides the
	// station's own MAC (ipvtap on Linux, compatibility mode on Windows) and works fully, but Wi-Fi
	// drops, roams and sleeps in ways no substrate fixes.
	switch {
	case f.NIC.Err != nil:
		cs = append(cs, Check{"network", Refuse, nicDetail(f.NIC), f.NIC.Fix()})
	case f.NIC.Wireless:
		cs = append(cs, Check{"network", Warn, fmt.Sprintf("%s is Wi-Fi (it holds this machine's default route)", f.NIC.Dev), WifiFix})
	default:
		cs = append(cs, Check{"network", Pass, nicDetail(f.NIC), ""})
	}

	// mDNS on this machine. The install ends by handing over `briard-<flock>.local`, and whether
	// that resolves on the machine reading it is the host's property, not ours: a box with no mDNS
	// resolver resolves no .local name from anywhere, ours included. Nothing is missing on our side,
	// but saying so before the install beats a user meeting a dead name after it. Never a Refuse,
	// not even substrate-scoped: the address always works, the name is the convenience, and this
	// warns about the household's own software.
	if f.HasMDNSResolver {
		cs = append(cs, Check{"mdns", Pass, "this machine can resolve .local names (mDNS)", ""})
	} else {
		cs = append(cs, Check{"mdns", Warn, "no mDNS resolver on this machine -- the briard-<name>.local address will not resolve HERE",
			"the numeric address always works (and other machines on your LAN resolve the name fine); install avahi-daemon + libnss-mdns if you want the name on this box too"})
	}

	// Encryption at rest, decided on this machine by one hardware fact: the data volume is created
	// encrypted wherever AES acceleration exists and in the clear where it does not, since a
	// software cipher on the write path of a household's data is a worse trade than an honest
	// report. So the fleet splits on a hardware-determined, predictable axis, said out loud here
	// rather than discovered later. Never a Refuse: an AES-less box is a perfectly good node. The
	// decision is really the guest's (it checks its own /proc/cpuinfo, where silicon, accelerator
	// and CPU model compose into one answer), so this reports what that check will find (`-cpu max`
	// passes this CPU through under KVM, which the kvm check above has already established).
	if f.CPUAES {
		cs = append(cs, Check{"encryption", Pass, "AES acceleration present -- the data volume will be encrypted at rest", ""})
	} else {
		cs = append(cs, Check{"encryption", Warn, "no AES acceleration on this CPU -- the data volume will be created UNENCRYPTED",
			"nothing to fix here: this is the CPU. Briard works normally; it just cannot encrypt at rest without paying for it on every write"})
	}

	cs = append(cs, vipCheck(f)...)

	return Report{Checks: cs}
}

// vipCheck compares the VIP, an address on the user's LAN, with that LAN, which nothing else
// does: a node claiming an off-LAN address boots and reports READY (the readiness probe runs
// in-guest, against an address the guest itself owns) and nobody in the house can reach it. The
// card is the last moment to turn that into a refusal with a reason, the whole promise of the
// card. It refuses rather than warns: there is no partial success, and admitting an off-LAN VIP
// is exactly the green-but-useless install the card exists to prevent.
//
// There is no default address, so the check has two halves:
//
//   - an address the user named is judged against this LAN and probed: if something already
//     answers for it, we would be putting a second claimant on a live address.
//   - no address means DHCP, and the question is whether this LAN hands them out (dhcpCheck).
//     That cannot be proven without leasing, so it warns on absent evidence and never refuses (a
//     deliberately static host on a DHCP-serving network is a false negative we must not turn
//     into a refusal), and is never silent: an unchecked empty VIP is how the off-LAN defect hides.
//
// VIPCheck is the same gate for a running node changing its address (`briard config set vip`), so
// the install and the change refuse the same addresses for the same reasons.
func VIPCheck(hostCIDR, vip string, answered bool) []Check {
	return vipCheck(HostFacts{HostCIDR: hostCIDR, VIPAddr: vip, VIPAnswered: answered})
}

func vipCheck(f HostFacts) []Check {
	if f.VIPAddr == "" {
		return dhcpCheck(f)
	}
	vip, _, err := net.ParseCIDR(f.VIPAddr)
	if err != nil || vip.To4() == nil {
		return []Check{{"vip", Refuse, fmt.Sprintf("the service address %q is not a valid IPv4 address/prefix", f.VIPAddr),
			"set BRIARD_VIP_ADDR to an address on this machine's LAN, in CIDR form (e.g. BRIARD_VIP_ADDR=192.168.1.50/24)"}}
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
			fmt.Sprintf("briard would claim an address nobody on your network can reach. Pick a free address inside %s and re-run: `BRIARD_VIP_ADDR=<address>/%s curl -fsSL https://get.briard.io/install.sh | sudo sh`",
				lan, prefixOf(f.HostCIDR))}}
	case vip.Equal(hostIP):
		return []Check{{"vip", Refuse,
			fmt.Sprintf("the service address %s is this machine's own address", vip),
			"the VM claims the VIP as a second machine on your LAN, so it cannot be the host's address; set BRIARD_VIP_ADDR to a free one"}}
	case f.VIPAnswered:
		// The third refusal for this gate, alongside off-LAN and host's-own. It is the only one
		// that needs the network rather than arithmetic, which is why it is checked last:
		// something out there already owns this address, and claiming it would put two machines
		// on one address.
		return []Check{{"vip", Refuse,
			fmt.Sprintf("the service address %s is already in use on this LAN (something answered for it)", vip),
			fmt.Sprintf("pick a free address inside %s, or leave BRIARD_VIP_ADDR unset and let your router assign one", lan)}}
	}
	return []Check{{"vip", Pass, fmt.Sprintf("service address %s is on this machine's LAN (%s), and nothing answered for it", vip, lan), ""}}
}

// dhcpCheck answers the question an unset BRIARD_VIP_ADDR asks: can this LAN give us an address?
// Only a lease proves it, and the install is not the place to take one we may not keep, so it
// reports evidence: this machine's own address being DHCP-assigned means a server answered on
// this segment recently, the best available proxy for "one will answer the guest too". Absent
// evidence warns: a deliberately static host on a network that does serve DHCP reads false, and
// refusing it would refuse a machine over a fact we merely failed to gather, the stance the disk
// and VIP checks already take.
func dhcpCheck(f HostFacts) []Check {
	if f.HostLeased {
		return []Check{{"vip", Pass, "no service address set, and this network hands out addresses (this machine holds a DHCP lease)", ""}}
	}
	return []Check{{"vip", Warn,
		"no service address set, and this machine's own address does not look DHCP-assigned",
		"briard will ask your router for an address when it starts. If your network has no DHCP server, set one explicitly instead: `BRIARD_VIP_ADDR=<free-address>/<prefix> curl -fsSL https://get.briard.io/install.sh | sudo sh`"}}
}

// prefixOf returns the "/24" part of a CIDR string, for quoting back in a fix line.
func prefixOf(cidr string) string {
	if i := strings.LastIndex(cidr, "/"); i >= 0 {
		return cidr[i+1:]
	}
	return "24"
}

// MacvtapAdvisories returns the macvtap-substrate caveats, layered onto the core card only when
// the selected device is one the guest will be macvtapped onto, a question about the device (a
// bridge gets a port instead, a wireless station ipvtap; platformAdvisories). They are advisory
// (WARN/PASS) and never Refuse: macvtap is never worse than the bridge substrate on any of these
// axes, so a caveat can steer but must not block a box the bridge path would admit. Pure, and
// unit-tested against fabricated facts.
func MacvtapAdvisories(f HostFacts) []Check {
	var cs []Check
	// USB-NIC unicast-filter exhaustion: a cheap USB NIC (RTL8153-class) can't hold the two
	// per-guest MACs in a hardware unicast filter, so the kernel falls the parent back to
	// promiscuous mode. Harmless (a Linux bridge does exactly this on every NIC, unconditionally)
	// but a hair more host CPU, so flag it, never refuse.
	if f.PrimaryNICBus == "usb" {
		cs = append(cs, Check{"macvtap-nic", Warn, "primary NIC is USB (e.g. RTL8153); macvtap likely runs it promiscuous (no hardware unicast filter for the VM's MACs)",
			"harmless -- a bridge does the same on every NIC -- but an onboard/PCIe NIC avoids the extra host CPU; no action needed if this is your only NIC"})
	} else {
		cs = append(cs, Check{"macvtap-nic", Pass, "primary NIC supports macvtap without forcing promiscuous mode", ""})
	}
	// MAC port-security is a switch property, not host-detectable: an always-on advisory so a
	// managed-switch LAN isn't surprised (the guest presents its own MAC, as under a bridge).
	cs = append(cs, Check{"macvtap-l2", Warn, "the VM presents its own MAC on the LAN (same as a bridge)",
		"if your switch enforces MAC port-security or you're on a hosted/cloud L2, allow the VM's MAC on this port"})
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
		fmt.Fprintln(w, "\nresult: this machine can run briard.")
	} else {
		fmt.Fprintln(w, "\nresult: REFUSED -- fix the REFUSE items above, then re-run.")
	}
}

// Run gathers the real host's facts, assesses them, prints the card to w, and returns whether the
// host is admitted: the one call `briard-agent install` / `briard-agent --report-card` makes. It
// takes no substrate, since being told one would let the card judge a substrate the machine will
// not end up on: the substrate answers "is the selected device a bridge", and from the selection
// the card derives the same answer the agent will.
func Run(ctx context.Context, w io.Writer) bool {
	f := Gather(ctx)
	r := Assess(f)
	r.Checks = append(r.Checks, platformAdvisories(f)...)
	Print(w, r)
	return r.Admit()
}

// gbOf renders MB as the GB a person reads on the card: one decimal, dropped when whole.
func gbOf(mb int) string {
	s := strconv.FormatFloat(float64(mb)/1024, 'f', 1, 64)
	return strings.TrimSuffix(s, ".0") + " GB"
}

// FastNICBps is the link speed above which a Windows bridge's disabled checksum offload costs
// throughput a household could notice: gigabit is the audience that unconditional disable was
// decided for. One number for the card's advisory and the host's alert.
const FastNICBps = 1_000_000_000

// WifiFix is what a node on Wi-Fi is told, here and by `briard doctor`: one sentence, the same on
// both hosts.
const WifiFix = "Wi-Fi can be unstable; we recommend a wired connection"
