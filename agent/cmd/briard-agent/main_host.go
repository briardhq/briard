//go:build !guest

package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"briard.io/agent/host"
	"briard.io/agent/install"
	"briard.io/agent/platform"
	"briard.io/agent/selfupdate"
)

// runHost is the default (untagged) build's host path: boot the guest, drive bring-up,
// observe status. Importing host here (not in main.go) is what lets a `-tags guest` build
// exclude host and everything it pulls in (platform, net/http, crypto/tls). Config comes
// from the environment for now; the north-bound shared/api report/config seam is wired here.
func runHost(ctx context.Context) error {
	return host.Run(ctx, host.ConfigFromEnv(), log.Printf)
}

// runGuestShutdown asks the VM on the given monitor socket to power off cleanly and waits until
// it is actually gone. Host-only (it pulls in platform/QEMU), stubbed in a `-tags guest` build.
func runGuestShutdown(ctx context.Context, qmpSock string) error {
	return platform.ShutdownVM(ctx, qmpSock, platform.GuestShutdownGrace)
}

// runFetchInstall downloads + verifies the signed artifact sets of BOTH chains into dest
// (assertion e), the network half of install.sh: the host bundle lands under dest/host and the
// guest image under dest/guest, each beside the manifest that verified it. The channel root,
// the release to install and the release keyring PEM come from the environment (install.sh
// sets BRIARD_CHANNEL_URL + BRIARD_RELEASE + BRIARD_KEYRING, the last the bundled release
// public key). It lives here, not main.go, so a `-tags guest` build never links
// install/net/http (the trim).
//
// All-or-nothing across the two chains as well as within each: dest appears only once both
// have verified, so install.sh never sees a host bundle without the guest image it was
// published beside ([B.86e]: host/stable + guest/stable IS the tested pair, by construction).
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
	if _, err := os.Stat(dest); err == nil {
		return fmt.Errorf("staging dest %s already exists", dest)
	}
	tmp, err := os.MkdirTemp(filepath.Dir(dest), ".briard-install-")
	if err != nil {
		return fmt.Errorf("staging dir: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			os.RemoveAll(tmp)
		}
	}()
	// The host chain has a platform level and this binary installs the Linux arm; the guest
	// image is the same VM on every host and has none.
	for chain, platform := range map[string]string{install.ChainHost: install.PlatformLinux, install.ChainGuest: ""} {
		f := &install.Fetcher{BaseURL: base, Chain: chain, Platform: platform, Keyring: kr, Logf: log.Printf}
		if err := f.FetchVerified(ctx, chainTarget(chain, release), filepath.Join(tmp, chain)); err != nil {
			return err
		}
	}
	if err := os.Rename(tmp, dest); err != nil {
		return fmt.Errorf("place staging dir: %w", err)
	}
	committed = true
	return nil
}

// chainTarget maps the ONE release selector an installer names onto each chain's target. The
// pointer words are the same on every chain. An exact id is a host id (`v3.<date>.<rev>`), and
// its guest counterpart is the same tree's guest release, `guest.<date>.<rev>` -- the two are
// staged from one commit by publish-release.sh, which is what makes the swap well-defined. A
// selector that does not fit either shape is passed through and fails at the fetch, loudly.
func chainTarget(chain, release string) string {
	if release == install.TargetStable || release == install.TargetLatest || chain == install.ChainHost {
		return release
	}
	if _, rest, ok := strings.Cut(release, "."); ok {
		return install.ChainGuest + "." + rest
	}
	return release
}

// runStageManifest writes dir/manifest.json describing the artifacts staged in dir as one
// release of one chain -- the release pipeline's writer, so the bytes a release publishes are
// described by the same code that installs them (agent/install.WriteManifest). Host-side for
// the same reason as runFetchInstall: it lives in the install package, which the `-tags guest`
// trim excludes.
func runStageManifest(dir, chain, platform, version string) error {
	return install.WriteManifest(dir, chain, platform, version)
}
