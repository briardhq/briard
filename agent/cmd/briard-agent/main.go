// Command agent is the host-side Briard daemon (privileged), and -- with `run --guest` --
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
	"crypto/rand"
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
	"briard.io/agent/subnet"
	"briard.io/shared/flockname"
)

func main() {
	args := os.Args[1:]

	// `run` is the DAEMON, and it is intercepted here rather than in agent/cli because the daemon
	// modes (runHost, runGuest, RunDeadman) cannot live in the CLI package. agent/cli documents it
	// in its command table all the same, so the help lists it ([V3b.23]).
	if len(args) > 0 && args[0] == "run" {
		runDaemon(args[1:])
		return
	}

	// Internal plumbing, still flags on purpose: a pipeline or a unit file invokes each of these,
	// nobody types them, and promoting them to verbs would put them in a help written for a
	// household. A leading '-' that is not a help request means one of them.
	if len(args) > 0 && strings.HasPrefix(args[0], "-") && args[0] != "-h" && args[0] != "--help" {
		runInternal(args)
		return
	}

	// Everything else — including no arguments at all, which prints the help — is the CLI.
	os.Exit(cli.Main(context.Background(), args, os.Stdout, os.Stderr))
}

// runDaemon is `briard run [--guest|--deadman]`: the long-running process, in one of its three
// modes. The host agent is the default because it is the one a host runs.
func runDaemon(args []string) {
	fs := flag.NewFlagSet("briard run", flag.ExitOnError)
	guest := fs.Bool("guest", false, "run as the in-guest control agent (serve the host channel)")
	deadman := fs.Bool("deadman", false, "run as the in-guest watchdog for the host agent")
	_ = fs.Parse(args)

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

	switch {
	case *deadman:
		// The host-agent deadman runs as its OWN guest service (briard-deadman), decoupled from
		// the per-connection guest agent (which crash-loops while the host is down).
		if err := guestagent.RunDeadman(ctx); err != nil {
			log.Fatalf("deadman: %v", err)
		}
	case *guest:
		if err := runGuest(ctx); err != nil {
			log.Fatalf("guest agent: %v", err)
		}
	default:
		// Host path: boot the guest, drive bring-up, observe status. runHost is
		// compiled out of a `-tags guest` build (main_guest.go stubs it), so the guest
		// binary doesn't link the host subsystems.
		if err := runHost(ctx); err != nil {
			log.Fatalf("host agent: %v", err)
		}
	}
}

// drawTimeout bounds --draw-subnets end to end. It is generous because the flock draw ARP-probes
// the LAN and an unanswered probe costs ~750ms, and it exists because an install must not be able
// to hang on a network question: past this, the draw fails and the installer says so.
const drawTimeout = 60 * time.Second

// runInternal is the flag-shaped surface: helpers an installer, a release pipeline or a unit file
// invokes, each of which does one thing and exits. They are deliberately NOT `briard` verbs —
// see the note at each one.
func runInternal(args []string) {
	fs := flag.NewFlagSet("briard-agent", flag.ExitOnError)
	reportCard := fs.Bool("report-card", false, "check whether this machine can run briard, then exit (0 = yes, 1 = no, with reasons)")
	fetchInstall := fs.String("fetch-install", "", "download and verify the signed release into <dir>, then exit (env: BRIARD_CHANNEL_URL, BRIARD_KEYRING)")
	stageManifest := fs.String("stage-manifest", "", "describe the artifacts in <dir> into <dir>/manifest.json and exit -- the release pipeline's manifest writer")
	mintFlockName := fs.Bool("mint-flock-name", false, "print a fresh random flock name (e.g. brave-elf) and exit -- install.sh uses this once")
	drawSubnets := fs.Bool("draw-subnets", false, "draw this node's two private subnets, checked against this machine's own network, and print them as SYSTEM_SUBNET=/PRIV_SUBNET= -- install.sh uses this once")
	guestShutdown := fs.String("guest-shutdown", "", "power the guest VM at this QMP socket off cleanly, then exit -- the guest unit's ExecStop, not an operator command")
	converge := fs.Bool("converge", false, "IN-GUEST: render, warm and start every service the replicated volume names, then exit -- briard-services.service's ExecStart, not an operator command")
	convergeStop := fs.Bool("converge-stop", false, "IN-GUEST: stop the service units this node converged to -- briard-services.service's ExecStop")
	_ = fs.Parse(args)

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

	// The two private subnets this node numbers itself from. Same category and same argument as
	// --mint-flock-name above: install.sh invokes it, it prints one thing, it exits -- and it is in
	// the BINARY rather than in the shell because the draw is not a random number. It is a table of
	// conventional occupants to avoid, a prefix cut over the host's own routes, and an ARP probe of
	// the candidate, none of which a shell script should be asked to hold twice.
	//
	// It draws unconditionally; the installer owns the decision to CALL it (BRIARD_SYSTEM_SUBNET
	// wins, and a node that already drew keeps what it has). A refusal is fatal on purpose: the
	// alternative is inventing a subnet on a machine that told us it has no room, which installs
	// green and cannot serve half the house.
	if *drawSubnets {
		ctx, cancel := context.WithTimeout(context.Background(), drawTimeout)
		defer cancel()
		d, err := subnet.Pick(subnet.Observe(ctx), rand.Reader, subnet.LANProbe(ctx, reportcard.DefaultRouteNIC()))
		if err != nil {
			log.Fatalf("draw-subnets: %v", err)
		}
		if err := subnet.Report(os.Stdout, d); err != nil {
			log.Fatalf("draw-subnets: %v", err)
		}
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

	// Converge-at-promotion, in the guest ([V3b.3](f)). briard-services.service runs these as a
	// promoter CHAIN MEMBER between the data mount and the VIP, so the exit code is load-bearing:
	// a non-zero ExecStart fails the whole promotion, the node never claims the VIP, and a primary
	// with no address is already reported unhealthy. That is the design -- converge failing is a
	// node that cannot serve, and it must say so rather than promote into a broken state. (A
	// SERVICE that fails to start is a different thing and never reaches here; Converge logs it
	// and returns nil.)
	//
	// Flags rather than `briard` verbs for the same reason as --guest-shutdown: nobody types them.
	// They are plumbing between the agent and a unit file, and drbd-reactor is the only caller.
	if *converge || *convergeStop {
		run, what := guestagent.Converge, "converge"
		if *convergeStop {
			run, what = guestagent.ConvergeStop, "converge-stop"
		}
		if err := run(ctx, guestagent.NewOSExecutor()); err != nil {
			log.Fatalf("%s: %v", what, err)
		}
		return
	}

	// A leading '-' that named none of the above. Not a daemon invocation: since [V3b.23] the
	// daemon is `briard run`, and falling through to it here would resurrect the very "a stray
	// flag silently starts a privileged process" behaviour this recut removed.
	fmt.Fprintf(os.Stderr, "briard: no internal helper named in %q (did you mean `briard run`?)\n", strings.Join(args, " "))
	os.Exit(2)
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
	if err := guestagent.ServeStamped(ctx, conn, guestagent.NewOSExecutor()); err != nil {
		return err
	}

	// Clean EOF: the host end went away. The unit's Restart=always puts this process straight back
	// on a freshly opened port for the next host connection -- but only the FIRST of those restarts
	// is a reconnect. While the host agent is down for good (stopped, not bouncing), the reopened
	// port reports EOF the instant it is read: qemu's chardev is `server=on,wait=off`, and
	// virtio-serial reports "no host attached" as end-of-file rather than by blocking. So exiting
	// immediately is a crash loop -- measured at ~48 restarts in 30s ([B.35]) -- that spams the
	// journal and churns the restart counter for the whole outage without bringing the channel back
	// one second sooner. Pausing here delays a genuine reconnect by at most hostAbsentPause, which
	// the host's own re-dial retry already absorbs, and turns the loop into a slow knock.
	//
	// Deliberately NOT a retry on this same fd: reopening per connection is the shipped behaviour of
	// the one channel the product cannot lose, and this bug is cosmetic. Slow the loop, don't
	// redesign it.
	//
	// CLOSE BEFORE PAUSING, and that ordering is the whole fix. The busy loop this replaced was
	// load-bearing for RE-ADOPT: with the guest port closed, qemu's virtio-serial flow control
	// stops draining the chardev socket, so a returning host agent's handshake request waits in
	// the socket buffer until the next instance opens the port and reads it. Pausing with the port
	// still OPEN reverses that -- qemu hands the frame into a port nobody is reading, and closing
	// the fd on the way out DISCARDS it. The host then waits out its handshake deadline for a reply
	// to a request that no longer exists, drops the connection, EOFs us again, and re-arms the same
	// cycle: `systemctl restart briard-agent` could never re-attach to a live guest (the agent
	// self-update path), which is what agent-readopt caught. The pause costs the host at most
	// hostAbsentPause of delay, which its handshake deadline absorbs; holding the port through it
	// costs the channel outright.
	conn.Close() // hand the port back so qemu buffers the next host request instead of losing it
	select {
	case <-ctx.Done():
	case <-time.After(hostAbsentPause):
	}
	return nil
}

// hostAbsentPause is how long a guest agent that found no host on the port waits before exiting
// into systemd's restart. Short enough that a returning host agent is not kept waiting (its dial
// retries anyway), long enough that a host-down window costs a handful of restarts instead of
// hundreds.
const hostAbsentPause = 5 * time.Second

// guestStopGrace is how long a cancelled guest agent may take to unwind before it is ended
// outright. Long enough for an in-flight verb to finish and answer, short enough that a stop is
// still a stop -- and deliberately far below systemd's 90s TimeoutStopSec, so the agent decides
// its own exit instead of being killed. Not a timeout tuned around a race: the close above is the
// mechanism, and this only runs when the port turns out not to be interruptible.
const guestStopGrace = 5 * time.Second
