package host

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"briard.io/shared/api"
	"briard.io/shared/dashboard"
)

// handoffWriter is the slice of the guest client the dashboard directive drives.
// *guestagent.Client satisfies it; a fake records the handoff in tests.
type handoffWriter interface {
	DashboardHandoff(ctx context.Context, h dashboard.Handoff) error
}

// applyDashboard handles a DirectiveDashboard ([V3b.31b]): mint a one-time code, hand it to the
// guest with what the payload says about the OS account, and report the URL to open.
//
// THE CODE IS MINTED HERE, ON THE HOST, and nowhere else. Identity is the host's ([[logic-on-host]]),
// and what this code proves is that its holder could drive the CLI on this machine -- so the
// mint sits behind the local door, and the guest only ever receives one. A fresh directive is a
// fresh code: the previous one is overwritten, which is how `briard dashboard` doubles as the
// reset when a browser lost its session.
func (cfg Config) applyDashboard(ctx context.Context, g handoffWriter, d api.Directive, logf func(string, ...any)) api.DirectiveOutcome {
	var h dashboard.Handoff
	if d.Payload != "" {
		if err := json.Unmarshal([]byte(d.Payload), &h); err != nil {
			return api.DirectiveOutcome{ID: d.ID, State: api.OutcomeFailed, Detail: fmt.Sprintf("dashboard payload does not parse: %v", err)}
		}
	}
	if cfg.FlockName == "" {
		// Without a name there is no address to print: the dashboard answers at the node's own
		// mDNS name, and net.mdnsname publishes nothing for an unnamed flock.
		return api.DirectiveOutcome{ID: d.ID, State: api.OutcomeFailed, Detail: "this node has no flock name, so the dashboard has no address"}
	}
	code, err := dashboard.NewCode()
	if err != nil {
		return api.DirectiveOutcome{ID: d.ID, State: api.OutcomeFailed, Detail: err.Error()}
	}
	h.Code = code
	h.Issued = time.Now()
	if err := g.DashboardHandoff(ctx, h); err != nil {
		return api.DirectiveOutcome{ID: d.ID, State: api.OutcomeFailed, Detail: fmt.Sprintf("hand the code to the guest: %v", err)}
	}
	url := dashboard.URL(cfg.FlockName, code)
	logf("dashboard: code handed to the guest (valid %s)", dashboard.TTL)
	return api.DirectiveOutcome{ID: d.ID, State: api.OutcomeDone, Detail: url}
}
