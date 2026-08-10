//go:build !guest

package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"

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

// runFetchInstall downloads + verifies the signed artifact set into dest (assertion e),
// the network half of install.sh. The channel base URL and the release keyring PEM come from
// the environment (install.sh sets BRIARD_CHANNEL_URL + BRIARD_KEYRING, the latter the bundled
// release public key). It lives here, not main.go, so a `-tags guest` build never links
// install/net/http (the trim).
func runFetchInstall(ctx context.Context, dest string) error {
	base := os.Getenv("BRIARD_CHANNEL_URL")
	if base == "" {
		return errors.New("BRIARD_CHANNEL_URL unset (the release channel base URL)")
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
	f := &install.Fetcher{BaseURL: base, Keyring: kr, Logf: log.Printf}
	return f.FetchVerified(ctx, dest)
}
