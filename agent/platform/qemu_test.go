package platform

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
)

func TestQEMUArgsFullSpec(t *testing.T) {
	got := strings.Join(qemuArgs(QEMUSpec{
		Accel:       "kvm:tcg",
		MemoryMB:    2048,
		Cores:       2,
		DiskImage:   "/var/lib/briard/guest.qcow2",
		DataDisk:    "/dev/vg/drbd0",
		ControlSock: "/run/briard/ctl.sock",
		ServiceTap:  "svc0",
		ServiceMAC:  "52:54:00:aa:bb:cc",
		SystemTap:   "sys0",
		SystemMAC:   "52:54:00:11:22:33",
		WitnessTap:  "wit0",
		WitnessMAC:  "52:54:00:de:ad:be",
	}), " ")
	for _, want := range []string{
		"accel=kvm:tcg",
		"-m 2048",
		"-smp 2",
		"-display none",
		"virtio-serial-pci",
		"socket,id=briardctl,path=/run/briard/ctl.sock,server=on,wait=off",
		"virtserialport,chardev=briardctl,name=briard.control",
		"file=/var/lib/briard/guest.qcow2",
		"file=/dev/vg/drbd0",
		"user,id=net0",                                     // eth0: throwaway user-net
		"tap,id=net1,ifname=sys0,script=no,downscript=no",  // eth1: system NIC (DRBD)
		"virtio-net-pci,netdev=net1,mac=52:54:00:11:22:33", // its unique MAC
		"tap,id=net2,ifname=svc0,script=no,downscript=no",  // eth2: service NIC (VIP)
		"virtio-net-pci,netdev=net2,mac=52:54:00:aa:bb:cc", // its unique MAC
		"tap,id=net3,ifname=wit0,script=no,downscript=no",  // eth3: witness NIC (guest<->host)
		"virtio-net-pci,netdev=net3,mac=52:54:00:de:ad:be", // its unique MAC
	} {
		if !strings.Contains(got, want) {
			t.Errorf("qemuArgs missing %q\ngot: %s", want, got)
		}
	}
}

// A data node's two tap NICs must enumerate in a stable order: system on net1
// (eth1, DRBD), service on net2 (eth2, VIP). net1 must precede net2 in the argv,
// so the DRBD NIC is eth1 everywhere (incl. a witness, which has only that NIC).
func TestQEMUArgsTwoNICOrder(t *testing.T) {
	got := strings.Join(qemuArgs(QEMUSpec{Accel: "tcg", ControlSock: "/s", ServiceTap: "svc0", SystemTap: "sys0"}), " ")
	iSys, iSvc := strings.Index(got, "ifname=sys0"), strings.Index(got, "ifname=svc0")
	if iSys < 0 || iSvc < 0 {
		t.Fatalf("both taps must appear:\n%s", got)
	}
	if iSys > iSvc {
		t.Errorf("system NIC (net1/eth1, DRBD) must precede service NIC (net2/eth2, VIP):\n%s", got)
	}
}

// The witness NIC must enumerate last: system (net1/eth1) < service (net2/eth2) <
// witness (net3/eth3), so eth3 is the private guest<->host link on a uniform
// managed guest. Ordering is by -device position (net.ifnames=0).
func TestQEMUArgsWitnessNICOrder(t *testing.T) {
	got := strings.Join(qemuArgs(QEMUSpec{
		Accel: "tcg", ControlSock: "/s", SystemTap: "sys0", ServiceTap: "svc0", WitnessTap: "wit0",
	}), " ")
	iSys, iSvc, iWit := strings.Index(got, "ifname=sys0"), strings.Index(got, "ifname=svc0"), strings.Index(got, "ifname=wit0")
	if iSys < 0 || iSvc < 0 || iWit < 0 {
		t.Fatalf("all three taps must appear:\n%s", got)
	}
	if !(iSys < iSvc && iSvc < iWit) {
		t.Errorf("NIC order must be system(eth1) < service(eth2) < witness(eth3):\n%s", got)
	}
}

// The system NIC alone (a witness) still gets the throwaway net0, and its DRBD tap
// is net1 -> eth1 (no service NIC to precede it, so no placeholder is needed).
func TestQEMUArgsSystemTapOnly(t *testing.T) {
	got := strings.Join(qemuArgs(QEMUSpec{Accel: "tcg", ControlSock: "/s", SystemTap: "sys0"}), " ")
	if !strings.Contains(got, "user,id=net0") || !strings.Contains(got, "tap,id=net1,ifname=sys0") {
		t.Errorf("system-tap-only spec should have net0 (user) + net1 (tap):\n%s", got)
	}
}

// NetMacvtap renders the service + system NICs as fd-attached macvtaps: qemu
// gets `-netdev tap,fd=N` (no ifname=), with the deterministic fd map system->3,
// service->4. MACs still render (pinned onto the macvtap by the launch wrapper).
func TestQEMUArgsMacvtap(t *testing.T) {
	got := strings.Join(qemuArgs(QEMUSpec{
		Accel: "tcg", ControlSock: "/s", NetMode: NetMacvtap,
		SystemTap: "briard-sys", SystemMAC: "52:54:00:11:22:33",
		ServiceTap: "briard-svc", ServiceMAC: "52:54:00:aa:bb:cc",
	}), " ")
	for _, want := range []string{
		"tap,id=net1,fd=3", // system NIC on fd 3
		"virtio-net-pci,netdev=net1,mac=52:54:00:11:22:33", // MAC still pinned
		"tap,id=net2,fd=4", // service NIC on fd 4
		"virtio-net-pci,netdev=net2,mac=52:54:00:aa:bb:cc",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("macvtap qemuArgs missing %q\ngot: %s", want, got)
		}
	}
	// Macvtap mode must NOT open the service/system taps by name.
	if strings.Contains(got, "ifname=") {
		t.Errorf("macvtap mode must not use ifname=:\n%s", got)
	}
}

// Even under NetMacvtap, the witness NIC stays a PLAIN tap (ifname=), never fd=: it
// is the private guest<->host link and macvtap would isolate it.
func TestQEMUArgsMacvtapWitnessStaysTap(t *testing.T) {
	got := strings.Join(qemuArgs(QEMUSpec{
		Accel: "tcg", ControlSock: "/s", NetMode: NetMacvtap,
		SystemTap: "briard-sys", ServiceTap: "briard-svc", WitnessTap: "wit0",
	}), " ")
	if !strings.Contains(got, "tap,id=net3,ifname=wit0,script=no,downscript=no") {
		t.Errorf("witness NIC must stay a plain ifname= tap under macvtap:\n%s", got)
	}
	// The service/system NICs are fd-attached; only the witness uses ifname=.
	if strings.Count(got, "ifname=") != 1 {
		t.Errorf("exactly one ifname= (the witness) expected under macvtap:\n%s", got)
	}
}

// Default (empty NetMode == NetBridge) is unchanged: taps opened by ifname=, no fd=.
func TestQEMUArgsBridgeDefault(t *testing.T) {
	got := strings.Join(qemuArgs(QEMUSpec{
		Accel: "tcg", ControlSock: "/s", SystemTap: "sys0", ServiceTap: "svc0",
	}), " ")
	if strings.Contains(got, "fd=") {
		t.Errorf("default (bridge) mode must not use fd=:\n%s", got)
	}
	if !strings.Contains(got, "ifname=sys0") || !strings.Contains(got, "ifname=svc0") {
		t.Errorf("default (bridge) mode must open taps by name:\n%s", got)
	}
}

// LaunchExec in the default (bridge) mode runs qemu directly: the argv is the binary
// followed by qemuArgs, with no wrapper.
func TestLaunchExecBridge(t *testing.T) {
	got := launchExec(QEMUSpec{Binary: "qemu-system-x86_64", Accel: "tcg", ControlSock: "/s", SystemTap: "sys0"})
	if got[0] != "qemu-system-x86_64" {
		t.Errorf("bridge mode must exec qemu directly, got %q", got[0])
	}
	for _, a := range got {
		if a == "--" {
			t.Errorf("bridge mode must not use the wrapper sentinel:\n%v", got)
		}
	}
}

// LaunchExec in macvtap mode runs the fd-passing wrapper: the wrapper binary, then a
// `<dev> <mac> <fd>` triple per macvtap NIC (system fd 3, service fd 4), then `--`, then
// qemu. The MAC is a plain argv word (its colons need no escaping). The witness NIC never
// appears in the triples (it is a plain tap qemu opens by name).
func TestLaunchExecMacvtap(t *testing.T) {
	got := launchExec(QEMUSpec{
		Binary: "qemu-system-x86_64", Accel: "tcg", ControlSock: "/s",
		NetMode: NetMacvtap, NetWrapBin: "/opt/briard/bin/briard-net-wrap",
		SystemTap: "briard-sys", SystemMAC: "52:54:00:11:22:33",
		ServiceTap: "briard-svc", ServiceMAC: "52:54:00:aa:bb:cc",
		WitnessTap: "briard-wit", WitnessMAC: "52:54:00:de:ad:be",
	})
	sep := -1
	for i, a := range got {
		if a == "--" {
			sep = i
			break
		}
	}
	if sep < 0 {
		t.Fatalf("macvtap launchExec must contain a `--` sentinel:\n%v", got)
	}
	head, qemu := got[:sep], got[sep+1:]
	wantHead := []string{
		"/opt/briard/bin/briard-net-wrap",
		"briard-sys", "52:54:00:11:22:33", "3", // system NIC -> fd 3
		"briard-svc", "52:54:00:aa:bb:cc", "4", // service NIC -> fd 4
	}
	if strings.Join(head, " ") != strings.Join(wantHead, " ") {
		t.Errorf("wrapper head mismatch:\n got %v\nwant %v", head, wantHead)
	}
	if qemu[0] != "qemu-system-x86_64" {
		t.Errorf("wrapper must exec qemu after `--`, got %q", qemu[0])
	}
	// The witness MAC must not appear as a triple word (it is not a macvtap).
	if strings.Contains(strings.Join(head, " "), "briard-wit") {
		t.Errorf("witness NIC must not be in the macvtap triples:\n%v", head)
	}
}

// The boot selector is per-launch and OFF by default. The default matters more
// than the arming does -- every ordinary launch, including the one that rolls a failed
// upgrade back, is the one that must NOT carry the flag.
func TestQEMUArgsBootStaging(t *testing.T) {
	off := strings.Join(qemuArgs(QEMUSpec{Accel: "tcg", ControlSock: "/s", DiskImage: "/d.qcow2"}), " ")
	if strings.Contains(off, "-smbios") {
		t.Errorf("BootStaging unset must pass no -smbios (that IS the rollback):\n%s", off)
	}
	on := strings.Join(qemuArgs(QEMUSpec{Accel: "tcg", ControlSock: "/s", DiskImage: "/d.qcow2", BootStaging: true}), " ")
	if !strings.Contains(on, "-smbios type=11,value="+BootSelectStaging) {
		t.Errorf("BootStaging should render the type-11 OEM string:\n%s", on)
	}
	// The guest's grub matches this string verbatim (guest-image/disk-image.nix); changing
	// it on one side only silently stops selecting, and a silent no-select just boots old.
	if BootSelectStaging != "briard_boot=staging" {
		t.Errorf("BootSelectStaging = %q; the guest grub snippet expects briard_boot=staging", BootSelectStaging)
	}
}

// The monitor is opt-in per launch: a guest that never needs stopping or
// resetting should carry no total-control socket at all, so empty renders nothing.
func TestQEMUArgsQMPSocket(t *testing.T) {
	off := strings.Join(qemuArgs(QEMUSpec{Accel: "tcg", ControlSock: "/s"}), " ")
	if strings.Contains(off, "-qmp") {
		t.Errorf("no QMPSock must mean no monitor at all:\n%s", off)
	}
	on := strings.Join(qemuArgs(QEMUSpec{Accel: "tcg", ControlSock: "/s", QMPSock: "/run/briard/qmp.sock"}), " ")
	// Wait=off: the guest must never block on a supervisor being attached.
	if !strings.Contains(on, "-qmp unix:/run/briard/qmp.sock,server=on,wait=off") {
		t.Errorf("QMPSock should render a server socket that does not wait:\n%s", on)
	}
}

// QMP addresses block devices by id, so the root drive must carry one.
func TestQEMUArgsRootDriveID(t *testing.T) {
	got := strings.Join(qemuArgs(QEMUSpec{Accel: "tcg", ControlSock: "/s", DiskImage: "/var/lib/briard/guest.qcow2"}), " ")
	if !strings.Contains(got, "file=/var/lib/briard/guest.qcow2,if=virtio,media=disk,id="+RootDriveID) {
		t.Errorf("root drive missing id=%s:\n%s", RootDriveID, got)
	}
}

// DataDir renders `-L <dir>` (the bundle's firmware path); empty omits it so a
// distro/Nix qemu keeps its compiled-in default.
func TestQEMUArgsDataDir(t *testing.T) {
	with := strings.Join(qemuArgs(QEMUSpec{Accel: "tcg", ControlSock: "/s", DataDir: "/opt/briard/qemu/share/qemu"}), " ")
	if !strings.Contains(with, "-L /opt/briard/qemu/share/qemu") {
		t.Errorf("DataDir should render -L:\n%s", with)
	}
	without := strings.Join(qemuArgs(QEMUSpec{Accel: "tcg", ControlSock: "/s"}), " ")
	if strings.Contains(without, "-L ") {
		t.Errorf("empty DataDir should omit -L:\n%s", without)
	}
}

// CPUModel renders `-cpu <model>`; empty omits it so qemu keeps its own default. The
// assertion that matters is the ABSENCE of a bare `-cpu host`: `host` requires KVM, Accel is
// a kvm:tcg fallback list, and a host that lands on tcg would then not boot at all.
func TestQEMUArgsCPUModel(t *testing.T) {
	with := strings.Join(qemuArgs(QEMUSpec{Accel: "kvm:tcg", ControlSock: "/s", CPUModel: "max"}), " ")
	if !strings.Contains(with, "-cpu max") {
		t.Errorf("CPUModel should render -cpu:\n%s", with)
	}
	without := strings.Join(qemuArgs(QEMUSpec{Accel: "tcg", ControlSock: "/s"}), " ")
	if strings.Contains(without, "-cpu") {
		t.Errorf("empty CPUModel should omit -cpu:\n%s", without)
	}
}

// Without disks/bridge, no -drive/-netdev appear, but the control channel and
// headless flags always do.
func TestQEMUArgsMinimal(t *testing.T) {
	got := strings.Join(qemuArgs(QEMUSpec{Accel: "tcg", MemoryMB: 512, Cores: 1, ControlSock: "/s"}), " ")
	if strings.Contains(got, "-drive") || strings.Contains(got, "-netdev") {
		t.Errorf("minimal spec should have no -drive/-netdev:\n%s", got)
	}
	if !strings.Contains(got, "name=briard.control") || !strings.Contains(got, "-display none") {
		t.Errorf("minimal spec missing control channel or headless:\n%s", got)
	}
}

// The guest unit's STOP contract, which is the half of the unit with no observable effect until
// the machine is going down -- and so the half that was silently absent. Stopping the unit
// SIGTERMs QEMU, which the guest experiences as a power cut; systemd stops every unit on a host
// reboot; so without an ExecStop, a user rebooting their own host power-cut the appliance.
//
// Three things are asserted and each one fails differently in the field:
//
//   - ExecStop names this binary's --guest-shutdown mode against THIS guest's monitor socket.
//     A wrong socket would power down someone else's VM, or nothing at all, and look identical.
//   - Both properties land BEFORE the "--" separator. After it they are not properties at all,
//     they are extra argv for qemu -- the unit would launch (qemu ignores nothing, it would
//     fail) or, worse, start fine and stop dirty. This ordering is invisible in review.
//   - TimeoutStopSec exceeds the grace the ExecStop itself waits. The other order puts
//     systemd's SIGKILL through the middle of a shutdown still making progress, i.e. it
//     manufactures the power cut the whole mechanism exists to prevent.
func TestLaunchArgsCleanStopContract(t *testing.T) {
	const sock = "/run/briard/qmp/qmp.sock"
	args := launchArgs(QEMUSpec{Accel: "tcg", ControlSock: "/s", QMPSock: sock})

	sep := -1
	var execStop, timeout string
	for i, a := range args {
		switch {
		case a == "--" && sep < 0:
			sep = i
		case strings.HasPrefix(a, "ExecStop="):
			execStop = a
			if sep >= 0 {
				t.Errorf("ExecStop at %d is AFTER the -- at %d; it would be qemu argv, not a unit property", i, sep)
			}
		case strings.HasPrefix(a, "TimeoutStopSec="):
			timeout = a
			if sep >= 0 {
				t.Errorf("TimeoutStopSec at %d is AFTER the -- at %d", i, sep)
			}
		}
	}
	if sep < 0 {
		t.Fatalf("no -- separator in the systemd-run args: %v", args)
	}
	if !strings.HasSuffix(execStop, " --guest-shutdown="+sock) {
		t.Errorf("ExecStop = %q, want it to end with --guest-shutdown=%s", execStop, sock)
	}
	if !strings.HasPrefix(execStop, "ExecStop=/") {
		t.Errorf("ExecStop = %q, want an absolute binary path (systemd requires one)", execStop)
	}
	if want := "TimeoutStopSec=75"; timeout != want {
		t.Errorf("TimeoutStopSec = %q, want %q", timeout, want)
	}
	if guestStopTimeout <= GuestShutdownGrace {
		t.Errorf("TimeoutStopSec (%s) must exceed the ExecStop's own grace (%s), or systemd kills mid-shutdown",
			guestStopTimeout, GuestShutdownGrace)
	}
}

// No monitor socket, no promise. A unit that advertises a clean stop it has no way to perform
// would burn the whole TimeoutStopSec on every stop and then kill QEMU anyway.
func TestLaunchArgsNoMonitorNoStopContract(t *testing.T) {
	args := strings.Join(launchArgs(QEMUSpec{Accel: "tcg", ControlSock: "/s"}), " ")
	if strings.Contains(args, "ExecStop") || strings.Contains(args, "TimeoutStopSec") {
		t.Errorf("a monitor-less guest should carry no stop contract:\n%s", args)
	}
}

// TestUnitAtRest pins the reading that [B.103] turned on: "deactivating" is a unit that is
// still running its ExecStop, and calling it stopped is what let a Launch collide with the
// guest it had just asked to go away. The at-rest set is deliberately small -- anything not
// named here keeps the caller waiting.
func TestUnitAtRest(t *testing.T) {
	for _, tc := range []struct {
		state string
		want  bool
	}{
		{"inactive", true}, // stopped, or never existed -- systemctl says the same for both
		{"failed", true},   // dead, and the unit stays only as a corpse --collect will reap
		{"deactivating", false},
		{"active", false},
		{"activating", false},
		{"reloading", false},
		{"refreshing", false}, // a state this code does not know: wait, do not assume
		{"", false},
	} {
		if got := unitAtRest(tc.state, nil); got != tc.want {
			t.Errorf("unitAtRest(%q) = %v, want %v", tc.state, got, tc.want)
		}
	}
	// A systemctl that failed is not an answer about the unit. Reading its empty output as
	// "inactive" would declare a running guest gone on any transient hiccup.
	if unitAtRest("inactive", errors.New("systemctl: exit status 1")) {
		t.Error("unitAtRest reported at-rest despite a systemctl error")
	}
}

// A unit NAME is free only once systemctl has forgotten the unit: systemd-run refuses a name
// that is still loaded, and a unit stays loaded through its whole stop and past it, until the
// corpse is collected. Reading "not running" as "free" is what spent three relaunch attempts in
// one second on a guest that was still stopping ([V3b.18]).
func TestUnitLoaded(t *testing.T) {
	for _, tc := range []struct {
		state string
		want  bool
	}{
		{"not-found", false}, // systemd has forgotten it -- the only free answer
		{"loaded", true},     // running, stopping, or a corpse: all still hold the name
		{"masked", true},
		{"error", true},
		{"bad-setting", true}, // a reading this code does not know: taken, so the caller waits
		{"", true},
	} {
		if got := unitLoaded(tc.state, nil); got != tc.want {
			t.Errorf("unitLoaded(%q) = %v, want %v", tc.state, got, tc.want)
		}
	}
	// A systemctl that failed is not an answer about the unit. Reading its empty output as
	// "not-found" would launch straight into a name that is still taken -- the failure this
	// wait exists to prevent, reached by trusting a broken systemctl.
	if !unitLoaded("not-found", errors.New("systemctl: exit status 1")) {
		t.Error("unitLoaded reported a free name despite a systemctl error")
	}
}

// The wait is bounded and says what it saw. A unit that never comes free must not hang the
// bring-up budget it is spending, and the two ways to reach the deadline -- a stop that never
// finished, a corpse nobody reaped -- must be told apart in the message.
func TestWaitUnitFreeGivesUpAndNamesTheState(t *testing.T) {
	err := waitUnitFree(context.Background(), "briard-test-nonexistent-unit.service", 0)
	if err == nil {
		t.Skip("systemctl reports this unit as not-found here, so there is nothing to wait for")
	}
	if !strings.Contains(err.Error(), "still loaded") || !strings.Contains(err.Error(), "LoadState") {
		t.Errorf("err = %v, want it to name the unit's LoadState", err)
	}
}

// insideUnit must be able to say YES, or the guard it feeds is decoration -- and the guard is
// the only thing standing between a stop job that asks for its own unit's name and a wait that
// cannot end ([B.110]).
//
// The positive case needs a real invocation to point at, so it BORROWS one that is already
// running on this machine rather than inventing a fixture: an id we made up would prove only
// that two strings compare equal, which is not the claim. No systemd, or no running service to
// borrow from, is a skip -- the honest answer for a machine that cannot host the question.
func TestInsideUnitRecognisesItsOwnInvocation(t *testing.T) {
	unit, id := borrowedInvocation(t)

	t.Setenv("INVOCATION_ID", id)
	if !insideUnit(unit) {
		t.Errorf("insideUnit(%s) = false while holding that unit's own INVOCATION_ID %s", unit, id)
	}
	// The same environment must not claim membership of a DIFFERENT unit: a guard that answers
	// yes to everything would refuse every legitimate wait, which is the expensive direction.
	if insideUnit("briard-test-nonexistent-unit.service") {
		t.Error("insideUnit claimed membership of a unit that does not exist")
	}
	// And a process systemd did not start is inside nothing.
	t.Setenv("INVOCATION_ID", "")
	if insideUnit(unit) {
		t.Errorf("insideUnit(%s) = true with no INVOCATION_ID in the environment", unit)
	}
}

// borrowedInvocation returns a running unit and its current InvocationID, skipping if this
// machine has no systemd to ask or no running service to ask about.
func borrowedInvocation(t *testing.T) (unit, id string) {
	t.Helper()
	out, err := exec.Command("systemctl", "list-units", "--type=service", "--state=running",
		"--no-legend", "--plain").Output()
	if err != nil {
		t.Skipf("no systemctl to ask here: %v", err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		name, _, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok || name == "" {
			continue
		}
		raw, err := exec.Command("systemctl", "show", "-p", "InvocationID", "--value", name).Output()
		if err != nil {
			continue
		}
		if got := strings.TrimSpace(string(raw)); got != "" {
			return name, got
		}
	}
	t.Skip("no running service with an InvocationID to borrow")
	return "", ""
}
