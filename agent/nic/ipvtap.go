package nic

import (
	"context"
	"fmt"
	"net"
	"slices"
	"strings"
)

// THE IPVTAP SUBSTRATE: the guest's L2 on a wireless station.
//
// One 802.11 station carries one MAC, and a macvtap child gives the guest a MAC of its own, so
// its frames die at the access point. ipvlan in L2 mode gives every child the PARENT's MAC, which
// the station can carry. What it costs is the demux: ipvlan delivers a frame from the wire to a
// child only when the destination IP is configured ON THAT CHILD, host-side. It learns nothing
// from traffic. So the guest's live addresses are copied onto the children (Hold), and the
// children live in a namespace of their own, because an address configured in the host's own
// namespace is an address the HOST answers for: it pings itself over `lo` and the guest never
// sees the frame. ipvlan's address table belongs to the parent, so a child in another namespace
// still demuxes the parent's traffic, and qemu opens /dev/tapN across namespaces.
//
// Everything else is macvtap's shape: two children, ALLMULTI on each, IPv6 off, and the private
// tap for host<->guest, because ipvlan never hands a frame the host sends out the parent to a
// child, and a child's broadcasts never reach the host stack.

// HoldNS is the namespace the ipvtap children live in. Nothing runs in it and nothing is routed
// through it: it exists only so the copied addresses belong to no stack that would answer them.
const HoldNS = "briard-hold"

// ipvtapAddArgs renders the creation of one ipvtap child of parent, in the host's namespace.
// `mode l2` keeps the guest a real L2 citizen (ARP, DHCP, mDNS) on the parent's MAC; `bridge`
// lets the two children reach each other. The child is moved into HoldNS before it is ever up.
func ipvtapAddArgs(dev, parent string) []string {
	return []string{"link", "add", "link", parent, "name", dev, "type", "ipvtap", "mode", "l2", "bridge"}
}

// holdIPv6Off is run inside HoldNS when it is created. A child carries the PARENT's MAC, so an
// IPv6 stack on it derives the host's own link-local address, and ipvlan would then deliver the
// host's IPv6 traffic to the child. `default` covers every device moved in afterwards, `all` any
// already there. A kernel with no IPv6 has neither file, and nothing to turn off.
const holdIPv6Off = `set -e; for k in all default; do f=/proc/sys/net/ipv6/conf/$k/disable_ipv6; [ ! -e "$f" ] || echo 1 >"$f"; done`

// ensureHoldNS creates HoldNS if it is not there. /run/netns is tmpfs, so every boot creates it
// again, and IPv6 is turned off on the pass that creates it.
func ensureHoldNS(ctx context.Context) error {
	if exists("/run/netns/" + HoldNS) {
		return nil
	}
	if out, err := ip(ctx, "netns", "add", HoldNS); err != nil {
		return fmt.Errorf("nic: netns %s: %s", HoldNS, firstLine(out, err))
	}
	if out, err := ip(ctx, "netns", "exec", HoldNS, "sh", "-c", holdIPv6Off); err != nil {
		return fmt.Errorf("nic: ipv6 off in %s: %s", HoldNS, firstLine(out, err))
	}
	return nil
}

// ensureIpvtap creates one ipvtap child of parent inside HoldNS and gives it ALLMULTI. held is
// what HoldNS holds right now (holdLinks), read once per pass by the caller.
func ensureIpvtap(ctx context.Context, dev, parent string, held map[string][]string) error {
	flags, created := held[dev], false
	if flags == nil {
		// A device by this name in the HOST's namespace is ours from the macvtap substrate,
		// left by a move from wire to Wi-Fi. It holds the name the child needs.
		if exists("/sys/class/net/" + dev) {
			if out, err := ip(ctx, "link", "del", dev); err != nil {
				return fmt.Errorf("nic: removing %s before re-creating it as ipvtap: %s", dev, firstLine(out, err))
			}
		}
		if out, err := ip(ctx, ipvtapAddArgs(dev, parent)...); err != nil {
			return fmt.Errorf("nic: ipvtap %s on %s: %s", dev, parent, firstLine(out, err))
		}
		// Moved while still down, so it never runs an IPv6 stack in the host's namespace.
		if out, err := ip(ctx, "link", "set", "dev", dev, "netns", HoldNS); err != nil {
			return fmt.Errorf("nic: moving %s into %s: %s", dev, HoldNS, firstLine(out, err))
		}
		created = true
	}
	// ALLMULTI for the same reason as on a macvtap child (ensureMacvtap): ipvlan's multicast
	// filter is per child, and without it inbound mDNS never reaches the guest.
	if !slices.Contains(flags, "ALLMULTI") {
		if out, err := ip(ctx, append([]string{"-n", HoldNS}, allmulticastOnArgs(dev)...)...); err != nil {
			return fmt.Errorf("nic: allmulticast on %s: %s", dev, firstLine(out, err))
		}
	}
	// Up only on the pass that made it -- the rule above Converge.
	if created {
		if out, err := ip(ctx, "-n", HoldNS, "link", "set", dev, "up"); err != nil {
			return fmt.Errorf("nic: %s up: %s", dev, firstLine(out, err))
		}
	}
	return nil
}

// holdLinks lists HoldNS's devices with their flags, from one `ip` call. An absent namespace is
// an empty map: nothing is held.
func holdLinks(ctx context.Context) map[string][]string {
	if !exists("/run/netns/" + HoldNS) {
		return map[string][]string{}
	}
	out, err := ip(ctx, "-n", HoldNS, "-o", "link", "show")
	if err != nil {
		return map[string][]string{}
	}
	return parseLinks(string(out))
}

// parseLinks reads `ip -o link show` into name -> flags. Pure, so the format is unit-tested:
//
//	7: briard0@if3: <BROADCAST,MULTICAST,ALLMULTI,UP,LOWER_UP> mtu 1500 ...
//
// The `@if3` names the parent by its index in ANOTHER namespace, and is cut off.
func parseLinks(text string) map[string][]string {
	links := map[string][]string{}
	for l := range strings.SplitSeq(text, "\n") {
		f := strings.Fields(l)
		if len(f) < 3 {
			continue
		}
		name, _, _ := strings.Cut(strings.TrimSuffix(f[1], ":"), "@")
		links[name] = strings.Split(strings.Trim(f[2], "<>"), ",")
	}
	return links
}

// Hold makes the IPv4 addresses on each child in HoldNS exactly want[child], each a /32: added
// when missing, removed when no longer wanted. It is how ipvlan learns which frames are the
// guest's. The caller decides what may be held; this only does it. One `ip` read per call, and a
// write only for what changed, because it runs on every status tick.
func Hold(ctx context.Context, want map[string][]string) error {
	out, err := ip(ctx, "-n", HoldNS, "-o", "-4", "addr", "show")
	if err != nil {
		return fmt.Errorf("nic: reading %s: %s", HoldNS, firstLine(out, err))
	}
	have := parseInet(string(out))
	for dev, addrs := range want {
		for _, a := range addrs {
			if !slices.Contains(have[dev], a+"/32") {
				if out, err := ip(ctx, "-n", HoldNS, "addr", "replace", a+"/32", "dev", dev); err != nil {
					return fmt.Errorf("nic: holding %s on %s: %s", a, dev, firstLine(out, err))
				}
			}
		}
		for _, h := range have[dev] {
			if a, _, _ := strings.Cut(h, "/"); !slices.Contains(addrs, a) {
				if out, err := ip(ctx, "-n", HoldNS, "addr", "del", h, "dev", dev); err != nil {
					return fmt.Errorf("nic: releasing %s from %s: %s", h, dev, firstLine(out, err))
				}
			}
		}
	}
	return nil
}

// parseInet reads `ip -o -4 addr show` into device -> its `inet A/P` values. Pure, for the same
// reason as parseLinks:
//
//	7: briard0    inet 192.168.7.120/32 scope global briard0\       valid_lft forever ...
func parseInet(text string) map[string][]string {
	out := map[string][]string{}
	for l := range strings.SplitSeq(text, "\n") {
		f := strings.Fields(l)
		for i, tok := range f {
			if tok == "inet" && i+1 < len(f) && len(f) > 1 {
				out[f[1]] = append(out[f[1]], f[i+1])
			}
		}
	}
	return out
}

// IPv4s lists dev's IPv4 addresses, bare, in the host's namespace. The ipvtap gate reads the
// host's own addresses on the parent with it, every tick, because a lease can move under it.
func IPv4s(dev string) []string {
	i, err := net.InterfaceByName(dev)
	if err != nil {
		return nil
	}
	addrs, err := i.Addrs()
	if err != nil {
		return nil
	}
	var out []string
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil {
			out = append(out, n.IP.String())
		}
	}
	return out
}

// delHeld deletes dev from HoldNS if it is there. A re-parent and an uninstall delete by name in
// both namespaces, because a node that moved between Wi-Fi and wire may hold either.
func delHeld(ctx context.Context, dev string, held map[string][]string) error {
	if held[dev] == nil {
		return nil
	}
	if out, err := ip(ctx, "-n", HoldNS, "link", "del", dev); err != nil {
		return fmt.Errorf("nic: removing %s from %s: %s", dev, HoldNS, firstLine(out, err))
	}
	return nil
}
