// Command driver is the nixosTest host driver -- NOT the product agent.
// It boots the real guest disk via QEMU and drives DRBD bring-up over the
// virtio-serial channel, exercising the SAME platform + guestagent code the agent
// uses (only the config source differs: env here, cloud enrollment in prod). The
// nixosTest testScript sets the env and prepares the (writable) disks.
//
// It launches the guest, brings up a single-node resource, waits for the reactor
// to promote it, prints CONVERGED, then holds the guest alive until signalled.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"briard.io/agent/drbd"
	"briard.io/agent/guest"
	"briard.io/agent/guestagent"
	"briard.io/agent/platform"
)

// vipHealth is where the payload answers on the harness L2 (guest-image bakes the VIP).
const vipHealth = "http://192.168.1.100:8080/healthz"

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func atoi(s string, def int) int {
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	return def
}

func main() {
	log.SetFlags(0)
	// Guest lifetime: killed when the testScript stops us (SIGTERM/SIGINT).
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	// The guest unit's ExecStop, which is THIS binary: platform.launchArgs writes that line as
	// `/proc/self/exe --guest-shutdown=<monitor socket>`, and under this harness /proc/self/exe
	// is the driver rather than the agent. So the driver has to answer the flag the agent
	// answers, and for the same reasons -- including NEVER being fatal, since a non-zero exit
	// here makes systemd report the guest unit as having failed to stop.
	//
	// It is load-bearing rather than tidy, because an unanswered flag here is IGNORED and not
	// rejected: systemd then runs a second whole driver as the stop job of the unit it is
	// stopping, and that driver re-enters platform.Launch from inside the very invocation
	// holding the unit's name ([B.110]).
	guestShutdown := flag.String("guest-shutdown", "",
		"power the guest at this QMP socket off cleanly -- the guest unit's ExecStop, not an operator command")
	flag.Parse()
	if *guestShutdown != "" {
		if err := platform.ShutdownVM(ctx, *guestShutdown, platform.GuestShutdownGrace); err != nil {
			log.Printf("guest-shutdown: %v (systemd will stop the VM the hard way)", err)
			return
		}
		log.Printf("guest-shutdown: the guest powered off cleanly")
		return
	}

	sock := env("CONTROL_SOCK", "/run/briard-ctl.sock")
	guest, err := platform.Launch(ctx, platform.QEMUSpec{
		Binary:      env("QEMU", "qemu-system-x86_64"),
		Accel:       env("ACCEL", "kvm:tcg"),
		CPUModel:    env("CPU", "max"), // the agent's default (config.go) -- tests run what prod runs
		MemoryMB:    atoi(os.Getenv("MEMORY_MB"), 2048),
		Cores:       atoi(os.Getenv("CORES"), 2),
		DiskImage:   os.Getenv("GUEST_DISK"), // writable overlay, prepared by the testScript
		DataDisk:    os.Getenv("DATA_DISK"),  // raw backing for the DRBD volume -> guest /dev/vdb
		ControlSock: sock,
		ServiceTap:  os.Getenv("SERVICE_TAP"), // host tap -> guest eth1 (the VIP NIC)
		// SERVICE_MAC pins the guest NIC's MAC. Empty is fine on bridge (qemu's default MAC is
		// unique enough single-node), but macvtap REQUIRES it: the wrapper pins the macvtap device
		// to this MAC so it matches qemu's mac=, else inbound frames to the device's (random) MAC
		// never reach the guest. The real agent derives it (deriveMAC); a driver-based test sets it.
		ServiceMAC: os.Getenv("SERVICE_MAC"),
		// Net substrate: "" (bridge) opens SERVICE_TAP by name; "macvtap" attaches it as a
		// macvtap chardev via the NET_WRAP_BIN fd-passing wrapper. Every driver-based test now
		// sets macvtap (the migration is finished); the passthrough stays so a bridge spike
		// can still be run by unsetting NET_MODE, for as long as the fallback is supported.
		NetMode:    os.Getenv("NET_MODE"),
		NetWrapBin: os.Getenv("NET_WRAP_BIN"),
		SerialLog:  os.Getenv("GUEST_SERIAL"), // capture guest console for debugging (empty = discard)
		// Arm the boot selector for this launch only (SMBIOS type-11 -> grub).
		BootStaging: os.Getenv("BOOT_STAGING") != "",
		QMPSock:     os.Getenv("QMP_SOCK"), // empty = no monitor, as for every other test
	})
	if err != nil {
		log.Fatalf("launch: %v", err)
	}
	defer guest.Stop()

	spec := guestagent.BringUpSpec{
		Resource: drbd.Resource{
			Name: "r0", Device: "/dev/drbd0",
			// Single node: majority-of-1 is quorate, so the reactor promotes.
			Peers: []drbd.Peer{{Name: env("NODE", "guest"), NodeID: 0, Address: "127.0.0.1:7789", Disk: "/dev/vdb"}},
		},
		FreshInit: true, // the test always starts from a blank data disk
		// The ordered unit: data mount -> payload container -> VIP claim. NO_PAYLOAD drops the
		// middle member, which is the SHIPPED shape (the payload slot is optional, and
		// naming a unit that does not exist fails the whole chain and takes the VIP with it).
		// It mirrors what the host agent derives from whether a service is installed, and it is
		// what lets a test that has no interest in a workload boot the artifact a stranger
		// installs rather than a fixture variant of it.
		Promoter: promoterUnits(os.Getenv("NO_PAYLOAD") == ""),
	}

	// One persistent control connection for the whole guest session -- bring-up AND the
	// managed upgrade -- matching the product host agent (agent/host dials once and reuses
	// it for upgrades). The guest agent serves a single connection then exits on its EOF;
	// the old pattern (BringUpGuest closes its conn, then the upgrade dials a fresh one)
	// forced the agent to exit + restart and raced that under nested KVM, hanging the
	// upgrade's reactor.pause (-- a test-harness artifact, not a product bug).
	conn, err := net.Dial("unix", sock)
	if err != nil {
		log.Fatalf("control dial: %v", err)
	}
	defer conn.Close()
	g := guestagent.NewClient(conn)

	// The boot-selector harness. Deliberately BEFORE bring-up and returning early
	// -- this proves one mechanism (which generation a launch comes up on) and nothing else,
	// so it runs with no DRBD, no payload and no upgrade orchestration anywhere near it.
	// Each invocation is one boot; the testScript stops the unit and starts another.
	if os.Getenv("BOOT_SELECT") != "" {
		runBootSelect(ctx, g, os.Getenv("STAGE_BOOT"))
		// This launch owns the guest's whole lifecycle, so it ends the guest
		// itself -- an ACPI power-off over QMP -- instead of being killed and taking QEMU
		// down with it. The deferred Stop is a power cut, and this sequence power-cuts a
		// guest that has just rewritten its own bootloader, which is what made the test
		// flaky before a monitor existed. Shutting down as ordinary work rather than from a
		// signal handler also keeps the shutdown out of systemd's stop job for this unit.
		// Ask the guest OS itself, over the channel we already hold, and confirm by watching
		// the VM disappear. The ACPI button would serve too: this route was chosen because a
		// nested L2 guest was believed to act on neither, and that turned out to be wrong --
		// the guest unit's own ExecStop powers the guest off over QMP in ~2 s under the same
		// nesting. Either way the guest must stop CLEANLY here -- the alternative is killing
		// QEMU moments after the guest rewrote its own bootloader, which made the next boot
		// hang about half the time.
		down, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		if err := g.PowerOff(down); err != nil {
			log.Printf("poweroff request failed: %v", err)
		}
		if err := guest.WaitStopped(down, 25*time.Second); err != nil {
			// A real failure now, not the expected nested outcome it was once thought to be.
			// The fallback is kept because falling through to the deferred Stop leaves the
			// guest idle with its writes flushed, which is a better power cut than an abrupt
			// one -- but the caller ASSERTS the CLEAN_SHUTDOWN marker below, so taking this
			// branch reds the test rather than passing quietly as it used to.
			log.Printf("guest did not stop cleanly, falling back to the power cut: %v", err)
			return
		}
		fmt.Println("CLEAN_SHUTDOWN")
		return
	}

	bringup, cancel := context.WithTimeout(ctx, 5*time.Minute)
	if err := g.BringUp(bringup, spec); err != nil {
		log.Fatalf("bring-up: %v", err)
	}
	if err := g.WaitPrimary(bringup, spec.Resource.Name, guestagent.DefaultPollInterval); err != nil {
		log.Fatalf("bring-up wait-primary: %v", err)
	}
	cancel()
	fmt.Println("CONVERGED") // the testScript waits for this marker

	// This driver does NOT drive OS upgrades. An OS upgrade rolls back to a snapshot of the OS
	// disk, and only the HOST can take or restore one; this launches a guest but is not the host
	// agent. The broken-generation proof lives in the lab rollback demo instead.
	//
	// if handed a target the guest does NOT hold, pull it in over a binary cache
	// and switch to it -- the delivery half always assumed and never had.
	if target := os.Getenv("STAGE_SYSTEM"); target != "" {
		runStage(ctx, g, target, guestagent.StageSource{
			URL: os.Getenv("STAGE_FROM"),
			Key: os.Getenv("STAGE_FROM_KEY"),
		})
	}

	<-ctx.Done() // hold the guest alive until signalled
}

// runBootSelect reports which generation this launch actually booted, and -- when handed a
// closure -- makes that closure bootable first (os.stageboot: a `staging` profile + a
// bootloader reinstall from the running system, with the default left alone).
//
// Two markers, because the two facts are independent: BOOTED names the generation that came
// up, and STAGED_BOOT says the staging profile is now armable. The testScript reads BOOTED
// from three separate launches of this same code -- one to stage, one with the selector, one
// without -- and it is the comparison ACROSS launches that carries the proof.
func runBootSelect(ctx context.Context, g *guestagent.Client, stage string) {
	call, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	sys, err := g.SystemPath(call)
	if err != nil {
		log.Fatalf("SystemPath: %v", err)
	}
	fmt.Printf("BOOTED system=%s\n", sys)

	if stage == "" {
		return
	}
	if stage == sys {
		log.Fatalf("asked to stage %s, which is already booted -- selecting it would prove nothing", stage)
	}
	if err := g.StageBoot(call, stage); err != nil {
		log.Fatalf("stageboot %s: %v", stage, err)
	}
	// Cheap guard on the obvious way to get this wrong: staging must touch the `staging`
	// profile, never the system one, and must not activate. Either mistake moves
	// /run/current-system here. (That the DEFAULT BOOT ENTRY also stayed put is not
	// observable from inside a running guest -- the launch without the selector is what
	// proves it, and that is the testScript's job.)
	if after, err := g.SystemPath(call); err != nil || after != sys {
		log.Fatalf("post-stageboot SystemPath = %q, err=%v; want the unchanged %s", after, err, sys)
	}
	fmt.Printf("STAGED_BOOT system=%s\n", stage)
}

// runStage exercises staging against a real guest: a closure that is NOT on the guest's
// disk is fetched from a binary cache and switched to.
//
// The precondition is the whole point, so it is PROVEN rather than assumed, using a guard
// that already exists: os.components resolves paths INSIDE a closure, so it cannot answer at
// all for one that is absent -- and unlike os.switch it changes nothing while asking. Read-before
// = must fail (the guest really lacks it -> the fetch is real), read-after = must succeed (the
// bytes really landed). Without that first failing read this test would pass just as well
// against a disk that had the closure baked all along -- which is exactly the state the.17
// exists to end.
func runStage(ctx context.Context, g *guestagent.Client, target string, src guestagent.StageSource) {
	call := bounded(ctx)
	cur, err := g.SystemPath(ctx)
	if err != nil {
		log.Fatalf("SystemPath: %v", err)
	}
	if cur == target {
		log.Fatalf("already running %s -- staging it would prove nothing", target)
	}
	// Precondition: the guest does not have it. os.components is the "is it staged?" oracle --
	// it resolves paths INSIDE the closure, so it cannot answer at all for one that is absent,
	// and unlike os.switch or os.stageboot it changes nothing while asking. A SUCCESS here means
	// the closure was already on the disk and this test proves nothing. (It was os.pin until
	// That verb is gone; the property being leaned on is the same one.)
	if err := call(30*time.Second, func(c context.Context) error { _, e := g.Components(c, target); return e }); err == nil {
		log.Fatalf("os.components read %s BEFORE staging -- the closure was already on the disk, so this test proves nothing", target)
	}
	// The fetch. Generous bound: this is the one call that goes to the network.
	if err := call(10*time.Minute, func(c context.Context) error { return g.Stage(c, target, src) }); err != nil {
		log.Fatalf("stage %s from %q: %v", target, src.URL, err)
	}
	// It landed: the same read that could not answer now can.
	if err := call(30*time.Second, func(c context.Context) error { _, e := g.Components(c, target); return e }); err != nil {
		log.Fatalf("os.components still cannot read %s after staging -- the closure did not land: %v", target, err)
	}
	fmt.Println("STAGED")

	// Decide HOW to activate before activating, against a REAL toplevel. The unit
	// tests cover the policy; what only a live guest can prove is that the four boot
	// components are actually readable where we think they are (`readlink` on
	// kernel/initrd/kernel-modules/systemd, `cat` on kernel-params) on both the booted
	// system and a staged closure. The target here differs from the booted generation by one
	// userland file, so the honest answer is `switch` -- if this ever says `reboot`, either
	// the fixture stopped being a userland-only delta or the component reads are lying, and
	// both would make every routine update cost a boot.
	var booted, want guestagent.SystemComponents
	if err := call(30*time.Second, func(c context.Context) (err error) { booted, err = g.Components(c, ""); return }); err != nil {
		log.Fatalf("read booted components: %v", err)
	}
	if err := call(30*time.Second, func(c context.Context) (err error) { want, err = g.Components(c, target); return }); err != nil {
		log.Fatalf("read target components: %v", err)
	}
	if booted.Kernel == "" || booted.KernelParams == "" {
		log.Fatalf("booted components came back empty (%+v) -- the reads are not seeing a real toplevel", booted)
	}
	method, reasons := guest.ActivationFor(booted, want)
	if method != guest.ActivateSwitch {
		log.Fatalf("activation = %s %v for a userland-only delta; want switch", method, reasons)
	}
	fmt.Printf("ACTIVATION method=%s\n", method)

	// And it is usable: activate it and prove the guest now runs it.
	if err := call(5*time.Minute, func(c context.Context) error { return g.Switch(c, target) }); err != nil {
		log.Fatalf("switch to staged %s: %v", target, err)
	}
	var sys string
	if err := call(30*time.Second, func(c context.Context) (err error) { sys, err = g.SystemPath(c); return }); err != nil || sys != target {
		log.Fatalf("post-switch SystemPath = %q, err=%v; want %s", sys, err, target)
	}
	fmt.Printf("STAGED_SYSTEM system=%s\n", sys)
}

// bounded returns a helper that runs one guest call under its own deadline, derived from
// ctx. Every control-channel call wants a bound of its own -- a hung guest must surface as a
// failed step, not a hung driver -- and the bound differs per call (a read is seconds, a
// fetch is minutes). It was a closure inside runStage until the staged broken upgrade needed
// the same thing.
func bounded(ctx context.Context) func(time.Duration, func(context.Context) error) error {
	return func(d time.Duration, f func(context.Context) error) error {
		c, cancel := context.WithTimeout(ctx, d)
		defer cancel()
		return f(c)
	}
}

// payloadUnit is the payload's promoter-managed systemd unit inside the guest (the
// oci-container service). Matches the Promoter target in the bring-up spec.
const payloadUnit = "podman-briard-payload.service"

// promoterUnits is the ordered promoter chain, with or without a payload. It mirrors
// nixosTest/lib.nix's promoterSnippet and the host agent's promoterUnits: the front door is
// not a member either way -- it rides briard-vip (wantedBy + partOf), so it tracks the
// primary regardless.
func promoterUnits(payload bool) []string {
	units := []string{"briard-data.service"}
	if payload {
		units = append(units, payloadUnit)
	}
	return append(units, "briard-vip.service")
}
