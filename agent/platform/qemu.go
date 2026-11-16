package platform

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"briard.io/agent/guestfirmware"
	"briard.io/agent/nic"
	"briard.io/shared/dashboard"
)

// QEMUSpec describes the guest VM to boot; the agent runs qemu-system directly. Accel is a QEMU
// accel list -- "kvm:tcg" on Linux, "whpx:tcg" on Windows: the hypervisor where available, TCG
// as the fallback on a host without one.
type QEMUSpec struct {
	Binary string // qemu-system-x86_64
	Accel  string // e.g. "kvm:tcg"
	// CPUModel is qemu's `-cpu`. Empty = qemu's default, which on x86_64 is `qemu64`: a
	// 1990s-baseline model below x86-64-v2 (60 CPUID features against the host's 190, measured on
	// the shipped qemu). It has no aes, sha-ni, pclmulqdq, ssse3, sse4.1/4.2, avx*, bmi1/2,
	// popcnt, rdrand, rdtscp, xsave, invpcid, pdpe1gb, tsc-deadline or pmu, so guest TLS, nix's
	// sha256, OCI digest checks and btrfs/DRBD crc32c all run the software path, and the guest
	// kernel cannot apply Spectre/SSBD mitigations at all because the spec-ctrl/ibpb/stibp bits are
	// not advertised. Passing the host's real CPU through costs nothing here: the usual reason not
	// to is live migration or a RAM-bearing savevm, and this product does neither (a guest never
	// moves hosts and is never snapshotted; an HA failover is a fresh boot on the other node, which
	// re-reads CPUID).
	//
	// `max`, not `host`: under KVM the two expand to an identical feature set (verified by
	// query-cpu-model-expansion), but `host` hard-fails without KVM ("CPU model 'host' requires
	// KVM") and Accel is a fallback list (kvm:tcg). A bare `host` would turn every no-virt host
	// from slow-but-booting into not-booting, exactly the case the tcg fallback exists for.
	CPUModel string
	MemoryMB int
	// MaxMemoryMB is how far the running VM may grow by AddMemory. Above MemoryMB, the launch
	// reserves the address space and the DIMM slots for it -- address space only: QEMU backs a
	// DIMM with memory when it is added, never before. At or below MemoryMB the VM cannot grow.
	MaxMemoryMB int
	Cores       int
	// DiskImage is the guest OS image, attached read-only: the guest writes nothing to it, and a
	// write it tried would fail inside the guest rather than land in a file the host never sized.
	// What an OS update swaps. Empty = no OS disk.
	DiskImage string
	DataDisk  string // backing block device for the DRBD volume -> guest /dev/vdb
	// StateDisk is the node-local state disk: every byte the guest writes, both what a restart must
	// not cost (podman's storage, the journal, the deadman's backoff) and the per-boot scratch that
	// /var and /tmp live on. Attached with a fixed serial so the guest finds it by id whatever the
	// bus order. Empty = none, and the shipped image then fails its boot: it has nowhere else to
	// write.
	StateDisk string
	// MachineUUID is the VM's DMI product UUID, from which systemd derives the guest's
	// machine-id when the OS disk carries none (a disposable OS must still be the same
	// machine each boot -- journal continuity, DHCP identity). Derived from the node name, so it
	// needs no disk. Empty = qemu's random one.
	MachineUUID string
	ControlSock string // host end of the virtio-serial control channel
	// AdminPortSock is the host end of the guest's admin port: the second serial
	// port, direction reversed -- the guest's dashboard writes a directive, the host answers.
	// Empty = no port (what every test that never runs a dashboard passes).
	AdminPortSock string
	// QMPSock is the host end of QEMU's own control channel: the VM, not the guest OS inside it.
	// Empty = no monitor, what every path that never needs to stop or reset a VM should pass. It
	// buys a clean ACPI shutdown (Guest.Shutdown) where the only alternative is killing QEMU (a
	// power cut), and a forced reset for a wedged guest. Launch creates its parent directory 0700
	// root: QMP is unrestricted control of the VM, so the containment is the filesystem (qmp.go).
	QMPSock    string
	ServiceTap string // host tap for the guest's service NIC (the VIP, LAN; -> eth2 on a data node); empty = no NIC
	ServiceMAC string // MAC for that NIC; empty = QEMU default (fine single-node, collides on a shared fleet bridge)
	SystemTap  string // host tap for the guest's system NIC (private DRBD subnet; -> eth1); empty = none
	SystemMAC  string // MAC for that NIC; empty = QEMU default
	WitnessTap string // host tap for the guest's private witness NIC (guest<->host cloud-witness reach; -> eth3); empty = none
	WitnessMAC string // MAC for that NIC; empty = QEMU default
	// NetMode selects the substrate for the service + system NICs: NetBridge (default) = host taps
	// qemu opens by name (`ifname=`, the bridge substrate); NetMacvtap = macvtap chardevs qemu
	// attaches to via an inherited fd (`fd=N` on /dev/tap<ifindex>), giving the guest L2
	// citizenship without a host bridge; NetIpvtap is NetMacvtap's shape for a wireless parent,
	// whose chardevs belong to ipvtap children in nic.HoldNS that carry the parent's MAC, so
	// SystemMAC and ServiceMAC are the parent's and nothing pins a MAC on the children. The witness
	// NIC is always a plain tap regardless: macvtap isolates guest<->host, exactly the private link
	// the witness-forwarder needs.
	NetMode string
	// NetWrapBin is the bundled fd-passing launch wrapper (briard-net-wrap) required by
	// NetMacvtap and NetIpvtap: systemd-run starts the guest via PID 1, so the agent cannot hand
	// qemu an inherited chardev fd. Empty in NetBridge mode (qemu opens taps by name).
	NetWrapBin string
	SerialLog  string // if set, capture the guest serial console (ttyS0) to this file; empty = discard
	Unit       string // transient systemd unit name for the guest; empty = GuestUnit
	// UnitLog is where the unit's own stdout and stderr go -- qemu's, not the guest's (that is
	// SerialLog). Empty = the service manager's default, which on Linux is the journal and is
	// right there. A host whose manager keeps no log (the SCM) names a file, or qemu's one line
	// explaining why it would not start goes nowhere.
	UnitLog string
	// DataDir is qemu's firmware/BIOS blob directory, passed as `-L`. Empty = qemu's
	// compiled-in default (correct when qemu is a distro/Nix package). The relocatable
	// bundle sets it to <prefix>/share/qemu: that qemu's built-in datadir is a /nix/store path
	// absent on a stock host, so SeaBIOS/vgabios would not be found and the guest would not boot.
	DataDir string
}

const (
	// StateDriveID / StateDiskSerial name the state disk: the drive id on the command
	// line, and the serial the guest sees it by (/dev/disk/by-id/virtio-briard-state).
	StateDriveID    = "briard-state"
	StateDiskSerial = "briard-state"
	// RootDriveID and DataDriveID name the guest OS disk and the DRBD backing disk. Nothing
	// addresses either, but every disk is an explicit device (see qemuArgs) and an explicit
	// device wants a named drive.
	RootDriveID = "briard-root"
	DataDriveID = "briard-data"
	// GuestDataDevice is where the data disk appears inside the guest: a hardware fact of the VM
	// this file builds, not configuration. The disks are explicit devices in command-line order, so
	// the slots ascend root < data < state and the guest sees vda/vdb/vdc (see qemuArgs). Named
	// here, beside the line that attaches it second, because the host tells the guest which device
	// to build its storage tier on (shared/nodestorage) rather than the guest assuming it.
	GuestDataDevice = "/dev/vdb"
	// IOThreadID names the one iothread every disk's I/O runs on (see qemuArgs).
	IOThreadID = "briard-io"
)

// Net substrate modes for QEMUSpec.NetMode.
const (
	NetBridge  = ""        // default: qemu opens the host tap by name (ifname=), the bridge substrate
	NetMacvtap = "macvtap" // qemu attaches to /dev/tap<ifindex> via an inherited fd
	NetIpvtap  = "ipvtap"  // the same, on ipvtap children held in nic.HoldNS (a wireless parent)
)

// fdPassed reports whether mode's NICs are chardevs the launch wrapper opens on inherited fds.
func fdPassed(mode string) bool { return mode == NetMacvtap || mode == NetIpvtap }

// The guest's WAN net (eth0, qemu SLIRP). These are qemu's defaults, written out because the guest
// configures eth0 statically from the same numbers and runs no DHCP client for it, so the two
// sides agree by construction rather than by a convention either could change without the other
// noticing. Paired with guest-image/disk-image.nix (networking.interfaces.eth0 / defaultGateway /
// nameservers): change one, change both.
const (
	slirpNet     = "10.0.2.0/24"
	slirpGateway = "10.0.2.2"
	slirpDNS     = "10.0.2.3"
)

// Fixed fds the macvtap launch wrapper opens and qemu inherits (fds 0-2 are the
// journal's stdio; a non-socket-activated systemd unit passes nothing above 2).
// The mapping is deterministic so qemuArgs (pure) and the wrapper agree without
// threading state: system NIC -> fd 3, service NIC -> fd 4.
const (
	sysFD = 3
	svcFD = 4
)

// MemoryStepMB is the size of one DIMM, the unit the VM grows by. Half a gigabyte is small
// enough that a step is not a jump for a 1 GB guest and large enough that a household's growth is
// a handful of steps, not dozens of slots.
const MemoryStepMB = 512

// maxDIMMSlots is QEMU's ceiling on memory-hotplug slots (ACPI_MAX_RAM_SLOTS); asking for more
// refuses the launch.
const maxDIMMSlots = 256

// memoryArg renders -m: the boot size alone, or the boot size plus the slots and the address
// space to grow into, one slot per step up to MaxMemoryMB.
func memoryArg(s QEMUSpec) string {
	if s.MaxMemoryMB <= s.MemoryMB {
		return strconv.Itoa(s.MemoryMB)
	}
	slots := min((s.MaxMemoryMB-s.MemoryMB+MemoryStepMB-1)/MemoryStepMB, maxDIMMSlots)
	return fmt.Sprintf("%d,slots=%d,maxmem=%dM", s.MemoryMB, slots, s.MaxMemoryMB)
}

// qemuArgs renders the QEMU argv (excluding the binary). Pure; unit-tested.
func qemuArgs(s QEMUSpec) []string {
	// -nodefaults: no device qemu adds unasked -- the empty CD-ROM, the floppy, the parallel port,
	// the VGA, the default NIC and monitor. None has a reader: grub and the kernel speak on the
	// serial console, the monitor is QMP, and every serial port, disk and NIC below is explicit. The
	// IDE controller stays: it is part of the pc machine itself, not a default device.
	//
	// hpet=off: qemu's emulated HPET ticks at 1 kHz in legacy mode, waking qemu's main loop every
	// millisecond to inject an interrupt, for a device the guest never reads. Its clocksource is the
	// hypervisor's paravirtual clock (kvm-clock under KVM, the Hyper-V TSC page under WHPX) and its
	// timers the local APIC's or Hyper-V's synthetic ones, so without the HPET it loses nothing.
	// Measured on idle nodes: qemu from ~15% of a core to ~7% (KVM) and from ~28% to ~4% (WHPX).
	args := []string{"-nodefaults", "-machine", "accel=" + s.Accel + ",hpet=off"}
	if s.CPUModel != "" {
		args = append(args, "-cpu", s.CPUModel)
	}
	args = append(args,
		"-m", memoryArg(s),
		"-smp", strconv.Itoa(s.Cores),
		// Every restart goes through the supervisor. `-no-reboot` makes QEMU exit on a guest
		// reset rather than quietly start the machine again inside the same process, so a guest
		// that reboots itself surfaces as a unit that stopped, an event the agent can see, count
		// and act on, instead of a gap in the control channel indistinguishable from every other
		// gap. The agent is the guest's sole policy supervisor (`-p Restart=no`, see GuestUnit),
		// and a supervisor that cannot observe its charge restarting is not supervising it. A
		// guest that panics its way through boot therefore exits rather than spinning invisibly,
		// which lets the recovery loop damp a crash loop instead of racing one.
		//
		// The cost: the guest cannot complete its own reboot; it needs the agent to relaunch it.
		// That is why the recovery loop checks this unit's state before anything else
		// (agent/host/guestrecover.go): a stopped unit is relaunched at once, with no window and
		// no gate, because a VM that is not running can neither heal itself nor outage a peer.
		"-no-reboot",
		"-display", "none",
		// The host<->guest control channel: a virtio-serial port named
		// guestfirmware.ControlPort, backed by a host unix socket QEMU serves.
		"-device", "virtio-serial-pci",
		"-chardev", "socket,id=briardctl,path="+s.ControlSock+",server=on,wait=off",
		"-device", "virtserialport,chardev=briardctl,name="+guestfirmware.ControlPort,
	)
	if s.AdminPortSock != "" {
		// The guest's admin port: the same shape as the control channel with the
		// roles reversed -- the dashboard in the guest writes, the host agent answers. A
		// second port on the same virtio-serial bus, so it exists on every platform the
		// control channel does, and nowhere the network is.
		args = append(args,
			"-chardev", "socket,id=briardadmin,path="+s.AdminPortSock+",server=on,wait=off",
			"-device", "virtserialport,chardev=briardadmin,name="+dashboard.AdminPort,
		)
	}
	if s.QMPSock != "" {
		// Wait=off so a guest never blocks on the monitor being connected: the agent dials
		// it only for the rare deliberate operations, and a VM that cannot boot
		// without a supervisor attached would be a worse dependency than the one it fixes.
		args = append(args, "-qmp", "unix:"+s.QMPSock+",server=on,wait=off")
	}
	if s.DataDir != "" {
		// Point qemu at the bundled firmware dir: its compiled-in datadir is a
		// /nix/store path that does not exist on a stock host, so SeaBIOS/vgabios must be
		// found here or the guest never boots.
		args = append(args, "-L", s.DataDir)
	}
	args = append(args, serialArgs(s)...)
	// The disks, every one an explicit device, in this order. `-drive if=virtio` shorthand and
	// `-device virtio-blk-pci` do not mix: qemu realises the explicit devices first and the
	// shorthand drives last, so a -device disk (the state disk needs one: a serial is a device
	// property) takes a lower PCI slot than a shorthand root. SeaBIOS then boots the blank state
	// disk, and the guest sits at a grub rescue prompt with an empty serial console and a host that
	// can only say "no handshake". Explicit devices in command-line
	// order keep the slots ascending root < data < state (vda/vdb/vdc), and bootindex=0 on the root
	// says which one boots whatever else is ever attached. The drive ids are what QMP addresses
	// (block_resize on StateDriveID); a serial is what the guest finds a disk by
	// (/dev/disk/by-id/virtio-<serial>).
	//
	// Every disk also gets format= (qemu probes the format of a file that names none, which its own
	// docs call unsafe); the cache mode diskCache, per OS (alloc_*.go says why each); and the one
	// iothread, so block I/O runs off qemu's main loop. That loop also serves the virtio-serial
	// control channel, the agent's liveness signal, and a disk busy with a resync must not delay it
	// into a self-fence.
	args = append(args, "-object", "iothread,id="+IOThreadID)
	if s.DiskImage != "" {
		args = append(args, "-drive", "file="+s.DiskImage+",if=none,format=qcow2,cache="+diskCache+",readonly=on,id="+RootDriveID,
			"-device", "virtio-blk-pci,drive="+RootDriveID+",iothread="+IOThreadID+",bootindex=0")
	}
	if s.DataDisk != "" {
		// werror=report, like the state drive: the volume is thick, so a host ENOSPC under it is
		// reachable only where a copy-on-write host filesystem did not honour the reservation, and
		// there an I/O error the guest's btrfs and DRBD can see and report beats qemu's default --
		// a whole-VM pause that nothing inside it can observe or explain.
		args = append(args, "-drive", "file="+s.DataDisk+",if=none,format=raw,cache="+diskCache+",werror=report,id="+DataDriveID,
			"-device", "virtio-blk-pci,drive="+DataDriveID+",iothread="+IOThreadID)
	}
	if s.StateDisk != "" {
		// No discard: the state disk is thick, its bytes paid for when it was made or grown, and a
		// guest TRIM passed through would punch holes in it and turn it sparse again, charging the
		// host at write time: the failure the thick disk exists to remove.
		//
		// werror=report: a write the host cannot back (a copy-on-write host filesystem the NOCOW
		// flag did not reach) is an I/O error to the one guest writer, not qemu's default of
		// pausing the whole VM: the state disk holds a cache, a journal and scratch, none of which
		// is worth the household's app stopping for.
		args = append(args, "-drive", "file="+s.StateDisk+",if=none,format=raw,cache="+diskCache+",werror=report,id="+StateDriveID,
			"-device", "virtio-blk-pci,drive="+StateDriveID+",iothread="+IOThreadID+",serial="+StateDiskSerial)
	}
	if s.MachineUUID != "" {
		args = append(args, "-uuid", s.MachineUUID)
	}
	// NIC order (the guest uses net.ifnames=0, so ethN follows -device order): eth0 is qemu's SLIRP
	// user-net, the WAN path, first so the tapped NICs enumerate predictably; eth1 the system/DRBD
	// NIC, present on every fleet node, a witness included, so it earns the low index and a
	// witness needs no placeholder; eth2 the service NIC (the VIP, on the LAN) on a data node, the
	// two-subnet model; eth3 the witness NIC (a private guest<->host link the host's
	// witness-forwarder answers), uniform on a pair-capable guest and idle until a pairing
	// addresses it (no hotplug, no reboot). Which NIC carries the VIP is the agent's to say
	// (net.configure); nothing is baked guest-side.
	//
	// ⚠️ The NIC count is the substrate fork, two shapes rather than one. Under macvtap the agent
	// sets all three taps (agent/host applySubstrate) and the guest boots eth1/eth2/eth3. Under
	// bridge mode it sets only SystemTap, one tap being all Windows can express: the guest boots
	// eth1 alone, makes eth2 itself (a macvlan child carrying the flock MAC), and has no eth3 at
	// all (a bridged tap already puts host and guest on one L2, leaving the private link nothing
	// to do). Both shapes present the same L3. ⚠️ Omitting SystemTap is what breaks: whatever else is
	// set slides down an index (the service NIC becomes eth1, the witness NIC eth2), and every
	// address the agent hands out lands on the wrong device. A caller setting ServiceTap or
	// WitnessTap must set SystemTap too.
	if s.ServiceTap != "" || s.SystemTap != "" || s.WitnessTap != "" {
		// SLIRP's addressing is pinned rather than defaulted: see slirpNet.
		args = append(args, "-netdev", "user,id=net0,net="+slirpNet+",host="+slirpGateway+",dns="+slirpDNS,
			"-device", "virtio-net-pci,netdev=net0")
	}
	if s.SystemTap != "" {
		args = append(args,
			"-netdev", netdevArg(systemNetdev, s.SystemTap, s.NetMode, sysFD),
			"-device", "virtio-net-pci,netdev="+systemNetdev+macArg(s.SystemMAC))
	}
	if s.ServiceTap != "" {
		args = append(args,
			"-netdev", netdevArg(serviceNetdev, s.ServiceTap, s.NetMode, svcFD),
			"-device", "virtio-net-pci,netdev="+serviceNetdev+macArg(s.ServiceMAC))
	}
	if s.WitnessTap != "" {
		// The witness NIC is always a plain tap, even under NetMacvtap: it is the private
		// guest<->host link the witness-forwarder answers, and macvtap would isolate exactly that
		// path. ⚠️ NetBridge here means "a tap qemu opens by name", not "this node is in bridge
		// mode": the two meanings share a constant and do not coincide. A node actually in bridge
		// mode has no WitnessTap at all, so this branch is reached only under macvtap/ipvtap.
		args = append(args,
			"-netdev", netdevArg("net3", s.WitnessTap, NetBridge, 0),
			"-device", "virtio-net-pci,netdev=net3"+macArg(s.WitnessMAC))
	}
	return args
}

// netdevArg renders a tapped NIC's -netdev value. In NetBridge mode qemu opens the
// host tap by name (ifname=). In NetMacvtap mode the device is a macvtap chardev
// /dev/tap<ifindex> qemu cannot open by name; the launch wrapper opens it on
// fd and qemu attaches to the inherited fd. dev is unused in macvtap mode here (the
// wrapper resolves the /dev/tap node from it), but naming it keeps the call sites uniform.
func netdevArg(id, dev, mode string, fd int) string {
	if fdPassed(mode) {
		return "tap,id=" + id + ",fd=" + strconv.Itoa(fd)
	}
	return "tap,id=" + id + ",ifname=" + dev + ",script=no,downscript=no"
}

// launchExec assembles the ExecStart argv (everything after systemd-run's `--`). Pure;
// unit-tested. In NetBridge mode qemu runs directly. In NetMacvtap mode it runs behind the bundled
// fd-passing wrapper, `briard-net-wrap <dev> <mac> <fd> [<dev> <mac> <fd>] -- qemu ...`, which
// pins each macvtap's MAC (matching qemu's mac=), brings it up, opens its /dev/tap<ifindex>
// chardev on the given fd, then execs qemu, which inherits the fds the `fd=` netdevs reference.
// The witness NIC is never listed: it is a plain tap qemu opens by name. Triples are separate argv
// words (not delimited), so a MAC's colons never need escaping.
//
// Under NetIpvtap a triple names `<namespace>/<dev>` (a slash cannot occur in an interface name)
// and an empty MAC: the children are in nic.HoldNS, and an ipvtap child's MAC is its parent's and
// cannot be changed, so there is nothing to pin. qemu's mac= still carries it (qemuArgs).
func launchExec(s QEMUSpec) []string {
	qemu := append([]string{s.Binary}, qemuArgs(s)...)
	if !fdPassed(s.NetMode) {
		return qemu
	}
	triple := func(dev, mac string, fd int) []string {
		if s.NetMode == NetIpvtap {
			dev, mac = nic.HoldNS+"/"+dev, ""
		}
		return []string{dev, mac, strconv.Itoa(fd)}
	}
	exec := []string{s.NetWrapBin}
	if s.SystemTap != "" {
		exec = append(exec, triple(s.SystemTap, s.SystemMAC, sysFD)...)
	}
	if s.ServiceTap != "" {
		exec = append(exec, triple(s.ServiceTap, s.ServiceMAC, svcFD)...)
	}
	exec = append(exec, "--")
	return append(exec, qemu...)
}

// macArg renders the ",mac=" suffix for a virtio-net-pci device, or "" to let QEMU
// pick its default. A default MAC is fine for a single guest but is identical across
// guests, so a fleet on a shared bridge must set unique MACs (else the NICs collide).
func macArg(mac string) string {
	if mac == "" {
		return ""
	}
	return ",mac=" + mac
}

// GuestUnit is the transient unit the guest VM runs as (on Windows, the name of its job object,
// unit_windows.go). On Linux the systemd manager launches the guest (via systemd-run), so it
// lands in its own cgroup, a sibling of briard-agent.service: a `systemctl restart briard-agent`
// kills only the agent's cgroup and leaves the guest serving. The agent stays the guest's sole
// policy supervisor (`-p Restart=no`), re-adopts the running guest over the persisted control
// socket after a restart, and stops it cleanly when it stops it at all (a node shutdown's
// PowerOffGuest; Stop is only the fallback when a clean stop fails). Not `systemd-run --scope`:
// a scope's payload stays in the caller's cgroup and dies with the agent.
const GuestUnit = "briard-guest.service"

// Guest is a running guest VM, identified by its transient systemd unit.
type Guest struct {
	ControlSock string
	QMPSock     string // empty = this guest was launched without a monitor
	unit        string
}

func (s QEMUSpec) unit() string {
	if s.Unit != "" {
		return s.Unit
	}
	return GuestUnit
}

// guestStopTimeout is the guest unit's TimeoutStopSec, deliberately above GuestShutdownGrace: the
// ExecStop gives up first and says so, and only then does systemd start killing. The other order
// would put a SIGKILL through the middle of a shutdown still making progress, the power cut this
// whole arrangement exists to avoid.
const guestStopTimeout = 75 * time.Second

// launchArgs renders the systemd-run command line for the guest unit, split out of Launch so the
// unit's stop contract (the part with no observable effect until the machine is going down) is
// assertable without a systemd.
//
// ExecStop makes a host shutdown survivable. Without it, stopping this unit SIGTERMs QEMU, a power
// cut to the guest (see Guest.Shutdown), and systemd stops every unit when the host reboots: a
// user rebooting their own machine for their distro's updates would power-cut the appliance every
// time, losing whatever the apps had not flushed. It belongs on the unit, not in the agent's own
// SIGTERM handler, and that is the crux, not a preference: an agent restart must be transparent
// to the guest, the signal alone does not say which stop it is, and systemd stops this unit for a
// shutdown and not for a restart, whatever the agent makes of its signal. The agent's own way
// down (PowerOffGuest) is a plain stop of this unit, so the powerdown is written once, here. No
// ExecStop without a monitor socket: there is then no way to ask, and a unit that promises a
// clean stop it cannot deliver is worse than one that promises nothing.
func launchArgs(s QEMUSpec) []string {
	args := []string{
		"--unit=" + s.unit(),
		"--collect", // GC a prior dead instance so re-launch doesn't hit a lingering failed unit
		"-p", "Restart=no",
		"-p", "Description=Briard guest VM (" + s.unit() + ")",
	}
	if bin := shutdownBin(); bin != "" && s.QMPSock != "" {
		args = append(args,
			"-p", "ExecStop="+bin+" --guest-shutdown="+s.QMPSock,
			"-p", "TimeoutStopSec="+strconv.Itoa(int(guestStopTimeout.Seconds())),
		)
	}
	if s.UnitLog != "" {
		args = append(args, "-p", "StandardOutput=append:"+s.UnitLog, "-p", "StandardError=append:"+s.UnitLog)
	}
	return append(append(args, "--"), launchExec(s)...)
}

// shutdownBin is the absolute path ExecStop invokes: this very binary, which grows a
// --guest-shutdown mode for the purpose. /proc/self/exe is read at launch time and resolves to
// the committed <base>/briard-agent (agent/selfupdate), the stable path: an update renames the
// new binary onto that name before starting it, so the file swaps under a unit line that stays
// correct. Should the path not resolve, systemd fails the ExecStop and falls through to SIGTERM:
// a lost clean stop, never a new failure.
func shutdownBin() string {
	bin, err := os.Executable()
	if err != nil {
		return ""
	}
	return bin
}

// Launch boots the guest as a transient systemd service (systemd-run) and waits until QEMU has
// created the control socket. The unit is detached from the agent's cgroup, so it outlives an
// agent restart. The socket exists before the guest OS boots, so the guest agent's readiness is
// established later, when a call over the channel first succeeds (guestagent.BringUp). ctx
// bounds only the brief systemd-run start call + the socket wait, not qemu's lifetime: binding
// qemu to the agent's ctx would re-couple the lifecycles.
func Launch(ctx context.Context, s QEMUSpec) (*Guest, error) {
	unit := s.unit()
	if fdPassed(s.NetMode) && s.NetWrapBin == "" {
		return nil, fmt.Errorf("platform: %s requires NetWrapBin (the fd-passing launch wrapper)", s.NetMode)
	}
	args := launchArgs(s)
	if err := secureQMPDir(s.QMPSock); err != nil {
		return nil, err
	}
	// The name has to be free before systemd-run will take it, and "not running" is not yet
	// "free" (see waitUnitFree).
	if err := waitUnitFree(ctx, unit, unitFreeGrace); err != nil {
		return nil, err
	}
	// ⚠️ After waitUnitFree and before startTransient, the only window where rotating is safe: no
	// qemu of ours holds the capture open, and the one about to has not opened it yet.
	// Rotating under a live qemu would rename the inode it is writing to, so the stream would go
	// on filling `.prev` while the file everything reads stayed empty.
	prepareSerialLog(s.SerialLog)
	if out, err := startTransient(ctx, args); err != nil {
		return nil, fmt.Errorf("platform: start guest unit: %w: %s", err, out)
	}
	if err := waitForSocket(ctx, s.ControlSock); err != nil {
		// Force, not the graceful stop: this guest never even produced its control socket, so
		// asking it to power off politely spends the whole ExecStop grace waiting for an ACPI
		// reply from something that has not finished booting, inside a failure path the bring-up
		// budget is already counting against.
		_ = forceStopUnit(unit)
		return nil, err
	}
	return &Guest{ControlSock: s.ControlSock, QMPSock: s.QMPSock, unit: unit}, nil
}

// secureQMPDir makes the monitor socket's directory owner-only, creating it if needed. Anyone who
// can connect to QMP owns the VM outright (reset it, stop it, dump its RAM to a file), and QEMU
// itself creates the socket 0755, so this is the containment: applied before qemu is started
// rather than after, and on every launch, so an existing directory with looser modes is tightened
// rather than trusted.
func secureQMPDir(sock string) error {
	if sock == "" {
		return nil
	}
	dir := filepath.Dir(sock)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("platform: QMP dir %s: %w", dir, err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("platform: QMP dir %s: %w", dir, err)
	}
	return nil
}

// serialLogMax is the size past which the guest's serial capture rolls to `.prev`: one generation,
// so the capture costs at most 2x this on disk and the previous story is still there. A constant
// rather than a knob: it is a disk-fill guard on an unattended machine, and the number that
// matters is "small enough that a home disk never notices, large enough to hold several boots".
// 32 MiB is both.
const serialLogMax = 32 << 20

// prepareSerialLog bounds the guest's serial capture and creates it with a mode of our own, on
// every launch and before qemu opens it, for secureQMPDir's reason: qemu creates what it opens
// under the unit's umask, so the containment has to come first. Best-effort throughout, and
// silent: it is a logging convenience, and a node must never fail to launch its guest because a
// log could not be rolled; a pre-create that fails costs the mode, not the capture (qemu creates
// the file itself).
//
// ⚠️ The bound has to be per launch. qemu deliberately appends and never truncates (serialArgs
// says why), and the guest is relaunched on every OS upgrade, every rollback, every rung of the
// recovery ladder. A check run once at agent start would miss every one of those, i.e. the
// crash-looping guest the cap exists for, and would fire while a re-adopted qemu held the file
// open: the rename trap the caller describes.
func prepareSerialLog(path string) {
	if path == "" {
		return // this node captures no console
	}
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	if fi, err := os.Stat(path); err == nil && fi.Size() > serialLogMax {
		_ = os.Rename(path, path+".prev")
	}
	// Created here rather than left to qemu, because a guest console carries the household's
	// hostnames and addresses: not secret, not public. Chmod unconditionally, so an existing
	// world-readable file is tightened rather than trusted: secureQMPDir's "applied on every
	// launch" rule.
	if f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640); err == nil {
		_ = f.Close()
	}
	_ = os.Chmod(path, 0o640)
}

// Adopt returns a handle to an already-running guest (re-adopt after an agent
// restart) without launching QEMU. The caller reconnects over the
// persisted control socket; the guest's bring-up is idempotent, so
// re-driving it is a no-op on an already-converged node.
func Adopt(s QEMUSpec) *Guest {
	return &Guest{ControlSock: s.ControlSock, QMPSock: s.QMPSock, unit: s.unit()}
}

// Running reports whether the guest's transient service is currently active — the
// re-adopt probe. Any non-"active" state (inactive/failed/absent) reads as false,
// so the caller launches fresh.
func Running(ctx context.Context, s QEMUSpec) bool {
	return unitIsActive(ctx, s.unit())
}

// LastLines is what the VM and the guest said last: the final lines of the unit's own output
// (qemu's: `WHPX: Unexpected VP exit code 4` is written there; on Linux the unit's journal for its
// current invocation, on Windows the file the unit appends to) and of the serial console capture
// (`Kernel panic - not syncing`). It is for saying why a guest stopped answering, in the alert
// that announces it, never for deciding that it has: the recovery ladder decides on silence alone,
// and reads this once it has decided. No pattern list, so a cause nobody anticipated is shown too.
// Best-effort throughout: a log that cannot be read contributes nothing, and "" means neither
// could.
func LastLines(ctx context.Context, s QEMUSpec) string {
	var parts []string
	if t := unitTail(ctx, s, lastLinesN); t != "" {
		parts = append(parts, "qemu: "+t)
	}
	if s.SerialLog != "" {
		if t := tailFile(s.SerialLog, lastLinesN); t != "" {
			parts = append(parts, "console: "+t)
		}
	}
	return strings.Join(parts, "; ")
}

// lastLinesN lines of each log, each cut at lastLineWidth runes: enough to carry the one line
// that names the cause, short enough for an alert body.
const (
	lastLinesN    = 3
	lastLineWidth = 160
)

// tailFile is the last n speaking lines of a file, read from a bounded window at its end so a
// console capture of many megabytes costs one small read.
func tailFile(path string, n int) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return ""
	}
	const window = 16 << 10
	off := max(fi.Size()-window, 0)
	buf := make([]byte, fi.Size()-off)
	if _, err := f.ReadAt(buf, off); err != nil && err != io.EOF {
		return ""
	}
	return lastLines(string(buf), n)
}

// ansiCSI matches a terminal control sequence, which a console capture carries around the kernel's
// coloured lines; the escape byte alone is not printable, but its parameters are.
var ansiCSI = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)

// lastLines keeps the last n non-blank lines of text, in order, joined by " | ", with control
// sequences and characters dropped (a console carries carriage returns and escapes) and each cut
// at lastLineWidth.
func lastLines(text string, n int) string {
	var keep []string
	lines := strings.Split(ansiCSI.ReplaceAllString(text, ""), "\n")
	for i := len(lines) - 1; i >= 0 && len(keep) < n; i-- {
		l := strings.TrimSpace(strings.Map(func(r rune) rune {
			if unicode.IsPrint(r) {
				return r
			}
			return -1
		}, lines[i]))
		if l == "" {
			continue
		}
		if r := []rune(l); len(r) > lastLineWidth {
			l = string(r[:lastLineWidth]) + "…"
		}
		keep = append([]string{l}, keep...)
	}
	return strings.Join(keep, " | ")
}

// unitState reads a unit's ActiveState. The service manager answers for units that do not exist
// too (an absent unit is "inactive", exit 0), so the error return means the query itself failed
// -- which is not an answer about the unit and must not be read as one.
func unitState(unit string) (string, error) {
	return unitShow(unit, "ActiveState")
}

// unitAtRest reports whether an ActiveState reading means the unit is finished (gone or dead, with
// nothing of it still running), as distinct from merely not-"active". A transient unit spends its
// ExecStop in "deactivating", which is not "active", so a caller that asks `is-active` hears
// "stopped" about a guest still flushing, and the next Launch trips over the unit it was told had
// gone ("already loaded or has a fragment file"), rolling back a good upgrade. Measured: such a
// stop takes ~19s, and the collision lands within the first 2.
//
// It names the states that mean at-rest rather than excluding the ones that mean running, so an
// unfamiliar reading (a systemd that grew a state, an empty line from a systemctl that failed)
// keeps the caller waiting instead of declaring a running guest gone. Waiting costs a bounded
// grace and reports what it saw; the other direction costs the upgrade.
func unitAtRest(state string, err error) bool {
	if err != nil {
		return false
	}
	return state == "inactive" || state == "failed"
}

// unitStopped is unitAtRest over a live systemctl.
func unitStopped(unit string) bool {
	return unitAtRest(unitState(unit))
}

// unitFreeGrace bounds the wait for a stopping guest's unit name to come free: above the unit's
// own TimeoutStopSec, the longest a stop can legitimately take before systemd gives up and kills
// it, and comfortably inside the default bring-up budget, which this wait is spending.
const unitFreeGrace = guestStopTimeout + 15*time.Second

// unitLoaded reports whether a LoadState reading means the unit name is still taken. systemd-run
// refuses a name that is loaded ("already loaded or has a fragment file"), and a unit stays loaded
// for its whole stop and until systemd garbage-collects the corpse, so a name is free only once
// systemctl has forgotten the unit entirely. Like unitAtRest, it names the reading that means gone
// rather than excluding the ones that mean present: an unfamiliar answer, or a systemctl that
// failed, keeps the caller waiting instead of launching into a name that is still taken.
func unitLoaded(state string, err error) bool {
	return err != nil || state != "not-found"
}

// waitUnitFree blocks until the transient unit name can be created again: unitAtRest's distinction
// one step earlier. The relaunch decision asks `is-active`, so a unit spending its ExecStop reads
// as not running, which is true and not the same as creatable: systemd-run then trips over the
// unit that is still there, and without this wait the guest-recovery ladder spends its relaunch
// burst inside one second on a condition that clears by itself in a few.
//
// reset-failed is the one prod that helps: a finished unit is garbage-collected on its own
// (`--collect`), but a corpse someone still references is not, and clearing it is free. It is
// asked only of a unit at rest: reset-failed says nothing to a unit that is still stopping.
func waitUnitFree(ctx context.Context, unit string, grace time.Duration) error {
	deadline := time.Now().Add(grace)
	for first := true; ; first = false {
		if !unitLoaded(unitLoadState(unit)) {
			return nil
		}
		// Asked once, and only once the name is known to be taken: if we are the invocation
		// holding it, waiting is not slow but impossible, and saying so beats spending the whole
		// grace to say nothing.
		if first && insideUnit(unit) {
			return fmt.Errorf("platform: refusing to wait for unit %s to come free: this process is "+
				"part of that unit's own invocation, so the name cannot be released until we return", unit)
		}
		if unitStopped(unit) {
			unitResetFailed(unit)
		}
		if time.Now().After(deadline) {
			// Name what it still looks like: a unit that is stopping and a corpse that will not
			// be reaped are different faults reaching this line the same way.
			active, _ := unitState(unit)
			load, _ := unitLoadState(unit)
			return fmt.Errorf("platform: unit %s is still loaded after %s (LoadState %q, ActiveState %q)",
				unit, grace, load, active)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("platform: waiting for unit %s to come free: %w", unit, ctx.Err())
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// insideUnit reports whether this process is part of unit's current invocation (systemd spawned it
// as an ExecStop, an ExecStopPost, or anything else inside the unit). A unit's stop job waiting for
// that unit's own name to come free is not a slow wait but a closed loop: the caller holds the
// name, so only the return this call is blocking could release it. Nothing looks wrong from inside
// (every state the wait reads is legitimately "still stopping"), so it spends the caller's whole
// budget in silence and the guest gets power-cut at the far end.
//
// The seam is $INVOCATION_ID, which systemd exports to every process it starts for a unit and
// which the unit itself answers with for as long as that invocation lasts. Measured inside an
// ExecStop: both sides read the same id, and the unit reads LoadState=loaded /
// ActiveState=deactivating, exactly what waitUnitFree treats as "keep waiting". Every uncertain
// answer is not-inside (an empty environment: nothing started us as a unit; a failed query; a
// mismatched id): the guard only ever converts a guaranteed-hopeless wait into an immediate error,
// so being wrong costs the bounded wait we would have done anyway, never a launch that would
// otherwise have succeeded.
func insideUnit(unit string) bool {
	id := os.Getenv("INVOCATION_ID")
	if id == "" {
		return false
	}
	cur, err := unitShow(unit, "InvocationID")
	if err != nil {
		return false
	}
	return cur == id
}

// unitLoadState reads a unit's LoadState ("loaded" / "not-found"). As with unitState, an error
// means the query itself failed and is not an answer about the unit.
func unitLoadState(unit string) (string, error) {
	return unitShow(unit, "LoadState")
}

// Stop is the forced stop, a power cut to the guest, and only ever the fallback when a clean stop
// has already failed: on the recovery ladder, a rescue, an image rollback. It is not called on an
// agent shutdown: an agent restart must be transparent to the guest, and a node shutdown powers
// it off cleanly (PowerOffGuest). No-op-safe if gone.
//
// It kills QEMU first: a guest that just failed a clean stop will not answer an ACPI powerdown
// either, so going through the unit's ExecStop would buy nothing and cost a full TimeoutStopSec.
// With the main process killed first, the unit deactivates on its own and the stop below is
// bookkeeping.
func (g *Guest) Stop() error {
	if g == nil || g.unit == "" {
		return nil
	}
	return forceStopUnit(g.unit)
}

// forceStopUnit takes the guest down now: SIGKILL the cgroup, then stop the unit to reap it. The
// kill is what makes it immediate: with an ExecStop on the unit, a plain stop is a request for a
// clean powerdown, and every caller here is one that cannot wait for one.
func forceStopUnit(unit string) error {
	// Best-effort: a unit that is already gone makes this fail, which is not worth reporting --
	// stopUnit below is what decides whether the guest is really down.
	unitKill(unit)
	return stopUnit(unit)
}

func stopUnit(unit string) error {
	if out, err := unitStop(unit); err != nil {
		return fmt.Errorf("platform: stop %s: %w: %s", unit, err, out)
	}
	return nil
}

// RemoveUnit takes one of briard's units off this machine (`briard uninstall`): stopped,
// disabled, and its unit file deleted. A unit the manager has never heard of is already removed.
//
// A plain stop, never forceStopUnit: on the guest unit that runs its ExecStop (the clean
// powerdown), and uninstall is the one caller that both can wait for it and must, since the data
// volume it leaves behind is only worth keeping if the guest was not power-cut off it. Windows
// has no ExecStop (a job is killed, unitStop): its uninstall powers the guest down first.
//
// The file is asked of the manager, not assumed: the install writes the units wherever it was
// told (BRIARD_UNIT_DIR; a hermetic install puts them in /run), nothing records where, and the
// manager knows. A transient unit has no file of ours (systemd-run's is the manager's own, deleted
// when it collects the unit), so that one is only stopped. The caller reloads once, after the last.
//
// A unit the stop leaves failed is cleared, or it stays loaded with its file gone: the disable's
// reload can fire the Persistent= update timer on the spot, and stopping the update it started is
// a SIGTERM that systemd records as a failure.
func RemoveUnit(unit string) error {
	load, err := unitShow(unit, "LoadState")
	if err != nil {
		return fmt.Errorf("platform: asking about %s: %w", unit, err)
	}
	if load == "not-found" {
		return nil
	}
	fragment, err := unitShow(unit, "FragmentPath")
	if err != nil {
		return fmt.Errorf("platform: asking where %s lives: %w", unit, err)
	}
	transient := unitTransient(fragment)
	if !transient {
		if out, err := unitDisable(unit); err != nil {
			return fmt.Errorf("platform: disable %s: %w: %s", unit, err, out)
		}
	}
	if err := stopUnit(unit); err != nil {
		return err
	}
	unitResetFailed(unit)
	if transient || fragment == "" {
		return nil
	}
	for _, p := range []string{fragment, unitStamp(unit)} {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("platform: removing %s: %w", p, err)
		}
	}
	return nil
}

// ReloadUnits makes the manager forget the unit files RemoveUnit deleted.
func ReloadUnits() error {
	if out, err := unitsReload(); err != nil {
		return fmt.Errorf("platform: daemon-reload: %w: %s", err, out)
	}
	return nil
}

func waitForSocket(ctx context.Context, path string) error {
	for {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("platform: control socket %s never appeared: %w", path, ctx.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// DebugChardevID names the chardev behind the guest's debug console (ttyS1). It is created null on
// every launch and only ever points somewhere while `briard debug shell` holds it open
// (DebugArm/DebugDisarm); naming it here is what lets a later QMP caller address it.
const DebugChardevID = "serial1"

// serialArgs renders both of the guest's serial ports: two, always, in this order. ttyS0 is the
// capture, appended (see below): kernel, systemd and the guest's whole journal (it sets
// ForwardToConsole). ttyS1 is the debug console, where the guest runs an autologin getty: present
// on every launch, but backed by qemu's `null` chardev (it discards writes and never delivers a
// byte), reachable only from when DebugArm swaps that backend for a unix socket until the moment
// DebugDisarm swaps it back.
//
// Order is the contract, and the empty case is why this is a function: the guest numbers its ports
// by -serial position, the first emitted being ttyS0. The agent captures by default
// (GUEST_SERIAL), but a launch without a capture is ordinary (an empty GUEST_SERIAL, several
// rigs), and emitting no -serial then would silently promote the debug console to ttyS0: a login
// prompt where the kernel console belongs, and a getty out from under the unit bound to
// /dev/ttyS1. So a null-backed serial0 stands in, and ttyS1 is ttyS1 on every launch of every node.
func serialArgs(s QEMUSpec) []string {
	var a []string
	if s.SerialLog != "" {
		// Appending, so a relaunch does not overwrite the boot that explains it: `-chardev file`
		// truncates on open, and this guest is relaunched routinely (every OS upgrade, every
		// rollback onto the previous image, every rung of the recovery ladder). Truncating means
		// the console you need is the one just overwritten by the boot that replaced it, so the
		// log reliably holds every incarnation except the interesting one.
		a = append(a, "-chardev", "file,id=serial0,path="+s.SerialLog+",append=on")
	} else {
		a = append(a, "-chardev", "null,id=serial0")
	}
	return append(a,
		"-serial", "chardev:serial0",
		"-chardev", "null,id="+DebugChardevID,
		"-serial", "chardev:"+DebugChardevID,
	)
}

// UnitState is unitState for callers outside this package: one unit's ActiveState, read through
// the same service manager seam every other systemctl call goes through, so the host agent does
// not grow a second way to ask systemd a question. The error return keeps unitState's meaning,
// and it is the part a caller must not flatten: the manager answers for units that do not exist
// as well ("inactive", exit 0), so an error is the query having failed, not an answer about the
// unit.
func UnitState(unit string) (string, error) { return unitState(unit) }
