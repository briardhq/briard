package reportcard

import (
	"bufio"
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"briard.io/agent/nic"
)

// The Linux eyes: Gather, and every reader beneath it that opens /proc, /sys, /etc or looks for a
// binary. The pure verdict stays in reportcard.go, the readers both OSes share in gather.go.
//
// Gather reads the real host into HostFacts (/proc + /sys + /dev), thin and best-effort: a failed
// read reads as "absent/false", which flows into a Refuse/Warn with a fix, never a crash; the
// verdict logic lives in the pure Assess. The NIC is selected once here and every NIC-derived
// fact hangs off that answer; re-reading it per fact from the default route would quietly judge a
// different device than the install uses whenever BRIARD_NIC is set.
func Gather(ctx context.Context) HostFacts {
	sel := nic.Choose(ctx, os.Getenv("BRIARD_NIC"), "")
	return HostFacts{
		NIC:       sel,
		DevKVM:    exists("/dev/kvm"),
		VirtFlags: cpuHasVirtFlags(),
		CPUAES:    cpuHasFlag("aes"),
		DevNetTun: exists("/dev/net/tun"),
		TunModule: tunModuleAvailable(),
		HasIP:     onPath("ip"),
		// /run/systemd/system is systemd's own "I am running this machine" marker (created at
		// boot), the question the install needs answered; `systemctl` on PATH says only that a
		// package is installed.
		SystemdBooted: exists("/run/systemd/system"),
		MemTotalMB:    MemTotalMB(),
		PrimaryNICBus: primaryNICBus(sel.Dev),
		DiskFreeMB:    diskFreeMB(installRoot()),
		KeptDataDisk:  exists(petDir + "/data.img"),
		KeptStateDisk: exists(petDir + "/state.img"),
		HostCIDR:      nic.HostCIDR(sel.Dev),
		// The address the install is about to hand the guest: BRIARD_VIP_ADDR (CIDR form), which
		// `briard-agent install` sets as VIP_ADDR before it runs the card; the card cannot judge an
		// address it is not told about, and this is the last gate before a VM boots holding it.
		VIPAddr: os.Getenv("VIP_ADDR"),
		// Is that address already somebody's? Probed here, not inside the check, so the verdict
		// stays pure. Only meaningful when an address was named: under DHCP the router picks from
		// its own pool and the question is not ours to ask.
		VIPAnswered:     os.Getenv("VIP_ADDR") != "" && AddressAnswers(os.Getenv("VIP_ADDR")),
		HostLeased:      hostHasLease(sel.Dev),
		HasMDNSResolver: hasMDNSResolver(),
	}
}

// AddressAnswers reports whether some machine on this segment already owns cidr's address.
// Exported for its second caller, the subnet draw, which asks it of a candidate flock subnet's .1:
// one mechanism, two questions ("is the address the user named taken", "is another flock living
// here").
//
// It provokes the kernel into resolving ARP with one datagram at the address (discard port;
// nothing need listen, since ARP sits below UDP and any IPv4 host on the segment must answer the
// resolution whatever it does with the payload), then reads the answer from the kernel's own
// neighbour table. No raw sockets, no CAP_NET_RAW, no shelling out. False is "nothing answered",
// including "we could not ask", deliberately: this gate turns evidence of use into a refusal, and
// absence of evidence proves nothing (a sleeping device will not answer, and the install proceeds).
func AddressAnswers(cidr string) bool {
	ip, _, err := net.ParseCIDR(cidr)
	if err != nil || ip.To4() == nil {
		return false // malformed: the check itself refuses on that, with a better message
	}
	if c, err := net.DialTimeout("udp4", net.JoinHostPort(ip.String(), "9"), arpProbeWait); err == nil {
		_, _ = c.Write([]byte{0})
		_ = c.Close()
	}
	deadline := time.Now().Add(arpProbeWait)
	for {
		if neighbourComplete(ip) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// neighbourComplete reports whether /proc/net/arp holds a resolved entry for ip. The flags column
// carries ATF_COM (0x2) once an address actually replied; an unanswered probe leaves the entry
// present but incomplete, flags 0x0, so the flag, not the row, is the evidence.
func neighbourComplete(ip net.IP) bool {
	f, err := os.Open("/proc/net/arp")
	if err != nil {
		return false
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		fields := strings.Fields(s.Text())
		if len(fields) < 4 || fields[0] != ip.String() {
			continue
		}
		flags, err := strconv.ParseInt(strings.TrimPrefix(fields[2], "0x"), 16, 64)
		if err == nil && flags&0x2 != 0 {
			return true
		}
	}
	return false
}

// hostHasLease reports whether this machine's address on dev looks DHCP-assigned, by looking for
// a lease under any manager a stranger's box might run: evidence that a DHCP server answered on
// this segment recently, the closest we get to "one will answer the guest too" without taking a
// lease we might not keep. Deliberately a broad net, not a detection of which manager is in use:
// we do not care who asked, only that somebody answered.
func hostHasLease(dev string) bool {
	if dev == "" {
		return false
	}
	globs := []string{
		"/var/lib/dhcpcd/" + dev + "*.lease",          // dhcpcd (and our own guest)
		"/var/lib/dhcp/dhclient*" + dev + "*.lease*",  // ISC dhclient
		"/var/lib/dhclient/*" + dev + "*.lease*",      // ISC dhclient, Fedora layout
		"/var/lib/NetworkManager/*" + dev + "*.lease", // NetworkManager's internal client
		"/run/systemd/netif/leases/*",                 // systemd-networkd (keyed by ifindex)
	}
	for _, g := range globs {
		if m, err := filepath.Glob(g); err == nil && len(m) > 0 {
			return true
		}
	}
	return false
}

// petDir is where the node's pet volumes live (agent/host/config_linux.go's stateDir).
const petDir = "/var/lib/briard"

// installRoot is the filesystem the install will land on. /opt and /var/lib can be separate
// mounts, so it measures the deepest existing ancestor of the prefix rather than assuming "/": on
// a reinstall /opt/briard exists and is the honest thing to measure, and on a fresh host the walk
// ends at / anyway. The prefix is a constant, not a knob: the qemu bundle bakes it into its own
// ELF interpreter, so an install anywhere else produces a qemu that cannot execute.
func installRoot() string {
	p := "/opt/briard"
	for p != "/" && p != "." {
		if exists(p) {
			return p
		}
		p = filepath.Dir(p)
	}
	return "/"
}

// hasMDNSResolver reports whether this machine can resolve a .local name. It asks nsswitch.conf,
// not "is avahi-daemon running", because resolution is what the user actually does and a daemon
// nothing consults resolves nothing. Two spellings count: nss-mdns (`mdns4_minimal`/`mdns`, the
// Debian/Ubuntu/Fedora desktop default) and systemd-resolved (`resolve`), which answers mDNS
// itself. An unreadable file reads as false, which warns and never refuses: absence of evidence
// about someone else's machine is not evidence of absence.
func hasMDNSResolver() bool {
	b, err := os.ReadFile("/etc/nsswitch.conf")
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(b), "\n") {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), "hosts:")
		if !ok {
			continue
		}
		for _, mod := range strings.Fields(rest) {
			if strings.HasPrefix(mod, "mdns") || mod == "resolve" {
				return true
			}
		}
	}
	return false
}

// arpProbeWait bounds the ARP probe. A device on the same segment answers in single-digit
// milliseconds; anything slower than this is a device we would rather not block an install on.
const arpProbeWait = 750 * time.Millisecond

// primaryNICBus returns the bus of the NIC the install will use ("usb", "pci", or "" if it cannot
// be determined), best-effort, for the macvtap advisories: /sys/class/net/<dev>/device points
// under .../usb... for a USB NIC, under .../pci... for a PCIe one.
func primaryNICBus(dev string) string {
	if dev == "" {
		return ""
	}
	link, err := os.Readlink("/sys/class/net/" + dev + "/device")
	if err != nil {
		return ""
	}
	switch {
	case strings.Contains(link, "usb"):
		return "usb"
	case strings.Contains(link, "pci"):
		return "pci"
	default:
		return ""
	}
}

func onPath(bin string) bool {
	_, err := exec.LookPath(bin)
	return err == nil
}

// cpuHasVirtFlags reports whether /proc/cpuinfo advertises Intel VT-x (vmx) or AMD-V (svm).
func cpuHasVirtFlags() bool {
	return cpuHasFlag("vmx") || cpuHasFlag("svm")
}

// cpuHasFlag reports whether /proc/cpuinfo advertises a CPU feature. It reads both "flags" (x86)
// and "Features" (aarch64): virtualization is x86-only in practice, but AES acceleration is
// exactly the question a Pi has to answer, and a Pi says "Features".
func cpuHasFlag(want string) bool {
	f, err := os.Open("/proc/cpuinfo")
	if err != nil {
		return false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "flags") && !strings.HasPrefix(line, "Features") {
			continue
		}
		for _, tok := range strings.Fields(line) {
			if tok == want {
				return true
			}
		}
	}
	_ = sc.Err()
	return false
}

// tunModuleAvailable reports whether the tun driver is loaded, built-in, or present as a module the
// agent can load -- so a not-yet-loaded-but-loadable tun still passes (the agent loads it).
func tunModuleAvailable() bool {
	if exists("/sys/module/tun") { // already loaded
		return true
	}
	rel := unameRelease()
	if rel == "" {
		return false
	}
	// A packaged module: /lib/modules/<rel>/kernel/drivers/net/tun.ko[.*compression]
	matches, _ := filepath.Glob("/lib/modules/" + rel + "/kernel/drivers/net/tun.ko*")
	if len(matches) > 0 {
		return true
	}
	// Built-in kernels list it in modules.builtin.
	if data, err := os.ReadFile("/lib/modules/" + rel + "/modules.builtin"); err == nil {
		if strings.Contains(string(data), "/tun.ko") {
			return true
		}
	}
	return false
}

func unameRelease() string {
	if data, err := os.ReadFile("/proc/sys/kernel/osrelease"); err == nil {
		return strings.TrimSpace(string(data))
	}
	return ""
}
