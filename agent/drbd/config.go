package drbd

import (
	"fmt"
	"strings"
)

// The seam's names. DataDevice is the backing device every diskful node runs its data resource
// on: the data LV of the guest's VG. DRBD names it and never anything else, which is the whole
// point of the seam: the LV's table can be reloaded underneath, so the backing can be moved onto
// an encrypted PV and back with `pvmove` while DRBD's device object stays open. A bare disk has no
// table to reload, and inserting a seam later means DRBD must close and reopen.
//
// Defined here so the agent and the cloud's pairing directive name one value, and restated (never
// shared) by the nixosTest harness (nixosTest/lib.nix), which builds the VG this points into and
// cannot import Go.
const (
	// DataVG, DataLV and MetaLV are the volume group and the two logical volumes the seam builds.
	// The device paths are composed from these parts rather than restated beside them: the host
	// names the VG and the LVs separately when it renders the node's storage spec
	// (shared/nodestorage), and literals that must agree are places for them to stop agreeing.
	DataVG = "briardvol"
	DataLV = "data"
	// MetaLV holds DRBD's external metadata, at the end of the PV at a computed size
	// (agent/guestagent/nodestorage.go): DRBD's own internal-metadata placement, spelled in LVM so
	// the data LV can carry a filesystem that fills it and still be attached later without
	// shrinking anything.
	MetaLV = "metadata"
	// DataDevice is the mapper path the data LV appears at, and what a `.res` names as its
	// backing; MetaDevice is the metadata LV's, and what it names as `meta-disk`. ⚠️ Composed by
	// plain concatenation because our names carry no dash: device-mapper escapes a dash in a VG or
	// LV name by doubling it, so this form is correct only for names that have none.
	// nodestorage.Validate refuses the character, which is what keeps it true.
	DataDevice = "/dev/mapper/" + DataVG + "-" + DataLV
	MetaDevice = "/dev/mapper/" + DataVG + "-" + MetaLV
	// MaxPeers is the number of peer slots every node's metadata is created with (`create-md
	// --max-peers`). A product constant, not drbdadm's default of "the peers the .res names",
	// because the number is baked into the metadata: a node installed alone would get one slot,
	// and the flock it later grows into (a second anchor, a third, a witness) would need its
	// metadata recreated. Four is two more anchors than the two-anchor flock plus a witness, and
	// costs dataBytes/32768 per slot.
	MaxPeers = 4
)

// Peer is one node's placement in a DRBD resource. Address is "ip:port" on the private
// replication subnet; Disk is the backing device, or empty for a diskless witness.
//
// WitnessLocal, when set on a disk anchor, is that anchor's own "ip:port" on the private
// guest↔host witness link (e.g. "10.11.9.2:7789"). Its presence, with a diskless witness peer
// whose Address is the host forwarder, switches Config to explicit `connection` stanzas: the
// anchor reaches the cloud witness through its own host's forwarder, so the witness path needs a
// per-connection local address distinct from the anchor's LAN mesh Address, which connection-mesh
// (one address per node) cannot express. Empty = the plain single-node / LAN-witness case,
// rendered as a connection-mesh.
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

// DiskfulPeers counts the members that carry a replica, which decides whether the resource is
// worth running at all. Diskful on purpose: a witness is diskless, so one anchor beside a witness
// has one copy and no peer that could hold another, and a gate written as "peers > 0" would run
// DRBD on exactly the node it exists to spare.
func (r Resource) DiskfulPeers() int {
	n := 0
	for _, p := range r.Peers {
		if p.Disk != "" {
			n++
		}
	}
	return n
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

// writeOn renders one node's `on` block. withAddr controls the `address` line:
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
		fmt.Fprintf(b, "\t\t\tmeta-disk %s;\n", MetaDevice)
	}
	b.WriteString("\t\t}\n")
	b.WriteString("\t}\n")
}

// writeMesh is the plain form: each node carries its address, connection-mesh
// wires every pair.
func (r Resource) writeMesh(b *strings.Builder) {
	hosts := make([]string, len(r.Peers))
	for i, p := range r.Peers {
		hosts[i] = p.Name
		r.writeOn(b, p, true)
	}
	fmt.Fprintf(b, "\tconnection-mesh {\n\t\thosts %s;\n\t}\n", strings.Join(hosts, " "))
}

// writeExplicit is the forwarded-witness form (the witness-proxy shape): addresses live in the
// connections. The disk anchors interconnect directly on their LAN addresses; each anchor reaches
// the diskless witness through its own host forwarder (WitnessLocal → witness.Address). Targets
// the 2-anchor home: the anchor interconnect is a single pairwise `connection`.
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

// witnessSplit recognises a forwarded-witness mesh: exactly one diskless peer and every disk
// anchor carrying a WitnessLocal, returning the witness + the disk anchors so Config renders
// explicit connections. Otherwise ok=false and the resource renders as a plain connection-mesh,
// single-node and the paired LAN-witness case (a diskless peer but no WitnessLocal) included.
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

// ReactorConfig renders the drbd-reactor promoter snippet: the ordered unit that runs on the
// primary only, in start order (reverse on demote); drbd-reactor, not this code, drives that
// lifecycle. start lists the systemd units in the order they must come up (shared/chain.Members:
// storage → VIP → apps → reverse proxy → dashboard).
//
// ⚠️ `adjust-resource-on-start = false` draws the layer line. The promoter's default, true, has the
// reactor run its own `drbdadm adjust` (attach the backing device) the moment it reads this
// snippet. Attaching is not its job here: the guest's node-storage step starts the stock
// `drbd@<res>.target` (drbd-utils' "(Re)configure DRBD resource" unit,
// agent/guestagent/nodestorage.go) and reads its result, because a supervisor that cannot tell
// whether bring-up succeeded is not supervising. With both, the two race (~250ms apart, landing
// about half the time) and the loser fails with
//
//	0: Failure: (124) Device is attached to a disk (use detach first)
//
// failing drbd@<res>.service, the target and bring-up: the host agent exits non-zero and systemd
// restarts it. A harness never shows it (its config is baked, no agent races it). Upstream
// provides this key for exactly this split: the promoter promotes, the drbd@ chain configures.
//
// ⚠️ `target-as = Wants` is the other layer line. The default, `Requires`, would demote the node on
// one crash of a member (job-level propagation: chainMemberFailure, agent/guestagent/units.go).
// With `Wants` the target still starts every member, and a member that stops, crashes or is
// restarted no longer reaches the promotion. A member that has genuinely given up still hands the
// resource on, through a state-level hook, not a dependency: chainMemberFailure's
// `OnFailure=briard-promotion-hold.service` on every member.
func ReactorConfig(resource string, start []string) string {
	quoted := make([]string, len(start))
	for i, u := range start {
		quoted[i] = fmt.Sprintf("%q", u)
	}
	return fmt.Sprintf(
		"[[promoter]]\n[promoter.resources.%s]\nadjust-resource-on-start = false\ntarget-as = \"Wants\"\nstart = [ %s ]\n",
		resource, strings.Join(quoted, ", "),
	)
}
