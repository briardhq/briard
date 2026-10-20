package nic

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
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

// built is up here: IFF_UP is the flag the agent set on the pass that made the child, and
// nothing else touches it.
func built(dev string) bool { return up(dev) }

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

// DefaultRoute reads the guest's parent from /proc/net/route -- the MAIN table, which is the
// point (see Select) -- by parseDefaultRouteDev's rule, with prefer winning a tie. "" if none.
func DefaultRoute(prefer string) string {
	return parseDefaultRouteDev(readText(procRoute), Carrier, prefer)
}

// Carrier reports whether dev has a link: /sys/class/net/<dev>/carrier reads 1. IFF_UP (up) is
// not the question -- a pulled cable leaves it set -- and the file of an admin-down device cannot
// be read, which reads as no carrier.
func Carrier(dev string) bool {
	return dev != "" && strings.TrimSpace(readText("/sys/class/net/"+dev+"/carrier")) == "1"
}

// lowerOf names the device dev was created on -- a macvtap child's parent -- from the kernel's
// `lower_<dev>` adjacency link. "" when dev is stacked on nothing (a tap, a NIC, a bridge port,
// whose bridge is its master instead). The link rather than `iflink`: a veth's iflink is its
// peer's index, in whatever namespace the peer is in, and would name an unrelated device here.
func lowerOf(dev string) string {
	m, _ := filepath.Glob("/sys/class/net/" + dev + "/lower_*")
	if len(m) != 1 {
		return ""
	}
	return strings.TrimPrefix(filepath.Base(m[0]), "lower_")
}

// Settling names a device the selection should wait for: hardware behind it, carrier, a port of
// nothing, and no default route yet -- a NIC whose DHCP is still in flight at boot. A bridge's or
// a bond's port is excluded, because its master holds the route. "" when there is none.
func Settling() string {
	var hw []string
	for _, d := range Candidates() {
		if exists("/sys/class/net/"+d+"/device") && master(d) == "" {
			hw = append(hw, d)
		}
	}
	return parseSettling(readText(procRoute), hw, Carrier)
}

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

// canProbe: the probe creates a device, which only root may. An unprivileged report card skips
// it rather than condemning the host for an EPERM.
func canProbe() bool { return os.Geteuid() == 0 }

// THE PROCFS AND SYSFS READERS. Each reads one small kernel file whole and hands the text to a
// pure parser in the shared files, so the format handling is unit-tested from any OS and only
// the read is Linux's.

// procRoute is the kernel's routing table as text. Named once because two readers parse it for
// two different columns, and their tests parse the same fixture.
const procRoute = "/proc/net/route"

// procARP is the kernel's IPv4 neighbour table as text.
//
// ⚠️ IPv4-ONLY, and legacy. It is the right file while addressing is v4 (by design it
// stays); the day it is not, this reader is the first thing that moves.
const procARP = "/proc/net/arp"

// readText reads a small procfs or sysfs file whole. An unreadable file reads as "", which is the
// answer every caller in this package wants: absence is "no evidence" here, never an error to
// propagate (see Read's best-effort rule), and there is nothing a caller could do with the error
// that "" does not already say.
//
// Reading whole rather than scanning is what lets the parser above it be pure: these
// files are a few hundred bytes, and the split is what makes the format handling testable.
func readText(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(b)
}

// defaultGateway reads the next hop of the main table's default route on dev, from
// /proc/net/route -- the same file and the same table DefaultRoute uses, for the same reason.
func defaultGateway(dev string) string { return parseDefaultGateway(readText(procRoute), dev) }

// arpWait bounds the fingerprint's neighbour resolution. A gateway is by definition something
// this host has been talking to, so its entry is usually already in the table; this is the budget
// for provoking one that is not, and it is short because a fingerprint must never be able to
// delay a bring-up.
const arpWait = 750 * time.Millisecond

// neighbourMAC resolves ip's link address on dev. It provokes the kernel the same way the
// report card's address probe does -- one datagram at the discard port, then read the kernel's
// own neighbour table -- so it needs no raw socket and no capability.
func neighbourMAC(ctx context.Context, dev, ip string) string {
	if mac := arpTableMAC(dev, ip); mac != "" {
		return mac
	}
	if c, err := net.DialTimeout("udp4", net.JoinHostPort(ip, "9"), arpWait); err == nil {
		_, _ = c.Write([]byte{0})
		_ = c.Close()
	}
	for range 10 {
		if mac := arpTableMAC(dev, ip); mac != "" {
			return mac
		}
		select {
		case <-ctx.Done():
			return ""
		case <-time.After(arpWait / 10):
		}
	}
	return ""
}

// arpTableMAC reads a RESOLVED entry for ip on dev out of /proc/net/arp.
func arpTableMAC(dev, ip string) string { return parseARPMAC(readText(procARP), dev, ip) }

// disableIPv6Path names the procfs knob that stops a device autoconfiguring. See ensureMacvtap for
// why the host must not autoconfigure on a device carrying the GUEST's MAC.
func disableIPv6Path(dev string) string {
	return "/proc/sys/net/ipv6/conf/" + dev + "/disable_ipv6"
}

// ipv6Disabled reports whether dev is set not to autoconfigure. A host whose kernel has no IPv6
// at all publishes no such file, and there is nothing to disable -- that reads as satisfied, so
// the tick does not chase a knob the machine does not have.
//
// It reads with os.ReadFile rather than through readText because here the ERROR is the
// answer: "no such file" means satisfied, while readText flattens an unreadable file and an empty
// one to the same "".
func ipv6Disabled(dev string) bool {
	b, err := os.ReadFile(disableIPv6Path(dev))
	if err != nil {
		return true
	}
	return strings.TrimSpace(string(b)) == "1"
}

// flags reads /sys/class/net/<dev>/flags -- the raw IFF_ bitmap, which is where ALLMULTI is
// visible at all (net.Interface does not carry it).
func flags(dev string) int64 { return parseIfFlags(readText("/sys/class/net/" + dev + "/flags")) }

// asMember is the identity here: a Linux bridge is the user's, so the guest never moves by
// changing its members.
func asMember(dev string) (string, string) { return dev, "" }

// Uplink names the device whose presence and carrier stand for parent's -- parent itself here --
// and whether the guest moves off it by a member swap: never on Linux, where the agent changes no
// bridge's members.
func Uplink(parent string) (string, bool) { return parent, false }
