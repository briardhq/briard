package subnet

import (
	"net/netip"
	"strings"
	"testing"
)

// cycler is a deterministic entropy stand-in: it hands out the given octets over and over, so a
// test can say exactly which candidates the draw is offered and in which order. Never returns an
// error, so an exhausted draw exhausts on COLLISIONS rather than on running out of bytes -- which
// is the failure this file is about.
type cycler struct {
	b []byte
	i int
}

func (c *cycler) Read(p []byte) (int, error) {
	for n := range p {
		p[n] = c.b[c.i%len(c.b)]
		c.i++
	}
	return len(p), nil
}

// octets spells a sequence of candidate proposals: two bytes per system candidate, one per link
// candidate, in the order Pick consumes them.
func octets(v ...byte) *cycler { return &cycler{b: v} }

func TestPickSkipsTheConventionalOccupants(t *testing.T) {
	// Offered in order: 10.0.0 (Xfinity), 10.0.2 (our own SLIRP), 10.11.9 (the link pool, which
	// the flock pool must never draw from), then a free one. Only the last may survive.
	r := octets(0, 0, 0, 2, 11, 9, 173, 94 /* link: */, 71)
	d, err := Pick(Observed{}, r, nil)
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	if d.System != "10.173.94" {
		t.Errorf("system subnet = %q, want 10.173.94 (the first candidate no vendor has spoken for)", d.System)
	}
	if d.Priv != "10.11.71" {
		t.Errorf("private link = %q, want 10.11.71", d.Priv)
	}
}

// The link pool is 10.11.x and the flock pool excludes 10.11.x, which is what keeps a node's
// private link from ever landing on its own flock's subnet -- structurally, rather than by a
// check either draw could forget to make.
func TestPoolsAreStructurallyDisjoint(t *testing.T) {
	link := netip.MustParsePrefix("10.11.0.0/16")
	// Entropy that offers the flock draw the link pool itself, twice, before anything free.
	d, err := Pick(Observed{}, octets(11, 11, 11, 250, 42, 7 /* link: */, 71), nil)
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	if sys := netip.MustParsePrefix(d.System + ".0/24"); link.Overlaps(sys) {
		t.Errorf("system subnet %s landed in the link pool %s", sys, link)
	}
	if d.System != "10.42.7" {
		t.Errorf("system subnet = %q, want 10.42.7 (the first offer outside 10.11/16)", d.System)
	}
	if p := netip.MustParsePrefix(d.Priv + ".0/24"); !link.Overlaps(p) {
		t.Errorf("private link %s is outside the link pool %s", p, link)
	}
}

func TestPickRejectsWhatTheHostAlreadyHas(t *testing.T) {
	// A corporate VPN that named 10.173.0.0/16, plus this machine's own LAN at 10.11.71.0/24 --
	// the L3 collision that made the LINK's subnet need a draw of its own ([V3b.26f]): it never
	// touches the LAN, but the host's routing table is shared.
	obs := Observed{
		Prefixes: []netip.Prefix{
			netip.MustParsePrefix("10.173.0.0/16"),
			netip.MustParsePrefix("10.11.71.0/24"),
		},
		Where: map[netip.Prefix]string{
			netip.MustParsePrefix("10.173.0.0/16"): "routed via tun0",
			netip.MustParsePrefix("10.11.71.0/24"): "the address on eth0",
		},
	}
	d, err := Pick(obs, octets(173, 94, 42, 30 /* link: */, 71, 203), nil)
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	if d.System != "10.42.30" {
		t.Errorf("system subnet = %q, want 10.42.30 -- 10.173.94 is inside the VPN's /16", d.System)
	}
	if d.Priv != "10.11.203" {
		t.Errorf("private link = %q, want 10.11.203 -- 10.11.71 is the host's own LAN", d.Priv)
	}
}

func TestPickProbesTheFlockSubnetOnly(t *testing.T) {
	var asked []string
	answers := func(a netip.Addr) bool {
		asked = append(asked, a.String())
		return a.String() == "10.173.94.1" // an existing flock's node-id 0 answers here
	}
	d, err := Pick(Observed{}, octets(173, 94, 42, 30 /* link: */, 203), answers)
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	if d.System != "10.42.30" {
		t.Errorf("system subnet = %q, want 10.42.30 -- something answered on 10.173.94.1", d.System)
	}
	// .1 and not .0 or .129: node IPs are positional and node-id 0 is the one every flock has.
	if want := []string{"10.173.94.1", "10.42.30.1"}; len(asked) != 2 || asked[0] != want[0] || asked[1] != want[1] {
		t.Errorf("probed %v, want %v", asked, want)
	}
	// The link is a point-to-point tap that never reaches the LAN, so asking the LAN about it
	// would be asking the wrong network a meaningless question.
	for _, a := range asked {
		if strings.HasPrefix(a, "10.11.") {
			t.Errorf("probed the LAN for the private link at %s; the link is never on the LAN", a)
		}
	}
}

// Exhaustion must REFUSE, not invent: a host that really has 10/8 carved up gets a message naming
// the variable, which is DESIGN §4's existing posture for the VIP.
func TestPickRefusesWhenEveryCandidateCollides(t *testing.T) {
	all := Observed{Prefixes: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}}
	_, err := Pick(all, octets(173, 94), nil)
	if err == nil {
		t.Fatal("Pick accepted a subnet on a host whose whole 10/8 is spoken for; it must refuse")
	}
	if !strings.Contains(err.Error(), "BRIARD_SYSTEM_SUBNET") {
		t.Errorf("refusal %q does not name BRIARD_SYSTEM_SUBNET, so the user cannot act on it", err)
	}
	// And the link half refuses on its own variable rather than borrowing the flock's.
	linkOnly := Observed{Prefixes: []netip.Prefix{netip.MustParsePrefix("10.11.0.0/16")}}
	_, err = Pick(linkOnly, octets(173, 94), nil)
	if err == nil || !strings.Contains(err.Error(), "BRIARD_PRIV_SUBNET") {
		t.Errorf("link refusal = %v, want one naming BRIARD_PRIV_SUBNET", err)
	}
}

func TestOccupiedNamesWhatItFound(t *testing.T) {
	obs := Observed{
		Prefixes: []netip.Prefix{netip.MustParsePrefix("10.42.0.0/16")},
		Where:    map[netip.Prefix]string{netip.MustParsePrefix("10.42.0.0/16"): "routed via wg0"},
	}
	if got := obs.Occupied(netip.MustParsePrefix("10.42.7.0/24")); !strings.Contains(got, "wg0") {
		t.Errorf("Occupied = %q, want it to name wg0", got)
	}
	if got := obs.Occupied(netip.MustParsePrefix("10.43.7.0/24")); got != "" {
		t.Errorf("Occupied(10.43.7.0/24) = %q, want empty -- nothing on this host holds it", got)
	}
	// Occupied must NOT apply the conventional table: the link's entire pool is one of its
	// entries, so folding them together would reject every link candidate.
	if got := obs.Occupied(netip.MustParsePrefix("10.11.71.0/24")); got != "" {
		t.Errorf("Occupied(10.11.71.0/24) = %q, want empty -- the conventional table belongs to the flock pool alone", got)
	}
}

func TestConventionalTableCoversTheVerifiedOccupants(t *testing.T) {
	for _, tc := range []struct{ cidr, want string }{
		{"10.0.0.0/24", "Xfinity"},
		{"10.0.1.0/24", "AirPort"},
		{"10.0.2.0/24", "SLIRP"},
		{"10.8.0.0/24", "OpenVPN"},
		{"10.1.0.0/24", "round number"},
		{"10.10.0.0/24", "round number"},
		{"10.100.0.0/24", "Kubernetes"},   // inside 10.96.0.0/12, so the belt entry under it is redundant today
		{"10.111.255.0/24", "Kubernetes"}, // the far end of 10.96.0.0/12
		{"10.244.9.0/24", "Flannel"},
		{"10.147.17.0/24", "ZeroTier"},
		{"10.11.9.0/24", "private-link"},
	} {
		got := conventional(netip.MustParsePrefix(tc.cidr))
		if !strings.Contains(got, tc.want) {
			t.Errorf("conventional(%s) = %q, want it to mention %q", tc.cidr, got, tc.want)
		}
	}
	// And it must not be a blanket: the pool has to have something left in it.
	for _, free := range []string{"10.173.94.0/24", "10.42.7.0/24", "10.95.255.0/24", "10.112.0.0/24"} {
		if got := conventional(netip.MustParsePrefix(free)); got != "" {
			t.Errorf("conventional(%s) = %q, want it free", free, got)
		}
	}
}

func TestReportIsTheFormatInstallShParses(t *testing.T) {
	var b strings.Builder
	if err := Report(&b, Draw{System: "10.173.94", Priv: "10.11.203"}); err != nil {
		t.Fatalf("Report: %v", err)
	}
	if got, want := b.String(), "SYSTEM_SUBNET=10.173.94\nPRIV_SUBNET=10.11.203\n"; got != want {
		t.Errorf("Report wrote %q, want %q", got, want)
	}
}
