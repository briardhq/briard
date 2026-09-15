package nic

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// The host's side of the guest's L2, built by the AGENT ([B.150](d)).
//
// It used to be a shell script the installer generated -- `net-up.sh`, with this host's NIC,
// device names, addresses and gateway baked into a heredoc, run once inline and again at every
// boot from a oneshot unit. Two things were wrong with that and neither was the shell.
//
// THE FIRST IS THAT NO RELEASE COULD REACH IT. A bug in those lines shipped a fix that landed on
// no installed node. ALLMULTI is the example: without it the guest announces its name fine and
// answers unicast, so every client caches the record and the household name resolves -- until
// that cached record expires, after which the name is dead. An install looks correct at the
// moment it finishes and only fails later, off-box.
//
// THE SECOND IS THAT A ONESHOT CANNOT ADAPT. A macvtap cannot be re-parented: you delete and
// recreate, which invalidates the fd handed to qemu, which means relaunching the guest. Only the
// thing that runs the guest can sequence that. And `Requires=briard-net.service` meant a failed
// network unit stopped the agent from starting at all -- a cable out at boot took the node fully
// dark, with nothing left running to report, retry or answer `briard status`.
//
// EVERYTHING HERE IS CHECK-FIRST. Convergence runs on the ordinary status tick, so the common
// case is "already right" and must cost a few reads and change nothing. It is also what makes
// this safe to run beside a rig that built its own devices: a tap that exists, is up and has its
// flags is converged, whoever made it.

// ifAllmulti is IFF_ALLMULTI in /sys/class/net/<dev>/flags. Read rather than set-and-hope,
// because setting it on every tick would be a netlink write per device per 10 seconds forever.
const ifAllmulti = 0x200

// Spec is the host-side L2 for one guest: which parent, and which devices on it. It is DERIVED
// from the selected device rather than configured ([B.150](c)) -- if the parent is a bridge the
// user owns, the guest gets one port on it and makes its own service identity inside; otherwise
// the guest gets macvtap children and a private link.
type Spec struct {
	// Parent is the device the guest's L2 hangs off -- nic.Choose's answer.
	Parent string
	// Bridge is whether Parent is a bridge. It is the substrate fork, and it is a QUESTION ABOUT
	// THE DEVICE rather than a mode someone selects: we never create a bridge, we only join one
	// that is already there ([B.150](c)).
	Bridge bool
	// SystemTap is the guest's eth1 -- its node IP, and where DRBD binds. Built on every
	// substrate: a macvtap child of Parent, or a plain tap enslaved to it.
	SystemTap string
	// ServiceTap is the guest's eth2, where the VIP lives. EMPTY on a bridge, where the guest
	// makes its own service identity as a macvlan child inside ([V3b.26c]) -- Windows admits one
	// tap per qemu process, and this substrate is that shape's Linux clone.
	ServiceTap string
	// PrivTap is the guest's eth3, the private host<->guest link. EMPTY on a bridge, where host
	// and guest already share one L2 and the link would be a second tap Windows cannot give us
	// ([V3b.26a]).
	PrivTap string
	// Addrs are the addresses this host puts on its own side, in CIDR form, keyed by device.
	// Under macvtap: the link's host end and this node's system-subnet /32, both on PrivTap.
	// Under a bridge: this node's system-subnet address, a /24, on the bridge itself.
	Addrs []Addr
}

// Addr is one address this host holds for its own guest's benefit.
type Addr struct{ CIDR, Dev string }

// Converge makes the host's side of the guest's L2 match s, and returns the first thing it could
// not do. It is idempotent by construction and safe to call on every tick.
func Converge(ctx context.Context, s Spec) error {
	if s.Parent == "" || s.SystemTap == "" {
		return fmt.Errorf("nic: incomplete spec (%+v)", s)
	}
	if err := ensureUp(ctx, s.Parent); err != nil {
		return err
	}
	if s.Bridge {
		// ONE PORT ON A BRIDGE WE DID NOT MAKE. Nothing is enslaved, nothing is moved, and the
		// host's own LAN identity is never touched -- which is the whole of [B.150](c): the
		// sequence that took a NIC away from NetworkManager, moved the address and the default
		// route onto a bridge of ours, and armed a watchdog to undo it if we cut the operator's
		// own SSH session, does not exist any more. The user owns the bridge; we add a port.
		if err := ensureTap(ctx, s.SystemTap); err != nil {
			return err
		}
		if err := ensureMaster(ctx, s.SystemTap, s.Parent); err != nil {
			return err
		}
		if err := ensureUp(ctx, s.SystemTap); err != nil {
			return err
		}
	} else {
		// The guest's two NIC macvtaps on the parent, created with the kernel's random MAC; the
		// launch wrapper (briard-net-wrap) pins the agent-derived per-node MAC at guest start.
		//
		// ⚠️ ORDER DOES NOT MATTER, whatever the old script's comment claimed. briard-net-wrap
		// takes <dev> <mac> <fd> triples from the AGENT's argv and the guest's ethN follows
		// qemu's -netdev order, not the order the devices were created in.
		for _, t := range []string{s.SystemTap, s.ServiceTap} {
			if t == "" {
				continue
			}
			if err := ensureMacvtap(ctx, t, s.Parent); err != nil {
				return err
			}
		}
		if s.PrivTap != "" {
			if err := ensureTap(ctx, s.PrivTap); err != nil {
				return err
			}
			if err := ensureUp(ctx, s.PrivTap); err != nil {
				return err
			}
		}
	}
	for _, a := range s.Addrs {
		if err := ensureAddr(ctx, a); err != nil {
			return err
		}
	}
	return nil
}

// Rebuild converges s after DELETING the guest's NIC devices, so they are recreated on s.Parent
// whatever they were hanging off before. It is the re-parent's half of [B.150](e).
//
// The delete is the whole difference from Converge, and it is not optional: a macvtap cannot be
// re-parented, and Converge is check-first -- a child that still EXISTS (a parent renamed rather
// than removed, say) would be left attached to the old device and reported converged. Deleting
// first is what makes "the guest's L2 hangs off this parent now" true rather than hoped.
//
// ⚠️ THE PRIVATE TAP IS NOT TOUCHED. It has no parent -- it is a point-to-point wire to our own
// guest -- so a re-parent has nothing to do to it, and recreating it would drop the host's own
// addresses and the permanent neighbour entry that make the reboot gate and the VIP route work.
func Rebuild(ctx context.Context, s Spec) error {
	if s.Bridge {
		// Nothing to rebuild: the port is a plain tap on a bridge the USER owns, and a bridge
		// that went away is theirs to restore ([B.150](c)).
		return Converge(ctx, s)
	}
	for _, t := range []string{s.SystemTap, s.ServiceTap} {
		if t != "" && exists("/sys/class/net/"+t) {
			if out, err := ip(ctx, "link", "del", t); err != nil {
				return fmt.Errorf("nic: removing %s before re-parenting: %s", t, firstLine(out, err))
			}
		}
	}
	return Converge(ctx, s)
}

// Converged reports whether Converge would change nothing. It exists so the tick can stay silent
// on the overwhelmingly common path -- and so the agent can SAY that the network is not what it
// should be without having tried to fix it yet.
func Converged(s Spec) bool {
	if s.Parent == "" || !up(s.Parent) {
		return false
	}
	for _, t := range []string{s.SystemTap, s.ServiceTap, s.PrivTap} {
		if t == "" {
			continue
		}
		if !exists("/sys/class/net/"+t) || !up(t) {
			return false
		}
		// The two flags on a macvtap child that are not kernel defaults. BOTH are checked, and
		// the IPv6 one is the whole of [B.106]'s repair path: a device that already exists is
		// never re-created, so if this is not the question the tick asks, a host installed before
		// the fix keeps autoconfiguring on the guest's MAC forever -- and every reachability
		// check still passes, which is why it went unnoticed the first time.
		if !s.Bridge && t != s.PrivTap && (flags(t)&ifAllmulti == 0 || !ipv6Disabled(t)) {
			return false
		}
	}
	if s.Bridge && s.SystemTap != "" && master(s.SystemTap) != s.Parent {
		return false
	}
	for _, a := range s.Addrs {
		// An empty CIDR is "this node takes no address here", which ensureAddr treats as a no-op --
		// so Converged must too, or the tick would report a mismatch it then could not fix and
		// would say so on every pass forever. (Every agent-less harness has one: the private link
		// is addressed by the rig, not from a config this agent was given.)
		if a.CIDR != "" && !hasAddr(a.Dev, a.CIDR) {
			return false
		}
	}
	return true
}

// ensureMacvtap creates one macvtap child and gives it the two flags that are not defaults.
func ensureMacvtap(ctx context.Context, dev, parent string) error {
	if !exists("/sys/class/net/" + dev) {
		if out, err := ip(ctx, "link", "add", "link", parent, "name", dev, "type", "macvtap", "mode", "bridge"); err != nil {
			return fmt.Errorf("nic: macvtap %s on %s: %s", dev, parent, firstLine(out, err))
		}
	}
	// IPv6 OFF, and OUTSIDE the create branch on purpose. The host end of a macvtap holds no
	// address, but the device carries the GUEST's MAC -- so a host that autoconfigures on it
	// derives the SAME EUI-64 identifier the guest derives, on the same L2: a duplicate address
	// whose winner DAD picks and whose loser silently drops it, and the host's avahi joins mDNS
	// on the guest's segment, where the name is the guest's to publish. Ubuntu ships
	// net.ipv6.conf.default.accept_ra=1 and every new device inherits the "default" values, so
	// that is what happens unless we say otherwise ([B.106]). On a fresh device this runs before
	// it is up, so no advertisement can be accepted at all; on a device that already exists the
	// write FLUSHES what it already picked up, which is how an upgraded install gets repaired.
	//
	// A procfs write rather than sysctl(8), because this runs on stock hosts and on NixOS alike.
	if p := "/proc/sys/net/ipv6/conf/" + dev + "/disable_ipv6"; exists(p) {
		if err := os.WriteFile(p, []byte("1\n"), 0o644); err != nil {
			return fmt.Errorf("nic: disable ipv6 on %s: %w", dev, err)
		}
	}
	if err := ensureUp(ctx, dev); err != nil {
		return err
	}
	// ALLMULTI, or inbound multicast never reaches the guest. The guest's avahi joins 224.0.0.251
	// on its VIRTIO NIC inside the VM, and nothing carries that join out to this device: qemu
	// only emits a NIC_RX_FILTER_CHANGED event, and the code that acts on it is libvirt's, gated
	// on trustGuestRxFilters, and we run qemu directly. So this macvtap's multicast list holds
	// only the all-hosts group, and macvlan_broadcast() drops every inbound mDNS query on its
	// per-child test_bit(hash, vlan->mc_filter). ALLMULTI is what fills that bitmap.
	//
	// Egress does not need it, and that asymmetry is the trap: without this the guest still
	// ANNOUNCES its name fine and answers unicast queries, so every client caches the record and
	// the household name resolves -- until that cached record expires, after which nothing can
	// query the guest and the name is dead.
	//
	// THE CHILD, NOT THE PARENT: adding the group to the parent's own multicast list does not
	// help, because the gate is the per-child filter. Multicast only -- deliberately not
	// promiscuous, which would pull every unicast frame on the segment off the wire for nothing.
	if flags(dev)&ifAllmulti == 0 {
		if out, err := ip(ctx, "link", "set", dev, "allmulticast", "on"); err != nil {
			return fmt.Errorf("nic: allmulticast on %s: %s", dev, firstLine(out, err))
		}
	}
	return nil
}

// ensureTap creates a plain tap if it is not there. The private host<->guest link and the bridge
// substrate's one port are both this shape.
func ensureTap(ctx context.Context, dev string) error {
	if exists("/sys/class/net/" + dev) {
		return nil
	}
	if out, err := ip(ctx, "tuntap", "add", dev, "mode", "tap"); err != nil {
		return fmt.Errorf("nic: tap %s: %s", dev, firstLine(out, err))
	}
	return nil
}

// ensureMaster enslaves dev to the bridge, if it is not already a port of it. A device that is a
// port of some OTHER bridge is re-enslaved: the recorded parent is the answer, and a leftover
// membership from a previous parent is exactly what a re-parent has to fix.
func ensureMaster(ctx context.Context, dev, bridge string) error {
	if master(dev) == bridge {
		return nil
	}
	if out, err := ip(ctx, "link", "set", dev, "master", bridge); err != nil {
		return fmt.Errorf("nic: %s -> %s: %s", dev, bridge, firstLine(out, err))
	}
	return nil
}

func ensureUp(ctx context.Context, dev string) error {
	if up(dev) {
		return nil
	}
	if out, err := ip(ctx, "link", "set", dev, "up"); err != nil {
		return fmt.Errorf("nic: %s up: %s", dev, firstLine(out, err))
	}
	return nil
}

// ensureAddr puts one address on one device. `addr replace` rather than `add`, so a prefix that
// changed (the macvtap /32 vs the bridge substrate's /24) converges rather than collides.
func ensureAddr(ctx context.Context, a Addr) error {
	if a.CIDR == "" || a.Dev == "" || hasAddr(a.Dev, a.CIDR) {
		return nil
	}
	if out, err := ip(ctx, "addr", "replace", a.CIDR, "dev", a.Dev); err != nil {
		return fmt.Errorf("nic: %s on %s: %s", a.CIDR, a.Dev, firstLine(out, err))
	}
	return nil
}

// IsBridge reports whether dev is a bridge -- the substrate fork, asked of the device itself.
// /sys/class/net/<dev>/bridge exists only on one, whoever created it and however.
func IsBridge(dev string) bool { return dev != "" && exists("/sys/class/net/"+dev+"/bridge") }

// ipv6Disabled reports whether dev is set not to autoconfigure. A host whose kernel has no IPv6
// at all publishes no such file, and there is nothing to disable -- that reads as satisfied, so
// the tick does not chase a knob the machine does not have.
func ipv6Disabled(dev string) bool {
	b, err := os.ReadFile("/proc/sys/net/ipv6/conf/" + dev + "/disable_ipv6")
	if err != nil {
		return true
	}
	return strings.TrimSpace(string(b)) == "1"
}

// up reads IFF_UP off the device rather than asking `ip`, because this runs on every tick.
func up(dev string) bool {
	i, err := net.InterfaceByName(dev)
	return err == nil && i.Flags&net.FlagUp != 0
}

// flags reads /sys/class/net/<dev>/flags -- the raw IFF_ bitmap, which is where ALLMULTI is
// visible at all (net.Interface does not carry it).
func flags(dev string) int64 {
	b, err := os.ReadFile("/sys/class/net/" + dev + "/flags")
	if err != nil {
		return 0
	}
	n, err := strconv.ParseInt(strings.TrimPrefix(strings.TrimSpace(string(b)), "0x"), 16, 64)
	if err != nil {
		return 0
	}
	return n
}

// master names the bridge dev is a port of, "" when it is a port of nothing.
//
// filepath.Base, not a prefix trim: the symlink is a MULTI-LEVEL relative path
// (`../../devices/virtual/net/br0`), so stripping one `../` leaves a path that matches no bridge
// name — which would make Converged answer false forever and the tick re-converge on every pass.
func master(dev string) string {
	l, err := os.Readlink("/sys/class/net/" + dev + "/master")
	if err != nil {
		return ""
	}
	return filepath.Base(l)
}

// hasAddr reports whether dev already holds exactly this address AND prefix. The prefix is part
// of the question: the same address as a /32 and as a /24 are two different routing claims.
func hasAddr(dev, cidr string) bool {
	i, err := net.InterfaceByName(dev)
	if err != nil {
		return false
	}
	addrs, err := i.Addrs()
	if err != nil {
		return false
	}
	for _, a := range addrs {
		if a.String() == cidr {
			return true
		}
	}
	return false
}
