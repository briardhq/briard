package host

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"
	"time"

	"briard.io/agent/guest"
	"briard.io/agent/guestagent"
	"briard.io/shared/api"
	"briard.io/shared/notify"
)

// certRequester holds the node-side state of the CSR renewal handshake. On a
// DirectiveCertRequest it generates a fresh keypair + CSR -- the private key stays home
// ([[cloud-issues-certs-not-node]]) -- stashing the key until the signed cert returns and
// queuing the CSR to ride the next report up. On the DirectiveCert it pairs the returned
// cert with the stashed key and writes both to the replicated volume. In-memory only: a
// restart drops the stash, and the cloud simply re-requests a CSR on its next tick.
type certRequester struct {
	name       string // the DNS name in flight
	keyPEM     string // the stashed private key awaiting its cert
	pendingCSR []byte // DER CSR to upload on the next report; cleared once uploaded
}

// Request generates a keypair + CSR for name, stashing the key and queuing the CSR upload.
func (cr *certRequester) request(name string) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject:  pkix.Name{CommonName: name},
		DNSNames: []string{name},
	}, key)
	if err != nil {
		return err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	cr.name = name
	cr.keyPEM = string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
	cr.pendingCSR = der
	return nil
}

// KeyFor returns the stashed key paired with name (a cert just arrived), or "" when the node
// holds no matching key -- e.g. a restart lost the stash, so the cloud must re-request.
func (cr *certRequester) keyFor(name string) string {
	if cr != nil && cr.name == name {
		return cr.keyPEM
	}
	return ""
}

// upgrader is the slice of the guest binding the upgrade directives drive -- narrow so the
// dispatch is unit-testable without a live control channel.
//
// Upgrade and RebootUpgrade are the two methods of a whole-OS upgrade (decides which),
// and neither is guest.Manager's: both roll back to a snapshot of the OS disk, which only the
// host can take or restore, and the reboot one also outlives the channel a Manager is bound
// to. They report rolledBack separately from err because a node healthy on its old code and a
// node the host could not finish with are different news, and the cloud acts on them
// differently.
//
// Neither takes a ServiceSpec, and that is load-bearing rather than tidy: an OS upgrade must
// not touch services, and a signature with nothing to name a service with is a
// separation the compiler keeps rather than one a reader has to.
type upgrader interface {
	Upgrade(ctx context.Context, target string) (rolledBack bool, err error)
	RebootUpgrade(ctx context.Context, target string) (rolledBack bool, err error)
	Stage(ctx context.Context, closure string, src guestagent.StageSource) error             //b: pull the closure in BEFORE anything switches to it
	ActivationMethod(ctx context.Context, target string) (guest.Activation, []string, error) // V3.17c1: switch-only or reboot-only, decided before activating
	WriteCert(ctx context.Context, cert, key string) error                                   //: apply a renewed cert to the vol
	// RescueGuest rebuilds the guest from the verified image under its overlay (B.10) -- the one
	// recovery rung that is never a reflex. It is on this interface rather than beside it because
	// it performs the same VM+channel+Manager swap the upgrade legs do, and a second owner of that
	// swap would be a second way to do it.
	RescueGuest(ctx context.Context) error
}

// selfUpdater is the slice of the host-agent self-update the agent-update directive drives
// -- narrow so the dispatch is unit-testable without a real binary/systemd. Stage fetches +
// verifies + arms a signed agent binary (refuse-and-stay on a bad signature: current kept); the
// host loop then triggers Restart once the outcome is acked, so the Type=notify pivot
// trials it (Armed reports whether a trial is pending). Current is the running agent version, so
// a re-offer of the same version is an idempotent no-op. nil on a node with no release keyring
// provisioned -> the directive refuses.
type selfUpdater interface {
	Stage(ctx context.Context, u api.AgentUpdate) error
	Armed() bool
	Restart(ctx context.Context) error
	Current() string
}

// applyDirective acts on a directive from the controller (down-channel). Runs synchronously in
// the observe loop: an upgrading node is legitimately "busy" and its status stalls until it
// finishes (the controller then reads it degraded, which it is). Unknown kinds: logged.
// applyDirective returns the op's terminal outcome: done, rolled-back (an upgrade the
// health-gate reverted), or failed (couldn't apply / no clean revert). The caller reports it
// back so the cloud moves the intent terminal -- the durable answer a post-outage reconcile
// polls. Re-delivery of an already-applied directive is an idempotent no-op that re-reports done.
//
// IT TAKES NO SERVICE, and that is the shape [V3b.3](e1) left behind: nothing here acts on one.
// A version change is a service-install directive carrying a catalog name (applyServiceInstall),
// an OS upgrade must not touch services at all, and the image re-pin that needed a single named
// service died with the build-time payload slot.
func applyDirective(ctx context.Context, d api.Directive, up upgrader, n notify.Notifier, cr *certRequester, su selfUpdater, logf func(string, ...any), upgradeBudget time.Duration, wd *beat) api.DirectiveOutcome {
	done := api.DirectiveOutcome{ID: d.ID, State: api.OutcomeDone}
	failed := func(detail string) api.DirectiveOutcome {
		return api.DirectiveOutcome{ID: d.ID, State: api.OutcomeFailed, Detail: detail}
	}
	rolledBack := func(detail string) api.DirectiveOutcome {
		return api.DirectiveOutcome{ID: d.ID, State: api.OutcomeRolledBack, Detail: detail}
	}
	switch d.Kind {
	case api.DirectiveNoop:
		logf("directive kind=noop acked")
		return done
	case api.DirectiveLog:
		logf("directive kind=log payload=%q", d.Payload)
		return done
	case api.DirectiveRescue:
		if up == nil {
			logf("directive kind=rescue ignored (no guest on this node)")
			return failed("no guest on this node")
		}
		// The bound is the bring-up budget plus room for the stop, and it is generous on purpose:
		// past the rebuild the old overlay is GONE, so a context that expires mid-bring-up leaves
		// a node needing another rescue rather than one that reverted. There is nothing to revert
		// to -- that is the nature of this rung, and the reason it is never automatic.
		rctx, cancel := wd.budget(ctx, upgradeBudget)
		defer cancel()
		logf("directive kind=rescue: rebuilding the guest from its backing image")
		if err := up.RescueGuest(rctx); err != nil {
			logf("directive rescue failed: %v", err)
			return failed(err.Error())
		}
		logf("directive rescue applied: the guest was rebuilt and re-converged")
		return done
	case api.DirectiveUpgradeSystem:
		// An OS upgrade needs a TARGET and an UPGRADER -- not a service. The old guard also
		// required spec.Name, which silently made the SHIPPED node un-upgradable: a fresh
		// install has the zero ServiceSpec, so every OS update it was ever offered came back
		// "failed". That condition dates from when every anchor carried a service and
		// "no service" once meant "nothing to upgrade"; zero services is the shipped state
		// and the two stopped being the same statement. The system closure is a property of
		// the NODE, so what happens to run on top of it cannot be a precondition for updating it.
		if up == nil || d.Payload == "" {
			logf("directive kind=upgrade-system ignored (no target/upgrader on this node)")
			return failed("no target/upgrader on this node")
		}
		// The whole-OS switch is heavier than a service install (activation, service
		// restarts), so give it a longer bound. The same bound covers staging, which is
		// the one part that goes to the network (tens of MB).
		//
		// It is ALSO the wait a node spends degraded when the gate never passes, because
		// AwaitOSReady polls until this context ends — so the number is a product property
		// rather than a timeout detail. Config.UpgradeBudget carries that reasoning and the
		// bounds on lowering it.
		uctx, cancel := wd.budget(ctx, upgradeBudget)
		defer cancel()
		// Pull the closure in first. This is deliberately OUTSIDE the upgrade:
		// a stage failure means the bytes never arrived, so nothing was quiesced,
		// snapshotted or switched — the node keeps running what it ran, and the directive
		// reports failed rather than rolled-back. Folding the fetch into Upgrade would
		// dress a download error up as a failed upgrade and put a healthy node through a
		// rollback it never needed.
		logf("directive kind=upgrade-system: staging %s", d.Payload)
		if err := up.Stage(uctx, d.Payload, guestagent.StageSource{}); err != nil {
			logf("directive upgrade-system: staging failed, not switching (node unchanged): %v", err)
			escalate(ctx, n, logf, "this node", "OS stage", d.Payload, err)
			return failed(err.Error())
		}
		// Decide HOW to activate before touching anything. Committing
		// up front is the whole rule: a switch that discovers halfway through that it needed
		// a boot has already replaced the running services, leaving no honest rollback point
		// and a health-gate judging a state that is neither generation.
		method, reasons, err := up.ActivationMethod(uctx, d.Payload)
		if err != nil {
			logf("directive upgrade-system: could not determine activation method, not switching: %v", err)
			escalate(ctx, n, logf, "this node", "OS activation check", d.Payload, err)
			return failed(err.Error())
		}
		if method != guest.ActivateSwitch {
			// A kernel/initrd/systemd/params change cannot be applied in band -- doing so
			// would leave the guest running userland from one generation on a kernel from
			// another -- so it goes the heavy way: stage the boot, stop cleanly, snapshot
			// the OS disk, come back up on the target, and gate the fresh boot.
			logf("directive kind=upgrade-system: reboot into %s (%s changed)", d.Payload, strings.Join(reasons, ", "))
			back, err := up.RebootUpgrade(uctx, d.Payload)
			switch {
			case errors.Is(err, ErrHandoverRequired):
				// Not an incident: the node looked, saw a peer that could take the work, and
				// declined to fail itself over unattended. Nothing moved, so there is
				// nothing for an owner to do and no alert -- an HA pair would otherwise mail
				// them on every OS release. The cloud is the audience here: it holds the flock
				// view and is what schedules the handover.
				//
				// Reported as rolled-back because that is the closest TRUE thing in the existing
				// outcome set (the node is healthy on its old code) and shared/api is a closed
				// allowlist, not a place to add an enum value in passing. A distinct "declined"
				// state is the honest shape and belongs with the cloud work that will read it.
				logf("directive upgrade-system DECLINED, node untouched and serving: %v", err)
				return rolledBack(err.Error())
			case err != nil && back:
				logf("directive upgrade-system rolled back: %v", err)
				escalate(ctx, n, logf, "this node", "OS upgrade (reboot)", d.Payload, err)
				return rolledBack(err.Error())
			case err != nil:
				// The node did NOT come back on the target and was not returned to where it
				// started -- the one outcome that needs a human, so do not dress it up as a
				// rollback the way a switch failure is allowed to.
				logf("directive upgrade-system FAILED without a clean rollback: %v", err)
				escalate(ctx, n, logf, "this node", "OS upgrade (reboot)", d.Payload, err)
				return failed(err.Error())
			}
			logf("directive upgrade-system applied: rebooted into %s", d.Payload)
			return done
		}
		logf("directive kind=upgrade-system: switch to %s", d.Payload)
		// Same three outcomes as the reboot method, for the same reason: since the
		// switch path's rollback also goes through the OS disk, "it failed and the node is back
		// on its old code" and "it failed and the node needs a human" are once again different
		// answers, and this used to report both as a rollback.
		back, err := up.Upgrade(uctx, d.Payload)
		switch {
		case err != nil && back:
			logf("directive upgrade-system rolled back: %v", err)
			escalate(ctx, n, logf, "this node", "OS upgrade", d.Payload, err)
			return rolledBack(err.Error())
		case err != nil:
			logf("directive upgrade-system FAILED without a clean rollback: %v", err)
			escalate(ctx, n, logf, "this node", "OS upgrade", d.Payload, err)
			return failed(err.Error())
		}
		logf("directive upgrade-system applied: now running %s", d.Payload)
		return done
	case api.DirectiveCertRequest:
		if up == nil || d.Payload == "" || cr == nil {
			logf("directive kind=cert-request ignored (no payload/target on this node)")
			return failed("no target on this node")
		}
		if err := cr.request(d.Payload); err != nil {
			logf("directive cert-request (%s): could not build CSR: %v", d.Payload, err)
			return failed(err.Error())
		}
		logf("directive cert-request: generated a keypair + CSR for %s (uploads on the next report)", d.Payload)
		return done
	case api.DirectiveCert:
		if up == nil || d.Payload == "" {
			logf("directive kind=cert ignored (no payload/target on this node)")
			return failed("no target on this node")
		}
		var b api.CertBundle
		if err := json.Unmarshal([]byte(d.Payload), &b); err != nil {
			logf("directive cert: bad payload: %v", err)
			return failed("bad payload")
		}
		key := cr.keyFor(b.Name)
		if key == "" {
			logf("directive cert (%s): no matching key held -- awaiting a fresh cert-request", b.Name)
			return failed("no matching key held")
		}
		cctx, cancel := wd.budget(ctx, 30*time.Second)
		defer cancel()
		if err := up.WriteCert(cctx, b.Cert, key); err != nil {
			logf("directive cert (%s) failed: %v", b.Name, err)
			escalate(ctx, n, logf, b.Name, "cert renewal", b.Name, err)
			return failed(err.Error())
		}
		logf("directive cert applied: renewed cert for %s written to the volume", b.Name)
		return done
	case api.DirectiveAgentUpdate:
		if su == nil || d.Payload == "" {
			logf("directive kind=agent-update ignored (no self-updater/keyring/payload on this node)")
			return failed("no self-updater on this node")
		}
		var u api.AgentUpdate
		if err := json.Unmarshal([]byte(d.Payload), &u); err != nil {
			logf("directive agent-update: bad payload: %v", err)
			return failed("bad payload")
		}
		if u.Version != "" && u.Version == su.Current() {
			logf("directive agent-update: already running %s; nothing to do", u.Version)
			return done // idempotent: a re-offer of the running version is a no-op
		}
		// Bound the fetch+verify; staging is atomic, so a timeout here can't leave a torn slot.
		sctx, cancel := wd.budget(ctx, 10*time.Minute)
		defer cancel()
		logf("directive kind=agent-update: fetch+verify %s (%s)", u.Version, u.URL)
		if err := su.Stage(sctx, u); err != nil {
			// Refuse-and-stay: a bad/absent signature or a failed fetch keeps the running binary.
			logf("directive agent-update refused (current kept): %v", err)
			escalate(ctx, n, logf, "briard-agent", "agent self-update", u.Version, err)
			return failed(err.Error())
		}
		// Staged + armed. The host loop restarts the unit once this outcome is acked, so the
		// The pivot trials it; convergence is confirmed via NodeStatus.AgentVersion, not here.
		logf("directive agent-update: staged + armed %s (restart pending)", u.Version)
		return done
	default:
		logf("directive kind=%q unhandled", d.Kind)
		return failed("unhandled kind " + d.Kind)
	}
}

// escalate pushes an alert when an upgrade fails: upgrades are rare, so this is not fatigue --
// and a failure includes a revert that could not finish, which is exactly the case a bound exists
// to surface instead of hanging silently.
//
// IT WRITES THE LOCAL TRAIL FIRST, and that ordering is the point rather than a detail. This
// function used to do nothing BUT hand the alert to the notifier -- which on the free tier is
// notify.Nop(), because there is no cloud contact to configure one from. So every failed OS
// upgrade, service upgrade, cert renewal and agent self-update on a free node produced an alert
// that reached NOBODY: not the owner, not the journal, not a support bundle. The one place a
// person looks after "it stopped working" held no record that briard had noticed anything.
//
// The redundancy alerter had this line from the start (alert.go); this path simply never grew
// it, and nothing failed loudly enough to say so -- an alert nobody receives looks exactly like
// an alert nobody needed to receive.
//
// A nil notifier (a witness) still logs: it has no owner to push to, but it has a journal.
func escalate(ctx context.Context, n notify.Notifier, logf func(string, ...any), subject, kind, target string, cause error) {
	al := notify.Alert{
		Level: notify.Warning,
		Title: "Briard: " + kind + " failed",
		Body:  fmt.Sprintf("%s: %s to %s failed and rolled back — %v", subject, kind, target, cause),
	}
	logf("%s", notify.LogLine(al))
	if n == nil {
		return
	}
	nctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_ = n.Notify(nctx, al)
}
