package platform

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"briard.io/agent/guestagent"
)

// QEMUSpec describes the guest VM to boot. The Linux v0 backend shells out to
// qemu-system directly (libvirt is a later backend behind this same boundary). Accel is a QEMU accel list, e.g. "kvm:tcg" -- KVM where
// available, TCG fallback (the Windows-Home / no-nested-virt case).
type QEMUSpec struct {
	Binary      string // qemu-system-x86_64
	Accel       string // e.g. "kvm:tcg"
	MemoryMB    int
	Cores       int
	DiskImage   string // guest OS disk; empty in kernel/initrd boots
	DataDisk    string // backing block device for the DRBD volume -> guest /dev/vdb
	ControlSock string // host end of the virtio-serial control channel
	// QMPSock is the host end of QEMU's own control channel -- the VM, not the guest OS
	// inside it. Empty = no monitor, which is what every path that never needs to
	// stop or reset a VM should pass. It buys a clean ACPI shutdown (Guest.Shutdown) where
	// the only alternative is killing QEMU, i.e. a power cut, and a forced reset for a
	// wedged guest. Launch creates its parent directory 0700 root: QMP is unrestricted
	// control of the VM, so the containment is the filesystem (see qmp.go).
	QMPSock    string
	ServiceTap string // host tap for the guest's service NIC (the VIP, LAN; -> eth2 on a data node); empty = no NIC
	ServiceMAC string // MAC for that NIC; empty = QEMU default (fine single-node, collides on a shared fleet bridge)
	SystemTap  string // host tap for the guest's system NIC (private DRBD subnet; -> eth1); empty = none
	SystemMAC  string // MAC for that NIC; empty = QEMU default
	WitnessTap string // host tap for the guest's private witness NIC (guest<->host cloud-witness reach; -> eth3); empty = none
	WitnessMAC string // MAC for that NIC; empty = QEMU default
	// NetMode selects the substrate for the *service + system* NICs:
	// NetBridge (default) = host taps qemu opens by name (`ifname=`, the install.sh bridge-
	// enslave substrate); NetMacvtap = macvtap chardevs qemu attaches to via an inherited fd
	// (`fd=N` on /dev/tap<ifindex>), which gives the guest L2 citizenship without a host bridge
	//. The witness NIC is ALWAYS a plain tap regardless -- macvtap isolates guest<->host,
	// Which is exactly the private link the witness-forwarder needs.
	NetMode string
	// NetWrapBin is the bundled fd-passing launch wrapper (briard-net-wrap) required by
	// NetMacvtap: systemd-run starts the guest via PID 1, so the agent cannot hand qemu an
	// inherited macvtap fd. Empty in NetBridge mode (qemu opens taps by name).
	NetWrapBin string
	// BootStaging arms the boot selector for THIS launch: it hands the guest's
	// firmware an SMBIOS type-11 OEM string that the guest's grub reads and, on a match,
	// uses to boot the `staging` profile instead of its default entry (see
	// guest-image/disk-image.nix and guestagent's os.stageboot).
	//
	// The arming is a property of the LAUNCH, not of the disk -- which is the point. An
	// OS-disk snapshot taken before the reboot therefore contains nothing armed, so
	// restoring it cannot re-run the generation the restore was undoing; and the rollback
	// is simply not passing the flag next launch. Unrecognised or absent, grub keeps its
	// default -- a bug boots the OLD system.
	BootStaging bool
	SerialLog   string // if set, capture the guest serial console (ttyS0) to this file; empty = discard
	Unit        string // transient systemd unit name for the guest; empty = GuestUnit
	// DataDir is qemu's firmware/BIOS blob directory, passed as `-L`. Empty = qemu's
	// compiled-in default (correct when qemu is a distro/Nix package). The relocatable
	// bundle sets it to <prefix>/share/qemu: that qemu's built-in datadir is a /nix/store path
	// absent on a stock host, so SeaBIOS/vgabios would not be found and the guest would not boot.
	DataDir string
}

const (
	// BootSelectStaging is the SMBIOS OEM string QEMUSpec.BootStaging passes and the guest's
	// grub matches on, verbatim. Its two halves are written in two repos-worth of places
	// apart (here and guest-image/disk-image.nix's extraConfig), so it is spelled out once
	// here and quoted there.
	BootSelectStaging = "briard_boot=staging"
	// RootDriveID names the guest OS disk on the QEMU command line so QMP can address it.
	RootDriveID = "briard-root"
)

// Net substrate modes for QEMUSpec.NetMode.
const (
	NetBridge  = ""        // default: qemu opens the host tap by name (ifname=), the bridge substrate
	NetMacvtap = "macvtap" // qemu attaches to /dev/tap<ifindex> via an inherited fd
)

// The guest's WAN net (eth0, qemu SLIRP). These ARE qemu's defaults; they are written out
// because the guest configures eth0 statically from the same numbers and runs no DHCP client
// for it -- so the two sides agree by construction rather than by a convention either could
// change without the other noticing. PAIRED with guest-image/disk-image.nix
// (networking.interfaces.eth0 / defaultGateway / nameservers): change one, change both.
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

// qemuArgs renders the QEMU argv (excluding the binary). Pure; unit-tested.
func qemuArgs(s QEMUSpec) []string {
	args := []string{
		"-machine", "accel=" + s.Accel,
		"-m", strconv.Itoa(s.MemoryMB),
		"-smp", strconv.Itoa(s.Cores),
		"-no-reboot",
		"-display", "none",
		// The host<->guest control channel: a virtio-serial port named
		// guestagent.ControlPort, backed by a host unix socket QEMU serves.
		"-device", "virtio-serial-pci",
		"-chardev", "socket,id=briardctl,path=" + s.ControlSock + ",server=on,wait=off",
		"-device", "virtserialport,chardev=briardctl,name=" + guestagent.ControlPort,
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
	if s.SerialLog != "" {
		// Capture the guest's ttyS0 console (kernel + systemd) for debugging.
		args = append(args, "-serial", "file:"+s.SerialLog)
	}
	if s.BootStaging {
		// The boot selector. QEMU emits this as an SMBIOS type-11 (OEM Strings)
		// structure; the guest's grub reads it back with `smbios --type 11 --get-string 4`
		// -- offset 4 is type 11's Count byte, and grub interprets the byte at the given
		// offset as a string NUMBER, so it resolves to the last (here: only) OEM string.
		// The full "briard_boot=staging" text is what grub matches, so the flag stays
		// self-describing in a process list and under dmidecode.
		args = append(args, "-smbios", "type=11,value="+BootSelectStaging)
	}
	if s.DiskImage != "" {
		// Id= is what a QMP command addresses the drive by (the snapshot work); the
		// name is fixed rather than derived so both sides can hard-code it.
		args = append(args, "-drive", "file="+s.DiskImage+",if=virtio,media=disk,id="+RootDriveID)
	}
	if s.DataDisk != "" {
		args = append(args, "-drive", "file="+s.DataDisk+",if=virtio,format=raw")
	}
	// Eth0 is a throwaway user-net so the tapped NICs enumerate predictably (the
	// guest uses net.ifnames=0, so ethN follows -device order). The system/DRBD NIC
	// comes first -> eth1: it is present on *every* fleet node, including a witness,
	// so it earns the low index and a witness needs no placeholder. The service NIC
	// (the VIP, on the LAN) comes second -> eth2 on a data node -- the two-subnet
	// model. The witness NIC (a private guest<->host link the host's
	// witness-forwarder answers) comes third -> eth3: uniform on a
	// managed/pair-capable guest, idle until a pairing addresses it (no hotplug, no
	// reboot -- extends c-ii's uniform layout). eth3 assumes the uniform layout
	// (system + service present, as the pairing path always sets them). A
	// single-node/legacy guest with only a service tap has it land on eth1
	// (positional), which the baked default vipDev matches; a data node's VIP moves
	// to eth2, which the agent tells the guest (net.configure).
	if s.ServiceTap != "" || s.SystemTap != "" || s.WitnessTap != "" {
		// SLIRP's addressing is PINNED rather than defaulted, because the guest configures eth0
		// statically from these exact numbers and runs no DHCP client for it. They happen to be
		// qemu's defaults today; written out, the two sides agree by construction instead of by
		// a convention either could change without the other noticing.
		args = append(args, "-netdev", "user,id=net0,net="+slirpNet+",host="+slirpGateway+",dns="+slirpDNS,
			"-device", "virtio-net-pci,netdev=net0")
	}
	if s.SystemTap != "" {
		args = append(args,
			"-netdev", netdevArg("net1", s.SystemTap, s.NetMode, sysFD),
			"-device", "virtio-net-pci,netdev=net1"+macArg(s.SystemMAC))
	}
	if s.ServiceTap != "" {
		args = append(args,
			"-netdev", netdevArg("net2", s.ServiceTap, s.NetMode, svcFD),
			"-device", "virtio-net-pci,netdev=net2"+macArg(s.ServiceMAC))
	}
	if s.WitnessTap != "" {
		// The witness NIC is ALWAYS a plain tap (NetBridge), even under NetMacvtap: it is
		// the private guest<->host link the witness-forwarder answers, and macvtap would
		// isolate exactly that path (decision).
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
	if mode == NetMacvtap {
		return "tap,id=" + id + ",fd=" + strconv.Itoa(fd)
	}
	return "tap,id=" + id + ",ifname=" + dev + ",script=no,downscript=no"
}

// launchExec assembles the ExecStart argv (everything after systemd-run's `--`). Pure;
// unit-tested. In NetBridge mode qemu runs directly. In NetMacvtap mode qemu runs behind
// the bundled fd-passing wrapper: `briard-net-wrap <dev> <mac> <fd> [<dev> <mac> <fd>]
// -- qemu ...`. The wrapper pins each macvtap's MAC (matching qemu's mac=), brings it up,
// opens its /dev/tap<ifindex> chardev on the given fd, then execs qemu, which inherits the
// fds referenced by the `fd=` netdevs. The witness NIC is never listed here -- it is a plain
// tap qemu opens by name. Triples are separate argv words (not delimited), so a MAC's colons
// never need escaping.
func launchExec(s QEMUSpec) []string {
	qemu := append([]string{s.Binary}, qemuArgs(s)...)
	if s.NetMode != NetMacvtap {
		return qemu
	}
	exec := []string{s.NetWrapBin}
	if s.SystemTap != "" {
		exec = append(exec, s.SystemTap, s.SystemMAC, strconv.Itoa(sysFD))
	}
	if s.ServiceTap != "" {
		exec = append(exec, s.ServiceTap, s.ServiceMAC, strconv.Itoa(svcFD))
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

// GuestUnit is the transient systemd service the guest VM runs as. The guest is
// launched *by the systemd manager* (via systemd-run), so it lands in its own
// cgroup, a sibling of briard-agent.service — a `systemctl restart briard-agent`
// kills only the agent's cgroup and leaves the guest serving. The
// agent stays the guest's sole *policy* supervisor (`-p Restart=no`), re-adopts
// the running guest over the persisted control socket after a restart, and stops
// it only for the self-fence VM-destroy backstop. (A `systemd-run --scope` was
// tried first and failed the detach test — its payload stayed in the caller's
// cgroup and died with the agent.)
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

// Launch boots the guest as a transient systemd service (systemd-run) and waits
// until QEMU has created the control socket. The unit is detached from the agent's
// cgroup, so it outlives an agent restart. Note the socket exists
// before the guest OS boots, so readiness of the *guest agent* is established later,
// when a call over the channel first succeeds (guestagent.BringUp). ctx bounds only
// the brief systemd-run start call + the socket wait, NOT qemu's lifetime — binding
// qemu to the agent's ctx would re-couple the lifecycles.
func Launch(ctx context.Context, s QEMUSpec) (*Guest, error) {
	unit := s.unit()
	if s.NetMode == NetMacvtap && s.NetWrapBin == "" {
		return nil, fmt.Errorf("platform: NetMacvtap requires NetWrapBin (the fd-passing launch wrapper)")
	}
	args := []string{
		"--unit=" + unit,
		"--collect", // GC a prior dead instance so re-launch doesn't hit a lingering failed unit
		"-p", "Restart=no",
		"-p", "Description=Briard guest VM (" + unit + ")",
		"--",
	}
	args = append(args, launchExec(s)...)
	if err := secureQMPDir(s.QMPSock); err != nil {
		return nil, err
	}
	if out, err := exec.CommandContext(ctx, "systemd-run", args...).CombinedOutput(); err != nil {
		return nil, fmt.Errorf("platform: systemd-run guest: %w: %s", err, out)
	}
	if err := waitForSocket(ctx, s.ControlSock); err != nil {
		_ = stopUnit(unit)
		return nil, err
	}
	return &Guest{ControlSock: s.ControlSock, QMPSock: s.QMPSock, unit: unit}, nil
}

// secureQMPDir makes the monitor socket's directory owner-only, creating it if needed.
// Anyone who can connect to QMP owns the VM outright (reset it, stop it, dump its RAM to a
// file), and QEMU itself creates the socket 0755 -- so this is the containment, applied
// BEFORE qemu is started rather than after, and applied on every launch so an existing
// directory with looser modes is tightened rather than trusted.
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
	return unitActive(s.unit())
}

func unitActive(unit string) bool {
	out, _ := exec.Command("systemctl", "is-active", unit).Output()
	return strings.TrimSpace(string(out)) == "active"
}

// Stop terminates the guest VM by stopping its transient service (the self-fence
// VM-destroy backstop). It is NOT called on a normal agent shutdown — an agent
// restart must be transparent to the guest. No-op-safe if gone.
func (g *Guest) Stop() error {
	if g == nil || g.unit == "" {
		return nil
	}
	return stopUnit(g.unit)
}

func stopUnit(unit string) error {
	if out, err := exec.Command("systemctl", "stop", unit).CombinedOutput(); err != nil {
		return fmt.Errorf("platform: stop %s: %w: %s", unit, err, out)
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
