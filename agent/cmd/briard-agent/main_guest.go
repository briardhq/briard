//go:build guest

package main

import (
	"context"
	"errors"
)

// runHost is stubbed in a `-tags guest` build: the guest-only binary excludes the host
// subsystems, so host mode is unavailable. The guest VM only ever runs `agent --guest`;
// if this fires, the wrong binary was deployed to a host.
func runHost(context.Context) error {
	return errors.New("agent: built with -tags guest (guest-only); host mode is unavailable")
}

// runFetchInstall is likewise host-only (it pulls in install/net/http): stubbed in the guest
// build so the trim holds. The installer runs the untagged host binary.
func runFetchInstall(context.Context, string) error {
	return errors.New("agent: built with -tags guest (guest-only); fetch-install is host-only")
}
