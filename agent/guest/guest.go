package guest

import (
	"context"
	"fmt"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"briard.io/agent/guestagent"
	"briard.io/shared/model"
)

// GuestManager is the host↔guest boundary and the upgrade/rollback mechanism.
// The guest is the VM that carries the services; this seam starts/stops/
// health-checks it, snapshots/restores its data, and switches its code (the NixOS
// system closure) — the {code+data} rollback unit. ("unit" here is
// the systemd/rollback sense, not the guest.)
//
// Scope is asymmetric on purpose: service lifecycle, health and data
// snapshot/restore are per-service (contain the data blast radius); the code half
// (SystemPath/Switch) is whole-VM. So Snapshot/Restore move only data, and it is the
// SERVICE-install sequence (agent/host/service.go) that moves manifest and data back
// together on a failed health-gate. The OS upgrade moves neither: it is a
// property of the node, and it leaves the workload alone.
type GuestManager interface {
	Start(ctx context.Context, spec model.ServiceSpec) error
	Stop(ctx context.Context, spec model.ServiceSpec) error
	Health(ctx context.Context, spec model.ServiceSpec) (Health, error)
	Snapshot(ctx context.Context, spec model.ServiceSpec) (SnapshotRef, error)
	Restore(ctx context.Context, ref SnapshotRef) error
	SystemPath(ctx context.Context) (string, error)   // current code identity (closure store path)
	Switch(ctx context.Context, closure string) error // switch the whole-VM system closure
}

// Health is the guest's health-gate signal.
type Health struct {
	Running bool // the service's unit is active
	Ready   bool // passed its /healthz
}

// ReadinessAssessor is an optional *differential* health signal layered above the
// HTTP-200 liveness floor. The floor answers "is the service
// answering?"; an assessor answers "did the upgrade break something that worked before?"
// — service-specific (HA config-entry state), so it stays behind this seam and
// the generic Manager keeps no service knowledge. nil = the floor alone (v0/dummy/
// non-HA payloads are unaffected).
//
// The Manager captures a Baseline before quiesce (old version still serving), then after
// the floor reports ready calls Assess to settle and judge the post-upgrade signal. A
// Rollback verdict trips the {code+data} rollback; Hold keeps the upgrade but surfaces
// it (Hold leans on the rollback window +
// user-as-sensor); Pass keeps it silently. A failure to capture or assess degrades to
// the floor alone — S1 never blocks or reverts an upgrade because its own telemetry
// failed (the safe direction).
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

// Verdict is a ReadinessAssessor's decision, mapped from the service-specific gate (e.g.
// entrygate.Verdict) so the Manager stays service-agnostic.
type Verdict string

const (
	VerdictPass     Verdict = "pass"
	VerdictHold     Verdict = "hold"
	VerdictRollback Verdict = "rollback"
)

// SnapshotRef identifies a pre-upgrade {data} snapshot, self-contained so the
// upgrade sequence can restore it without re-deriving. Per-service (one subvolume,
// never the whole volume —) and pinned to the system closure it was taken
// under. System is the closure store path (node-independent), never a generation
// number (a per-node counter, meaningless on a peer — identity).
type SnapshotRef struct {
	Service   string // ServiceSpec.Name
	DataDir   string // the live subvolume this snapshot restores over
	Subvolume string // the RO snapshot subvolume path in the guest
	System    string // system closure store path at snapshot time (the code↔data pin)
	Image     string // payload OCI image ref at snapshot time (the per-service pin)
}

// control is the guest-side surface Manager drives — satisfied by *guestagent.Client
// (asserted below), faked in tests.
type control interface {
	ServiceStart(ctx context.Context, unit string) error
	ServiceStop(ctx context.Context, unit string) error
	ServiceActive(ctx context.Context, unit string) (bool, error)
	ServiceHealth(ctx context.Context, url string) (bool, error)
	VIP(ctx context.Context, dev string) (string, error)
	Snapshot(ctx context.Context, dataDir, dest string) error
	Restore(ctx context.Context, dataDir, src string) error
	SystemPath(ctx context.Context) (string, error)
	Stage(ctx context.Context, closure string, src guestagent.StageSource) error
	Components(ctx context.Context, closure string) (guestagent.SystemComponents, error)
	Switch(ctx context.Context, closure string) error
	StageBoot(ctx context.Context, closure string) error
	CollectGarbage(ctx context.Context) error
	WriteCert(ctx context.Context, cert, key string) error
	ReactorPause(ctx context.Context, snippet string) error
	ReactorResume(ctx context.Context, snippet string) error
	Cluster(ctx context.Context, resource string) (model.Cluster, error)
}

var _ control = (*guestagent.Client)(nil)

// Config carries Manager policy not derivable from a ServiceSpec.
type Config struct {
	// HealthURL is the CONFIGURED probe target (the front door's /healthz at the VIP). It is no
	// longer the last word: when the VIP is acquired by DHCP inside the guest, only the guest
	// knows the address, so ResolveHealthURL asks it every cycle and this demotes to the
	// fallback for a node whose address we did set. "" is NOT "no probe" — see Diskless.
	HealthURL string
	// VIPDev is the guest NIC the VIP lives on, when the agent named it (VIP_DEV). Only a
	// device WE named is safe to read an address back from, and unset means this node claims no
	// VIP at all -- so there is no address to resolve and nothing to probe. (It used to mean the
	// guest fell back to a baked eth1, which in the agent-less harnesses also carries the DRBD
	// address: "the first address on eth1" was then the replication link rather than the VIP.
	// [V3b.16a] deleted that fallback; [V3b.16] is what it cost in the field.)
	VIPDev string
	// Diskless marks a witness: no services, no VIP, nothing to probe — its health follows
	// quorum. This is the ONLY thing that means "never probe". Emptiness cannot carry that
	// meaning any more, because a data node acquiring its address by DHCP has no configured
	// URL either, and the two must not answer alike: a witness with no probe is healthy when
	// quorate, a data node with no address is not ready.
	Diskless bool
	// Resource is the DRBD resource this node serves — what OSReady reads its own role and
	// disk state from. Empty means the OS gate has no cluster to consult, which it
	// treats as "nothing node-local to certify" rather than as a failure.
	Resource string
	// HTTPClient probes HealthURL; nil defaults to a short-timeout client.
	HTTPClient *http.Client
	// GateInterval is how often Upgrade re-checks health while waiting for ready;
	// the caller bounds the total wait via the Upgrade context. Zero defaults.
	gateInterval time.Duration
	// HealthPollTimeout bounds each individual health poll in awaitReady. The poll runs
	// on a context *detached* from the upgrade deadline so that deadline can never
	// fire inside a control-channel call and close the channel mid-call — which would
	// break the rollback that reuses it. This caps the detached poll so a wedged guest
	// can't stall it. Zero defaults.
	healthPollTimeout time.Duration
	// ReactorSnippet enables promoter-coordinated quiesce: the service is
	// drbd-reactor-managed, so Upgrade pauses the promoter for this snippet around the
	// swap and resumes after. Quiesce is a surgical stop (ignore-dependencies), so the
	// VIP/data stay up and no target re-raise is needed. Empty = no promoter
	// coordination (unit tests / non-promoter payloads).
	ReactorSnippet string
	// ReadinessAssessor, if set, layers the differential S1 health-gate above the
	// HTTP-200 floor: the upgrade paths capture its Baseline before quiesce
	// and consult its verdict after the floor passes. nil = floor-only (the default).
	ReadinessAssessor ReadinessAssessor
	// Logf, if set, receives one line per upgrade step (progress/observability).
	Logf func(format string, args ...any)
	// IdFn generates the per-snapshot id; nil defaults to a timestamp.
	idFn func() string
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
	if cfg.idFn == nil {
		cfg.idFn = func() string { return strconv.FormatInt(time.Now().UnixNano(), 10) }
	}
	return &Manager{ctl: ctl, cfg: cfg}
}

// unitOf is the systemd unit that answers "is this service up?" — now one definition, on the
// spec itself (model.ServiceSpec.ServingUnit), because the host's resource telemetry had rebuilt
// the same derivation inline and got the runtime-installed case wrong. Kept as a name because it
// reads better at the eight call sites below than the method does.
func unitOf(spec model.ServiceSpec) string { return spec.ServingUnit() }

// hasService reports whether this node runs a workload at all. ZERO SERVICES IS THE SHIPPED
// STATE -- a fresh node serves the front door's own landing page and nothing else --
// so every service-shaped step in an upgrade has to be a no-op there rather than an error.
//
// It exists because the opposite assumption was left behind: unitOf on an empty spec
// yields the nonexistent `podman-.service`, and asking systemd about it answered "inactive",
// which the health gate read as "not ready" forever. The node was simultaneously reporting
// Healthy: true to the cloud (cfg.snapshot probes the front door and takes its answer, with
// no such conjunct) and refusing every OS upgrade to itself.
func hasService(spec model.ServiceSpec) bool { return spec.Name != "" || spec.Unit != "" }

func (m *Manager) Start(ctx context.Context, spec model.ServiceSpec) error {
	if !hasService(spec) {
		return nil // nothing to raise; the front door rides briard-vip, not this call
	}
	return m.ctl.ServiceStart(ctx, unitOf(spec))
}

func (m *Manager) Stop(ctx context.Context, spec model.ServiceSpec) error {
	if !hasService(spec) {
		return nil // nothing to quiesce
	}
	return m.ctl.ServiceStop(ctx, unitOf(spec))
}

// Health is the upgrade gate's floor. With a service it means what it always did: the service
// unit is active AND the probe passes. With NO service the probe alone decides, because the
// front door answers /healthz itself in that state -- 200, "no backend configured" -- which is
// exactly what reverse-proxy was widened to do so that "a node with nothing installed
// is ready, not sick". Running is vacuously true there: there is no service that could be down.
func (m *Manager) Health(ctx context.Context, spec model.ServiceSpec) (Health, error) {
	if !hasService(spec) {
		return Health{Running: true, Ready: m.probeReady(ctx)}, nil
	}
	running, err := m.ctl.ServiceActive(ctx, unitOf(spec))
	if err != nil {
		return Health{}, err
	}
	ready := running && m.probeReady(ctx)
	return Health{Running: running, Ready: ready}, nil
}

func (m *Manager) SystemPath(ctx context.Context) (string, error) { return m.ctl.SystemPath(ctx) }

// WriteCert lands a renewed cert/key on the guest's DRBD volume: the terminator
// hot-reloads it, so a cloud-scheduled renewal applies with no restart, and it replicates
// so a failover serves the same cert.
func (m *Manager) WriteCert(ctx context.Context, cert, key string) error {
	return m.ctl.WriteCert(ctx, cert, key)
}

func (m *Manager) Switch(ctx context.Context, closure string) error {
	return m.ctl.Switch(ctx, closure)
}

// Stage pulls closure into the guest's store before anything needs it there. It is separate from Switch — rather than folded into it — because the two
// have opposite network contracts: staging is the one step that may fetch, and switching
// (like rollback, and like a promoting peer's converge) must work with no network at all.
// Keeping them apart is what lets every node hold the code warm and a failover stay local.
//
// src is the zero StageSource in production. Errors are the caller's to interpret: the
// upgrade path treats a stage failure as "do not proceed", which leaves the node running
// what it ran before — a refusal, not a rollback, since nothing was touched yet.
func (m *Manager) Stage(ctx context.Context, closure string, src guestagent.StageSource) error {
	return m.ctl.Stage(ctx, closure, src)
}

// StageBoot makes a staged closure BOOTABLE without making it the default: it
// registers the closure in a `staging` system profile and reinstalls the bootloader from
// the *running* system, so grub gains an entry while its default entry does not move.
//
// This is the reboot path's counterpart to Switch, and the asymmetry is the safety: after
// StageBoot the disk still boots what it booted before, and which generation actually comes
// up is decided per-launch by the host (QEMUSpec.BootStaging). So the rollback is to not
// pass a flag, and nothing armed on disk can survive into a snapshot restore.
func (m *Manager) StageBoot(ctx context.Context, closure string) error {
	return m.ctl.StageBoot(ctx, closure)
}

// CollectGarbage drops old profile generations and collects the guest store.
//
// Its trigger is COMMIT -- the end of a health-green upgrade -- and that placement is the
// whole safety argument, not a scheduling preference. An upgrade is the
// only thing that adds a closure, so collecting where one was just committed is exactly
// proportional; and because commit IS the bracket closing, "never inside a maintenance
// bracket" holds by construction. It also means a GC can never run while a rollback is
// possible: by the time it fires, the gate has passed and the way back is spent.
func (m *Manager) CollectGarbage(ctx context.Context) error {
	return m.ctl.CollectGarbage(ctx)
}

// Activation is how a staged generation must be brought into service.
type Activation string

const (
	// ActivateSwitch: `switch-to-configuration switch`, in band, no downtime.
	ActivateSwitch Activation = "switch"
	// ActivateReboot: install as boot default and reboot. The guest keeps serving until
	// the reboot, so this is not "worse" -- it is a different, heavier shape, and on an HA
	// pair it is a failover rather than an outage.
	ActivateReboot Activation = "reboot"
)

// ActivationFor decides how a target must be activated by diffing it against the BOOTED
// generation, and returns the component names that forced a reboot (empty for a switch) so
// the decision explains itself in a log rather than being a bare verdict.
//
// The rule this implements is "commit to ONE method before activating, never
// switch-then-maybe-reboot". A switch that discovers halfway through that it
// needed a boot has already replaced the running system's services; there is no honest
// rollback point left, and the health-gate would be judging a state that is neither
// generation. Deciding first means rollback robustness scales with the change: a cheap
// change gets an in-band switch-back, a kernel change gets a real prior boot.
//
// The reference is the BOOTED generation, not the current one: after a switch-only update
// they differ, and it is the booted kernel that is actually running. (Our own policy keeps
// them consistent -- a switch-only update never changes the kernel -- so today the two
// agree; comparing against booted is right for the reason, not just the result.)
//
// systemd counts. NixOS itself re-execs PID 1 rather than demanding a boot, and we are
// deliberately stricter: a re-exec'd init is a third state, neither generation as-booted,
// and the health-gate's verdict is only meaningful about a state we can return to.
func ActivationFor(booted, target guestagent.SystemComponents) (Activation, []string) {
	var reasons []string
	for _, d := range []struct {
		name          string
		before, after string
	}{
		{"kernel", booted.Kernel, target.Kernel},
		{"initrd", booted.Initrd, target.Initrd},
		{"kernel-modules", booted.KernelModules, target.KernelModules},
		{"systemd", booted.Systemd, target.Systemd},
		{"kernel-params", booted.KernelParams, target.KernelParams},
	} {
		if d.before != d.after {
			reasons = append(reasons, d.name)
		}
	}
	if len(reasons) > 0 {
		return ActivateReboot, reasons
	}
	return ActivateSwitch, nil
}

// ActivationMethod reads both sides over the control channel and applies ActivationFor:
// the target's components against the booted generation's.
func (m *Manager) ActivationMethod(ctx context.Context, target string) (Activation, []string, error) {
	booted, err := m.ctl.Components(ctx, "") // "" = the booted generation
	if err != nil {
		return "", nil, fmt.Errorf("read booted components: %w", err)
	}
	want, err := m.ctl.Components(ctx, target)
	if err != nil {
		return "", nil, fmt.Errorf("read target components %s: %w", target, err)
	}
	method, reasons := ActivationFor(booted, want)
	if method != ActivateSwitch {
		// Say what the verdict was READ FROM, not merely which field disagreed. A reboot is a
		// failover on a serving node, so "why did this node reboot?" has to stay answerable from
		// the log afterwards, and a reason NAME cannot distinguish a genuine kernel bump from a
		// misread. is exactly that case: this decision was seen flipping to reboot for a
		// pair whose closures have byte-identical kernel-params.
		m.cfg.Logf("activation: reboot forced by %v; booted %+v; target %+v", reasons, booted, want)
	}
	return method, reasons, nil
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
//     hides behind is the one V3.19 exists for: probing a node-local address that answers
//     after the VIP has moved away, i.e. healthy-while-not-serving.
//   - no configured address means the VIP came from DHCP inside the guest, so the guest is the
//     only one who knows it. Ask, every cycle — the lease can change and the VIP moves on
//     failover. "" (a Secondary, or a promotion in flight) is "nothing to probe yet", which
//     reads as not-ready rather than as healthy.
//
// A verb error falls back to the configured URL, the same compatibility posture probeReady
// takes: an old guest predating net.vip is one that still has a baked address to fall back to.
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
// fixed on purpose: the target is the FRONT DOOR (:80 /healthz), which is what makes the probe
// stable across zero and one service — see the HealthURL default in agent/host/config.go.
func healthURLAt(cidr string) string {
	addr, _, _ := strings.Cut(cidr, "/")
	return "http://" + addr + "/healthz"
}

// healthURL resolves this node's probe target for one call. See ResolveHealthURL.
func (m *Manager) healthURL(ctx context.Context) string {
	return ResolveHealthURL(ctx, m.ctl, m.cfg.Diskless, m.cfg.VIPDev, m.cfg.HealthURL)
}

// ProbeReady reports whether the health endpoint answers 200. It prefers the in-guest
// probe (payload.health over the control channel) so the readiness gate survives a networking
// substrate where the host can't reach the VIP (macvtap); it falls back to a direct host-side GET
// only when that verb errors — an old guest built before it existed, which is always tap-based, so
// the LAN GET still works there. Any error/non-200 is "not ready" — the health-gate treats a
// non-answering service as a rollback trigger.
//
// Nothing to probe is not ready either: a witness has no services (its callers gate on role
// before asking), and a data node with no address yet is exactly the node nobody in the house
// can reach.
func (m *Manager) probeReady(ctx context.Context) bool {
	url := m.healthURL(ctx)
	if url == "" {
		return false
	}
	if ready, err := m.ctl.ServiceHealth(ctx, url); err == nil {
		return ready
	}
	return m.hostProbeReady(ctx, url)
}

// HostProbeReady is the legacy host-side GET of the VIP, kept as the fallback for a guest that
// predates the payload.health verb.
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

func (m *Manager) Snapshot(ctx context.Context, spec model.ServiceSpec) (SnapshotRef, error) {
	system, err := m.ctl.SystemPath(ctx)
	if err != nil {
		return SnapshotRef{}, err
	}
	// With no service there is no data to snapshot, and the rollback point is the code
	// identity alone -- which is the whole of an OS upgrade on a zero-service node.
	// Returning it rather than erroring is what makes ref.System a usable rollback target.
	if !hasService(spec) {
		return SnapshotRef{System: system}, nil
	}
	// Snapshots are siblings of the data subvolume under the btrfs root (path.Dir),
	// never inside it — DataDir is itself the subvolume that restore deletes.
	dest := path.Join(path.Dir(spec.DataDir), ".snapshots", spec.Name+"-"+m.cfg.idFn())
	if err := m.ctl.Snapshot(ctx, spec.DataDir, dest); err != nil {
		return SnapshotRef{}, err
	}
	return SnapshotRef{Service: spec.Name, DataDir: spec.DataDir, Subvolume: dest, System: system}, nil
}

func (m *Manager) Restore(ctx context.Context, ref SnapshotRef) error {
	if ref.Subvolume == "" {
		return nil // a code-only rollback point (no service): nothing to put back
	}
	return m.ctl.Restore(ctx, ref.DataDir, ref.Subvolume)
}

// THE OS UPGRADE USED TO LIVE HERE, and where it went is worth a sentence.
//
// Manager.Upgrade ran the whole switch-only sequence in the guest: quiesce the payload,
// snapshot its data, switch, restart it, gate, and on failure switch back and restore the
// data. Two things retired it.: an OS upgrade must not touch services, which removes
// the payload stop/start and the data snapshot/restore -- everything that made the sequence
// service-shaped. And (c2)'s one-rollback-for-both-methods: the point an OS upgrade rolls
// back to is the OS DISK, which only the host can snapshot or restore, so the sequence has to
// be owned by the side that owns the disk (agent/host/osupgrade.go).
//
// What is left here is what the guest genuinely owns -- Switch, StageBoot, AwaitReady,
// CaptureBaseline/Assess, EnterMaintenance/ExitMaintenance -- called by the host in order, on
// both paths, rather than composed into a second sequence. The service half went the other way
// entirely: {manifest + data} rolling back together is still the whole point, but a service is
// installed from a runtime manifest now, so that sequence lives host-side with the manifest
// ([V3b.3](e1)/(e2)) and this package's payload upgrade is gone.

// CollectStore drops the generation this upgrade displaced, at commit.
// Best-effort and deliberately last: the node is healthy on the new code either way, and a
// store that is one generation fuller is not worth failing a good upgrade over.
//
// Only the OS path calls it. A payload upgrade re-pins an OCI image and adds nothing to the
// nix store, so there is nothing there for it to collect.
func (m *Manager) CollectStore(ctx context.Context) {
	if err := m.ctl.CollectGarbage(ctx); err != nil {
		m.cfg.Logf("store gc after commit failed (harmless, retried next upgrade): %v", err)
	}
}

// EnterMaintenance holds the promoter for the service's resource, if configured.
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

// AwaitReady polls health until Ready, or returns when ctx expires (the health-gate
// trip). Transient control errors are treated as not-ready until the deadline.
//
// The health poll runs on a context *detached* from ctx: were it on the upgrade
// ctx, a deadline landing *inside* a control-channel call would close the channel
// mid-call, and the rollback that reuses that channel would then fail to restore. By
// detaching, the upgrade deadline is observed only in the select below — between polls,
// never during one — so the channel handed to rollback is always live. The detached
// poll is itself bounded (healthPollTimeout) so a wedged guest can't stall it.
func (m *Manager) AwaitReady(ctx context.Context, spec model.ServiceSpec) error {
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

// Await is the shared poll loop. Both gates use it so the detachment above is stated and
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
//	Primary           ⇒ /healthz     (…and if I am serving, I am serving)
//
// The precondition was always enforced — an unreadable cluster has never been a pass — but only
// incidentally, as an error propagating out of a read the implications happened to need. It is
// stated as a condition now because it is the one that catches the heaviest regression an OS
// update can ship: a generation whose DRBD does not load at all. Nothing below can see
// that, since both antecedents are read FROM DRBD — no module, no `drbdsetup`, no antecedent,
// and a gate made only of implications would pass a node with no replication whatsoever.
//
// ~~configured-anchor ⇒ Diskful~~ was tried and REMOVED (owner's call, and it was right): the
// realistic way to lose Diskful on a configured anchor is a device that fails to attach, i.e. a
// DISK failure — a different failure mode from an upgrade regression, not repaired by reverting
// the closure, so a rollback trigger it has no business being. What it was reaching for is the
// precondition above, which catches the software case directly and cheaply.
//
// WHY IT IS NOT AwaitReady's QUESTION. The old OS gate probed HealthURL — the VIP — which a
// SECONDARY does not own: its peer answers. So a Secondary health-gated its own upgrade against
// another machine's front door and committed a broken generation (measured; the front door is
// `wantedBy`/`partOf` briard-vip, so the node still promotes and then serves nothing). The fix is
// not a better probe, it is a node-local question: a Secondary's job is to be a viable failover
// target, so that is what its gate measures.
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
// quorum — after a reboot the node is stranded on either generation — and a rollback is
// itself a guest restart. A trigger should name something reverting repairs.
func (m *Manager) OSReady(ctx context.Context) (bool, error) {
	if m.cfg.Resource == "" {
		// No cluster to consult: nothing node-local to certify, so the front door alone
		// answers. This is the standalone/test shape, not a field one.
		return m.probeReady(ctx), nil
	}
	// DRBD answers at all. A failure here is the module not loaded, `drbdsetup` gone, or the
	// resource absent from its output — the shapes a generation that broke replication takes.
	// Not-ready rather than an exception: the caller polls, so a transient is retried and only
	// a persistent one spends the gate's budget and reverts.
	cl, err := m.ctl.Cluster(ctx, m.cfg.Resource)
	if err != nil {
		return false, err
	}
	if cl.Diskful && cl.Quorate && !cl.UpToDate {
		return false, nil
	}
	if cl.Primary && !m.probeReady(ctx) {
		return false, nil
	}
	return true, nil
}

// Readiness carries the pre-upgrade baseline from capture (before quiesce) to the
// post-floor assessment. The zero value (no assessor, or a baseline that couldn't be
// captured) makes assess a no-op — the floor + rollback window stand alone.
type Readiness struct {
	base Baseline
	on   bool
}

// CaptureBaseline samples the assessor's pre-upgrade signal while the old version still
// serves. A missing assessor or a capture failure degrades to the floor-only gate (S1
// never blocks an upgrade because its own telemetry failed — the safe direction).
func (m *Manager) CaptureBaseline(ctx context.Context) Readiness {
	if m.cfg.ReadinessAssessor == nil {
		return Readiness{}
	}
	base, err := m.cfg.ReadinessAssessor.Baseline(ctx)
	if err != nil {
		m.cfg.Logf("readiness: baseline capture failed (%v) -> floor-only gate", err)
		return Readiness{}
	}
	return Readiness{base: base, on: true}
}

// Assess runs the differential S1 gate above the liveness floor. It returns a
// non-nil error only on a Rollback verdict (the caller trips {code+data} rollback); a
// Hold keeps the upgrade but is surfaced loudly, and a
// Pass keeps it silently. A nil assessor, uncaptured baseline, or an assess failure is a
// no-op — never an auto-rollback on our own signal breaking.
func (m *Manager) Assess(ctx context.Context, r Readiness) error {
	if !r.on {
		return nil
	}
	verdict, reason, err := m.cfg.ReadinessAssessor.Assess(ctx, r.base)
	if err != nil {
		m.cfg.Logf("readiness: assess failed (%v) -> keep (floor stood)", err)
		return nil
	}
	switch verdict {
	case VerdictRollback:
		return fmt.Errorf("readiness gate: %s", reason)
	case VerdictHold:
		m.cfg.Logf("readiness: HOLD -- %s (kept; awaiting review)", reason)
	}
	return nil
}

// Stub is a no-op GuestManager for core tests and v0 wiring.
type Stub struct{}

func (Stub) Start(context.Context, model.ServiceSpec) error { return nil }
func (Stub) Stop(context.Context, model.ServiceSpec) error  { return nil }
func (Stub) Health(context.Context, model.ServiceSpec) (Health, error) {
	return Health{Running: true, Ready: true}, nil
}
func (Stub) Snapshot(_ context.Context, spec model.ServiceSpec) (SnapshotRef, error) {
	return SnapshotRef{Service: spec.Name, DataDir: spec.DataDir, Subvolume: "stub", System: "stub"}, nil
}
func (Stub) Restore(context.Context, SnapshotRef) error { return nil }
func (Stub) SystemPath(context.Context) (string, error) { return "stub", nil }
func (Stub) Switch(context.Context, string) error       { return nil }

var (
	_ GuestManager = Stub{}
	_ GuestManager = (*Manager)(nil)
)
