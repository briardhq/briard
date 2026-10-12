package subnet

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
)

// Observe reads what this host can already see of the address space: the addresses on its own
// NICs, and the routes it holds. Thin and best-effort throughout -- a read that fails contributes
// nothing, which draws freely rather than refusing (see Observed).
//
// This is the cheap half of the open unknown-LAN validation, and it costs nothing new:
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

// Report renders a drawn set as the node's record: three shell-shaped lines, nothing else. It is
// read back by Parse and greppable by a human and a rig, so keep it boring.
func Report(w io.Writer, d Draw) error {
	_, err := fmt.Fprintf(w, "SYSTEM_SUBNET=%s\nPRIV_SUBNET=%s\nPOD_SUBNET=%s\n", d.System, d.Priv, d.Pod)
	return err
}

// recorded matches one line of what Report wrote. The pattern IS the format's assertion: anything
// that is not a bare 10.a.b reads as ABSENT, so a truncated or hand-mangled record leaves that
// field empty and the caller draws it again rather than numbering a node out of a typo.
var recorded = regexp.MustCompile(`(?m)^(SYSTEM_SUBNET|PRIV_SUBNET|POD_SUBNET)=(10\.\d{1,3}\.\d{1,3})$`)

// Parse reads a record back. A field the record does not carry comes back empty -- which is how a
// node that predates a pool gains only that pool, rather than being renumbered wholesale.
//
// Never a shell `source`, on a file that holds three numbers: there is no reason to give it the
// power to run anything.
func Parse(b []byte) Draw {
	var d Draw
	for _, m := range recorded.FindAllSubmatch(b, -1) {
		switch string(m[1]) {
		case "SYSTEM_SUBNET":
			d.System = string(m[2])
		case "PRIV_SUBNET":
			d.Priv = string(m[2])
		case "POD_SUBNET":
			d.Pod = string(m[2])
		}
	}
	return d
}
