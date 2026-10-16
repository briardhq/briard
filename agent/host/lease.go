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
)

// leaseRelaunch is the internal act that relaunches the guest for a new address (acts.go).
const leaseRelaunch = "vip-lease"

// dhcpAsker is nic.DHCPClient, as an interface so the policy is testable without a wire. Not a
// seam -- a test double inside one component.
type dhcpAsker interface {
	Discover(ctx context.Context) (nic.DHCPLease, error)
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
	prefix    func(dev string) string
	now       func() time.Time

	ex sync.Mutex // one exchange at a time: a bring-up's and the renewal's

	mu       sync.Mutex
	spec     nic.Spec // the substrate, as the last bring-up saw it
	rec      leaseRecord
	retry    time.Time // the next attempt after a silence or a refusal; zero = the record's T1
	why      string    // the refusal's cause, "" = not refused
	relaunch bool      // the guest must be relaunched: a lost lease, or a first one after a refusal
	alerted  bool      // the refusal's alert is open
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
		prefix: func(dev string) string {
			_, p, _ := strings.Cut(reportcard.HostCIDR(dev), "/")
			return p
		},
		now: time.Now,
	}
	if b, err := os.ReadFile(l.path); err == nil {
		_ = json.Unmarshal(b, &l.rec)
	}
	return l
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
	if rec.Addr != "" {
		switch addr, err := l.keep(ctx, rec, logf); {
		case err == nil:
			return addr
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

// discover asks for a new address: DISCOVER, judge the offer, REQUEST it. "" when refused.
func (l *vipLeaser) discover(ctx context.Context, logf func(string, ...any)) string {
	offer, err := l.wire().Discover(ctx)
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
// It never touches the guest; what the guest must do it leaves for the loop (tick).
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
		}
		l.renew(ctx, logf)
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
	case rec.Addr != "":
		if _, err := l.keep(ctx, rec, logf); errors.Is(err, nic.ErrNAK) {
			l.askRelaunch() // rule 2: the guest drops the address before anyone DISCOVERs
		} else if err != nil {
			// Kept past its expiry while nobody answers, as the guest's own client does on a wired
			// substrate: a router that is down is not a router that wants the address back.
			if now := l.now(); rec.Until.Before(now) && !rec.Until.Before(now.Add(-leaseRetrySilent)) {
				logf("vip lease: %s expired with no answer from the router; kept, and asked for again", rec.Addr)
			}
			l.setRetry(l.now().Add(leaseRetrySilent))
		}
	case refused:
		// The guest was given no address, so a DISCOVER is allowed.
		if l.discover(ctx, logf) != "" {
			l.askRelaunch()
		}
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
