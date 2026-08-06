package model

import "testing"

// ServingUnit is the one definition of "which unit is this service?", and it has to be right for
// both shapes: a runtime-installed service names its quadlet-rendered container explicitly, the
// baked slot derives from the name, and a node with no service names nothing at all (asking
// systemd about "podman-.service" is what once answered "inactive" forever).
func TestServingUnit(t *testing.T) {
	cases := []struct {
		name string
		spec ServiceSpec
		want string
	}{
		{"runtime-installed names its unit",
			ServiceSpec{Name: "home-assistant", Unit: "briard-home-assistant-app.service"},
			"briard-home-assistant-app.service"},
		{"baked slot derives from the name",
			ServiceSpec{Name: "briard-payload"},
			"podman-briard-payload.service"},
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
