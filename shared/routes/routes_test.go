package routes

import (
	"strings"
	"testing"
)

// The name shape is the one measured constraint the whole design turns on (V3.19d): ONE label
// before `.local`, because `mdns4_minimal` -- the resolver in Debian/Ubuntu's nsswitch -- handles
// exactly one. A hierarchical `home-assistant.briard-brave-elf.local` would publish fine and
// resolve nowhere, which is the failure that is invisible from the publishing side.
func TestHostNameIsASingleLabel(t *testing.T) {
	got := HostName("brave-elf", "home-assistant")
	if got != "briard-brave-elf-home-assistant.local" {
		t.Fatalf("HostName = %q, want the flock-scoped single label", got)
	}
	if labels := strings.Split(strings.TrimSuffix(got, ".local"), "."); len(labels) != 1 {
		t.Fatalf("HostName = %q, which is %d labels before .local; mdns4_minimal resolves one", got, len(labels))
	}
	// No flock name, no name: `briard--home-assistant.local` is worse than silence, and the same
	// rule already governs the flock's own publisher.
	if got := HostName("", "home-assistant"); got != "" {
		t.Errorf("HostName with no flock = %q, want no name at all", got)
	}
}

// What a client actually sends in Host, all of which must find the same service: the port when it
// is not the scheme's default, a trailing dot from a fully-qualified resolver, and any case.
func TestLookupNormalisesWhatClientsSend(t *testing.T) {
	tbl := Table{Services: []Service{
		{Name: "home-assistant", Hosts: []string{"briard-brave-elf-home-assistant.local"}, Address: "http://127.0.0.1:8123"},
	}}
	for _, host := range []string{
		"briard-brave-elf-home-assistant.local",
		"briard-brave-elf-home-assistant.local:80",
		"briard-brave-elf-home-assistant.local.",
		"briard-brave-elf-home-assistant.local.:443",
		"BRIARD-BRAVE-ELF-Home-Assistant.local",
		"  briard-brave-elf-home-assistant.local  ",
	} {
		if s, ok := tbl.Lookup(host); !ok || s.Name != "home-assistant" {
			t.Errorf("Lookup(%q) = %+v %v, want the service", host, s, ok)
		}
	}
	if _, ok := tbl.Lookup("192.168.1.100"); ok {
		t.Error("the bare address matched a service; it belongs to the node, not to one of them")
	}
}

// An IPv6 literal has colons of its own, so the port must come off the LAST one and only when the
// address is not bare -- a naive split truncates every v6 Host to "[".
func TestNormaliseKeepsIPv6Literals(t *testing.T) {
	for in, want := range map[string]string{
		"[fe80::1]:8080": "[fe80::1]",
		"[fe80::1]":      "[fe80::1]",
		"fe80::1":        "fe80::1",
		"host.local:80":  "host.local",
	} {
		if got := Normalise(in); got != want {
			t.Errorf("Normalise(%q) = %q, want %q", in, got, want)
		}
	}
}

// The table is written by one process and read by another, so it has to survive the round trip
// exactly -- and refuse a field this build does not understand rather than route on a table it
// only partly read.
func TestMarshalParseRoundTripAndRefusesUnknownFields(t *testing.T) {
	in := Table{Services: []Service{
		{Name: "home-assistant", Hosts: []string{"briard-brave-elf-home-assistant.local"}, Address: "http://127.0.0.1:8123", HealthPath: "/manifest.json"},
		{Name: "mosquitto", Hosts: []string{"briard-brave-elf-mosquitto.local"}},
	}}
	raw, err := in.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	out, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Services) != 2 || out.Services[0].Address != "http://127.0.0.1:8123" || out.Services[1].Address != "" {
		t.Fatalf("round trip lost something: %+v", out.Services)
	}
	// A not-fronted service keeps its name and its place in the table: the door must be able to
	// say "this is ours and it does not answer HTTP" rather than 404.
	if s, ok := out.Lookup("briard-brave-elf-mosquitto.local"); !ok || s.Address != "" {
		t.Fatalf("the not-fronted service = %+v %v, want a match the door may not front", s, ok)
	}
	if got := out.Hosts(); len(got) != 2 {
		t.Fatalf("Hosts() = %v, want both names (it is the publisher's whole input)", got)
	}
	if _, err := Parse([]byte(`{"services":[{"name":"x","privileged":true}]}`)); err == nil {
		t.Error("a table carrying an unknown field parsed; a partly-understood table must be refused")
	}
}
