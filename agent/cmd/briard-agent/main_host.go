package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"briard.io/agent/host"
	"briard.io/agent/install"
	"briard.io/agent/platform"
	"briard.io/agent/reportcard"
	"briard.io/agent/selfupdate"
)

// runHost is the default (untagged) build's host path: boot the guest, drive bring-up,
// observe status. Importing host here (not in main.go) is what once let a `-tags guest` build
// exclude host and everything it pulls in (platform, net/http, crypto/tls). Config comes
// from the environment for now; the north-bound shared/api report/config seam is wired here.
func runHost(ctx context.Context) error {
	return host.Run(ctx, host.ConfigFromEnv(), log.Printf)
}

// runGuestShutdown asks the VM on the given monitor socket to power off cleanly and waits until
// it is actually gone. Host-only (it pulls in platform/QEMU).
func runGuestShutdown(ctx context.Context, qmpSock string) error {
	return platform.ShutdownVM(ctx, qmpSock, platform.GuestShutdownGrace)
}

// runFetchInstall downloads + verifies the signed artifact sets of BOTH chains into dest
// (install.FetchRelease). The channel root, the release to install and the release keyring PEM
// come from the environment: BRIARD_CHANNEL_URL + BRIARD_RELEASE + BRIARD_KEYRING. It lives
// here, not main.go; the guest is its own main and never links install/net/http (the trim).
//
// `briard install` fetches through the same function. The flag stays for the installer that
// predates the verb: an install.sh saved to disk heals forward to current stable ("old
// installer, new artifacts", scripts/publish-release.sh), so a bootstrap must still answer the
// --report-card / --fetch-install pair that installer calls before laying the files down itself.
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
	return install.FetchRelease(ctx, base, release, kr, dest, log.Printf)
}

// runUpdate is the update unit's verb: the whole self-update (install.Update), run on the fresh
// binary the unit just pulled. The layout comes from the env the unit passes through (UPDATE_BASE /
// UPDATE_RUN_DIR), and the smoke test's accelerator and CPU from the node's own config, so the
// staged qemu is proven the way this node launches its guest.
func runUpdate(ctx context.Context, target string) (string, error) {
	base := os.Getenv("BRIARD_CHANNEL_URL")
	if base == "" {
		return "", errors.New("BRIARD_CHANNEL_URL unset (the release channel root URL)")
	}
	keyPath := os.Getenv("BRIARD_KEYRING")
	if keyPath == "" {
		return "", errors.New("BRIARD_KEYRING unset (the release keyring PEM path)")
	}
	pemBytes, err := os.ReadFile(keyPath)
	if err != nil {
		return "", fmt.Errorf("read keyring %s: %w", keyPath, err)
	}
	kr, err := selfupdate.NewKeyring(pemBytes)
	if err != nil {
		return "", err
	}
	cfg := host.ConfigFromEnv()
	unit := selfupdate.DefaultAgentUnit
	u := &install.Update{
		Fetcher: &install.Fetcher{BaseURL: base, Chain: install.ChainBriard, Platform: install.PlatformHost, Keyring: kr, Logf: log.Printf},
		Layout:  selfupdate.New(os.Getenv("UPDATE_BASE"), os.Getenv("UPDATE_RUN_DIR")),
		Logf:    log.Printf,
		Stop: func(ctx context.Context, patience time.Duration) error {
			return platform.StopService(ctx, unit, patience, log.Printf)
		},
		Start: func(ctx context.Context) error { return platform.StartService(ctx, unit) },
		Smoke: func(ctx context.Context, tree string) error {
			return platform.SmokeTest(ctx, tree, cfg.Accel, cfg.CPUModel, log.Printf)
		},
	}
	return u.Run(ctx, target)
}

// runStageManifest writes dir/manifest.json describing the artifacts staged in dir as one
// release of one chain -- the release pipeline's writer, so the bytes a release publishes are
// described by the same code that installs them (agent/install.WriteManifest). Host-side for
// the same reason as runFetchInstall: it lives in the install package, which the guest
// trim excludes.
func runStageManifest(dir, chain, platform, version, system, minBriard, vm, inputs string) error {
	return install.WriteManifest(dir, chain, platform, version, system, minBriard, vm, inputs)
}

// runInstall is `briard-agent install [-wait]`, the installer's body (host.Install). Without
// -wait: the machine report card, then the layout -- the card FIRST, so a refusal is truthful
// when it says nothing was installed and its disk check measures a disk the install has not
// eaten into. With -wait: the closing, once the installer has enabled the service
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
	// The card is told the two things it cannot infer. VIP_ADDR, because judging a service
	// address against THIS LAN is the one check that compares our intent with the household's
	// network. BRIARD_NIC (read as is), because the card SELECTS the device the guest's L2 will
	// hang off and validates it by creating a throwaway macvtap on it -- judging a different device
	// than the install will use is the worst possible half-truth. The substrate is not passed at
	// all: the card derives it from the device.
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
	// WINDOWED: started from Windows' Installed apps, whose entry asks for an administrator's
	// rights and starts this in a console of its own, which closes the moment this exits. There
	// the question and the outcome are windows. Never with -yes: that is the quiet form a package
	// manager or a script runs, with nobody there to close a window.
	win := !*yes && windowed()
	if win && !elevated() {
		tell("Briard was not uninstalled: "+uninstallElevate+".", true)
		return 1
	}
	if !*yes {
		keep := "Your DATA IS KEPT: " + dataTree + " stays, holding the data volume and this\n" +
			"machine's identity, and installing again picks up where it left off.\n" +
			"Add -delete-data to delete it as well; that cannot be undone.\n"
		if *deleteData {
			keep = "-delete-data DELETES " + dataTree + " as well: the data volume, every app's data\n" +
				"on it, and this machine's identity. That cannot be undone.\n"
		}
		what := "this stops briard and its VM (cleanly), and removes its services,\n" +
			"network devices, " + installTree + " and the `briard` command.\n\n" + keep
		if !win {
			fmt.Fprint(os.Stderr, "briard uninstall: "+what+"\nRe-run with -yes to go ahead.\n")
			return 2
		}
		if !confirm("T" + what[1:] + "\nUninstall Briard?") {
			return 2
		}
	}
	if !elevated() {
		fmt.Fprintf(os.Stderr, "briard uninstall: %s\n", uninstallElevate)
		return 1
	}
	var said []string
	say := func(format string, a ...any) {
		line := fmt.Sprintf(format, a...)
		said = append(said, line)
		fmt.Printf("briard: %s\n", line)
	}
	err := host.ConfigFromEnv().Uninstall(context.Background(), *deleteData, say)
	if err != nil {
		fmt.Fprintf(os.Stderr, "briard uninstall: %v\n", err)
	}
	if win {
		// What the console said, kept where the user can read it.
		if err != nil {
			said = append(said, "Briard was NOT fully uninstalled: "+err.Error())
		}
		tell(strings.Join(said, "\n\n"), err != nil)
	}
	if err != nil {
		return 1
	}
	return 0
}
