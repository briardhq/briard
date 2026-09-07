//go:build !guest

package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"

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
	// The host chain first -- it has a platform level and this binary installs the Linux arm --
	// and then the guest release ITS MANIFEST NAMES ([B.86i]): the guest image is a function of its
	// inputs and is re-published only when they change, so its id is no longer derivable from the
	// host id, and the host manifest is where the pairing lives. One selector still installs one
	// tested pair; it is just the host side that resolves it.
	hf := &install.Fetcher{BaseURL: base, Chain: install.ChainHost, Platform: install.PlatformLinux, Keyring: kr, Logf: log.Printf}
	if err := hf.FetchVerified(ctx, release, filepath.Join(tmp, install.ChainHost)); err != nil {
		return err
	}
	hm, err := install.ReadManifest(filepath.Join(tmp, install.ChainHost, install.ManifestName))
	if err != nil {
		return fmt.Errorf("read the fetched host manifest: %w", err)
	}
	if hm.Guest == "" {
		return fmt.Errorf("host release %s names no guest release -- published before [B.86i]; the alpha reinstalls from a current channel", hm.Version)
	}
	gf := &install.Fetcher{BaseURL: base, Chain: install.ChainGuest, Keyring: kr, Logf: log.Printf}
	if err := gf.FetchVerified(ctx, hm.Guest, filepath.Join(tmp, install.ChainGuest)); err != nil {
		return err
	}
	if err := os.Rename(tmp, dest); err != nil {
		return fmt.Errorf("place staging dir: %w", err)
	}
	committed = true
	return nil
}

// runFetchUpdate is the update unit's verb ([B.86a]): resolve target on the host chain, decide
// against the installed manifest, stage + arm the agent if due, and return the one line the run
// ended on. The layout comes from the same env the agent unit carries (UPDATE_BASE /
// UPDATE_RUN_DIR), which the frozen unit passes through -- so the candidate lands exactly where
// briard-exec looks for it.
func runFetchUpdate(ctx context.Context, target string) (string, error) {
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
	u := &install.Update{
		Fetcher: &install.Fetcher{BaseURL: base, Chain: install.ChainHost, Platform: install.PlatformLinux, Keyring: kr, Logf: log.Printf},
		Layout:  selfupdate.New(os.Getenv("UPDATE_BASE"), os.Getenv("UPDATE_RUN_DIR")),
		Logf:    log.Printf,
	}
	return u.Run(ctx, target)
}

// runStageManifest writes dir/manifest.json describing the artifacts staged in dir as one
// release of one chain -- the release pipeline's writer, so the bytes a release publishes are
// described by the same code that installs them (agent/install.WriteManifest). Host-side for
// the same reason as runFetchInstall: it lives in the install package, which the `-tags guest`
// trim excludes.
func runStageManifest(dir, chain, platform, version, system, minHost, guest, inputs string) error {
	return install.WriteManifest(dir, chain, platform, version, system, minHost, guest, inputs)
}
