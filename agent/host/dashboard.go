package host

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"briard.io/shared/api"
	"briard.io/shared/casa"
	"briard.io/shared/dashboard"
)

// handoffWriter is the slice of the guest client the dashboard directive drives.
// *guestagent.Client satisfies it; a fake records the handoff in tests.
type handoffWriter interface {
	DashboardHandoff(ctx context.Context, h dashboard.Handoff) error
}

// applyDashboard handles a DirectiveDashboard: mint a one-time code, hand it to the
// guest with what the payload says about the OS account, and report the URL to open.
//
// THE CODE IS MINTED HERE, ON THE HOST, and nowhere else. Identity is the host's,
// and what this code proves is that its holder could drive the CLI on this machine -- so the
// mint sits behind the local door, and the guest only ever receives one. A fresh directive is a
// fresh code: the previous one is overwritten, which is how `briard open` doubles as the
// reset when a browser lost its session.
func (cfg Config) applyDashboard(ctx context.Context, g handoffWriter, d api.Directive, cs *casaRunner, logf func(string, ...any)) api.DirectiveOutcome {
	var h dashboard.Handoff
	if d.Payload != "" {
		if err := json.Unmarshal([]byte(d.Payload), &h); err != nil {
			return api.DirectiveOutcome{ID: d.ID, State: api.OutcomeFailed, Detail: fmt.Sprintf("dashboard payload does not parse: %v", err)}
		}
	}
	if cfg.FlockName == "" {
		// Without a name there is no address to print: the door answers the bare `briard.local`
		// only beside the flock's own name, and net.mdnsname publishes nothing for an unnamed flock.
		return api.DirectiveOutcome{ID: d.ID, State: api.OutcomeFailed, Detail: "this machine has no name yet, so the Briard page has no address"}
	}
	code, err := dashboard.NewCode()
	if err != nil {
		return api.DirectiveOutcome{ID: d.ID, State: api.OutcomeFailed, Detail: err.Error()}
	}
	h.Code = code
	h.Issued = time.Now()
	hctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := g.DashboardHandoff(hctx, h); err != nil {
		return api.DirectiveOutcome{ID: d.ID, State: api.OutcomeFailed, Detail: fmt.Sprintf("hand the code to the guest: %v", err)}
	}
	// The address to open, one per line: the household's own name first when it is usable --
	// registered, found to resolve on this network, with its certificate on the volume -- and
	// the `.local` alias under it, the same code on both (it works once, on whichever is
	// opened; the dashboard carries the trust across its names on its own). The tray opens the
	// first line; the CLI prints them all.
	url := dashboard.URL(code)
	if name, ok := cs.usableName(); ok {
		url = "https://" + name + "/?code=" + code + "\n" + url
	}
	logf("dashboard: code handed to the guest (valid %s)", dashboard.TTL)
	return api.DirectiveOutcome{ID: d.ID, State: api.OutcomeDone, Detail: url}
}

// usableName is the household's name when a browser can open it now: registered, found to
// resolve on this network, with a certificate. Nil-safe, for a loop built without a runner.
func (c *casaRunner) usableName() (string, bool) {
	if c == nil || c.cfg.FlockName == "" {
		return "", false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.st.Registered || c.resolves != dashboard.CasaResolves || c.certUntil.IsZero() {
		return "", false
	}
	return casa.Names(c.cfg.FlockName)[0], true
}
