package platform

import (
	"context"
	"slices"
	"strings"
	"testing"
)

// The `via` form is the whole point of the route, so it is asserted rather than assumed: the
// guest answers ARP only on the interface holding the address asked for (arp_ignore=1), which
// makes the on-link spelling -- `dev <tap>` with no `via` -- a route that installs cleanly and
// then black-holes. A regression to that form would pass every other test in the tree.
func TestRouteReplaceArgs(t *testing.T) {
	got := strings.Join(routeReplaceArgs(VIPRoute{
		Addr: "192.168.9.225", Via: "10.11.9.2", Dev: "briard-priv0", Src: "10.11.9.1",
	}), " ")
	want := "route replace 192.168.9.225/32 via 10.11.9.2 dev briard-priv0 src 10.11.9.1"
	if got != want {
		t.Errorf("ip %s\nwant ip %s", got, want)
	}
}

// A /32 and nothing wider: the household's LAN prefix is not ours to route.
func TestRouteReplaceArgs_HostRouteOnly(t *testing.T) {
	for _, a := range routeReplaceArgs(VIPRoute{Addr: "10.0.0.5", Via: "10.11.9.2", Dev: "d", Src: "10.11.9.1"}) {
		if strings.Contains(a, "/") && a != "10.0.0.5/32" {
			t.Errorf("route names %q, want only the single host address", a)
		}
	}
}

// The delete is keyed on device as well as address, so it can never withdraw a route the
// household's own network put there.
func TestRouteDelArgs(t *testing.T) {
	got := strings.Join(routeDelArgs("192.168.9.225", "briard-priv0"), " ")
	want := "route del 192.168.9.225/32 dev briard-priv0"
	if got != want {
		t.Errorf("ip %s\nwant ip %s", got, want)
	}
}

func TestRouteAbsent(t *testing.T) {
	cases := []struct {
		out  string
		want bool
	}{
		{"RTNETLINK answers: No such process\n", true},
		{"RTNETLINK answers: No such file or directory\n", true},
		{"Cannot find device \"briard-priv0\"\n", false},
		{"RTNETLINK answers: Operation not permitted\n", false},
		{"", false},
	}
	for _, c := range cases {
		if got := routeAbsent(c.out); got != c.want {
			t.Errorf("routeAbsent(%q) = %v, want %v", c.out, got, c.want)
		}
	}
}

// An incomplete spec is refused rather than handed to `ip`, which would otherwise be asked to
// parse "/32" as an address.
func TestSetVIPRoute_RejectsIncompleteSpec(t *testing.T) {
	full := VIPRoute{Addr: "192.168.9.225", Via: "10.11.9.2", Dev: "briard-priv0", Src: "10.11.9.1"}
	for _, blank := range []func(VIPRoute) VIPRoute{
		func(r VIPRoute) VIPRoute { r.Addr = ""; return r },
		func(r VIPRoute) VIPRoute { r.Via = ""; return r },
		func(r VIPRoute) VIPRoute { r.Dev = ""; return r },
		func(r VIPRoute) VIPRoute { r.Src = ""; return r },
	} {
		if err := SetVIPRoute(t.Context(), blank(full)); err == nil {
			t.Errorf("SetVIPRoute(%+v) = nil, want an error", blank(full))
		}
	}
}

// The node route's two argvs (SetNodeRoute issues the neighbour first; the order is not asserted
// here). What reading the code cannot confirm is tested: the neighbour entry is permanent and pins
// the derived MAC, and the route carries a src. Failable: drop `nud permanent` and the kernel
// expires the entry, then re-ARPs for an address the guest will not answer for on this interface
// (arp_ignore=1, the ARP-flux fix), and the path dies minutes after it was proven working.
func TestNodeRouteArgs(t *testing.T) {
	r := NodeRoute{GuestIP: "10.0.0.1", Dev: "briard-priv0", Src: "10.0.0.129", LLAddr: "52:54:00:ab:cd:ef"}

	wantNeigh := []string{"neigh", "replace", "10.0.0.1", "lladdr", "52:54:00:ab:cd:ef", "dev", "briard-priv0", "nud", "permanent"}
	if got := nodeNeighArgs(r); !slices.Equal(got, wantNeigh) {
		t.Errorf("neigh argv = %v, want %v", got, wantNeigh)
	}
	wantRoute := []string{"route", "replace", "10.0.0.1/32", "dev", "briard-priv0", "src", "10.0.0.129"}
	if got := nodeRouteArgs(r); !slices.Equal(got, wantRoute) {
		t.Errorf("route argv = %v, want %v", got, wantRoute)
	}
}

// Every field is load-bearing, so an incomplete spec must be refused rather than handed to `ip`
// to produce a partial path. Failable: relax the guard and each of these silently installs a
// route with an empty argument.
func TestSetNodeRouteRefusesIncompleteSpec(t *testing.T) {
	full := NodeRoute{GuestIP: "10.0.0.1", Dev: "briard-priv0", Src: "10.0.0.129", LLAddr: "52:54:00:ab:cd:ef"}
	for _, missing := range []string{"GuestIP", "Dev", "Src", "LLAddr"} {
		r := full
		switch missing {
		case "GuestIP":
			r.GuestIP = ""
		case "Dev":
			r.Dev = ""
		case "Src":
			r.Src = ""
		case "LLAddr":
			r.LLAddr = ""
		}
		if err := SetNodeRoute(context.Background(), r); err == nil {
			t.Errorf("missing %s: want an error, got nil", missing)
		}
	}
}

// The MAC parse, against real `ip -o link show` text. Split out and tested because the private
// link's host end is READ rather than derived (a tap's MAC is random per creation), so a parse
// that silently returns "" would leave the guest with no neighbour entry and send it back to the
// broken ARP this whole mechanism exists to avoid.
func TestLinkMACFrom(t *testing.T) {
	const real = `7: briard-priv0: <BROADCAST,MULTICAST,UP,LOWER_UP> mtu 1500 qdisc fq_codel state UP mode DEFAULT group default qlen 1000\    link/ether 1a:dc:6c:07:70:5b brd ff:ff:ff:ff:ff:ff`
	if got := linkMACFrom(real); got != "1a:dc:6c:07:70:5b" {
		t.Errorf("linkMACFrom = %q, want the ether address", got)
	}
	// A device with no link address must read as "" rather than as something plausible -- the
	// caller turns "" into "no neighbour entry", which is a degradation it logs, not a wrong pin.
	if got := linkMACFrom(`3: lo: <LOOPBACK,UP> mtu 65536 qdisc noqueue state UNKNOWN`); got != "" {
		t.Errorf("linkMACFrom on an ether-less device = %q, want \"\"", got)
	}
}

// The check reads the kernel's tables as the kernel writes them: the samples are /proc/net/route
// and /proc/net/arp's real layout, holding the two routes and the neighbour SetNodeRoute and
// SetVIPRoute install, next to the entries that must not count as them.
func TestTheRoutesAreReadFromTheKernelsTables(t *testing.T) {
	route := []byte("Iface\tDestination\tGateway \tFlags\tRefCnt\tUse\tMetric\tMask\t\tMTU\tWindow\tIRTT\n" +
		"eno1\t00000000\t010AA8C0\t0003\t0\t0\t1024\t00000000\t0\t0\t0\n" +
		"briard-priv0\t00090B0A\t00000000\t0001\t0\t0\t0\t00FFFFFF\t0\t0\t0\n" +
		"briard-priv0\t02090B0A\t00000000\t0005\t0\t0\t0\tFFFFFFFF\t0\t0\t0\n" +
		"briard-priv0\tE109A8C0\t02090B0A\t0007\t0\t0\t0\tFFFFFFFF\t0\t0\t0\n")
	arp := []byte("IP address       HW type     Flags       HW address            Mask     Device\n" +
		"192.168.10.1     0x1         0x2         f4:fc:49:35:a0:68     *        eno1\n" +
		"10.11.9.2        0x1         0x6         52:54:00:AB:CD:EF     *        briard-priv0\n" +
		"10.11.9.3        0x1         0x2         52:54:00:00:00:03     *        briard-priv0\n")
	for _, c := range []struct {
		name           string
		addr, dev, via string
		want           bool
	}{
		{"the node route", "10.11.9.2", "briard-priv0", "", true},
		{"the VIP route", "192.168.9.225", "briard-priv0", "10.11.9.2", true},
		{"the VIP on-link, not via the guest", "192.168.9.225", "briard-priv0", "", false},
		{"another device", "10.11.9.2", "eno1", "", false},
		{"the link's /24 is no /32", "10.11.9.0", "briard-priv0", "", false},
		{"absent", "10.11.9.7", "briard-priv0", "", false},
	} {
		if got := routeListed(route, c.addr, c.dev, c.via); got != c.want {
			t.Errorf("%s: routeListed = %v, want %v", c.name, got, c.want)
		}
	}
	for _, c := range []struct {
		name         string
		ip, mac, dev string
		want         bool
	}{
		{"pinned, the MAC in either case", "10.11.9.2", "52:54:00:ab:cd:ef", "briard-priv0", true},
		{"another MAC", "10.11.9.2", "52:54:00:ab:cd:00", "briard-priv0", false},
		{"another device", "10.11.9.2", "52:54:00:ab:cd:ef", "eno1", false},
		{"learned, not pinned", "10.11.9.3", "52:54:00:00:00:03", "briard-priv0", false},
		{"absent", "10.11.9.9", "52:54:00:ab:cd:ef", "briard-priv0", false},
	} {
		if got := neighPinned(arp, c.ip, c.mac, c.dev); got != c.want {
			t.Errorf("%s: neighPinned = %v, want %v", c.name, got, c.want)
		}
	}
}
