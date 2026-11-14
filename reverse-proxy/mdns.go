package main

// The household's mDNS responder, which lives in the reverse proxy for the reason the reverse proxy
// exists: a `.local` name means "this app is reachable here", and the reverse proxy is what "here"
// is. It answers off the SAME routing table the router reads, so a name that is published but
// not routed is not expressible; a separate publisher, a second reader of the same file, could
// only assert it.
//
// ⚠️ IT ANSWERS QUERIES AND SAYS NOTHING UNSOLICITED. No probing, no announcements, no goodbyes.
// That is the robustness argument for not using avahi, not a shortcut: avahi's entry group is a
// state machine we cannot see into, and it wedges -- silently, permanently -- if the address
// moves or an interface appears inside its probe window. A responder with no probe has no such
// state. "What address does this name have" is a field read when the answer is written.
//
// ⚠️ THE COST, ACCEPTED: a browsing client learns of a NEW app on its next periodic query rather
// than instantly, and an uninstalled one leaves a stale PTR until its TTL. Failover needs none of
// it -- both nodes publish byte-identical records, because every name points at a VIP whose
// address does not change when it moves.
//
// ⚠️ NO PROBING ALSO MEANS NO CONFLICT DETECTION, and the bare names rely on that being
// acceptable: `briard.local` and `briard-<app>.local` are answered by every flock beside its
// flock-scoped names, so two flocks in one house both answer them and a client gets whichever
// replies first (routes.BareHostName). The flock-scoped names stay unambiguous, and nothing the
// node does depends on the bare ones.
// ⚠️ If an announcer is ever added, `nixosTest/install-macvtap.nix`'s cold-cache assertion needs
// an announcement-tail wait in front of it (that test's silence check fails first): an
// announcement is the same packet as a response, so a client can be answered by a multicast it
// never asked for, and the assertion silently becomes a cache read -- one that hides inbound mDNS
// being dropped on the macvtap while egress works.

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"briard.io/shared/routes"
	pmdns "github.com/pion/mdns/v2"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
)

// mdnsService is one DNS-SD record: what a browsing appliance finds, and where it is sent.
// Tasmota- and ESPHome-class firmware browses `_mqtt._tcp` and never types a name.
type mdnsService struct {
	instance string // the flock-scoped label a household picks from, composed by routes.InstanceName
	typ      string // `_mqtt._tcp` and its kin
	host     string // the SRV target: the app's OWN name, never the guest's node-scoped hostname
	port     uint16
}

// mdnsWorld is everything that decides what this node answers on the household LAN. It is a value
// so that "has anything changed" is an equality test rather than a set of flags to keep in step.
type mdnsWorld struct {
	addr  string // the VIP, as text; every name resolves to it
	names []string
	svcs  []mdnsService
}

// mdnsWorldFor is the PURE half, split from the responder for the reason agent/subnet splits its
// reader from its parser: the rule for what belongs on the wire is testable against real tables
// rather than trusted. Nothing here composes a name — they are read from the table the agent
// materialised (routes.HostNames) or are the flock's own two (routes.FlockHostName,
// routes.BareFlockHostName), so the naming rule stays in one package.
//
// AN EMPTY WORLD IS A REAL STATE, not a failure: a node whose flock has no minted name, or that
// has converged to nothing, publishes nothing. The reverse proxy serves HTTP either way.
func mdnsWorldFor(addr, flock string, t routes.Table) mdnsWorld {
	w := mdnsWorld{addr: addr}
	if addr == "" {
		// No address is nothing to point a name at, and a name resolving to nowhere is worse
		// than a name that does not resolve: the household gets a connection that hangs instead
		// of one that fails.
		return mdnsWorld{}
	}
	seen := map[string]bool{}
	add := func(n string) {
		// Only `.local` is ours to claim on the LAN: the table also carries each app's
		// `<app>.<flock>.briard.casa` name, which public DNS answers for and
		// mDNS must not -- a multicast responder for a unicast name is a name that lies.
		if n == "" || seen[n] || !strings.HasSuffix(n, ".local") {
			return
		}
		seen[n] = true
		w.names = append(w.names, n)
	}
	if own := routes.FlockHostName(flock); own != "" {
		// The bare alias rides with the flock's own name and never without it: a node that
		// cannot name its flock answers for no flock, not for "the" one.
		add(own)
		add(routes.BareFlockHostName)
	}
	for _, s := range t.Apps {
		for _, h := range s.Hosts {
			add(h)
		}
	}
	for _, s := range t.Apps {
		if len(s.Hosts) == 0 {
			// An announcement needs an SRV target and the only candidate is the app's own
			// name. routes.Validate already refuses this combination; skipping rather than
			// composing a fallback keeps that the single rule.
			continue
		}
		for _, a := range s.Announce {
			if a.Name == "" || a.Type == "" || a.Port <= 0 || a.Port > 65535 {
				continue
			}
			w.svcs = append(w.svcs, mdnsService{
				instance: a.Name,
				typ:      a.Type,
				host:     s.Hosts[0],
				port:     uint16(a.Port),
			})
		}
	}
	sort.Strings(w.names)
	sort.Slice(w.svcs, func(i, j int) bool {
		if w.svcs[i].typ != w.svcs[j].typ {
			return w.svcs[i].typ < w.svcs[j].typ
		}
		return w.svcs[i].instance < w.svcs[j].instance
	})
	return w
}

// empty reports whether there is nothing to publish, which is the shipped state of a node that has
// no flock name yet.
func (w mdnsWorld) empty() bool { return w.addr == "" || (len(w.names) == 0 && len(w.svcs) == 0) }

// same is what decides whether the responder is rebuilt at all.
func (w mdnsWorld) same(o mdnsWorld) bool {
	if w.addr != o.addr || len(w.names) != len(o.names) || len(w.svcs) != len(o.svcs) {
		return false
	}
	for i := range w.names {
		if w.names[i] != o.names[i] {
			return false
		}
	}
	for i := range w.svcs {
		if w.svcs[i] != o.svcs[i] {
			return false
		}
	}
	return true
}

func (w mdnsWorld) describe() string {
	if w.empty() {
		return "nothing"
	}
	return fmt.Sprintf("%d name(s) and %d service record(s) at %s", len(w.names), len(w.svcs), w.addr)
}

// mdnsResponder holds whatever is currently answering, and nothing else. Rebuilding is the only
// way to change the NAME set -- pion fixes local names at construction -- and rebuilding is cheap
// precisely because nothing probes: the new server answers its first query.
type mdnsResponder struct {
	ifaces []net.Interface
	// ask is the one question asked of the LAN, at bring-up: who else answers the bare name.
	// A field so the test can answer it without a second responder on a real interface.
	ask func(ctx context.Context, ifaces []net.Interface, name string) string

	mu    sync.Mutex
	conn  *pmdns.Conn
	cur   mdnsWorld
	other string // another briard's address answering routes.BareFlockHostName; "" when none heard
}

func newMDNSResponder(ifaces []net.Interface) *mdnsResponder {
	return &mdnsResponder{ifaces: ifaces, ask: askLAN}
}

// set makes the wire match w, and is a no-op when it already does.
//
// ⚠️ THE NEW SERVER IS OPENED BEFORE THE OLD ONE IS CLOSED. Two sockets share :5353 (Go sets
// SO_REUSEADDR on a socket listening on a multicast group, which is also how Home Assistant's own
// python-zeroconf coexists there), so this leaves no window in which the name resolves nowhere --
// and, more importantly, no window in which a rebuild can fail because the port it just released
// is still held. For the few milliseconds both exist they answer identically for every unchanged
// name, which is the shadowing case mDNS defines as no conflict at all.
//
// THE ONE QUESTION IS ASKED AT BRING-UP, from silence to publishing, and nowhere else. It is the
// only moment the answer is clean: this node is not yet answering, so whatever replies to the
// bare name is somebody else. A rebuild keeps the old conn answering until the new one is up, so
// asking then would hear ourselves. Each bring-up asks again -- a second flock installed later is
// heard at this node's next bring-up, and until then the bare name is simply ambiguous, which it
// is allowed to be (routes.BareHostName).
func (r *mdnsResponder) set(w mdnsWorld) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if w.same(r.cur) {
		return nil
	}
	var next *pmdns.Conn
	if !w.empty() {
		if r.conn == nil {
			r.other = r.otherBriard(w)
		}
		c, err := r.open(w)
		if err != nil {
			return err
		}
		next = c
	} else {
		r.other = ""
	}
	if r.conn != nil {
		_ = r.conn.Close()
	}
	r.conn, r.cur = next, w
	return nil
}

// askTimeout bounds the bring-up question. A responder answers a shared name within tens of
// milliseconds; a second is the price of hearing nothing, paid once per bring-up, inside the
// promoter chain's first synchronous pass.
const askTimeout = time.Second

// otherBriard asks the question and judges the answer: it is a stranger only when it is not
// this node's own address. The old Primary of THIS flock may still answer the bare name for a
// moment after a failover, and it answers with the same VIP -- that is a handover, not a
// second household.
func (r *mdnsResponder) otherBriard(w mdnsWorld) string {
	ctx, cancel := context.WithTimeout(context.Background(), askTimeout)
	defer cancel()
	heard := r.ask(ctx, r.ifaces, routes.BareFlockHostName)
	if heard == "" || heard == w.addr {
		return ""
	}
	return heard
}

// askLAN sends one mDNS question for name on the household NICs and returns the first address
// that answers, or "" when nothing does before ctx ends. A throwaway conn with no local names:
// it answers nobody, and is closed as soon as it has heard.
func askLAN(ctx context.Context, ifaces []net.Interface, name string) string {
	addr4, err := net.ResolveUDPAddr("udp4", pmdns.DefaultAddressIPv4)
	if err != nil {
		return ""
	}
	l4, err := net.ListenUDP("udp4", addr4)
	if err != nil {
		return ""
	}
	opts := []pmdns.ServerOption{}
	if len(ifaces) > 0 {
		opts = append(opts, pmdns.WithInterfaces(ifaces...))
	}
	conn, err := pmdns.NewServer(ipv4.NewPacketConn(l4), nil, opts...)
	if err != nil {
		_ = l4.Close()
		return ""
	}
	defer func() { _ = conn.Close() }()
	_, addr, err := conn.QueryAddr(ctx, name)
	if err != nil {
		return ""
	}
	return addr.Unmap().String()
}

func (r *mdnsResponder) open(w mdnsWorld) (*pmdns.Conn, error) {
	ip := net.ParseIP(w.addr)
	if ip == nil {
		return nil, fmt.Errorf("mdns: %q is not an address to publish", w.addr)
	}
	// IPv4 is the one that must work: the VIP is v4, and a household that cannot reach the name
	// over v4 cannot reach it. IPv6 is opened when it can be and skipped when it cannot.
	addr4, err := net.ResolveUDPAddr("udp4", pmdns.DefaultAddressIPv4)
	if err != nil {
		return nil, fmt.Errorf("mdns: resolve v4 group: %w", err)
	}
	l4, err := net.ListenUDP("udp4", addr4)
	if err != nil {
		return nil, fmt.Errorf("mdns: listen on the v4 group: %w", err)
	}
	var p6 *ipv6.PacketConn
	if addr6, err := net.ResolveUDPAddr("udp6", pmdns.DefaultAddressIPv6); err == nil {
		if l6, err := net.ListenUDP("udp6", addr6); err == nil {
			p6 = ipv6.NewPacketConn(l6)
		}
	}

	opts := []pmdns.ServerOption{
		pmdns.WithLocalNames(w.names...),
		pmdns.WithLocalAddress(ip),
	}
	if len(r.ifaces) > 0 {
		// The household's NICs and nothing else -- never the interfaces podman creates.
		opts = append(opts, pmdns.WithInterfaces(r.ifaces...))
	}
	conn, err := pmdns.NewServer(ipv4.NewPacketConn(l4), p6, opts...)
	if err != nil {
		_ = l4.Close()
		return nil, fmt.Errorf("mdns: serve: %w", err)
	}
	for _, s := range w.svcs {
		// Host carries the trailing dot the SRV target needs; Instance and Type are read from the
		// table, never composed here.
		if err := conn.Register(pmdns.ServiceInstance{
			Instance: s.instance,
			Service:  s.typ,
			Domain:   "local",
			Host:     s.host + ".",
			Port:     s.port,
		}); err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("mdns: announce %s under %s: %w", s.instance, s.typ, err)
		}
	}
	return conn, nil
}

// published is the names this node is REALLY answering for, which writePublished records for the
// host. It is the set it was given: this responder never conflict-renames behind our back.
func (r *mdnsResponder) published() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.cur.names))
	copy(out, r.cur.names)
	return out
}

// otherHeard is the other briard the bring-up question heard, or "" -- none heard, or not
// publishing. writeOther records it beside published().
func (r *mdnsResponder) otherHeard() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.other
}

func (r *mdnsResponder) close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.conn != nil {
		_ = r.conn.Close()
		r.conn = nil
	}
	r.cur = mdnsWorld{}
}

// The four runtime files this reads, and the household NICs it answers on.
//
// ⚠️ PAIRED WITH THEIR WRITERS AND READERS: the guest agent writes vip.env (net.configure) and
// mdns.env (net.mdnsname) and reads back mdns.published and mdns.other, under its own consts of the
// same paths (agent/guestagent); briard-vip's script in guest-image/configuration.nix writes
// vip.live. They are constants rather than flags because nothing chooses them per deployment, and
// a flag would only be a second place for the same value to be wrong.
const (
	// vipEnvPath is the agent's configured VIP; vipLivePath is what briard-vip ACTUALLY claimed,
	// which under DHCP is the only one that knows. The live file wins when it names an address
	// (mdnsVIP).
	vipEnvPath  = "/run/briard/vip.env"
	vipLivePath = "/run/briard/vip.live"
	// mdnsEnvPath carries FLOCK_NAME, written by the agent's net.mdnsname and never baked: it is
	// PET identity arriving at a CATTLE image. Absent means the flock has no minted name, which
	// is a state, not a fault.
	mdnsEnvPath = "/run/briard/mdns.env"
	// mdnsPublishedPath is what the host reads back (net.mdnspublished) when something asks --
	// its report to the controller, `briard doctor` -- BARE: no `briard-` prefix and no `.local`.
	// Absent means this node publishes nothing, which is the normal answer on a Secondary.
	mdnsPublishedPath = "/run/briard/mdns.published"
	// mdnsOtherPath is the address of ANOTHER briard heard answering the bare `briard.local` when
	// this node came up (net.mdnsother). Absent means none was heard, or this node publishes
	// nothing. Written beside mdnsPublishedPath, read the same way, for the doctor to warn that
	// another briard also answers `briard.local`.
	mdnsOtherPath = "/run/briard/mdns.other"
	// mdnsWatch is how long a change takes to reach the wire. Lag, not a race: nothing waits on a
	// name within a deadline, and an install prints the name from the agent's own knowledge.
	mdnsWatch = 2 * time.Second
)

// householdNICs are the interfaces a household is on, and the exclusions are the point: never the
// interfaces podman creates, and never eth0 (qemu's user-net, the guest's WAN path). eth1 and eth2
// are both here because which one carries the VIP is not fixed (VIP_DEV); eth3, the private
// host<->guest link, is here because the host machine cannot hear the guest's multicast over
// macvtap, so it is the household's own machine's only path to the name -- and every answer
// carries the VIP, never the private link's address. Whichever of these exist are used; none
// existing is a fault worth failing on, because a responder answering on no interface is a name
// that resolves nowhere.
var householdNICs = []string{"eth1", "eth2", "eth3"}

// mdnsIfaces resolves the household NICs that exist on this node.
func mdnsIfaces() ([]net.Interface, error) {
	var out []net.Interface
	for _, name := range householdNICs {
		ifi, err := net.InterfaceByName(name)
		if err != nil {
			continue
		}
		out = append(out, *ifi)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("mdns: none of %v exist, so there is nowhere to answer", householdNICs)
	}
	return out, nil
}

// envValue reads one KEY=VALUE out of a systemd EnvironmentFile. Absent files and absent keys are
// "", never errors: every one of them has a legitimate empty state (no VIP yet, no minted name).
func envValue(path, key string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var val string
	for line := range strings.SplitSeq(string(raw), "\n") {
		line = strings.TrimSpace(line)
		k, v, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(k) != key {
			continue
		}
		// Last wins within a file, as systemd does.
		val = strings.Trim(strings.TrimSpace(v), `"'`)
	}
	return val
}

// mdnsVIP is the address every name resolves to: what briard-vip actually claimed, falling back to
// what the agent configured.
//
// ⚠️ IT IS THE RECORDED ADDRESS, NOT THE INTERFACE'S. Reading the device would be the ground truth
// net.vip reports, but a household NIC can carry the node's own address as well as the VIP
// (the node-IP doctrine), and picking between them here would be a SECOND rule for which
// address is the VIP. The live file is the existing one.
func mdnsVIP(livePath, cfgPath string) string {
	addr := envValue(livePath, "VIP_ADDR")
	if addr == "" {
		addr = envValue(cfgPath, "VIP_ADDR")
	}
	addr, _, _ = strings.Cut(addr, "/") // the prefix length is the claimer's business, not the name's
	return strings.TrimSpace(addr)
}

// serveMDNS keeps the wire matching the node's state until ctx ends.
//
// ⚠️ THE FIRST PASS IS SYNCHRONOUS AND ITS ERROR IS THE CALLER'S TO ESCALATE. A reverse proxy that
// cannot publish is a node a `.local`-only household cannot reach at all, so the failure belongs in
// the promoter chain rather than in a log line: a publisher that dies saying nothing leaves the
// name gone for days. Having NOTHING to publish is not that failure and never fails: a node with
// no minted flock name serves HTTP and says nothing on the LAN.
func serveMDNS(ctx context.Context, resp *mdnsResponder, tbl *routeReloader) error {
	tick := func() error {
		flock := envValue(mdnsEnvPath, "FLOCK_NAME")
		if err := resp.set(mdnsWorldFor(mdnsVIP(vipLivePath, vipEnvPath), flock, tbl.current().table)); err != nil {
			return err
		}
		if err := writePublished(mdnsPublishedPath, flock, resp.published()); err != nil {
			return err
		}
		return writeOther(mdnsOtherPath, resp.otherHeard())
	}
	if err := tick(); err != nil {
		return err
	}
	go func() {
		t := time.NewTicker(mdnsWatch)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				// AFTER the first pass, a failure is logged and retried rather than fatal: the
				// inputs move under us (converge rewrites the table, a lease is renewed), and
				// tearing the reverse proxy down over a transient read would trade a name for the
				// whole household's HTTP.
				if err := tick(); err != nil {
					log.Printf("reverse-proxy: mdns: %v", err)
				}
			}
		}
	}()
	return nil
}

// writePublished records the flock name this node is REALLY answering for, bare -- no `briard-`
// prefix and no `.local` -- where net.mdnspublished reads it. Publishing nothing REMOVES the file,
// because absent is how a Secondary says "none" and an empty file would be a name of length zero.
//
// It is derived from what the responder is actually serving, never from what it was asked to
// serve: the host reads this to answer "what name does this node really have", and echoing the
// request would report a name that is plausible and not necessarily the one being served.
func writePublished(path, flock string, names []string) error {
	want := routes.FlockHostName(flock)
	serving := false
	for _, n := range names {
		if n == want && want != "" {
			serving = true
			break
		}
	}
	if !serving {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("mdns: clearing %s: %w", path, err)
		}
		return nil
	}
	if err := os.WriteFile(path, []byte(flock+"\n"), 0o644); err != nil {
		return fmt.Errorf("mdns: recording the published name: %w", err)
	}
	return nil
}

// world is what is currently on the wire, for the one line the reverse proxy logs at startup.
func (r *mdnsResponder) world() mdnsWorld {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cur
}

// writeOther records the other briard the bring-up question heard, where net.mdnsother reads
// it; nothing heard REMOVES the file, for the reason writePublished removes its own.
func writeOther(path, addr string) error {
	if addr == "" {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("mdns: clearing %s: %w", path, err)
		}
		return nil
	}
	if err := os.WriteFile(path, []byte(addr+"\n"), 0o644); err != nil {
		return fmt.Errorf("mdns: recording the other briard: %w", err)
	}
	return nil
}
