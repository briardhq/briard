package platform

import (
	"strings"
	"testing"
)

// The `via` form is the whole point of the route, so it is asserted rather than assumed: the
// guest answers ARP only on the interface holding the address asked for (arp_ignore=1), which
// makes the on-link spelling -- `dev <tap>` with no `via` -- a route that installs cleanly and
// then black-holes. A regression to that form would pass every other test in the tree.
func TestRouteReplaceArgs(t *testing.T) {
	got := strings.Join(routeReplaceArgs(VIPRoute{
		Addr: "192.168.9.225", Via: "10.9.9.2", Dev: "briard-priv0", Src: "10.9.9.1",
	}), " ")
	want := "route replace 192.168.9.225/32 via 10.9.9.2 dev briard-priv0 src 10.9.9.1"
	if got != want {
		t.Errorf("ip %s\nwant ip %s", got, want)
	}
}

// A /32 and nothing wider: the household's LAN prefix is not ours to route.
func TestRouteReplaceArgs_HostRouteOnly(t *testing.T) {
	for _, a := range routeReplaceArgs(VIPRoute{Addr: "10.0.0.5", Via: "10.9.9.2", Dev: "d", Src: "10.9.9.1"}) {
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
	full := VIPRoute{Addr: "192.168.9.225", Via: "10.9.9.2", Dev: "briard-priv0", Src: "10.9.9.1"}
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
