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

	"briard.io/agent/install"
	"briard.io/shared/api"
	"briard.io/shared/notify"
)

// testUpgradeBudget is what these tests pass for Config.UpgradeBudget. Every fake here answers
// immediately, so the value only has to be non-zero — the PRODUCTION default is asserted where it
// is actually decided, in TestConfigFromEnv_UpgradeBudgetDefault.
const testUpgradeBudget = time.Minute

// fakeUpgrader records the upgrade call a directive drives.
type fakeUpgrader struct {
	hold            func() error // blocks inside the budget (see beat_test.go)
	rescued         bool         // RescueGuest was called (B.10)
	certCert        string       // WriteCert's cert PEM
	certKey         string       // WriteCert's key PEM
	err             error
	imageTarget     install.Manifest // ImageUpgrade's release ([B.86h])
	imageErr        error
	imageRolledBack bool
}

func (f *fakeUpgrader) WriteCert(_ context.Context, cert, key string) error {
	f.certCert, f.certKey = cert, key
	return f.err
}

// fakeSelfUpdater records the agent-update the dispatch drives, without a real unit,
// keyring, or systemd. triggered records the version handed to the update unit; armed/current
// are configurable.
type fakeSelfUpdater struct {
	triggered  string
	triggerErr error  // non-nil -> the unit refused (a bad pin / bad signature / failed fetch)
	current    string // the running version, for the idempotency check
	restarted  bool
	isArmed    bool
}

func (s *fakeSelfUpdater) Trigger(_ context.Context, version string) (string, error) {
	if s.triggerErr != nil {
		return s.triggerErr.Error(), s.triggerErr
	}
	s.triggered = version
	s.isArmed = true
	return "staged " + version + ", armed", nil
}
func (s *fakeSelfUpdater) Armed() bool                   { return s.isArmed }
func (s *fakeSelfUpdater) Current() string               { return s.current }
func (s *fakeSelfUpdater) Restart(context.Context) error { s.restarted = true; return nil }

func TestApplyDirective(t *testing.T) {
	var logs []string
	logf := func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }
	applyDirective(context.Background(), api.Directive{Kind: api.DirectiveLog, Payload: "x"}, nil, nil, nil, nil, logf, testUpgradeBudget, nil)
	applyDirective(context.Background(), api.Directive{Kind: api.DirectiveNoop}, nil, nil, nil, nil, logf, testUpgradeBudget, nil)
	applyDirective(context.Background(), api.Directive{Kind: "weird"}, nil, nil, nil, nil, logf, testUpgradeBudget, nil)
	joined := strings.Join(logs, "\n")
	for _, want := range []string{"kind=log payload=\"x\"", "kind=noop acked", `kind="weird" unhandled`} {
		if !strings.Contains(joined, want) {
			t.Errorf("logs missing %q\ngot:\n%s", want, joined)
		}
	}
}

// The cert handshake: a cert-request makes the node generate a keypair + CSR and queue
// it for upload; the follow-up cert directive pairs the returned cert with the node's stashed
// key and writes both to the volume. The key is node-generated and never came from the cloud.
func TestApplyDirectiveCert(t *testing.T) {
	up := &fakeUpgrader{}
	cr := &certRequester{}
	// Leg 1: the cloud asks for a CSR.
	applyDirective(context.Background(), api.Directive{Kind: api.DirectiveCertRequest, Payload: "briard.test"}, up, nil, cr, nil, func(string, ...any) {}, testUpgradeBudget, nil)
	if cr.pendingCSR == nil || cr.keyPEM == "" {
		t.Fatal("cert-request must generate a keypair + a CSR queued for upload")
	}
	if _, err := x509.ParseCertificateRequest(cr.pendingCSR); err != nil {
		t.Fatalf("queued CSR does not parse: %v", err)
	}
	// Leg 3: the signed cert (cert-only) arrives and is paired with the stashed key.
	bundle, _ := json.Marshal(api.CertBundle{Name: "briard.test", Cert: "CERTPEM"})
	applyDirective(context.Background(), api.Directive{Kind: api.DirectiveCert, Payload: string(bundle)}, up, nil, cr, nil, func(string, ...any) {}, testUpgradeBudget, nil)
	if up.certCert != "CERTPEM" || up.certKey != cr.keyPEM {
		t.Errorf("WriteCert got cert=%q key=%q, want the cert paired with the node's stashed key", up.certCert, up.certKey)
	}
}

// A cert directive with no matching stashed key (e.g. a restart lost it) is skipped, not
// applied with a mismatched key -- the cloud re-requests a CSR on its next tick.
func TestApplyDirectiveCertNoKeySkips(t *testing.T) {
	up := &fakeUpgrader{}
	bundle, _ := json.Marshal(api.CertBundle{Name: "briard.test", Cert: "CERTPEM"})
	applyDirective(context.Background(), api.Directive{Kind: api.DirectiveCert, Payload: string(bundle)}, up, nil, &certRequester{}, nil, func(string, ...any) {}, testUpgradeBudget, nil)
	if up.certCert != "" {
		t.Errorf("cert with no held key must be skipped, got WriteCert cert=%q", up.certCert)
	}
}

// An agent-update directive hands the offered version to the update unit below the agent. A
// staged update reports done (the host loop then restarts to trial it); convergence is
// confirmed later via NodeStatus.AgentVersion, not this outcome.
func TestApplyDirectiveAgentUpdateTriggersTheUnit(t *testing.T) {
	su := &fakeSelfUpdater{current: "v1"}
	payload, _ := json.Marshal(api.AgentUpdate{Version: "v2"})
	o := applyDirective(context.Background(), api.Directive{ID: "u", Kind: api.DirectiveAgentUpdate, Payload: string(payload)},
		nil, nil, nil, su, func(string, ...any) {}, testUpgradeBudget, nil)
	if o.State != api.OutcomeDone {
		t.Fatalf("agent-update outcome = %+v, want done", o)
	}
	if su.triggered != "v2" {
		t.Errorf("the unit was handed %q, want the offered v2", su.triggered)
	}
}

// The load-bearing negative: a refused update (a pin below the floor, a bad signature, a failed
// fetch) reports FAILED carrying the unit's own line, and escalates -- and nothing was staged,
// so the running binary is kept. [[verification-assertions-must-fail]]
func TestApplyDirectiveAgentUpdateRefusedEscalates(t *testing.T) {
	su := &fakeSelfUpdater{current: "v1", triggerErr: fmt.Errorf("v0 is older than stable v1 — move stable to go there")}
	fn := &fakeNotifier{}
	payload, _ := json.Marshal(api.AgentUpdate{Version: "v0"})
	o := applyDirective(context.Background(), api.Directive{ID: "u", Kind: api.DirectiveAgentUpdate, Payload: string(payload)},
		nil, fn, nil, su, func(string, ...any) {}, testUpgradeBudget, nil)
	if o.State != api.OutcomeFailed {
		t.Fatalf("a refused update outcome = %+v, want failed", o)
	}
	if !strings.Contains(o.Detail, "older than stable") {
		t.Errorf("the outcome does not carry the unit's verdict: %+v", o)
	}
	if su.triggered != "" || su.isArmed {
		t.Error("a refused update staged/armed something -- refuse-and-stay violated")
	}
	if len(fn.alerts) != 1 || fn.alerts[0].Level != notify.Warning {
		t.Errorf("a refused self-update must escalate one warning, got %+v", fn.alerts)
	}
}

// A re-offer of the version already running is an idempotent no-op (done, no unit run) -- so a
// re-delivered directive after the update committed doesn't loop the agent.
func TestApplyDirectiveAgentUpdateIdempotent(t *testing.T) {
	su := &fakeSelfUpdater{current: "v2"}
	payload, _ := json.Marshal(api.AgentUpdate{Version: "v2"})
	o := applyDirective(context.Background(), api.Directive{ID: "u", Kind: api.DirectiveAgentUpdate, Payload: string(payload)},
		nil, nil, nil, su, func(string, ...any) {}, testUpgradeBudget, nil)
	if o.State != api.OutcomeDone {
		t.Fatalf("re-offer of the running version outcome = %+v, want done", o)
	}
	if su.triggered != "" {
		t.Error("a re-offer of the running version ran the unit -- not idempotent")
	}
}

// A node with no self-updater wired refuses the directive -- fail closed.
func TestApplyDirectiveAgentUpdateNoUpdaterRefuses(t *testing.T) {
	payload, _ := json.Marshal(api.AgentUpdate{Version: "v2"})
	o := applyDirective(context.Background(), api.Directive{ID: "u", Kind: api.DirectiveAgentUpdate, Payload: string(payload)},
		nil, nil, nil, nil, func(string, ...any) {}, testUpgradeBudget, nil)
	if o.State != api.OutcomeFailed {
		t.Errorf("agent-update with no updater = %+v, want failed", o)
	}
}

// ApplyDirective returns the terminal outcome the node reports back -- done on success,
// rolled-back on an upgrade the health-gate reverts, failed when it can't apply.
func TestApplyDirectiveOutcome(t *testing.T) {
	nolog := func(string, ...any) {}

	if o := applyDirective(context.Background(), api.Directive{ID: "1", Kind: api.DirectiveNoop}, nil, nil, nil, nil, nolog, testUpgradeBudget, nil); o.ID != "1" || o.State != api.OutcomeDone {
		t.Errorf("noop outcome = %+v, want done id=1", o)
	}
	if o := applyDirective(context.Background(), api.Directive{ID: "4", Kind: "weird"}, nil, nil, nil, nil, nolog, testUpgradeBudget, nil); o.State != api.OutcomeFailed {
		t.Errorf("unhandled outcome = %+v, want failed", o)
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
		up, nil, nil, nil, func(string, ...any) {}, testUpgradeBudget, nil)
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
		nil, nil, nil, nil, func(string, ...any) {}, testUpgradeBudget, nil)
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
		up, nil, nil, nil, func(string, ...any) {}, testUpgradeBudget, nil)
	if o.State != api.OutcomeFailed || !strings.Contains(o.Detail, "not an overlay") {
		t.Errorf("outcome = %+v, want failed carrying the node's reason", o)
	}
}

func (f *fakeUpgrader) ImageUpgrade(_ context.Context, rel install.Manifest) (bool, error) {
	f.imageTarget = rel
	return f.imageRolledBack, f.imageErr
}
