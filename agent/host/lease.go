package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"briard.io/agent/nic"
	"briard.io/agent/platform"
	"briard.io/agent/reportcard"
	"briard.io/shared/atomicfile"
	"briard.io/shared/notify"
)

// THE HOST LEASES THE FLOCK ITS ADDRESS on a single-anchor substrate.
//
// On an ipvtap child of a wireless station, and behind the Windows bridge, the guest's frames
// leave with the host's L2 source, and a router that keys a lease on it -- or on a chaddr the
// bridge rewrote to it -- hands the guest the host's own address. So the host asks instead, under
// the flock's chaddr (nic/dhcp.go), and pushes the answer to the guest as its VIP_ADDR; the
// guest's own client is never started here (VIP_LEASE=host). The address is the household's
// router's choice, and when it leases us nothing distinct the node REFUSES and names the remedy:
// we never pick an address and defend it.
//
// THE TWO RULES, both because a reply to a ciaddr or a conflict check reaches the guest:
//  1. The lease is kept by INIT-REBOOT only: at every bring-up, and as the renewal at T1.
//  2. A DISCOVER is sent only while the guest holds no address of ours: from a bring-up of a
//     fresh guest, or while refused (the guest was given none). A lease that is lost (a NAK)
//     relaunches the guest, and its bring-up asks again.
//
// THE RECORD is node-local pet state, written before the guest is given the address, so a host
// restart can keep the address it pushed without asking a router that may be down: a failover
// and a power cut must not need the network, only an install may.

// leaseFile is the record, in the state dir.
const leaseFile = "vip-lease.json"

// Retry cadences: a renewal nobody answered, and a refused node asking again.
const (
	leaseRetrySilent  = time.Minute
	leaseRetryRefused = 2 * time.Minute
	leaseMinRenew     = 30 * time.Second
	// leaseProbeEvery spaces the probe (renew) after one met silence: each one holds
	// the VIP dark for a DISCOVER's length, and a router that ignored the last will likely ignore
	// the next.
	leaseProbeEvery = 10 * time.Minute
)

// leaseRelaunch is the internal act that relaunches the guest for a new address (acts.go).
const leaseRelaunch = "vip-lease"

// dhcpAsker is nic.DHCPClient, as an interface so the policy is testable without a wire. Not a
// seam -- a test double inside one component.
type dhcpAsker interface {
	Discover(ctx context.Context, hint netip.Addr) (nic.DHCPLease, error)
	Request(ctx context.Context, offer nic.DHCPLease) (nic.DHCPLease, error)
	Reboot(ctx context.Context, addr netip.Addr) (nic.DHCPLease, error)
}

// leaseRecord is the lease as kept on disk.
type leaseRecord struct {
	Addr  string    `json:"addr"`  // CIDR
	Renew time.Time `json:"renew"` // T1
	Until time.Time `json:"until"` // expiry: past it the address is kept, and asked for again
}

// vipLeaser is the flock's lease on this host. Machinery on Config like the copier, built for
// every node; it holds a lease only on a single-anchor substrate, which each bring-up hands it
// (acquire), so a re-parent between Wi-Fi and wire moves it on or off with the guest's restart.
// Every method is nil-safe.
type vipLeaser struct {
	path  string
	mac   net.HardwareAddr
	named func() bool // the household named an address: the lease is not ours to keep
	// ask, hostAddrs and prefix are the wire and the host's own addresses, as fields so the policy
	// is testable without either. Not a seam -- test doubles inside one component.
	ask       func(nic.Spec) dhcpAsker
	hostAddrs func(dev string) []string
	node      string // the host's own system-subnet address, never a LAN address (moved)
	prefix    func(dev string) string
	now       func() time.Time
	// gateway reports whether the router answers ARP on dev (the fingerprint's read), and quiet
	// silences the VIP (on) or gives it back (off) -- the probe's two levers, as fields for the
	// same reason.
	gateway func(ctx context.Context, dev string) bool
	quiet   func(ctx context.Context, on bool) error
	wake    chan struct{}         // askNow's nudge to run; nil in a test that drives renew itself
	carrier func(dev string) bool // nic.Carrier, for watch

	seen    bool   // watch has a reading
	linkWas bool   // ...the carrier it read
	hostWas string // ...and the host's own addresses on the link

	ex sync.Mutex // one exchange at a time: a bring-up's and the renewal's

	mu       sync.Mutex
	spec     nic.Spec // the substrate, as the last bring-up saw it
	rec      leaseRecord
	retry    time.Time // the next attempt after a silence or a refusal; zero = the record's T1
	why      string    // the refusal's cause, "" = not refused
	relaunch bool      // the guest must be relaunched: a lost lease, or a first one after a refusal
	alerted  bool      // the refusal's alert is open
	probed   time.Time // the last probe that met silence; the next waits leaseProbeEvery
}

// newVIPLeaser builds this node's leaser; nil without a state dir to keep the record in.
func (cfg Config) newVIPLeaser() *vipLeaser {
	mac, err := net.ParseMAC(cfg.serviceMAC())
	if err != nil || cfg.stateDir() == "" {
		return nil
	}
	vip := cfg.vip
	l := &vipLeaser{
		path:  filepath.Join(cfg.stateDir(), leaseFile),
		mac:   mac,
		named: func() bool { return vip != nil && *vip != "" },
		ask: func(s nic.Spec) dhcpAsker {
			dev := s.Parent // the Windows bridge: a socket bound to its address
			if s.Ipvtap {
				dev = s.ServiceTap // the guest's service child, in nic.HoldNS
			}
			// The flock's name in the router's client list, from the same MAC: what the guest's
			// own client sends on a wired substrate (guest-image, briard-vip-dhcpcd).
			return nic.DHCPClient{Dev: dev, Chaddr: mac, Hostname: "briard-" + strings.ReplaceAll(mac.String()[9:], ":", "")}
		},
		hostAddrs: nic.IPv4s,
		node:      cfg.hostNodeIP(),
		prefix: func(dev string) string {
			_, p, _ := strings.Cut(reportcard.HostCIDR(dev), "/")
			return p
		},
		now:     time.Now,
		gateway: func(ctx context.Context, dev string) bool { return nic.Read(ctx, dev).GatewayMAC != "" },
		quiet:   cfg.quietVIP,
		wake:    make(chan struct{}, 1),
		carrier: nic.Carrier,
	}
	if b, err := os.ReadFile(l.path); err == nil {
		_ = json.Unmarshal(b, &l.rec)
	}
	return l
}

// quietVIP silences the guest's VIP (on) or gives it back (off): QMP set_link on the NIC the VIP
// lives on -- the service NIC of an ipvtap station, the one NIC behind a Windows bridge. It is the
// probe's "drop" (renew): with the link down nothing answers for the address, so the router's
// conflict check finds it free.
//
// ⚠️ NEVER UP WHILE THE UPLINK IS DOWN, the link mirror's rule (linkmirror.go): giving it back on
// a bridge sets the link to its member's carrier, which is what the mirror holds it at, so the
// two agree whichever ran last.
func (cfg Config) quietVIP(ctx context.Context, on bool) error {
	g := platform.Adopt(cfg.guestSpec())
	set := g.SetSystemLink
	if cfg.net != nil && cfg.net.Ipvtap {
		set = g.SetServiceLink
	}
	if on {
		return set(ctx, false)
	}
	parent := ""
	if cfg.net != nil {
		parent = cfg.net.Parent
	}
	return set(ctx, linkBack(parent, nic.Uplink, nic.Up))
}

// linkBack is the link the probe gives back: up, except behind a bridge whose member is down or
// gone, where it is the member's carrier -- never up while the uplink is down.
func linkBack(parent string, uplink func(string) (string, bool), up func(string) bool) bool {
	m, swaps := uplink(parent)
	return !swaps || m != "" && up(m)
}

// singleAnchor reports whether the host leases the flock's address on s.
func singleAnchor(s nic.Spec) bool { return s.Ipvtap || s.Bridge && hostLeasesBridge }

// active reports whether the host holds the flock's lease now: on a single-anchor substrate, and
// not when the household named an address.
func (l *vipLeaser) active() bool {
	if l == nil || l.named() {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return singleAnchor(l.spec)
}

// acquire is a bring-up's question: the address to push on substrate s, or "" when the host leases
// nothing there or is refused. fresh is a guest that was just launched, holding nothing; an adopted
// one holds the recorded address, so it is pushed as it stands and the renewal asks the router at
// once.
func (l *vipLeaser) acquire(ctx context.Context, s nic.Spec, fresh bool, logf func(string, ...any)) string {
	if l == nil {
		return ""
	}
	l.mu.Lock()
	l.spec = s
	l.mu.Unlock()
	if !l.active() {
		return ""
	}
	l.ex.Lock()
	defer l.ex.Unlock()
	l.mu.Lock()
	rec := l.rec
	l.mu.Unlock()
	if rec.Addr != "" && !fresh {
		l.setRetry(l.now())
		return rec.Addr
	}
	// AN EXPIRED ADDRESS IS NEVER PUSHED UN-ASKED: a fresh guest holds nothing, so rule 2 allows
	// the DISCOVER, and the record's address rides it as the hint (discover). The laptop asleep for
	// a week comes back here, through the resume's relaunch.
	if rec.Addr != "" && rec.Until.Before(l.now()) {
		logf("vip lease: %s expired; asking for it again", rec.Addr)
		return l.discover(ctx, logf)
	}
	if rec.Addr != "" {
		switch addr, err := l.keep(ctx, rec, logf); {
		case err == nil:
			return addr
		case !errors.Is(err, nic.ErrNAK) && l.moved(rec, logf):
			// The machine came up on another network (a NIC change): the guest holds nothing yet,
			// so it DISCOVERs here, with no hint -- the old address means nothing on this LAN.
			l.store(leaseRecord{}, logf)
			return l.discover(ctx, logf)
		case !errors.Is(err, nic.ErrNAK):
			// Silence, at a bring-up: a router slower to come back from a power cut than this
			// machine. The address is kept, and the renewal asks again.
			logf("vip lease: %s kept unconfirmed: %v", rec.Addr, err)
			l.setRetry(l.now().Add(leaseRetrySilent))
			return rec.Addr
		}
		// The guest was just launched and holds nothing, so rule 2 allows the DISCOVER.
	}
	return l.discover(ctx, logf)
}

// keep is one INIT-REBOOT for the recorded address: on an ACK for it the record moves on, on a NAK
// (or an ACK for another address) the record is dropped and ErrNAK returned.
func (l *vipLeaser) keep(ctx context.Context, rec leaseRecord, logf func(string, ...any)) (string, error) {
	want, err := netip.ParsePrefix(rec.Addr)
	if err != nil {
		l.store(leaseRecord{}, logf)
		return "", nic.ErrNAK
	}
	got, err := l.wire().Reboot(ctx, want.Addr())
	if err == nil && got.Addr.Addr() != want.Addr() {
		err = nic.ErrNAK
	}
	if errors.Is(err, nic.ErrNAK) {
		logf("vip lease: the router no longer leases %s to this flock", rec.Addr)
		l.store(leaseRecord{}, logf)
		return "", err
	}
	if err != nil {
		return "", err
	}
	l.store(l.record(got), logf)
	return got.Addr.String(), nil
}

// discover asks for a new address: DISCOVER, judge the offer, REQUEST it. "" when refused. The
// record's address, when there is one (an expired lease), is the DISCOVER's hint, and the record
// is kept until a new lease replaces it, so the hint outlives a refusal and a relaunch.
func (l *vipLeaser) discover(ctx context.Context, logf func(string, ...any)) string {
	offer, err := l.wire().Discover(ctx, l.hint())
	if err != nil {
		if ctx.Err() == nil {
			l.refuse("nothing answered when Briard asked for an address", logf)
		}
		return ""
	}
	// The address of the machine we run on: the router keyed on, or was handed, the L2 source.
	// Never requested -- asking for it under the flock's chaddr could re-file the host's own lease.
	if slices.Contains(l.hostAddrs(l.parent()), offer.Addr.Addr().String()) {
		l.refuse(fmt.Sprintf("it offered %s, the address of the machine Briard runs on", offer.Addr.Addr()), logf)
		return ""
	}
	ack, err := l.wire().Request(ctx, offer)
	if err != nil {
		if ctx.Err() == nil {
			l.refuse(fmt.Sprintf("it offered %s and then did not lease it (%v)", offer.Addr.Addr(), err), logf)
		}
		return ""
	}
	l.store(l.record(ack), logf)
	logf("vip lease: %s from %s for %s, under the flock's MAC", ack.Addr, ack.Server, ack.Time)
	l.mu.Lock()
	if l.why != "" {
		logf("network: the refusal is over: the router leased this flock %s", ack.Addr)
	}
	l.why = ""
	l.mu.Unlock()
	return ack.Addr.String()
}

// hint is the record's address, for a DISCOVER's option 50; invalid when there is none.
func (l *vipLeaser) hint() netip.Addr {
	l.mu.Lock()
	defer l.mu.Unlock()
	p, err := netip.ParsePrefix(l.rec.Addr)
	if err != nil {
		return netip.Addr{}
	}
	return p.Addr()
}

// record turns a lease into what is kept: T1 no sooner than leaseMinRenew from now.
func (l *vipLeaser) record(got nic.DHCPLease) leaseRecord {
	now := l.now()
	return leaseRecord{Addr: got.Addr.String(), Renew: now.Add(max(got.T1, leaseMinRenew)), Until: now.Add(got.Time)}
}

// store keeps rec in memory and on disk, and clears a pending retry. tmp + fsync + rename: the
// record is the address's only copy on this side of the guest.
func (l *vipLeaser) store(rec leaseRecord, logf func(string, ...any)) {
	l.mu.Lock()
	l.rec, l.retry = rec, time.Time{}
	l.mu.Unlock()
	var err error
	if rec.Addr == "" {
		err = os.Remove(l.path)
		if os.IsNotExist(err) {
			err = nil
		}
	} else {
		b, _ := json.Marshal(rec)
		err = atomicfile.Write(l.path, b, 0o600, 0o700)
	}
	if err != nil {
		logf("vip lease: could not record the lease: %v", err)
	}
}

func (l *vipLeaser) setRetry(t time.Time) {
	l.mu.Lock()
	l.retry = t
	l.mu.Unlock()
}

// refuse records why, says it once per episode, and asks again later. THE LINE install.sh's
// closing wait ends on, and prints (install.go's refusedLine): keep the prefix in step with it.
func (l *vipLeaser) refuse(cause string, logf func(string, ...any)) {
	l.mu.Lock()
	first := l.why == ""
	l.why = cause
	l.retry = l.now().Add(leaseRetryRefused)
	l.mu.Unlock()
	if first {
		detail, fix := l.refusal()
		logf("network: refused: %s; %s", detail, fix)
	}
}

// refused reports whether the node is refused now.
func (l *vipLeaser) refused() bool {
	if !l.active() {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.why != ""
}

// refusal is the refusal's detail and remedy, in the household's words. Empty when not refused.
func (l *vipLeaser) refusal() (detail, fix string) {
	if !l.refused() {
		return "", ""
	}
	l.mu.Lock()
	why := l.why
	l.mu.Unlock()
	prefix := l.prefix(l.parent())
	if prefix == "" {
		prefix = "24"
	}
	return "your router leases Briard nothing of its own: " + why,
		"pick a free address outside your router's DHCP range and " + fmt.Sprintf(vipFixHint, prefix)
}

// run keeps the lease: an INIT-REBOOT at T1, and -- while refused -- a DISCOVER every few minutes.
// askNow wakes it early. It never touches the guest's address; what the guest must do it leaves for
// the loop (tick).
func (l *vipLeaser) run(ctx context.Context, logf func(string, ...any)) {
	if l == nil {
		return
	}
	for {
		t := time.NewTimer(max(l.due().Sub(l.now()), time.Second))
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		case <-l.wake:
			t.Stop()
		}
		l.renew(ctx, logf)
	}
}

// askNow moves the next ask to now: the link the lease rides got its carrier back, or the host's
// own address on it changed -- a roam, a cable moved to another switch, a router back from a
// reboot. A client re-asks on such an edge; INIT-REBOOT is what this client may send while the
// guest holds the address (rule 1). Called from the loop, so it never blocks: a wake already
// pending is the same ask.
func (l *vipLeaser) askNow() {
	if !l.active() {
		return
	}
	l.setRetry(l.now())
	select {
	case l.wake <- struct{}{}:
	default:
	}
}

// watch reads the link the lease rides, once a tick. The host's own LAN address on it changing
// asks now, on every substrate: it is the level that catches a roam shorter than a tick, and the
// host's own lease landing on a new network -- the reading moved needs. The carrier coming back
// asks too, where nothing else reports it (a Linux ipvtap station; on a bridge the link mirror
// does, mirrorLink). Link-local and the node's own system-subnet address are no LAN address, so
// a lease pending or the agent's own convergence never asks. The first reading only records.
func (l *vipLeaser) watch() {
	if !l.active() {
		return
	}
	l.mu.Lock()
	s := l.spec
	l.mu.Unlock()
	up := !s.Ipvtap || l.carrier(s.Parent)
	var lan []string
	for _, a := range l.hostAddrs(s.Parent) {
		if p, err := netip.ParseAddr(a); err == nil && !p.IsLinkLocalUnicast() && a != l.node {
			lan = append(lan, a)
		}
	}
	host := strings.Join(lan, ",")
	l.mu.Lock()
	seen, was, wasHost := l.seen, l.linkWas, l.hostWas
	l.seen, l.linkWas, l.hostWas = true, up, host
	l.mu.Unlock()
	if seen && up && (!was || host != "" && host != wasHost) {
		l.askNow()
	}
}

// renew is one of run's asks.
func (l *vipLeaser) renew(ctx context.Context, logf func(string, ...any)) {
	if !l.active() {
		return
	}
	l.ex.Lock()
	defer l.ex.Unlock()
	l.mu.Lock()
	rec, refused := l.rec, l.why != ""
	l.mu.Unlock()
	switch {
	case refused:
		// The guest was given no address, so a DISCOVER is allowed. First, because a refused node
		// may still hold an expired record -- the hint, never an address the guest has.
		if l.discover(ctx, logf) != "" {
			l.askRelaunch()
		}
	case rec.Addr != "":
		_, err := l.keep(ctx, rec, logf)
		switch {
		case errors.Is(err, nic.ErrNAK):
			l.askRelaunch() // rule 2: the guest drops the address before anyone DISCOVERs
		case err == nil:
		case l.moved(rec, logf):
			// The link moved to another network and its router is silent about ours: the address
			// cannot be reached here, so it is lost -- the NAK path.
			l.store(leaseRecord{}, logf)
			l.askRelaunch()
		case rec.Until.Before(l.now()) && l.now().Sub(l.probed) >= leaseProbeEvery && l.gateway(ctx, l.parent()):
			l.probe(ctx, rec, logf)
		default:
			// Kept past its expiry while nobody answers, as the guest's own client does on a wired
			// substrate: a router that is down is not a router that wants the address back.
			if now := l.now(); rec.Until.Before(now) && !rec.Until.Before(now.Add(-leaseRetrySilent)) {
				logf("vip lease: %s expired with no answer from the router; kept, and asked for again", rec.Addr)
			}
			l.setRetry(l.now().Add(leaseRetrySilent))
		}
	}
}

// moved reports whether the host has moved off the VIP's network: after an INIT-REBOOT nobody
// answered, the host's own address on the link -- read now, since the host's own lease moves with
// the link -- is outside the VIP's subnet. A router that is silent about an address it does not
// know is not a router that wants it kept, and nobody on this LAN can reach it.
//
// ⚠️ NO LAN ADDRESS IS NO EVIDENCE: a host whose own lease has not landed yet holds only
// link-local (or nothing), and that reads false, so the address is kept and asked for again. Only
// an address positively elsewhere moves it. The node's own system-subnet address is the agent's,
// never the LAN's, and is not counted.
func (l *vipLeaser) moved(rec leaseRecord, logf func(string, ...any)) bool {
	vip, err := netip.ParsePrefix(rec.Addr)
	if err != nil {
		return false
	}
	var lan []string
	for _, s := range l.hostAddrs(l.parent()) {
		a, err := netip.ParseAddr(s)
		if err != nil || a.IsLinkLocalUnicast() || s == l.node {
			continue
		}
		if vip.Masked().Contains(a) {
			return false
		}
		lan = append(lan, s)
	}
	if len(lan) == 0 {
		return false
	}
	logf("vip lease: this host is on another network now (%s), and its router does not answer for %s",
		strings.Join(lan, ", "), rec.Addr)
	return true
}

// probe is the lease's answer to a router that is up -- it answers ARP -- and silent about an
// expired lease: the RFC's own sequence from the host's hand. A router that keeps its leases in
// RAM forgets them at a reboot, and a non-authoritative one then answers every renewal with
// silence, where a client drops the address and DISCOVERs (RFC 2131 4.4.5). The VIP's link goes
// down (the drop: nothing answers for the address, so the router's conflict check finds it free),
// a DISCOVER carries the address as its hint, and the link comes back. An offer of our address is
// requested and renewed in place, a few seconds dark; an offer of another is the NAK path, since
// the guest takes an address only at bring-up; silence keeps the address, as a router that is down
// leaves it -- spec-shaped while the router is up, local-first when it is not.
func (l *vipLeaser) probe(ctx context.Context, rec leaseRecord, logf func(string, ...any)) {
	if err := l.quiet(ctx, true); err != nil {
		logf("vip lease: %s expired with no answer; could not silence it to ask again: %v", rec.Addr, err)
		l.setRetry(l.now().Add(leaseRetrySilent))
		return
	}
	logf("vip lease: %s expired and the router does not answer for it; asking it afresh, the address silent meanwhile", rec.Addr)
	offer, err := l.wire().Discover(ctx, l.hint())
	switch want := l.hint(); {
	case err == nil && offer.Addr.Addr() == want:
		if ack, err := l.wire().Request(ctx, offer); err == nil && ack.Addr.Addr() == want {
			l.store(l.record(ack), logf)
			logf("vip lease: %s renewed in place, from %s for %s", ack.Addr, ack.Server, ack.Time)
		} else {
			l.setRetry(l.now().Add(leaseRetrySilent))
		}
	case err == nil:
		logf("vip lease: the router offers %s now, not %s; restarting the guest to take an address", offer.Addr.Addr(), want)
		l.store(leaseRecord{}, logf)
		l.askRelaunch()
	default:
		logf("vip lease: no offer for %s either; kept, and asked for again", rec.Addr)
		l.probed = l.now() // only a silent probe waits out leaseProbeEvery
		l.setRetry(l.now().Add(leaseRetrySilent))
	}
	if err := l.quiet(ctx, false); err != nil {
		logf("vip lease: could not give %s its link back: %v", rec.Addr, err)
	}
}

// due is when run next asks.
func (l *vipLeaser) due() time.Time {
	l.mu.Lock()
	defer l.mu.Unlock()
	switch {
	case !l.retry.IsZero():
		return l.retry
	case l.rec.Addr != "":
		return l.rec.Renew
	}
	return l.now().Add(leaseRetryRefused)
}

func (l *vipLeaser) askRelaunch() {
	l.mu.Lock()
	l.relaunch = true
	l.mu.Unlock()
}

// tick is the loop's half: the alert's edges, and the relaunch a new address needs, started as an
// internal act. A relaunch the act lane refuses (busy) is asked for again on the next tick.
func (l *vipLeaser) tick(ctx context.Context, acts *actLane, up any, n notify.Notifier, logf func(string, ...any)) {
	if l == nil {
		return
	}
	l.watch()
	refused, active := l.refused(), l.active()
	l.mu.Lock()
	open, relaunch := l.alerted, l.relaunch && active
	l.alerted = refused
	l.mu.Unlock()
	if refused && !open {
		detail, fix := l.refusal()
		fireAlert(ctx, n, logf, notify.Alert{
			Key:      "address",
			Kind:     notify.Open,
			Severity: notify.Critical,
			Title:    "Briard: this node cannot take an address on your network",
			Body:     detail + ". Nothing in your home can reach Briard until you " + fix + ".",
		})
	} else if !refused && open {
		fireAlert(ctx, n, logf, notify.Alert{
			Key:   "address",
			Kind:  notify.Resolved,
			Title: "Briard: this node has an address on your network",
			Body:  "Briard has an address on your network again.",
		})
	}
	rb, ok := up.(guestRebooter)
	if !relaunch || !ok {
		return
	}
	if acts.runInternal(leaseRelaunch, func() {
		logf("vip lease: restarting the guest to take a new address")
		if err := rb.RebootGuest(ctx); err != nil {
			logf("vip lease: guest restart failed: %v", err)
		}
	}) {
		l.mu.Lock()
		l.relaunch = false
		l.mu.Unlock()
	}
}

// wire is the DHCP client for the substrate the last bring-up saw.
func (l *vipLeaser) wire() dhcpAsker {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.ask(l.spec)
}

// parent is the host's device on that substrate: where its own addresses are.
func (l *vipLeaser) parent() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.spec.Parent
}

// clear drops a refusal: the household changed the address (configset.go).
func (l *vipLeaser) clear() {
	if l != nil {
		l.mu.Lock()
		l.why = ""
		l.mu.Unlock()
	}
}
