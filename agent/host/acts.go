package host

import (
	"context"
	"sync"

	"briard.io/shared/api"
	"briard.io/shared/notify"
)

// THE OBSERVE LOOP KEEPS TICKING WHILE A DIRECTIVE RUNS.
//
// A directive used to run on the loop itself, so a service install held the tick -- and with it
// the guest's liveness observation, the recovery ladder and every CLI or dashboard request -- for
// its whole budget: a guest that froze four seconds into a pull was first noticed when the
// budget expired, twenty-five minutes later. The control channel now serves reads beside an act
// (guestfirmware.ServeFrames), so the directives that only talk to the guest leave the loop.
//
// Three lanes, by what a directive does to the channel the loop reads:
//
//   - RELAUNCHERS stay on the loop: an OS update, `config set`, a pairing, a rescue stop the
//     guest and bring it back on a NEW channel, which the loop then adopts (Run). The loop could
//     not read during one anyway, and running it anywhere else would leave the loop on a channel
//     that is about to die. So would the instant kinds (noop, log, a cert request's keygen), the
//     QMP debug arm and the casa claim, which share the loop's own state.
//   - ACTS run off the loop, ONE AT A TIME: an install, a restore, a handover, the agent update.
//     A second act while one runs is refused with "busy" -- not queued, because a queue holds a
//     household's intent for minutes with nothing to tell it, and a refusal is honest at once.
//   - PULLS run off the loop whenever asked, beside an act: doctor, an app's history, the
//     dashboard code. They are what the household reaches for DURING an install.
//
// Outcomes come back through one channel the loop drains each cycle, so the bookkeeping a
// directive ends with -- the cloud's pending outcome, the CLI's answer, adopting what was
// installed -- stays on the loop, where it always was. The lane outlives any one observe() call
// (it is Run's, like pendingOutcomes): an act that spans a channel bounce finishes on the client
// it started with and still reports through here.
type actLane struct {
	mu      sync.Mutex
	busy    string // the kind of the act in flight, "" when none
	results chan actResult
}

// actResult is a directive's outcome on its way back to the loop, with what the loop needs to
// finish it: the directive (what to adopt) and, for a local one, whom to answer.
type actResult struct {
	d  api.Directive
	o  api.DirectiveOutcome
	rq *localRequest // nil for a cloud directive, whose outcome joins pendingOutcomes
}

func newActLane() *actLane {
	// Buffered past anything in flight: one act plus a handful of pulls, and the loop drains it
	// every cycle. A send never waits on the loop, so a directive's goroutine cannot be held
	// by the loop it is reporting to.
	return &actLane{results: make(chan actResult, 16)}
}

// offLoopActs are the kinds that run off the loop one at a time; offLoopPulls the kinds that run
// off the loop whenever asked. Every other kind runs on the loop (the comment above says why).
var (
	offLoopActs = map[string]bool{
		api.DirectiveServiceInstall: true,
		api.DirectiveServicePrewarm: true,
		api.DirectiveServiceRestore: true,
		api.DirectiveHandover:       true,
		api.DirectiveSync:           true,
		api.DirectiveCert:           true,
		api.DirectiveAgentUpdate:    true,
	}
	offLoopPulls = map[string]bool{
		api.DirectiveDoctor:         true,
		api.DirectiveServiceMembers: true,
		api.DirectiveDashboard:      true,
	}
)

// inFlight reports whether an act is running off the loop. nil-safe: a loop built without a
// lane (tests) runs everything on itself and never has one.
func (a *actLane) inFlight() bool {
	if a == nil {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.busy != ""
}

// claim takes the act slot for kind, or reports what holds it.
func (a *actLane) claim(kind string) (held string, ok bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.busy != "" {
		return a.busy, false
	}
	a.busy = kind
	return "", true
}

func (a *actLane) release() {
	a.mu.Lock()
	a.busy = ""
	a.mu.Unlock()
}

// run dispatches one directive on its lane. It returns the outcome at once for a kind that runs
// on the loop (or an act refused as busy), and ok=false for one it sent off the loop, whose
// outcome arrives later on the lane's results channel.
func (cfg Config) run(ctx context.Context, d api.Directive, o origin, rq *localRequest, r guestReader, up upgrader, n notify.Notifier, cr *certRequester, cs *casaRunner, su selfUpdater, logf func(string, ...any)) (api.DirectiveOutcome, bool) {
	a := cfg.acts
	if a == nil || !(offLoopActs[d.Kind] || offLoopPulls[d.Kind]) {
		return cfg.dispatch(ctx, d, o, r, up, n, cr, cs, su, logf), true
	}
	if offLoopActs[d.Kind] {
		if held, ok := a.claim(d.Kind); !ok {
			logf("directive kind=%s refused: busy with %s", d.Kind, held)
			return api.DirectiveOutcome{ID: d.ID, State: api.OutcomeFailed,
				Detail: "busy: a " + held + " is in progress on this node; try again when it finishes"}, true
		}
	}
	go func() {
		if offLoopActs[d.Kind] {
			defer a.release()
		}
		a.results <- actResult{d: d, o: cfg.dispatch(ctx, d, o, r, up, n, cr, cs, su, logf), rq: rq}
	}()
	return api.DirectiveOutcome{}, false
}

// results is the lane's channel for the loop's select; nil -- never ready -- without a lane.
func (a *actLane) ch() <-chan actResult {
	if a == nil {
		return nil
	}
	return a.results
}

// finish is the loop's end of a directive, wherever it ran: the CLI's answer or the cloud's
// pending outcome, then adopting what an install changed. On the loop, because pendingOutcomes
// and cfg.Services are the loop's own.
func (cfg *Config) finish(res actResult, pending *[]api.DirectiveOutcome, logf func(string, ...any)) {
	if res.rq != nil {
		res.rq.resp <- res.o // answer the CLI first; adopting is bookkeeping it need not wait on
	} else if res.o.ID != "" {
		*pending = append(*pending, res.o)
	}
	cfg.adoptInstalledServices(res.d, res.o, logf)
}
