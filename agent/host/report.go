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
	"fmt"
	"strings"
	"time"

	"briard.io/agent/install"
	"briard.io/shared/api"
	"briard.io/shared/notify"
)

// certRequester holds the node-side state of the CSR renewal handshake. On a
// DirectiveCertRequest it generates a fresh keypair + CSR (the private key stays home), stashes
// the key until the signed cert returns, and queues the CSR to ride the next report up. On the
// DirectiveCert it pairs the returned cert with the stashed key and writes both to the replicated
// volume. In-memory only: a restart drops the stash, and the cloud re-requests a CSR on its next
// tick.
type certRequester struct {
	name       string // the DNS name in flight
	keyPEM     string // the stashed private key awaiting its cert
	pendingCSR []byte // DER CSR to upload on the next report; cleared once uploaded
}

// request generates a keypair + CSR for name, stashing the key and queuing the CSR upload.
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

// keyFor returns the stashed key paired with name (a cert just arrived), or "" when the node
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
// ImageUpgrade is the whole-OS upgrade, and not guest.Manager's: it rolls back by putting the
// previous OS image back, which only the host holds, and its reboot replaces the channel a
// Manager is bound to. It reports rolledBack separately from err because a node healthy on its
// old code and a node the host could not finish with are different news, and the cloud acts on
// them differently.
//
// No method takes an AppSpec, and that is load-bearing rather than tidy: an OS upgrade must not
// touch apps, and a signature with nothing to name an app with is a separation the compiler
// keeps rather than one a reader has to.
type upgrader interface {
	ImageUpgrade(ctx context.Context, rel install.Manifest) (rolledBack bool, err error) // the guest chain's release, image already staged
	WriteCert(ctx context.Context, cert, key string) error                               // apply a renewed cert to the volume
	// RescueGuest restarts the guest on its verified, read-only image, the one recovery rung that
	// is never a reflex. It is on this interface, not beside it, because it performs the same
	// VM+channel+Manager swap the upgrade legs do, and a second owner of that swap would be a
	// second way to do it.
	RescueGuest(ctx context.Context) error
}

// selfUpdater is the slice of the host-agent self-update the agent-update directive drives,
// narrow so the dispatch is unit-testable without a real binary/systemd. Trigger hands the
// offered version to the frozen update unit below the agent and returns once the run is started:
// that run fetches, verifies, stops this agent, swaps and starts the new one, and its verdict is
// the update result the alerter reads. Current is the running agent version, so a re-offer of the
// same version is an idempotent no-op.
type selfUpdater interface {
	Trigger(ctx context.Context, version string) (string, error)
	Current() string
}

// applyDirective is dispatch's fallthrough: noop, log, rescue, cert-request, cert and
// agent-update, from the cloud or the local door; any other kind is logged and fails as
// unhandled. Which lane each runs on is run's decision (acts.go). It returns the op's terminal
// outcome, done or failed, which the caller reports back so the cloud moves the intent terminal:
// the durable answer a post-outage reconcile polls. A re-offer of the running agent version is a
// no-op that reports done. It deliberately takes no app: nothing here acts on one, and a version
// change is an app-install directive carrying a catalog name (applyAppInstall).
func applyDirective(ctx context.Context, d api.Directive, up upgrader, n notify.Notifier, cr *certRequester, su selfUpdater, logf func(string, ...any), upgradeBudget time.Duration, wd *beat) api.DirectiveOutcome {
	done := api.DirectiveOutcome{ID: d.ID, State: api.OutcomeDone}
	failed := func(detail string) api.DirectiveOutcome {
		return api.DirectiveOutcome{ID: d.ID, State: api.OutcomeFailed, Detail: detail}
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
			return failed("no VM on this machine")
		}
		// The bound is the bring-up budget plus room for the stop, generous on purpose: past the
		// stop the guest is down, so a context that expires mid-bring-up leaves a node needing
		// another rescue, not one that reverted. There is nothing to revert to; that is the nature
		// of this rung, and the reason it is never automatic.
		rctx, cancel := wd.budget(ctx, upgradeBudget)
		defer cancel()
		logf("directive kind=rescue: restarting the guest on its image")
		if err := up.RescueGuest(rctx); err != nil {
			logf("directive rescue failed: %v", err)
			return failed(err.Error())
		}
		logf("directive rescue applied: the guest was rebuilt and re-converged")
		return done
	case api.DirectiveCertRequest:
		if up == nil || d.Payload == "" || cr == nil {
			logf("directive kind=cert-request ignored (no payload/target on this node)")
			return failed("no target on this machine")
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
			return failed("no target on this machine")
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
			logf("directive kind=agent-update ignored (no self-updater/payload on this node)")
			return failed("no self-updater on this machine")
		}
		var u api.AgentUpdate
		if err := json.Unmarshal([]byte(d.Payload), &u); err != nil || u.Version == "" {
			logf("directive agent-update: bad payload: %v", err)
			return failed("bad payload")
		}
		if u.Version == su.Current() {
			logf("directive agent-update: already running %s; nothing to do", u.Version)
			return done // idempotent: a re-offer of the running version is a no-op
		}
		// The cloud trigger: write the target and start the frozen unit, which pulls a fresh agent
		// from the channel and lets that binary run the update, so a fetch bug in this running
		// binary cannot prevent its own replacement. Started, not waited on: the run stops this
		// agent, and the stop finishes the act in flight first, which would be this one. So the
		// outcome is "started"; whether the node moved is NodeStatus.AgentVersion, and a run that
		// refused is the update result the alerter reports.
		sctx, cancel := wd.budget(ctx, 30*time.Second)
		defer cancel()
		logf("directive kind=agent-update: converging to %s via the update unit", u.Version)
		line, err := su.Trigger(sctx, u.Version)
		if err != nil {
			logf("directive agent-update: could not start the update unit: %v", err)
			escalate(ctx, n, logf, "briard-agent", "agent self-update", u.Version, err)
			return failed(err.Error())
		}
		logf("directive agent-update: %s", line)
		return done
	default:
		logf("directive kind=%q unhandled", d.Kind)
		return failed("unhandled kind " + d.Kind)
	}
}

// escalate raises an alert when a change to what the node runs fails: an app install or update,
// a guest OS update or image stage, a guest bundle push, the agent's self-update, a cert renewal.
// These are rare, so this is not fatigue, and a failure includes a revert that could not finish,
// exactly the case a bound exists to surface instead of hanging silently. It goes through the
// alert store like every alert, so on the free tier, with no endpoint to push to, the record is
// still there for the person who looks after "it stopped working".
func escalate(
	ctx context.Context, n notify.Notifier, logf func(string, ...any), subject, kind, target string, cause error) {
	fireAlert(ctx, n, logf, notify.Alert{
		// An event: the rollback already happened, there is nothing to resolve. The instance
		// is the kind of upgrade, so a display can tell "the OS keeps failing" from one bad run.
		Key:      "upgrade-rolled-back:" + strings.ReplaceAll(strings.ToLower(kind), " ", "-"),
		Kind:     notify.Event,
		Severity: notify.Info,
		Title:    "Briard: " + kind + " failed",
		Body:     fmt.Sprintf("%s: %s to %s failed and rolled back — %v", subject, kind, target, cause),
	})
}
