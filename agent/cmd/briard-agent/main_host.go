package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"briard.io/agent/host"
	"briard.io/agent/install"
	"briard.io/agent/platform"
	"briard.io/agent/reportcard"
	"briard.io/agent/selfupdate"
	"briard.io/shared/api"
)

// runHost is the host agent: boot the guest, drive bring-up, observe status (host.Run, which
// wires everything else), under the config host.ConfigFromEnv reads -- the environment over
// config.env.
func runHost(ctx context.Context) error {
	return host.Run(ctx, host.ConfigFromEnv(), log.Printf)
}

// runGuestShutdown asks the VM on the given monitor socket to power off cleanly and waits until
// it is actually gone. Host-only (it pulls in platform/QEMU).
func runGuestShutdown(ctx context.Context, qmpSock string) error {
	return platform.ShutdownVM(ctx, qmpSock, platform.GuestShutdownGrace)
}

// runFetchInstall downloads + verifies the signed artifact sets of both chains into dest
// (install.FetchRelease), from BRIARD_CHANNEL_URL (the channel root), BRIARD_RELEASE (the release
// to install, default stable) and BRIARD_KEYRING (the release keyring PEM). `briard-agent
// install` fetches through the same function; the flag stays for the installer that predates the
// verb: an install.sh saved to disk heals forward to current stable ("old installer, new
// artifacts", scripts/publish-release.sh), so a bootstrap must still answer the --report-card /
// --fetch-install pair that installer calls before laying the files down itself.
func runFetchInstall(ctx context.Context, dest string) error {
	base := os.Getenv("BRIARD_CHANNEL_URL")
	if base == "" {
		return errors.New("BRIARD_CHANNEL_URL unset (the release channel root URL)")
	}
	release := os.Getenv("BRIARD_RELEASE")
	if release == "" {
		release = install.TargetStable
	}
	keyPath := os.Getenv("BRIARD_KEYRING")
	if keyPath == "" {
		return errors.New("BRIARD_KEYRING unset (the release keyring PEM path)")
	}
	pemBytes, err := os.ReadFile(keyPath)
	if err != nil {
		return fmt.Errorf("read keyring %s: %w", keyPath, err)
	}
	kr, err := selfupdate.NewKeyring(pemBytes)
	if err != nil {
		return err
	}
	return install.FetchRelease(ctx, base, release, kr, dest, "", log.Printf)
}

// readKeyring loads the release keyring the environment names (BRIARD_KEYRING).
func readKeyring() (*selfupdate.Keyring, string, error) {
	keyPath := os.Getenv("BRIARD_KEYRING")
	if keyPath == "" {
		return nil, "", errors.New("BRIARD_KEYRING unset (the release keyring PEM path)")
	}
	pemBytes, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, "", fmt.Errorf("read keyring %s: %w", keyPath, err)
	}
	kr, err := selfupdate.NewKeyring(pemBytes)
	return kr, keyPath, err
}

// runVerifyAgent is --verify-agent: install.VerifyAgent against the node's keyring.
func runVerifyAgent(dir string) error {
	kr, _, err := readKeyring()
	if err != nil {
		return err
	}
	return install.VerifyAgent(kr, dir)
}

// selfTestTimeout bounds the candidate's check: a few hashes and a signature, so anything near it
// is a candidate that hangs, which is a refusal like any other.
const selfTestTimeout = time.Minute

// selfTest runs the candidate agent at path with --verify-agent, as the update unit will once it
// is committed: its own process and command line, so what is proven is the invocation itself.
func selfTest(ctx context.Context, path, dir, keyring string) error {
	ctx, cancel := context.WithTimeout(ctx, selfTestTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "--verify-agent", dir)
	cmd.Env = append(os.Environ(), "BRIARD_KEYRING="+keyring)
	out, err := cmd.CombinedOutput()
	if err != nil {
		if line := strings.TrimSpace(string(out)); line != "" {
			return fmt.Errorf("%w: %s", err, line)
		}
		return err
	}
	return nil
}

// runUpdate is the update unit's verb: the whole self-update (install.Update), run on the fresh
// copy of the target's agent the unit fetched. The layout comes from the env the unit passes
// through (UPDATE_BASE / UPDATE_RUN_DIR), and the smoke test's accelerator and CPU from the node's
// own config, so the staged qemu is proven the way this node launches its guest.
func runUpdate(ctx context.Context, target string) (string, error) {
	base := os.Getenv("BRIARD_CHANNEL_URL")
	if base == "" {
		return "", errors.New("BRIARD_CHANNEL_URL unset (the release channel root URL)")
	}
	kr, keyPath, err := readKeyring()
	if err != nil {
		return "", err
	}
	cfg := host.ConfigFromEnv()
	u := &install.Update{
		Fetcher: &install.Fetcher{BaseURL: base, Chain: install.ChainBriard, Platform: install.PlatformHost, Keyring: kr, Logf: log.Printf},
		Layout:  selfupdate.New(os.Getenv("UPDATE_BASE"), os.Getenv("UPDATE_RUN_DIR")),
		Keyring: keyPath,
		Logf:    log.Printf,
		Stop:    updateStop,
		Start:   updateStart,
		Smoke: func(ctx context.Context, tree string) error {
			return platform.SmokeTest(ctx, tree, cfg.Accel, cfg.CPUModel, log.Printf)
		},
		SelfTest: selfTest,
		Stopped:  func(context.Context) (bool, error) { return platform.ServiceStopped(selfupdate.DefaultAgentUnit) },
		// The image, through the running agent's admin socket: the agent applies the rule for
		// whose OS this is, and a node it does not apply to answers done. Two answers are "not
		// now" rather than failures, deferred to the next run: another act in flight, and an agent
		// that cannot be asked at all (down, or crashed), whose state its own alerts cannot report
		// either, so failing this run over it would name the wrong fault.
		Guest: func(ctx context.Context, target string) (string, error) {
			o, err := host.Submit(ctx, cfg.AdminSock, api.Directive{Kind: install.DirectiveUpdateVMAuto, Payload: target})
			switch {
			case err != nil:
				return "not asked, the agent did not answer (" + err.Error() + "); the next run asks again", nil
			case o.State == api.OutcomeDone:
				return o.Detail, nil
			case o.State == api.OutcomeFailed && strings.HasPrefix(o.Detail, host.BusyPrefix):
				return "deferred to the next run (" + o.Detail + ")", nil
			}
			return "", errors.New(o.Detail)
		},
	}
	line, err := u.Run(ctx, target)
	if err != nil && install.Unreachable(err) {
		err = fmt.Errorf("%s%w", install.UnreachablePrefix, err)
	}
	return line, err
}

// runStageManifest writes dir/manifest.json describing the artifacts staged in dir as one
// release of one chain -- the release pipeline's writer, so the bytes a release publishes are
// described by the same code that installs them (agent/install.WriteManifest). Host-side: the
// guest agent is its own main and never links the install package.
func runStageManifest(dir, chain, platform, version, system, minBriard, vm, inputs string) error {
	return install.WriteManifest(dir, chain, platform, version, system, minBriard, vm, inputs)
}

// runInstall is `briard-agent install [-wait]`, the installer's body (host.Install). Without
// -wait: the machine report card, then the layout -- the card first, so a refusal is truthful
// when it says nothing was installed and its disk check measures a disk the install has not
// eaten into. With -wait: the closing, once install.sh has enabled the agent's unit
// (host.InstallWait). Failures read as the installer's own (`briard: ERROR: ...`), since that is
// whose output they are.
func runInstall(args []string) int {
	fs := flag.NewFlagSet("briard install", flag.ContinueOnError)
	wait := fs.Bool("wait", false, "wait for the installed node to answer, then print its one-time link")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprint(os.Stderr, "briard install: takes no arguments\n")
		return 2
	}
	say := func(format string, a ...any) { fmt.Printf("briard: "+format+"\n", a...) }
	fail := func(err error) int { fmt.Fprintf(os.Stderr, "briard: ERROR: %v\n", err); return 1 }
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	if *wait {
		if err := host.InstallWait(ctx, say); err != nil {
			return fail(err)
		}
		return 0
	}
	if !elevated() {
		return fail(errors.New(elevateHint))
	}
	// The card is told the two things it cannot infer. VIP_ADDR, because judging a service address
	// against this LAN is the one check that compares our intent with the household's network.
	// BRIARD_NIC (read as is), because the card selects the device the guest's L2 will hang off and
	// validates it with a throwaway macvtap on it -- judging a different device than the install
	// will use is the worst possible half-truth. The substrate is not passed at all: the card
	// derives it from the device.
	say("checking host readiness ...")
	os.Setenv("VIP_ADDR", os.Getenv("BRIARD_VIP_ADDR"))
	cardCtx, cancel := context.WithTimeout(ctx, cardTimeout)
	ok := reportcard.Run(cardCtx, os.Stdout)
	cancel()
	if !ok {
		return fail(errors.New("host is not ready (see the fix above); nothing was installed"))
	}
	if err := host.Install(ctx, say, log.Printf); err != nil {
		return fail(err)
	}
	if host.InstallOwesRestart() {
		say("installed; Windows must restart before briard can start")
		return host.ExitRestartRequired
	}
	return 0
}

// runUninstall is `briard uninstall`. The flags are parsed here and the work is the host
// agent's (host.Uninstall); agent/cli carries the verb's help. Without -yes it only says what would
// go and what would stay, the way `briard rescue` does: there is no undo for either half.
func runUninstall(args []string) int {
	fs := flag.NewFlagSet("briard uninstall", flag.ContinueOnError)
	yes := fs.Bool("yes", false, "confirm: remove briard from this machine")
	deleteData := fs.Bool("delete-data", false, "also delete this machine's data volume and identity ("+dataTree+")")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprint(os.Stderr, "briard uninstall: takes no arguments\n")
		return 2
	}
	// Windowed: started from Windows' Installed apps, which starts this elevated in a console of its
	// own that closes the moment this exits; there the question and the outcome are windows. Never
	// with -yes: that is the quiet form a package manager or a script runs, with nobody there to
	// close a window. The quiet form in a console of its own is that package manager's run, and
	// shows no window at all.
	alone := windowed()
	win := !*yes && alone
	if win {
		noQuickEdit()
	}
	if *yes && alone {
		hideConsole()
	}
	if win && !elevated() {
		tell("Briard was not uninstalled: "+uninstallElevate+".", true)
		return 1
	}
	if !*yes && !win {
		keep := "Your DATA IS KEPT: " + dataTree + " stays, holding the data volume and this\n" +
			"machine's identity, and installing again picks up where it left off.\n" +
			"Add -delete-data to delete it as well; that cannot be undone.\n"
		if *deleteData {
			keep = "-delete-data DELETES " + dataTree + " as well: the data volume, every app's data\n" +
				"on it, and this machine's identity. That cannot be undone.\n"
		}
		fmt.Fprint(os.Stderr, "briard uninstall: this stops briard and its VM (cleanly), and removes its "+
			systemUnits+",\nnetwork devices, "+installTree+" and the `briard` command.\n\n"+keep+
			"\nRe-run with -yes to go ahead.\n")
		return 2
	}
	if win {
		// The window asks with a checkbox where the terminal takes a flag, in words anyone can read:
		// the terminal's names the parts. Deleting rides a second, positive answer; its No
		// uninstalls nothing.
		ok, del := confirmUninstall("Uninstall Briard?",
			"This stops Briard and removes it from this computer.\n\n"+
				"Your apps and their data are kept unless you tick the box below, so installing Briard "+
				"again picks up where you left off.",
			"Also delete my apps and their data. This cannot be undone.",
			*deleteData)
		if !ok {
			return 2
		}
		if del && !confirmDelete("Delete your apps and their data for good?\n\n"+
			"Everything your apps keep, such as their settings and history, is deleted with Briard. "+
			"This cannot be undone.\n\n"+
			"Yes deletes them with Briard. No cancels, and nothing is uninstalled.") {
			return 2
		}
		*deleteData = del
	}
	if !elevated() {
		// Windows asks for the rights through UAC, as the installer does; elsewhere it refuses.
		return uninstallElevated(args)
	}
	var logged strings.Builder
	if win {
		// The console stays open while the uninstall runs; its lines, and what it logs (a retry, a
		// detail), are kept for the end.
		fmt.Println("Uninstalling Briard. This takes up to a minute, and this window closes by itself.")
		log.SetOutput(&logged)
	}
	var said []string
	say := func(format string, a ...any) {
		line := fmt.Sprintf(format, a...)
		said = append(said, line)
		if !win {
			fmt.Printf("briard: %s\n", line)
		}
	}
	err := host.ConfigFromEnv().Uninstall(context.Background(), *deleteData, say)
	if err != nil {
		fmt.Fprintf(os.Stderr, "briard uninstall: %v\n", err)
	}
	if win {
		if l := strings.TrimSpace(logged.String()); l != "" {
			said = append(said, strings.Split(l, "\n")...)
		}
		tell(uninstalled(said, *deleteData, err), err != nil)
	}
	if err != nil {
		return 1
	}
	return 0
}

// uninstalled is the windowed uninstall's last word, from what the uninstall said and how it
// ended: plain words when it worked, and when it did not, everything it did, which is what
// someone helping needs.
func uninstalled(said []string, deleteData bool, err error) string {
	if err != nil {
		return "Briard was not fully uninstalled: " + err.Error() + "\n\n" +
			"To ask for help, open " + helpLink + " and include what the uninstall did:\n\n" +
			strings.Join(said, "\n\n")
	}
	text := "Briard is uninstalled."
	if deleteData {
		text += " Your apps and their data are deleted."
	} else {
		text += "\n\nYour apps and their data are kept in " + dataTree + ", so installing Briard again " +
			"picks up where you left off. To delete them too, delete that folder."
	}
	for _, l := range said {
		if strings.Contains(l, "next restart") {
			text += "\n\nRestart Windows to finish: a few files go then."
			break
		}
	}
	return text
}

// helpLink is where a user asks for help: the project's discussions.
const helpLink = "https://github.com/briardhq/briard/discussions"
