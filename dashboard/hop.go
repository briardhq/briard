package main

// THE HOP: trust earned on one of the dashboard's names reaches the others.
//
// A session is a cookie, and a cookie is per host: a browser trusted on `briard.local` (the
// address `briard open` prints) arrives at `<flock>.briard.casa` as a stranger, and the name --
// the one address with a real certificate -- would ask the household to approve the device a
// second time. Nothing a page runs can cross that line: a script on the https name may not
// fetch the http alias (mixed content), and a cross-site fetch carries no cookie anyway. A
// top-level navigation does both, so that is the whole mechanism:
//
//   - A browser the NAME does not trust is sent to the alias's /hop, with the path it wanted.
//   - The alias's /hop, in the browser that holds the alias's cookie, mints a one-time code
//     bound to that device's own session token and sends the browser back to the name with it;
//     in a browser that holds nothing, it sends it back with an empty one.
//   - The name redeems the code as its own cookie -- the SAME token, so Trusted devices keeps
//     one row and revoking it revokes every name -- and shows the page that was asked for. An
//     empty code is the honest answer "no other trust to carry", and the name then asks a
//     trusted device as before.
//
// THE DEAD END, GUARDED. A browser that cannot resolve `.local` at all never comes back from the
// bounce: it sees its own "server not found" page, and the back button lands here again. So the
// name marks the attempt in a cookie that lives a minute, and a browser carrying that mark gets
// the ordinary quick-connect page instead of a second bounce. The mark is never read on the way
// that works.
//
// WHOSE ALIAS. `briard.local` is the alias every flock answers, so on a LAN with two households
// the bounce may reach the other one. The alias's /hop therefore answers only for its own name
// and refuses any other: a browser arriving there from a foreign name is sent nowhere (no open
// redirect through this host), and its own name's guard shows it quick-connect on the way back.

import (
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"briard.io/shared/routes"
)

const (
	hopCookie = "briard_hop" // the name's mark of a bounce tried, a minute long
	hopTTL    = time.Minute  // a code's life, and the mark's
)

// hop is one minted code, from the alias's mint until the name redeems it.
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

// bounce is the first half: an untrusted GET for a page on the name, with no hop answered and
// none tried, goes to the alias's /hop. Reports whether it did; the caller refuses otherwise.
// Only a page a person navigates to is bounced -- a fragment's fetch could not follow.
func (a *app) bounce(w http.ResponseWriter, r *http.Request) bool {
	name := a.casaName()
	if r.Method != http.MethodGet || strings.HasPrefix(r.URL.Path, "/f/") || name == "" || hostOf(r) != name {
		return false
	}
	if _, answered := r.URL.Query()["hop"]; answered {
		return false
	}
	if _, tried := r.Cookie(hopCookie); tried == nil {
		return false
	}
	http.SetCookie(w, &http.Cookie{Name: hopCookie, Value: "1", Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode,
		Secure: scheme(r) == "https", MaxAge: int(hopTTL / time.Second)})
	q := url.Values{"name": {name}, "to": {r.URL.RequestURI()}}
	http.Redirect(w, r, "http://"+routes.BareFlockHostName+"/hop?"+q.Encode(), http.StatusSeeOther)
	return true
}

// serveHop is the alias's half: back to the name with a code for this browser's session, or
// with an empty one. The name must be this home's, and the path a path.
func (a *app) serveHop(w http.ResponseWriter, r *http.Request) {
	name, to := r.URL.Query().Get("name"), r.URL.Query().Get("to")
	if own := a.casaName(); own == "" || name != own {
		http.Error(w, "that name is not this home's\n", http.StatusNotFound)
		return
	}
	if !strings.HasPrefix(to, "/") || strings.HasPrefix(to, "//") {
		to = "/"
	}
	u, err := url.Parse("https://" + name + to)
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
