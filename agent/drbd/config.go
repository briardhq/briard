package drbd

import (
	"fmt"
	"strings"
)

// Peer is one node's placement in a DRBD resource. Address is "ip:port" on the
// private replication subnet; Disk is the backing device, or empty
// for a diskless witness.
//
// WitnessLocal, when set on a disk anchor, is that anchor's own "ip:port" on the
// private guest↔host witness link (e.g. "10.9.9.2:7789"). Its presence — together
// with a diskless witness peer whose Address is the host forwarder — switches
// Config to explicit `connection` stanzas: the anchor reaches the
// cloud witness through its OWN host's forwarder, so the witness path needs a
// per-connection local address distinct from the anchor's LAN mesh Address, which
// connection-mesh (one address per node) cannot express. Empty = the plain
// single-node / LAN-witness case, rendered as before.
type Peer struct {
	Name         string
	NodeID       int
	Address      string
	Disk         string
	WitnessLocal string
}

// Resource describes a single-volume DRBD 9 resource. The safety options are
// fixed, not configurable — they are the mandatory arbiter config:
// quorum majority, on-no-quorum=io-error, force-secondary on outdated primary,
// no auto-promote (drbd-reactor promotes). This is the production form of what
// nixosTest/lib.nix bakes for the nixosTests.
type Resource struct {
	Name   string // e.g. "r0"
	Device string // e.g. "/dev/drbd0"
	Peers  []Peer
}

// Config renders the resource as a drbd.d/<name>.res file. A plain resource uses
// the connection-mesh form (one address per node, DRBD wires the pairs). A
// forwarded-witness resource instead uses explicit `connection` stanzas
// so the anchor↔witness path can carry its own local address — see witnessSplit.
func (r Resource) Config() string {
	var b strings.Builder
	fmt.Fprintf(&b, "resource %s {\n", r.Name)
	b.WriteString("\tnet {\n\t\tprotocol C;\n\t}\n")
	b.WriteString("\toptions {\n")
	b.WriteString("\t\tauto-promote no;\n")
	b.WriteString("\t\tquorum majority;\n")
	b.WriteString("\t\ton-no-quorum io-error;\n")
	b.WriteString("\t\ton-suspended-primary-outdated force-secondary;\n")
	b.WriteString("\t}\n")
	if witness, anchors, ok := r.witnessSplit(); ok {
		r.writeExplicit(&b, witness, anchors)
	} else {
		r.writeMesh(&b)
	}
	b.WriteString("}\n")
	return b.String()
}

// WriteOn renders one node's `on` block. withAddr controls the `address` line:
// connection-mesh puts each node's address here; the explicit-connection form
// keeps addresses in the connections (a node's two links use different locals).
func (r Resource) writeOn(b *strings.Builder, p Peer, withAddr bool) {
	fmt.Fprintf(b, "\ton %s {\n", p.Name)
	fmt.Fprintf(b, "\t\tnode-id %d;\n", p.NodeID)
	if withAddr {
		fmt.Fprintf(b, "\t\taddress %s;\n", p.Address)
	}
	b.WriteString("\t\tvolume 0 {\n")
	fmt.Fprintf(b, "\t\t\tdevice %s;\n", r.Device)
	if p.Disk == "" {
		b.WriteString("\t\t\tdisk none;\n")
	} else {
		fmt.Fprintf(b, "\t\t\tdisk %s;\n", p.Disk)
		b.WriteString("\t\t\tmeta-disk internal;\n")
	}
	b.WriteString("\t\t}\n")
	b.WriteString("\t}\n")
}

// WriteMesh is the plain form: each node carries its address, connection-mesh
// wires every pair. Unchanged from the older renderer.
func (r Resource) writeMesh(b *strings.Builder) {
	hosts := make([]string, len(r.Peers))
	for i, p := range r.Peers {
		hosts[i] = p.Name
		r.writeOn(b, p, true)
	}
	fmt.Fprintf(b, "\tconnection-mesh {\n\t\thosts %s;\n\t}\n", strings.Join(hosts, " "))
}

// WriteExplicit is the forwarded-witness form (the witness-proxy shape): addresses live in the connections. The disk anchors
// interconnect directly on their LAN addresses; each anchor reaches the diskless
// witness through its OWN host forwarder (WitnessLocal → witness.Address). Targets
// the 2-anchor home — the anchor interconnect is a single pairwise `connection`.
func (r Resource) writeExplicit(b *strings.Builder, witness Peer, anchors []Peer) {
	for _, p := range r.Peers {
		r.writeOn(b, p, false)
	}
	b.WriteString("\tconnection {\n")
	for _, a := range anchors {
		fmt.Fprintf(b, "\t\thost %s address %s;\n", a.Name, a.Address)
	}
	b.WriteString("\t}\n")
	for _, a := range anchors {
		b.WriteString("\tconnection {\n")
		fmt.Fprintf(b, "\t\thost %s address %s;\n", a.Name, a.WitnessLocal)
		fmt.Fprintf(b, "\t\thost %s address %s;\n", witness.Name, witness.Address)
		b.WriteString("\t}\n")
	}
}

// WitnessSplit recognises a forwarded-witness mesh: exactly one diskless
// peer and every disk anchor carrying a WitnessLocal. Returns the witness + the
// disk anchors so Config renders explicit connections. Otherwise ok=false and the
// resource renders as a plain connection-mesh — so single-node and the paired
// LAN-witness case (a diskless peer but no WitnessLocal) are unchanged.
func (r Resource) witnessSplit() (witness Peer, anchors []Peer, ok bool) {
	var diskless []Peer
	for _, p := range r.Peers {
		if p.Disk == "" {
			diskless = append(diskless, p)
		} else {
			anchors = append(anchors, p)
		}
	}
	if len(diskless) != 1 || len(anchors) == 0 {
		return Peer{}, nil, false
	}
	for _, a := range anchors {
		if a.WitnessLocal == "" {
			return Peer{}, nil, false
		}
	}
	return diskless[0], anchors, true
}

// ReactorConfig renders the drbd-reactor promoter snippet: the ordered unit that
// runs on the primary only, in start order (reverse on demote). drbd-reactor —
// not this code — drives that lifecycle. start lists the systemd
// units in the order they must come up (workload → VIP).
func ReactorConfig(resource string, start []string) string {
	quoted := make([]string, len(start))
	for i, u := range start {
		quoted[i] = fmt.Sprintf("%q", u)
	}
	return fmt.Sprintf(
		"[[promoter]]\n[promoter.resources.%s]\nstart = [ %s ]\n",
		resource, strings.Join(quoted, ", "),
	)
}
