// Command briard-agent is the host-side Briard daemon (privileged) and the `briard` operator CLI.
// The host half orchestrates the guest and reports status; the in-guest half -- the control
// agent that serves the host over virtio-serial, the deadman, the converge step -- is
// briard-guest-agent, its own main with its own import graph ([B.137]). drbd-reactor inside the
// guest drives failover.
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
	"briard.io/agent/reportcard"
	"briard.io/shared/sdnotify"
)

func main() {
	args := os.Args[1:]

	// `run` is the DAEMON, and it is intercepted here rather than in agent/cli because the daemon
	// mode (runHost) cannot live in the CLI package. agent/cli documents it
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

// runDaemon is `briard run`: the host agent, the long-running process. The in-guest modes that
// used to hide behind `run --guest` / `run --deadman` are briard-guest-agent's now ([B.137]), a
// different binary with a different import graph.
func runDaemon(args []string) {
	fs := flag.NewFlagSet("briard run", flag.ExitOnError)
	_ = fs.Parse(args)

	// SIGTERM/SIGINT cancels the context so a `systemctl stop` is a clean shutdown rather than a
	// kill: the host agent stops its guest.
	//
	// Note what installing this handler COSTS, because it is easy to miss: it removes Go's
	// default "SIGTERM terminates the process". From here on, every path under this context is
	// responsible for noticing cancellation itself, and a path parked in a blocking syscall
	// notices nothing at all.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	// Take the notify socket out of the environment before anything is exec'd, so the
	// children this agent spawns cannot inherit it ([V3b.21e]).
	sdnotify.Adopt()

	// Boot the guest, drive bring-up, observe status.
	if err := runHost(ctx); err != nil {
		log.Fatalf("host agent: %v", err)
	}
}

// cardTimeout bounds --report-card end to end: the card ARP-probes a named VIP and creates a
// throwaway macvtap, and neither may become a way for an install to hang. Generous against the sum
// of its bounded parts, not against any one.
const cardTimeout = 30 * time.Second

// runInternal is the flag-shaped surface: helpers an installer, a release pipeline or a unit file
// invokes, each of which does one thing and exits. They are deliberately NOT `briard` verbs —
// see the note at each one.
func runInternal(args []string) {
	fs := flag.NewFlagSet("briard-agent", flag.ExitOnError)
	reportCard := fs.Bool("report-card", false, "check whether this machine can run briard, then exit (0 = yes, 1 = no, with reasons)")
	fetchInstall := fs.String("fetch-install", "", "download and verify the signed release (briard + vm chains) into <dir>, then exit (env: BRIARD_CHANNEL_URL, BRIARD_RELEASE, BRIARD_KEYRING)")
	fetchUpdate := fs.String("fetch-update", "", "resolve <target> (stable, latest, or a release id) on the channel, stage + arm this node's agent if due, then exit -- the frozen update unit's verb (env: BRIARD_CHANNEL_URL, BRIARD_KEYRING, UPDATE_BASE, UPDATE_RUN_DIR)")
	stageManifest := fs.String("stage-manifest", "", "describe the artifacts in <dir> into <dir>/manifest.json and exit -- the release pipeline's manifest writer (with --chain, --platform, --release)")
	stageChain := fs.String("chain", "", "with --stage-manifest: the release chain the directory belongs to (briard, vm)")
	stagePlatform := fs.String("platform", "", "with --stage-manifest: the platform arm within the chain (linux, windows; empty for the vm chain)")
	stageRelease := fs.String("release", "", "with --stage-manifest: the release id the directory is (e.g. v3.20260905.abc1234)")
	stageSystem := fs.String("system", "", "with --stage-manifest --chain vm: the store path of the NixOS toplevel the image boots (Manifest.System)")
	stageMinBriard := fs.String("min-briard", "", "with --stage-manifest --chain vm: the oldest briard release this VM tolerates (Manifest.MinBriard)")
	stageVM := fs.String("vm", "", "with --stage-manifest --chain briard: the vm release this briard release is published beside ([B.86i])")
	stageInputs := fs.String("inputs", "", "with --stage-manifest --chain vm: the image's input hash (sha256 hex; nix eval .#artifacts.guest-disk.inputs)")
	guestShutdown := fs.String("guest-shutdown", "", "power the guest VM at this QMP socket off cleanly, then exit -- the guest unit's ExecStop, not an operator command")
	_ = fs.Parse(args)

	// The release pipeline's manifest writer, and it is HERE rather than in the shell script that
	// calls it for exactly the reason the identifiers moved into the agent ([B.157]): the manifest
	// is a CONTRACT between the
	// publisher and every installing node, and it used to have two implementations -- a printf
	// loop in publish-release.sh (hand-assembling JSON, including `"mode":493`, which is 0o755
	// written in decimal by a human) and the struct in agent/install. Writing it with the same
	// code that reads it is what makes the format unable to disagree with itself.
	//
	// Same category as --report-card and --fetch-install: a pipeline invokes it, it does one
	// thing, it exits. It costs nothing in the shipped binary -- sha256 and encoding/json are
	// already linked -- and the guest agent is its own main ([B.137]), so the
	// guest trim is unaffected.
	if *stageManifest != "" {
		if err := runStageManifest(*stageManifest, *stageChain, *stagePlatform, *stageRelease, *stageSystem, *stageMinBriard, *stageVM, *stageInputs); err != nil {
			log.Fatalf("stage-manifest: %v", err)
		}
		return
	}

	// The machine report card -- the free-local installer's first gate.
	// Pure host inspection (no host subsystems), so it runs on any build; refuses the unfit with
	// the fix named before anything is installed.
	//
	// Bounded, because since [B.150](b) the card does one thing that can block: it creates and
	// deletes a throwaway macvtap on the NIC it selected, to turn "this device cannot carry the
	// guest" into a refusal rather than an unreachable VM. A gate is worthless if it can hang.
	if *reportCard {
		ctx, cancel := context.WithTimeout(context.Background(), cardTimeout)
		defer cancel()
		if !reportcard.Run(ctx, os.Stdout) {
			os.Exit(1)
		}
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	// The installer's signed-artifact fetch (assertion e) -- verify the qemu bundle +
	// guest image against the release keyring before install.sh uses them. Host-only (it pulls
	// in net/http); the guest agent is its own main and never sees it ([B.137]).
	if *fetchInstall != "" {
		if err := runFetchInstall(ctx, *fetchInstall); err != nil {
			log.Fatalf("fetch-install: %v", err)
		}
		return
	}

	// The frozen update unit's verb ([B.86a]). The unit runs it on a FRESH binary it just
	// pulled from the channel, never on the committed one -- so this is the suspect side doing
	// the verified fetch, and the one line it prints last on stdout is the unit's verdict (the
	// unit relays it to whoever started the run: the cloud's directive, the timer's journal, or
	// `briard update`). Host-only, like --fetch-install.
	if *fetchUpdate != "" {
		line, err := runFetchUpdate(ctx, *fetchUpdate)
		if err != nil {
			log.Fatalf("fetch-update: %v", err)
		}
		fmt.Fprintln(os.Stdout, line)
		return
	}

	// The guest unit's ExecStop (platform.launchArgs writes the line; systemd runs it). On a host
	// shutdown this is the only thing that gets a chance to power the appliance down cleanly --
	// the alternative is the SIGTERM systemd would otherwise send QEMU, which the guest
	// experiences as a power cut.
	//
	// A flag rather than a `briard` subcommand for the same reason as
	// --report-card: nobody types it. It is plumbing between the agent and a unit file the
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

	// A leading '-' that named none of the above. Not a daemon invocation: since [V3b.23] the
	// daemon is `briard run`, and falling through to it here would resurrect the very "a stray
	// flag silently starts a privileged process" behaviour this recut removed.
	fmt.Fprintf(os.Stderr, "briard: no internal helper named in %q (did you mean `briard run`?)\n", strings.Join(args, " "))
	os.Exit(2)
}
