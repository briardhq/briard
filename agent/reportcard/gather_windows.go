package reportcard

import (
	"context"
	"encoding/binary"
	"net"
	"os"
	"os/exec"
	"strings"
	"unsafe"

	"golang.org/x/sys/cpu"
	"golang.org/x/sys/windows"

	"briard.io/agent/nic"
)

// Gather reads the Windows host into HostFacts. Thin + best-effort, as on Linux: a read that fails
// reads as absent, which lands on a Refuse or a Warn with its fix.
func Gather(ctx context.Context) HostFacts {
	sel := nic.Choose(ctx, os.Getenv("BRIARD_NIC"), "")
	present := hypervisorPresent()
	return HostFacts{
		NIC:               sel,
		Hypervisor:        present,
		HypervisorPending: !present && featureEnabled(ctx, "HypervisorPlatform"),
		Elevated:          windows.GetCurrentProcessToken().IsElevated(),
		TAP:               nic.TapInstalled(),
		NICSpeedBps:       nic.Speed(sel.Dev),
		CPUAES:            cpu.X86.HasAES,
		MemTotalMB:        MemTotalMB(),
		DiskFreeMB:        diskFreeMB(installRoot()),
		KeptDataDisk:      exists(petDir + `\data.img`),
		KeptStateDisk:     exists(petDir + `\state.img`),
		HostCIDR:          HostCIDR(sel.Dev),
		VIPAddr:           os.Getenv("VIP_ADDR"),
		VIPAnswered:       os.Getenv("VIP_ADDR") != "" && AddressAnswers(os.Getenv("VIP_ADDR")),
		HostLeased:        nic.Leased(sel.Dev),
		// Windows resolves .local names itself (since Windows 10), so the briard-<name>.local the
		// install hands over resolves on this machine without anything added.
		HasMDNSResolver: true,
	}
}

// petDir is where the node's pet volumes live on Windows (config_windows.go's stateDir).
const petDir = `C:\ProgramData\briard\state`

// installRoot is the volume the install lands on.
func installRoot() string { return `C:\ProgramData` }

var procWHvGetCapability = windows.NewLazySystemDLL("WinHvPlatform.dll").NewProc("WHvGetCapability")

// hypervisorPresent asks the Windows Hypervisor Platform whether a hypervisor is running
// (WHvCapabilityCodeHypervisorPresent). The DLL is absent until the feature is installed, and the
// answer is false until the machine restarts with it -- which is the question.
func hypervisorPresent() bool {
	if procWHvGetCapability.Find() != nil {
		return false
	}
	var present, written uint32
	hr, _, _ := procWHvGetCapability.Call(0, uintptr(unsafe.Pointer(&present)), unsafe.Sizeof(present), uintptr(unsafe.Pointer(&written)))
	return hr == 0 && present != 0
}

// featureEnabled reports whether an optional Windows feature is turned on -- including turned on
// and waiting for a restart, which the feature list does not tell apart.
func featureEnabled(ctx context.Context, name string) bool {
	out, err := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-Command",
		"(Get-WindowsOptionalFeature -Online -FeatureName "+name+").State").Output()
	return err == nil && strings.TrimSpace(string(out)) == "Enabled"
}

var procSendARP = windows.NewLazySystemDLL("iphlpapi.dll").NewProc("SendARP")

// AddressAnswers reports whether something on the LAN answers ARP for cidr's address. SendARP asks
// the wire itself: for an address with no route of its own it ARPs for the address on the
// default-route adapter (measured), so no temporary route is needed the way Linux needs one.
func AddressAnswers(cidr string) bool {
	ip, _, err := net.ParseCIDR(cidr)
	if err != nil || ip.To4() == nil {
		return false
	}
	var mac [8]byte
	n := uint32(len(mac))
	r, _, _ := procSendARP.Call(uintptr(binary.LittleEndian.Uint32(ip.To4())), 0,
		uintptr(unsafe.Pointer(&mac[0])), uintptr(unsafe.Pointer(&n)))
	return r == 0 && n > 0
}
