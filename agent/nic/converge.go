package nic

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// The host's side of the guest's L2, built and kept by the agent: never by an installer-written
// script, which no release can fix once installed, nor by a unit of its own, since a macvtap cannot
// be re-parented, only deleted and recreated, which invalidates the fd handed to qemu -- so only
// the thing that runs the guest can sequence it (agent/host/network.go).
//
// Everything here is check-first: it runs at every network check of the observe loop, so the
// common "already right" case must cost a few reads and change nothing. That also makes it safe
// beside a rig that built its own devices: a tap that exists, is up and has its flags is
// converged, whoever made it.

// ifAllmulti is IFF_ALLMULTI in /sys/class/net/<dev>/flags. Read rather than set-and-hope,
// because setting it at every network check would be a netlink write per device per check forever.
const ifAllmulti = 0x200

// The argv and the one path, rendered purely (as agent/platform/route.go does). Each carries a
// detail the call site does not show -- `type macvtap mode bridge`, `mode tap`, `allmulticast on`,
// the procfs knob's exact path -- whose rationale lives at the caller, where the decision is. Pure
// so a unit test pins them, rather than a 12-minute rig being the only thing to notice a change.

// macvtapAddArgs renders the creation of one macvtap child of parent. `mode bridge` is the mode
// that lets the children talk to each other (the guest's two NICs); it does not make a bridge and
// it does not reach the parent, which is the isolation agent/platform/route.go's /32 works around.
func macvtapAddArgs(dev, parent string) []string {
	return []string{"link", "add", "link", parent, "name", dev, "type", "macvtap", "mode", "bridge"}
}

// tapAddArgs renders the creation of a plain tap -- the private host<->guest link, and the bridge
// substrate's one port.
func tapAddArgs(dev string) []string { return []string{"tuntap", "add", dev, "mode", "tap"} }

// allmulticastOnArgs renders the ALLMULTI set. See ensureMacvtap for why the guest is unreachable
// by name without it, and why it goes on the child.
func allmulticastOnArgs(dev string) []string {
	return []string{"link", "set", dev, "allmulticast", "on"}
}

// Spec is the host-side L2 for one guest: which parent, and which devices on it. It is derived
// from the selected device, not configured: on a bridge the user owns, the guest gets one port and
// makes its own service identity inside; on a wireless station, ipvtap children in HoldNS and a
// private link; otherwise macvtap children and a private link.
type Spec struct {
	// Parent is the device the guest's L2 hangs off -- nic.Choose's answer.
	Parent string
	// Bridge is whether Parent is a bridge: the substrate fork, a question about the device rather
	// than a mode someone selects. We never create a bridge, only join one already there.
	Bridge bool
	// Ipvtap is whether Parent is a wireless station, whose one MAC is all the guest may use
	// (ipvtap.go). The children are ipvtap rather than macvtap and live in HoldNS; the rest of
	// the spec is macvtap's. Never true together with Bridge: a managed station is no bridge.
	Ipvtap bool
	// SystemTap is the guest's eth1 -- its node IP, and where DRBD binds. Built on every
	// substrate: a macvtap child of Parent, an ipvtap child of it in HoldNS, or a plain tap
	// enslaved to it.
	SystemTap string
	// ServiceTap is the guest's eth2, where the VIP lives. Empty on a bridge, where the guest makes
	// its own service identity as a macvlan child inside -- Windows admits one tap per qemu process,
	// and this substrate is that shape's Linux clone.
	ServiceTap string
	// PrivTap is the guest's eth3, the private host<->guest link. Empty on a bridge, where host
	// and guest already share one L2 and the link would be a second tap Windows cannot give us.
	PrivTap string
	// Addrs are the addresses this host puts on its own side, in CIDR form, keyed by device.
	// Under macvtap: the link's host end and this node's system-subnet /32, both on PrivTap.
	// Under a bridge: this node's system-subnet /32, on the bridge itself.
	Addrs []Addr
	// Routes are the host routes this host holds to its own guest. Under a bridge: one on-link /32
	// to the guest's node IP, the host's only way to its guest since its own /32 claims no subnet --
	// and deliberately reaching nothing else of the system subnet: a /24 there would be a second
	// connected route for the whole range on the host's primary device, beside its real LAN address.
	Routes []Route
}

// Addr is one address this host holds for its own guest's benefit.
type Addr struct{ CIDR, Dev string }

// Route is one host route: Dst (a bare address, routed as a /32) on-link on Dev, sourced from Src
// where the OS can pin a route's source (Linux). Windows cannot, and its callers bind Src
// themselves.
type Route struct{ Dst, Dev, Src string }

// built reports whether every device this spec names already exists. It is the difference between
// a pass that builds (first convergence, or the one after a reboot) and a pass that merely
// re-asserts -- and only the first may touch link state.
func (s Spec) built(ctx context.Context) bool {
	held := map[string][]string{}
	if s.Ipvtap {
		held = holdLinks(ctx)
	}
	for _, d := range []string{s.SystemTap, s.ServiceTap, s.PrivTap} {
		switch {
		case d == "":
		case s.Ipvtap && d != s.PrivTap:
			if held[d] == nil {
				return false
			}
		case !Exists(d):
			return false
		}
	}
	return true
}

// ⚠️ Convergence restores what drifts; it does not undo what somebody did. Everything here brings
// a device up only on the pass that creates it; one that exists and is administratively down is
// left alone, by rule, not oversight:
//
//   - The drift this loop exists for is NetworkManager flushing an address (a value, not link
//     state) we put on a device it manages, when a connection reactivates: a value reconciled away
//     underneath us. An `ip link set X down` is not drift; it is an act, by an operator or a test.
//   - A real partition is carrier-down, not admin-down, and an admin-up device with no carrier is
//     still IFF_UP, so this never fights a pulled cable and must not fight its stand-in either: a
//     partition test downs a node's SystemTap, and an agent that re-upped it would heal the
//     partition within a check, so DRBD would never see the peer drop
//     (nixosTest/install-macvtap.nix asserts the downed tap stays down).

// Converge makes the host's side of the guest's L2 match s, and returns the first thing it could
// not do. It is idempotent by construction and safe to call on every tick.
func Converge(ctx context.Context, s Spec) error {
	if s.Parent == "" || s.SystemTap == "" {
		return fmt.Errorf("nic: incomplete spec (%+v)", s)
	}
	if err := substrateBuildable(s); err != nil {
		return err
	}
	// The parent is brought up, and the tun driver loaded, only on a pass about to create something
	// (a first convergence, or one after a reboot took our devices): the parent, because a pass
	// finding everything present has no reason to touch the host's own NIC; the tun driver, because
	// nothing below can open /dev/net/tun until it exists, and nothing else on a stock host will
	// have asked for it.
	if !s.built(ctx) {
		loadTun(ctx)
		if err := ensureUp(ctx, s.Parent); err != nil {
			return err
		}
	}
	if s.Bridge {
		// One port on a bridge we did not make. No host NIC is enslaved or taken from
		// NetworkManager, no address or default route is moved, and the host's own LAN identity
		// is never touched. The user owns the bridge; we add a port.
		if err := ensureTap(ctx, s.SystemTap); err != nil {
			return err
		}
		if err := ensureMaster(ctx, s.SystemTap, s.Parent); err != nil {
			return err
		}
	} else if s.Ipvtap {
		// The guest's two NICs as ipvtap children in HoldNS, on the parent's MAC -- the one MAC
		// a wireless station can carry (ipvtap.go). The private tap is macvtap's, unchanged.
		if err := ensureHoldNS(ctx); err != nil {
			return err
		}
		held := holdLinks(ctx)
		for _, t := range []string{s.SystemTap, s.ServiceTap} {
			if t == "" {
				continue
			}
			if err := ensureIpvtap(ctx, t, s.Parent, held); err != nil {
				return err
			}
		}
		if s.PrivTap != "" {
			if err := ensureTap(ctx, s.PrivTap); err != nil {
				return err
			}
		}
	} else {
		// The guest's two NIC macvtaps on the parent, created with the kernel's random MAC; the
		// launch wrapper (briard-net-wrap) pins the agent-derived per-node MAC at guest start.
		// ⚠️ Order does not matter: briard-net-wrap takes <dev> <mac> <fd> triples from the agent's
		// argv, and the guest's ethN follows qemu's -netdev order, not the devices' creation order.
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
		}
	}
	for _, a := range s.Addrs {
		if err := ensureAddr(ctx, a); err != nil {
			return err
		}
	}
	// After the addresses: a route sourced from an address is refused until the address is there.
	for _, r := range s.Routes {
		if r.Dst == "" || hasRoute(r) {
			continue
		}
		if err := addRoute(ctx, r); err != nil {
			return fmt.Errorf("nic: route to %s on %s: %w", r.Dst, r.Dev, err)
		}
	}
	return nil
}

// Rebuild converges s after deleting the guest's NIC devices, so they are recreated on s.Parent
// whatever they hung off before: the re-parent's half of recovering from a moved LAN. The delete is
// the whole difference from Converge, and not optional: a macvtap cannot be re-parented, and
// Converge is check-first, so a child that still exists (a parent renamed rather than removed, say)
// would be left on the old device and reported converged. Deleting first makes "the guest's L2
// hangs off this parent now" true rather than hoped.
//
// ⚠️ The private tap is not touched. It has no parent -- it is a point-to-point wire to our own
// guest -- so a re-parent has nothing to do to it, and recreating it would drop the host's own
// addresses and the permanent neighbour entry that make the reboot gate and the VIP route work.
//
// The children are deleted from both namespaces, whatever s says: a move between Wi-Fi and wire
// changes the substrate, and the old children are wherever the old substrate put them.
func Rebuild(ctx context.Context, s Spec) error {
	if s.Bridge {
		// Nothing to rebuild: the port is a plain tap on a bridge the user owns, and a bridge
		// that went away is theirs to restore.
		return Converge(ctx, s)
	}
	held := holdLinks(ctx)
	for _, t := range []string{s.SystemTap, s.ServiceTap} {
		if t == "" {
			continue
		}
		if Exists(t) {
			if out, err := ip(ctx, "link", "del", t); err != nil {
				return fmt.Errorf("nic: removing %s before re-parenting: %s", t, firstLine(out, err))
			}
		}
		if err := delHeld(ctx, t, held); err != nil {
			return err
		}
	}
	return Converge(ctx, s)
}

// Remove undoes Converge (`briard uninstall`): every device s names is deleted, and every address
// s puts on a device it did not create -- the user's bridge -- is taken back off it. Deleting a
// device removes everything else we hung on it (its addresses, the VIP route and the permanent
// neighbour entry over the private tap, the per-device IPv6 knob); the bridge is the one device we
// touch and must leave standing, so its address is the one thing removed by hand. Absent devices
// and addresses are already removed; it carries on past a failure and returns the first, so one
// stuck device does not leave the rest behind.
func Remove(ctx context.Context, s Spec) error {
	var first error
	keep := func(err error) {
		if first == nil {
			first = err
		}
	}
	for _, t := range []string{s.SystemTap, s.ServiceTap, s.PrivTap} {
		if t != "" && Exists(t) {
			if out, err := ip(ctx, "link", "del", t); err != nil {
				keep(fmt.Errorf("nic: removing %s: %s", t, firstLine(out, err)))
			}
		}
	}
	// The ipvtap substrate's namespace, and the children in it with it.
	if exists("/run/netns/" + HoldNS) {
		if out, err := ip(ctx, "netns", "del", HoldNS); err != nil {
			keep(fmt.Errorf("nic: removing %s: %s", HoldNS, firstLine(out, err)))
		}
	}
	for _, r := range s.Routes {
		if r.Dst == "" || !hasRoute(r) {
			continue
		}
		if err := delRoute(ctx, r); err != nil {
			keep(fmt.Errorf("nic: removing the route to %s on %s: %w", r.Dst, r.Dev, err))
		}
	}
	for _, a := range s.Addrs {
		if a.CIDR == "" || !hasAddr(a.Dev, a.CIDR) {
			continue
		}
		if err := delAddr(ctx, a); err != nil {
			keep(fmt.Errorf("nic: removing %s from %s: %w", a.CIDR, a.Dev, err))
		}
	}
	return first
}

// Converged reports whether Converge would change nothing. It exists so the tick can stay silent
// on the overwhelmingly common path -- and so the agent can say that the network is not what it
// should be without having tried to fix it yet.
func Converged(s Spec) bool {
	if s.Parent == "" || !Exists(s.Parent) {
		return false
	}
	var held map[string][]string
	if s.Ipvtap {
		// Bounded: a check that is every network check's first step may not wait on `ip`
		// indefinitely.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		held = holdLinks(ctx)
		cancel()
	}
	for _, t := range []string{s.SystemTap, s.ServiceTap, s.PrivTap} {
		if t == "" {
			continue
		}
		// The ipvtap children are asked of HoldNS, all in the one `ip` call above. IPv6 is off
		// namespace-wide from the pass that made it, so ALLMULTI is the flag left to check.
		if s.Ipvtap && t != s.PrivTap {
			if !slices.Contains(held[t], "ALLMULTI") {
				return false
			}
			continue
		}
		// Existence, not link state: an administratively down device is somebody's decision, not
		// drift, and Converge would not undo it anyway (the rule above it), so reporting it as
		// unconverged would only make the tick say so forever.
		if !Exists(t) {
			return false
		}
		// The two flags on a macvtap child that are not kernel defaults, both checked. The IPv6
		// one is the whole of its repair path: an existing device is never re-created, so unless
		// the tick asks this, a child autoconfiguring on the guest's MAC keeps doing so forever
		// -- and every reachability check still passes.
		if !s.Bridge && t != s.PrivTap && (flags(t)&ifAllmulti == 0 || !ipv6Disabled(t)) {
			return false
		}
		// And its parent, the macvtap's twin of the bridge port's master below: a child standing
		// on another device is not this spec, whatever its flags say. A device stacked on nothing
		// (a rig's own tap) has no parent to disagree.
		if !s.Bridge && t != s.PrivTap {
			if l := lowerOf(t); l != "" && l != s.Parent {
				return false
			}
		}
	}
	if s.Bridge && s.SystemTap != "" && master(s.SystemTap) != s.Parent {
		return false
	}
	for _, a := range s.Addrs {
		// An empty CIDR is "this node takes no address here", a no-op to ensureAddr, so Converged
		// must agree, or the tick would report a mismatch it could not fix, on every pass forever.
		// (Every agent-less harness has one: the rig addresses the private link, not a config this
		// agent was given.)
		if a.CIDR != "" && !hasAddr(a.Dev, a.CIDR) {
			return false
		}
	}
	for _, r := range s.Routes {
		if r.Dst != "" && !hasRoute(r) {
			return false
		}
	}
	return true
}

// ensureMacvtap creates one macvtap child and gives it the two flags that are not defaults.
func ensureMacvtap(ctx context.Context, dev, parent string) error {
	created := !Exists(dev)
	if created {
		if out, err := ip(ctx, macvtapAddArgs(dev, parent)...); err != nil {
			return fmt.Errorf("nic: macvtap %s on %s: %s", dev, parent, firstLine(out, err))
		}
	}
	// A child of another parent is refused, not moved: a macvtap cannot be re-parented, deleting it
	// here would take the NIC from under a guest that may hold it, and the move is Rebuild's, behind
	// the re-parent's tiers. Converging past it would report a parent the guest is not on.
	if l := lowerOf(dev); !created && l != "" && l != parent {
		return fmt.Errorf("nic: %s is a macvtap child of %s, not of %s", dev, l, parent)
	}
	// IPv6 off, outside the create branch on purpose. The host end of a macvtap holds no address,
	// but the device carries the guest's MAC, so a host autoconfiguring on it derives the same
	// EUI-64 identifier as the guest, on the same L2: a duplicate address whose winner DAD picks and
	// whose loser silently drops it, and the host's avahi joins mDNS on the guest's segment, where
	// the name is the guest's to publish. Ubuntu ships net.ipv6.conf.default.accept_ra=1, and every
	// new device inherits the "default" values. On a fresh device this runs before it is up, so no
	// advertisement can be accepted at all; on an existing one the write flushes what it already
	// picked up, which is how one that came up without it is repaired. A procfs write rather than
	// sysctl(8), because this runs on stock hosts and on NixOS alike.
	if p := disableIPv6Path(dev); exists(p) {
		if err := os.WriteFile(p, []byte("1\n"), 0o644); err != nil {
			return fmt.Errorf("nic: disable ipv6 on %s: %w", dev, err)
		}
	}
	// Up only on the pass that made it (the rule above Converge). On a fresh device the write above
	// lands first, deliberately: nothing can accept a router advertisement on an interface not up.
	if created {
		if err := ensureUp(ctx, dev); err != nil {
			return err
		}
	}
	// ALLMULTI, or inbound multicast never reaches the guest. The guest's mDNS responder (in the
	// reverse proxy) joins 224.0.0.251 on its virtio NIC inside the VM, and nothing carries that
	// join out to this device: qemu only emits a NIC_RX_FILTER_CHANGED event, the code acting on it
	// is libvirt's (gated on trustGuestRxFilters), and we run qemu directly. So this macvtap's
	// multicast list holds only the all-hosts group, macvlan_broadcast() drops every inbound mDNS
	// query on its per-child test_bit(hash, vlan->mc_filter), and ALLMULTI is what fills that bitmap.
	// The trap is that egress does not need it: without it the guest's answers still leave and
	// unicast queries still reach it, so a client holding the record keeps resolving the household
	// name until that cached record expires, after which nothing can query the guest and the name
	// is dead. On the child, not the parent: the gate is the per-child filter, so adding the group
	// to the parent's own multicast list does not help. Multicast only, deliberately not
	// promiscuous, which would pull every unicast frame on the segment off the wire for nothing.
	if flags(dev)&ifAllmulti == 0 {
		if out, err := ip(ctx, allmulticastOnArgs(dev)...); err != nil {
			return fmt.Errorf("nic: allmulticast on %s: %s", dev, firstLine(out, err))
		}
	}
	return nil
}

// ensureTap creates a plain tap if it is not there. The private host<->guest link and the bridge
// substrate's one port are both this shape.
func ensureTap(ctx context.Context, dev string) error {
	if Exists(dev) {
		return nil // exists: not ours to re-up (see the rule above Converge)
	}
	if err := createTap(ctx, dev); err != nil {
		return fmt.Errorf("nic: tap %s: %w", dev, err)
	}
	return ensureUp(ctx, dev)
}

// ensureMaster enslaves dev to the bridge, if it is not already a port of it. A device that is a
// port of some other bridge is re-enslaved: the recorded parent is the answer, and a leftover
// membership from a previous parent is exactly what a re-parent has to fix.
func ensureMaster(ctx context.Context, dev, bridge string) error {
	if master(dev) == bridge {
		return nil
	}
	if err := joinBridge(ctx, dev, bridge); err != nil {
		return fmt.Errorf("nic: %s -> %s: %w", dev, bridge, err)
	}
	return nil
}

func ensureUp(ctx context.Context, dev string) error {
	if up(dev) {
		return nil
	}
	if err := linkUp(ctx, dev); err != nil {
		return fmt.Errorf("nic: %s up: %w", dev, err)
	}
	return nil
}

// ensureAddr puts one address on one device. addAddr replaces rather than adds on Linux, so a
// prefix that changed converges rather than collides.
func ensureAddr(ctx context.Context, a Addr) error {
	if a.CIDR == "" || a.Dev == "" || hasAddr(a.Dev, a.CIDR) {
		return nil
	}
	if err := addAddr(ctx, a); err != nil {
		return fmt.Errorf("nic: %s on %s: %w", a.CIDR, a.Dev, err)
	}
	return nil
}

// The macvtap child's two flag readers -- flags (the raw IFF_ bitmap, where ALLMULTI is visible at
// all) and ipv6Disabled (the per-device procfs knob) -- and disableIPv6Path are the Linux arm's,
// in link_linux.go: the substrate they describe exists on no other OS.

// parseIfFlags decodes that bitmap. Pure, so the `0x` hex the kernel renders it in is unit-tested
// rather than trusted. An unreadable or unparseable value is 0 -- no flags set, which
// sends Converged to false and makes the tick try to fix the device rather than assume it is fine.
func parseIfFlags(text string) int64 {
	n, err := strconv.ParseInt(strings.TrimPrefix(strings.TrimSpace(text), "0x"), 16, 64)
	if err != nil {
		return 0
	}
	return n
}

// parseMaster takes the bridge name out of the `master` symlink's target. Pure, so the one detail
// that cannot be inferred by reading it is unit-tested rather than trusted: ⚠️ filepath.Base, not
// a prefix trim. The target's depth depends on where the port's real sysfs node lives -- one level
// from its bridge for a virtual device (`../br0`), several for a physical NIC
// (`../../../../virtual/net/br0`) -- and stripping a single `../` matches no bridge name on the
// second shape, so Converged would answer false forever and the tick re-converge on every pass.
func parseMaster(link string) string { return filepath.Base(link) }

// hasAddr reports whether dev already holds exactly this address and prefix. The prefix is part
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
