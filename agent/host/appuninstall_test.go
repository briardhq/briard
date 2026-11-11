package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"briard.io/agent/quadlet"
	"briard.io/shared/api"
	"briard.io/shared/notify"
)

// uninstallGuest is the install fake taught the uninstall's two verbs.
type uninstallGuest struct {
	*fakeInstaller
	uninstallEr error
}

func (u *uninstallGuest) AppUninstall(_ context.Context, app string, keep bool, _ time.Time) error {
	u.steps = append(u.steps, fmt.Sprintf("uninstall:%s:keep=%t", app, keep))
	if u.uninstallEr != nil {
		return u.uninstallEr
	}
	delete(u.prior, app)
	return nil
}

func (u *uninstallGuest) AppReinstate(_ context.Context, app, member string) error {
	u.steps = append(u.steps, "reinstate:"+member)
	return nil
}

func (u *uninstallGuest) SupportsAppUninstall() bool { return true }

func uninstall(cfg Config, g *uninstallGuest, n notify.Notifier, payload string) api.DirectiveOutcome {
	d := api.Directive{ID: "d1", Kind: api.DirectiveAppUninstall, Payload: payload}
	return cfg.applyAppUninstall(context.Background(), g, d, n, func(string, ...any) {})
}

func stepAt(steps []string, prefix string) int {
	return slices.IndexFunc(steps, func(s string) bool { return strings.HasPrefix(s, prefix) })
}

// AN UNINSTALL STOPS THE APP, THEN REMOVES IT KEEPING ITS DATA, and forgets it on this node: its
// cached manifest, its images, its open alert; and Home Assistant is told.
func TestUninstallStopsTheAppThenRemovesItAndForgetsIt(t *testing.T) {
	cfg := catalogFor(t, testManifest())
	cfg.AppCache = t.TempDir()
	if err := cfg.cacheApp("home-assistant", []byte("{}")); err != nil {
		t.Fatal(err)
	}
	g := &uninstallGuest{fakeInstaller: &fakeInstaller{primary: true, active: true, prior: mustPrior(t)}}
	n := &fakeNotifier{}
	o := uninstall(cfg, g, n, "home-assistant")
	if o.State != api.OutcomeDone || !strings.Contains(o.Detail, "kept until") {
		t.Fatalf("outcome %+v", o)
	}
	stop, rm := stepAt(g.steps, "stop:"), stepAt(g.steps, "uninstall:home-assistant:keep=true")
	if stop < 0 || rm < stop {
		t.Errorf("steps %v: want the app stopped, then uninstalled keeping its data", g.steps)
	}
	if stepAt(g.steps, "rmi:") < rm || stepAt(g.steps, "nudge") < rm {
		t.Errorf("steps %v: want its images dropped and Home Assistant told after", g.steps)
	}
	if _, err := os.Stat(filepath.Join(cfg.AppCache, "home-assistant.json")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the cached manifest is still there: %v", err)
	}
	if len(n.alerts) != 1 || n.alerts[0].Key != "app:home-assistant" || n.alerts[0].Kind != notify.Resolved {
		t.Errorf("alerts %+v, want the app's resolved", n.alerts)
	}
}

// NOW KEEPS NOTHING, and on an app already uninstalled it deletes what was kept; without it an app
// that is not installed is refused, untouched.
func TestUninstallNowAndNotInstalled(t *testing.T) {
	cfg := catalogFor(t, testManifest())
	g := &uninstallGuest{fakeInstaller: &fakeInstaller{primary: true, active: true, prior: mustPrior(t)}}
	if o := uninstall(cfg, g, nil, "home-assistant now"); o.State != api.OutcomeDone || !strings.Contains(o.Detail, "deleted") ||
		stepAt(g.steps, "uninstall:home-assistant:keep=false") < 0 {
		t.Fatalf("outcome %+v after %v", o, g.steps)
	}

	g.steps = nil
	if o := uninstall(cfg, g, nil, "home-assistant"); o.State != api.OutcomeFailed || stepAt(g.steps, "uninstall:") >= 0 || stepAt(g.steps, "stop:") >= 0 {
		t.Errorf("an app not installed: outcome %+v after %v, want a refusal that touched nothing", o, g.steps)
	}
	if o := uninstall(cfg, g, nil, "home-assistant now"); o.State != api.OutcomeDone || !strings.Contains(o.Detail, "nothing of home-assistant is kept") {
		t.Errorf("Delete now: outcome %+v", o)
	}
	for _, bad := range []string{"", "home-assistant later", "../x now", "a b c"} {
		if o := uninstall(cfg, g, nil, bad); o.State != api.OutcomeFailed {
			t.Errorf("payload %q: outcome %+v, want refused", bad, o)
		}
	}
}

// A FAILED UNINSTALL CONVERGES, so an app whose manifest still stands is started again.
func TestAFailedUninstallStartsTheAppAgain(t *testing.T) {
	cfg := catalogFor(t, testManifest())
	g := &uninstallGuest{fakeInstaller: &fakeInstaller{primary: true, active: true, prior: mustPrior(t)}, uninstallEr: errors.New("boom")}
	o := uninstall(cfg, g, nil, "home-assistant")
	if o.State != api.OutcomeFailed || stepAt(g.steps, "converge") < stepAt(g.steps, "uninstall:") {
		t.Errorf("outcome %+v after %v, want failed and converged", o, g.steps)
	}
}

// INSTALLING AN UNINSTALLED APP AGAIN RESTORES WHAT IT KEPT, files too, at the version it was kept
// under -- never the catalog's over it -- and takes no undo point of the empty app.
func TestInstallingAgainRestoresTheKeptCopy(t *testing.T) {
	cfg := catalogFor(t, testManifest())
	cfg.readinessSettle = time.Millisecond
	raw, _ := json.Marshal(testManifest())
	kept := quadlet.SnapshotMember("home-assistant", quadlet.TriggerUninstall, fixedNow.Add(-48*time.Hour))
	g := &uninstallGuest{fakeInstaller: &fakeInstaller{primary: true, active: true, healthy: true,
		members: []quadlet.SnapshotEntry{{Member: kept, Meta: quadlet.SnapshotMeta{
			App: "home-assistant", Trigger: quadlet.TriggerUninstall, Manifest: string(raw),
		}}}}}
	d := api.Directive{ID: "d1", Kind: api.DirectiveAppInstall, Payload: "home-assistant"}
	o := cfg.applyAppInstall(context.Background(), g, d, nil, func(string, ...any) {})
	if o.State != api.OutcomeDone || !strings.Contains(o.Detail, "installed again") {
		t.Fatalf("outcome %+v after %v", o, g.steps)
	}
	restore, reinstate := stepAt(g.steps, "restore:"+kept), stepAt(g.steps, "reinstate:"+kept)
	if restore < 0 || reinstate < restore || stepAt(g.steps, "converge") < reinstate {
		t.Errorf("steps %v: want the data restored, the files put back, then a converge", g.steps)
	}
	if stepAt(g.steps, "snapshot:") >= 0 || stepAt(g.steps, "stage:") >= 0 {
		t.Errorf("steps %v: an undo point of the empty app, or a fresh install's stage", g.steps)
	}

	// Installed now, the copy is its History: an install is the ordinary one.
	g.steps, g.prior = nil, mustPrior(t)
	cfg.applyAppInstall(context.Background(), g, d, nil, func(string, ...any) {})
	if stepAt(g.steps, "reinstate:") >= 0 || stepAt(g.steps, "stage:") < 0 {
		t.Errorf("an installed app's install restored its kept copy: %v", g.steps)
	}
}
