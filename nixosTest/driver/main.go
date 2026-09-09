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
	"briard.io/agent/guestagent"
	"briard.io/agent/host"
	"briard.io/agent/platform"
	"briard.io/shared/nodestorage"
)

// vipHealth is where the service answers on the harness L2 (guest-image bakes the VIP).
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
		QMPSock: os.Getenv("QMP_SOCK"), // empty = no monitor, as for every other test
	})
	if err != nil {
		log.Fatalf("launch: %v", err)
	}
	defer guest.Stop()

	node := env("NODE", "guest")
	res := drbd.Resource{
		Name: "r0", Device: "/dev/drbd0",
		// Single node: majority-of-1 is quorate, so the reactor promotes.
		Peers: []drbd.Peer{{Name: node, NodeID: 0, Address: "127.0.0.1:7789", Disk: drbd.DataDevice}},
	}
	// THE PRODUCT'S OWN RENDERER ([V3b.33](d)). The storage spec is the host's to compose, and
	// this driver is the harness standing in for the host -- so it calls host.StorageSpec rather
	// than assembling a spec of its own, and a rig therefore exercises the real composition.
	// DATA_ENCRYPTION reaches it the same way it reaches the agent, which is what lets a rig ask
	// for `off` or `adiantum` without a second code path.
	storage, err := host.Config{
		Node:           node,
		DataEncryption: nodestorage.Mode(env("DATA_ENCRYPTION", string(nodestorage.ModeAuto))),
	}.StorageSpec(res, false, true) // the test always starts from a blank data disk
	if err != nil {
		log.Fatalf("storage spec: %v", err)
	}
	spec := guestagent.BringUpSpec{
		Storage: storage,
		// The ordered unit: data mount -> converge -> VIP claim. The same three units on every
		// node whatever is installed ([V3b.3](e2)) -- what a node runs comes off the VOLUME at
		// promotion, so the chain has nothing to vary with.
		Promoter: promoterUnits(),
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

	bringup, cancel := context.WithTimeout(ctx, 5*time.Minute)
	if err := g.BringUp(bringup, spec); err != nil {
		log.Fatalf("bring-up: %v", err)
	}
	if err := g.WaitPrimary(bringup, spec.Storage.Resource.Name, guestagent.DefaultPollInterval); err != nil {
		log.Fatalf("bring-up wait-primary: %v", err)
	}
	cancel()
	fmt.Println("CONVERGED") // the testScript waits for this marker

	<-ctx.Done() // hold the guest alive until signalled
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

// promoterUnits is the ordered promoter chain. It mirrors nixosTest/lib.nix's promoterSnippet
// and the host agent's promoterUnits, both of which are the same constant: the services a node
// runs are read off the volume by briard-services AFTER promotion, so no member varies with
// them. The front door is not a member -- it rides briard-vip (wantedBy + partOf), so it tracks
// the primary regardless.
func promoterUnits() []string {
	return []string{"briard-primary-storage.service", "briard-services.service", "briard-vip.service"}
}
