// Command driver is the nixosTest host driver, not the product agent. It boots the real guest
// disk via QEMU and drives one bring-up over the virtio-serial channel, exercising the same
// platform + guestagent code the agent uses, without the agent's standing observe loop. Its
// caller sets the env and prepares the (writable) disks.
//
// It launches the guest, brings up a lone node (one diskful member, so no DRBD), waits until it
// serves, prints CONVERGED, then holds the guest alive until signalled.
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

// vipHealth is where the app answers on the harness L2, at the VIP 192.168.1.100.
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

	// The guest unit's ExecStop is this binary: platform.launchArgs writes that line as the
	// running binary's own path (os.Executable) plus `--guest-shutdown=<QMP socket>`, and under
	// this harness that binary is the driver rather than the agent. So the driver must answer the
	// flag as the agent does, for the same reasons, and is never fatal: a non-zero exit makes
	// systemd report the guest unit as having failed to stop. Load-bearing, not tidy: a driver
	// that ignored the flag would run as a second whole driver in the stop job of the unit it is
	// stopping, re-entering platform.Launch from inside the very invocation holding the unit's name.
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
		DiskImage:   os.Getenv("GUEST_IMAGE"), // the guest image, attached read-only
		DataDisk:    os.Getenv("DATA_DISK"),   // raw backing for the data volume -> guest /dev/vdb
		ControlSock: sock,
		ServiceTap:  os.Getenv("SERVICE_TAP"), // host tap -> guest eth1 (the VIP NIC)
		// SERVICE_MAC pins the guest NIC's MAC. Empty is fine on bridge (qemu's default MAC is
		// unique enough single-node), but macvtap requires it: the wrapper pins the macvtap device
		// to this MAC so it matches qemu's mac=, else inbound frames to the device's (random) MAC
		// never reach the guest. The real agent derives it (deriveMAC); a driver-based test sets it.
		ServiceMAC: os.Getenv("SERVICE_MAC"),
		// Net substrate: "" (bridge) opens SERVICE_TAP by name; "macvtap" attaches it as a
		// macvtap chardev via the NET_WRAP_BIN fd-passing wrapper.
		NetMode:    os.Getenv("NET_MODE"),
		NetWrapBin: os.Getenv("NET_WRAP_BIN"),
		SerialLog:  os.Getenv("GUEST_SERIAL"), // capture guest console for debugging (empty = discard)
		// The VM's monitor socket, which also gives the guest unit its clean ExecStop.
		QMPSock: os.Getenv("QMP_SOCK"), // empty = no monitor
	})
	if err != nil {
		log.Fatalf("launch: %v", err)
	}
	defer guest.Stop()

	node := env("NODE", "guest")
	res := drbd.Resource{
		Name: "r0", Device: "/dev/drbd0",
		// One diskful member: a lone node, so StorageSpec renders no DRBD and the guest
		// starts its chain target instead of drbd-reactor.
		Peers: []drbd.Peer{{Name: node, NodeID: 0, Address: "127.0.0.1:7789", Disk: drbd.DataDevice}},
	}
	// The product's own renderer: the storage spec is the host's to compose and this driver stands
	// in for the host, so it calls host.StorageSpec rather than assembling a spec of its own, and a
	// rig exercises the real composition. DATA_ENCRYPTION reaches it as it reaches the agent, which
	// lets a rig ask for `off` or `adiantum` without a second code path.
	storage, err := host.Config{
		Node:           node,
		DataEncryption: nodestorage.Mode(env("DATA_ENCRYPTION", string(nodestorage.ModeAuto))),
	}.StorageSpec(res, false, true) // the test always starts from a blank data disk
	if err != nil {
		log.Fatalf("storage spec: %v", err)
	}
	spec := guestagent.BringUpSpec{
		Storage: storage,
		// On a lone node only its being non-empty matters: the guest's chain target carries
		// its own member list (shared/chain.Members). See promoterUnits.
		Promoter: promoterUnits(),
	}

	// One persistent control connection for the whole guest session, as the host agent holds
	// one. The guest agent serves a single connection then exits on its EOF, so a connection
	// closed early would restart it under the session.
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
// fetch is minutes).
func bounded(ctx context.Context) func(time.Duration, func(context.Context) error) error {
	return func(d time.Duration, f func(context.Context) error) error {
		c, cancel := context.WithTimeout(ctx, d)
		defer cancel()
		return f(c)
	}
}

// promoterUnits is the promoter list the bring-up spec carries. The product's chain is
// shared/chain.Members(), which the host agent's promoterUnits returns and nixosTest/lib.nix's
// promoterSnippet copies; this list holds only its first three members. It would reach
// drbd-reactor only on a replicated node, and this driver brings up a lone one.
func promoterUnits() []string {
	return []string{"briard-primary-storage.service", "briard-vip.service", "briard-apps.service"}
}
