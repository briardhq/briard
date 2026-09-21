package host

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"briard.io/agent/platform"
	"briard.io/agent/selfupdate"
	"briard.io/shared/notify"
)

// updateAlerter is the reader the timer path never had ([B.161](a)).
//
// EVERY OTHER TRIGGER OF THE FROZEN UPDATE UNIT HANDS ITS VERDICT TO SOMEBODY: `briard update`
// prints the line to whoever typed it, and the cloud's directive carries it back as an outcome.
// The TIMER does not -- and the timer is the one that runs on every node, nightly, and is the
// whole of updating on a standalone home. It starts the unit directly, so a run that fails ends
// in that unit's journal and stops there. Nothing on a node reports failed units anywhere. A
// household that has silently stopped updating therefore looks exactly like one that is current.
//
// That is the failure the upgrade floor exists to prevent, arriving through the other door. A
// node below the floor ([B.159](e)) is refused BY DESIGN, keeps serving, and is supposed to be
// reinstalled -- and until this existed, nobody was ever told to reinstall it.
//
// ⚠️ THE READER IS THE AGENT, NOT THE UNIT. The frozen layer carries no product knowledge
// ([B.86] rule 1): briard-update reports a line and an exit status, and what "this node has
// stopped updating" MEANS to a household is the product's to say, not a shell script's. So this
// watches the unit from above rather than teaching the script to alert -- and the frozen side
// needs no release to make it work.
type updateAlerter struct {
	n       notify.Notifier
	node    string
	unit    string            // the frozen update unit whose verdict this reads
	lay     selfupdate.Layout // for the run's own last line (read, never consumed)
	logf    func(string, ...any)
	state   func(unit string) (string, error) // overridable in tests
	failing bool                              // the edge: is this node currently not updating
}

// updateCheckEvery is how often the unit's verdict is read. The unit runs once a night, so this
// is not a race to catch a transition -- it is how long a node that has stopped updating stays
// quiet about it, and an hour is comfortably inside the next run.
const updateCheckEvery = time.Hour

func newUpdateAlerter(n notify.Notifier, node string, lay selfupdate.Layout, logf func(string, ...any)) *updateAlerter {
	return &updateAlerter{
		n:     n,
		node:  node,
		unit:  selfupdate.DefaultUpdateUnit,
		lay:   lay,
		logf:  logf,
		state: platform.UnitState,
	}
}

// watchUpdates reads the frozen update unit's verdict on a slow cadence for the life of the
// agent. Started once, beside the guest update timer, and on EVERY node -- a witness included.
// A node that can no longer update is outside its support window whatever else it is, and that
// is not a statement about replicas, services or role.
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

// observe reads the unit's ActiveState and fires on a change.
//
// ⚠️ IT DOES NOT PRIME, and that is the opposite of the redundancy alerter's rule on purpose.
// Priming exists there to stop a startup reading being reported as a transition. Here the
// startup reading is the case this whole thing was built for: a floored node's agent is the one
// agent that never gets replaced, so "already failing when I came up" is the steady state, not
// a false positive, and a primed alerter would be silent about it forever. Starting from
// not-failing means the first at-rest failing reading fires, which is the intent.
//
// The cost is one alert per agent restart while the state holds, which is why the alerter is
// built once for the life of the process rather than per observe loop: a channel re-dial must
// not re-announce it.
func (a *updateAlerter) observe(ctx context.Context) {
	state, err := a.state(a.unit)
	if err != nil {
		// The query failed. That is not an answer about the unit and must not be read as one,
		// in either direction -- say nothing and look again next tick.
		a.logf("update watch: reading %s failed: %v", a.unit, err)
		return
	}
	var failing bool
	switch state {
	case "failed":
		failing = true
	case "inactive":
		failing = false // a oneshot at rest after a run that did not fail, or one yet to run
	default:
		// activating / deactivating / active: a run is in flight and has no verdict yet.
		// An unfamiliar reading lands here too, and holding is the safe way to be wrong.
		return
	}
	if failing == a.failing {
		return
	}
	a.failing = failing
	fireAlert(ctx, a.n, a.logf, a.alertFor(failing))
}

// alertFor is what the household is actually told. The warning names the CONSEQUENCE rather
// than the unit -- "this node has stopped updating" is the fact an owner can act on, where
// "briard-update.service failed" is a fact about systemd -- and it carries the run's own last
// line, which is where the remedy lives: a floored node's refusal says in so many words that it
// must be reinstalled ([install.ErrTooOldToUpgrade]).
func (a *updateAlerter) alertFor(failing bool) notify.Alert {
	if !failing {
		return notify.Alert{
			Level: notify.Recovered,
			Title: "Briard: updates are working again",
			Body: fmt.Sprintf("node %s completed an update check; it is following its release channel again.",
				a.node),
		}
	}
	body := fmt.Sprintf("node %s's nightly update check is failing, so it has stopped receiving "+
		"fixes. It keeps serving in the meantime, and nothing you have is at risk right now.", a.node)
	if last := a.lastResult(); last != "" {
		body += " The update run said: " + last
	}
	return notify.Alert{
		Level: notify.Warning,
		Title: "Briard: this node has stopped updating",
		Body:  body,
	}
}

// lastResult is the update run's own last line, READ WITHOUT CONSUMING IT.
//
// Every other reader of this message takes it -- the CLI and the cloud each need the verdict
// once, and a message that has been read is gone, which is what keeps the run dir free of
// lifecycle. This one must not: the timer leaves its line behind with no consumer, and an
// alerter that unlinked it would steal the verdict from a `briard update` running at the same
// moment. Best-effort in both directions -- the alert is truthful without it, and a line left
// by an earlier run is still the last thing this node's updater had to say.
func (a *updateAlerter) lastResult() string {
	b, err := os.ReadFile(a.lay.ResultPath())
	if err != nil {
		return ""
	}
	return strings.Join(strings.Fields(string(b)), " ")
}
