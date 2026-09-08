// Command briard-guest-agent is the in-guest half of Briard: the control agent that serves the
// host over the virtio-serial channel, the host-agent deadman, and the converge step
// drbd-reactor runs at promotion. It is its OWN main ([B.137]) so that what the guest links is
// an import graph, not a build tag: this package imports guestagent and what guestagent needs,
// and nothing of the host -- no channel fetcher, no self-update layout, no CLI, no report card,
// no QEMU launcher. `internal/arch` asserts the fence, and the flake hashes exactly this graph
// to decide when the guest image changed ([B.86i]).
//
// The argv is the contract the image's units and the pushed bundle share (guest-image/
// disk-image.nix, configuration.nix, [B.86j]'s picker passes it through verbatim):
//
//	briard-guest-agent run --guest      serve the host (Type=notify; READY once the port is open)
//	briard-guest-agent run --deadman    the host-agent watchdog, its own unit
//	briard-guest-agent --converge       render, warm and start every service the volume names
//	briard-guest-agent --converge-stop  stop those units (briard-services' ExecStop)
//	briard-guest-agent --test-launch    the cheap self-test a staged copy passes before it is
//	                                    trialled ([B.138]): execs, parses, sees the port device
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"briard.io/agent/guestagent"
	"briard.io/agent/guestfirmware"
	"briard.io/shared/sdnotify"
)

func main() {
	args := os.Args[1:]
	if len(args) > 0 && args[0] == "run" {
		runDaemon(args[1:])
		return
	}
	if len(args) > 0 && strings.HasPrefix(args[0], "-") {
		runInternal(args)
		return
	}
	fmt.Fprintln(os.Stderr, "briard-guest-agent: the in-guest agent; invoked by the guest's units as `run --guest`, `run --deadman`, `--converge` or `--converge-stop`")
	os.Exit(2)
}

// runDaemon is `run --guest` or `run --deadman`: the two long-running modes. There is no default
// mode here -- the host agent is a different binary -- so a bare `run` is refused rather than
// guessed.
func runDaemon(args []string) {
	fs := flag.NewFlagSet("briard-guest-agent run", flag.ExitOnError)
	guest := fs.Bool("guest", false, "serve the host over the virtio-serial control channel")
	deadman := fs.Bool("deadman", false, "run the host-agent deadman")
	_ = fs.Parse(args)

	// SIGTERM/SIGINT cancels the context so a `systemctl stop` is a clean shutdown rather than a
	// kill. Installing this handler removes Go's default "SIGTERM terminates the process": every
	// path under this context is responsible for noticing cancellation itself, and a path parked
	// in a blocking syscall notices nothing -- which is why runGuest closes its port and holds a
	// deadline.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	// Take the notify socket out of the environment before anything is exec'd, so the children
	// this agent spawns (systemctl, podman, drbdadm) cannot inherit it; Ready() keeps the socket.
	sdnotify.Adopt()

	switch {
	case *deadman:
		// The deadman runs as its OWN guest service (briard-deadman), decoupled from the
		// per-connection guest agent, which crash-loops while the host is down.
		if err := guestagent.RunDeadman(ctx); err != nil {
			log.Fatalf("deadman: %v", err)
		}
	case *guest:
		if err := runGuest(ctx); err != nil {
			log.Fatalf("guest agent: %v", err)
		}
	default:
		fmt.Fprintln(os.Stderr, "briard-guest-agent run: say which one: --guest or --deadman")
		os.Exit(2)
	}
}

// runInternal is the flag-shaped surface a unit file invokes: converge-at-promotion, in the
// guest ([V3b.3](f)). briard-services.service runs these as a promoter CHAIN MEMBER between the
// data mount and the VIP, so the exit code is load-bearing: a non-zero ExecStart fails the whole
// promotion, the node never claims the VIP, and a primary with no address is already reported
// unhealthy. Converge failing is a node that cannot serve, and it must say so rather than
// promote into a broken state. (A SERVICE that fails to start is a different thing and never
// reaches here; Converge logs it and returns nil.)
func runInternal(args []string) {
	fs := flag.NewFlagSet("briard-guest-agent", flag.ExitOnError)
	converge := fs.Bool("converge", false, "render, warm and start every service the replicated volume names, then exit -- briard-services.service's ExecStart")
	convergeStop := fs.Bool("converge-stop", false, "stop the service units this node converged to -- briard-services.service's ExecStop")
	testLaunch := fs.Bool("test-launch", false, "the push protocol's cheap self-test ([B.138]): check what a staged copy can check without the port, then exit 0")
	_ = fs.Parse(args)

	if *testLaunch {
		// What a staged agent can prove without the running agent's port: it execs on this
		// kernel and libc (we are here), its flags parse (they did), and the control port device
		// the real start will open exists. The running agent holds the port, so opening it is
		// not part of the test.
		if _, err := os.Stat(guestfirmware.ControlPortDev); err != nil {
			fmt.Fprintf(os.Stderr, "briard-guest-agent: test launch: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("briard-guest-agent: test launch ok")
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	if *converge || *convergeStop {
		// Converge's skip list is DROPPED here on purpose: this is drbd-reactor's call, and a
		// service it could not prepare must not fail the unit -- briard-services is a chain
		// member, so exiting non-zero would take the promotion down over one service's
		// packaging. Converge has already logged which one and declined to start it. The install
		// path reads the list through the verb, where failing IS the right answer.
		run, what := func(ctx context.Context, x guestagent.Executor) error {
			_, err := guestagent.Converge(ctx, x)
			return err
		}, "converge"
		if *convergeStop {
			run, what = guestagent.ConvergeStop, "converge-stop"
		}
		if err := run(ctx, guestfirmware.NewOSExecutor()); err != nil {
			log.Fatalf("%s: %v", what, err)
		}
		return
	}
	fmt.Fprintf(os.Stderr, "briard-guest-agent: no internal helper named in %q\n", strings.Join(args, " "))
	os.Exit(2)
}

// runGuest opens the virtio-serial port and serves the guestagent dispatch loop for ONE host
// connection; the unit's Restart=always puts it back on the port for the next.
//
// Cancellation is a deadline, not a hope. A `systemctl stop` lands while Serve is parked in a
// blocking read of the port, which sees no cancellation; closing the port is what unparks it, and
// guestStopGrace bounds the case where even that is not enough -- the host's clean-shutdown
// timing ([B.51], [B.127]) depends on this process actually ending.
func runGuest(ctx context.Context) error {
	// THE PUSH PROTOCOL'S START-TIME DUTY ([B.138]), before the port: a trial start is the
	// verdict on the whole pushed set (the doors' real launch, where they run), and a refused
	// verdict exits here, port never opened, so the host's reconnect meets the committed agent
	// and reads the old release. A non-trial start with a staged set left behind discards it.
	x := guestfirmware.NewOSExecutor()
	if err := guestfirmware.BinStartup(ctx, x, log.Printf); err != nil {
		return err
	}
	conn, err := os.OpenFile(guestfirmware.ControlPortDev, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer conn.Close()

	go func() {
		<-ctx.Done()
		time.AfterFunc(guestStopGrace, func() { os.Exit(0) })
	}()

	// READY at listen ([B.86j]): the unit is Type=notify under the guest's frozen pivot, and its
	// ExecStartPost commits a pushed binary only after this. A pushed agent that cannot open the
	// port never says it, and the next start falls back to the committed one.
	_ = sdnotify.Ready()
	if err := guestagent.ServeStamped(ctx, conn, x); err != nil {
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

// hostAbsentPause is how long a guest agent that found no host on the port waits before exiting
// on a clean EOF, so the unit's restart loop is a slow poll rather than a spin.
const hostAbsentPause = 5 * time.Second

// guestStopGrace is how long a cancelled guest agent may take to unwind before it is ended
// outright. Strictly longer than PowerOffGrace, so a detached os.poweroff can finish its reply
// before the process that carries it is killed ([B.132]).
const guestStopGrace = 5 * time.Second

const _ = uint(guestStopGrace - guestfirmware.PowerOffGrace)
