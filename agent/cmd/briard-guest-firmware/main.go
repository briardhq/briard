// Command briard-guest-firmware is the ONE briard binary the guest image bakes ([B.139]).
//
// It serves the host over the virtio-serial channel and answers exactly five verbs: the
// handshake, the three push verbs the host dresses a guest through, and the clean shutdown.
// Everything else a running node needs -- DRBD bring-up, services, converge, telemetry, the
// deadman -- belongs to `briard-guest-agent`, which the host PUSHES at every bring-up and which
// the guest's pivot then runs in this binary's place (guest-image/pivot.nix). The overlay the
// guest boots on is disposable, so every boot starts here and the host re-dresses it.
//
// WHY IT IS ITS OWN MAIN. The guest image's version is a function of its INPUTS ([B.86i]), and
// the inputs are the Go packages the baked binary links. While the image baked the full agent,
// every agent edit moved that hash and republished a 400 MB guest chain for a change no image
// needed. Baking only the push protocol makes the image move when the PROTOCOL moves, which is
// the honest condition. `internal/arch` asserts flake.nix's guestInputPackages against
// `go list -deps ./agent/cmd/briard-guest-firmware`, so a new import cannot slip outside the hash.
//
// The argv is the contract the image's units and the pushed bundle share (guest-image/
// disk-image.nix, pivot.nix's picker passes it through verbatim):
//
//	briard-guest-firmware run --guest   serve the host (Type=notify; READY once the port is open)
//
// There is deliberately nothing else. A verb outside the five is answered with what is actually
// wrong -- this guest has not been dressed yet -- rather than with a bare "unknown verb".
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"briard.io/agent/guestfirmware"
	"briard.io/shared/sdnotify"
)

func main() {
	args := os.Args[1:]
	if len(args) != 2 || args[0] != "run" || args[1] != "--guest" {
		fmt.Fprintln(os.Stderr, "briard-guest-firmware: the guest image's baked agent; invoked by the guest's unit as `run --guest`")
		os.Exit(2)
	}
	fs := flag.NewFlagSet("briard-guest-firmware run", flag.ExitOnError)
	_ = fs.Bool("guest", false, "serve the host over the virtio-serial control channel")
	_ = fs.Parse(args[1:])

	// SIGTERM/SIGINT cancels the context so a `systemctl stop` is a clean shutdown rather than a
	// kill. Installing this handler removes Go's default "SIGTERM terminates the process": every
	// path under this context is responsible for noticing cancellation itself, and a path parked
	// in a blocking syscall notices nothing -- which is why runGuest closes its port and holds a
	// deadline.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	// Take the notify socket out of the environment before anything is exec'd, so the children
	// this process spawns (systemctl, the staged binaries' --test-launch) cannot inherit it;
	// Ready() keeps the socket.
	sdnotify.Adopt()

	if err := runGuest(ctx); err != nil {
		log.Fatalf("guest firmware: %v", err)
	}
}

// runGuest opens the virtio-serial port and serves the firmware dispatch for ONE host
// connection; the unit's Restart=always puts it back on the port for the next.
//
// Cancellation is a deadline, not a hope. A `systemctl stop` lands while Serve is parked in a
// blocking read of the port, which sees no cancellation; closing the port is what unparks it, and
// guestStopGrace bounds the case where even that is not enough -- the host's clean-shutdown
// timing ([B.51], [B.127]) depends on this process actually ending.
func runGuest(ctx context.Context) error {
	// THE PUSH PROTOCOL'S START-TIME DUTY ([B.138]), before the port. Reaching this binary at all
	// means the picker found no pushed agent, so this start is never a trial -- but it IS the
	// start that follows a first dress whose trial failed, and the aftermath rule is what
	// discards the staged set and puts the doors back on what they ran before.
	x := guestfirmware.NewOSExecutor()
	if err := guestfirmware.BinStartup(ctx, x, log.Printf); err != nil {
		return err
	}
	conn, err := os.OpenFile(guestfirmware.ControlPortDev, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer conn.Close()

	// The same call the pushed agent makes ([B.148]), and for the flags rather than the commit:
	// reaching this binary means the picker found no trial to run, so there is nothing to
	// commit -- but the markers an earlier start left in the tmpfs must still be cleared here,
	// or the next trial's verdict would read a door's stale `trial` as its own.
	guestfirmware.BinCommit(ctx, x, log.Printf)

	go func() {
		<-ctx.Done()
		time.AfterFunc(guestStopGrace, func() { os.Exit(0) })
	}()

	// READY at listen ([B.86j]): the unit is Type=notify under the guest's pivot. The firmware's
	// own start commits nothing -- only a trial start does -- but it says READY at the same
	// point, once the port it exists to serve is open.
	_ = sdnotify.Ready()
	if err := guestfirmware.Serve(ctx, conn, x); err != nil {
		return err
	}

	// A clean EOF: the host disconnected (a host-agent restart, a re-adopt). Hand the port back
	// so qemu buffers the next host request instead of losing it, then pause before exiting --
	// with the host end gone for good the reopened port returns EOF at once, and without this
	// pause Restart=always spun ~48 times in 30 s ([B.35]).
	conn.Close()
	select {
	case <-ctx.Done():
	case <-time.After(hostAbsentPause):
	}
	return nil
}

// hostAbsentPause is how long a firmware that found no host on the port waits before exiting on
// a clean EOF, so the unit's restart loop is a slow poll rather than a spin.
const hostAbsentPause = 5 * time.Second

// guestStopGrace is how long a cancelled firmware may take to unwind before it is ended
// outright. Strictly longer than PowerOffGrace, so a detached os.poweroff can finish its reply
// before the process that carries it is killed ([B.132]).
const guestStopGrace = 5 * time.Second

const _ = uint(guestStopGrace - guestfirmware.PowerOffGrace)
