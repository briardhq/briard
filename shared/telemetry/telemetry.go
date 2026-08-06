// Package telemetry holds the node's resource-footprint telemetry -- the numeric input to
// the soak's *trend* oracle. It lives outside shared/api deliberately: this
// data does NOT cross the host↔cloud wire (shared/api is the closed, audited allowlist of
// what leaves the house -- privacy-as-schema). It rides only the internal
// paths: the host↔guest control channel (the guest returns it to the host) and the
// host→lab collector file the soak reads out-of-band. Removing it from shared/api is the
// point -- the cloud stops receiving agent/payload RSS+FDs, load, log/store sizes, guest
// kernel errors. A product-health subset (volume used/free, crash-loop restarts) is re-added
// to shared/api *deliberately* if/when the cloud consumes it.
package telemetry

// NodeResources is the resource telemetry a node measures each observe cycle -- the
// numeric input to the soak's *trend* oracle: a leak or unbounded growth shows as
// a rising post-warmup slope or a crossed ceiling across a run, on a build aged this way. It
// is a *signal*, never a gate -- every field is best-effort (0 means unread this cycle, never
// a hard failure of the observe loop).
//
// Two scopes in one struct: the Agent* fields are the host-side product daemon's own
// footprint (measured in-process via /proc/self on every node, witness included -- the "does
// the agent leak over days" surface); the rest are the appliance (guest), read via the
// sys.resources verb (zero on a witness / payload-less node, which has no payload or volume).
type NodeResources struct {
	AgentRSSKB int64 `json:"agent_rss_kb,omitempty"` // host agent (product daemon) resident set
	AgentFDs   int   `json:"agent_fds,omitempty"`    // host agent open file descriptors

	PayloadRSSKB  int64   `json:"payload_rss_kb,omitempty"`  // payload container resident set
	PayloadFDs    int     `json:"payload_fds,omitempty"`     // payload open fds (leaks earlier than RSS)
	Load1         float64 `json:"load1,omitempty"`           // 1-min load average = cpu-at-rest
	VolumeUsedKB  int64   `json:"volume_used_kb,omitempty"`  // used space on the DRBD data volume
	SnapshotCount int     `json:"snapshot_count,omitempty"`  // btrfs snapshots on the volume (GC bound)
	LogSizeKB     int64   `json:"log_size_kb,omitempty"`     // systemd journal on-disk size
	PodmanStoreKB int64   `json:"podman_store_kb,omitempty"` // container/code store size

	// PayloadRestarts + KernelErrors are guest-internal soak signals (no-silent-restarts /
	// no-bad-kernel-log) that ride the host↔guest channel because the L2 guest is reachable
	// only over it -- not machinectl/journalctl, which hit the L1 container and its shared
	// host kernel. The guest reports mechanism (the payload unit's NRestarts; recent warning+
	// kernel lines, normally empty); the oracle applies policy (a climb; a bad-pattern scan).
	// Complements ShellSubstrate, which covers the host/L1 layer (qemu OOM, KVM).
	PayloadRestarts uint64   `json:"payload_restarts,omitempty"` // payload unit NRestarts (crash-loop)
	KernelErrors    []string `json:"kernel_errors,omitempty"`    // recent guest kernel warning+ lines
}
