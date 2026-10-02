package host

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"briard.io/agent/nic"
	"briard.io/agent/reportcard"
)

// fakeVIPGuest answers net.vip and records the writes the copier asks for, in order.
type fakeVIPGuest struct {
	cidr  string
	err   error
	stops []string
}

func (g *fakeVIPGuest) VIP(context.Context, string) (string, error) { return g.cidr, g.err }
func (g *fakeVIPGuest) ServiceStop(_ context.Context, unit string) error {
	g.stops = append(g.stops, unit)
	return nil
}
func (g *fakeVIPGuest) ForgetVIP(context.Context) error {
	g.stops = append(g.stops, "forget")
	return nil
}

// copierRig is one copier with its doubles, over a wireless node whose guest node IP is 10.42.7.1.
type copierRig struct {
	c     *ipvtapCopier
	host  []string
	held  []map[string][]string
	lines []string
	cfg   Config
}

func newCopierRig(ipvtap bool) *copierRig {
	r := &copierRig{host: []string{"192.168.7.50"}}
	r.c = &ipvtapCopier{
		hold: func(_ context.Context, want map[string][]string) error {
			r.held = append(r.held, want)
			return nil
		},
		hostAddrs: func(string) []string { return r.host },
	}
	r.cfg = Config{SystemCIDR: "10.42.7.1/24", VIPDev: "eth2"}
	r.cfg.net = &nic.Spec{Parent: "wlan0", Ipvtap: ipvtap, SystemTap: "briard-drbd0", ServiceTap: "briard0"}
	return r
}

func (r *copierRig) tick(g *fakeVIPGuest) {
	r.c.tick(context.Background(), r.cfg, g, nil, func(f string, a ...any) { r.lines = append(r.lines, fmt.Sprintf(f, a...)) })
}

func (r *copierRig) last() map[string][]string { return r.held[len(r.held)-1] }

// Off ipvtap nothing is copied: the copy is this substrate's duty and no other's.
func TestIPvtapCopierInertOffIpvtap(t *testing.T) {
	r := newCopierRig(false)
	r.tick(&fakeVIPGuest{cidr: "192.168.7.120/24"})
	if len(r.held) != 0 {
		t.Errorf("a macvtap node had addresses copied: %v", r.held)
	}
}

// The ordinary tick: the node IP onto the system child, the live VIP onto the service child.
func TestIPvtapCopierHoldsTheGuestsAddresses(t *testing.T) {
	r := newCopierRig(true)
	r.tick(&fakeVIPGuest{cidr: "192.168.7.120/24"})
	want := map[string][]string{"briard-drbd0": {"10.42.7.1"}, "briard0": {"192.168.7.120"}}
	if got := r.last(); !mapsEqual(got, want) {
		t.Errorf("held %v, want %v", got, want)
	}
	// The lease moved: the copy follows it, and the old address is dropped by Hold's diff.
	r.tick(&fakeVIPGuest{cidr: "192.168.7.121/24"})
	if got := r.last()["briard0"]; !slices.Equal(got, []string{"192.168.7.121"}) {
		t.Errorf("after the lease moved, held %v", got)
	}
	// No address (between leases, or a standby): nothing held on the service child.
	r.tick(&fakeVIPGuest{cidr: ""})
	if got, ok := r.last()["briard0"]; !ok || got != nil {
		t.Errorf("with no VIP the service child must be emptied, held %v (present %v)", got, ok)
	}
}

// A guest that does not answer leaves the service child alone -- a channel blip must not cost the
// LAN its reach -- while the node IP, which the host knows itself, is still held.
func TestIPvtapCopierLeavesTheVIPOnABlip(t *testing.T) {
	r := newCopierRig(true)
	r.tick(&fakeVIPGuest{err: errors.New("channel down")})
	got := r.last()
	if _, ok := got["briard0"]; ok {
		t.Errorf("a failed read touched the service child: %v", got)
	}
	if !slices.Equal(got["briard-drbd0"], []string{"10.42.7.1"}) {
		t.Errorf("the node IP was not held: %v", got)
	}
}

// THE GATE. An address the host holds on the parent is never copied: ipvlan would accept it and
// the host would drop off the LAN. Read each tick, so a host lease that moves is seen.
func TestIPvtapCopierNeverCopiesTheHostsAddress(t *testing.T) {
	r := newCopierRig(true)
	r.host = []string{"192.168.7.50", "10.42.7.1"}
	r.tick(&fakeVIPGuest{cidr: "192.168.7.120/24"})
	if got := r.last()["briard-drbd0"]; got != nil {
		t.Errorf("copied the host's own address onto a child: %v", got)
	}
}

// A ROUTER THAT IGNORES CLIENT IDS hands the guest the host's own address. Refused: the VIP's
// client is stopped (without a RELEASE -- the unit's stop path), the copy is never made, the
// route is never installed, and the household is told what to do. Said once, stopped each time.
func TestIPvtapCopierRefusesTheHostsAddressAsVIP(t *testing.T) {
	r := newCopierRig(true)
	g := &fakeVIPGuest{cidr: "192.168.7.50/24"}
	r.tick(g)
	if got := r.last()["briard0"]; got != nil {
		t.Errorf("held the host's own address as the VIP: %v", got)
	}
	if !r.c.refused() || !slices.Equal(g.stops, []string{"briard-vip.service", "forget"}) {
		t.Fatalf("refused=%v stops=%v, want refused, the VIP unit stopped, then its address forgotten", r.c.refused(), g.stops)
	}
	said := strings.Join(r.lines, "\n")
	for _, want := range []string{"network: refused: ", "told apart only by DHCP client ID", "192.168.7.50", "briard config set vip <address>/24"} {
		if !strings.Contains(said, want) {
			t.Errorf("the refusal does not say %q:\n%s", want, said)
		}
	}
	// Seen again (the guest restarted and its client ran again): stopped again, not said again.
	n := len(r.lines)
	r.tick(g)
	if len(g.stops) != 4 || len(r.lines) != n {
		t.Errorf("second sighting: stops=%v, new lines=%q", g.stops, r.lines[n:])
	}
	// Sticky: a later, different address is not held either -- the remedy is the household naming one.
	r.tick(&fakeVIPGuest{cidr: "192.168.7.120/24"})
	if got := r.last()["briard0"]; got != nil {
		t.Errorf("a refused node held a VIP: %v", got)
	}
	detail, fix := r.c.refusal()
	cs := judgeDoctor(doctorFacts{Refused: detail, RefusedFix: fix, Cluster: servingLone().Cluster})
	if c := checkNamed(t, cs, "address"); c.Status != reportcard.Refuse || !strings.Contains(c.Fix, "config set vip") {
		t.Errorf("the doctor's address check = %+v, want the refusal and its remedy", c)
	}
}

// The control for the refusal: a VIP that is NOT the host's is held, and nothing is stopped.
func TestIPvtapCopierDoesNotRefuseADifferentAddress(t *testing.T) {
	r := newCopierRig(true)
	g := &fakeVIPGuest{cidr: "192.168.7.51/24"}
	r.tick(g)
	if r.c.refused() || len(g.stops) != 0 {
		t.Errorf("refused=%v stops=%v for an address the host does not hold", r.c.refused(), g.stops)
	}
}

func mapsEqual(a, b map[string][]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || !slices.Equal(v, w) {
			return false
		}
	}
	return true
}
