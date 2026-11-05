package dashboard

import (
	"testing"

	"briard.io/shared/manifest"
)

// A slug the index does not list is titled from its words.
func TestTitleOfSlug(t *testing.T) {
	for slug, want := range map[string]string{"home-assistant": "Home Assistant", "mosquitto": "Mosquitto", "z-wave-js-ui": "Z Wave Js Ui"} {
		if got := TitleOf(slug); got != want {
			t.Errorf("TitleOf(%q) = %q, want %q", slug, got, want)
		}
	}
}

// The index's title wins; an app it does not list, or no index yet, falls back to the slug.
func TestTitleIsTheIndexsFirst(t *testing.T) {
	c := &Catalog{Apps: []CatalogApp{{App: manifest.App{Name: "esphome", Title: "ESPHome"}}}}
	for _, tc := range []struct {
		c          *Catalog
		name, want string
	}{
		{c, "esphome", "ESPHome"},
		{c, "home-assistant", "Home Assistant"},
		{nil, "esphome", "Esphome"},
	} {
		if got := tc.c.Title(tc.name); got != tc.want {
			t.Errorf("Title(%q) = %q, want %q", tc.name, got, tc.want)
		}
	}
}
