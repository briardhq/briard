package reportcard

import (
	"bufio"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

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

// HostCIDR returns dev's own IPv4 address in CIDR form ("192.168.9.100/24") -- the LAN this node
// is on, which is what the VIP has to be inside. "" when unreadable or the NIC has no IPv4, which
// the VIP check reads as "unknown" and stays quiet about rather than refusing over.
func HostCIDR(dev string) string {
	if dev == "" {
		return ""
	}
	iface, err := net.InterfaceByName(dev)
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

// primaryNICBus returns the bus of the NIC the install will use ("usb", "pci", or "" if it cannot be
// determined) -- best-effort, for the macvtap advisories. It resolves
// /sys/class/net/<dev>/device to a bus:
// a USB NIC's device link points under .../usb..., a PCIe NIC's under .../pci....
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
	return cpuHasFlag("vmx") || cpuHasFlag("svm")
}

// cpuHasFlag reports whether /proc/cpuinfo advertises a CPU feature.
//
// It reads both "flags" (x86) and "Features" (aarch64), because the two facts it answers live on
// different lines depending on the architecture: virtualization is x86-only in practice, but AES
// acceleration is exactly the question a Pi has to answer and a Pi says "Features".
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

// DiskFreeMB is the free space on the filesystem holding path, in MB; 0 when it cannot be read.
// The host agent checks the same number before it grows the guest's state disk.
func DiskFreeMB(path string) int { return diskFreeMB(path) }

// The any-ethernet / wired-ethernet survey used to live here, and the NIC selection retired it: it asked
// whether the MACHINE had a wired device, not whether the device the install would actually use
// was one. A laptop with an unplugged eth0 and the default route on wlan0 passed it, and then
// macvtapped onto the wireless station. The question is now asked of nic.Choose's answer.
