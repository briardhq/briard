package platform

import (
	"context"
	"fmt"
	"strings"
)

// The host's route to its own guest's service address. Under the macvtap substrate a parent NIC is
// isolated from its own children, and a switch does not reflect a frame to the port it came from,
// so the machine running the guest is the one machine on the LAN that cannot open the address the
// guest serves. Every other house on the network reaches it, a second briard host too (that
// traffic goes out the wire and back). Measured at the same instant: from the host
// `http://<vip>/ -> 000`, from the router `-> OK`.
//
// The private host<->guest link is not isolated: it is a plain tap on both ends (the agent creates
// it, agent/nic, and qemu.go deliberately keeps it NetBridge even under macvtap, which would
// isolate the very path the witness forwarder and the deadman gate ride). So one /32 through that
// link restores the one address the household is given, on the one machine that was missing it.
//
// This is deliberately not a way to publish a private address: the user is never given the guest's
// address on the private link. That address is node-scoped transport, while the VIP and the name
// are flock-scoped and survive a failover it does not; a household that bookmarked it would have
// bookmarked a machine instead of an app.

// VIPRoute is the host-side route to the guest's service address over the private link.
type VIPRoute struct {
	Addr string // the guest's service address, bare (no prefix) -- what the household is given
	Via  string // the guest's node IP -- resolvable on the tap through SetNodeRoute's pinned neighbour
	Dev  string // the host's end of the private link (the tap)
	Src  string // the host's own system-subnet address on that tap -- the route's preferred source
}

// routeReplaceArgs renders the `ip` argv that installs the route; pure, so the one detail that
// cannot be inferred from reading it is unit-tested rather than trusted.
//
// ⚠️ `via <guest>`, not `dev <tap>`, the form that looks simpler and does not work. The guest sets
// arp_ignore=1 (guest-image/configuration.nix, the ARP-flux fix), so it answers ARP only on the
// interface that holds the address asked for. An on-link route makes the host ARP for the VIP on
// the tap; the VIP lives on the guest's eth2 and the request arrives on its eth3, so the guest
// stays silent and the route black-holes. Going `via` the guest's node IP makes the next hop the
// address SetNodeRoute pinned a permanent neighbour entry for, so the host never ARPs at all.
// Neither side forwards: the VIP is a local address of the guest, accepted on any interface under
// Linux's weak host model, so `via` names a destination rather than a router.
//
// `src`, the host's own system-subnet address on the tap, keeps the return path legal: the guest
// replies from the VIP, and the host's reverse-path check on the tap passes only because this /32
// is the best route to the VIP. The route is not merely how the request leaves; it is why the
// answer is allowed back in.
func routeReplaceArgs(r VIPRoute) []string {
	return []string{"route", "replace", r.Addr + "/32", "via", r.Via, "dev", r.Dev, "src", r.Src}
}

// routeDelArgs renders the `ip` argv that withdraws it. Keyed on address + device so it can
// never remove a route the household's own network put there.
func routeDelArgs(addr, dev string) []string {
	return []string{"route", "del", addr + "/32", "dev", dev}
}

// SetVIPRoute installs (or re-points) the host's route to the guest's service address.
// Idempotent by construction -- `ip route replace` writes the route whether or not one is there.
func SetVIPRoute(ctx context.Context, r VIPRoute) error {
	if r.Addr == "" || r.Via == "" || r.Dev == "" || r.Src == "" {
		return fmt.Errorf("platform: VIP route spec incomplete (%+v)", r)
	}
	if out, err := runIP(ctx, routeReplaceArgs(r)...); err != nil {
		return fmt.Errorf("platform: ip route replace %s: %w: %s", r.Addr, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// NodeRoute is the host's standing path to its own guest's node IP over the private link, the one
// address anything uses to reach that node. Unlike VIPRoute it does not come and go: the guest
// cannot lose the node IP to a failover, so this is installed once at bring-up and re-asserted.
type NodeRoute struct {
	GuestIP string // the guest's node IP, bare -- its address on the system subnet, held on eth1
	Dev     string // the host's end of the private link (the tap)
	Src     string // the host's own system-subnet address, on that tap
	LLAddr  string // the guest's private-link MAC -- what the permanent neighbour entry pins
}

// nodeRouteArgs and nodeNeighArgs render the two `ip` argvs, pure so the one detail that cannot
// be inferred by reading them is unit-tested rather than trusted.
//
// ⚠️ The neighbour entry is not an optimisation: without it this route black-holes. The guest's
// node IP lives on eth1, the host's ARP request for it arrives on eth3, and arp_ignore=1 (the
// ARP-flux fix) makes the guest answer ARP only on the interface that holds the address asked
// for, so nothing resolves. Pinning the MAC means the host never asks: the entry is `permanent`,
// so the kernel never expires or revalidates it. That is safe precisely because the MAC is
// derived, not discovered: deriveMAC(node, "wit") is what the agent already hands qemu for that
// NIC, so there is no second source to disagree.
//
// It is also why routing needs no addressing on the link itself: the pinned node IP is the next
// hop for both this route and VIPRoute, so the link's own addresses are never one.
func nodeRouteArgs(r NodeRoute) []string {
	return []string{"route", "replace", r.GuestIP + "/32", "dev", r.Dev, "src", r.Src}
}

func nodeNeighArgs(r NodeRoute) []string {
	return []string{"neigh", "replace", r.GuestIP, "lladdr", r.LLAddr, "dev", r.Dev, "nud", "permanent"}
}

// SetNodeRoute installs both halves. Neighbour first, then the route: a route whose next hop
// cannot be resolved drops packets for as long as the gap lasts, and the gap is avoidable.
func SetNodeRoute(ctx context.Context, r NodeRoute) error {
	if r.GuestIP == "" || r.Dev == "" || r.Src == "" || r.LLAddr == "" {
		return fmt.Errorf("platform: node route spec incomplete (%+v)", r)
	}
	for _, args := range [][]string{nodeNeighArgs(r), nodeRouteArgs(r)} {
		if out, err := runIP(ctx, args...); err != nil {
			return fmt.Errorf("platform: ip %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
		}
	}
	return nil
}

// LinkMAC reads a host interface's link address: the private link's host end, whose MAC the guest
// pins as a permanent neighbour (the net.configure handler says why it must).
//
// Read, not derived, and re-read at every bring-up: `ip tuntap add` hands out a random MAC, so this
// end's address changes whenever the tap is recreated (after a host reboot the agent makes it
// again). The guest's end is the opposite (deriveMAC makes it a function of the node name), which
// is why only this direction needs carrying over the channel.
func LinkMAC(ctx context.Context, dev string) (string, error) {
	out, err := runIP(ctx, "-o", "link", "show", "dev", dev)
	if err != nil {
		return "", fmt.Errorf("platform: ip link show %s: %w: %s", dev, err, strings.TrimSpace(string(out)))
	}
	mac := linkMACFrom(string(out))
	if mac == "" {
		return "", fmt.Errorf("platform: no link address on %s (%q)", dev, strings.TrimSpace(string(out)))
	}
	return mac, nil
}

// linkMACFrom picks the ether address out of `ip -o link show` output. Split out so the parse is
// unit-tested against real iproute2 text rather than trusted.
func linkMACFrom(out string) string {
	f := strings.Fields(out)
	for i, tok := range f {
		if tok == "link/ether" && i+1 < len(f) {
			return f[i+1]
		}
	}
	return ""
}

// ClearVIPRoute withdraws it. A route already gone is a success, not an error: the caller's job is
// to leave the host with no route, and both spellings of "there is none" mean the same to it.
// Anything else is reported: a delete failing for a reason we did not anticipate leaves the host
// pointing at a guest that may no longer serve, the one failure mode this whole path can cause.
func ClearVIPRoute(ctx context.Context, addr, dev string) error {
	out, err := runIP(ctx, routeDelArgs(addr, dev)...)
	if err == nil || routeAbsent(string(out)) {
		return nil
	}
	return fmt.Errorf("platform: ip route del %s: %w: %s", addr, err, strings.TrimSpace(string(out)))
}

// routeAbsent reports whether an `ip route del` failure means "there was nothing to delete".
// iproute2 spells that as the errno text for ESRCH/ENOENT on the netlink reply.
func routeAbsent(out string) bool {
	s := strings.ToLower(out)
	return strings.Contains(s, "no such process") || strings.Contains(s, "no such file or directory")
}
