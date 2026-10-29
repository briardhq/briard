package main

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"briard.io/shared/casa"
	"briard.io/shared/dashboard"
	"briard.io/shared/routes"
)

const hopName = "brave-elf.briard.casa"

// on sends one GET as a browser that asked for the given host, following nothing.
func (r *rig) on(host, path string, cookies ...*http.Cookie) *http.Response {
	r.t.Helper()
	req, _ := http.NewRequest("GET", r.srv.URL+path, nil)
	req.Host = host
	for _, c := range cookies {
		if c != nil {
			req.AddCookie(c)
		}
	}
	resp, err := noRedirect.Do(req)
	must(r.t, err)
	resp.Body.Close()
	return resp
}

func cookieNamed(resp *http.Response, name string) *http.Cookie {
	for _, c := range resp.Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// THE HOP: a browser trusted on the alias arrives at the name a stranger, is bounced to the
// alias, comes back with a code, and is trusted on the name as the SAME device -- one row,
// nothing typed. A browser trusted nowhere comes back with an empty code and sees quick-connect.
func TestTrustHopsFromTheAliasToTheName(t *testing.T) {
	r := newRig(t)
	r.app.casaPath = r.dir + "/casa.json"
	r.hostSays(t, dashboard.Casa{State: casa.ClaimRegistered, Name: hopName, Resolves: dashboard.CasaResolves})
	c := r.trust() // on the alias, as `briard open` leaves it

	// 1. The name, untrusted: bounced to the alias's /hop, the path kept, the attempt marked.
	resp := r.on(hopName, "/apps")
	loc := resp.Header.Get("Location")
	if resp.StatusCode != http.StatusSeeOther || !strings.HasPrefix(loc, "http://"+routes.BareFlockHostName+"/hop?") {
		t.Fatalf("the name's refusal = %d %q, want a bounce to the alias", resp.StatusCode, loc)
	}
	u, _ := url.Parse(loc)
	if u.Query().Get("name") != hopName || u.Query().Get("to") != "/apps" {
		t.Errorf("the bounce carries %q, want the name and the path", u.RawQuery)
	}
	mark := cookieNamed(resp, hopCookie)
	if mark == nil || mark.MaxAge != int(hopTTL/time.Second) {
		t.Errorf("the bounce left no minute-long mark: %+v", mark)
	}

	// 2. The alias's /hop, in the browser that holds the alias's cookie: back with a code.
	resp = r.on(routes.BareFlockHostName, "/hop?"+u.RawQuery, c)
	back, _ := url.Parse(resp.Header.Get("Location"))
	if resp.StatusCode != http.StatusSeeOther || back.Scheme != "https" || back.Host != hopName || back.Path != "/apps" || back.Query().Get("hop") == "" {
		t.Fatalf("the alias's answer = %d %q, want https://name/apps?hop=<code>", resp.StatusCode, back)
	}

	// 3. The name redeems it: the same token as its own cookie, the page without the code.
	resp = r.on(hopName, back.RequestURI())
	got := cookieNamed(resp, cookieName)
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/apps" || got == nil || got.Value != c.Value || !got.HttpOnly {
		t.Fatalf("the redemption = %d %q cookie=%+v, want 303 /apps with the alias's token", resp.StatusCode, resp.Header.Get("Location"), got)
	}
	if resp := r.on(hopName, "/apps", got); resp.StatusCode != http.StatusOK {
		t.Errorf("the name does not trust the hopped cookie: %d", resp.StatusCode)
	}
	if _, body := get(t, r, "/devices", got); strings.Count(body, "<tr><td>") != 1 {
		t.Errorf("the hop added a device row: %s", body)
	}
	// The code was one use.
	if resp := r.on(hopName, back.RequestURI()); resp.StatusCode != http.StatusUnauthorized || cookieNamed(resp, cookieName) != nil {
		t.Errorf("a spent code opened something: %d", resp.StatusCode)
	}

	// 4. A browser trusted nowhere: the alias answers with an empty code, and the name then
	// refuses without bouncing again; so does a browser carrying the mark; so does the alias
	// itself, and a fragment's fetch, which could not follow a bounce.
	resp = r.on(routes.BareFlockHostName, "/hop?"+u.RawQuery)
	if l := resp.Header.Get("Location"); resp.StatusCode != http.StatusSeeOther || l != "https://"+hopName+"/apps?hop=" {
		t.Errorf("the alias with no session = %d %q, want back with an empty hop", resp.StatusCode, l)
	}
	for host, path := range map[string]string{hopName: "/apps?hop=", routes.BareFlockHostName: "/apps", "192.168.1.20": "/"} {
		if resp := r.on(host, path); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s%s untrusted = %d, want the refusal", host, path, resp.StatusCode)
		}
	}
	if resp := r.on(hopName, "/apps", mark); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("the name bounced a browser that already tried: %d", resp.StatusCode)
	}
	if resp := r.on(hopName, "/f/address-step"); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("the name bounced a fragment: %d", resp.StatusCode)
	}

	// 5. Another household's name is not this alias's to answer for: no redirect anywhere.
	if resp := r.on(routes.BareFlockHostName, "/hop?name=other.briard.casa&to=/", c); resp.StatusCode != http.StatusNotFound {
		t.Errorf("the alias hopped for a foreign name: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	// A path that is not one is the home; a stale code is nothing.
	resp = r.on(routes.BareFlockHostName, "/hop?name="+hopName+"&to=//evil.example/", c)
	if back, _ := url.Parse(resp.Header.Get("Location")); back == nil || back.Host != hopName || back.Path != "/" {
		t.Errorf("a scheme-relative path was kept: %q", resp.Header.Get("Location"))
	}
	r.app.now = func() time.Time { return time.Now().Add(2 * hopTTL) }
	if resp := r.on(hopName, back.RequestURI()); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("a code past its minute opened something: %d", resp.StatusCode)
	}
}

// THE PAGE NAMES ITS APPS THE WAY IT WAS REACHED: under the household's name on the name, by
// `.local` otherwise -- and a page reached by `.local` or an address, while the name resolves
// with a certificate, is offered the name once, at the top.
func TestAppsAreNamedTheWayThePageWasReached(t *testing.T) {
	r := newRig(t)
	r.app.casaPath = r.dir + "/casa.json"
	c := r.trust()
	page := func(host, path string) string {
		req, _ := http.NewRequest("GET", r.srv.URL+path, nil)
		req.Host = host
		req.AddCookie(c)
		resp, err := http.DefaultClient.Do(req)
		must(t, err)
		return bodyOf(t, resp)
	}
	casaHA := routes.CasaHostName("brave-elf", "home-assistant")

	// Registered, resolving, with a certificate: the name is offered on the alias, not on the
	// name; the app's address (its page's About card) follows the host the page came in on.
	r.hostSays(t, dashboard.Casa{State: casa.ClaimRegistered, Name: hopName, Resolves: dashboard.CasaResolves, CertUntil: time.Now().Add(60 * 24 * time.Hour)})
	if body := page(routes.BareFlockHostName, "/"); !strings.Contains(body, `href="https://`+hopName+`">Open https://`+hopName+`</a>`) {
		t.Errorf("the alias's home does not offer the name: %s", body)
	}
	if body := page(hopName, "/"); strings.Contains(body, "Open Briard securely") {
		t.Errorf("the name's home offers itself: %s", body)
	}
	if body := page(routes.BareFlockHostName, "/apps/home-assistant"); !strings.Contains(body, "<code>briard-home-assistant.local</code>") || strings.Contains(body, casaHA) {
		t.Errorf("the alias's page names its app by the household's name: %s", body)
	}
	if body := page(hopName, "/apps/home-assistant"); !strings.Contains(body, "<code>"+casaHA+"</code>") || strings.Contains(body, "briard-home-assistant.local") {
		t.Errorf("the name's page names its app by .local: %s", body)
	}
	// No certificate yet, or the router blocking the name: nothing is offered.
	r.hostSays(t, dashboard.Casa{State: casa.ClaimRegistered, Name: hopName, Resolves: dashboard.CasaResolves})
	if body := page(routes.BareFlockHostName, "/"); strings.Contains(body, "Open Briard securely") {
		t.Errorf("the name is offered before its certificate: %s", body)
	}
	r.hostSays(t, dashboard.Casa{State: casa.ClaimRegistered, Name: hopName, Resolves: dashboard.CasaBlocked, CertUntil: time.Now().Add(60 * 24 * time.Hour)})
	if body := page(routes.BareFlockHostName, "/"); strings.Contains(body, "Open Briard securely") {
		t.Errorf("the name is offered while the router blocks it: %s", body)
	}
}
