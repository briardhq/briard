package host

import (
	"context"
	"fmt"
	"time"

	"briard.io/agent/guest"
	"briard.io/agent/guest/entrygate"
	"briard.io/agent/hass"
	"briard.io/shared/manifest"
)

// The per-service readiness registry: which catalogued services the product knows how to judge
// beyond "it answers", and how.
//
// WHY A REGISTRY AND NOT A MANIFEST FIELD ([V3b.29]). A service upgrade that gates on liveness
// alone lets Home Assistant come back, answer /manifest.json with a 200, and serve a house whose
// integrations are half dead. Judging that needs to know what a config entry is, which states are
// terminal, and which are HA's own retry noise — knowledge no generic schema can carry without
// becoming an expression language, and no manifest field should carry anyway, because a manifest
// is a document a publisher writes. A curated catalog is precisely the licence to encode this in
// the product: a small set of services we support, so we can know how each is operated.
//
// The default is the current behaviour. An unknown name gets no assessor, and no assessor is the
// liveness floor alone — exactly what every service got before this existed, and what mosquitto
// and every future entry get on the day they land ([V3b.4]).
//
// The DECISION is here; how to obtain the sample is in the guest (agent/hass), because the token
// and the service's loopback are both there. That is the same line the rest of the product draws
// ([[logic-on-host-by-default]]): identity and decisions on the host, hands in the guest.

// readinessProbe is the slice of the guest an assessor drives — a narrow interface for DI, not a
// seam. Deliberately its own rather than a widening of `upgrader`: an OS upgrade must not be able
// to name a service, and `upgrader` carries nothing that can.
type readinessProbe interface {
	ServiceReadiness(ctx context.Context, name string, port int) ([]hass.Entry, error)
	SupportsServiceReadiness() bool
}

// settleWindow is how long the post-change sample waits before it is judged.
//
// It exists because the signal is a settling state machine, not a level. Home Assistant retries a
// failed integration with backoff, so an entry sampled the instant the service answers is
// routinely in setup_retry for reasons that have nothing to do with the upgrade — and the gate
// reads "was loaded, not loaded now" as a Hold. Waiting is what turns that into a real
// observation: what is still not loaded after the retries have had their run.
//
// A minute is the cost of the gate on every Home Assistant upgrade, and it is paid AFTER the
// service is already serving — the household is not waiting on it, only the directive's outcome
// is. It sits well inside installBudget, which the health gate has already spent most of.
const settleWindow = 60 * time.Second

// sampleBudget bounds one sample. The install window has to be able to finish or revert, and an
// HA that accepts a connection and then never answers must not be able to eat it — the failure
// this bounds is precisely the one the gate exists to catch, so it must not become a hang.
const sampleBudget = 30 * time.Second

// assessorFor returns the differential readiness assessor for a catalogued service, or nil for
// the liveness floor alone.
//
// nil for THREE distinct reasons, all of which mean the same thing to the caller and are worth
// keeping distinct in the log: the product knows nothing about this service; the guest is too old
// to sample one; or the change is a fresh install, which the caller decides (see below). The
// first is the shipped default, the second degrades rather than refuses — an install must not
// fail because the gate above its floor is unavailable.
func (cfg Config) assessorFor(m manifest.Manifest, p readinessProbe, logf func(string, ...any)) guest.ReadinessAssessor {
	switch m.Name {
	case hass.Name:
	default:
		return nil
	}
	if !p.SupportsServiceReadiness() {
		logf("service install %s: this guest cannot sample readiness (no service.readiness); gating on liveness alone", m.Name)
		return nil
	}
	settle := cfg.readinessSettle
	if settle == 0 {
		settle = settleWindow
	}
	return entryAssessor{p: p, name: m.Name, port: m.Primary().Port, settle: settle}
}

// entryAssessor is the config-entry differential gate, wired to a guest that can sample one. The
// verdict logic is entrygate's and is not restated here: this type is the plumbing between a guest
// verb and a pure function.
type entryAssessor struct {
	p      readinessProbe
	name   string
	port   int
	settle time.Duration
}

// Baseline samples the states while the OLD version still serves. It is the confounder control:
// an integration already failing before the upgrade is excluded from the verdict, so only things
// the upgrade broke can trip the gate.
func (a entryAssessor) Baseline(ctx context.Context) (guest.Baseline, error) {
	ctx, cancel := context.WithTimeout(ctx, sampleBudget)
	defer cancel()
	return a.p.ServiceReadiness(ctx, a.name, a.port)
}

// Assess settles, samples again, and hands both to the real gate.
func (a entryAssessor) Assess(ctx context.Context, base guest.Baseline) (guest.Verdict, string, error) {
	pre, ok := base.([]hass.Entry)
	if !ok {
		return "", "", fmt.Errorf("readiness: baseline is %T, not a config-entry sample", base)
	}
	select {
	case <-ctx.Done():
		// Out of budget before the signal could settle. An error, not a verdict: an unsettled
		// sample judged as if it were settled is how a transient becomes a rollback.
		return "", "", fmt.Errorf("readiness: no time left to settle: %w", ctx.Err())
	case <-time.After(a.settle):
	}
	sampleCtx, cancel := context.WithTimeout(ctx, sampleBudget)
	defer cancel()
	post, err := a.p.ServiceReadiness(sampleCtx, a.name, a.port)
	if err != nil {
		return "", "", err
	}
	res := entrygate.Assess(entries(pre), entries(post))
	return verdict(res.Verdict), res.Reason(), nil
}

// entries maps the wire shape onto the gate's. Two types rather than one because they answer to
// different owners: hass.Entry is what Home Assistant's API says, entrygate.Entry is what the
// verdict reasons over, and entrygate is deliberately pure — no HA client, no wire format.
func entries(in []hass.Entry) []entrygate.Entry {
	out := make([]entrygate.Entry, len(in))
	for i, e := range in {
		out[i] = entrygate.Entry{ID: e.ID, Domain: e.Domain, State: entrygate.State(e.State)}
	}
	return out
}

// verdict maps the service-specific gate's decision onto the generic one. The two vocabularies
// happen to spell their values identically today; mapping them explicitly is what keeps that a
// coincidence rather than a dependency, so a second service's gate can name its own.
func verdict(v entrygate.Verdict) guest.Verdict {
	switch v {
	case entrygate.Rollback:
		return guest.VerdictRollback
	case entrygate.Hold:
		return guest.VerdictHold
	default:
		return guest.VerdictPass
	}
}
