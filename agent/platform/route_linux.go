package platform

import (
	"context"
	"os/exec"
)

// runIP is the iproute2 half of the platform seam. Only the runner is per-OS; the argv renderers
// in route.go stay shared, because they are pure and their unit tests are the only check that the
// one detail a reader cannot infer (the permanent neighbour entry) is right. A port owes both its
// own renderers and its own runner, since nothing about `ip route replace` translates.
func runIP(ctx context.Context, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, "ip", args...).CombinedOutput()
}
