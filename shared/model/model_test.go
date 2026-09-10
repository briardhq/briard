package model

import "testing"

// ServingUnit is the one definition of "which unit is this service?". A service names its
// quadlet-rendered container explicitly, and anything else names NOTHING — never a unit assembled
// from the service name, which is what once had the host asking systemd about "podman-.service"
// and being told "inactive" forever. The name-only case is asserted because it is the one a
// derivation would be tempted to answer.
func TestServingUnit(t *testing.T) {
	cases := []struct {
		name string
		spec ServiceSpec
		want string
	}{
		{"a service names its rendered unit",
			ServiceSpec{Name: "home-assistant", Unit: "briard-home-assistant-app.service"},
			"briard-home-assistant-app.service"},
		{"a name alone derives nothing", ServiceSpec{Name: "home-assistant"}, ""},
		{"no service names nothing", ServiceSpec{}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.spec.ServingUnit(); got != tc.want {
				t.Errorf("ServingUnit() = %q, want %q", got, tc.want)
			}
		})
	}
}

// Serving is THE "does this node hold the house" predicate, and both conjuncts are load-bearing:
// a Primary that has lost quorum is refused its writes and about to be demoted, so it is not
// serving; a quorate Secondary is participating, not serving. Pinned so no reader is tempted
// back to Primary alone.
func TestServing(t *testing.T) {
	cases := []struct {
		name string
		qs   QuorumState
		want bool
	}{
		{"quorate primary serves", QuorumState{Primary: true, Quorate: true}, true},
		{"primary without quorum does not", QuorumState{Primary: true}, false},
		{"quorate secondary participates, does not serve", QuorumState{Quorate: true}, false},
		{"nothing serves nothing", QuorumState{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.qs.Serving(); got != tc.want {
				t.Errorf("Serving() = %t, want %t", got, tc.want)
			}
		})
	}
}
