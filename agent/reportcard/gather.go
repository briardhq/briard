package reportcard

import (
	"bufio"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Gather reads the real host into HostFacts (Linux /proc + /sys + /dev). Thin + best-effort: a
// read that fails reads as "absent/false", which flows into a Refuse/Warn with a fix -- never a
// crash. All the verdict logic lives in the pure Assess (this is just the eyes).
func Gather() HostFacts {
	return HostFacts{
		DevKVM:    exists("/dev/kvm"),
		VirtFlags: cpuHasVirtFlags(),
		DevNetTun: exists("/dev/net/tun"),
		TunModule: tunModuleAvailable(),
		HasIP:     onPath("ip"),
		// /run/systemd/system is systemd's own "I am running this machine" marker (it creates the
		// directory at boot), which is the question the install actually needs answered -- `systemctl`
		// on PATH says only that a package is installed.
		SystemdBooted: exists("/run/systemd/system"),
		MemTotalMB:    memTotalMB(),
		WiredEthernet: hasWiredEthernet(),
		AnyEthernet:   hasAnyEthernet(),
		PrimaryNICBus: primaryNICBus(),
		DiskFreeMB:    diskFreeMB(installRoot()),
		HostCIDR:      hostCIDR(DefaultRouteNIC()),
		// The address the install is about to hand the guest. install.sh already computes it
		// (BRIARD_VIP, CIDR form) and passes it here the same way it passes NET_MODE -- the card
		// cannot judge an address it is not told about, and this is the last gate before a VM
		// boots holding it.
		VIPAddr: os.Getenv("VIP_ADDR"),
		// Is that address already somebody's? Probed here rather than inside the check so the
		// verdict logic stays pure. Only meaningful when an address was named -- under DHCP the
		// router picks from its own pool and this question is not ours to ask.
		VIPAnswered:     os.Getenv("VIP_ADDR") != "" && AddressAnswers(os.Getenv("VIP_ADDR")),
		HostLeased:      hostHasLease(DefaultRouteNIC()),
		HasMDNSResolver: hasMDNSResolver(),
	}
}

// hasMDNSResolver reports whether this machine can resolve a .local name.
//
// nsswitch.conf is the question rather than "is avahi-daemon running", because RESOLUTION is what
// the user actually does and a daemon nothing consults resolves nothing. Two spellings count:
// nss-mdns (`mdns4_minimal`/`mdns`, the Debian/Ubuntu/Fedora desktop default) and systemd-resolved
// (`resolve`), which answers mDNS itself. An unreadable file reads as false, which warns and never
// refuses -- absence of evidence about someone else's machine is not evidence of absence.
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

// AddressAnswers reports whether some machine on this segment already owns cidr's address.
//
// Exported for its second caller: the subnet draw asks the same question of a candidate flock
// subnet's .1 ([V3b.26f]). One mechanism, two questions -- the VIP's is "is the address the user
// named already taken", the draw's is "is another flock already living here".
//
// It provokes the kernel into resolving ARP by sending one datagram at the address (discard port,
// nothing listens, and nothing needs to -- ARP sits below UDP, so any IPv4 host on the segment
// must reply to the resolution regardless of what it does with the payload). Then it reads the
// answer out of the kernel's own neighbour table. No raw sockets, no CAP_NET_RAW, no shelling out.
//
// A false is "nothing answered", which includes "we could not ask". That asymmetry is deliberate:
// this gate turns evidence-of-use into a refusal, and must never turn absence-of-evidence into
// permission -- a sleeping device will not answer, and the install proceeds, as it did before this
// check existed.
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

// neighbourComplete reports whether /proc/net/arp holds a RESOLVED entry for ip. The flags column
// carries ATF_COM (0x2) once an address actually replied; an unanswered probe leaves the entry
// present but incomplete, with flags 0x0 -- so the flag, not the row, is the evidence.
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

// hostHasLease reports whether THIS machine's address on nic looks DHCP-assigned, by looking for a
// lease under any of the managers a stranger's box might be running. It is evidence that a DHCP
// server answered on this segment recently, which is the closest we can get to "one will answer
// the guest too" without taking a lease we might not keep.
//
// Deliberately a broad net over several managers rather than a detection of which one is in use:
// we do not care who asked, only that somebody answered.
func hostHasLease(nic string) bool {
	if nic == "" {
		return false
	}
	globs := []string{
		"/var/lib/dhcpcd/" + nic + "*.lease",          // dhcpcd (and our own guest)
		"/var/lib/dhcp/dhclient*" + nic + "*.lease*",  // ISC dhclient
		"/var/lib/dhclient/*" + nic + "*.lease*",      // ISC dhclient, Fedora layout
		"/var/lib/NetworkManager/*" + nic + "*.lease", // NetworkManager's internal client
		"/run/systemd/netif/leases/*",                 // systemd-networkd (keyed by ifindex)
	}
	for _, g := range globs {
		if m, err := filepath.Glob(g); err == nil && len(m) > 0 {
			return true
		}
	}
	return false
}

// hostCIDR returns nic's own IPv4 address in CIDR form ("192.168.9.100/24") -- the LAN this node
// is on, which is what the VIP has to be inside. "" when unreadable or the NIC has no IPv4, which
// the VIP check reads as "unknown" and stays quiet about rather than refusing over.
func hostCIDR(nic string) string {
	if nic == "" {
		return ""
	}
	iface, err := net.InterfaceByName(nic)
	if err != nil {
		return ""
	}
	addrs, err := iface.Addrs()
	if err != nil {
		return ""
	}
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil {
			return n.String()
		}
	}
	return ""
}

// installRoot is the filesystem the install will land on. /opt and /var/lib can be separate
// mounts, so measure the deepest existing ancestor of the prefix rather than assuming "/": on a
// reinstall /opt/briard already exists and is the honest thing to measure, and on a fresh host the
// walk ends at / anyway. BRIARD_PREFIX is honoured so the card measures where install.sh writes.
func installRoot() string {
	p := os.Getenv("BRIARD_PREFIX")
	if p == "" {
		p = "/opt/briard"
	}
	for p != "/" && p != "." {
		if exists(p) {
			return p
		}
		p = filepath.Dir(p)
	}
	return "/"
}

// primaryNICBus returns the bus of the default-route NIC ("usb", "pci", or "" if it can't be
// determined) -- best-effort, for the macvtap advisories. It reads the default route from
// /proc/net/route (no iproute2 dependency), then resolves /sys/class/net/<dev>/device to a bus:
// a USB NIC's device link points under .../usb..., a PCIe NIC's under .../pci....
func primaryNICBus() string {
	dev := DefaultRouteNIC()
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

// DefaultRouteNIC reads the interface owning the default route (destination 00000000) from
// /proc/net/route -- the NIC macvtap would parent onto. "" if none.
func DefaultRouteNIC() string {
	f, err := os.Open("/proc/net/route")
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Scan() // header
	for sc.Scan() {
		fields := strings.Fields(sc.Text()) // Iface Destination Gateway Flags ...
		if len(fields) >= 2 && fields[1] == "00000000" {
			return fields[0]
		}
	}
	_ = sc.Err()
	return ""
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func onPath(bin string) bool {
	_, err := exec.LookPath(bin)
	return err == nil
}

// cpuHasVirtFlags reports whether /proc/cpuinfo advertises Intel VT-x (vmx) or AMD-V (svm).
func cpuHasVirtFlags() bool {
	f, err := os.Open("/proc/cpuinfo")
	if err != nil {
		return false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "flags") {
			continue
		}
		for _, tok := range strings.Fields(line) {
			if tok == "vmx" || tok == "svm" {
				return true
			}
		}
	}
	_ = sc.Err()
	return false
}

// tunModuleAvailable reports whether the tun driver is loaded, built-in, or present as a module the
// installer can load -- so a not-yet-loaded-but-loadable tun still passes (the installer modprobes).
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

func memTotalMB() int {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "MemTotal:") {
			continue
		}
		fields := strings.Fields(line) // MemTotal:  16384000 kB
		if len(fields) >= 2 {
			if kb, err := strconv.Atoi(fields[1]); err == nil {
				return kb / 1024
			}
		}
	}
	_ = sc.Err()
	return 0
}

// netInterfaces walks /sys/class/net, returning whether any non-loopback interface exists and
// whether any of those is wired (not wireless). A wireless iface has a `wireless` dir or `phy80211`
// symlink; virtual/loopback are skipped.
func netInterfaces() (anyEth, wiredEth bool) {
	entries, err := os.ReadDir("/sys/class/net")
	if err != nil {
		return false, false
	}
	for _, e := range entries {
		name := e.Name()
		if name == "lo" {
			continue
		}
		base := "/sys/class/net/" + name
		anyEth = true
		wireless := exists(base+"/wireless") || exists(base+"/phy80211")
		if !wireless {
			wiredEth = true
		}
	}
	return anyEth, wiredEth
}

func hasAnyEthernet() bool   { a, _ := netInterfaces(); return a }
func hasWiredEthernet() bool { _, w := netInterfaces(); return w }
