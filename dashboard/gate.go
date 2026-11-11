package main

// THE GATE'S DASHBOARD HALF (reverse-proxy/gate.go is the door's). A gated app -- Files -- has no
// login of its own; the household reaches it with this dashboard's session, carried to the
// app's own name the way the hop carries it between the dashboard's names:
//
//   - GET /open/<app>, on a browser this dashboard trusts, mints a one-time code for the device's
//     OWN token and sends the browser to the app's name with it. A browser it does not trust gets
//     the ordinary refusal (and on the household's name, the bounce), with the address kept.
//   - The door redeems the code at /gate/redeem and sets the token as the app name's cookie,
//     then asks /gate/check whether a token still stands for the app before it signs anyone in.
//     That question comes before every way in -- an open, an address typed, a page left open --
//     so it is where an app is made ready for the user (Mealie's first account, mealie.go).
//
// Both answers are the door's alone, asked over loopback: the door never forwards /gate/ from a
// browser, and refusing a request that came through it (it always says the scheme) is the
// second guard.

import (
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"

	"briard.io/agent/files"
	"briard.io/agent/mealie"
	"briard.io/shared/casa"
	"briard.io/shared/routes"
)

// gateHopParam carries the code to the app's name; the door's name for it.
const gateHopParam = "briard_hop"

// gatedApps is every app the routing table gates, by name.
func gatedApps(routesPath string) map[string]routes.App {
	raw, err := os.ReadFile(routesPath)
	if err != nil {
		return nil
	}
	t, err := routes.Parse(raw)
	if err != nil {
		log.Printf("dashboard: routes: %v", err)
		return nil
	}
	out := map[string]routes.App{}
	for _, s := range t.Apps {
		if _, ok := s.Route(routes.ListenName); ok && s.Gate != "" && len(s.Hosts) > 0 {
			out[s.Name] = s
		}
	}
	return out
}

// openGated sends a trusted browser to a gated app, signed in.
func (a *app) openGated(w http.ResponseWriter, r *http.Request, name string) {
	s, ok := gatedApps(a.routesPath)[name]
	if !ok {
		http.NotFound(w, r)
		return
	}
	// Back to the name the browser was on when it is one of the app's own; else the one that
	// matches how this page was reached -- the household's name on its name, the alias otherwise.
	host := routes.Normalise(r.URL.Query().Get("host"))
	if !slices.ContainsFunc(s.Hosts, func(h string) bool { return routes.Normalise(h) == host }) {
		host = routes.BareHostName(name)
		if onName(r, a.casaState()) {
			for _, h := range s.Hosts {
				if strings.HasSuffix(h, "."+casa.Zone) {
					host = h
				}
			}
		}
	}
	to := r.URL.Query().Get("to")
	if !strings.HasPrefix(to, "/") || strings.HasPrefix(to, "//") {
		to = "/"
	}
	scheme := "http"
	if strings.HasSuffix(host, "."+casa.Zone) {
		scheme = "https"
	}
	u, err := url.Parse(scheme + "://" + host + to)
	if err != nil {
		http.Error(w, "that address is not one this app answers\n", http.StatusBadRequest)
		return
	}
	code, err := newSecret()
	if err != nil {
		http.Error(w, "could not open the app\n", http.StatusInternalServerError)
		return
	}
	a.mu.Lock()
	a.sweepHops()
	a.hops[code] = hop{token: sessionToken(r), at: a.now()}
	a.mu.Unlock()
	q := u.Query()
	q.Set(gateHopParam, code)
	u.RawQuery = q.Encode()
	http.Redirect(w, r, u.String(), http.StatusSeeOther)
}

// viaDoor says a request came through the front door, which names the scheme on everything it
// forwards here; the door's own loopback questions name none.
func viaDoor(r *http.Request) bool { return r.Header.Get("X-Forwarded-Proto") != "" }

// gateRedeem trades a code openGated minted for the token it was minted for, once.
func (a *app) gateRedeem(w http.ResponseWriter, r *http.Request) {
	if viaDoor(r) {
		http.NotFound(w, r)
		return
	}
	code := r.FormValue("code")
	a.mu.Lock()
	a.sweepHops()
	h, ok := a.hops[code]
	delete(a.hops, code)
	a.mu.Unlock()
	if code == "" || !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprint(w, h.token)
}

// gateCheck says whether a session token is still a trusted device's, and whose, for the app the
// door names: 200 with the username the door signs into the app -- Files' one fixed name
// (agent/files), or the one Mealie's accounts say (mealie.go); 401 not a trusted device; 409 a
// trusted device with no record, which only an open writes and Mealie's first account takes its
// name from; 503 an app with no account of the user's to open.
func (a *app) gateCheck(w http.ResponseWriter, r *http.Request) {
	if viaDoor(r) {
		http.NotFound(w, r)
		return
	}
	if _, ok := a.deviceOf(r.Header.Get("X-Briard-Session")); !ok {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if _, err := a.account(); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	// Mealie's user is the one Mealie's own accounts say, read at every check (mealie.go).
	user := files.User
	if app := r.URL.Query().Get("app"); app == mealie.Name {
		if s, ok := gatedApps(a.routesPath)[app]; ok {
			var err error
			if user, err = a.mealieUser(r.Context(), s); err != nil {
				log.Printf("dashboard: find the user's account in Mealie: %v", err)
				http.Error(w, err.Error(), http.StatusServiceUnavailable)
				return
			}
		}
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprint(w, user)
}
