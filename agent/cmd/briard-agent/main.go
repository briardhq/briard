// Command agent is the host-side Briard daemon (privileged), and -- with --guest --
// the in-guest control agent that serves the host over the virtio-serial channel.
//
// The host half orchestrates the guest and reports status; drbd-reactor inside the
// guest drives failover.
//
// the host path lives behind the `!guest` build tag (runHost in main_host.go), so
// `go build -tags guest` produces a guest-only binary that never links the host
// subsystems (platform/QEMU launcher, net/http health probing, the TLS stack they pull
// in) -- the binary the guest VM actually ships. The default (untagged) build keeps both.
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"briard.io/agent/cli"
	"briard.io/agent/guestagent"
	"briard.io/agent/reportcard"
)

func main() {
	// `briard` — the operator CLI, a MODE of this binary rather than a second one.
	// A bare first argument (no leading '-') is a subcommand; the daemon modes keep their flags,
	// so install.sh, the systemd unit and every existing test are untouched by this door opening.
	if len(os.Args) > 1 && !strings.HasPrefix(os.Args[1], "-") {
		os.Exit(cli.Main(context.Background(), os.Args[1:], os.Stdout, os.Stderr))
	}

	guest := flag.Bool("guest", false, "run as the in-guest control agent (serve the host channel)")
	deadman := flag.Bool("deadman", false, "run as the in-guest watchdog for the host agent")
	reportCard := flag.Bool("report-card", false, "check whether this machine can run briard, then exit (0 = yes, 1 = no, with reasons)")
	fetchInstall := flag.String("fetch-install", "", "download and verify the signed release into <dir>, then exit (env: BRIARD_CHANNEL_URL, BRIARD_KEYRING)")
	flag.Parse()

	// The machine report card -- the free-local installer's first gate.
	// Pure host inspection (no host subsystems), so it runs on any build; refuses the unfit with
	// the fix named before anything is installed.
	if *reportCard {
		if !reportcard.Run(os.Stdout, os.Getenv("NET_MODE") == "macvtap") {
			os.Exit(1)
		}
		return
	}

	// SIGTERM/SIGINT cancels the context so a `systemctl stop` is a clean shutdown rather than a
	// kill: the host agent stops its guest, and the guest agent leaves its serve loop.
	//
	// Note what installing this handler COSTS, because it is easy to miss: it removes Go's
	// default "SIGTERM terminates the process". From here on, every path under this context is
	// responsible for noticing cancellation itself, and a path parked in a blocking syscall
	// notices nothing at all. That is precisely how the 90-second shutdown stall happened --
	// see runGuest, which now closes its port and holds a deadline for exactly this reason.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	// The installer's signed-artifact fetch (assertion e) -- verify the qemu bundle +
	// guest image against the release keyring before install.sh uses them. Host-only (it pulls
	// in net/http); runFetchInstall is stubbed out of a `-tags guest` build.
	if *fetchInstall != "" {
		if err := runFetchInstall(ctx, *fetchInstall); err != nil {
			log.Fatalf("fetch-install: %v", err)
		}
		return
	}

	if *deadman {
		// The host-agent deadman runs as its OWN guest service (briard-deadman), decoupled from
		// the per-connection guest agent (which crash-loops while the host is down).
		if err := guestagent.RunDeadman(ctx); err != nil {
			log.Fatalf("deadman: %v", err)
		}
		return
	}
	if *guest {
		if err := runGuest(ctx); err != nil {
			log.Fatalf("guest agent: %v", err)
		}
		return
	}
	// Host path: boot the guest, drive bring-up, observe status. runHost is
	// compiled out of a `-tags guest` build (main_guest.go stubs it), so the guest
	// binary doesn't link the host subsystems.
	if err := runHost(ctx); err != nil {
		log.Fatalf("host agent: %v", err)
	}
}

// runGuest opens the virtio-serial port and serves the guestagent dispatch loop.
func runGuest(ctx context.Context) error {
	conn, err := os.OpenFile(guestagent.ControlPortDev, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer conn.Close()

	// Make SIGTERM actually stop this process. Cancelling the context is not enough on
	// its own: the serve loop can only observe cancellation BETWEEN frames, and between them it
	// is parked in a blocking read on the virtio-serial port, which no context can interrupt.
	// The cost was concrete and load-bearing -- the guest did all its real shutdown work in ~25s
	// and then systemd sat on this unit for the full 90s TimeoutStopSec before SIGKILLing it,
	// which was 90 of the 94 seconds an OS reboot upgrade took. Worse, that SIGKILL is exactly
	// the power cut the clean-shutdown path exists to avoid, arriving on the one machine whose job is not
	// losing data.
	//
	// Two mechanisms, because one is fast and the other is certain:
	//
	//   Close unblocks a read already in flight -- measured, it returns "file already closed"
	//   immediately -- but ONLY while the port is registered with Go's runtime poller. Whether a
	//   character device is depends on the runtime's judgement about the fd, which is not ours to
	//   promise, so it cannot be the whole answer.
	//
	//   The deadline covers the case where it is not. A process that was asked to stop and has
	//   not is a process to end: the agent is a stateless request server (every verb synchronous,
	//   nothing buffered across requests), so exiting costs at most one in-flight reply -- and
	//   the alternative it replaces is that same loss 90 seconds later, with a SIGKILL on top.
	//   Exit 0 because a deliberate stop is not a failure; systemd does not restart a unit it is
	//   stopping, and Restart=always already treats the ordinary EOF exit this way.
	go func() {
		<-ctx.Done()
		conn.Close()
		time.AfterFunc(guestStopGrace, func() { os.Exit(0) })
	}()

	// ServeStamped bumps the deadman's contact stamp on each request; the deadman itself runs in
	// its own process (briard-deadman → RunDeadman), decoupled from this connection lifecycle.
	return guestagent.ServeStamped(ctx, conn, guestagent.NewOSExecutor())
}

// guestStopGrace is how long a cancelled guest agent may take to unwind before it is ended
// outright. Long enough for an in-flight verb to finish and answer, short enough that a stop is
// still a stop -- and deliberately far below systemd's 90s TimeoutStopSec, so the agent decides
// its own exit instead of being killed. Not a timeout tuned around a race: the close above is the
// mechanism, and this only runs when the port turns out not to be interruptible.
const guestStopGrace = 5 * time.Second
