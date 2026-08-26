package platform

import (
	"context"
	"os/exec"
)

// runIP is the iproute2 half of the platform seam. Only the RUNNER is per-OS; the argv
// renderers above it stay shared, because they are pure and their unit tests are the only
// check that the one detail a reader cannot infer (the permanent neighbour entry) is right.
// A port owes both -- its own renderers as well as its own runner -- since nothing about
// `ip route replace` translates.
func runIP(ctx context.Context, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, "ip", args...).CombinedOutput()
}
