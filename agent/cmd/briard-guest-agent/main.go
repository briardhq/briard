// Command briard-guest-agent is the in-guest half of Briard: the control agent serving the host
// over virtio-serial, the host-agent deadman, and the converge step drbd-reactor runs at
// promotion. Its own main, so the guest links an import graph rather than a build tag:
// guestagent and what it needs, nothing of the host (no channel fetcher, self-update layout, CLI,
// report card or QEMU launcher). The host pushes this binary, so it is outside the guest image's
// input hash, which covers the baked firmware (agent/cmd/briard-guest-firmware) that
// internal/arch fences.
//
// The argv is the contract the guest's units share -- the image's (guest-image/disk-image.nix)
// and those this agent renders (guestagent.WriteUnits); the pushed-binary picker passes it through
// verbatim:
//
//	briard-guest-agent run --guest      serve the host (Type=notify; READY once the port is open)
//	briard-guest-agent run --deadman    the host-agent watchdog, its own unit
//	briard-guest-agent run --inbound-listen  the inbound channel alone; `run --guest`
//	                                    does this itself, so only an agent-less rig invokes it
//	briard-guest-agent --converge       render, warm and start every app the volume names
//	briard-guest-agent --converge-stop  stop those units (briard-apps' ExecStop)
//	briard-guest-agent --node-storage   build this node's tiers and attach the DRBD resource,
//	                                    from the spec the host wrote (briard-node-storage)
//	briard-guest-agent --primary-storage      format on first use + mount the replicated volume
//	briard-guest-agent --primary-storage-stop unmount it (briard-primary-storage's ExecStop)
//	briard-guest-agent --write-units    render the units the agent owns; `run --guest`
//	                                    does this itself, so only an agent-less rig invokes it
//	briard-guest-agent --test-launch    the cheap self-test a staged copy passes before it is
//	                                    trialled: execs, parses, sees the port device
package main

import (
	"context"
	"encoding/json"
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

// runDaemon is `run --guest`, `run --deadman` or a harness's `run --inbound-listen`: the
// long-running modes. There is no default mode here -- the host agent is a different binary -- so
// a bare `run` is refused rather than guessed.
func runDaemon(args []string) {
	fs := flag.NewFlagSet("briard-guest-agent run", flag.ExitOnError)
	guest := fs.Bool("guest", false, "serve the host over the virtio-serial control channel")
	deadman := fs.Bool("deadman", false, "run the host-agent deadman")
	inboundListen := fs.Bool("inbound-listen", false, "serve the inbound channel alone -- what `run --guest` does inside itself, for a harness with no host")
	_ = fs.Parse(args)

	// SIGTERM/SIGINT cancels the context so `systemctl stop` is a clean shutdown, not a kill. The
	// handler removes Go's default "SIGTERM terminates the process": every path under this context
	// must notice cancellation itself, and one parked in a blocking syscall notices nothing -- hence
	// the serve loop closes the port under its read (guestfirmware.ServeFrames) and runGuest holds a
	// deadline.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	// Take the notify socket out of the environment before anything is exec'd, so the children
	// this agent spawns (systemctl, podman, drbdadm) cannot inherit it; Ready() keeps the socket.
	sdnotify.Adopt()

	switch {
	case *deadman:
		// The deadman runs as its own guest unit (briard-deadman), decoupled from the per-connection
		// guest agent, which restarts in a loop while the host is down.
		if err := guestagent.RunDeadman(ctx); err != nil {
			log.Fatalf("deadman: %v", err)
		}
	case *guest:
		if err := runGuest(ctx); err != nil {
			log.Fatalf("guest agent: %v", err)
		}
	case *inboundListen:
		// For a guest with no host, like --write-units and for the same reason: the product runs
		// the inbound listener inside runGuest, where everything it reasons about lives, and never
		// invokes this. The agent-less rigs run no `run --guest` (nixosTest/lib.nix: no host on the
		// control channel's other end), yet only they get Home Assistant running; without this the
		// channel is unreachable in every test and an L0 run proves nothing about it. The start
		// evaluator rides along for the same reason: it is the other half of the ring that lives in
		// the long-running agent.
		go guestagent.EvaluateStarts(ctx, guestfirmware.NewOSExecutor())
		if err := guestagent.ListenInbound(ctx, guestfirmware.NewOSExecutor()); err != nil {
			log.Fatalf("inbound channel: %v", err)
		}
	default:
		fmt.Fprintln(os.Stderr, "briard-guest-agent run: say which one: --guest, --deadman or --inbound-listen")
		os.Exit(2)
	}
}

// runInternal is the flag-shaped surface: what the guest's units invoke (converge, storage, the
// app hooks) and the helpers below that only a harness with no host invokes.
//
// --converge is converge-at-promotion, run by briard-apps.service as a promoter chain member after
// the data mount and the VIP. Its exit code is load-bearing: a non-zero ExecStart fails the whole
// promotion, so a node that cannot converge, and so cannot serve, must say so rather than
// promote into a broken state. An app that fails to start never reaches here: Converge logs it
// and returns nil.
func runInternal(args []string) {
	fs := flag.NewFlagSet("briard-guest-agent", flag.ExitOnError)
	converge := fs.Bool("converge", false, "render, warm and start every app the replicated volume names, then exit -- briard-apps.service's ExecStart")
	convergeStop := fs.Bool("converge-stop", false, "stop the app units this node converged to -- briard-apps.service's ExecStop")
	nodeStorage := fs.Bool("node-storage", false, "build every tier /run/briard/node-storage.json names and attach the resource -- briard-node-storage.service's ExecStart")
	primaryStorage := fs.Bool("primary-storage", false, "format on first use, mount the replicated volume -- briard-primary-storage.service's ExecStart")
	primaryStorageStop := fs.Bool("primary-storage-stop", false, "unmount the replicated volume -- briard-primary-storage.service's ExecStop")
	writeUnits := fs.Bool("write-units", false, "render the units this agent owns into /run/systemd/system and reload -- what `run --guest` does at start, for a harness with no host")
	testLaunch := fs.Bool("test-launch", false, "the push protocol's cheap self-test: check what a staged copy can check without the port, then exit 0")
	appStarting := fs.String("app-starting", "", "take this app's ring member; the rendered container unit's ExecStartPre")
	clock := fs.String("clock", "", "take this app's quiesced clock sample -- an internal flag a harness invokes; the product drives this verb from the host")
	dbCheck := fs.Bool("dbcheck", false, "run Home Assistant's recorder check in line and print its report as JSON -- an internal flag a harness invokes; the product drives this verb from the host")
	dbRestore := fs.String("dbrestore", "", "restore Home Assistant's recorder database from this ring member -- an internal flag a harness invokes; the product drives this verb from the host")
	backup := fs.String("backup", "", "run the backup in line into the REST repository at this URL, under the key the volume holds, and print its report as JSON -- an internal flag a harness invokes; the product drives this verb from the host")
	backupRestore := fs.String("backup-restore", "", "put one night of the backup at this REST URL into the ring as a member (-app, -snapshot) and print the member's path -- an internal flag a harness invokes; the product drives this verb from the host")
	restoreApp := fs.String("app", "", "with -backup-restore: the app")
	restoreSnapshot := fs.String("snapshot", "latest", "with -backup-restore: the night, a snapshot id or latest")
	backupAdopt := fs.String("backup-adopt", "", "add the volume's key to the repository at this REST URL, opened once with -key -- an internal flag a harness invokes; the product drives this verb from the host")
	adoptKey := fs.String("key", "", "with -backup-adopt: the key that opens the repository")
	appStopped := fs.String("app-stopped", "", "record whether this app's data was flushed by a clean stop; the rendered container unit's ExecStopPost")
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

	if *writeUnits {
		// For a guest with no host: in the product `run --guest` does this first, so nothing
		// invokes this flag. The nixosTest machines have no host to dress them and no control
		// channel to serve; this is how they get the product's own units instead of a harness's
		// copy (nixosTest/lib.nix). A rig that declared its own could not notice the product
		// changing them.
		if err := guestagent.WriteUnits(ctx, guestfirmware.NewOSExecutor()); err != nil {
			log.Fatalf("write-units: %v", err)
		}
		return
	}

	if *primaryStorage || *primaryStorageStop {
		// A promoter chain member, so the exit code is load-bearing: a non-zero ExecStart fails the
		// whole promotion, the node never claims the VIP, and a primary with no address is already
		// reported unhealthy. An unformatted or unmountable volume is a node that cannot serve, and
		// it must say so rather than promote into a broken state.
		run, what := guestagent.PrimaryStorage, "primary-storage"
		if *primaryStorageStop {
			run, what = guestagent.PrimaryStorageStop, "primary-storage-stop"
		}
		if err := run(ctx, guestfirmware.NewOSExecutor()); err != nil {
			log.Fatalf("%s: %v", what, err)
		}
		return
	}

	if *appStarting != "" {
		// The generic hook: every catalogued app gets a member at container start (each container
		// that mounts its data), the only boundary visible from outside it; Home Assistant adds its
		// own restarts through the inbound channel, everything else has just this. The rendered line
		// deliberately carries no tool profile (the renderer is a pure function of the manifest, and
		// the image would be a second thing to know): the ring puts the tools on its own PATH
		// (guestagent.ensureToolsOnPath), covering every entry point that can take a member rather
		// than each caller remembering.
		//
		// ⚠️ Never fails the start: losing a household's app because a snapshot could not be taken
		// is the wrong trade. The rendered line (agent/quadlet) also carries systemd's `-`; the
		// belt-and-braces is deliberate, because `-` alone hides why: the exit code is 0 either
		// way, the journal has the reason.
		detail, err := guestagent.TakeStartMember(ctx, guestfirmware.NewOSExecutor(), *appStarting)
		if err != nil {
			log.Printf("app-starting %s: %v (starting anyway)", *appStarting, err)
			return
		}
		log.Printf("app-starting %s: %s", *appStarting, detail)
		return
	}

	if *clock != "" {
		// For a guest with no host, like --inbound-listen: the rigs that get Home Assistant running
		// are agent-less, and this is the only way an L0 run can exercise the recorder lock against
		// a real Home Assistant. Nothing in the product invokes it.
		detail, err := guestagent.TakeClockMember(ctx, guestfirmware.NewOSExecutor(), *clock, time.Now())
		if err != nil {
			log.Fatalf("clock %s: %v", *clock, err)
		}
		log.Printf("clock %s: %s", *clock, detail)
		return
	}

	if *dbCheck || *dbRestore != "" {
		// For a guest with no host, like --clock: the product reaches these through the host's
		// verbs, the agent-less rigs that run a real Home Assistant reach them here. The report
		// goes to stdout as JSON, for the harness to read; the log goes to stderr.
		x := guestfirmware.NewOSExecutor()
		if *dbRestore != "" {
			if err := guestagent.RestoreRecorder(ctx, x, *dbRestore); err != nil {
				log.Fatalf("dbrestore: %v", err)
			}
			log.Printf("dbrestore: restored from %s", *dbRestore)
			return
		}
		b, err := json.Marshal(guestagent.CheckRecorder(ctx, x))
		if err != nil {
			log.Fatalf("dbcheck: %v", err)
		}
		fmt.Println(string(b))
		return
	}

	if *backup != "" {
		// For a guest with no host, like --dbcheck: the product starts it through the host's verb, in
		// the background; an agent-less rig runs the same backup in line through here.
		b, err := json.Marshal(guestagent.Backup(ctx, guestfirmware.NewOSExecutor(), *backup))
		if err != nil {
			log.Fatalf("backup: %v", err)
		}
		fmt.Println(string(b))
		return
	}

	if *backupRestore != "" {
		// For a guest with no host, like --backup: the product's host materialises a night through
		// the verb and restores the member; a rig proves the materialisation here.
		member, err := guestagent.RestoreBackupMember(ctx, guestfirmware.NewOSExecutor(), *backupRestore, *restoreSnapshot, *restoreApp)
		if err != nil {
			log.Fatalf("backup-restore: %v", err)
		}
		fmt.Println(member)
		return
	}

	if *backupAdopt != "" {
		if err := guestagent.AdoptBackup(ctx, guestfirmware.NewOSExecutor(), *backupAdopt, *adoptKey); err != nil {
			log.Fatalf("backup-adopt: %v", err)
		}
		return
	}

	if *appStopped != "" {
		// The clean-stop marker, the other half of the question the member above answers: whether
		// this app's data was flushed, which a stopped container does not prove on its own.
		// systemd's $SERVICE_RESULT is the evidence, and it exists only in an ExecStopPost, which
		// is why the rendered unit puts this there. Never fails a stop, as above (the rendered line
		// carries `-` as well): a marker that could not be written costs the next member its claim
		// to be quiesced, the safe direction.
		if err := guestagent.RecordAppStop(ctx, guestfirmware.NewOSExecutor(), *appStopped, os.Getenv("SERVICE_RESULT")); err != nil {
			log.Printf("app-stopped %s: %v (the next member will read as crash-consistent)", *appStopped, err)
		}
		return
	}

	if *nodeStorage {
		// Storage is the host's decision and the guest's work: the host wrote the spec before
		// starting this unit, so there is nothing to pass on the command line and nothing for this
		// process to decide. A non-zero exit fails the verb the host is still waiting on.
		if err := guestagent.NodeStorage(ctx, guestfirmware.NewOSExecutor()); err != nil {
			log.Fatalf("node storage: %v", err)
		}
		return
	}

	if *converge || *convergeStop {
		// Converge's skip list is dropped here on purpose: this is drbd-reactor's call, and an app
		// it could not prepare must not fail the unit: briard-apps is a chain member, so exiting
		// non-zero would take the promotion down over one app's packaging. Converge has already
		// logged which one and declined to start it. The install path reads the list through the
		// verb, where failing is the right answer.
		run, what := func(ctx context.Context, x guestagent.Executor) error {
			_, err := guestagent.Converge(ctx, x, "")
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

// runGuest opens the virtio-serial port and serves the guestagent dispatch loop for one host
// connection; the unit's Restart=always puts it back on the port for the next.
//
// Cancellation is a deadline, not a hope: a `systemctl stop` lands while Serve is parked in a
// blocking read of the port, which sees no cancellation. Closing the port unparks it, and
// guestStopGrace bounds the case where even that is not enough -- the host's clean-shutdown
// timing depends on this process actually ending.
func runGuest(ctx context.Context) error {
	x := guestfirmware.NewOSExecutor()
	// The units this binary owns, before anything else: the guest image does not define them, so
	// nothing below (the trial verdict, the host's first bring-up verb after the port) can name a
	// unit until they are written. Ordering this ahead of the trial also makes the trial honest: a
	// staged agent is judged on the units it writes, not the ones its predecessor left behind.
	if err := guestagent.WriteUnits(ctx, x); err != nil {
		return err
	}
	// The inbound channel: one socket, bound unconditionally, whether this node runs zero apps or
	// five. It needs no app list: a caller identifies itself with a token converge minted, so there
	// is nothing to learn, and nothing to re-learn when a promotion brings an app this process was
	// never told about. It runs in its own goroutine, and a bind failure is logged rather than
	// returned: an app's own snapshot notifications are worth strictly less than serving the host,
	// and a node that would not serve its household because a socket path was busy is the wrong
	// trade -- the posture Home Assistant's wrapper (agent/hass/run.sh) takes with `|| true`, one
	// level up.
	go func() {
		if err := guestagent.ListenInbound(ctx, x); err != nil {
			log.Printf("inbound channel: %v; apps' own restarts will not be snapshotted", err)
		}
	}()
	// The start evaluator: a start sample is evaluated once its boot has a verdict, which only a
	// process that outlives the pre-start hook can wait for. Its own goroutine, for the reason above.
	go guestagent.EvaluateStarts(ctx, x)
	// The push protocol's start-time duty, before the port. A trial start is the verdict on the
	// whole pushed set (the web units' real launch, where they run); a refused verdict exits here,
	// port never opened, so the host's reconnect meets the committed agent and reads the old
	// release. A non-trial start discards a staged set left behind. A failed trial's own restart
	// lands back at WriteUnits above, so the committed agent's units replace the candidate's on the
	// way back down -- the revert is the same mechanism.
	if err := guestfirmware.BinStartup(ctx, x, log.Printf); err != nil {
		return err
	}
	conn, err := os.OpenFile(guestfirmware.ControlPortDev, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer conn.Close()

	// The commit, here and nowhere else: the port is open, the last thing this binary had to prove,
	// and nothing has been served yet. The host's gate is the port, not READY, so anything committed
	// after the first verb is a set the units bring-up starts cannot exec (briard-node-storage names
	// the committed path directly). BinCommit says why in full.
	guestfirmware.BinCommit(ctx, x, log.Printf)

	go func() {
		<-ctx.Done()
		time.AfterFunc(guestStopGrace, func() { os.Exit(0) })
	}()

	// READY at listen: the unit is Type=notify under the guest's pivot. A pushed agent that cannot
	// open the port never sends it, and the next start falls back to the committed one. The set is
	// already committed by then, so READY means "serving the binaries this node keeps", not "about to
	// commit them".
	_ = sdnotify.Ready()
	if err := guestagent.ServeStamped(ctx, conn, x); err != nil {
		return err
	}
	log.Printf("control: the host disconnected; handing the port back and restarting")

	// A clean EOF: the host disconnected (a host-agent restart, a re-adopt). Hand the port back
	// so qemu buffers the next host request instead of losing it, then pause before exiting --
	// with the host end gone for good the reopened port returns EOF at once, and without this
	// pause Restart=always restarts it ~48 times in 30 s.
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
// before the process that carries it is killed.
const guestStopGrace = 5 * time.Second

const _ = uint(guestStopGrace - guestfirmware.PowerOffGrace)
