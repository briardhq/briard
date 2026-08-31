package routes

import (
	"strings"
	"testing"
)

// svc is a fronted service in the shape converge writes.
func svc(name, host string, port string) Service {
	return Service{
		Name: name, Hosts: []string{host}, Address: "127.0.0.1",
		Health: "http://:" + port + "/healthz",
		Routes: []Route{{Listen: ListenName, To: "http://:" + port}},
	}
}

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
	tbl := Table{Services: []Service{svc("home-assistant", "briard-brave-elf-home-assistant.local", "8123")}}
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

// ONE ADDRESS PER SERVICE, spliced into every host-less spec. A service is one pod and a pod is one
// network namespace, so the address is a service fact and the endpoints carry only ports -- which
// is what makes the private-network flip change one field rather than every URL in the file.
func TestResolveSplicesTheServiceAddress(t *testing.T) {
	s := Service{Name: "x", Address: "10.89.0.7", Health: "http://:9883/api/v1/listeners",
		Routes: []Route{{Listen: ListenName, To: "http://:9001"}}}
	for spec, want := range map[string]string{
		"http://:9883/api/v1/listeners": "http://10.89.0.7:9883/api/v1/listeners",
		"http://:9001":                  "http://10.89.0.7:9001",
	} {
		u, err := s.Resolve(spec)
		if err != nil || u.String() != want {
			t.Errorf("Resolve(%q) = %v, %v; want %q", spec, u, err, want)
		}
	}
	// IPv6 addresses must come back bracketed, or the dialer parses the last group as a port.
	v6 := Service{Name: "x", Address: "fd00::5"}
	if u, err := v6.Resolve("http://:8080"); err != nil || u.String() != "http://[fd00::5]:8080" {
		t.Errorf("Resolve on a v6 address = %v, %v", u, err)
	}
	// No address is an error, never a silent localhost.
	if _, err := (Service{Name: "x"}).Resolve("http://:1"); err == nil {
		t.Error("a service with no address resolved; the caller would dial nowhere")
	}
}

// THE HOST-LESS RULE IS A SECURITY BOUNDARY, not a spelling convention: the only address that can
// ever be spliced in is the service's own, so no table -- however written -- can aim the front
// door at another machine. Validate is where that becomes true of the document rather than of the
// code that happens to read it.
func TestValidateRefusesATargetThatNamesAHost(t *testing.T) {
	for _, tbl := range []Table{
		{Services: []Service{{Name: "x", Address: "127.0.0.1",
			Routes: []Route{{Listen: ListenName, To: "http://evil.example:80"}}}}},
		{Services: []Service{{Name: "x", Address: "127.0.0.1", Health: "http://evil.example:80/z"}}},
	} {
		if err := tbl.Validate(); err == nil {
			t.Errorf("a table naming another host validated: %+v", tbl.Services)
		} else if !strings.Contains(err.Error(), "host") {
			t.Errorf("unhelpful error for an off-node target: %v", err)
		}
	}
	// And a port is required: a spec without one would dial :80 by default, silently.
	bad := Table{Services: []Service{{Name: "x", Address: "127.0.0.1",
		Routes: []Route{{Listen: ListenName, To: "http://"}}}}}
	if err := bad.Validate(); err == nil {
		t.Error("a route with no port validated")
	}
}

// An unknown listener is REFUSED, never skipped. A route the door silently drops is a service that
// is unreachable with nothing saying why -- the failure mode that looks like a user error. This is
// also what makes adding `tls:<port>` a change nobody can half-deploy.
func TestValidateRefusesAnUnimplementedListener(t *testing.T) {
	tbl := Table{Services: []Service{{Name: "mosquitto", Address: "127.0.0.1",
		Routes: []Route{{Listen: "tls:8883", To: "http://:1883"}}}}}
	if err := tbl.Validate(); err == nil {
		t.Fatal("a route on an unimplemented listener validated; it would be silently unserved")
	} else if !strings.Contains(err.Error(), "tls:8883") {
		t.Errorf("the error does not name the listener it refused: %v", err)
	}
}

// The table is written by one process and read by two, so it has to survive the round trip exactly
// -- and refuse a field this build does not understand rather than route on a table it only partly
// read.
func TestMarshalParseRoundTripAndRefusesUnknownFields(t *testing.T) {
	in := Table{Services: []Service{
		svc("home-assistant", "briard-brave-elf-home-assistant.local", "8123"),
		// The broker: named and probed, with NO route. Absence is how "the door may not serve
		// this" is said -- there is no address here for the door to be trusted not to use.
		{Name: "mosquitto", Hosts: []string{"briard-brave-elf-mosquitto.local"},
			Address: "127.0.0.1", Health: "http://:9883/api/v1/listeners"},
	}}
	raw, err := in.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	out, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Services) != 2 {
		t.Fatalf("round trip lost a service: %+v", out.Services)
	}
	ha, ok := out.Lookup("briard-brave-elf-home-assistant.local")
	if !ok {
		t.Fatal("the fronted service did not survive the round trip")
	}
	if r, ok := ha.Route(ListenName); !ok || r.To != "http://:8123" {
		t.Errorf("route = %+v %v, want the host-less target", r, ok)
	}
	// The broker's name is OURS -- it matches -- and there is nothing to serve under it. Both
	// halves matter: the door needs the match to say so instead of 404ing.
	mq, ok := out.Lookup("briard-brave-elf-mosquitto.local")
	if !ok {
		t.Fatal("the broker's name did not match; the door would 404 a name it owns")
	}
	if _, ok := mq.Route(ListenName); ok {
		t.Error("the broker has a route; its management API would be republished on the LAN")
	}
	if mq.Health == "" {
		t.Error("the broker has no health endpoint; not-fronted must not mean not-probed")
	}
	if _, err := Parse([]byte(`{"services":[{"name":"x","privileged":true}]}`)); err == nil {
		t.Error("a table carrying an unknown field parsed; a partly-understood table must be refused")
	}
}

// TestInstanceNameIsTheHostNameWithoutTheDomain — the instance label and the SRV target it points
// at must be the same name, and the way to guarantee that is to derive one from the other. A
// second composition would drift silently: the record would resolve nowhere while both halves
// looked plausible in the file.
func TestInstanceNameIsTheHostNameWithoutTheDomain(t *testing.T) {
	host := HostName("brave-elf", "mosquitto")
	name := InstanceName("brave-elf", "mosquitto")
	if name != "briard-brave-elf-mosquitto" {
		t.Errorf("InstanceName = %q, want the flock-scoped label", name)
	}
	if name+".local" != host {
		t.Errorf("the instance %q and the host %q are not the same name", name, host)
	}
	// No flock name yields no label, exactly as it yields no host: a node between install and its
	// first name announces nothing rather than a guess.
	if got := InstanceName("", "mosquitto"); got != "" {
		t.Errorf("a nameless flock produced the instance label %q", got)
	}
}

// TestValidateRefusesAnAnnouncementWithNothingToPointAt — the record's SRV target is the service's
// own name, so an announcement without one is a record aimed nowhere. Refused rather than
// published against the guest's hostname, which is node-scoped: on a failover every device that
// had browsed for the broker would keep dialling the machine that just demoted.
func TestValidateRefusesAnAnnouncementWithNothingToPointAt(t *testing.T) {
	tbl := Table{Services: []Service{{Name: "mosquitto", Address: "10.12.9.3",
		Announce: []Announcement{{Name: "briard-brave-elf-mosquitto", Type: "_mqtt._tcp", Port: 1883}}}}}
	if err := tbl.Validate(); err == nil {
		t.Fatal("an announcement with no host validated; it would resolve nowhere")
	} else if !strings.Contains(err.Error(), "no name to point it at") {
		t.Errorf("the error does not say what is missing: %v", err)
	}
}

// TestValidateRefusesUnusableAnnouncements — these strings become arguments to a publisher on a
// household's LAN, and the publisher splits them on tabs. A malformed type is a record avahi
// refuses and nobody notices; a tab in a name is a shifted line, i.e. a port parsed as a type.
func TestValidateRefusesUnusableAnnouncements(t *testing.T) {
	for _, tc := range []struct {
		what string
		a    Announcement
	}{
		{"no instance name", Announcement{Type: "_mqtt._tcp", Port: 1883}},
		{"a tab in the name", Announcement{Name: "briard\tevil", Type: "_mqtt._tcp", Port: 1883}},
		{"a newline in the name", Announcement{Name: "briard\nevil", Type: "_mqtt._tcp", Port: 1883}},
		{"no type", Announcement{Name: "briard-x", Port: 1883}},
		{"a bare protocol name", Announcement{Name: "briard-x", Type: "mqtt", Port: 1883}},
		{"an unknown transport", Announcement{Name: "briard-x", Type: "_mqtt._sctp", Port: 1883}},
		{"port zero", Announcement{Name: "briard-x", Type: "_mqtt._tcp"}},
		{"a port out of range", Announcement{Name: "briard-x", Type: "_mqtt._tcp", Port: 70000}},
	} {
		tbl := Table{Services: []Service{{Name: "mosquitto", Hosts: []string{"briard-x.local"},
			Address: "10.12.9.3", Announce: []Announcement{tc.a}}}}
		if err := tbl.Validate(); err == nil {
			t.Errorf("an announcement with %s validated: %+v", tc.what, tc.a)
		}
	}
	// The control: the shape converge actually writes has to pass, or the assertions above are
	// vacuous and the broker is never announced at all.
	tbl := Table{Services: []Service{{Name: "mosquitto", Hosts: []string{"briard-brave-elf-mosquitto.local"},
		Address: "10.12.9.3", Announce: []Announcement{{Name: "briard-brave-elf-mosquitto", Type: "_mqtt._tcp", Port: 1883}}}}}
	if err := tbl.Validate(); err != nil {
		t.Errorf("the broker's own announcement was refused: %v", err)
	}
	// And it survives the file, which is the only form the publisher ever sees.
	raw, err := tbl.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	out, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	mq, _ := out.Get("mosquitto")
	if len(mq.Announce) != 1 || mq.Announce[0].Type != "_mqtt._tcp" || mq.Announce[0].Port != 1883 {
		t.Errorf("the announcement did not survive the round trip: %+v", mq.Announce)
	}
}
