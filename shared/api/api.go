package api

import (
	"time"

	"briard.io/shared/model"
)

// Host<->guest control protocol versioning. The host and guest agents
// version this wire protocol so they can evolve on independent cadences and so the host
// detects skew during a rolling update/failover -- a survivor's guest may run a newer OS
// generation (thus a newer guest agent) than the host was built against. The host
// handshakes on connect and refuses a guest whose protocol it can't speak: a safe
// deferral (bring-up/upgrade fails -> rollback / no promotion) beats silent misbehaviour.
const (
	GuestProtocol    = 1 // the current host<->guest wire protocol version
	MinGuestProtocol = 1 // the oldest guest protocol this host can still drive
)

// GuestHello is the guest's handshake reply: its protocol version plus the verbs it
// supports (fine-grained capability negotiation on top of the coarse version gate), and
// which BOOT of the guest is answering.
//
// BootID is the guest kernel's boot_id, and it is the host's only way to tell "the in-guest
// agent bounced" from "the guest rebooted underneath me" -- two events that look identical
// on the channel and need opposite responses ([B.102]). The agent serves one connection then
// exits, so a handshake re-running proves nothing; the boot_id is stable across that and
// changes only across an actual boot. Empty from a guest too old to send it, which reads as
// "no evidence" rather than "a new boot" -- the host must not re-converge on silence.
type GuestHello struct {
	Version      int      `json:"version"`
	Capabilities []string `json:"capabilities,omitempty"`
	BootID       string   `json:"boot_id,omitempty"`
}

// EnrollRequest asks an overlay provider to admit this node to the tenant network.
type EnrollRequest struct {
	Tenant   string
	NodeName string
}

// NodeIdentity is who this node is on the overlay after enrollment.
type NodeIdentity struct {
	Name    string
	Address string // overlay address
}

// NodeInfo is what a node tells the cloud about itself at registration.
type NodeInfo struct {
	NodeName string     `json:"node_name"`
	Role     model.Role `json:"role"`
	// Timezone is this machine's IANA zone name ("Europe/Athens"), read from the host at
	// registration; "" when it cannot be determined. It is a SCHEDULING INPUT, not telemetry:
	// automatic updates run inside a window the household states in LOCAL time,
	// and 03:00 in the wrong zone is the middle of someone's evening. Without it a home cannot
	// be scheduled at all, and since automatic updates are obligatory that is a fault rather
	// than a preference.
	//
	// It widens the closed upward allowlist by one coarse-location field, which
	// is why it is spelled out here rather than added quietly: a zone is roughly a continent
	// and an hour, it is already derivable from the local timestamps a node sends, and the
	// product cannot deliver the window the owner asked for without it. The cloud stores it
	// against the HOME with the first reporter winning; a later disagreement raises an operator
	// alert rather than moving a schedule the household already has.
	Timezone string `json:"timezone,omitempty"`
}

// Assignment is the cloud's answer: which tenant/role this node belongs to.
type Assignment struct {
	Tenant string     `json:"tenant"`
	Role   model.Role `json:"role"`
}

// NodeStatus is the periodic state a node reports up. This
// carries health *signals*, never user-home contents.
type NodeStatus struct {
	NodeName string            `json:"node_name"`
	Role     model.Role        `json:"role"`
	Quorum   model.QuorumState `json:"quorum"`
	Healthy  bool              `json:"healthy"`
	// Tenant is the tenant this node was assigned at registration. Single-tenant
	// today (DefaultTenant); the cloud treats it as advisory -- authoritative binding of
	// tenant to the node's identity is the cloud's job, so the controller keys storage on its own
	// tenant, not this self-asserted value. Empty on a node that hasn't registered.
	Tenant string `json:"tenant,omitempty"`
	// Image is the payload OCI ref this node is currently serving. It makes a
	// rolling update observable through the seam: the controller confirms convergence
	// to the new image, not just a health blip. Empty on a witness (no payload).
	Image string `json:"image,omitempty"`
	// System is the NixOS system closure this node is currently running (whole-OS
	// rolling update): the store path of /run/current-system. Ground truth for the OS
	// rollout -- the controller confirms the serving node switched to the target closure.
	// Empty on a witness (or when unread).
	System string `json:"system,omitempty"`
	// AgentVersion is the release id of the host-agent binary now running. It makes a
	// host-agent self-update observable through the seam the same way Image does for the
	// payload: the cloud confirms a canary committed the offered version (its trial went
	// healthy) before it offers the fleet -- a reverted trial keeps the old version, so a bad
	// build never advances past the canary. Empty when the binary is built without a version.
	AgentVersion string `json:"agent_version,omitempty"`
	// PublishedName is the flock name this node is ACTUALLY publishing over mDNS, bare
	// ("brave-elf"), read back from avahi rather than echoed from config. Empty on a node
	// publishing none: a Secondary (the name is bound to the VIP, so only the serving node
	// publishes it), a witness, or a node installed before the name existed.
	//
	// Widening the closed allowlist by one field is a deliberate act, so here is the argument.
	// The value is the household's own public LAN name -- a curated random word pair carrying no
	// login name, which is exactly why it is not $SUDO_USER -- and the cloud needs it for two
	// things it cannot do otherwise: reconcile a name CLAIM against what is really being served,
	// and notice a silent avahi conflict-rename, which is otherwise invisible to everyone
	// including the household whose name changed.
	PublishedName string `json:"published_name,omitempty"`
	// Overlay is this node's remote-reach state. Nil when the node runs no
	// overlay (standalone / LAN-only, the default) -- the overlay is off the failover
	// critical path, so it reports as a signal, never a gate.
	Overlay *OverlayStatus `json:"overlay,omitempty"`
	// Resource-footprint telemetry (RSS/FDs/load/disk, guest kernel errors, restart counts)
	// deliberately does NOT ride this wire: it is a soak leak-instrument, not
	// product-health, so it stays home on the internal host→lab channel (shared/telemetry).
	// shared/api is the closed cloud allowlist -- a product-health subset re-enters via the
	// separate MetricsReport (volume used, load -- rolled up, never raw), NOT on this
	// per-report current-state. Expanding that set is a deliberate, visible act.
}

// MetricAggregate is one field's hourly rollup a node uploads: min/max/avg over the
// raw samples it measured in the hour beginning at PeriodStart, plus Samples (the count behind
// them). Aggregates only -- raw samples never leave the node (V2-retention). The read-side
// mirror of store.MetricAggregate, like NodeReport mirrors NodeStatus.
type MetricAggregate struct {
	Metric      string    `json:"metric"`
	PeriodStart time.Time `json:"period_start"`
	Min         float64   `json:"min"`
	Max         float64   `json:"max"`
	Avg         float64   `json:"avg"`
	Samples     int       `json:"samples"`
}

// MetricsReport is a node's upload of its hourly aggregates (MetricsPath POST). Node names
// itself in the envelope (like NodeStatus.NodeName on a report); the cloud keys on its own
// tenant. Aggregates carry only the deliberate dashboard allowlist of numeric fields (volume
// used, load) -- the soak's internal leak instruments stay home on shared/telemetry.
// Expanding the set is a visible act: the wire is the closed allowlist.
type MetricsReport struct {
	Node       string            `json:"node"`
	Aggregates []MetricAggregate `json:"aggregates"`
}

// MetricsResponse carries a page of a node's aggregates for one metric in PeriodStart order
// (MetricsPath GET) -- the read a cloud dashboard renders. Advance by passing the last
// PeriodStart as the next request's `since`.
type MetricsResponse struct {
	Aggregates []MetricAggregate `json:"aggregates"`
}

// CloudDiag is the controller's self-report of its own health — the cloud reporting on
// itself, read by the soak's trend oracle (the analog of a node's resource telemetry, for the
// cloud). Two surfaces: the controller's process footprint (RSS/FDs — the ingestion/connection
// -leak surface a long run would expose, e.g. a pgx pool that never releases) and the durable
// store's size (the growth-bounded surface — does retention hold the line, does node_states stay
// flat). Not privacy-sensitive: these are the cloud's own operational numbers, never a node's.
type CloudDiag struct {
	RSSKB int64      `json:"rss_kb"` // controller resident set (KB)
	FDs   int        `json:"fds"`    // controller open file descriptors
	Store StoreStats `json:"store"`
}

// StoreStats is per-table row counts plus the on-disk size (DBBytes 0 for the in-memory Fake).
// The soak watches these for unbounded growth: NodeStates must stay flat (upsert = one row per
// node, so a climb means ingest is leaking rows per heartbeat), Events grow only on transitions
// (legitimate), and the metric tiers are held by retention policy.
type StoreStats struct {
	NodeStates    int64 `json:"node_states"`
	Events        int64 `json:"events"`
	MetricsHourly int64 `json:"metrics_hourly"`
	MetricsDaily  int64 `json:"metrics_daily"`
	Directives    int64 `json:"directives"`
	DBBytes       int64 `json:"db_bytes"`
}

// OverlayStatus is a node's remote-reach health, mirrored from overlay.Health.
type OverlayStatus struct {
	Up      bool `json:"up"`
	Relayed bool `json:"relayed,omitempty"` // peer connectivity is via relay, not direct
	PeersUp int  `json:"peers_up,omitempty"`
}

// Directive is an instruction the controller sends down (upgrade, config, role change).
type Directive struct {
	ID      string `json:"id,omitempty"`      // stable intent id the node echoes in its outcome
	Kind    string `json:"kind"`              // e.g. DirectiveNoop, DirectiveLog; "upgrade"
	Payload string `json:"payload,omitempty"` // opaque to transport; interpreted by the agent
}

// DirectiveOutcome is a node's report of a planned op's terminal result, keyed by the directive
// ID it acted on: the cloud moves that intent to a terminal state so
// a post-outage reconcile knows the op finished. Detail carries the cause on a non-clean result.
type DirectiveOutcome struct {
	ID     string `json:"id"`
	State  string `json:"state"` // OutcomeDone / OutcomeRolledBack / OutcomeFailed
	Detail string `json:"detail,omitempty"`
}

// Terminal outcome states a node reports for a planned op. They mirror the cloud store's
// directive states 1:1 -- the wire value is the persisted value.
const (
	OutcomeDone       = "done"        // the op succeeded
	OutcomeRolledBack = "rolled-back" // the op failed its health-gate and auto-reverted
	OutcomeFailed     = "failed"      // the op failed with no clean revert (e.g. a wedged rollback)
)

// Directive kinds the agent understands. The down-channel carries cheap,
// safe directives plus "upgrade" (routed to guest.Manager).
const (
	DirectiveNoop    = "noop"    // acknowledge only -- proves the round-trip
	DirectiveLog     = "log"     // agent logs Payload -- push a marker/instruction to a node
	DirectiveUpgrade = "upgrade" // Payload = the new payload OCI image ref; the agent runs
	//                              Guest.Manager.UpgradePayload (health-gated, auto-rollback),
	//                              which pins it so a failover converges (rolling update).
	DirectiveUpgradeSystem = "upgrade-system" // Payload = target system closure store path; the
	//                              Agent runs guest.Manager.Upgrade (whole-OS switch, health-gated,
	//                              auto-rollback), pinning it so a failover converges.
	DirectiveCert = "cert" // Payload = a JSON CertBundle (cert-only); the agent pairs it with the
	//                        Key it holds and writes both to the DRBD volume, where the TLS
	//                        terminator hot-reloads it (renewal, cloud-scheduled).
	DirectiveCertRequest = "cert-request" // Payload = the DNS name; the node generates a keypair +
	//                        CSR (the private key stays home — [[cloud-issues-certs-not-node]]) and
	//                        uploads the CSR on its next report (ReportRequest.CSR). The cloud signs
	//                        it (DNS-01) and returns the cert via DirectiveCert.
	DirectiveAgentUpdate = "agent-update" // Payload = a JSON AgentUpdate; the node fetches the signed
	//                        Host-agent binary, verifies it against its release keyring, and — only on
	//                        a valid signature — stages+arms it for the Type=notify trial. A
	//                        bad/absent signature is refused, current kept.
	DirectiveServiceInstall = "service-install" // Payload = a CATALOG NAME (a slug, e.g.
	//                        "home-assistant"). The node fetches that name's signed manifest from the
	//                        catalog, verifies it against the release keyring, renders quadlet
	//                        units, provisions the service subvolume (Primary only), and rewrites the
	//                        promoter chain inside the maintenance bracket — health-gated, with
	//                        the chain reverted on failure. Deliberately a NAME and not the
	//                        manifest itself: shipping the document down this channel would make the
	//                        directive a second delivery path for signed content, and the node would
	//                        then be trusting the sender rather than the signature. Sent by the cloud,
	//                        or injected locally by `briard service install` over the admin socket
	//                        — both land in the same dispatch.
	DirectiveServicePrewarm = "service-prewarm" // Payload = a CATALOG NAME, as service-install. Renders
	//                        The quadlet units and pulls the images WITHOUT touching the promoter chain,
	//                        the volume, or what is currently serving -- the warm-standby half, made a
	//                        named operation.
	//
	//                        WHY IT IS ITS OWN KIND rather than "service-install on a node that happens
	//                        not to be Primary", which is what a secondary already did.
	//
	//                        THE REASON THAT MATTERS IS SAFETY, not tidiness. That design inferred the
	//                        PHASE from the ROLE, and roles are not ours to control: drbd-reactor
	//                        promotes on quorum events. So a directive enqueued to a secondary meaning
	//                        "download this" would, if that node promoted between enqueue and
	//                        execution, silently become "provision the volume, rewrite the promoter
	//                        chain, health-gate it" -- an EXECUTE the sender never asked for, landing
	//                        on a node in the middle of a failover. A download and an execute are
	//                        different operations and must not be the same message read twice.
	//
	//                        Second, and it is why the Primary gets one too: warming every anchor in
	//                        PARALLEL. The pull is the expensive step and it is the same pull
	//                        everywhere. Serialised behind the Primary's own install it costs two
	//                        pulls end to end; stated as a phase it costs one, and the Primary's later
	//                        install finds the image already local and warms in a no-op.
	//
	//                        Idempotent and safe to re-send: rendering is a file write, and pulling a
	//                        digest already present is a no-op. It never changes what the node serves,
	//                        which is what lets the cloud fan it out to every anchor before committing
	//                        anything.
	DirectiveRescue = "rescue" // Payload = "" — rebuild this node's guest from the verified image
	//                        under its OS-disk overlay: stop the VM, discard the overlay, lay down
	//                        a fresh one on the same backing file, bring it up. The REPLICATED DATA
	//                        DISK is not touched, so identity, replica and service pin all survive —
	//                        the same node returns with a factory code half.
	//
	//                        THE ONE DIRECTIVE THAT IS NEVER A REFLEX (B.10). Every other recovery
	//                        rung fires on its own; this waits for a human — or, later, a cloud that
	//                        has read the logs and decided. It is drastic, its result is uncertain,
	//                        and the rebuilt guest must re-pull its OCI images over the WAN at
	//                        exactly the moment something is already wrong. Refused outright on a
	//                        disk with no backing image: that is not an overlay, it is the only copy
	//                        of itself.
	DirectivePair = "pair" // Payload = a JSON MeshSpec: reconcile this node's DRBD to a target mesh
	//                        (runtime anchor pairing). The serving primary adjusts in place
	//                        (keeps its data); a blank joiner (anchor/witness) brings up and resyncs
	//                        as SyncTarget. The cloud computes the mesh and is the authorized sender
	//                       ; a stub injects it in the test. Blank-join only — re-homing
	//                        an already-seeded island is cloud-composed.
	DirectiveHandover = "handover" // Payload = "" (plain), "keep-masked", or "unmask". Give this node's
	//                        Work to a peer -- a PLANNED failover, which is what lets a
	//                        healthy node reboot into a new generation while its peer serves the house.
	//                        Runs drbd-reactorctl's own `evict`; see the guest verb for why that rather
	//                        than a demote dance.
	//
	//                        IT SAYS "NOT ME", NOT "YOU": the destination is drbd-reactor's election.
	//                        The sender must therefore verify where the work LANDED rather than assume
	//                        it went where intended -- deterministic only because our shipped topology
	//                        has exactly one other node that can hold the resource.
	//
	//                        `keep-masked` leaves the node ineligible to take the work back, which the
	//                        reboot path needs so an unverified generation cannot reclaim the house on
	//                        the way up; `unmask` releases that and evicts nothing. Sent by the cloud
	//                        when rolling a pair, or injected locally by `briard handover`
	//                        -- both land in the same dispatch.
	DirectiveSync = "sync" // Payload empty. Flush the node's replicated data volume (dirty page
	//                        cache -> both disks), so a handover issued moments later unmounts a
	//                        volume that owes almost nothing -- the pre-copy half of the eviction
	//                        discipline ([B.100a]). The sequencer sends it to the node it is about
	//                        to evict BEFORE its settle window, so the flush's own replication
	//                        burst lands while the fleet is still under observation rather than
	//                        inside the demote path. Node-local and idempotent; a node with
	//                        nothing mounted (a Secondary, a witness) reports done("skipped"),
	//                        which is an answer, not a failure.
)

// CertBundle is a renewed cert the controller pushes down (JSON-encoded into a DirectiveCert's
// Payload). Cert-only: the node generated the keypair and holds the private key, so
// only the signed certificate crosses the wire ([[cloud-issues-certs-not-node]] — the key never
// leaves home). The cloud/controller schedules + issues (it holds the DNS token + does DNS-01);
// the node pairs this cert with its stashed key and applies both to the replicated volume.
type CertBundle struct {
	Name string `json:"name"`
	Cert string `json:"cert"` // full chain PEM
}

// AgentUpdate is the payload of a DirectiveAgentUpdate: a signed host-agent binary the
// cloud offers a node for self-update. The node fetches URL, verifies the detached signature
// Sig against its release keyring (Ed25519), and stages+arms it for the Type=notify trial
// ONLY on a valid signature — a bad or absent signature is refused and the running binary is
// kept ([[cloud-issues-certs-not-node]]'s sibling principle: the node validates, never trusts
// the transport). Version is the offered release, echoed in NodeStatus.AgentVersion once it
// commits, so the cloud can tell a canary converged before it offers the fleet.
type AgentUpdate struct {
	Version string `json:"version"` // the offered release id (idempotency + convergence signal)
	URL     string `json:"url"`     // where to fetch the raw binary artifact
	Sig     string `json:"sig"`     // base64 detached Ed25519 signature over the exact artifact bytes
}

// MeshPeer is one node's placement in a DRBD pairing mesh. The sender (the cloud, or a pairing test) computes the whole mesh and sends the same peer set to every node; each renders it
// into its DRBD .res. Disk empty marks the diskless witness (the quorum tiebreaker).
type MeshPeer struct {
	Name    string `json:"name"`
	NodeID  int    `json:"node_id"`
	Address string `json:"address"`        // ip:port on the replication subnet
	Disk    string `json:"disk,omitempty"` // "" = diskless witness
}

// MeshSpec is a DirectivePair payload: the target DRBD mesh plus how THIS recipient joins it.
// Join selects the side of the pairing: false = the existing serving primary (adjust the running
// resource in place, keep its data), true = a blank joiner (bring up fresh + resync as SyncTarget;
// blank-join only, never merging existing data). SystemDev/SystemCIDR give this node its
// replication-subnet NIC address — a single node replicated over loopback, so pairing first needs
// a routable address DRBD can bind/connect on. The sender fills SystemCIDR per recipient.
type MeshSpec struct {
	Resource   string       `json:"resource"`
	Device     string       `json:"device"`
	Peers      []MeshPeer   `json:"peers"`
	Join       bool         `json:"join"`
	SystemDev  string       `json:"system_dev,omitempty"`
	SystemCIDR string       `json:"system_cidr,omitempty"`
	Witness    *MeshWitness `json:"witness,omitempty"`
}

// MeshWitness carries the host-side cloud-witness forwarder wiring for a managed pairing.
// Non-nil only when the mesh's diskless voter is the forwarded cloud witness, reached through
// each anchor's OWN host witness-forwarder over a private guest↔host link ([[cloud-witness-v3-1d]],
// [[logic-on-host-by-default]]). The recipient anchor uses it to (a) render the explicit-connection
// .res (LocalAddr → drbd.Peer.WitnessLocal; the diskless peer's mesh Address is the host forwarder
// the guest's DRBD dials), (b) address its guest witness NIC (Dev/CIDR = eth3), and (c) start its
// host witness-forwarder (tunnelling that link to Target over mTLS with the host-held anchor cert).
// The private-link numbering is fleet-constant, so this block is identical on every anchor. Nil for
// single-node, LAN-witness, and blank-join meshes — those render as a plain connection-mesh.
type MeshWitness struct {
	Dev        string `json:"dev"`         // guest witness NIC, e.g. "eth3"
	CIDR       string `json:"cidr"`        // its address on the private link, e.g. "10.9.9.2/24"
	LocalAddr  string `json:"local_addr"`  // the anchor's DRBD witness-side local "ip:port", e.g. "10.9.9.2:7789"
	Target     string `json:"target"`      // the cloud witness-proxy address the host forwarder tunnels to
	ServerName string `json:"server_name"` // expected SAN on the witness-proxy server cert
}

// The north-bound control seam. A node POSTs its status up and gets any
// pending directives back in the same response -- poll-based + node-initiated, so it
// works from behind NAT (the v2-cloud shape). Versioned paths so node<->controller
// can't silently drift.
const (
	RegisterPath  = "/v1/register"  // node -> cloud: NodeInfo up, Assignment (tenant/role) back
	ReportPath    = "/v1/report"    // node -> controller: status up, directives down
	ClusterPath   = "/v1/cluster"   // read the aggregate fleet view + verdict
	DirectivePath = "/v1/directive" // admin: enqueue a directive for a node
	HistoryPath   = "/v1/history"   // read a node's event history: ?node=<name>&after=<seq>
	MetricsPath   = "/v1/metrics"   // POST: node uploads hourly aggregates; GET: read ?node=&metric=&since=
	DiagPath      = "/v1/diag"      // read the controller's self-diagnostics (footprint + store size)
	ReadyzPath    = "/readyz"       // deploy readiness: 200+serving when the schema matches this binary, else 503
)

// Readyz is a controller instance's deploy-readiness self-report: the /readyz body,
// also the 503 body the gate returns while not serving. State is "serving" (200),
// "starting" (503 -- store unreachable, or the migration hasn't run yet), or "superseded"
// (503 -- a newer instance migrated the DB past this binary; forward-only, so it stands down
// to be retired). Detail is a human-readable reason. The deploy gates cutover on State ==
// serving; a node/UI that gets a 503 simply retries (the report loop already does).
type Readyz struct {
	State  string `json:"state"`
	Detail string `json:"detail,omitempty"`
}

// ReportRequest is a node's periodic status report. CSR carries a DER-encoded certificate
// signing request when the node is responding to a DirectiveCertRequest: the node
// generated the keypair at home and uploads only the CSR for the cloud to sign. Empty on
// every other report.
type ReportRequest struct {
	Status   NodeStatus         `json:"status"`
	CSR      []byte             `json:"csr,omitempty"`
	Outcomes []DirectiveOutcome `json:"outcomes,omitempty"` // terminal results of prior directives
}

// ReportResponse carries any directives queued for the reporting node.
type ReportResponse struct {
	Directives []Directive `json:"directives,omitempty"`
}

// EnqueueRequest queues a directive for a node (admin, DirectivePath).
type EnqueueRequest struct {
	Node      string    `json:"node"`
	Directive Directive `json:"directive"`
}

// Fleet health verdicts (the controller's one-word answer; mirrors fleet-status.sh).
const (
	VerdictHealthy    = "HEALTHY"
	VerdictDegraded   = "DEGRADED"
	VerdictOutage     = "OUTAGE"
	VerdictSplitBrain = "SPLIT-BRAIN"
)

// NodeReport is a node's latest status plus how stale it is.
type NodeReport struct {
	Status NodeStatus `json:"status"`
	AgeSec int        `json:"age_sec"` // since last report; large => not reporting
	Down   bool       `json:"down"`    // never reported, or stale past the threshold
}

// ClusterView is the controller's aggregate answer (ClusterPath): the fleet a v2 cloud
// would render, plus a one-word verdict and the reasons behind a non-healthy one.
type ClusterView struct {
	Verdict string       `json:"verdict"`
	Reasons []string     `json:"reasons,omitempty"`
	Nodes   []NodeReport `json:"nodes"`
}

// Event is one entry in a node's transition/alert history, the read-side mirror of
// the persisted store event -- like NodeReport mirrors NodeStatus. Seq is the per-node
// monotonic cursor (pass the last seen as `after` to page forward). Tenant is not on the wire
// (single-tenant, and the controller keys on its own tenant, not a client-supplied one).
type Event struct {
	NodeName   string    `json:"node_name"`
	Seq        int64     `json:"seq"`
	ID         string    `json:"id"`     // node-supplied idempotency key (stable across re-delivery)
	Kind       string    `json:"kind"`   // the closed transition taxonomy: health/failover/image/system/role
	Detail     string    `json:"detail"` // human-readable "was → now"
	OccurredAt time.Time `json:"occurred_at"`
	RecordedAt time.Time `json:"recorded_at"`
}

// HistoryResponse carries a page of a node's events in Seq order (HistoryPath). Advance by
// passing the last Seq as the next request's `after`.
type HistoryResponse struct {
	Events []Event `json:"events"`
}

// HealthReport is an aggregate health signal (placeholder).
type HealthReport struct {
	NodeName string
	OK       bool
}

// ClusterState is the cloud's view of a tenant's nodes (placeholder).
type ClusterState struct {
	Tenant string
	Nodes  []NodeStatus
}
