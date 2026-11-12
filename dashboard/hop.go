package main

// THE HOP: trust earned on one of the dashboard's names reaches the others.
//
// A session is a cookie, and a cookie is per host: a browser trusted on `briard.local` (the
// address `briard open` prints second) arrives at `<flock>.briard.casa` -- the one address with a
// real certificate -- as a stranger. Nothing a page runs can cross that line: a script on the
// https name may not fetch the http alias (mixed content), and a cross-site fetch carries no
// cookie anyway. A top-level navigation does both, and the household makes one whenever it
// follows the dashboard's own link to the name, so that is the whole mechanism:
//
//   - Every link the dashboard draws to the name goes through its own /hop, on the host the page
//     is already on -- a link the browser can always follow. /hop mints a one-time code bound to
//     that device's own session token and sends the browser on to the name with it; in a browser
//     that holds nothing, with an empty one. The code is minted at the click, not when the page
//     is drawn, so a page left open longer than a code lives still carries its trust.
//   - The name redeems the code as its own cookie -- the SAME token, so Trusted devices keeps
//     one row and revoking it revokes every name -- and shows the page that was asked for. An
//     empty or spent code is the ordinary refusal, and the name asks a trusted device.
//
// NEVER A BOUNCE. The name does not send a browser it does not trust to the alias to find out
// whether the alias trusts it: a browser that cannot resolve `.local` -- every one reaching home
// over a private network, and some at home -- would be left on its own "server not found" page.
// A browser that opens the name by itself is asked the ordinary way.
//
// WHOSE NAME. /hop answers only for this home's own name and refuses any other, so there is no
// open redirect through this host.

import (
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const hopTTL = time.Minute // a code's life

// hop is one minted code, from /hop's mint until the name redeems it.
type hop struct {
	token string
	at    time.Time
}

// hostOf is the host the browser asked for, without a port and in one case.
func hostOf(r *http.Request) string {
	h := r.Host
	if hp, _, err := net.SplitHostPort(h); err == nil {
		h = hp
	}
	return strings.ToLower(h)
}

// onName says the browser came in on the household's name rather than a `.local` name or an
// address. The name is the one host the hop carries trust to, and a page served on it names its
// apps under the name too (view): a page opened securely must not lead to a `.local` link and
// its warning.
func onName(r *http.Request, c *casaView) bool {
	return c != nil && hostOf(r) == c.Name
}

// serveHop is the first half: on to the name with a code for this browser's session, or with
// an empty one. The name must be this home's, and the path a path.
func (a *app) serveHop(w http.ResponseWriter, r *http.Request) {
	name, to := r.URL.Query().Get("name"), r.URL.Query().Get("to")
	if own := a.casaName(); own == "" || name != own {
		http.Error(w, "that name is not this home's\n", http.StatusNotFound)
		return
	}
	if !strings.HasPrefix(to, "/") || strings.HasPrefix(to, "//") {
		to = "/"
	}
	u, err := url.Parse(a.casaScheme() + "://" + name + to)
	if err != nil {
		http.Error(w, "that is not a path\n", http.StatusBadRequest)
		return
	}
	q := u.Query()
	q.Set("hop", "")
	if _, ok := a.session(r); ok {
		code, err := newSecret()
		if err != nil {
			http.Error(w, "could not mint a code\n", http.StatusInternalServerError)
			return
		}
		a.mu.Lock()
		a.sweepHops()
		a.hops[code] = hop{token: sessionToken(r), at: a.now()}
		a.mu.Unlock()
		q.Set("hop", code)
	}
	u.RawQuery = q.Encode()
	http.Redirect(w, r, u.String(), http.StatusSeeOther)
}

// redeemHop is the name's half: the code becomes this host's cookie, the same token, and the
// browser gets the page it asked for without the code in its address bar. A code nobody minted,
// or one past its minute, opens nothing -- the ordinary refusal follows, as for an empty one.
func (a *app) redeemHop(w http.ResponseWriter, r *http.Request, code string) bool {
	a.mu.Lock()
	a.sweepHops()
	h, ok := a.hops[code]
	delete(a.hops, code)
	a.mu.Unlock()
	if !ok {
		return false
	}
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: h.token, Path: "/",
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: scheme(r) == "https",
		Expires: a.now().Add(365 * 24 * time.Hour),
	})
	q := r.URL.Query()
	q.Del("hop")
	u := *r.URL
	u.RawQuery = q.Encode()
	http.Redirect(w, r, u.RequestURI(), http.StatusSeeOther)
	return true
}

// sweepHops forgets codes past their minute. Called with the lock held.
func (a *app) sweepHops() {
	for code, h := range a.hops {
		if a.now().Sub(h.at) > hopTTL {
			delete(a.hops, code)
		}
	}
}
