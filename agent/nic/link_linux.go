package nic

import (
	"context"
	"errors"
	"net"
	"os"
)

// The link facts, read from sysfs and procfs. They are the per-OS half of this package: what a
// device IS (there, up, a bridge, a station, whose port) and which one holds the default route.
// Everything that decides on the answers is shared.

// Exists reports whether this host has a device named dev, up or not.
func Exists(dev string) bool { return exists("/sys/class/net/" + dev) }

// up reads IFF_UP off the device rather than asking `ip`, because this runs on every tick.
func up(dev string) bool {
	i, err := net.InterfaceByName(dev)
	return err == nil && i.Flags&net.FlagUp != 0
}

// IsBridge reports whether dev is a bridge -- the substrate fork, asked of the device itself.
// /sys/class/net/<dev>/bridge exists only on one, whoever created it and however.
func IsBridge(dev string) bool { return dev != "" && exists("/sys/class/net/"+dev+"/bridge") }

// Wireless reports whether dev is an 802.11 station. Both spellings: `wireless` is the old
// extension, `phy80211` the cfg80211 one, and drivers differ on which they publish.
func Wireless(dev string) bool {
	base := "/sys/class/net/" + dev
	return exists(base+"/wireless") || exists(base+"/phy80211")
}

// master names the bridge dev is a port of, "" when it is a port of nothing.
func master(dev string) string {
	l, err := os.Readlink("/sys/class/net/" + dev + "/master")
	if err != nil {
		return ""
	}
	return parseMaster(l)
}

// DefaultRoute reads the interface owning the default route (destination 00000000) from
// /proc/net/route -- the MAIN table, which is the point (see Choose). "" if none.
func DefaultRoute() string { return parseDefaultRouteDev(readText(procRoute)) }

// The writers, as iproute2. Each returns the one line of `ip`'s complaint worth showing.

func ipDo(ctx context.Context, args ...string) error {
	if out, err := ip(ctx, args...); err != nil {
		return errors.New(firstLine(out, err))
	}
	return nil
}

func createTap(ctx context.Context, dev string) error { return ipDo(ctx, tapAddArgs(dev)...) }

func joinBridge(ctx context.Context, dev, bridge string) error {
	return ipDo(ctx, "link", "set", dev, "master", bridge)
}

func linkUp(ctx context.Context, dev string) error { return ipDo(ctx, "link", "set", dev, "up") }

// addAddr uses `addr replace` rather than `add`, so a prefix that changed converges rather than
// collides.
func addAddr(ctx context.Context, a Addr) error {
	return ipDo(ctx, "addr", "replace", a.CIDR, "dev", a.Dev)
}

func delAddr(ctx context.Context, a Addr) error {
	return ipDo(ctx, "addr", "del", a.CIDR, "dev", a.Dev)
}

func addRoute(ctx context.Context, r Route) error {
	args := []string{"route", "replace", r.Dst + "/32", "dev", r.Dev}
	if r.Src != "" {
		args = append(args, "src", r.Src)
	}
	return ipDo(ctx, args...)
}

func delRoute(ctx context.Context, r Route) error {
	return ipDo(ctx, "route", "del", r.Dst+"/32", "dev", r.Dev)
}

// hasRoute reads the main table rather than asking `ip`, because it runs on every tick.
func hasRoute(r Route) bool { return parseHostRoute(readText(procRoute), r.Dev, r.Dst) }

// substrateBuildable is nil on Linux: every substrate the spec can name is one this host builds.
func substrateBuildable(Spec) error { return nil }
