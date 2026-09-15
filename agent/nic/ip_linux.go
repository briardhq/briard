package nic

import (
	"context"
	"os/exec"
)

// ip is the iproute2 runner for this package -- the one per-OS thing in it. Everything above it
// (which device, what the message says, what the spec is) is shared and testable; a Windows host
// owes its own runner and its own Spec builder, because nothing about `ip link add ... type
// macvtap` translates to tap-windows6 and a bridge ([V5.5]).
//
// It is this package's own rather than agent/platform's: platform's runIP is unexported and
// exporting it would invite arbitrary `ip` calls from anywhere, which is the thing keeping it
// private buys. Two three-line runners behind two narrow APIs is the smaller cost.
func ip(ctx context.Context, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, "ip", args...).CombinedOutput()
}
