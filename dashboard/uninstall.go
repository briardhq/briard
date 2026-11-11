package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"time"

	"briard.io/agent/quadlet"
	"briard.io/shared/api"
	"briard.io/shared/dashboard"
)

// UNINSTALLING ONE APP, from its page, and deleting what an uninstall kept, from its Available card.
// The host does both (`app-uninstall`); this asks, with a page between the button and the act that
// says what is kept and what goes.

// keptView is what an uninstall kept of an app, on its Available card.
type keptView struct {
	Until string // when the copy goes by itself
}

// uninstallView is the page between the button and the act.
type uninstallView struct {
	App, Title string
	// Installed: the app's own Uninstall; otherwise the Available card's Delete now.
	Installed bool
	Until     string // the day the kept copy goes, uninstalled now
	Size      string // what the data holds now, at most what is kept; "" unmeasured
	Kept      *keptView
	Error     string
}

// keptUntil is the day a copy kept at `at` goes.
func keptUntil(at time.Time) string {
	return at.Add(quadlet.RetainUninstalled).In(zone()).Format("Mon 2 Jan")
}

// readKept fills each card's kept copy: the newest member an uninstall kept, read off the names in
// the volume's History directory, which say the app, the trigger and the moment (quadlet's naming).
// A directory that cannot be read says nothing of one; installing still finds it.
func (a *app) readKept(cards []*appView) {
	kept := a.keptCopies()
	for _, av := range cards {
		if at, ok := kept[av.Name]; ok {
			av.Kept = &keptView{Until: keptUntil(at)}
		}
	}
}

// keptCopies is the newest uninstall member of each app in the History directory.
func (a *app) keptCopies() map[string]time.Time {
	out := map[string]time.Time{}
	entries, err := os.ReadDir(a.snapshotsPath)
	if err != nil {
		return out
	}
	for _, e := range entries {
		app, tr, at, ok := quadlet.ParseSnapshotMember(e.Name())
		if ok && tr == quadlet.TriggerUninstall && at.After(out[app]) {
			out[app] = at
		}
	}
	return out
}

func (a *app) keptOf(name string) *keptView {
	if at, ok := a.keptCopies()[name]; ok {
		return &keptView{Until: keptUntil(at)}
	}
	return nil
}

// requestUninstall is the app page's Uninstall and the Available card's Delete now. TWO STEPS,
// like an undo: the first press shows what is kept and what goes. The act takes the app's stop
// and seconds more, so it is answered in the request.
func (a *app) requestUninstall(w http.ResponseWriter, r *http.Request) {
	name := r.FormValue("app")
	if !a.knows(name) {
		http.NotFound(w, r)
		return
	}
	v := uninstallView{App: name, Title: a.appTitle(name), Installed: a.installedApps()[name]}
	if r.FormValue("confirm") != "yes" {
		a.showUninstall(w, r, v)
		return
	}
	payload := name
	if r.FormValue("now") == "yes" {
		payload += " now"
	}
	ctx, cancel := context.WithTimeout(r.Context(), restoreBudget)
	defer cancel()
	o, err := a.port.Submit(ctx, api.Directive{Kind: api.DirectiveAppUninstall, Payload: payload})
	log.Printf("dashboard: uninstall %q: outcome=%+v err=%v", payload, o, err)
	switch {
	case err != nil:
		v.Error = err.Error()
	case o.State != api.OutcomeDone:
		v.Error = o.Detail
	default:
		http.Redirect(w, r, "/apps", http.StatusSeeOther)
		return
	}
	a.showUninstall(w, r, v)
}

// showUninstall renders the page between, with what the app holds now or what it kept.
func (a *app) showUninstall(w http.ResponseWriter, r *http.Request, v uninstallView) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	if v.Installed {
		v.Until = keptUntil(time.Now())
		if o, err := a.port.Submit(ctx, api.Directive{Kind: api.DirectiveAppUsage, Payload: v.App}); err == nil && o.State == api.OutcomeDone {
			var u dashboard.AppUsage
			if json.Unmarshal([]byte(o.Detail), &u) == nil {
				var n int64
				measured := true
				for _, t := range []*dashboard.TierUsage{u.Data, u.Files} {
					if t != nil {
						n, measured = n+t.Current, measured && t.Measured
					}
				}
				if measured && n > 0 {
					v.Size = gb(n)
				}
			}
		}
	} else {
		v.Kept = a.keptOf(v.App)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := page.ExecuteTemplate(w, "uninstall", v); err != nil {
		log.Printf("dashboard: uninstall page: %v", err)
	}
}
