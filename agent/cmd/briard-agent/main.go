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
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"briard.io/agent/cli"
	"briard.io/agent/guestagent"
	"briard.io/agent/reportcard"
	"briard.io/shared/flockname"
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
	stageManifest := flag.String("stage-manifest", "", "describe the artifacts in <dir> into <dir>/manifest.json and exit -- the release pipeline's manifest writer")
	mintFlockName := flag.Bool("mint-flock-name", false, "print a fresh random flock name (e.g. brave-elf) and exit -- install.sh uses this once")
	guestShutdown := flag.String("guest-shutdown", "", "power the guest VM at this QMP socket off cleanly, then exit -- the guest unit's ExecStop, not an operator command")
	flag.Parse()

	// Mint the household-visible name. An installer-internal helper rather than a `briard`
	// subcommand: it is not an operator verb, it is the same category as --report-card and
	// --fetch-install (install.sh invokes it, it prints one thing, it exits).
	//
	// It lives in the BINARY rather than in install.sh because the word list is 846 words and a
	// CONTRACT: the cloud admits a claimed name by validating it against that very list
	// (shared/flockname), so a shell copy would be a second list to keep in step -- the
	// cross-boundary drift the pairing tests exist to catch, invented on purpose for no reason.
	if *mintFlockName {
		name, err := flockname.Generate()
		if err != nil {
			log.Fatalf("mint-flock-name: %v", err)
		}
		fmt.Fprintln(os.Stdout, name)
		return
	}

	// The release pipeline's manifest writer, and it is HERE rather than in the shell script that
	// calls it for exactly the reason --mint-flock-name is: the manifest is a CONTRACT between the
	// publisher and every installing node, and it used to have two implementations -- a printf
	// loop in publish-release.sh (hand-assembling JSON, including `"mode":493`, which is 0o755
	// written in decimal by a human) and the struct in agent/install. Writing it with the same
	// code that reads it is what makes the format unable to disagree with itself.
	//
	// Same category as --report-card and --fetch-install: a pipeline invokes it, it does one
	// thing, it exits. It costs nothing in the shipped binary -- sha256 and encoding/json are
	// already linked -- and runStageManifest is stubbed out of a `-tags guest` build, so the
	// guest trim is unaffected.
	if *stageManifest != "" {
		if err := runStageManifest(*stageManifest); err != nil {
			log.Fatalf("stage-manifest: %v", err)
		}
		return
	}

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

	// The guest unit's ExecStop (platform.launchArgs writes the line; systemd runs it). On a host
	// shutdown this is the only thing that gets a chance to power the appliance down cleanly --
	// the alternative is the SIGTERM systemd would otherwise send QEMU, which the guest
	// experiences as a power cut.
	//
	// A flag rather than a `briard` subcommand for the same reason as --report-card and
	// --mint-flock-name: nobody types it. It is plumbing between the agent and a unit file the
	// agent itself wrote.
	//
	// NEVER FATAL, and that is the load-bearing part. A non-zero exit here would make systemd
	// report the guest unit as failed to stop and, worse, could delay the host's own shutdown
	// over a VM that is already gone. Every failure degrades to precisely the old behaviour --
	// systemd kills QEMU after TimeoutStopSec -- so the honest response is to say what happened
	// in the journal and get out of the way.
	if *guestShutdown != "" {
		if err := runGuestShutdown(ctx, *guestShutdown); err != nil {
			log.Printf("guest-shutdown: %v (systemd will stop the VM the hard way)", err)
			return
		}
		log.Printf("guest-shutdown: the guest powered off cleanly")
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
