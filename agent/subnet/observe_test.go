package subnet

import (
	"net/netip"
	"testing"
)

// Real `ip -4 route show table all` output from a host with a VPN, a container runtime and a
// prior briard install -- the four shapes the parser has to survive: a default route, type
// keywords (table local prints them), a bare host address, and non-10 space.
const ipRouteAll = `default via 192.168.9.1 dev enp3s0 proto dhcp src 192.168.9.100 metric 100
10.0.0.0/8 dev wg0 scope link
10.96.0.0/12 dev cni0 proto kernel scope link src 10.96.0.1
10.11.9.0/24 dev briard-priv0 proto kernel scope link src 10.11.9.1
172.17.0.0/16 dev docker0 proto kernel scope link src 172.17.0.1
192.168.9.0/24 dev enp3s0 proto kernel scope link src 192.168.9.100 metric 100
broadcast 127.0.0.0 dev lo table local proto kernel scope link src 127.0.0.1
local 10.11.9.1 dev briard-priv0 table local proto kernel scope host src 10.11.9.1
multicast 224.0.0.0/4 dev enp3s0 table local proto static scope link
`

func TestParseIPRoutes(t *testing.T) {
	got := map[netip.Prefix]string{}
	for _, s := range parseIPRoutes(ipRouteAll) {
		got[s.prefix] = s.where
	}
	for _, want := range []string{"10.96.0.0/12", "10.11.9.0/24", "10.11.9.1/32"} {
		if _, ok := got[netip.MustParsePrefix(want)]; !ok {
			t.Errorf("parseIPRoutes lost %s, which is a range something on this host named", want)
		}
	}
	// THE PREFIX CUT. A VPN pushing the whole of 10/8 must not veto the entire pool, or an
	// employee's laptop can never install -- while 10.96.0.0/12 above must survive, which is why
	// the cut is /12 and not the more obvious /16.
	if _, ok := got[netip.MustParsePrefix("10.0.0.0/8")]; ok {
		t.Error("parseIPRoutes honoured a 10.0.0.0/8 catch-all; nothing could then be drawn")
	}
	// Anything outside 10/8 cannot collide with either pool, so carrying it only lengthens a message.
	for _, unwanted := range []string{"172.17.0.0/16", "192.168.9.0/24", "224.0.0.0/4"} {
		if _, ok := got[netip.MustParsePrefix(unwanted)]; ok {
			t.Errorf("parseIPRoutes kept %s, which is outside the pool", unwanted)
		}
	}
	if where := got[netip.MustParsePrefix("10.96.0.0/12")]; where != "routed via cni0" {
		t.Errorf("source of 10.96.0.0/12 = %q, want %q", where, "routed via cni0")
	}
}

// /proc/net/route is the fallback for a host with no `ip`. Little-endian hex, main table only.
// Iface Destination Gateway Flags RefCnt Use Metric Mask MTU Window IRTT
const procRoute = `Iface	Destination	Gateway 	Flags	RefCnt	Use	Metric	Mask		MTU	Window	IRTT
enp3s0	00000000	0109A8C0	0003	0	0	100	00000000	0	0	0
briard-priv0	00090B0A	00000000	0001	0	0	0	00FFFFFF	0	0	0
wg0	0000000A	00000000	0001	0	0	0	000000FF	0	0	0
`

func TestParseProcRoutes(t *testing.T) {
	got := map[netip.Prefix]string{}
	for _, s := range parseProcRoutes(procRoute) {
		got[s.prefix] = s.where
	}
	if where := got[netip.MustParsePrefix("10.11.9.0/24")]; where != "routed via briard-priv0" {
		t.Errorf("10.11.9.0/24 = %q, want %q -- the little-endian decode is the whole job here", where, "routed via briard-priv0")
	}
	if _, ok := got[netip.MustParsePrefix("10.0.0.0/8")]; ok {
		t.Error("the /8 catch-all survived the prefix cut")
	}
	if len(got) != 1 {
		t.Errorf("parsed %v, want only the /24 -- the default route and the /8 are both dropped", got)
	}
}

func TestWorthHonouringIsTheCut(t *testing.T) {
	for _, tc := range []struct {
		cidr string
		want bool
	}{
		{"10.0.0.0/8", false},  // the whole-RFC1918 catch-all a VPN pushes
		{"10.0.0.0/11", false}, // still too broad to be somebody NAMING a range
		{"10.96.0.0/12", true}, // Kubernetes' service CIDR -- a genuine collision
		{"10.11.9.0/24", true},
		{"10.11.9.1/32", true},
		{"192.168.1.0/24", false}, // outside the pool entirely
		{"172.16.0.0/12", false},
	} {
		if got := worthHonouring(netip.MustParsePrefix(tc.cidr)); got != tc.want {
			t.Errorf("worthHonouring(%s) = %v, want %v", tc.cidr, got, tc.want)
		}
	}
}

// A probe that cannot ask must answer "nothing answered", never "collision": a probe able to
// exhaust the draw could refuse an install over a question it never managed to put.
func TestLANProbeWithNoNICCannotRefuse(t *testing.T) {
	if LANProbe(t.Context(), "")(netip.MustParseAddr("10.42.7.1")) {
		t.Error("LANProbe with no NIC claimed an answer it could not have heard")
	}
}
