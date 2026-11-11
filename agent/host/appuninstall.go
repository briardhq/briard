package host

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"time"

	"briard.io/agent/quadlet"
	"briard.io/shared/api"
	"briard.io/shared/manifest"
	"briard.io/shared/notify"
)

// appUninstaller is the slice of the guest an uninstall, and installing an uninstalled app again,
// need beyond appInstaller. *guestagent.Client satisfies it. Narrow interface for DI, not a seam.
type appUninstaller interface {
	AppUninstall(ctx context.Context, app string, keep bool, at time.Time) error
	AppReinstate(ctx context.Context, app, member string) error
	SupportsAppUninstall() bool
}

// applyAppUninstall removes one app: the household's Uninstall, and its Delete now
// (DirectiveAppUninstall). The guest keeps the app's data and files areas as members named for the
// uninstall and deletes the rest (agent/guestagent/uninstall.go); installing the app again within
// quadlet.RetainUninstalled restores them (keptMember). This side stops the app first, so what is
// kept is consistent, and afterwards forgets it node-locally: its cached manifest, the images no
// other app pins, its open alert -- and tells a Home Assistant that is running, as an install does.
func (cfg Config) applyAppUninstall(ctx context.Context, g appInstaller, d api.Directive, n notify.Notifier, logf func(string, ...any)) api.DirectiveOutcome {
	failed := func(detail string) api.DirectiveOutcome {
		return api.DirectiveOutcome{ID: d.ID, State: api.OutcomeFailed, Detail: detail}
	}
	f := strings.Fields(d.Payload)
	if len(f) == 0 || len(f) > 2 || !manifest.ValidName(f[0]) || (len(f) == 2 && f[1] != "now") {
		return failed(fmt.Sprintf("%q is not an app's name, optionally followed by \"now\"", d.Payload))
	}
	name, now := f[0], len(f) == 2
	u, ok := g.(appUninstaller)
	if !ok || !u.SupportsAppUninstall() {
		return failed("this VM cannot uninstall one app; update the VM first")
	}
	ctx, cancel := cfg.beat.budget(ctx, installBudget)
	defer cancel()

	// The volume is the Primary's: a node that does not hold it forgets the app here, and the
	// Primary removes it.
	qs, err := g.Status(ctx, cfg.Resource.Name)
	if err != nil {
		return failed(fmt.Sprintf("read machine status: %v", err))
	}
	if !qs.Serving() {
		cfg.forgetApp(name, logf)
		return api.DirectiveOutcome{ID: d.ID, State: api.OutcomeDone, Detail: name + " forgotten on this machine (not serving)"}
	}
	raw, err := g.AppInstalled(ctx, name)
	if err != nil {
		return failed(fmt.Sprintf("read %s's installed manifest: %v", name, err))
	}
	if raw == "" && !now {
		return failed(name + " is not installed here")
	}
	keeps := false
	if raw != "" {
		m, _, err := manifest.Parse([]byte(raw))
		if err != nil {
			return failed(fmt.Sprintf("%s's installed manifest does not parse: %v", name, err))
		}
		r, err := quadlet.Render(m, "")
		if err != nil {
			return failed(fmt.Sprintf("%s's installed manifest does not render: %v", name, err))
		}
		keeps = !now && (m.UsesTier(manifest.TierData) || m.UsesTier(manifest.TierFiles))
		// Stopped first: what is kept is then what a clean stop flushed.
		cfg.quiesce(ctx, g, r.ContainerUnits, logf)
	}
	at := cfg.takenAt()
	if err := u.AppUninstall(ctx, name, !now, at); err != nil {
		// A converge starts what the volume names -- the app too, if its manifest still stands.
		if _, cerr := g.AppConverge(context.WithoutCancel(ctx), ""); cerr != nil {
			return failed(fmt.Sprintf("uninstall %s: %v; AND the node could not converge afterwards: %v", name, err, cerr))
		}
		return failed(fmt.Sprintf("uninstall %s: %v", name, err))
	}
	if raw == "" {
		logf("app uninstall %s: its kept copy deleted", name)
		return api.DirectiveOutcome{ID: d.ID, State: api.OutcomeDone, Detail: "nothing of " + name + " is kept any more"}
	}
	cfg.forgetApp(name, logf)
	cfg.dropSuperseded(ctx, g, raw, logf)
	if told, err := g.HassNudge(ctx); err != nil {
		logf("app uninstall %s: could not tell Home Assistant to reconsider (%v); it will pick this up at its next start", name, err)
	} else if told {
		logf("app uninstall %s: Home Assistant was told to reconsider what the node offers", name)
	}
	// The alert loop reads only the installed apps, so nothing else would ever close this one.
	fireAlert(context.WithoutCancel(ctx), n, logf, notify.Alert{
		Key: "app:" + name, Kind: notify.Resolved,
		Title: "Briard: " + name + " was uninstalled",
		Body:  fmt.Sprintf("%s was uninstalled from machine %s.", name, cfg.Node),
	})
	detail := name + " is uninstalled"
	switch {
	case keeps:
		detail += fmt.Sprintf("; its data is kept until %s, and installing it again before then brings it back as you left it",
			at.Add(quadlet.RetainUninstalled).Local().Format("Mon 2 Jan"))
	case now:
		detail += ", and its data deleted"
	}
	logf("app uninstall %s: %s", name, detail)
	return api.DirectiveOutcome{ID: d.ID, State: api.OutcomeDone, Detail: detail}
}

// forgetApp drops an uninstalled app's manifest from the node-local cache, so this node's next
// bring-up does not render it.
func (cfg Config) forgetApp(name string, logf func(string, ...any)) {
	if cfg.AppCache == "" {
		return
	}
	if err := os.Remove(cfg.manifestPath(name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		logf("app uninstall %s: WARNING: could not forget the cached manifest (%v); this node's next start renders it again", name, err)
	}
}

// keptCopyOf is the newest copy an uninstall kept of an app that is not installed: what installing
// it again restores (applyOneAppInstall). none (zero) when there is nothing to restore -- the app is installed
// (that copy is then its History), this node does not hold the volume, or the guest predates
// uninstalling one app.
func (cfg Config) keptCopyOf(ctx context.Context, g appInstaller, name string) (quadlet.SnapshotEntry, error) {
	if u, ok := g.(appUninstaller); !ok || !u.SupportsAppUninstall() || !g.SupportsMembers() {
		return quadlet.SnapshotEntry{}, nil
	}
	qs, err := g.Status(ctx, cfg.Resource.Name)
	if err != nil {
		return quadlet.SnapshotEntry{}, fmt.Errorf("read machine status: %w", err)
	}
	if !qs.Serving() {
		return quadlet.SnapshotEntry{}, nil
	}
	raw, err := g.AppInstalled(ctx, name)
	if err != nil {
		return quadlet.SnapshotEntry{}, fmt.Errorf("read %s's installed manifest: %w", name, err)
	}
	if raw != "" {
		return quadlet.SnapshotEntry{}, nil
	}
	members, err := g.Members(ctx, name)
	if err != nil {
		return quadlet.SnapshotEntry{}, fmt.Errorf("read whether %s's data was kept when it was uninstalled: %w", name, err)
	}
	var kept quadlet.SnapshotEntry
	for _, m := range members { // oldest first
		if m.Meta.Trigger == quadlet.TriggerUninstall {
			kept = m
		}
	}
	return kept, nil
}
