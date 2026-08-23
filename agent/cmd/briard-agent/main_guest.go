//go:build guest

package main

import (
	"context"
	"errors"
)

// runHost is stubbed in a `-tags guest` build: the guest-only binary excludes the host
// subsystems, so host mode is unavailable. The guest VM only ever runs `briard run --guest`;
// if this fires, the wrong binary was deployed to a host.
func runHost(context.Context) error {
	return errors.New("agent: built with -tags guest (guest-only); host mode is unavailable")
}

// runFetchInstall is likewise host-only (it pulls in install/net/http): stubbed in the guest
// build so the trim holds. The installer runs the untagged host binary.
func runFetchInstall(context.Context, string) error {
	return errors.New("agent: built with -tags guest (guest-only); fetch-install is host-only")
}

// runGuestShutdown is host-only too (it pulls in platform/QEMU). Nothing inside the guest has a
// monitor socket to talk to — it IS the VM being powered off.
func runGuestShutdown(context.Context, string) error {
	return errors.New("agent: built with -tags guest (guest-only); guest-shutdown is host-only")
}

// runStageManifest is host-only too: it lives in the install package, which the guest trim
// excludes. Only a release pipeline calls it, and a release pipeline is never a guest.
func runStageManifest(string) error {
	return errors.New("agent: built with -tags guest (guest-only); stage-manifest is host-only")
}
