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
