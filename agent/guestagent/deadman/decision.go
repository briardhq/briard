// Package deadman is the guest-side host-agent deadman: the guest's
// fail-safe reflex when its host agent goes silent. Every host control call refreshes a
// last-seen-agent stamp; after T_deadman with no contact the guest reboots — a graceful reboot
// whose promoter teardown demotes cleanly, so the reboot IS the failover trigger — but ONLY when
// the cluster keeps quorum without it, so a deadman reboot can never cause a serving outage.
//
// The whole decision — reboot vs. hold vs. serve — lives here, dependency-free and exhaustively
// unit-tested. The driver (deadman.go) supplies the live inputs (the stamp, DRBD quorum, the wall
// clock, persisted backoff state) and carries out the action.
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
	// below quorum (a quorum-critical node, incl. a lone home — that would outage a serving node
	// for nothing), or we are still within the backoff window since the last reboot. Keep serving.
	Hold
	// Reboot — graceful reboot now: the deadman fired, the cluster keeps quorum without this node
	// (so a peer takes over, or a lone node resumes on the way back), and the backoff has elapsed.
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
	QuorumSafe   bool          // the cluster keeps quorum if THIS node departs (a reboot won't outage a peer)
	SinceReboot  time.Duration // now − last deadman reboot this episode (very large if none yet)
	Attempt      int           // deadman reboots so far this degraded episode (drives the backoff)
}

// Decide is the entire gate:
//
//	link alive                              → Serve
//	deadman fired, NOT quorum-safe          → Hold   (quorum-critical / lone node: never self-outage)
//	deadman fired, quorum-safe, backing off → Hold   (wait out the inter-reboot backoff; keep serving)
//	deadman fired, quorum-safe, backoff done → Reboot
//
// The load-bearing property: Reboot is returned only when QuorumSafe, so a deadman reboot can
// never drop a serving node — the reflex cannot cause an outage.
func Decide(in Input) Action {
	if in.SinceContact < in.Deadman {
		return Serve // the agent is (still / again) talking to us
	}
	if !in.QuorumSafe {
		return Hold // rebooting would tip the cluster below quorum — wait + alert, keep serving
	}
	if in.SinceReboot < Backoff(in.Attempt) {
		return Hold // quorum-safe, but too soon since the last reboot — back off (still serving)
	}
	return Reboot
}

// BackoffBase / backoffCap bound the inter-reboot cadence: a short first interval to escape a
// transient wedge, exponential growth, then a slow perpetual cap. It never returns "stop" — a
// persistently-degraded (quorum-safe) node keeps gently retrying (useful for a remote fix to
// catch, and a visible sign of degradation), because a quorum-gated reboot is never harmful.
const (
	backoffBase = 30 * time.Second
	backoffCap  = 2 * time.Hour
)

// Backoff is the minimum interval between successive deadman reboots in one degraded episode.
// Attempt 0 (the first reboot of an episode) fires as soon as the deadman does; later attempts
// grow 30s, 1m, 2m, 4m, … capped at backoffCap.
func Backoff(attempt int) time.Duration {
	if attempt <= 0 {
		return 0 // first reboot: no extra delay beyond T_deadman itself
	}
	shift := attempt - 1
	if shift > 40 { // guard the shift from overflowing int64 before the cap catches it
		return backoffCap
	}
	d := backoffBase << uint(shift)
	if d <= 0 || d > backoffCap { // <=0 catches any overflow
		return backoffCap
	}
	return d
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

// QuorumSafe reports whether the cluster keeps quorum if this node departs — the whole reboot
// gate. total is the configured cluster vote count (peers + self); connected is how many peers
// are currently connected (would remain if this node left). Safe ⇔ the remaining connected peers
// still form a majority. A lone node (total 1, connected 0) is never safe (it IS the quorum) → it
// holds/keeps serving rather than self-outaging.
func QuorumSafe(total, connected int) bool {
	if total <= 1 {
		return false // single-node: departing loses quorum; never reboot a lone serving node
	}
	majority := total/2 + 1
	return connected >= majority
}
