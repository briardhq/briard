package model

// Role is a node's declared flock function: anchor = diskful voter
// that may host the workload; diskless = member with no storage peer (carries
// the witness vote and/or radio duties). "" is reserved for plain machines,
// which have no role; the agent's ROLE defaults to anchor.
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

// QuorumState is this node's serving state (is it the write authority for the home's data right
// now, and is it allowed to be), read as ground truth: the agent follows it and never overrides
// it. On a flock it is DRBD's view (role and quorum, `drbdsetup status`), hence the names. It is
// computed in one place, the guest's node-status verb (`drbd.status`), and readers ask Serving()
// for "does this node hold the house" rather than spelling the answer from the fields, so a node
// that runs no DRBD (a lone anchor) is made to read as Primary (its chain target active) and
// quorate by that verb alone, and no reader learns a second shape.
type QuorumState struct {
	Primary   bool `json:"primary"`   // is this node the write authority (on a flock: the DRBD primary)?
	Quorate   bool `json:"quorate"`   // is it allowed to write (on a flock: does it have quorum)?
	Connected int  `json:"connected"` // peers connected right now
	// This node's own storage, in the same terms PeerState reports for everyone else: whether it
	// is itself a viable failover target, not only whether a peer is. The OS-upgrade gate asks a
	// Secondary this rather than the VIP, which on a Secondary the peer answers. Same sample, same
	// parse.
	Diskful  bool `json:"diskful"`  // carries real storage — false for a diskless witness
	UpToDate bool `json:"uptodate"` // every volume this node carries is UpToDate
}

// Serving reports whether this node holds the house right now: it is the write authority and
// allowed to write. It is the one predicate for "is this node serving": readers ask here rather
// than spelling Primary ∧ Quorate or reading Primary alone to mean it, so the answer is derived
// once and the lone-node branch lives in the verb that fills the fields, never at a call site.
// A Primary that has lost quorum is not serving: DRBD refuses its writes (on-no-quorum=io-error)
// and the reactor demotes it. Narrower questions stay on the fields: Quorate alone is
// participation (a standby's whole job), Diskful/UpToDate are what the node carries, and
// Primary alone is the role, which single-primary and minority-refuses are invariants of.
func (q QuorumState) Serving() bool { return q.Primary && q.Quorate }

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
// stale data. Deliberately not "am I still quorate without me": quorum is about
// who may write, not about who can take the work.
func (p PeerState) CanTakeOver() bool { return p.Connected && p.Diskful && p.UpToDate }

// Cluster is this node's whole view of its DRBD resource: its own QuorumState plus its peers, one
// sample from one call. The demote decision compares "am I Primary" with "can a peer take over",
// so reading the two halves from separate calls would tear across exactly the state change being
// decided about. QuorumState is embedded, not nested, so its wire keys are top-level as in a bare
// QuorumState: this rides the drbd.status verb, and an agent on either side of a version skew
// reads what it knows and ignores the rest. Peers deliberately do not ride the cloud wire
// (shared/api's NodeStatus carries the QuorumState alone): this is an input to a local decision,
// not product-health telemetry, and widening the closed allowlist is a visible act.
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

// AppSpec describes one app the node runs. Every spec is built from a signed manifest
// (agent/host.specOf is the only constructor), so each field below is always filled.
type AppSpec struct {
	Name    string // e.g. "home-assistant"
	DataDir string // path on the DRBD volume

	// Units is the app's systemd units in start order, as the quadlet renderer produced them:
	// the pod first, then its members, because starting a pod unit does not start its
	// containers.
	Units []string
	// Unit is the single unit Start/Stop/Health act on — the container that actually serves, so
	// "is the app up?" keeps one answer even when the pod has several members.
	Unit string

	// Manifest is the identity of the signed manifest this app was installed from: sha256 over
	// the exact bytes (shared/manifest.Identity). It rides the spec rather than being re-read at
	// report time because the bytes it hashes are already in hand wherever a spec is built
	// (installedApps parses them at bring-up, the install path holds them), and re-reading the
	// cache to recompute a value that cannot change without an install would be a file read per
	// node per report cycle for nothing.
	Manifest string
	// Version is that manifest's upstream version, for people: what a page says the app runs
	// and what an offered update moves it from.
	Version string
	// MinMemoryMB is the manifest's declared minimum (shared/manifest.Manifest.MinMemoryMB): what
	// the guest must hold for this app from its first second, before growth can react.
	MinMemoryMB int
}

// ServingUnit is the systemd unit that answers "is this app up?", and therefore the one to
// measure an app's footprint from. The app names it explicitly: its units come from the quadlet
// renderer (briard-<app>-<container>.service, plus a pod unit) and the serving container is the
// one whose state is the app's state. The zero spec names nothing, which is the honest answer:
// asking systemd about a unit assembled from an empty name reports "inactive" forever.
//
// A method rather than a bare field read so that no caller rebuilds the name: a guessed
// "podman-<name>.service" names nothing on a runtime-installed app, and the footprint, and with
// it the crash-loop counter, would read zero for exactly the apps users install. One definition,
// on the type that carries the fact.
func (s AppSpec) ServingUnit() string { return s.Unit }
