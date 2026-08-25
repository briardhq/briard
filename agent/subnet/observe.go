package subnet

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"briard.io/agent/reportcard"
)

// Observe reads what this host can already see of the address space: the addresses on its own
// NICs, and the routes it holds. Thin and best-effort throughout -- a read that fails contributes
// nothing, which draws freely rather than refusing (see Observed).
//
// This is the cheap half of DESIGN §4's open unknown-LAN validation, and it costs nothing new:
// the report card already reads the host's interfaces to judge the VIP, so the machine was
// already being looked at.
func Observe(ctx context.Context) Observed {
	obs := Observed{Where: map[netip.Prefix]string{}}
	obs.add(hostAddrs())
	obs.add(hostRoutes(ctx))
	return obs
}

func (o *Observed) add(seen []seenPrefix) {
	for _, s := range seen {
		if _, dup := o.Where[s.prefix]; dup {
			continue
		}
		o.Prefixes = append(o.Prefixes, s.prefix)
		o.Where[s.prefix] = s.where
	}
}

type seenPrefix struct {
	prefix netip.Prefix
	where  string
}

// hostAddrs reads the networks this machine's own NICs sit on. The NETWORK, not the address: a
// host at 10.9.9.5/24 occupies the whole /24 as far as a draw is concerned, because that is what
// its on-link route claims.
//
// Down interfaces count too. A NIC that is down today is a NIC that comes up tomorrow -- and the
// one a laptop has unplugged while a stranger installs over wifi is exactly the one whose subnet
// we must not squat.
func hostAddrs() []seenPrefix {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []seenPrefix
	for _, iface := range ifaces {
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			n, ok := a.(*net.IPNet)
			if !ok || n.IP.To4() == nil {
				continue
			}
			p, err := netip.ParsePrefix(n.String())
			if err != nil {
				continue
			}
			if p = p.Masked(); worthHonouring(p) {
				out = append(out, seenPrefix{p, "the address on " + iface.Name})
			}
		}
	}
	return out
}

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

// routeTypes are the leading keywords `ip route` prints before a destination. Skipping them is
// what makes `local 10.11.9.1 dev briard-priv0 table local` parse as 10.11.9.1/32 rather than as
// nothing.
var routeTypes = map[string]bool{
	"unicast": true, "local": true, "broadcast": true, "multicast": true,
	"throw": true, "unreachable": true, "prohibit": true, "blackhole": true, "nat": true, "anycast": true,
}

// parseIPRoutes pulls destination prefixes out of `ip -4 route show table all`. Pure, so the
// format handling is unit-tested against real output rather than trusted.
func parseIPRoutes(out string) []seenPrefix {
	var seen []seenPrefix
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) > 0 && routeTypes[f[0]] {
			f = f[1:]
		}
		if len(f) == 0 || f[0] == "default" {
			continue // a default route claims everything and therefore nothing (see worthHonouring)
		}
		p, err := parseDest(f[0])
		if err != nil || !worthHonouring(p) {
			continue
		}
		seen = append(seen, seenPrefix{p, "routed via " + devOf(f)})
	}
	return seen
}

// parseProcRoutes reads the same thing out of /proc/net/route, which is main-table-only and
// little-endian hex. Used only where `ip` cannot run.
func parseProcRoutes(out string) []seenPrefix {
	var seen []seenPrefix
	sc := bufio.NewScanner(strings.NewReader(out))
	sc.Scan() // header
	for sc.Scan() {
		f := strings.Fields(sc.Text()) // Iface Destination Gateway Flags RefCnt Use Metric Mask
		if len(f) < 8 {
			continue
		}
		dst, derr := leHex(f[1])
		mask, merr := leHex(f[7])
		if derr != nil || merr != nil {
			continue
		}
		ones, bits := net.IPv4Mask(mask[0], mask[1], mask[2], mask[3]).Size()
		if bits == 0 {
			continue // not a contiguous mask
		}
		p := netip.PrefixFrom(netip.AddrFrom4(dst), ones).Masked()
		if !worthHonouring(p) {
			continue
		}
		seen = append(seen, seenPrefix{p, "routed via " + f[0]})
	}
	return seen
}

// leHex decodes /proc/net/route's little-endian hex quad into address order.
func leHex(s string) ([4]byte, error) {
	v, err := strconv.ParseUint(s, 16, 32)
	if err != nil {
		return [4]byte{}, err
	}
	return [4]byte{byte(v), byte(v >> 8), byte(v >> 16), byte(v >> 24)}, nil
}

// parseDest turns `ip route`'s destination field into a prefix. A bare address is a host route.
func parseDest(s string) (netip.Prefix, error) {
	if strings.Contains(s, "/") {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			return netip.Prefix{}, err
		}
		return p.Masked(), nil
	}
	a, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Prefix{}, err
	}
	return netip.PrefixFrom(a, a.BitLen()), nil
}

// devOf pulls the `dev X` out of an `ip route` line, for the message only.
func devOf(f []string) string {
	for i, w := range f {
		if w == "dev" && i+1 < len(f) {
			return f[i+1]
		}
	}
	return "this machine"
}

// worthHonouring is THE PREFIX CUT: ignore /8 through /11, honour /12 and longer.
//
// The pathological case is specifically the sloppy whole-RFC1918 catch-all -- a corporate VPN
// pushing 10.0.0.0/8 must not veto the entire pool, or an employee's laptop can never install.
// The obvious cut of "/16 and smaller" would also discard Kubernetes' 10.96.0.0/12 service CIDR,
// which is a genuine collision. Anything more specific than a /8 is somebody NAMING a range;
// treat it as real.
//
// Non-10/8 prefixes are dropped here too: they cannot collide with either pool, and carrying them
// only makes a refusal's message longer.
func worthHonouring(p netip.Prefix) bool {
	return p.Addr().Is4() && p.Bits() >= 12 && p.Addr().As4()[0] == 10
}

// LANProbe returns the flock draw's second question: is another flock already living on this
// candidate subnet? It asks the household's own L2 by ARP -- the same mechanism DESIGN §4 already
// names for validating BRIARD_VIP, so this adds a caller rather than a mechanism.
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

// Report renders a drawn pair for install.sh: two shell-shaped lines on stdout, nothing else.
// The installer parses them with sed rather than sourcing the file, so this format is a contract
// with one regexp -- keep it boring.
func Report(w io.Writer, d Draw) error {
	_, err := fmt.Fprintf(w, "SYSTEM_SUBNET=%s\nPRIV_SUBNET=%s\n", d.System, d.Priv)
	return err
}
