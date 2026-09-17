package main

// The household's mDNS responder ([B.152]), which lives in the door for the reason the door
// exists: a `.local` name means "this service is reachable here", and the door is what "here"
// is. It answers off the SAME routing table the router reads, so a name that is published but
// not routed is not expressible, where a separate publisher could only assert it, because it
// is a second reader of the same file.
//
// ⚠️ IT ANSWERS QUERIES AND SAYS NOTHING UNSOLICITED. No probing, no announcements, no goodbyes.
// That is the whole robustness argument for replacing avahi rather than a shortcut: avahi's
// failures were a state machine we could not see into — an entry group that wedged, silently,
// permanently, if the address moved inside its probe window (V3.22) or an interface appeared
// inside it ([V3b.30](a)) — and a responder with no probe has no such state. "What address does
// this name have" is a field read when the answer is written.
//
// ⚠️ THE COST, ACCEPTED ON THE OWNER'S CALL ([B.152]): a browsing client learns of a NEW service
// on its next periodic query rather than instantly, and an uninstalled one leaves a stale PTR
// until its TTL. Failover needs none of it — both nodes publish byte-identical records, because
// every name is flock-scoped and points at a VIP whose address does not change when it moves.
// ⚠️ If an announcer is ever added, `nixosTest/install-macvtap`'s cold-cache assertion needs its
// announcement-tail wait back: an announcement is the same packet as a response, so a client can
// be answered by a multicast it never asked for, and the assertion silently becomes a cache read
// ([B.129] is what that hid: inbound mDNS dropped on the macvtap while egress worked perfectly).

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
	host     string // the SRV target: the service's OWN name, never the guest's node-scoped hostname
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
// materialised (routes.HostName) or from routes.FlockHostName, so the naming rule stays in one
// function.
//
// AN EMPTY WORLD IS A REAL STATE, not a failure: a node whose flock has no minted name, or that
// has converged to nothing, publishes nothing. The door serves HTTP either way.
func mdnsWorldFor(addr, flock string, t routes.Table) mdnsWorld {
	w := mdnsWorld{addr: addr}
	if addr == "" {
		// No address is nothing to point a name at, and a name resolving to nowhere is worse
		// than a name that does not resolve: the household gets a connection that hangs instead
		// of one that fails. The same rule already governed the shell publisher.
		return mdnsWorld{}
	}
	seen := map[string]bool{}
	add := func(n string) {
		if n == "" || seen[n] {
			return
		}
		seen[n] = true
		w.names = append(w.names, n)
	}
	add(routes.FlockHostName(flock))
	for _, s := range t.Services {
		for _, h := range s.Hosts {
			add(h)
		}
	}
	for _, s := range t.Services {
		if len(s.Hosts) == 0 {
			// An announcement needs an SRV target and the only candidate is the service's own
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
// way to change the NAME set — pion fixes local names at construction — and rebuilding is cheap
// precisely because nothing probes: the new server answers its first query, where avahi would have
// had to re-establish an entry group.
type mdnsResponder struct {
	ifaces []net.Interface

	mu   sync.Mutex
	conn *pmdns.Conn
	cur  mdnsWorld
}

func newMDNSResponder(ifaces []net.Interface) *mdnsResponder {
	return &mdnsResponder{ifaces: ifaces}
}

// set makes the wire match w, and is a no-op when it already does.
//
// ⚠️ THE NEW SERVER IS OPENED BEFORE THE OLD ONE IS CLOSED. Two sockets share :5353 (they set
// SO_REUSEADDR, which is also why the guest's avahi and Home Assistant's own python-zeroconf have
// always coexisted there), so this leaves no window in which the name resolves nowhere — and,
// more importantly, no window in which a rebuild can fail because the port it just released is
// still held. For the few milliseconds both exist they answer identically for every unchanged
// name, which is the shadowing case mDNS defines as no conflict at all.
func (r *mdnsResponder) set(w mdnsWorld) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if w.same(r.cur) {
		return nil
	}
	var next *pmdns.Conn
	if !w.empty() {
		c, err := r.open(w)
		if err != nil {
			return err
		}
		next = c
	}
	if r.conn != nil {
		_ = r.conn.Close()
	}
	r.conn, r.cur = next, w
	return nil
}

func (r *mdnsResponder) open(w mdnsWorld) (*pmdns.Conn, error) {
	ip := net.ParseIP(w.addr)
	if ip == nil {
		return nil, fmt.Errorf("mdns: %q is not an address to publish", w.addr)
	}
	// IPv4 is the one that must work: the VIP is v4, and a household that cannot reach the name
	// over v4 cannot reach it. IPv6 is opened when it can be and skipped when it cannot, which is
	// what avahi did here too -- it joins the v6 group when the interface has a v6 address.
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
		// The household's NICs and nothing else -- never the interfaces podman creates, which is
		// the exclusion avahi's allowInterfaces expressed ([V3b.30](a)).
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

// published is the names this node is REALLY answering for, which the host reads every observe
// cycle. It is the set it was given: nothing renames behind our back, which is the apparatus
// avahi's silent conflict-rename made necessary and this responder makes unnecessary.
func (r *mdnsResponder) published() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.cur.names))
	copy(out, r.cur.names)
	return out
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
// ⚠️ PAIRED WITH guest-image/configuration.nix, which writes three of them and names the same
// interfaces -- the pairing the topology word already uses ("PAIRED with the Go const
// guestagent.topologyEnvPath"). They are constants rather than flags because nothing chooses them
// per deployment: the image fixes the paths and names the NICs, and a flag would only be a second
// place for the same value to be wrong.
const (
	// vipEnvPath is the agent's configured VIP; vipLivePath is what briard-vip ACTUALLY claimed,
	// which under DHCP is the only one that knows. Last wins, exactly as the unit's
	// EnvironmentFile ordering did.
	vipEnvPath  = "/run/briard/vip.env"
	vipLivePath = "/run/briard/vip.live"
	// mdnsEnvPath carries FLOCK_NAME, written by the agent's net.mdnsname and never baked: it is
	// PET identity arriving at a CATTLE image. Absent means the flock has no minted name, which
	// is a state, not a fault.
	mdnsEnvPath = "/run/briard/mdns.env"
	// mdnsPublishedPath is what the host reads back every observe cycle (net.mdnspublished),
	// BARE -- no `briard-` prefix and no `.local`. Absent means this node publishes nothing, which
	// is the normal answer on a Secondary.
	mdnsPublishedPath = "/run/briard/mdns.published"
	// mdnsWatch is how long a change takes to reach the wire. Lag, not a race: nothing waits on a
	// name within a deadline, and an install prints the name from the agent's own knowledge.
	mdnsWatch = 2 * time.Second
)

// householdNICs are the interfaces a household is on, and the exclusions are the point: never the
// interfaces podman creates ([V3b.30](a) -- a veth appearing mid-probe is what wedged avahi), and
// never the host link. Whichever of these exist are used; none existing is a fault worth failing
// on, because a responder answering on no interface is a name that resolves nowhere.
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
// ([V3b.26]'s node-IP doctrine), and picking between them here would be a SECOND rule for which
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
// ⚠️ THE FIRST PASS IS SYNCHRONOUS AND ITS ERROR IS THE CALLER'S TO ESCALATE. A door that cannot
// publish is a node a `.local`-only household cannot reach at all, so the failure belongs in the
// promoter chain rather than in a log line -- which is the whole lesson of the daemon this
// replaces ([B.151]: avahi died, said nothing, and the name was gone for days). Having NOTHING to
// publish is not that failure and never fails: a node with no minted flock name serves HTTP and
// says nothing on the LAN.
func serveMDNS(ctx context.Context, resp *mdnsResponder, tbl *routeReloader) error {
	tick := func() error {
		flock := envValue(mdnsEnvPath, "FLOCK_NAME")
		if err := resp.set(mdnsWorldFor(mdnsVIP(vipLivePath, vipEnvPath), flock, tbl.current().table)); err != nil {
			return err
		}
		return writePublished(mdnsPublishedPath, flock, resp.published())
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
				// tearing the front door down over a transient read would trade a name for the
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
// serve: the host reads this every cycle to answer "what name does this node really have", and
// echoing the request would rebuild the failure the read-back exists to end (V3.19).
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

// world is what is currently on the wire, for the one line the door logs at startup.
func (r *mdnsResponder) world() mdnsWorld {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cur
}
