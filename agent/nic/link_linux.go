package nic

import (
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
