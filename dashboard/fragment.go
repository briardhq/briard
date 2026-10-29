package main

// THE MOVING PARTS: a card that is on its way -- an install, a Home Assistant still starting, a
// name claim waiting on its link or its check, an undo in flight -- polls itself through htmx,
// the one script on the page, and nothing else on the page polls at all. The template puts the
// hx-get on the card only while its part moves, and the fragment answers with the same partial
// over the same view, so the page and its poll cannot disagree. When the part has stopped
// moving the page's shape may have changed around it -- an install became an app card and a
// glance, a claim became an address -- so the answer is then HX-Refresh: the page reloads once,
// and the polling ends with it. No JS still works: the page is complete on every load.

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"html/template"
	"log"
	"net/http"
	"time"
)

// themeScript sets Basecoat's dark class from the OS preference before the page paints. It is
// inline so there is no flash of the wrong palette, and the CSP below allows it by hash and
// nothing else inline.
const themeScript = "if (matchMedia('(prefers-color-scheme: dark)').matches) document.documentElement.classList.add('dark')"

// csp is every page's Content-Security-Policy: scripts from this origin and the one inline line.
// The dashboard is the whole security budget of the home's browser, so nothing else may run.
var csp = func() string {
	sum := sha256.Sum256([]byte(themeScript))
	return "script-src 'self' 'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"
}()

func theme() template.JS { return template.JS(themeScript) }

// fragment is /f/<part>?app=<name>: the partial for one moving part, or HX-Refresh once it has
// stopped moving. A part that is not one of the four, or an app the dashboard does not offer,
// is not found.
func (a *app) fragment(w http.ResponseWriter, r *http.Request, self device, part string) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	name := r.URL.Query().Get("app")
	var (
		tmpl   string
		data   any
		moving bool
	)
	switch part {
	case "install", "app":
		if offeredApp(name) == nil {
			http.NotFound(w, r)
			return
		}
		v := a.view(ctx, r, self)
		av := v.app(name)
		data = av
		if part == "install" {
			tmpl, moving = "installing", av.Install != nil && av.Install.Running
		} else {
			av.Here = r.URL.Query().Has("here")
			tmpl, moving = "app-card", av.HA != nil && !av.HA.Running
		}
	case "address-step", "address-card":
		// The step is over the name alone, and moves only while it is still owed: a claim that
		// became the name is the step's end, not another state of it. The card is over the
		// view, for the apps' names.
		v := a.view(ctx, r, self)
		tmpl, data, moving = part, v.Casa, v.Casa.Refresh() && v.Steps.Address
		if part == "address-card" {
			data, moving = v, v.Casa.Refresh()
		}
	case "history":
		if offeredApp(name) == nil {
			http.NotFound(w, r)
			return
		}
		hv := a.history(ctx, name)
		tmpl, data, moving = "history-card", hv, hv.Refresh
	default:
		http.NotFound(w, r)
		return
	}
	if !moving {
		w.Header().Set("HX-Refresh", "true")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := page.ExecuteTemplate(w, tmpl, data); err != nil {
		log.Printf("dashboard: fragment %s: %v", part, err)
	}
}

// app is the view's entry for an offered app, by slug.
func (v view) app(name string) *appView {
	for _, av := range v.Apps {
		if av.Name == name {
			return av
		}
	}
	return nil
}
