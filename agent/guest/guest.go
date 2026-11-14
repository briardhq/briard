package guest

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"briard.io/agent/guestagent"
	"briard.io/shared/model"
)

// GuestManager is the host↔guest boundary. The guest is the VM that carries the apps; this
// seam starts/stops/health-checks an app and reads the guest's code identity (the NixOS system
// closure).
//
// Scope is asymmetric on purpose: app lifecycle and health are per-app (contain the data
// blast radius); the code half (SystemPath) is whole-VM. The OS upgrade moves neither
// app nor data: it is a property of the node, and it leaves the workload alone.
//
// NO SNAPSHOT/RESTORE HERE. The app-install sequence (agent/host/app.go) moves manifest and
// data back together, driving the guest agent's data.member/data.restore verbs directly, pinned
// to the manifest rather than to a system closure.
type GuestManager interface {
	Start(ctx context.Context, spec model.AppSpec) error
	Stop(ctx context.Context, spec model.AppSpec) error
	Health(ctx context.Context, spec model.AppSpec) (Health, error)
	SystemPath(ctx context.Context) (string, error) // current code identity (closure store path)
}

// Health is the guest's health-gate signal.
type Health struct {
	Running bool // the app's unit is active
	Ready   bool // passed its /healthz
}

// ReadinessAssessor is an optional *differential* health signal layered above the
// HTTP-200 liveness floor. The floor answers "is the app answering?"; an assessor answers
// "did the upgrade break something that worked before?" — app-specific (HA config-entry
// state), so it stays behind this interface and this package keeps no app knowledge. nil =
// the floor alone, which is what every app without an assessor gets (agent/host/readiness.go
// picks them).
//
// The Gate captures a Baseline before the change (old version still serving), then after
// the floor reports ready calls Assess to settle and judge the post-upgrade signal. A
// Rollback verdict trips the {code+data} rollback; Hold keeps the upgrade but surfaces
// it (the rollback window and the user are the backstop); Pass keeps it silently. A failure
// to capture or assess degrades to the floor alone — S1 never blocks or reverts an upgrade
// because its own telemetry failed (the safe direction).
type ReadinessAssessor interface {
	// Baseline captures the pre-upgrade steady-state signal, returning an opaque token
	// handed back to Assess. Called while the old version still serves.
	Baseline(ctx context.Context) (Baseline, error)
	// Assess settles the post-upgrade signal and judges it against base. Called only
	// after the liveness floor reports ready.
	Assess(ctx context.Context, base Baseline) (Verdict, string, error)
}

// Baseline is an assessor's opaque pre-upgrade sample, carried from Baseline to Assess.
type Baseline any

// Verdict is a ReadinessAssessor's decision, mapped from the app-specific gate (e.g.
// entrygate.Verdict) so the Manager stays app-agnostic.
type Verdict string

const (
	VerdictPass     Verdict = "pass"
	VerdictHold     Verdict = "hold"
	VerdictRollback Verdict = "rollback"
)

// control is the guest-side surface Manager drives — satisfied by *guestagent.Client
// (asserted below), faked in tests.
type control interface {
	AppStart(ctx context.Context, unit string) error
	AppStop(ctx context.Context, unit string) error
	AppActive(ctx context.Context, unit string) (bool, error)
	AppHealth(ctx context.Context, url string) (bool, error)
	VIP(ctx context.Context, dev string) (string, error)
	SystemPath(ctx context.Context) (string, error)
	WriteCert(ctx context.Context, cert, key string) error
	ReactorPause(ctx context.Context, snippet string) error
	ReactorResume(ctx context.Context, snippet string) error
	Cluster(ctx context.Context, resource string) (model.Cluster, error)
}

var _ control = (*guestagent.Client)(nil)

// Config carries Manager policy not derivable from a AppSpec.
type Config struct {
	// HealthURL is the CONFIGURED probe target (the reverse proxy's /healthz at the VIP). Set,
	// it is what is probed; unset, the VIP is acquired by DHCP inside the guest, only the guest
	// knows the address, and ResolveHealthURL asks it every cycle. "" is NOT "no probe" — see
	// Diskless.
	HealthURL string
	// VIPDev is the guest NIC the VIP lives on, when the agent named it (VIP_DEV). Only a
	// device WE named is safe to read an address back from, and unset means this node claims no
	// VIP at all -- so there is no address to resolve and nothing to probe. Never guess a
	// device: in the agent-less harnesses eth1 also carries the DRBD address, so "the first
	// address on eth1" would be the replication link rather than the VIP.
	VIPDev string
	// Diskless marks a witness: no apps, no VIP, nothing to probe — its health follows
	// quorum. This is the ONLY thing that means "never probe". Emptiness cannot carry that
	// meaning, because a data node acquiring its address by DHCP has no configured URL
	// either, and the two must not answer alike: a witness with no probe is healthy when
	// quorate, a data node with no address is not ready.
	Diskless bool
	// Resource is the DRBD resource this node serves — what OSReady reads its own role and
	// disk state from. Empty means the OS gate has no cluster to consult, which it
	// treats as "nothing node-local to certify" rather than as a failure.
	Resource string
	// HTTPClient probes HealthURL; nil defaults to a short-timeout client.
	HTTPClient *http.Client
	// gateInterval is how often await re-checks while waiting for ready; the caller bounds
	// the total wait via its context. Zero defaults.
	gateInterval time.Duration
	// healthPollTimeout bounds each individual poll in await. The poll runs on a context
	// *detached* from the caller's deadline so that deadline can never fire inside a
	// control-channel call and close the channel mid-call — which would break the rollback
	// that reuses it. This caps the detached poll so a wedged guest can't stall it. Zero
	// defaults.
	healthPollTimeout time.Duration
	// ReactorSnippet enables the maintenance bracket: EnterMaintenance pauses the promoter and
	// ExitMaintenance resumes it, around an OS upgrade (agent/host/osupgrade.go). The guest
	// stops the whole drbd-reactor daemon and does not read the snippet, so only set vs empty
	// matters. Empty = no promoter coordination (unit tests / non-promoter payloads).
	ReactorSnippet string
	// Logf is defaulted to a no-op by NewManager; nothing in Manager logs through it.
	Logf func(format string, args ...any)
}

// Manager is the real GuestManager: it maps the seam onto guest control-channel
// verbs (guestagent) plus a host-side readiness probe.
type Manager struct {
	ctl control
	cfg Config
}

// NewManager wires a Manager to the guest control channel.
func NewManager(ctl control, cfg Config) *Manager {
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 5 * time.Second}
	}
	if cfg.gateInterval == 0 {
		cfg.gateInterval = 2 * time.Second
	}
	if cfg.healthPollTimeout == 0 {
		cfg.healthPollTimeout = 15 * time.Second // bound each detached health poll (> the 5s probe)
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	return &Manager{ctl: ctl, cfg: cfg}
}

// unitOf is the systemd unit that answers "is this app up?". The one definition is on the spec
// (model.AppSpec.ServingUnit); this name only reads better at the call sites below.
func unitOf(spec model.AppSpec) string { return spec.ServingUnit() }

// hasApp reports whether this node runs a workload at all. ZERO APPS IS THE SHIPPED
// STATE -- a fresh node serves the reverse proxy's own landing page and nothing else --
// so every app-shaped step has to be a no-op there rather than an error. The zero spec names
// no unit, and asking systemd about one anyway reads "inactive": a health gate stuck on "not
// ready" forever on a node whose own Healthy (cfg.snapshot, the reverse proxy alone) is true.
func hasApp(spec model.AppSpec) bool { return spec.Name != "" || spec.Unit != "" }

func (m *Manager) Start(ctx context.Context, spec model.AppSpec) error {
	if !hasApp(spec) {
		return nil // nothing to raise; the reverse proxy rides briard-vip, not this call
	}
	return m.ctl.AppStart(ctx, unitOf(spec))
}

func (m *Manager) Stop(ctx context.Context, spec model.AppSpec) error {
	if !hasApp(spec) {
		return nil // nothing to quiesce
	}
	return m.ctl.AppStop(ctx, unitOf(spec))
}

// Health is the upgrade gate's floor. With an app: the app unit is active AND the probe
// passes. With NO app the probe alone decides, because the reverse proxy answers /healthz
// itself in that state -- 200, "ok: reverse proxy up, no apps routed" -- so a node with
// nothing installed is ready, not sick. Running is vacuously true there: there is no app that
// could be down.
func (m *Manager) Health(ctx context.Context, spec model.AppSpec) (Health, error) {
	if !hasApp(spec) {
		return Health{Running: true, Ready: m.probeReady(ctx)}, nil
	}
	running, err := m.ctl.AppActive(ctx, unitOf(spec))
	if err != nil {
		return Health{}, err
	}
	ready := running && m.probeReady(ctx)
	return Health{Running: running, Ready: ready}, nil
}

func (m *Manager) SystemPath(ctx context.Context) (string, error) { return m.ctl.SystemPath(ctx) }

// WriteCert lands a renewed cert/key on the guest's DRBD volume: the reverse proxy
// hot-reloads it, so a cloud-scheduled renewal applies with no restart, and it replicates
// so a failover serves the same cert.
func (m *Manager) WriteCert(ctx context.Context, cert, key string) error {
	return m.ctl.WriteCert(ctx, cert, key)
}

// VIPReader is the one call health resolution needs from the guest: the address a device
// ACTUALLY holds. Satisfied by *guestagent.Client, and by the host's own guest reader — which
// is why it is exported: the readiness gate and the observe loop must resolve by the same
// rule, and one rule in two packages is two rules waiting to disagree.
type VIPReader interface {
	VIP(ctx context.Context, dev string) (string, error)
}

// ResolveHealthURL answers what to probe for node health, RIGHT NOW, in three cases:
//
//   - a witness (diskless) probes nothing; its health follows quorum. "" here means that and
//     only that, which is why the caller must pass the ROLE and never infer it from an
//     empty address.
//   - an address we configured is the address we probe. We set it on the guest, so reading it
//     back would tell us nothing new — but it would tell us something WRONG whenever the
//     device also holds a lease (dhcpcd still serves the service NIC), and the failure that
//     hides behind is the one the LAN-owned address exists to end: probing a node-local
//     address that answers after the VIP has moved away, i.e. healthy-while-not-serving.
//   - no configured address means the VIP came from DHCP inside the guest, so the guest is the
//     only one who knows it. Ask, every cycle — the lease can change and the VIP moves on
//     failover. "" (a Secondary, or a promotion in flight) is "nothing to probe yet", which
//     reads as not-ready rather than as healthy. A verb error answers the same "".
//
// No configured address and no VIPDev means this node claims no VIP: "" again.
func ResolveHealthURL(ctx context.Context, v VIPReader, diskless bool, vipDev, configured string) string {
	if diskless {
		return ""
	}
	if configured != "" || vipDev == "" {
		return configured
	}
	cidr, err := v.VIP(ctx, vipDev)
	if err != nil || cidr == "" {
		return configured
	}
	return healthURLAt(cidr)
}

// healthURLAt builds the probe target from an address in CIDR form. The port and path are
// fixed on purpose: the target is the REVERSE PROXY (:80 /healthz), which is what makes the probe
// stable across zero and one app — see HealthURL in agent/host/config.go.
func healthURLAt(cidr string) string {
	addr, _, _ := strings.Cut(cidr, "/")
	return "http://" + addr + "/healthz"
}

// healthURL resolves this node's probe target for one call. See ResolveHealthURL.
func (m *Manager) healthURL(ctx context.Context) string {
	return ResolveHealthURL(ctx, m.ctl, m.cfg.Diskless, m.cfg.VIPDev, m.cfg.HealthURL)
}

// probeReady reports whether the health endpoint answers 200. It prefers the in-guest
// probe (the app.health verb over the control channel) so the readiness gate survives a networking
// substrate where the host can't reach the VIP (macvtap); it falls back to a direct host-side GET
// only when that verb errors — a guest built before the verb, which is always tap-based, so
// the LAN GET still works there. Any error/non-200 is "not ready" — the health-gate treats a
// non-answering app as a rollback trigger.
//
// Nothing to probe is not ready either: a witness has no apps (its callers gate on role
// before asking), and a data node with no address yet is exactly the node nobody in the house
// can reach.
func (m *Manager) probeReady(ctx context.Context) bool {
	url := m.healthURL(ctx)
	if url == "" {
		return false
	}
	if ready, err := m.ctl.AppHealth(ctx, url); err == nil {
		return ready
	}
	return m.hostProbeReady(ctx, url)
}

// hostProbeReady is the host-side GET of the VIP, the fallback for a guest that predates the
// app.health verb.
func (m *Manager) hostProbeReady(ctx context.Context, url string) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false
	}
	resp, err := m.cfg.HTTPClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// The OS upgrade sequence is the host's (agent/host/osupgrade.go): the point it rolls back to is
// the OS image, which only the host holds. This package keeps the phases the guest owns --
// EnterMaintenance/ExitMaintenance and the health gates -- which the host calls in order rather
// than composing a second sequence.

// EnterMaintenance pauses the promoter (the guest stops drbd-reactor), if ReactorSnippet is set.
func (m *Manager) EnterMaintenance(ctx context.Context) error {
	if m.cfg.ReactorSnippet == "" {
		return nil
	}
	return m.ctl.ReactorPause(ctx, m.cfg.ReactorSnippet)
}

// ExitMaintenance resumes the promoter, on a detached context (recovery-critical: it
// must run even when the upgrade context is already cancelled).
func (m *Manager) ExitMaintenance(ctx context.Context) error {
	if m.cfg.ReactorSnippet == "" {
		return nil
	}
	return m.ctl.ReactorResume(context.WithoutCancel(ctx), m.cfg.ReactorSnippet)
}

// AwaitReady polls Health until Ready, or returns when ctx expires (the health-gate
// trip). Transient control errors are treated as not-ready until the deadline.
//
// Each poll runs on a context *detached* from ctx (await): were it on ctx, a deadline landing
// *inside* a control-channel call would close the channel mid-call, and the rollback that
// reuses that channel would then fail to restore. Detached, the deadline is observed only
// between polls, never during one, so the channel handed to rollback is always live. The
// detached poll is itself bounded (healthPollTimeout) so a wedged guest can't stall it.
func (m *Manager) AwaitReady(ctx context.Context, spec model.AppSpec) error {
	return m.await(ctx, func(c context.Context) bool {
		h, err := m.Health(c, spec)
		return err == nil && h.Ready
	})
}

// AwaitOSReady is the gate an OS UPGRADE waits on. It is a different question from
// AwaitReady's and must not be answered by it: see OSReady.
func (m *Manager) AwaitOSReady(ctx context.Context) error {
	return m.await(ctx, func(c context.Context) bool {
		ok, err := m.OSReady(c)
		return err == nil && ok
	})
}

// AwaitOSReadyServing is AwaitOSReady for a node that HELD THE HOUSE when it went down and had
// nobody to hand it to. The fact that let its reboot through -- no takeover-capable peer -- is
// the same fact that makes "not serving" a failure here rather than a vacuous pass: nobody else
// can be serving, so a node that does not come back serving has left the house dark. See
// OSReadyServing.
func (m *Manager) AwaitOSReadyServing(ctx context.Context) error {
	return m.await(ctx, func(c context.Context) bool {
		ok, err := m.OSReadyServing(c)
		return err == nil && ok
	})
}

// await is the shared poll loop. Every gate uses it so the detachment above is stated and
// enforced once; the only thing that varies is the question being asked.
func (m *Manager) await(ctx context.Context, ready func(context.Context) bool) error {
	for {
		hctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), m.cfg.healthPollTimeout)
		ok := ready(hctx)
		cancel()
		if ok {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("health-gate: not ready before deadline: %w", ctx.Err())
		case <-time.After(m.cfg.gateInterval):
		}
	}
}

// OSReady answers "is this node healthy WITH RESPECT TO AN OS UPGRADE?" — one precondition and
// two implications, each asking the node about the job it actually holds:
//
//	DRBD ANSWERS AT ALL              (the replication layer came back)
//	diskful ∧ quorate ⇒ UpToDate     (…and I am a viable failover target)
//	serving           ⇒ /healthz     (…and if I am serving, I am serving)
//
// The precondition catches the heaviest regression an OS update can ship: a release whose DRBD
// does not load at all. The implications cannot see that, since both antecedents are read FROM
// DRBD — no module, no `drbdsetup`, no antecedent — and a gate made only of implications would
// pass a node with no replication whatsoever.
//
// NOT "configured anchor ⇒ Diskful": the realistic way to lose Diskful on a configured anchor
// is a device that fails to attach, i.e. a DISK failure — not an upgrade regression and not
// repaired by reverting the image, so no rollback trigger. The software case is the
// precondition's.
//
// WHY IT IS NOT AwaitReady's QUESTION. AwaitReady probes HealthURL — the VIP — which a
// SECONDARY does not own: its peer answers. A Secondary gated that way certifies its own upgrade
// against another machine's reverse proxy and commits a broken release. A Secondary's job is to
// be a viable failover target, so that node-local question is what its gate measures.
//
// WHY IMPLICATIONS, AND WHY VACUITY IS CORRECT HERE. A node that is neither serving nor a viable
// failover target has no PRESENT obligation it can fail, so it passes — deliberately. OS updates
// must reach non-quorate nodes (the free tier, every pre-pairing node, and a degraded pair that
// may need the update to RECOVER — gating on quorum would deadlock exactly the case that needs
// fixing). What such a node cannot be certified for is deferred to when it takes the job, which
// is the rolling procedure's to check: a Secondary's upgrade is not fully verified
// until it has served, so a rolling HA upgrade must END with handover-and-verify.
//
// Quorum is deliberately NOT a rollback trigger, even differentially. Reverting does not restore
// quorum — after a reboot the node is stranded on either release — and a rollback is
// itself a guest restart. A trigger should name something reverting repairs.
func (m *Manager) OSReady(ctx context.Context) (bool, error) {
	if m.cfg.Resource == "" {
		// No cluster to consult: nothing node-local to certify, so the reverse proxy alone
		// answers. This is the standalone/test shape, not a field one.
		return m.probeReady(ctx), nil
	}
	// DRBD answers at all. A failure here is the module not loaded, `drbdsetup` gone, or the
	// resource absent from its output — the shapes a release that broke replication takes.
	// Not-ready rather than an exception: the caller polls, so a transient is retried and only
	// a persistent one spends the gate's budget and reverts.
	cl, err := m.ctl.Cluster(ctx, m.cfg.Resource)
	if err != nil {
		return false, err
	}
	if cl.Diskful && cl.Quorate && !cl.UpToDate {
		return false, nil
	}
	if cl.Serving() && !m.probeReady(ctx) {
		return false, nil
	}
	return true, nil
}

// OSReadyServing is OSReady with the vacuous half closed: the node must be serving and its
// reverse proxy must answer. For the lone holder of the house -- the only serving node an OS
// upgrade is allowed to reboot (with a takeover-capable peer it refuses) -- "if I am serving, I
// am serving" is not enough, because the node's own failure handling can make it stop serving
// before the gate looks: a release whose reverse proxy fails outright boots, promotes, the
// proxy hits its start limit, OnFailure demotes the node under a promotion hold -- and OSReady,
// polling at that moment, sees a Secondary with nothing to fail and commits a release that left
// the house dark. Nobody else could have been serving, which is exactly why the reboot was
// allowed; so here not serving is the failure, not a pass. The vacuous rows of OSReady stay
// right for every node that did NOT hold the house: a standby is certified only by the job it
// has.
func (m *Manager) OSReadyServing(ctx context.Context) (bool, error) {
	if m.cfg.Resource == "" {
		return m.probeReady(ctx), nil
	}
	cl, err := m.ctl.Cluster(ctx, m.cfg.Resource)
	if err != nil {
		return false, err
	}
	if !cl.Serving() || (cl.Diskful && cl.Quorate && !cl.UpToDate) {
		return false, nil
	}
	return m.probeReady(ctx), nil
}

// Readiness carries the pre-upgrade baseline from Capture (taken before the app is stopped) to
// Judge after the floor. The zero value (no assessor, or a baseline that couldn't be captured)
// makes Judge a no-op — the floor + rollback window stand alone.
type Readiness struct {
	base Baseline
	on   bool
}

// Gate is the S1 differential health gate: capture a baseline before the change, judge the
// signal after the liveness floor passes. It holds the POLICY — what a missing assessor means,
// what a failed sample means, and which verdict is worth undoing an upgrade for — and holds it
// exactly once.
//
// Its one caller is the APP-install path (agent/host/app.go), which has no Manager — it is
// handed a narrow guest interface. The OS-upgrade path runs no assessor: an OS gate asks only
// generic questions of the guest, never an app-specific one (agent/host/osupgrade.go).
type Gate struct {
	// Assessor is the app-specific differential signal. nil = the liveness floor alone,
	// which is the shipped default for every app the product holds no knowledge about.
	Assessor ReadinessAssessor
	// Logf receives the gate's one line per non-Pass outcome. Never nil in practice; guarded
	// anyway, because a gate that panicked while reporting a Hold would turn an advisory into
	// an outage.
	Logf func(format string, args ...any)
}

func (g Gate) logf(format string, args ...any) {
	if g.Logf != nil {
		g.Logf(format, args...)
	}
}

// Capture samples the assessor's pre-change signal while the old version still serves. A missing
// assessor or a capture failure degrades to the floor-only gate: S1 never blocks a change because
// its own telemetry failed — the safe direction, and the one that keeps a broken gate from
// becoming a broken update path.
func (g Gate) Capture(ctx context.Context) Readiness {
	if g.Assessor == nil {
		return Readiness{}
	}
	base, err := g.Assessor.Baseline(ctx)
	if err != nil {
		g.logf("readiness: baseline capture failed (%v) -> floor-only gate", err)
		return Readiness{}
	}
	return Readiness{base: base, on: true}
}

// Judge runs the differential gate above the liveness floor. It returns a non-nil error only on a
// Rollback verdict (the caller trips the {code+data} rollback); a Hold keeps the change but is
// surfaced loudly, and a Pass keeps it silently. A nil assessor, an uncaptured baseline, or an
// assess failure is a no-op — never an auto-rollback on our own signal breaking.
func (g Gate) Judge(ctx context.Context, r Readiness) error {
	if !r.on {
		return nil
	}
	verdict, reason, err := g.Assessor.Assess(ctx, r.base)
	if err != nil {
		g.logf("readiness: assess failed (%v) -> keep (floor stood)", err)
		return nil
	}
	switch verdict {
	case VerdictRollback:
		return fmt.Errorf("readiness gate: %s", reason)
	case VerdictHold:
		g.logf("readiness: HOLD -- %s (kept; awaiting review)", reason)
	}
	return nil
}

// Stub is a no-op GuestManager.
type Stub struct{}

func (Stub) Start(context.Context, model.AppSpec) error { return nil }
func (Stub) Stop(context.Context, model.AppSpec) error  { return nil }
func (Stub) Health(context.Context, model.AppSpec) (Health, error) {
	return Health{Running: true, Ready: true}, nil
}
func (Stub) SystemPath(context.Context) (string, error) { return "stub", nil }
func (Stub) Switch(context.Context, string) error       { return nil }

var (
	_ GuestManager = Stub{}
	_ GuestManager = (*Manager)(nil)
)
