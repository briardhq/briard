// Package deadman is the guest-side host-agent deadman: the guest's
// fail-safe reflex when its host agent goes silent. Every host control call refreshes a
// last-seen-agent stamp; after T_deadman with no contact the guest reboots — a graceful reboot
// whose promoter teardown demotes cleanly, so the reboot IS the failover trigger — but ONLY when
// RebootAllowed says going down cannot cost anyone ELSE their quorum.
//
// The guest goes FIRST, deliberately. The host holds the same reflex from outside (host/
// guestrecover.go) on a longer window, so the graceful in-guest reboot gets its chance before the
// host resorts to power-cycling the VM. Ordering it the other way would spend a clean demote —
// and the DRBD metadata that comes with it — on every incident where the kernel was still fine
// and only the agent had wedged.
//
// The whole decision — reboot vs. hold vs. serve — lives here, dependency-free and exhaustively
// unit-tested. The driver (deadman.go) supplies the live inputs (the stamp, the DRBD fabric, the
// wall clock, persisted backoff state), carries out the action, and serves the gate's answer to
// the host over the private link.
package deadman

import (
	"hash/fnv"
	"time"
)

// Action is the deadman's verdict for one evaluation tick.
type Action int

const (
	// Serve — the host-agent link is alive (or just returned): normal operation, and the
	// degraded episode (attempt count / backoff) resets.
	Serve Action = iota
	// Hold — the deadman has fired, but do NOT reboot: either rebooting would tip the cluster
	// below quorum (a quorum-critical node — that would outage a peer that is still serving), or
	// we are still within the cadence since the last reboot. Keep serving.
	Hold
	// Reboot — graceful reboot now: the deadman fired, RebootAllowed says nobody else loses
	// quorum over it (a peer takes over, or a lone node simply resumes on the way back), and the
	// cadence has elapsed.
	Reboot
)

func (a Action) String() string {
	switch a {
	case Serve:
		return "serve"
	case Hold:
		return "hold"
	case Reboot:
		return "reboot"
	default:
		return "unknown"
	}
}

// Input is everything Decide needs — all of it observable guest-locally, so the deadman works
// precisely when the host agent (its normal information source) is gone.
type Input struct {
	SinceContact time.Duration // now − last host-agent contact
	Deadman      time.Duration // the effective (jittered) T_deadman for this node this attempt
	Allowed      bool          // RebootAllowed(Fabric): rebooting me will not outage anyone else
	SinceReboot  time.Duration // now − last deadman reboot this episode (very large if none yet)
	Attempt      int           // deadman reboots so far this degraded episode (drives the backoff)
}

// Decide is the entire decision:
//
//	link alive                           → Serve
//	deadman fired, NOT allowed           → Hold   (quorum-critical: never outage a surviving peer)
//	deadman fired, allowed, backing off  → Hold   (wait out the inter-reboot cadence)
//	deadman fired, allowed, cadence done → Reboot
//
// The load-bearing property: Reboot is returned only when RebootAllowed, so the reflex can never
// be what drops a cluster below quorum. Note it is no longer "never causes an outage" — a lone
// node reboots itself now, which is a brief outage of a node that had already lost its agent and
// would otherwise stay wedged forever (see RebootAllowed).
func Decide(in Input) Action {
	if in.SinceContact < in.Deadman {
		return Serve // the agent is (still / again) talking to us
	}
	if !in.Allowed {
		return Hold // rebooting would tip the cluster below quorum — wait + alert, keep serving
	}
	if in.SinceReboot < Backoff(in.Attempt) {
		return Hold // allowed, but too soon since the last reboot — back off (still serving)
	}
	return Reboot
}

// RebootCadence is the interval between successive reboots once the first one has failed to fix
// anything. It never becomes "stop": a persistently degraded node keeps gently retrying, which is
// both a standing chance that whatever wedged it clears and a visible sign of degradation for a
// remote fix to catch.
//
// It is FLAT rather than an exponential ramp, and that is a consequence of single-node nodes now
// rebooting at all (RebootAllowed). A ramp starting at 30s is fine when a peer is carrying the
// workload; on a lone node every attempt is a real service interruption, and a handful of them in
// the first few minutes is a worse experience than the wedge. One immediate attempt — which fixes
// the transient case — and then a slow, steady two hours.
const RebootCadence = 2 * time.Hour

// Backoff is the minimum interval between successive deadman reboots in one degraded episode.
// Attempt 0 (the first of an episode) fires as soon as the deadman does; every later attempt
// waits RebootCadence.
func Backoff(attempt int) time.Duration {
	if attempt <= 0 {
		return 0 // first reboot: no extra delay beyond T_deadman itself
	}
	return RebootCadence
}

// Timings tunable at construction; the self-update invariant binds them to the self-update pivot.
const (
	// DefaultDeadman is the base T_deadman: the minimum silence before the deadman fires. It sits
	// comfortably above the self-update window (see the invariant below) so a routine agent
	// restart / update trial never trips it.
	DefaultDeadman = 6 * time.Minute
	// DefaultJitter is the per-node spread window added on top of the base, so a correlated agent
	// outage staggers reboots across the fleet instead of firing them in lockstep.
	DefaultJitter = 60 * time.Second

	// SelfUpdateWindow is the worst-case host-agent self-update trial before the pivot's
	// Type=notify TimeoutStartSec (≥180s) reverts it, plus the revert + re-adopt
	// margin. The INVARIANT (asserted in the tests): DefaultDeadman > SelfUpdateWindow — so a trial
	// always converges or auto-reverts before the deadman can fire. Threshold-only; no protocol.
	SelfUpdateWindow = 180*time.Second + 60*time.Second
)

// EffectiveDeadman spreads base by up to window, deterministically from seed (no RNG, so it
// survives a reboot and is reproducible/debuggable). The driver seeds it with the node name (a
// stable per-node offset) folded with the attempt (so retries decorrelate across the fleet too,
// not just the first fire). The minimum returned value is base itself, so the invariant on
// base still holds after jitter.
func EffectiveDeadman(base, window time.Duration, seed string) time.Duration {
	if window <= 0 {
		return base
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(seed))
	return base + time.Duration(h.Sum64()%uint64(window))
}

// Fabric is this node's view of the replication cluster, and the sole input to the reboot gate.
// Read guest-locally (drbdsetup), so the gate answers precisely when the host agent — the normal
// source of everything — is gone.
type Fabric struct {
	Peers     int  // peers CONFIGURED for this resource, connected or not (self excluded)
	Connected int  // how many of those are currently Connected
	Quorate   bool // this node has quorum right now, i.e. DRBD lets it write
}

// RebootAllowed is THE gate, and the same one on both rungs of the ladder: the deadman calls it
// to decide whether to reboot itself, and the host reads its answer over the private link to
// decide whether to power-cycle a guest that has stopped answering. One definition, so the two
// rungs cannot drift into disagreeing about when a reboot is safe.
//
// It is deliberately NOT "am I doing useful work". A guest that has lost its host agent is
// degraded BY POLICY — most operations run through the host, the link is otherwise stable, and
// the alternative is reasoning forever about the availability of no-agent scenarios. So serving
// is never a reason for a node to stay up; it is only ever a reason not to take someone ELSE
// down. Hence the three clauses below are three different arguments, not one formula:
//
//	SINGLE NODE — there is no cluster to disrupt. Quorum is guaranteed the moment it comes back
//	(it is the only voter), the data is on its own disk, and a one-node install makes no
//	high-availability promise to break. This is the common shape of a briard install, and
//	deriving it from the majority formula instead of stating it left the reflex INERT on most
//	of the fleet: such a node held, alerted once, and then sat wedged and silent forever.
//
//	ALREADY FAILED — this node has no quorum, so DRBD has already stopped it writing and it is
//	serving nobody. Nothing is preserved by staying up, and a reboot is free and occasionally
//	curative — the partitioned-anchor case (WAN out, cloud witness unreachable).
//
//	SURVIVES WITHOUT ME — the peers I can still see form a majority on their own, so my
//	departure leaves the cluster quorate and a peer takes the workload. This is the original
//	gate and the case it was built for: a 3-voter cluster with one member already down, where a
//	second self-removal would drop the survivor below majority and outage the house.
//
// Connected counts peers connected TO ME; peers connected to each other but partitioned from me
// do not count. Conservative in the right direction — it can only withhold a reboot.
func RebootAllowed(f Fabric) bool {
	if f.Peers == 0 {
		return true // single node: no peer to disrupt, quorum guaranteed on the way back
	}
	if !f.Quorate {
		return true // already outside quorum: serving nobody, so there is nothing to protect
	}
	majority := (f.Peers+1)/2 + 1
	return f.Connected >= majority
}
