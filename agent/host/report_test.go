package host

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"briard.io/agent/guest"
	"briard.io/agent/guestagent"
	"briard.io/shared/api"
	"briard.io/shared/model"
	"briard.io/shared/notify"
)

// testUpgradeBudget is what these tests pass for Config.UpgradeBudget. Every fake here answers
// immediately, so the value only has to be non-zero — the PRODUCTION default is asserted where it
// is actually decided, in TestConfigFromEnv_UpgradeBudgetDefault.
const testUpgradeBudget = time.Minute

// fakeUpgrader records the upgrade call a directive drives.
type fakeUpgrader struct {
	hold              func() error // blocks inside the budget (see beat_test.go)
	spec              model.ServiceSpec
	oldImg, newImg    string
	target            string // Upgrade's system closure (whole-OS)
	rescued           bool   // RescueGuest was called (B.10)
	certCert          string // WriteCert's cert PEM
	certKey           string // WriteCert's key PEM
	err               error
	called            bool
	sysCalled         bool
	convCalled        bool
	activation        guest.Activation // ActivationMethod's verdict; "" -> switch
	activationReasons []string
	activationErr     error
	activationFor     string                 // the closure ActivationMethod was asked about
	staged            string                 // Stage's closure
	stagedFrom        guestagent.StageSource // zero in production
	stageErr          error                  // when set, staging fails and nothing may switch
	rebootTarget      string                 // RebootUpgrade's closure
	rebootErr         error                  // when set, the reboot upgrade fails
	rebootRolledBack  bool                   // ...and whether it got the node back
	sysRolledBack     bool                   // ...and the same answer for the switch method
}

func (f *fakeUpgrader) UpgradePayload(_ context.Context, spec model.ServiceSpec, oldImage, newImage string) (guest.SnapshotRef, error) {
	if f.hold != nil {
		if err := f.hold(); err != nil {
			return guest.SnapshotRef{}, err
		}
	}
	f.called, f.spec, f.oldImg, f.newImg = true, spec, oldImage, newImage
	return guest.SnapshotRef{}, f.err
}

// Neither OS-upgrade method takes a ServiceSpec: there is nothing an OS upgrade
// has to say to a workload, so there is nothing here for the fake to record.
func (f *fakeUpgrader) Upgrade(_ context.Context, target string) (bool, error) {
	f.sysCalled, f.target = true, target
	return f.sysRolledBack, f.err
}

func (f *fakeUpgrader) Stage(_ context.Context, closure string, src guestagent.StageSource) error {
	if f.hold != nil {
		if err := f.hold(); err != nil {
			return err
		}
	}
	f.staged, f.stagedFrom = closure, src
	return f.stageErr
}

// Defaults to a switch when unset, so tests about other things don't all have to opt into
// an activation method; the reboot/error cases set it explicitly.
func (f *fakeUpgrader) ActivationMethod(_ context.Context, target string) (guest.Activation, []string, error) {
	f.activationFor = target
	if f.activation == "" && f.activationErr == nil {
		return guest.ActivateSwitch, nil, nil
	}
	return f.activation, f.activationReasons, f.activationErr
}

func (f *fakeUpgrader) Converge(_ context.Context, spec model.ServiceSpec, target string) error {
	f.convCalled, f.spec, f.target = true, spec, target
	return f.err
}

func (f *fakeUpgrader) RebootUpgrade(_ context.Context, target string) (bool, error) {
	f.rebootTarget = target
	return f.rebootRolledBack, f.rebootErr
}

func (f *fakeUpgrader) WriteCert(_ context.Context, cert, key string) error {
	f.certCert, f.certKey = cert, key
	return f.err
}

// fakeSelfUpdater records the agent-update the dispatch drives, without a real binary,
// keyring, or systemd. staged records the offered update; armed/current are configurable.
type fakeSelfUpdater struct {
	staged    *api.AgentUpdate
	stageErr  error  // non-nil -> Stage refuses (a bad signature / failed fetch)
	current   string // the running version, for the idempotency check
	restarted bool
	isArmed   bool
}

func (s *fakeSelfUpdater) Stage(_ context.Context, u api.AgentUpdate) error {
	if s.stageErr != nil {
		return s.stageErr
	}
	s.staged = &u
	s.isArmed = true
	return nil
}
func (s *fakeSelfUpdater) Armed() bool                   { return s.isArmed }
func (s *fakeSelfUpdater) Current() string               { return s.current }
func (s *fakeSelfUpdater) Restart(context.Context) error { s.restarted = true; return nil }

func TestApplyDirective(t *testing.T) {
	var logs []string
	logf := func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }
	spec := model.ServiceSpec{Name: "dummy", Image: "img:v0"}
	applyDirective(context.Background(), api.Directive{Kind: api.DirectiveLog, Payload: "x"}, nil, spec, "img:v0", nil, nil, nil, logf, testUpgradeBudget, nil)
	applyDirective(context.Background(), api.Directive{Kind: api.DirectiveNoop}, nil, spec, "img:v0", nil, nil, nil, logf, testUpgradeBudget, nil)
	applyDirective(context.Background(), api.Directive{Kind: "weird"}, nil, spec, "img:v0", nil, nil, nil, logf, testUpgradeBudget, nil)
	joined := strings.Join(logs, "\n")
	for _, want := range []string{"kind=log payload=\"x\"", "kind=noop acked", `kind="weird" unhandled`} {
		if !strings.Contains(joined, want) {
			t.Errorf("logs missing %q\ngot:\n%s", want, joined)
		}
	}
}

// An upgrade directive drives Manager.UpgradePayload with the node's *current* served
// image as the baseline (UpgradePayload's rollback target) and the directive payload as
// the new image. The new version is not tracked in-memory -- the observe loop re-reads it
// from the guest pin next cycle -- so this just asserts the baseline is threaded through.
func TestApplyDirectiveUpgrade(t *testing.T) {
	up := &fakeUpgrader{}
	spec := model.ServiceSpec{Name: "dummy", Image: "briard-dummy:v0", DataDir: "/var/lib/briard/dummy"}
	applyDirective(context.Background(), api.Directive{Kind: api.DirectiveUpgrade, Payload: "briard-dummy:v1"}, up, spec, "briard-dummy:v0", nil, nil, nil, func(string, ...any) {}, testUpgradeBudget, nil)
	if !up.called || up.oldImg != "briard-dummy:v0" || up.newImg != "briard-dummy:v1" || up.spec.Name != "dummy" {
		t.Errorf("UpgradePayload call = %+v (called=%v), want dummy v0->v1", up, up.called)
	}
}

// The upgrade baseline is whatever the caller read from the guest -- so a node already on
// v1 (converged) chains v1->v2, reverting to v1, not the original v0.
func TestApplyDirectiveUpgradeBaselineIsCurrent(t *testing.T) {
	up := &fakeUpgrader{}
	spec := model.ServiceSpec{Name: "dummy", Image: "briard-dummy:v0", DataDir: "/d"}
	applyDirective(context.Background(), api.Directive{Kind: api.DirectiveUpgrade, Payload: "briard-dummy:v2"}, up, spec, "briard-dummy:v1", nil, nil, nil, func(string, ...any) {}, testUpgradeBudget, nil)
	if up.oldImg != "briard-dummy:v1" || up.newImg != "briard-dummy:v2" {
		t.Errorf("upgrade baseline = %q->%q, want the current v1->v2", up.oldImg, up.newImg)
	}
}

// An upgrade-system directive drives Manager.Upgrade with the target system closure
// (the whole-OS switch); Upgrade derives its own rollback point from the guest.
func TestApplyDirectiveUpgradeSystem(t *testing.T) {
	up := &fakeUpgrader{}
	spec := model.ServiceSpec{Name: "briard-payload", DataDir: "/var/lib/briard/dummy"}
	applyDirective(context.Background(), api.Directive{Kind: api.DirectiveUpgradeSystem, Payload: "/nix/store/abc-nixos-system"}, up, spec, "img:v0", nil, nil, nil, func(string, ...any) {}, testUpgradeBudget, nil)
	if !up.sysCalled || up.target != "/nix/store/abc-nixos-system" || up.called {
		t.Errorf("Upgrade call = %+v, want the OS switch to the target closure (not UpgradePayload)", up)
	}
}

// The OS switch stages the closure first, and stages the SAME closure it is about
// to switch to. Production passes no source, so the guest uses the caches baked into its
// image — a non-zero StageSource here would mean a test-shaped override leaked into the
// product path.
func TestApplyDirectiveUpgradeSystemStagesFirst(t *testing.T) {
	up := &fakeUpgrader{}
	spec := model.ServiceSpec{Name: "briard-payload", DataDir: "/var/lib/briard/dummy"}
	const target = "/nix/store/abc-nixos-system"
	applyDirective(context.Background(), api.Directive{Kind: api.DirectiveUpgradeSystem, Payload: target}, up, spec, "img:v0", nil, nil, nil, func(string, ...any) {}, testUpgradeBudget, nil)
	if up.staged != target {
		t.Errorf("staged %q, want the switch target %q", up.staged, target)
	}
	if (up.stagedFrom != guestagent.StageSource{}) {
		t.Errorf("staged from %+v, want the zero source (the guest's own baked caches)", up.stagedFrom)
	}
}

// If the bytes never arrive, nothing switches. The node is left exactly as it was
// -- so this reports FAILED, not rolled-back: no snapshot was taken and no generation was
// touched, and calling it a rollback would claim a recovery that never ran.
func TestApplyDirectiveUpgradeSystemStageFailureDoesNotSwitch(t *testing.T) {
	up := &fakeUpgrader{stageErr: fmt.Errorf("substituter unreachable")}
	fn := &fakeNotifier{}
	spec := model.ServiceSpec{Name: "briard-payload", DataDir: "/d"}
	o := applyDirective(context.Background(), api.Directive{ID: "9", Kind: api.DirectiveUpgradeSystem, Payload: "/nix/store/abc-nixos-system"}, up, spec, "img:v0", fn, nil, nil, func(string, ...any) {}, testUpgradeBudget, nil)
	if up.sysCalled {
		t.Error("switched despite a failed stage — the closure is not in the store")
	}
	if o.State != api.OutcomeFailed {
		t.Errorf("outcome = %+v, want failed (nothing was touched, so it is not a rollback)", o)
	}
	if len(fn.alerts) != 1 {
		t.Errorf("a failed stage must escalate once, got %+v", fn.alerts)
	}
}

// The activation method is decided BEFORE anything is touched, and against the
// closure about to be activated. Ordering matters as much as the verdict — deciding after
// the switch would be deciding after the point of no return.
func TestApplyDirectiveUpgradeSystemDecidesActivationFirst(t *testing.T) {
	up := &fakeUpgrader{}
	spec := model.ServiceSpec{Name: "briard-payload", DataDir: "/d"}
	const target = "/nix/store/abc-nixos-system"
	applyDirective(context.Background(), api.Directive{Kind: api.DirectiveUpgradeSystem, Payload: target}, up, spec, "img:v0", nil, nil, nil, func(string, ...any) {}, testUpgradeBudget, nil)
	if up.activationFor != target {
		t.Errorf("activation checked for %q, want the target %q", up.activationFor, target)
	}
	if !up.sysCalled {
		t.Error("a switch-only target must still upgrade")
	}
}

// V3.17c1/c2: a target needing a reboot takes the reboot path, and must NEVER take the switch
// one — switching a kernel/initrd change in band would leave the guest running userland from
// one generation on a kernel from another, the exact mismatch a reboot exists to avoid. The
// three outcomes are separated because the cloud acts on them differently: the node is on the
// target, back on its old code, or somewhere a human has to look.
func TestApplyDirectiveUpgradeSystemRebootTarget(t *testing.T) {
	const target = "/nix/store/abc-nixos-system"
	for _, tc := range []struct {
		name      string
		err       error
		back      bool
		want      string
		wantAlert int
	}{
		{name: "booted and gated", want: api.OutcomeDone},
		{name: "gate tripped, node recovered", err: fmt.Errorf("health-gate"), back: true, want: api.OutcomeRolledBack, wantAlert: 1},
		{name: "no clean rollback", err: fmt.Errorf("disk not restored"), want: api.OutcomeFailed, wantAlert: 1},
		// A peer can take over, so the reboot would be a failover and the node declines.
		// wantAlert is 0, and that zero IS the assertion rather than an omission -- an HA pair
		// would otherwise mail its owner on every OS release, about a node serving perfectly.
		{name: "declined, needs a scheduled handover", err: fmt.Errorf("gate: %w", ErrHandoverRequired),
			back: true, want: api.OutcomeRolledBack, wantAlert: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := &fakeUpgrader{
				activation: guest.ActivateReboot, activationReasons: []string{"kernel", "initrd"},
				rebootErr: tc.err, rebootRolledBack: tc.back,
			}
			fn := &fakeNotifier{}
			spec := model.ServiceSpec{Name: "briard-payload", DataDir: "/d"}
			o := applyDirective(context.Background(), api.Directive{ID: "7", Kind: api.DirectiveUpgradeSystem, Payload: target}, up, spec, "img:v0", fn, nil, nil, func(string, ...any) {}, testUpgradeBudget, nil)
			if up.sysCalled {
				t.Error("switched a reboot-only target — that is the switch-then-maybe-reboot the rule forbids")
			}
			if up.rebootTarget != target {
				t.Errorf("reboot upgrade drove %q, want the target %q", up.rebootTarget, target)
			}
			if o.State != tc.want {
				t.Errorf("outcome = %+v, want %s", o, tc.want)
			}
			if len(fn.alerts) != tc.wantAlert {
				t.Errorf("escalations = %+v, want %d", fn.alerts, tc.wantAlert)
			}
		})
	}
}

// The staging half must still gate the reboot path: a closure that never arrived cannot be
// booted into, and reporting a rollback for bytes that never landed would claim a recovery
// that never ran.
func TestApplyDirectiveUpgradeSystemRebootNeedsStagingFirst(t *testing.T) {
	up := &fakeUpgrader{
		activation: guest.ActivateReboot, activationReasons: []string{"kernel"},
		stageErr: fmt.Errorf("no route to cache"),
	}
	spec := model.ServiceSpec{Name: "briard-payload", DataDir: "/d"}
	o := applyDirective(context.Background(), api.Directive{ID: "9", Kind: api.DirectiveUpgradeSystem, Payload: "/nix/store/abc"}, up, spec, "img:v0", &fakeNotifier{}, nil, nil, func(string, ...any) {}, testUpgradeBudget, nil)
	if up.rebootTarget != "" {
		t.Errorf("rebooted into %q after staging failed", up.rebootTarget)
	}
	if o.State != api.OutcomeFailed {
		t.Errorf("outcome = %+v, want failed (nothing was touched, so nothing rolled back)", o)
	}
}

// If the method can't be determined at all, refuse too: an unknown answer is not a licence
// to guess the cheap one.
func TestApplyDirectiveUpgradeSystemRefusesOnActivationError(t *testing.T) {
	up := &fakeUpgrader{activationErr: fmt.Errorf("guest unreachable")}
	spec := model.ServiceSpec{Name: "briard-payload", DataDir: "/d"}
	o := applyDirective(context.Background(), api.Directive{ID: "8", Kind: api.DirectiveUpgradeSystem, Payload: "/nix/store/abc"}, up, spec, "img:v0", &fakeNotifier{}, nil, nil, func(string, ...any) {}, testUpgradeBudget, nil)
	if up.sysCalled || o.State != api.OutcomeFailed {
		t.Errorf("switched=%v outcome=%+v, want no switch and failed", up.sysCalled, o)
	}
}

// A failed upgrade (rolled back — including a wedged, timed-out rollback) escalates via
// the notifier: upgrades are rare, so this is a real signal, not fatigue.
func TestApplyDirectiveUpgradeEscalates(t *testing.T) {
	up := &fakeUpgrader{err: fmt.Errorf("health-gate tripped -> rolled back")}
	fn := &fakeNotifier{}
	spec := model.ServiceSpec{Name: "dummy", Image: "briard-dummy:v0"}
	applyDirective(context.Background(), api.Directive{Kind: api.DirectiveUpgrade, Payload: "briard-dummy:v1"}, up, spec, "briard-dummy:v0", fn, nil, nil, func(string, ...any) {}, testUpgradeBudget, nil)
	if len(fn.alerts) != 1 || fn.alerts[0].Level != notify.Warning {
		t.Fatalf("a failed upgrade must escalate one warning, got %+v", fn.alerts)
	}
	// A successful upgrade must NOT escalate.
	up2, fn2 := &fakeUpgrader{}, &fakeNotifier{}
	applyDirective(context.Background(), api.Directive{Kind: api.DirectiveUpgrade, Payload: "briard-dummy:v1"}, up2, spec, "briard-dummy:v0", fn2, nil, nil, func(string, ...any) {}, testUpgradeBudget, nil)
	if len(fn2.alerts) != 0 {
		t.Errorf("a successful upgrade must not escalate, got %+v", fn2.alerts)
	}
}

// The cert handshake: a cert-request makes the node generate a keypair + CSR and queue
// it for upload; the follow-up cert directive pairs the returned cert with the node's stashed
// key and writes both to the volume. The key is node-generated and never came from the cloud.
func TestApplyDirectiveCert(t *testing.T) {
	up := &fakeUpgrader{}
	cr := &certRequester{}
	// Leg 1: the cloud asks for a CSR.
	applyDirective(context.Background(), api.Directive{Kind: api.DirectiveCertRequest, Payload: "briard.test"}, up, model.ServiceSpec{Name: "briard-payload"}, "", nil, cr, nil, func(string, ...any) {}, testUpgradeBudget, nil)
	if cr.pendingCSR == nil || cr.keyPEM == "" {
		t.Fatal("cert-request must generate a keypair + a CSR queued for upload")
	}
	if _, err := x509.ParseCertificateRequest(cr.pendingCSR); err != nil {
		t.Fatalf("queued CSR does not parse: %v", err)
	}
	// Leg 3: the signed cert (cert-only) arrives and is paired with the stashed key.
	bundle, _ := json.Marshal(api.CertBundle{Name: "briard.test", Cert: "CERTPEM"})
	applyDirective(context.Background(), api.Directive{Kind: api.DirectiveCert, Payload: string(bundle)}, up, model.ServiceSpec{Name: "briard-payload"}, "", nil, cr, nil, func(string, ...any) {}, testUpgradeBudget, nil)
	if up.certCert != "CERTPEM" || up.certKey != cr.keyPEM {
		t.Errorf("WriteCert got cert=%q key=%q, want the cert paired with the node's stashed key", up.certCert, up.certKey)
	}
}

// A cert directive with no matching stashed key (e.g. a restart lost it) is skipped, not
// applied with a mismatched key -- the cloud re-requests a CSR on its next tick.
func TestApplyDirectiveCertNoKeySkips(t *testing.T) {
	up := &fakeUpgrader{}
	bundle, _ := json.Marshal(api.CertBundle{Name: "briard.test", Cert: "CERTPEM"})
	applyDirective(context.Background(), api.Directive{Kind: api.DirectiveCert, Payload: string(bundle)}, up, model.ServiceSpec{Name: "briard-payload"}, "", nil, &certRequester{}, nil, func(string, ...any) {}, testUpgradeBudget, nil)
	if up.certCert != "" {
		t.Errorf("cert with no held key must be skipped, got WriteCert cert=%q", up.certCert)
	}
}

// An agent-update directive drives the self-updater to fetch+verify+stage a signed
// binary. A staged update reports done (the host loop then restarts to trial it); convergence
// is confirmed later via NodeStatus.AgentVersion, not this outcome.
func TestApplyDirectiveAgentUpdateStages(t *testing.T) {
	su := &fakeSelfUpdater{current: "v1"}
	payload, _ := json.Marshal(api.AgentUpdate{Version: "v2", URL: "https://rel/agent", Sig: "c2ln"})
	o := applyDirective(context.Background(), api.Directive{ID: "u", Kind: api.DirectiveAgentUpdate, Payload: string(payload)},
		nil, model.ServiceSpec{}, "", nil, nil, su, func(string, ...any) {}, testUpgradeBudget, nil)
	if o.State != api.OutcomeDone {
		t.Fatalf("agent-update outcome = %+v, want done", o)
	}
	if su.staged == nil || su.staged.Version != "v2" || su.staged.URL != "https://rel/agent" {
		t.Errorf("Stage got %+v, want the offered v2", su.staged)
	}
}

// The load-bearing negative: a refused update (bad signature / failed fetch) reports FAILED and
// escalates -- and nothing was staged, so the running binary is kept. [[verification-assertions-must-fail]]
func TestApplyDirectiveAgentUpdateRefusedEscalates(t *testing.T) {
	su := &fakeSelfUpdater{current: "v1", stageErr: fmt.Errorf("signature does not verify")}
	fn := &fakeNotifier{}
	payload, _ := json.Marshal(api.AgentUpdate{Version: "v2", URL: "https://rel/agent", Sig: "bad"})
	o := applyDirective(context.Background(), api.Directive{ID: "u", Kind: api.DirectiveAgentUpdate, Payload: string(payload)},
		nil, model.ServiceSpec{}, "", fn, nil, su, func(string, ...any) {}, testUpgradeBudget, nil)
	if o.State != api.OutcomeFailed {
		t.Fatalf("a refused update outcome = %+v, want failed", o)
	}
	if su.staged != nil || su.isArmed {
		t.Error("a refused update staged/armed something -- refuse-and-stay violated")
	}
	if len(fn.alerts) != 1 || fn.alerts[0].Level != notify.Warning {
		t.Errorf("a refused self-update must escalate one warning, got %+v", fn.alerts)
	}
}

// A re-offer of the version already running is an idempotent no-op (done, no fetch) -- so a
// re-delivered directive after the update committed doesn't loop the agent.
func TestApplyDirectiveAgentUpdateIdempotent(t *testing.T) {
	su := &fakeSelfUpdater{current: "v2"}
	payload, _ := json.Marshal(api.AgentUpdate{Version: "v2", URL: "https://rel/agent", Sig: "c2ln"})
	o := applyDirective(context.Background(), api.Directive{ID: "u", Kind: api.DirectiveAgentUpdate, Payload: string(payload)},
		nil, model.ServiceSpec{}, "", nil, nil, su, func(string, ...any) {}, testUpgradeBudget, nil)
	if o.State != api.OutcomeDone {
		t.Fatalf("re-offer of the running version outcome = %+v, want done", o)
	}
	if su.staged != nil {
		t.Error("a re-offer of the running version re-staged -- not idempotent")
	}
}

// A node with no self-updater wired (no keyring provisioned) refuses the directive -- fail closed.
func TestApplyDirectiveAgentUpdateNoUpdaterRefuses(t *testing.T) {
	payload, _ := json.Marshal(api.AgentUpdate{Version: "v2", URL: "u", Sig: "s"})
	o := applyDirective(context.Background(), api.Directive{ID: "u", Kind: api.DirectiveAgentUpdate, Payload: string(payload)},
		nil, model.ServiceSpec{}, "", nil, nil, nil, func(string, ...any) {}, testUpgradeBudget, nil)
	if o.State != api.OutcomeFailed {
		t.Errorf("agent-update with no updater = %+v, want failed", o)
	}
}

// ApplyDirective returns the terminal outcome the node reports back -- done on success,
// rolled-back on an upgrade the health-gate reverts, failed when it can't apply.
func TestApplyDirectiveOutcome(t *testing.T) {
	spec := model.ServiceSpec{Name: "dummy", Image: "briard-dummy:v0", DataDir: "/d"}
	nolog := func(string, ...any) {}

	if o := applyDirective(context.Background(), api.Directive{ID: "1", Kind: api.DirectiveNoop}, nil, spec, "", nil, nil, nil, nolog, testUpgradeBudget, nil); o.ID != "1" || o.State != api.OutcomeDone {
		t.Errorf("noop outcome = %+v, want done id=1", o)
	}
	up := &fakeUpgrader{}
	if o := applyDirective(context.Background(), api.Directive{ID: "2", Kind: api.DirectiveUpgrade, Payload: "briard-dummy:v1"}, up, spec, "briard-dummy:v0", nil, nil, nil, nolog, testUpgradeBudget, nil); o.State != api.OutcomeDone {
		t.Errorf("upgrade outcome = %+v, want done", o)
	}
	upErr := &fakeUpgrader{err: fmt.Errorf("gate tripped")}
	if o := applyDirective(context.Background(), api.Directive{ID: "3", Kind: api.DirectiveUpgrade, Payload: "briard-dummy:v1"}, upErr, spec, "briard-dummy:v0", &fakeNotifier{}, nil, nil, nolog, testUpgradeBudget, nil); o.State != api.OutcomeRolledBack {
		t.Errorf("failed-upgrade outcome = %+v, want rolled-back", o)
	}
	if o := applyDirective(context.Background(), api.Directive{ID: "4", Kind: "weird"}, nil, spec, "", nil, nil, nil, nolog, testUpgradeBudget, nil); o.State != api.OutcomeFailed {
		t.Errorf("unhandled outcome = %+v, want failed", o)
	}
}

// A node with no payload (witness / empty spec) ignores an upgrade directive.
func TestApplyDirectiveUpgradeIgnoredWithoutTarget(t *testing.T) {
	up := &fakeUpgrader{}
	applyDirective(context.Background(), api.Directive{Kind: api.DirectiveUpgrade, Payload: "x"}, up, model.ServiceSpec{}, "", nil, nil, nil, func(string, ...any) {}, testUpgradeBudget, nil)
	if up.called {
		t.Error("must not upgrade when the node has no payload spec")
	}
}

// The SHIPPED node -- a fresh install with nothing on it -- must accept an OS upgrade.
// The old guard also required spec.Name, so a zero ServiceSpec made the directive come back
// "failed" without the node ever staging or switching anything. That is the state install.sh
// leaves every free-tier island in, which makes it the population the guard broke for.
func TestApplyDirectiveUpgradeSystemOnAZeroServiceNode(t *testing.T) {
	up := &fakeUpgrader{}
	const target = "/nix/store/abc-nixos-system"
	o := applyDirective(context.Background(), api.Directive{Kind: api.DirectiveUpgradeSystem, Payload: target},
		up, model.ServiceSpec{}, "", nil, nil, nil, func(string, ...any) {}, testUpgradeBudget, nil)

	if o.State != api.OutcomeDone {
		t.Errorf("outcome = %q (%s), want done -- a node with no service is still a node", o.State, o.Detail)
	}
	if !up.sysCalled || up.target != target {
		t.Errorf("Upgrade call = %+v, want the OS switch to %s", up, target)
	}
	if up.staged != target {
		t.Errorf("staged %q, want %s -- delivery does not depend on a service", up.staged, target)
	}
}

// The guard that remains: no TARGET is still refused. Without this, "loosen the guard" could
// have been satisfied by removing it entirely.
func TestApplyDirectiveUpgradeSystemStillRefusesAnEmptyTarget(t *testing.T) {
	up := &fakeUpgrader{}
	o := applyDirective(context.Background(), api.Directive{Kind: api.DirectiveUpgradeSystem, Payload: ""},
		up, model.ServiceSpec{}, "", nil, nil, nil, func(string, ...any) {}, testUpgradeBudget, nil)
	if o.State == api.OutcomeDone {
		t.Error("an upgrade-system with no target must not report done")
	}
	if up.sysCalled {
		t.Error("nothing should have been upgraded")
	}
}

// RescueGuest records the call; the fake never touches a disk. The real one is proven by
// nixosTest/guest-rescue.nix, which is the only place a rebuilt overlay can be observed.
func (f *fakeUpgrader) RescueGuest(context.Context) error {
	if f.hold != nil {
		if err := f.hold(); err != nil {
			return err
		}
	}
	f.rescued = true
	return f.err
}

// A rescue directive drives RescueGuest and needs NOTHING else: no payload (the node rescues
// itself from its own disk, so there is nothing for a caller to name or get wrong) and no
// ServiceSpec (an OS-disk rebuild is a property of the node, and a fresh install carries the zero
// spec -- the same trap that once made the shipped node un-upgradable, DirectiveUpgradeSystem).
func TestApplyDirectiveRescue(t *testing.T) {
	up := &fakeUpgrader{}
	o := applyDirective(context.Background(), api.Directive{Kind: api.DirectiveRescue},
		up, model.ServiceSpec{}, "", nil, nil, nil, func(string, ...any) {}, testUpgradeBudget, nil)
	if !up.rescued {
		t.Error("RescueGuest was not called")
	}
	if o.State != api.OutcomeDone {
		t.Errorf("outcome = %+v, want done", o)
	}
}

// A node with no guest refuses rather than pretending. There is no rollback past a rescue -- the
// old overlay is gone -- so "failed" has to be reachable and honest.
func TestApplyDirectiveRescueRefusesWithoutAGuest(t *testing.T) {
	o := applyDirective(context.Background(), api.Directive{Kind: api.DirectiveRescue},
		nil, model.ServiceSpec{}, "", nil, nil, nil, func(string, ...any) {}, testUpgradeBudget, nil)
	if o.State != api.OutcomeFailed {
		t.Errorf("outcome with no upgrader = %+v, want failed", o)
	}
}

// The node's reason reaches the caller verbatim: this verb's refusals ("not an overlay", "the
// backing image is not readable") are the operator's only guidance, and a dispatch that swallowed
// them would leave `briard rescue` saying only that something went wrong.
func TestApplyDirectiveRescueSurfacesTheReason(t *testing.T) {
	up := &fakeUpgrader{err: errors.New("not an overlay")}
	o := applyDirective(context.Background(), api.Directive{Kind: api.DirectiveRescue},
		up, model.ServiceSpec{}, "", nil, nil, nil, func(string, ...any) {}, testUpgradeBudget, nil)
	if o.State != api.OutcomeFailed || !strings.Contains(o.Detail, "not an overlay") {
		t.Errorf("outcome = %+v, want failed carrying the node's reason", o)
	}
}
