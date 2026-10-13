package host

import (
	"context"
	"fmt"
	"time"

	"briard.io/shared/notify"
)

// lidAlerter tells the owner when closing this computer's lid puts it to sleep, and again when it
// no longer does. The agent holds idle sleep off, but no power request overrides the lid: that is
// the power plan's, and the plan is the user's to change, so the alert hands them the setting
// rather than the agent rewriting it. Plugged in only -- a laptop serving the house runs on its
// charger, and on battery it stops when the battery does whatever the lid says.
//
// AN UNKNOWN IS NOT AN ALARM: a machine with no lid, a plan that did not answer, and Linux (which
// holds nothing, so promises nothing about a lid) read "" and neither open nor clear. Read every
// lidReadEvery rather than once, because the fix is a setting the person changes while the agent
// runs, and the alert should close when they do.
type lidAlerter struct {
	read func() string // lidAction in production: "sleeps", "stays", or "" for unknown
	next time.Time     // when to read again
}

const lidReadEvery = 5 * time.Minute

func (l *lidAlerter) observe(ctx context.Context, n notify.Notifier, node string, now time.Time, logf func(string, ...any)) {
	if now.Before(l.next) {
		return
	}
	l.next = now.Add(lidReadEvery)
	switch l.read() {
	case "sleeps":
		fireAlert(ctx, n, logf, notify.Alert{
			Key:      "lid",
			Kind:     notify.Open,
			Severity: notify.Warning,
			Title:    "Briard: closing this computer's lid will stop it serving",
			Body: fmt.Sprintf("node %s sleeps, hibernates or shuts down when its lid is closed while plugged in, "+
				"and Briard -- Home Assistant included -- stops while it does. If you want to keep the lid closed, "+
				"open Control Panel > Power Options > Choose what closing the lid does, and set "+
				"\"When I close the lid\" (Plugged in) to Do nothing. Or, in PowerShell as administrator: "+
				"powercfg /setacvalueindex SCHEME_CURRENT SUB_BUTTONS LIDACTION 0; powercfg /setactive SCHEME_CURRENT", node),
		})
	case "stays":
		fireAlert(ctx, n, logf, notify.Alert{
			Key:   "lid",
			Kind:  notify.Resolved,
			Title: "Briard: closing the lid no longer stops this computer",
			Body:  fmt.Sprintf("node %s keeps running with its lid closed while plugged in.", node),
		})
	}
}
