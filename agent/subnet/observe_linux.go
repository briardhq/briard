package subnet

import (
	"context"
	"net/netip"
	"os"
	"os/exec"

	"briard.io/agent/reportcard"
)

// hostRoutes reads the ranges this host routes, from every table rather than just main: a VPN
// client can install its ranges into a table of its own behind an ip rule, and those collide
// exactly as hard as main's do.
//
// `ip` first because it is the only reader that sees all tables, /proc/net/route as the fallback
// because it needs no binary at all. iproute2 is a hard requirement of the install (the report
// card refuses without it), so the fallback is belt-and-braces rather than a supported mode.
func hostRoutes(ctx context.Context) []seenPrefix {
	if out, err := exec.CommandContext(ctx, "ip", "-4", "route", "show", "table", "all").Output(); err == nil {
		return parseIPRoutes(string(out))
	}
	b, err := os.ReadFile("/proc/net/route")
	if err != nil {
		return nil
	}
	return parseProcRoutes(string(b))
}

// LANProbe returns the flock draw's second question: is another flock already living on this
// candidate subnet? It asks the household's own L2 by ARP -- the same mechanism already
// used for validating BRIARD_VIP_ADDR, so this adds a caller rather than a mechanism.
//
// ⚠️ THE TEMPORARY ROUTE IS WHY THIS WORKS AT ALL. A candidate subnet is by construction one this
// host has no address in, so a datagram to it leaves by the default route -- to the router's MAC,
// resolving nothing about the candidate. One /32 `dev` route makes the address on-link for the
// length of the probe, so the kernel ARPs for it on the LAN, which is the question we came to ask.
// `route add` rather than `replace`: it must never clobber a route the household's own network put
// there, and if one is already there Observe has seen it and the candidate is gone anyway.
//
// A false is "nothing answered", INCLUDING "we could not ask" -- a probe that cannot run must not
// be able to exhaust the draw and refuse the install.
func LANProbe(ctx context.Context, nic string) func(netip.Addr) bool {
	if nic == "" {
		return func(netip.Addr) bool { return false }
	}
	return func(ip netip.Addr) bool {
		dst := ip.String() + "/32"
		if err := exec.CommandContext(ctx, "ip", "route", "add", dst, "dev", nic).Run(); err != nil {
			return false
		}
		defer func() { _ = exec.CommandContext(ctx, "ip", "route", "del", dst, "dev", nic).Run() }()
		return reportcard.AddressAnswers(dst)
	}
}
