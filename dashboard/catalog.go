package main

// THE CATALOG ON THE PAGE. Every app card -- installed, available, coming soon -- takes its
// title, its function, its description and its icon from the catalog's index, as the host read
// and verified it and pushed it here (shared/dashboard.CatalogPath). The page never fetches the
// index and no copy of it is compiled in: before the host has pushed one, the page shows what is
// installed and nothing it could add, because a list from any other clock would be the wrong list.
//
// "I WANT THIS" is a wish for an app the index lists but no manifest exists for yet. It is a
// directive to the host like an install is: relayed, decided there, recorded on the node, and
// read back from the view the host pushes. Pressing it again takes the wish back. The card is
// re-rendered in place when the page's script is running, and the page reloads when it is not.

import (
	"context"
	"encoding/json"
	"html/template"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"briard.io/shared/api"
	"briard.io/shared/dashboard"
	"briard.io/shared/routes"
)

// catalogState is the host's view of the index; nil before the host has pushed one.
func (a *app) catalogState() *dashboard.Catalog {
	raw, err := os.ReadFile(a.catalogPath)
	if err != nil {
		return nil
	}
	var c dashboard.Catalog
	if err := json.Unmarshal(raw, &c); err != nil {
		log.Printf("dashboard: catalog view: %v", err)
		return nil
	}
	return &c
}

// listed is the index's entry for a slug, nil for one it does not list.
func (a *app) listed(name string) *dashboard.CatalogApp {
	c := a.catalogState()
	if c == nil {
		return nil
	}
	for i := range c.Apps {
		if c.Apps[i].Name == name {
			return &c.Apps[i]
		}
	}
	return nil
}

// knows says whether the page has a card for a slug: the index lists it, or this machine runs
// it. Anything else is not found.
func (a *app) knows(name string) bool {
	return a.listed(name) != nil || a.installedApps()[name]
}

// installable says whether the page may ask the host to install a slug: listed, with a
// manifest. The host decides again; this only keeps a button off a card that has none.
func (a *app) installable(name string) bool {
	l := a.listed(name)
	return l != nil && l.Available
}

// offered says whether the host's view offers an update for a slug: the page may then ask the
// host to install it over what runs. The host decides again.
func (a *app) offered(name string) bool {
	l := a.listed(name)
	return l != nil && l.Update
}

// appTitle is an app's name for a person: the index's, or the slug made readable for an app the
// index does not list (one installed before the index was read, or by hand).
func (a *app) appTitle(name string) string {
	if l := a.listed(name); l != nil {
		return l.Title
	}
	return titleOf(name)
}

// titleOf is a slug as a title: "home-assistant" reads "Home Assistant". A fallback, never the
// first choice; the index carries the name the project spells.
func titleOf(slug string) string {
	words := strings.Split(slug, "-")
	for i, w := range words {
		if w != "" {
			words[i] = strings.ToUpper(w[:1]) + w[1:]
		}
	}
	return strings.Join(words, " ")
}

// requestWish is the button on a coming-soon card, and its second press: one wish directive to
// the host, then the view it pushed back, then the card as it stands now. With the page's
// script running (an htmx request) the answer is the one card; without it the page reloads.
func (a *app) requestWish(w http.ResponseWriter, r *http.Request, name string) {
	before := a.listed(name)
	if before == nil {
		http.NotFound(w, r)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	o, err := a.port.Submit(ctx, api.Directive{Kind: api.DirectiveWish, Payload: name})
	switch {
	case err != nil:
		log.Printf("dashboard: wish %s: %v", name, err)
	case o.State != api.OutcomeDone:
		log.Printf("dashboard: wish %s: %s", name, o.Detail)
	default:
		// The host records the wish and pushes the view on its next cycle; the card is rendered
		// from that view, so wait for it the way a backup setting's answer does.
		a.awaitCatalog(ctx, name, !before.Wished)
	}
	if r.Header.Get("HX-Request") == "" {
		http.Redirect(w, r, back(r), http.StatusSeeOther)
		return
	}
	v := a.view(ctx, r, device{})
	av := v.app(name)
	if av == nil {
		http.NotFound(w, r)
		return
	}
	tmpl := "soon-card"
	if av.Available {
		tmpl = "available-card"
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := page.ExecuteTemplate(w, tmpl, av); err != nil {
		log.Printf("dashboard: wish card: %v", err)
	}
}

// awaitCatalog waits, up to viewWait, for the host's pushed view to show the named app wished
// or not: a few of the host's ticks, wall time, since the page's clock may be a fixture.
func (a *app) awaitCatalog(ctx context.Context, name string, wished bool) {
	deadline := time.Now().Add(a.viewWait)
	for {
		if l := a.listed(name); l != nil && l.Wished == wished {
			return
		}
		if !time.Now().Before(deadline) {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// icon is an index icon as a template may place it in src: a data URI the host verified
// (shared/manifest validates it), so it is trusted as a URL; "" renders no image.
func icon(s string) template.URL { return template.URL(s) }

// routedNames is what this machine serves, in the routes table's order: the names the index
// may not list, for cards of their own.
func routedNames(routesPath string) []string {
	raw, err := os.ReadFile(routesPath)
	if err != nil {
		return nil
	}
	t, err := routes.Parse(raw)
	if err != nil {
		return nil
	}
	var names []string
	for _, s := range t.Services {
		names = append(names, s.Name)
	}
	return names
}
