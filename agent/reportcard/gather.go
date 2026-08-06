package reportcard

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// Gather reads the real host into HostFacts (Linux /proc + /sys + /dev). Thin + best-effort: a
// read that fails reads as "absent/false", which flows into a Refuse/Warn with a fix -- never a
// crash. All the verdict logic lives in the pure Assess (this is just the eyes).
func Gather() HostFacts {
	return HostFacts{
		DevKVM:        exists("/dev/kvm"),
		VirtFlags:     cpuHasVirtFlags(),
		DevNetTun:     exists("/dev/net/tun"),
		TunModule:     tunModuleAvailable(),
		HasIP:         onPath("ip"),
		MemTotalMB:    memTotalMB(),
		WiredEthernet: hasWiredEthernet(),
		AnyEthernet:   hasAnyEthernet(),
		PrimaryNICBus: primaryNICBus(),
		DiskFreeMB:    diskFreeMB(installRoot()),
	}
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

// diskFreeMB returns free space on the filesystem holding path, in MB (0 if unreadable --
// best-effort, like every other reader here). Bfree, not Bavail: the installer runs as root, so
// the root-reserved blocks really are available to it and Bavail would under-report by ~5%.
func diskFreeMB(path string) int {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0
	}
	return int(uint64(st.Bfree) * uint64(st.Bsize) >> 20)
}

// primaryNICBus returns the bus of the default-route NIC ("usb", "pci", or "" if it can't be
// determined) -- best-effort, for the macvtap advisories. It reads the default route from
// /proc/net/route (no iproute2 dependency), then resolves /sys/class/net/<dev>/device to a bus:
// a USB NIC's device link points under .../usb..., a PCIe NIC's under .../pci....
func primaryNICBus() string {
	dev := defaultRouteNIC()
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

// defaultRouteNIC reads the interface owning the default route (destination 00000000) from
// /proc/net/route -- the NIC macvtap would parent onto. "" if none.
func defaultRouteNIC() string {
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
