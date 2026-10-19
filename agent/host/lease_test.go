package host

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"briard.io/agent/nic"
	"briard.io/shared/notify"
)

// fakeDHCP is the router, as the leaser's wire sees it: one scripted answer per message kind, and
// the record of what was sent.
type fakeDHCP struct {
	offer, ack, reboot nic.DHCPLease
	offerErr, ackErr   error
	rebootErr          error
	sent               []string
}

func (f *fakeDHCP) Discover(_ context.Context, hint netip.Addr) (nic.DHCPLease, error) {
	if hint.IsValid() {
		f.sent = append(f.sent, "DISCOVER hint "+hint.String())
		return f.offer, f.offerErr
	}
	f.sent = append(f.sent, "DISCOVER")
	return f.offer, f.offerErr
}
func (f *fakeDHCP) Request(_ context.Context, o nic.DHCPLease) (nic.DHCPLease, error) {
	f.sent = append(f.sent, "REQUEST "+o.Addr.Addr().String())
	return f.ack, f.ackErr
}
func (f *fakeDHCP) Reboot(_ context.Context, a netip.Addr) (nic.DHCPLease, error) {
	f.sent = append(f.sent, "INIT-REBOOT "+a.String())
	return f.reboot, f.rebootErr
}

func lease(addr string) nic.DHCPLease {
	return nic.DHCPLease{Addr: netip.MustParsePrefix(addr), Server: netip.MustParseAddr("192.168.7.1"),
		Time: 2 * time.Hour, T1: time.Hour}
}

var (
	ipvtapSpec = nic.Spec{Parent: "wlan0", Ipvtap: true, ServiceTap: "briard0"}
	clock      = time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
)

// leaseRig is one leaser over a fake router, on a host whose own address is 192.168.7.50.
type leaseRig struct {
	l     *vipLeaser
	wire  *fakeDHCP
	named string
	up    bool // the link's carrier, as watch reads it
	host  []string
	gw    bool   // the router answers ARP
	quiet []bool // the probe's link levers, in order: true = silenced
	lines []string
}

func newLeaseRig(t *testing.T) *leaseRig {
	r := &leaseRig{wire: &fakeDHCP{}, up: true, host: []string{"192.168.7.50"}, gw: true}
	r.l = &vipLeaser{
		path:      filepath.Join(t.TempDir(), leaseFile),
		named:     func() bool { return r.named != "" },
		ask:       func(nic.Spec) dhcpAsker { return r.wire },
		hostAddrs: func(string) []string { return r.host },
		carrier:   func(string) bool { return r.up },
		gateway:   func(context.Context, string) bool { return r.gw },
		quiet:     func(_ context.Context, on bool) error { r.quiet = append(r.quiet, on); return nil },
		wake:      make(chan struct{}, 1),
		prefix:    func(string) string { return "24" },
		now:       func() time.Time { return clock },
	}
	r.l.spec = ipvtapSpec // as the bring-up that precedes every renewal sets it
	return r
}

func (r *leaseRig) logf(f string, a ...any) { r.lines = append(r.lines, fmt.Sprintf(f, a...)) }

func (r *leaseRig) acquire(fresh bool) string {
	return r.l.acquire(context.Background(), ipvtapSpec, fresh, r.logf)
}

func (r *leaseRig) record(t *testing.T) leaseRecord {
	t.Helper()
	var rec leaseRecord
	b, err := os.ReadFile(r.l.path)
	if os.IsNotExist(err) {
		return rec
	}
	if err != nil || json.Unmarshal(b, &rec) != nil {
		t.Fatalf("the record does not read back: %v %q", err, b)
	}
	return rec
}

func (r *leaseRig) holding(addr string) {
	r.l.store(leaseRecord{Addr: addr, Renew: clock, Until: clock.Add(time.Hour)}, r.logf)
}

// Off a single-anchor substrate, or with an address the household named, the host asks nothing:
// the guest leases its own, or claims the named one.
func TestLeaseOnlyOnSingleAnchorSubstrates(t *testing.T) {
	r := newLeaseRig(t)
	for name, s := range map[string]nic.Spec{
		"macvtap":      {Parent: "eno1"},
		"linux bridge": {Parent: "br0", Bridge: !hostLeasesBridge && true},
	} {
		if got := r.l.acquire(context.Background(), s, true, r.logf); got != "" || len(r.wire.sent) != 0 {
			t.Errorf("%s: pushed %q after sending %v", name, got, r.wire.sent)
		}
	}
	r.named = "192.168.7.9/24"
	if got := r.acquire(true); got != "" || len(r.wire.sent) != 0 {
		t.Errorf("a named address: pushed %q after sending %v", got, r.wire.sent)
	}
	var nilLeaser *vipLeaser
	if nilLeaser.acquire(context.Background(), ipvtapSpec, true, r.logf) != "" || nilLeaser.refused() {
		t.Error("a nil leaser answered")
	}
}

// The first lease: DISCOVER, the offer judged, REQUEST, and the record written before the address
// is pushed, with its renewal at T1.
func TestLeaseFirstBringUpDiscovers(t *testing.T) {
	r := newLeaseRig(t)
	r.wire.offer, r.wire.ack = lease("192.168.7.120/24"), lease("192.168.7.120/24")
	if got := r.acquire(true); got != "192.168.7.120/24" {
		t.Fatalf("pushed %q", got)
	}
	if !slices.Equal(r.wire.sent, []string{"DISCOVER", "REQUEST 192.168.7.120"}) {
		t.Errorf("sent %v", r.wire.sent)
	}
	if rec := r.record(t); rec.Addr != "192.168.7.120/24" || !rec.Renew.Equal(clock.Add(time.Hour)) || !rec.Until.Equal(clock.Add(2*time.Hour)) {
		t.Errorf("record %+v", rec)
	}
	if r.l.refused() {
		t.Error("refused after a lease")
	}
}

// THE REFUSAL: the router offers the host's own address (it keyed on the L2 source), or nothing
// answers. Never requested, said once in the line the installer ends on, with the remedy.
func TestLeaseRefusesWhatIsNotTheFlocks(t *testing.T) {
	for name, tc := range map[string]struct {
		offer nic.DHCPLease
		err   error
		says  string
	}{
		"the host's own address": {offer: lease("192.168.7.50/24"), says: "it offered 192.168.7.50, the address of the machine Briard runs on"},
		"silence":                {err: nic.ErrNoAnswer, says: "nothing answered when Briard asked for an address"},
	} {
		t.Run(name, func(t *testing.T) {
			r := newLeaseRig(t)
			r.wire.offer, r.wire.offerErr = tc.offer, tc.err
			if got := r.acquire(true); got != "" {
				t.Fatalf("pushed %q", got)
			}
			if !slices.Equal(r.wire.sent, []string{"DISCOVER"}) {
				t.Errorf("sent %v: the offer must never be requested", r.wire.sent)
			}
			said := strings.Join(r.lines, "\n")
			for _, want := range []string{"network: refused: your router leases Briard nothing of its own: " + tc.says, "briard config set vip <address>/24"} {
				if !strings.Contains(said, want) {
					t.Errorf("does not say %q:\n%s", want, said)
				}
			}
			detail, fix := r.l.refusal()
			cs := judgeDoctor(doctorFacts{Refused: detail, RefusedFix: fix, Cluster: servingLone().Cluster})
			if c := checkNamed(t, cs, "address"); !strings.Contains(c.Fix, "config set vip") {
				t.Errorf("the doctor's address check = %+v, want the remedy", c)
			}
			// Asked again later, and still refused: said once.
			n := len(r.lines)
			r.l.renew(context.Background(), r.logf)
			if len(r.lines) != n || !r.l.refused() {
				t.Errorf("a second refusal said %q (refused %v)", r.lines[n:], r.l.refused())
			}
			// Naming an address ends it.
			r.named = "192.168.7.9/24"
			if r.l.refused() {
				t.Error("still refused with an address named")
			}
		})
	}
}

// A RESTART KEEPS THE ADDRESS by INIT-REBOOT, never a DISCOVER. NAK: the fresh guest holds
// nothing, so a DISCOVER follows. Silence: the address is kept -- a router slower than this
// machine to come back from a power cut -- and nothing new is asked for.
func TestLeaseBringUpKeepsTheRecordedAddress(t *testing.T) {
	for name, tc := range map[string]struct {
		reboot  nic.DHCPLease
		err     error
		push    string
		sent    []string
		renewAt time.Time
	}{
		"ACK": {reboot: lease("192.168.7.120/24"), push: "192.168.7.120/24",
			sent: []string{"INIT-REBOOT 192.168.7.120"}, renewAt: clock.Add(time.Hour)},
		"NAK": {err: nic.ErrNAK, push: "192.168.7.121/24",
			sent: []string{"INIT-REBOOT 192.168.7.120", "DISCOVER", "REQUEST 192.168.7.121"}, renewAt: clock.Add(time.Hour)},
		"an ACK for another address": {reboot: lease("192.168.7.99/24"), push: "192.168.7.121/24",
			sent: []string{"INIT-REBOOT 192.168.7.120", "DISCOVER", "REQUEST 192.168.7.121"}, renewAt: clock.Add(time.Hour)},
		"silence": {err: nic.ErrNoAnswer, push: "192.168.7.120/24",
			sent: []string{"INIT-REBOOT 192.168.7.120"}, renewAt: clock.Add(leaseRetrySilent)},
	} {
		t.Run(name, func(t *testing.T) {
			r := newLeaseRig(t)
			r.holding("192.168.7.120/24")
			r.wire.reboot, r.wire.rebootErr = tc.reboot, tc.err
			r.wire.offer, r.wire.ack = lease("192.168.7.121/24"), lease("192.168.7.121/24")
			if got := r.acquire(true); got != tc.push {
				t.Errorf("pushed %q, want %q", got, tc.push)
			}
			if !slices.Equal(r.wire.sent, tc.sent) {
				t.Errorf("sent %v, want %v", r.wire.sent, tc.sent)
			}
			if due := r.l.due(); !due.Equal(tc.renewAt) {
				t.Errorf("next ask at %v, want %v", due, tc.renewAt)
			}
		})
	}
}

// An ADOPTED guest is holding the recorded address: it is pushed as it stands, nothing is sent
// at bring-up, and the renewal asks at once.
func TestLeaseAdoptedGuestKeepsItsAddress(t *testing.T) {
	r := newLeaseRig(t)
	r.holding("192.168.7.120/24")
	if got := r.acquire(false); got != "192.168.7.120/24" || len(r.wire.sent) != 0 {
		t.Errorf("pushed %q after sending %v", got, r.wire.sent)
	}
	if !r.l.due().Equal(clock) {
		t.Errorf("the renewal is not due at once: %v", r.l.due())
	}
}

// THE RENEWAL, and rule 2: a lost lease relaunches the guest and sends no DISCOVER -- the guest
// still holds the address until it is relaunched. A silent router leaves the address in place.
func TestLeaseRenewal(t *testing.T) {
	r := newLeaseRig(t)
	r.holding("192.168.7.120/24")
	r.wire.reboot = lease("192.168.7.120/24")
	r.l.renew(context.Background(), r.logf)
	if rec := r.record(t); !rec.Renew.Equal(clock.Add(time.Hour)) || r.l.relaunch {
		t.Errorf("an ACKed renewal: record %+v, relaunch %v", rec, r.l.relaunch)
	}

	r.wire.rebootErr, r.wire.sent = nic.ErrNoAnswer, nil
	r.l.renew(context.Background(), r.logf)
	if rec := r.record(t); rec.Addr != "192.168.7.120/24" || r.l.relaunch || !r.l.due().Equal(clock.Add(leaseRetrySilent)) {
		t.Errorf("a silent renewal: record %+v, relaunch %v, due %v", rec, r.l.relaunch, r.l.due())
	}

	r.wire.rebootErr, r.wire.sent = nic.ErrNAK, nil
	r.l.renew(context.Background(), r.logf)
	if !slices.Equal(r.wire.sent, []string{"INIT-REBOOT 192.168.7.120"}) {
		t.Errorf("a NAKed renewal sent %v: no DISCOVER while the guest holds the address", r.wire.sent)
	}
	if rec := r.record(t); rec.Addr != "" || !r.l.relaunch {
		t.Errorf("a NAKed renewal: record %+v, relaunch %v", rec, r.l.relaunch)
	}
}

// A REFUSED node asks again, and a first lease relaunches the guest to push it.
func TestLeaseRefusalEndsWithALease(t *testing.T) {
	r := newLeaseRig(t)
	r.wire.offerErr = nic.ErrNoAnswer
	r.acquire(true)
	r.wire.offerErr, r.wire.offer, r.wire.ack = nil, lease("192.168.7.120/24"), lease("192.168.7.120/24")
	r.l.renew(context.Background(), r.logf)
	if r.l.refused() || !r.l.relaunch || r.record(t).Addr != "192.168.7.120/24" {
		t.Errorf("refused %v, relaunch %v, record %+v", r.l.refused(), r.l.relaunch, r.record(t))
	}
}

// The loop's half: the refusal's alert opens and resolves on its edges, and a relaunch is asked of
// the guest once.
func TestLeaseTick(t *testing.T) {
	r := newLeaseRig(t)
	n := &fakeNotifier{}
	rb := &fakeRebooter{}
	r.wire.offer = lease("192.168.7.50/24")
	r.acquire(true)
	r.l.tick(context.Background(), nil, rb, n, r.logf)
	r.l.tick(context.Background(), nil, rb, n, r.logf)
	if len(n.alerts) != 1 || n.alerts[0].Kind != notify.Open || n.alerts[0].Key != "address" {
		t.Fatalf("alerts after the refusal: %+v", n.alerts)
	}
	r.wire.offer, r.wire.ack = lease("192.168.7.120/24"), lease("192.168.7.120/24")
	r.l.renew(context.Background(), r.logf)
	r.l.tick(context.Background(), nil, rb, n, r.logf)
	r.l.tick(context.Background(), nil, rb, n, r.logf)
	if len(n.alerts) != 2 || n.alerts[1].Kind != notify.Resolved || rb.reboots != 1 {
		t.Errorf("after the lease: alerts %+v, reboots %d", n.alerts, rb.reboots)
	}
}

// The record outlives the agent: a new leaser reads it back.
func TestLeaseRecordIsReadAtStart(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{AssignmentCache: filepath.Join(dir, "assignment.json"), FlockID: "flock-a"}
	l := cfg.newVIPLeaser()
	l.store(leaseRecord{Addr: "192.168.7.120/24", Renew: clock}, t.Logf)
	if got := cfg.newVIPLeaser().rec.Addr; got != "192.168.7.120/24" {
		t.Errorf("read back %q", got)
	}
	if h := l.ask(ipvtapSpec).(nic.DHCPClient); h.Dev != "briard0" || !strings.HasPrefix(h.Hostname, "briard-") || len(h.Hostname) != 13 {
		t.Errorf("the ipvtap client is %+v, want the service child and the flock's name", h)
	}
}

// expired is a record whose lease ran out an hour ago.
func (r *leaseRig) expired(addr string) {
	r.l.store(leaseRecord{Addr: addr, Renew: clock.Add(-2 * time.Hour), Until: clock.Add(-time.Hour)}, r.logf)
}

// AN EXPIRED ADDRESS IS NEVER PUSHED UN-ASKED. A fresh guest holds nothing, so the bring-up
// DISCOVERs with the address as its hint. The record survives a silence, so the hint is there on
// the next ask -- and a refused node asks by DISCOVER first, never by keeping an address its guest
// was not given. An adopted guest holds the address, so it is pushed as it stands and asked at once.
func TestLeaseExpiredRecord(t *testing.T) {
	t.Run("fresh: DISCOVER with the hint", func(t *testing.T) {
		r := newLeaseRig(t)
		r.expired("192.168.7.120/24")
		r.wire.offer, r.wire.ack = lease("192.168.7.120/24"), lease("192.168.7.120/24")
		if got := r.acquire(true); got != "192.168.7.120/24" {
			t.Fatalf("pushed %q", got)
		}
		if !slices.Equal(r.wire.sent, []string{"DISCOVER hint 192.168.7.120", "REQUEST 192.168.7.120"}) {
			t.Errorf("sent %v", r.wire.sent)
		}
	})
	t.Run("fresh, silence: refused, the hint kept, and asked by DISCOVER again", func(t *testing.T) {
		r := newLeaseRig(t)
		r.expired("192.168.7.120/24")
		r.wire.offerErr = nic.ErrNoAnswer
		if got := r.acquire(true); got != "" || !r.l.refused() {
			t.Fatalf("pushed %q, refused=%v: an expired address went out un-asked", got, r.l.refused())
		}
		if r.record(t).Addr != "192.168.7.120/24" {
			t.Error("the record went with the silence; the next ask has no hint")
		}
		r.wire.offerErr, r.wire.offer, r.wire.ack = nil, lease("192.168.7.120/24"), lease("192.168.7.120/24")
		r.l.renew(context.Background(), r.logf)
		if !slices.Equal(r.wire.sent, []string{"DISCOVER hint 192.168.7.120", "DISCOVER hint 192.168.7.120", "REQUEST 192.168.7.120"}) {
			t.Errorf("sent %v", r.wire.sent)
		}
		if !r.l.relaunch {
			t.Error("a refused guest was leased an address and not relaunched to take it")
		}
	})
	t.Run("adopted: pushed as it stands, asked at once", func(t *testing.T) {
		r := newLeaseRig(t)
		r.expired("192.168.7.120/24")
		if got := r.acquire(false); got != "192.168.7.120/24" || len(r.wire.sent) != 0 || !r.l.due().Equal(clock) {
			t.Errorf("pushed %q, sent %v, due %v", got, r.wire.sent, r.l.due())
		}
	})
}

// THE PROBE: expired, the INIT-REBOOT unanswered, and the router answering ARP. The VIP goes
// silent, a DISCOVER carries the hint, and the link always comes back. Its three outcomes, and the
// two gates that keep it from running through an outage or every minute.
func TestLeaseProbe(t *testing.T) {
	setup := func(t *testing.T) *leaseRig {
		r := newLeaseRig(t)
		r.expired("192.168.7.120/24")
		r.wire.rebootErr = nic.ErrNoAnswer
		return r
	}
	t.Run("our address offered: renewed in place", func(t *testing.T) {
		r := setup(t)
		r.wire.offer, r.wire.ack = lease("192.168.7.120/24"), lease("192.168.7.120/24")
		r.l.renew(context.Background(), r.logf)
		if !slices.Equal(r.wire.sent, []string{"INIT-REBOOT 192.168.7.120", "DISCOVER hint 192.168.7.120", "REQUEST 192.168.7.120"}) {
			t.Errorf("sent %v", r.wire.sent)
		}
		if rec := r.record(t); !rec.Until.Equal(clock.Add(2*time.Hour)) || r.l.relaunch {
			t.Errorf("record %+v relaunch %v: want renewed in place, no restart", rec, r.l.relaunch)
		}
		if !slices.Equal(r.quiet, []bool{true, false}) {
			t.Errorf("link levers %v, want silenced then given back", r.quiet)
		}
	})
	t.Run("another address offered: the NAK path", func(t *testing.T) {
		r := setup(t)
		r.wire.offer = lease("192.168.7.121/24")
		r.l.renew(context.Background(), r.logf)
		if r.record(t).Addr != "" || !r.l.relaunch || slices.Contains(r.wire.sent, "REQUEST 192.168.7.121") {
			t.Errorf("record %+v relaunch %v sent %v: want dropped, a relaunch, nothing requested", r.record(t), r.l.relaunch, r.wire.sent)
		}
		if !slices.Equal(r.quiet, []bool{true, false}) {
			t.Errorf("link levers %v", r.quiet)
		}
	})
	t.Run("silence: kept", func(t *testing.T) {
		r := setup(t)
		r.wire.offerErr = nic.ErrNoAnswer
		r.l.renew(context.Background(), r.logf)
		if r.record(t).Addr != "192.168.7.120/24" || r.l.relaunch || r.l.refused() {
			t.Errorf("record %+v relaunch %v refused %v: a silent router took the address", r.record(t), r.l.relaunch, r.l.refused())
		}
		if !slices.Equal(r.quiet, []bool{true, false}) {
			t.Errorf("link levers %v", r.quiet)
		}
		// THE SECOND GATE: the next renewal, a minute on, does not probe again.
		r.wire.sent, r.quiet = nil, nil
		r.l.renew(context.Background(), r.logf)
		if len(r.quiet) != 0 || slices.Contains(r.wire.sent, "DISCOVER hint 192.168.7.120") {
			t.Errorf("probed again at once: levers %v sent %v", r.quiet, r.wire.sent)
		}
	})
	t.Run("the router not answering ARP: no probe, an outage", func(t *testing.T) {
		r := setup(t)
		r.gw = false
		r.l.renew(context.Background(), r.logf)
		if len(r.quiet) != 0 || !slices.Equal(r.wire.sent, []string{"INIT-REBOOT 192.168.7.120"}) {
			t.Errorf("levers %v sent %v", r.quiet, r.wire.sent)
		}
	})
	t.Run("not expired: no probe", func(t *testing.T) {
		r := setup(t)
		r.holding("192.168.7.120/24")
		r.l.renew(context.Background(), r.logf)
		if len(r.quiet) != 0 || len(r.wire.sent) != 1 {
			t.Errorf("levers %v sent %v", r.quiet, r.wire.sent)
		}
	})
}

// THE LINK'S EDGES ASK NOW, on the substrate with no notification: the carrier coming back, or the
// host's own address on it changing. The first reading only records; a carrier going away and an
// address lost with it ask nothing.
func TestLeaseWatchAsksOnAnEdge(t *testing.T) {
	r := newLeaseRig(t)
	r.holding("192.168.7.120/24")
	asked := func() bool {
		select {
		case <-r.l.wake:
			return true
		default:
			return false
		}
	}
	for _, step := range []struct {
		name string
		up   bool
		host []string
		ask  bool
	}{
		{"the first reading", true, []string{"192.168.7.50"}, false},
		{"nothing changed", true, []string{"192.168.7.50"}, false},
		{"the carrier lost, the address with it", false, nil, false},
		{"the carrier back", true, nil, true},
		{"the host's own address arrives", true, []string{"192.168.9.50"}, true},
		{"the address unchanged", true, []string{"192.168.9.50"}, false},
		{"another address, carrier up throughout: a roam shorter than a tick", true, []string{"192.168.7.51"}, true},
	} {
		r.up, r.host = step.up, step.host
		r.l.watch()
		if got := asked(); got != step.ask {
			t.Errorf("%s: asked = %v, want %v", step.name, got, step.ask)
		}
	}
	// Off ipvtap the mirror reports the edge (Windows): watch reads nothing.
	r.l.spec = nic.Spec{Parent: "Briard bridge", Bridge: true}
	r.up = false
	r.l.watch()
	r.up = true
	r.l.watch()
	if asked() {
		t.Error("watch asked on a bridge, where the link mirror owns the edge")
	}
}
