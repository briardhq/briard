package model

// Role is a node's declared flock function: anchor = diskful voter
// that may host the workload; diskless = member with no storage peer (carries
// the witness vote and/or radio duties). Plain machines have no role — today
// the agent still defaults to anchor (flock-scoped roles land with the
// placement work).
type Role string

const (
	RoleAnchor   Role = "anchor"
	RoleDiskless Role = "diskless"
)

// StorageClass selects a workload's storage + HA semantics.
type StorageClass string

const (
	StorageCritical StorageClass = "critical" // DRBD, synchronous, no SPOF
	StorageBulk     StorageClass = "bulk"     // NAS/SMB, weaker, isolated SPOF
)

// QuorumState is DRBD's view of this node, read as ground truth.
// The agent follows it; it never overrides it.
type QuorumState struct {
	Primary   bool `json:"primary"`   // is this node the DRBD primary?
	Quorate   bool `json:"quorate"`   // does this node currently have quorum?
	Connected int  `json:"connected"` // peers connected right now
	// This node's OWN storage, in the same terms PeerState reports for everyone else
	//. Their absence was load-bearing rather than an oversight: a node could say
	// Whether a PEER was a viable failover target but not whether IT was one — so the
	// OS-upgrade gate had nothing node-local to ask a Secondary, and asked the VIP instead,
	// which on a Secondary is answered by the peer. Same sample, same parse.
	Diskful  bool `json:"diskful"`  // carries real storage — false for a diskless witness
	UpToDate bool `json:"uptodate"` // every volume this node carries is UpToDate
}

// PeerState is one peer as *this* node sees it, read from the same
// `drbdsetup status --json` that yields QuorumState. It answers a question
// QuorumState structurally cannot: not "am I serving?" but "is there someone else
// who could?". Read-only ground truth, like QuorumState — the agent follows it.
type PeerState struct {
	Name      string `json:"name"`      // the peer's resource-config name (its node name)
	NodeID    int    `json:"node_id"`   // DRBD node-id; distinct from address
	Connected bool   `json:"connected"` // connection-state == Connected right now
	Role      string `json:"role"`      // Primary | Secondary | Unknown (DRBD's spelling, verbatim)
	Diskful   bool   `json:"diskful"`   // has real storage — false for a diskless witness/client
	UpToDate  bool   `json:"uptodate"`  // every volume this peer carries is UpToDate
}

// CanTakeOver reports whether this peer could serve the workload if this node
// stopped — the predicate the demote-first reboot rests on. All three
// conjuncts are load-bearing, and each rules out a peer that would otherwise look
// like a successor: an unreachable one cannot promote, a *diskless* one has no data
// to serve (a 1-data + 2-witness flock is quorate without us and yet has no
// successor — demoting into it is an outage), and an out-of-date one would serve
// stale data. Deliberately NOT "am I still quorate without me": quorum is about
// who may write, not about who can take the work.
func (p PeerState) CanTakeOver() bool { return p.Connected && p.Diskful && p.UpToDate }

// Cluster is this node's whole view of its DRBD resource: its own QuorumState plus
// its peers. One sample, one call — the demote decision compares "am I Primary" with
// "can a peer take over", so reading the two halves from separate calls would tear
// across exactly the state change being decided about.
//
// QuorumState is embedded, not nested, so the wire keys stay where they were: this
// rides the existing drbd.status verb, and an agent on either side of the version
// skew reads what it knows and ignores the rest.
//
// Peers deliberately do NOT ride the cloud wire — shared/api's NodeStatus keeps the
// three-field QuorumState summary. This is an input to a local decision, not
// product-health telemetry, and widening the closed allowlist is a visible act.
type Cluster struct {
	QuorumState
	Peers []PeerState `json:"peers,omitempty"`
}

// PeerCanTakeOver reports whether any peer could serve if this node stopped.
func (c Cluster) PeerCanTakeOver() bool {
	for _, p := range c.Peers {
		if p.CanTakeOver() {
			return true
		}
	}
	return false
}

// ServiceSpec describes the payload the unit should run.
type ServiceSpec struct {
	Name    string // e.g. "home-assistant"
	Image   string // pinned OCI image ref
	DataDir string // path on the DRBD volume

	// Units is the ordered set of systemd units the promoter starts for this service, between
	// the data mount and the VIP. Empty means the BAKED slot, whose single unit is
	// derived from Name — so a node that never had a service installed at runtime behaves
	// exactly as before. A runtime install fills this from the quadlet renderer: the pod first,
	// then its members, because starting a pod service does NOT start its containers (proven by
	// the quadlet spike).
	Units []string
	// Unit is the single unit Start/Stop/Health act on — the container that actually serves, so
	// "is the service up?" keeps one answer even when the pod has several members. Empty falls
	// back to the baked slot's derived name.
	Unit string

	// Manifest is the identity of the signed manifest this service was installed from --
	// sha256 over the exact bytes (shared/manifest.Identity). Empty means the BAKED slot, which
	// was never installed from a manifest and so has no identity: the same discriminator Unit
	// uses, and it dies with the slot ([V3b.3](e1)).
	//
	// It rides the spec rather than being re-read at report time because the bytes it hashes are
	// already in hand wherever a spec is built -- installedServices parses them at bring-up and
	// the install path holds them -- and re-reading the cache once per report cycle to recompute
	// a value that cannot change without an install would be a file read per node per cycle for
	// nothing.
	Manifest string
}

// ServingUnit is the systemd unit that answers "is this service up?", and therefore the one to
// measure a service's footprint from.
//
// A runtime-installed service names it explicitly: its units come from the quadlet renderer
// (briard-<service>-<container>.service, plus a pod unit) and the SERVING container is the one
// whose state is the service's state. An empty Unit means the baked slot, whose unit has always
// been derived from the name.
//
// It lives on the spec because the derivation was written twice: the guest manager resolved it
// correctly while the host's resource telemetry rebuilt "podman-<name>.service" inline, which
// names nothing on a runtime-installed service — so the payload footprint, and with it the
// crash-loop counter, read zero for exactly the services users install. One definition, on the
// type that carries the fact.
func (s ServiceSpec) ServingUnit() string {
	if s.Unit != "" {
		return s.Unit
	}
	if s.Name == "" {
		return "" // no service at all: naming podman-.service is what asked systemd about nothing
	}
	return "podman-" + s.Name + ".service"
}
