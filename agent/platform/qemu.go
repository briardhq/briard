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
	Binary string // qemu-system-x86_64
	Accel  string // e.g. "kvm:tcg"
	// CPUModel is qemu's `-cpu`. Empty = qemu's default, which on x86_64 is `qemu64`:
	// a 1990s-baseline model BELOW x86-64-v2 (60 CPUID features against the host's 190,
	// measured on the shipped qemu 10.2). It has no aes, sha-ni, pclmulqdq, ssse3, sse4.1/4.2,
	// avx*, bmi1/2, popcnt, rdrand, rdtscp, xsave, invpcid, pdpe1gb, tsc-deadline or pmu --
	// so guest TLS, nix's sha256, OCI digest checks and btrfs/DRBD crc32c all run the software
	// path, and the guest kernel cannot apply Spectre/SSBD mitigations at all because the
	// spec-ctrl/ibpb/stibp bits are not advertised. Passing the host's real CPU through costs
	// nothing here: the usual reason not to is live migration or a RAM-bearing savevm, and this
	// product does NEITHER -- a guest never moves hosts, and every snapshot it takes is
	// disk-only (snapshot.go, and QMP blockdev-snapshot-internal-sync). An HA failover is a
	// fresh boot on the other node, which re-reads CPUID.
	//
	// `max`, not `host`: under KVM the two expand to an identical feature set (verified by
	// query-cpu-model-expansion), but `host` HARD-FAILS without KVM ("CPU model 'host' requires
	// KVM") and Accel is a fallback LIST -- kvm:tcg. A bare `host` would turn every no-virt host
	// from slow-but-booting into not-booting, which is exactly the case the tcg fallback exists for.
	CPUModel    string
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
	args := []string{"-machine", "accel=" + s.Accel}
	if s.CPUModel != "" {
		args = append(args, "-cpu", s.CPUModel)
	}
	args = append(args,
		"-m", strconv.Itoa(s.MemoryMB),
		"-smp", strconv.Itoa(s.Cores),
		// EVERY RESTART GOES THROUGH THE SUPERVISOR. `-no-reboot` makes QEMU exit on a guest
		// reset rather than quietly starting the machine again inside the same process, so a
		// guest that reboots itself surfaces as a unit that STOPPED -- an event the agent can
		// see, count and act on -- instead of as a gap in the control channel indistinguishable
		// from every other gap. The agent is the guest's sole policy supervisor (`-p Restart=no`,
		// see GuestUnit), and a supervisor that cannot observe its charge restarting is not
		// supervising it.
		//
		// Two properties follow, both relied on elsewhere. The boot selector stays genuinely
		// ONE-SHOT: `-smbios briard_boot=staging` is armed per LAUNCH (see BootStaging), so a
		// guest that reset in place would come up on the staged generation a second time, when
		// the whole point is that the next launch is the host's decision. And a guest that
		// panics its way through boot exits rather than spinning invisibly, which is what lets
		// the recovery loop damp a crash loop instead of racing one.
		//
		// The cost is that the guest cannot complete its own reboot -- it needs the agent to
		// relaunch it. That is exactly why the recovery loop checks this unit's state before
		// anything else (host/guestrecover.go): a stopped unit is relaunched at once, with no
		// window and no gate, because a VM that is not running can neither heal itself nor
		// outage a peer.
		//
		// (Recorded 2026-08-12. The flag was here from the first commit with no comment and no
		// doc anywhere; the above is the justification it never had, arrived at by asking what
		// would actually break without it.)
		"-no-reboot",
		"-display", "none",
		// The host<->guest control channel: a virtio-serial port named
		// guestagent.ControlPort, backed by a host unix socket QEMU serves.
		"-device", "virtio-serial-pci",
		"-chardev", "socket,id=briardctl,path="+s.ControlSock+",server=on,wait=off",
		"-device", "virtserialport,chardev=briardctl,name="+guestagent.ControlPort,
	)
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
		// Capture the guest's ttyS0 console (kernel + systemd) for debugging -- APPENDING, because
		// `-serial file:` truncates on open and this guest is relaunched routinely: every OS
		// upgrade reboots it, every agent restart re-launches it, and a rollback launches the
		// PREVIOUS generation. Truncating means the console you need is the one just overwritten
		// by the boot that replaced it, so the log reliably holds every incarnation except the
		// interesting one. Measured while debugging a promotion that failed on an earlier boot
		// than the log could still show.
		args = append(args, "-chardev", "file,id=serial0,path="+s.SerialLog+",append=on",
			"-serial", "chardev:serial0")
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
	// (system + service present), and install.sh sets all three on EVERY install --
	// so eth1/eth2/eth3 is the only shape any shipped node has booted with. Which NIC
	// carries the VIP is the agent's to say (net.configure); nothing is baked
	// guest-side, so there is no default for a positional shape to be correct against
	// ([V3b.16a] deleted the last one).
	//
	// ⚠️ Omitting SystemTap slides the service NIC down to eth1 AND the witness NIC to
	// eth2 -- where the guest's baked 10.9.9.2 is not, so the private link silently
	// fails to exist and with it the reboot gate and the host's route to the VIP
	// ([V3b.19]). A caller that wants any of those must set all three.
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

// guestStopTimeout is the guest unit's TimeoutStopSec. Above GuestShutdownGrace on purpose: the
// ExecStop gives up first and says so, and only then does systemd start killing. The other order
// would put a SIGKILL through the middle of a shutdown that was still making progress, which is
// the power cut this whole arrangement exists to avoid.
const guestStopTimeout = 75 * time.Second

// launchArgs renders the systemd-run command line for the guest unit. Split out of Launch so the
// unit's stop contract -- the part with no observable effect until the machine is going down --
// is assertable without a systemd.
//
// ExecStop is what makes a HOST SHUTDOWN survivable, and its absence was the defect. Stopping
// this unit SIGTERMs QEMU, which is a power cut to the guest (see Guest.Shutdown); systemd stops
// every unit when the host reboots; so before this, a user rebooting their own machine for their
// own distro's updates power-cut the appliance every time, losing whatever the payload had not
// itself flushed. The OS-upgrade path had already been fixed for exactly this and only for
// itself.
//
// It belongs on the UNIT rather than in the agent's own SIGTERM handler, and that is the crux
// rather than a preference: host.Run carries an explicit "no defer g.Stop()" because an agent
// restart must be transparent to the guest, and the agent cannot tell "I am being restarted"
// from "the machine is going down". systemd can, and does: it stops this unit for the second and
// not for the first.
//
// No ExecStop without a monitor socket -- there is then no way to ask, and a unit that promises a
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
	return append(append(args, "--"), launchExec(s)...)
}

// shutdownBin is the absolute path ExecStop invokes: this very binary, which grows a
// --guest-shutdown mode for the purpose. /proc/self/exe is read at LAUNCH time, before any
// self-update could have renamed it, and under install.sh it resolves to the committed
// /var/lib/briard/briard-agent -- the stable path, so a later update swaps the file under a unit
// line that stays correct. (The uncovered corner is launching a guest during a self-update TRIAL
// boot, where it resolves to briard-agent.next and a commit renames that away. systemd then
// fails the ExecStop and falls through to SIGTERM, i.e. exactly the old behaviour: a lost
// improvement, never a new failure.)
func shutdownBin() string {
	bin, err := os.Executable()
	if err != nil {
		return ""
	}
	return bin
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
	args := launchArgs(s)
	if err := secureQMPDir(s.QMPSock); err != nil {
		return nil, err
	}
	// The NAME has to be free before systemd-run will take it, and "not running" is not yet
	// "free" -- see waitUnitFree.
	if err := waitUnitFree(ctx, unit, unitFreeGrace); err != nil {
		return nil, err
	}
	if out, err := exec.CommandContext(ctx, "systemd-run", args...).CombinedOutput(); err != nil {
		return nil, fmt.Errorf("platform: systemd-run guest: %w: %s", err, out)
	}
	if err := waitForSocket(ctx, s.ControlSock); err != nil {
		// FORCE, not the graceful stop: this is a guest that never even produced its control
		// socket, so asking it to power itself off politely spends the whole ExecStop grace
		// waiting for an ACPI reply from something that has not finished booting -- inside a
		// failure path that the bring-up budget is already counting against.
		_ = forceStopUnit(unit)
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

// unitState reads a unit's ActiveState. systemctl answers for units that do not exist too
// (an absent unit is "inactive", exit 0), so the error return means systemctl itself failed
// -- which is not an answer about the unit and must not be read as one.
func unitState(unit string) (string, error) {
	out, err := exec.Command("systemctl", "show", "-p", "ActiveState", "--value", unit).Output()
	return strings.TrimSpace(string(out)), err
}

// unitAtRest reports whether an ActiveState reading means the unit is FINISHED -- gone or
// dead, with nothing of it still running -- as distinct from merely not-"active".
//
// The distinction is the whole of [B.103]. A transient unit spends its ExecStop in
// "deactivating", which is not "active"; a caller that asks `is-active` therefore hears
// "stopped" about a guest that is still flushing, and the next Launch trips over the unit it
// was told had gone ("already loaded or has a fragment file"), rolling back a good upgrade.
// Measured: the collision landed 2s into a stop that took 19s in the adjacent run.
//
// It names the states that MEAN at-rest rather than excluding the ones that mean running, so
// an unfamiliar reading -- a systemd that grew a state, an empty line from a systemctl that
// failed -- keeps the caller waiting instead of declaring a running guest gone. Waiting costs
// a bounded grace and reports what it saw; the other direction costs the upgrade.
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

// unitFreeGrace bounds the wait for a stopping guest's unit name to come free. Above the unit's
// own TimeoutStopSec, because that is the longest a stop can legitimately take before systemd
// gives up and kills it -- and comfortably inside the default bring-up budget, which is what
// this wait is spending.
const unitFreeGrace = guestStopTimeout + 15*time.Second

// unitLoaded reports whether a LoadState reading means the unit NAME is still taken. systemd-run
// refuses a name that is loaded ("already loaded or has a fragment file"), and a unit stays
// loaded for its whole stop and until systemd garbage-collects the corpse -- so a name is free
// only once systemctl has forgotten the unit entirely.
//
// Like unitAtRest, it names the reading that means GONE rather than excluding the ones that mean
// present: an unfamiliar answer, or a systemctl that failed, keeps the caller waiting instead of
// launching into a name that is still taken.
func unitLoaded(state string, err error) bool {
	return err != nil || state != "not-found"
}

// waitUnitFree blocks until the transient unit name can be created again.
//
// It is [B.103]'s distinction one step earlier, and the step that was missing. The relaunch
// decision asks `is-active`, so a unit spending its ExecStop reads as *not running* — true, and
// not the same as *creatable*: systemd-run then trips over the unit that is still there, which
// on the guest-recovery ladder spent all three relaunch attempts inside one second on a
// condition that clears by itself in a few ([V3b.18]).
//
// reset-failed is the one prod that helps: a unit that has finished is garbage-collected on its
// own (`--collect`), but a corpse someone still references is not, and clearing it is free.
// Asked only of a unit at rest — reset-failed says nothing to a unit that is still stopping.
func waitUnitFree(ctx context.Context, unit string, grace time.Duration) error {
	deadline := time.Now().Add(grace)
	for first := true; ; first = false {
		if !unitLoaded(unitLoadState(unit)) {
			return nil
		}
		// Asked ONCE, and only once the name is known to be taken: if WE are the invocation
		// holding it, waiting is not slow but impossible, and saying so beats spending the
		// whole grace to say nothing ([B.110]).
		if first && insideUnit(unit) {
			return fmt.Errorf("platform: refusing to wait for unit %s to come free: this process is "+
				"part of that unit's own invocation, so the name cannot be released until we return", unit)
		}
		if unitStopped(unit) {
			_ = exec.Command("systemctl", "reset-failed", unit).Run()
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

// insideUnit reports whether THIS process is part of unit's current invocation -- systemd
// spawned it as an ExecStop, an ExecStopPost, or anything else inside the unit.
//
// It exists because a unit's stop job asking for that unit's own name to come free is not a
// slow wait but a closed loop: the name is held BY the caller, so the only thing that could
// release it is the return this call is blocking. Nothing about it looks wrong from inside --
// every state the wait reads is legitimately "still stopping" -- so it spends the caller's
// whole budget in silence and the guest gets power-cut at the far end ([B.110]).
//
// The seam is $INVOCATION_ID, which systemd exports to every process it starts for a unit and
// which the unit itself answers with for as long as that invocation lasts. Measured, from
// inside an ExecStop: both sides read the same id, and the unit reads LoadState=loaded /
// ActiveState=deactivating -- i.e. exactly the readings waitUnitFree treats as "keep waiting".
//
// Every uncertain answer is NOT-inside: an empty environment (nothing started us as a unit), a
// systemctl that failed, an id that does not match. The guard only ever converts a
// guaranteed-hopeless wait into an immediate error, so being wrong about it costs the bounded
// wait we would have done anyway -- never a launch that would otherwise have succeeded.
func insideUnit(unit string) bool {
	id := os.Getenv("INVOCATION_ID")
	if id == "" {
		return false
	}
	out, err := exec.Command("systemctl", "show", "-p", "InvocationID", "--value", unit).Output()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) == id
}

// unitLoadState reads a unit's LoadState ("loaded" / "not-found"). As with unitState, an error
// means systemctl itself failed and is not an answer about the unit.
func unitLoadState(unit string) (string, error) {
	out, err := exec.Command("systemctl", "show", "-p", "LoadState", "--value", unit).Output()
	return strings.TrimSpace(string(out)), err
}

// Stop terminates the guest VM by stopping its transient service (the self-fence
// VM-destroy backstop). It is NOT called on a normal agent shutdown — an agent
// restart must be transparent to the guest. No-op-safe if gone.
//
// It KILLS QEMU first, and had to start doing so once the unit grew an ExecStop. Every caller of
// this is a caller that wants the VM gone now and cannot wait: the self-fence backstop reaches
// for it on a guest that is already wedged, and the rollback leg on a guest whose disk is about
// to be reverted out from under it. A graceful ACPI powerdown asks such a guest a question it
// will not answer, so going through ExecStop would buy nothing and cost a full TimeoutStopSec on
// the two paths least able to spend it. Killing the main process first means the unit deactivates
// on its own and the stop below is bookkeeping.
func (g *Guest) Stop() error {
	if g == nil || g.unit == "" {
		return nil
	}
	return forceStopUnit(g.unit)
}

// forceStopUnit takes the guest down NOW: SIGKILL the cgroup, then stop the unit to reap it.
// The kill is what makes it immediate — with an ExecStop on the unit, a plain stop is a request
// for a clean powerdown, and every caller here is one that cannot wait for one.
func forceStopUnit(unit string) error {
	// Best-effort: a unit that is already gone makes this fail, which is not worth reporting --
	// stopUnit below is what decides whether the guest is really down.
	_ = exec.Command("systemctl", "kill", "--signal=SIGKILL", unit).Run()
	return stopUnit(unit)
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
