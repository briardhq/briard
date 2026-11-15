package platform

import (
	"context"
	"os"
	"os/exec"
)

// runIP is the iproute2 half of the platform seam. Only the runner is per-OS; the argv renderers
// in route.go stay shared, because they are pure and their unit tests are the only check that the
// one detail a reader cannot infer (the permanent neighbour entry) is right. A port owes both its
// own renderers and its own runner, since nothing about `ip route replace` translates.
func runIP(ctx context.Context, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, "ip", args...).CombinedOutput()
}

// NodeRoutePresent reports whether the kernel still holds both halves SetNodeRoute installs: the
// /32 to the guest's node IP on the tap and the permanent neighbour entry pinning its MAC. An
// unreadable table reads as absent, and the repair it causes is the same idempotent install.
func NodeRoutePresent(r NodeRoute) bool {
	route, _ := os.ReadFile("/proc/net/route")
	arp, _ := os.ReadFile("/proc/net/arp")
	return routeListed(route, r.GuestIP, r.Dev, "") && neighPinned(arp, r.GuestIP, r.LLAddr, r.Dev)
}

// VIPRoutePresent reports whether the kernel still holds the /32 SetVIPRoute installed for addr.
func VIPRoutePresent(addr, dev, via string) bool {
	route, _ := os.ReadFile("/proc/net/route")
	return routeListed(route, addr, dev, via)
}
