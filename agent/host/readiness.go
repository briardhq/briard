package host

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"briard.io/agent/guest"
	"briard.io/agent/guest/entrygate"
	"briard.io/agent/hass"
	"briard.io/agent/mosquitto"
	"briard.io/shared/manifest"
)

// The per-app readiness registry: which catalogued apps the product knows how to judge beyond
// "it answers", and how. A registry, not a manifest field: an app upgrade gated on liveness alone
// lets Home Assistant come back, answer /manifest.json with a 200, and serve a house whose
// integrations are half dead. Judging that needs to know what a config entry is, which states are
// terminal, and which are HA's own retry noise: knowledge no generic schema can carry without
// becoming an expression language, and no manifest field should carry anyway, because a manifest
// is a document a publisher writes. A curated catalog (a small set of apps we support, so we can
// know how each is operated) is precisely the licence to encode this in the product. An unknown
// name gets no assessor, which is the liveness floor alone: what every app without an entry here
// gets, including a new one on the day it lands.
//
// The decision is here; obtaining the sample is the guest's (agent/hass), where the token and the
// app's loopback both are: the line the rest of the product draws, identity and decisions on the
// host, hands in the guest.

// readinessProbe is the slice of the guest the registry's assessors drive — a narrow interface for
// DI, not a seam. Deliberately its own rather than a widening of `upgrader`: an OS upgrade must not
// be able to name an app, and `upgrader` carries nothing that can. One method per app that has a
// signal, named for it, because each speaks one app's API and returns that app's own shape; the
// generic thing is the assessor interface (guest.ReadinessAssessor), which knows none of this.
//
// Deliberately no capability checks: the agent and the guest closure are published together and
// the alpha reinstalls rather than upgrading in place, so a guest that does not know one of these
// verbs is a mismatched pair, a fault to see, not a configuration to tolerate. Not to be confused
// with a different rule: a sample that fails at runtime leaves the install on the liveness floor,
// because S1 must never revert a household's app on the strength of its own telemetry breaking.
type readinessProbe interface {
	HassReadiness(ctx context.Context, port int) ([]hass.Entry, error)
	MosquittoProbe(ctx context.Context, token string) (mosquitto.Sample, error)
}

// heldProbe asks whichever client the install holds when the sample is taken, not the one it
// held when the gate was built: the health gate between the two may have followed a reconnect.
type heldProbe struct{ g *appInstaller }

func (p heldProbe) HassReadiness(ctx context.Context, port int) ([]hass.Entry, error) {
	return (*p.g).HassReadiness(ctx, port)
}

func (p heldProbe) MosquittoProbe(ctx context.Context, token string) (mosquitto.Sample, error) {
	return (*p.g).MosquittoProbe(ctx, token)
}

// settleWindow is how long the post-change sample waits before it is judged, because the signal
// is a settling state machine, not a level. Home Assistant retries a failed integration with
// backoff, so an entry sampled the instant the app answers is routinely in setup_retry for
// reasons unrelated to the upgrade, and the gate reads "was loaded, not loaded now" as a Hold.
// Waiting turns that into a real observation: what is still not loaded after the retries have
// had their run. A minute is the cost of the gate on every Home Assistant upgrade, paid after the
// app is already serving: the household is not waiting on it, only the directive's outcome is.
// It sits well inside installBudget.
const settleWindow = 60 * time.Second

// sampleBudget bounds one sample. The install window has to be able to finish or revert, and an
// HA that accepts a connection and then never answers must not be able to eat it: that is
// precisely the failure the gate exists to catch, so it must not become a hang.
const sampleBudget = 30 * time.Second

// assessorFor returns the differential readiness assessor for a catalogued app, or nil -- the
// liveness floor alone -- for an app the product knows nothing about. A fresh install gets the
// floor too, but that is the caller's decision: it captures no baseline (applyOneAppInstall).
func (cfg Config) assessorFor(m manifest.Manifest, p readinessProbe, logf func(string, ...any)) guest.ReadinessAssessor {
	settle := cfg.readinessSettle
	switch m.Name {
	case hass.Name:
		if settle == 0 {
			settle = settleWindow
		}
		return entryAssessor{p: p, name: m.Name, port: m.Primary().Port, settle: settle}
	case mosquitto.Name:
		if settle == 0 {
			settle = probeSettle
		}
		return &probeAssessor{p: p, name: m.Name, settle: settle}
	default:
		return nil
	}
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

// Baseline samples the states while the old version still serves. It is the confounder control:
// an integration already failing before the upgrade is excluded from the verdict, so only things
// the upgrade broke can trip the gate.
func (a entryAssessor) Baseline(ctx context.Context) (guest.Baseline, error) {
	ctx, cancel := context.WithTimeout(ctx, sampleBudget)
	defer cancel()
	return a.p.HassReadiness(ctx, a.port)
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
	post, err := a.p.HassReadiness(sampleCtx, a.port)
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

// verdict maps the app-specific gate's decision onto the generic one. The two vocabularies
// happen to spell their values identically today; mapping them explicitly is what keeps that a
// coincidence rather than a dependency, so a second app's gate can name its own.
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

// probeSettle bounds how long a broker gets to start accepting clients before the gate judges it.
// It is a poll, not a wait: the sample is retried until the app answers or this expires, so a
// healthy upgrade pays only what it needs. Home Assistant's minute is a settle window because its
// signal is a state machine that retries with backoff; a broker either accepts a connection or it
// does not, and by the time this runs the liveness floor has already seen the app answer.
const probeSettle = 30 * time.Second

// probeRetry is how often the poll asks again. Short enough that a broker that comes up in two
// seconds is judged in two seconds.
const probeRetry = 2 * time.Second

// probeAssessor is the S1 gate for an app whose work is invisible to a sample: it leaves a token
// in the app's own durable state before the change and looks for it after. Its two findings are
// different failures with the same symptom at the floor (an app that answers), kept apart on
// purpose:
//
//   - Not serving: nothing can connect. The management endpoint the floor probes is not the port
//     clients use, so a broker that refuses every client answers the floor perfectly.
//   - Serving, token gone: it came back without the state it had. For a broker that is every
//     retained message the household had — device availability, the last reading of a sensor
//     that publishes daily, the discovery topics HA rebuilds its entities from.
//
// Both are Rollback, a judgement call: Hold would keep an app the household cannot use (or has
// silently lost data from) while it waits for someone to notice, and the pre-upgrade snapshot is
// exactly the state that would fix it. The false positive that would make Rollback dangerous is
// closed at the source rather than hedged here: the baseline is accepted only once the token has
// round-tripped and the broker has been made to persist it (agent/mosquitto's SIGUSR1), so "the
// token is gone" cannot mean "the token was never written down".
type probeAssessor struct {
	p      readinessProbe
	name   string
	settle time.Duration
}

// Baseline stores a fresh token in the app and confirms the app is serving. A fresh token every
// time, not decoration: the topic is retained, so a value left by a previous upgrade would still
// be there, and a broker that lost everything since would hand it back, turning total data loss
// into a Pass. Randomness makes the answer evidence of this upgrade's round trip.
//
// An error here degrades the install to the liveness floor, the right answer for both ways it
// can fail: an app not serving clients before the change is a confounder, not a regression, and
// a probe that could not run is the gate's own telemetry breaking.
func (a *probeAssessor) Baseline(ctx context.Context) (guest.Baseline, error) {
	ctx, cancel := context.WithTimeout(ctx, sampleBudget)
	defer cancel()
	token, err := newToken()
	if err != nil {
		return nil, err
	}
	s, err := a.p.MosquittoProbe(ctx, token)
	if err != nil {
		return nil, err
	}
	if !s.Serving {
		return nil, fmt.Errorf("readiness: %s was not serving clients before the change; nothing to compare against", a.name)
	}
	if s.Token != token {
		// The write did not round-trip. Not a verdict about the app: it is this gate failing
		// to establish a baseline, and it must not be dressed up as a regression later.
		return nil, fmt.Errorf("readiness: %s did not store the probe token", a.name)
	}
	return token, nil
}

// Assess polls until the app is serving again, then asks whether it still holds the token.
func (a *probeAssessor) Assess(ctx context.Context, base guest.Baseline) (guest.Verdict, string, error) {
	want, ok := base.(string)
	if !ok || want == "" {
		return "", "", fmt.Errorf("readiness: baseline is %T, not a probe token", base)
	}
	deadline := time.Now().Add(a.settle)
	var last mosquitto.Sample
	for {
		sampleCtx, cancel := context.WithTimeout(ctx, sampleBudget)
		s, err := a.p.MosquittoProbe(sampleCtx, "")
		cancel()
		if err != nil {
			// The probe itself broke. Never a verdict — S1 does not revert a household's app
			// because the gate could not ask.
			return "", "", err
		}
		last = s
		if s.Serving || time.Now().After(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			return "", "", fmt.Errorf("readiness: no time left to settle: %w", ctx.Err())
		case <-time.After(probeRetry):
		}
	}
	switch {
	case !last.Serving:
		return guest.VerdictRollback,
			fmt.Sprintf("%s answers its health endpoint but accepts no clients", a.name), nil
	case last.Token != want:
		return guest.VerdictRollback,
			fmt.Sprintf("%s came back without the state it had (its retained messages are gone)", a.name), nil
	default:
		return guest.VerdictPass, "", nil
	}
}

// newToken mints the probe value. crypto/rand because the only property that matters is that it
// cannot coincide with a value already on the topic — including one this code wrote before.
func newToken() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("readiness: mint a probe token: %w", err)
	}
	return "briard-" + hex.EncodeToString(b[:]), nil
}
