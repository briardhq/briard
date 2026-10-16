package host

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"

	"briard.io/agent/nic"
)

// fakeVIPGuest answers net.vip.
type fakeVIPGuest struct {
	cidr string
	err  error
}

func (g *fakeVIPGuest) VIP(context.Context, string) (string, error) { return g.cidr, g.err }

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
	r.c.tick(context.Background(), r.cfg, g, func(f string, a ...any) { r.lines = append(r.lines, fmt.Sprintf(f, a...)) })
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

// The gate holds for the VIP too: a guest holding the host's own address -- which no lease of
// ours ever hands it (lease.go) -- leaves the service child empty.
func TestIPvtapCopierNeverCopiesTheHostsAddressAsVIP(t *testing.T) {
	r := newCopierRig(true)
	r.tick(&fakeVIPGuest{cidr: "192.168.7.50/24"})
	if got, ok := r.last()["briard0"]; !ok || got != nil {
		t.Errorf("held the host's own address as the VIP: %v (present %v)", got, ok)
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
