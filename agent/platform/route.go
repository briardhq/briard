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
