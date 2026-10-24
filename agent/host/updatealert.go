package host

import (
	"context"
	"fmt"
	"time"

	"briard.io/agent/selfupdate"
	"briard.io/shared/notify"
)

// updateAlerter is the reader the timer path never had.
//
// EVERY OTHER TRIGGER OF THE FROZEN UPDATE UNIT HANDS ITS VERDICT TO SOMEBODY: `briard update`
// prints the line to whoever typed it. The TIMER does not -- and the timer is the one that runs on
// every node, nightly, and is the whole of updating on a standalone home. A run that fails ends in
// that unit's journal and stops there, so a household that has silently stopped updating would
// look exactly like one that is current.
//
// That is the failure the upgrade floor exists to prevent, arriving through the other door. A
// node below the floor is refused BY DESIGN, keeps serving, and is supposed to be reinstalled --
// and without this nobody is ever told to reinstall it.
//
// ⚠️ THE READER IS THE AGENT, NOT THE UNIT. The frozen layer carries no product knowledge: the
// update script leaves its run's verdict (selfupdate.Result), and what "this node has stopped
// updating" MEANS to a household is the product's to say, not a shell script's. The verdict is a
// file rather than the unit's systemd state so the same reader serves a service manager that keeps
// no such state.
type updateAlerter struct {
	n     notify.Notifier
	node  string
	lay   selfupdate.Layout // the run's verdict (read, never consumed)
	logf  func(string, ...any)
	start time.Time        // when this agent began watching: the clock for "no run at all"
	now   func() time.Time // overridable in tests
}

// updateCheckEvery is how often the verdict is read. The unit runs once a night, so this is not
// a race to catch a transition -- it is how long a node that has stopped updating stays quiet
// about it, and an hour is comfortably inside the next run.
const updateCheckEvery = time.Hour

// updateStaleAfter is how long a node may go without a finished run before that is itself the
// failure: the timer fires daily and at boot, so two days is one missed night with room.
const updateStaleAfter = 48 * time.Hour

func newUpdateAlerter(n notify.Notifier, node string, lay selfupdate.Layout, logf func(string, ...any)) *updateAlerter {
	return &updateAlerter{n: n, node: node, lay: lay, logf: logf, start: time.Now(), now: time.Now}
}

// watchUpdates reads the update run's verdict on a slow cadence for the life of the agent.
// Started once, beside the guest update timer, and on EVERY node -- a witness included. A node
// that can no longer update is outside its support window whatever else it is, and that is not a
// statement about replicas, services or role.
func (cfg Config) watchUpdates(ctx context.Context, n notify.Notifier, logf func(string, ...any)) {
	a := newUpdateAlerter(n, cfg.Node, selfupdate.New(cfg.UpdateBase, cfg.UpdateRunDir), logf)
	t := time.NewTicker(updateCheckEvery)
	defer t.Stop()
	for {
		a.observe(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// observe reads the last run's verdict and asserts it; the alert store turns that into the one
// alert on entry and one on clearing. A failed run is failing; so is a verdict older than
// updateStaleAfter, or none at all once this agent has watched that long (the run dir is tmpfs,
// so a fresh boot has none until the timer's boot run). Otherwise no verdict yet holds.
//
// IT DOES NOT PRIME, and that is the opposite of the redundancy alerter's rule on purpose. The
// startup reading is the case this whole thing was built for: a floored node's agent is the one
// agent that never gets replaced, so "already failing when I came up" is the steady state, not
// a false positive, and a primed alerter would be silent about it forever. The store is what
// keeps that first reading from being announced again at every restart while the state holds.
func (a *updateAlerter) observe(ctx context.Context) {
	r, ok := a.lay.ReadResult()
	now := a.now()
	switch {
	case ok && !r.OK:
		fireAlert(ctx, a.n, a.logf, a.alertFor(true, "The update run said: "+r.Line))
	case ok && now.Sub(r.At) > updateStaleAfter:
		fireAlert(ctx, a.n, a.logf, a.alertFor(true, "No update check has finished since "+r.At.Format(time.DateTime)+"."))
	case ok:
		fireAlert(ctx, a.n, a.logf, a.alertFor(false, ""))
	case now.Sub(a.start) > updateStaleAfter:
		fireAlert(ctx, a.n, a.logf, a.alertFor(true, "No update check has finished since this node last started."))
	}
}

// alertFor is what the household is actually told. The warning names the CONSEQUENCE rather
// than the unit -- "this node has stopped updating" is the fact an owner can act on -- and it
// carries why, which for a failed run is the run's own last line: that is where the remedy lives,
// and a floored node's refusal says in so many words that it must be reinstalled
// ([install.ErrTooOldToUpgrade]).
func (a *updateAlerter) alertFor(failing bool, why string) notify.Alert {
	if !failing {
		return notify.Alert{
			Key:   "updates",
			Kind:  notify.Resolved,
			Title: "Briard: updates are working again",
			Body: fmt.Sprintf("node %s completed an update check; it is following its release channel again.",
				a.node),
		}
	}
	return notify.Alert{
		Key:      "updates",
		Kind:     notify.Open,
		Severity: notify.Warning,
		Title:    "Briard: this node has stopped updating",
		Body: fmt.Sprintf("node %s's nightly update check is failing, so it has stopped receiving "+
			"fixes. It keeps serving in the meantime, and nothing you have is at risk right now. %s", a.node, why),
	}
}
