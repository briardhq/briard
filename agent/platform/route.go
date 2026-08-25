package platform

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// THE HOST'S ROUTE TO ITS OWN GUEST'S SERVICE ADDRESS.
//
// Under the macvtap substrate a parent NIC is isolated from its own children, and a switch does
// not reflect a frame to the port it came from. The consequence is narrow and unintuitive: the
// machine RUNNING the guest is the one machine on the LAN that cannot open the address the guest
// serves. Every other house on the network reaches it; a second briard host reaches it (that
// traffic goes out the wire and back); only the host itself cannot. Measured on a stranger's
// desktop, the same instant: from the host `http://<vip>/ -> 000`, from the router `-> OK`
// ([V3b.19]).
//
// The private host<->guest link is NOT isolated -- it is a plain tap on both ends (install.sh
// creates it with `ip tuntap`, and qemu.go keeps it NetBridge even under macvtap, deliberately,
// because macvtap would isolate the very path the witness forwarder and the deadman gate ride).
// So one /32 through that link restores the one address the household is given, on the one
// machine that was missing it.
//
// WHAT THIS DELIBERATELY IS NOT: a way to publish the private address. The user is never given
// 10.9.9.2. That address is node-scoped -- it is TRANSPORT -- while the VIP and the name are
// flock-scoped and survive a failover it does not. A household that bookmarked the private
// address would have bookmarked a machine instead of a service, which is the same incoherence
// [V3.20] took out of the mDNS name.

// VIPRoute is the host-side route to the guest's service address over the private link.
type VIPRoute struct {
	Addr string // the guest's service address, bare (no prefix) -- what the household is given
	Via  string // the guest's end of the private link (its eth3 address)
	Dev  string // the host's end of that link (the tap)
	Src  string // the host's own address on the link -- the route's preferred source
}

// routeReplaceArgs renders the `ip` argv that installs the route. Pure, so the one detail that
// cannot be inferred from reading it is unit-tested rather than trusted.
//
// ⚠️ `via <guest>` AND NOT `dev <tap>`, which is the form that looks simpler and does not work.
// The guest sets arp_ignore=1 (guest-image/configuration.nix, the [B.101] ARP-flux fix): it
// answers ARP only on the interface that HOLDS the address being asked for. An on-link route
// makes the host ARP for the VIP on the tap; the VIP lives on the guest's eth2 and the request
// arrives on its eth3, so the guest stays silent and the route black-holes. Going `via` makes
// the host ARP for the guest's eth3 address instead, which is the one address that interface
// does hold. No forwarding is involved on either side -- the VIP is a LOCAL address of the
// guest, accepted on any interface under Linux's weak host model, so `via` here names a
// destination rather than a router.
//
// `src` is the host's own end of the link, and it is what keeps the RETURN path legal: the
// guest replies from the VIP, and the host's reverse-path check on the tap passes only because
// this /32 is the best route to the VIP. The route is not merely how the request leaves; it is
// why the answer is allowed back in.
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
	if out, err := exec.CommandContext(ctx, "ip", routeReplaceArgs(r)...).CombinedOutput(); err != nil {
		return fmt.Errorf("platform: ip route replace %s: %w: %s", r.Addr, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// NodeRoute is the host's standing path to its own guest's NODE IP over the private link -- the
// one address anything uses to reach that node (DESIGN §4). Unlike VIPRoute it does not come and
// go: the node IP is not a thing the guest can lose to a failover, so this is installed once at
// bring-up and simply re-asserted.
type NodeRoute struct {
	GuestIP string // the guest's node IP, bare -- its address on the system subnet, held on eth1
	Dev     string // the host's end of the private link (the tap)
	Src     string // the host's own system-subnet address, on that tap
	LLAddr  string // the guest's private-link MAC -- what the permanent neighbour entry pins
}

// nodeRouteArgs and nodeNeighArgs render the two `ip` argvs, pure so the one detail that cannot
// be inferred by reading them is unit-tested rather than trusted.
//
// ⚠️ THE NEIGHBOUR ENTRY IS NOT AN OPTIMISATION -- without it this route black-holes. The guest's
// node IP lives on eth1 while the host's ARP request for it arrives on eth3, and arp_ignore=1
// (the [B.101] fix) makes the guest answer ARP only on the interface that HOLDS the address
// asked for. So the guest stays silent and nothing resolves. Pinning the MAC means the host never
// asks: the entry is `permanent`, so the kernel never expires it and never revalidates it. That
// is safe precisely because the MAC is not discovered but DERIVED -- deriveMAC(node, "wit") is
// what the agent already hands qemu for that NIC, so there is no second source to disagree.
//
// This is also why the link needs no addressing of its own. The older shape gave both ends
// addresses on a private 10.9.9.0/24 purely so the host could `via` a resolvable address; pinning
// the neighbour removes the reason for the subnet, and with it an invented range that could
// collide with a household's LAN just as 10.0.0.0/24 can ([V3b.26f]).
func nodeRouteArgs(r NodeRoute) []string {
	return []string{"route", "replace", r.GuestIP + "/32", "dev", r.Dev, "src", r.Src}
}

func nodeNeighArgs(r NodeRoute) []string {
	return []string{"neigh", "replace", r.GuestIP, "lladdr", r.LLAddr, "dev", r.Dev, "nud", "permanent"}
}

// SetNodeRoute installs both halves. Neighbour FIRST, then the route: a route whose next hop
// cannot be resolved drops packets for as long as the gap lasts, and the gap is avoidable.
func SetNodeRoute(ctx context.Context, r NodeRoute) error {
	if r.GuestIP == "" || r.Dev == "" || r.Src == "" || r.LLAddr == "" {
		return fmt.Errorf("platform: node route spec incomplete (%+v)", r)
	}
	for _, args := range [][]string{nodeNeighArgs(r), nodeRouteArgs(r)} {
		if out, err := exec.CommandContext(ctx, "ip", args...).CombinedOutput(); err != nil {
			return fmt.Errorf("platform: ip %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
		}
	}
	return nil
}

// ClearVIPRoute withdraws it. A route that is already gone is a success, not an error: the
// caller's job is to leave the host with no route, and both spellings of "there is none" mean
// the same thing to it. Anything else is reported -- a delete that fails for a reason we did not
// anticipate leaves the host pointing at a guest that may no longer serve, which is the one
// failure mode this whole path can cause.
func ClearVIPRoute(ctx context.Context, addr, dev string) error {
	out, err := exec.CommandContext(ctx, "ip", routeDelArgs(addr, dev)...).CombinedOutput()
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
