package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"briard.io/shared/routes"
)

// gateDashboard stands in for the dashboard's two answers to the reverse proxy: a one-time code for
// a device's session token, and whether a token is still a trusted device's.
type gateDashboard struct {
	srv    *httptest.Server
	codes  map[string]string // code -> token
	valid  map[string]bool   // token -> stands
	user   string            // the username a standing token is answered with; empty: no record
	checks atomic.Int32
	asked  []string // the app each check named, in order
}

func newGateDashboard(t *testing.T) *gateDashboard {
	d := &gateDashboard{codes: map[string]string{}, valid: map[string]bool{}, user: "kostas"}
	d.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/gate/redeem":
			tok, ok := d.codes[r.FormValue("code")]
			delete(d.codes, r.FormValue("code"))
			if !ok {
				http.NotFound(w, r)
				return
			}
			fmt.Fprint(w, tok)
		case "/gate/check":
			d.checks.Add(1)
			d.asked = append(d.asked, r.URL.Query().Get("app"))
			if d.valid[r.Header.Get("X-Briard-Session")] {
				if d.user == "" {
					http.Error(w, "no user record yet", http.StatusConflict)
					return
				}
				fmt.Fprint(w, d.user)
				return
			}
			w.WriteHeader(http.StatusUnauthorized)
		default:
			fmt.Fprint(w, "dashboard")
		}
	}))
	t.Cleanup(d.srv.Close)
	return d
}

// gatedApp is an app that verifies the reverse proxy's signature as Files does, and records what
// reached it.
type gatedApp struct {
	srv    *httptest.Server
	key    []byte
	seen   *http.Request
	sub    string
	groups []string
}

func newGatedApp(t *testing.T, key []byte) *gatedApp {
	a := &gatedApp{key: key}
	a.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.seen = r
		sub, groups, err := verify(r.Header.Get(assertionHeader), a.key)
		if err != nil {
			http.Error(w, err.Error(), http.StatusForbidden)
			return
		}
		a.sub, a.groups = sub, groups
		fmt.Fprint(w, "app")
	}))
	t.Cleanup(a.srv.Close)
	return a
}

// verify is the app's side of HS256: the signature over the first two parts, and the expiry.
func verify(tok string, key []byte) (string, []string, error) {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return "", nil, fmt.Errorf("not a token: %q", tok)
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(parts[0] + "." + parts[1]))
	if base64.RawURLEncoding.EncodeToString(mac.Sum(nil)) != parts[2] {
		return "", nil, fmt.Errorf("bad signature")
	}
	raw, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var c struct {
		Sub    string   `json:"sub"`
		Groups []string `json:"groups"`
		Exp    int64    `json:"exp"`
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		return "", nil, err
	}
	if time.Now().Unix() > c.Exp {
		return "", nil, fmt.Errorf("expired")
	}
	return c.Sub, c.Groups, nil
}

type gateRig struct {
	proxy *httptest.Server
	dash  *gateDashboard
	app   *gatedApp
	key   string
}

const filesHost = "briard-files.local"

func newGateRig(t *testing.T) *gateRig {
	t.Helper()
	key := filepath.Join(t.TempDir(), "gate.key")
	if err := os.WriteFile(key, []byte("s3cret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := &gateRig{dash: newGateDashboard(t), key: key}
	r.app = newGatedApp(t, []byte("s3cret"))
	s := fronted("files", filesHost, r.app.srv.URL)
	s.Hosts = append(s.Hosts, "files.brave-elf.briard.casa")
	s.Gate = key
	du, _ := url.Parse(r.dash.srv.URL)
	r.proxy = httptest.NewServer(newRouter(fixed(routes.Table{Apps: []routes.App{s}}), du))
	t.Cleanup(r.proxy.Close)
	return r
}

// do sends one request through the reverse proxy without following redirects.
func (r *gateRig) do(t *testing.T, method, path, host string, cookies ...*http.Cookie) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(method, r.proxy.URL+path, nil)
	req.Host = host
	for _, c := range cookies {
		req.AddCookie(c)
	}
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// A browser with no session for the app is sent to the dashboard to open it, carrying where it
// was going; nothing reaches the app.
func TestTheGateSendsAStrangerToTheDashboard(t *testing.T) {
	r := newGateRig(t)
	resp := r.do(t, "GET", "/files/Home%20Assistant?x=1", filesHost)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status %d, want 303", resp.StatusCode)
	}
	loc, _ := url.Parse(resp.Header.Get("Location"))
	if loc.Scheme+"://"+loc.Host+loc.Path != "http://briard.local/open/files" || loc.Query().Get("to") != "/files/Home%20Assistant?x=1" || loc.Query().Get("host") != filesHost {
		t.Fatalf("sent to %s", loc)
	}
	if r.app.seen != nil {
		t.Fatal("the app was reached without a session")
	}
	// The app's page asking for data after its session went gets a refusal, not HTML.
	if resp := r.do(t, "POST", "/api/resources", filesHost); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a POST without a session answered %d, want 401", resp.StatusCode)
	}
}

// On the household's HTTPS name the dashboard asked is the household's name, not the alias.
func TestTheGateSendsTheHouseholdsNameToItsDashboard(t *testing.T) {
	req := httptest.NewRequest("GET", "/x", nil)
	req.Host = "files.brave-elf.briard.casa"
	req.TLS = &tls.ConnectionState{}
	if got := openURL(req, "files"); !strings.HasPrefix(got, "https://brave-elf.briard.casa/open/files?") {
		t.Fatalf("openURL = %s", got)
	}
}

// The dashboard's one-time code becomes the app name's cookie -- the device's own token -- and the
// browser goes on to where it was going, with the code gone from the address.
func TestACodeBecomesTheSession(t *testing.T) {
	r := newGateRig(t)
	r.dash.codes["c0de"] = "tok-1"
	resp := r.do(t, "GET", "/files/?briard_hop=c0de&view=list", filesHost)
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/files/?view=list" {
		t.Fatalf("status %d to %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	var got *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == sessionCookie {
			got = c
		}
	}
	if got == nil || got.Value != "tok-1" || !got.HttpOnly {
		t.Fatalf("session cookie %+v", got)
	}
	// Spent: the same code a second time is refused.
	if resp := r.do(t, "GET", "/?briard_hop=c0de", filesHost); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("a spent code answered %d, want 403", resp.StatusCode)
	}
}

// A standing session is forwarded signed: the app verifies the user the dashboard named, never sees the
// dashboard's token, and keeps its own cookies. A token a client brings is replaced.
func TestASessionIsForwardedSignedAndWithoutTheDashboardsToken(t *testing.T) {
	r := newGateRig(t)
	r.dash.valid["tok-1"] = true
	forged := sign([]byte("not the key"), "household", time.Now())
	req, _ := http.NewRequest("GET", r.proxy.URL+"/api/resources?jwt="+forged, nil)
	req.Host = filesHost
	req.Header.Set(assertionHeader, forged)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: "tok-1"})
	req.AddCookie(&http.Cookie{Name: "filebrowser_quantum_jwt", Value: "apps-own"})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || r.app.sub != "kostas" || !slices.Equal(r.app.groups, []string{"admin"}) {
		t.Fatalf("status %d (%s), signed in as %q in %v; want kostas, the admin", resp.StatusCode, body, r.app.sub, r.app.groups)
	}
	if _, err := r.app.seen.Cookie(sessionCookie); err == nil {
		t.Fatal("the dashboard's session token reached the app")
	}
	if c, err := r.app.seen.Cookie("filebrowser_quantum_jwt"); err != nil || c.Value != "apps-own" {
		t.Fatal("the app's own cookie did not reach it")
	}
	if r.app.seen.URL.Query().Has(assertionQuery) {
		t.Fatal("a client's own token reached the app in the query")
	}
}

// A revoked device is shut out at the next check, and a standing one is not asked about on every
// request.
func TestARevokedSessionIsShutOutAndAStandingOneIsKept(t *testing.T) {
	r := newGateRig(t)
	r.dash.valid["tok-1"] = true
	ck := &http.Cookie{Name: sessionCookie, Value: "tok-1"}
	for range 3 {
		if resp := r.do(t, "GET", "/", filesHost, ck); resp.StatusCode != http.StatusOK {
			t.Fatalf("a standing session answered %d", resp.StatusCode)
		}
	}
	if n := r.dash.checks.Load(); n != 1 {
		t.Fatalf("the dashboard was asked %d times for three requests, want once", n)
	}
	r.dash.valid["tok-1"] = false
	if resp := r.do(t, "GET", "/", filesHost, &http.Cookie{Name: sessionCookie, Value: "tok-2"}); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("an unknown token answered %d, want 303", resp.StatusCode)
	}
}

// The dashboard is asked about a session for each app by name, and its word for one app never
// admits the same session to another: its answer covers the app too (Mealie's first account is
// the user's before the answer is yes).
func TestASessionIsCheckedForEachAppByName(t *testing.T) {
	r := newGateRig(t)
	r.dash.valid["tok-1"] = true
	other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	t.Cleanup(other.Close)
	s := fronted("files", filesHost, r.app.srv.URL)
	s.Gate = r.key
	m := fronted("mealie", "briard-mealie.local", other.URL)
	m.Gate, m.GateHeader = r.key, "X-Mealie-Reverse-Proxy-Secret"
	du, _ := url.Parse(r.dash.srv.URL)
	r.proxy = httptest.NewServer(newRouter(fixed(routes.Table{Apps: []routes.App{s, m}}), du))
	t.Cleanup(r.proxy.Close)
	ck := &http.Cookie{Name: sessionCookie, Value: "tok-1"}
	for _, host := range []string{filesHost, "briard-mealie.local", "briard-mealie.local", filesHost} {
		if resp := r.do(t, "GET", "/", host, ck); resp.StatusCode != http.StatusOK {
			t.Fatalf("%s answered %d", host, resp.StatusCode)
		}
	}
	if !slices.Equal(r.dash.asked, []string{"files", "mealie"}) {
		t.Fatalf("the dashboard was asked about %q, want files then mealie, once each", r.dash.asked)
	}
}

// An app without a gate is forwarded exactly as before, and the dashboard's gate answers are
// never served to a browser through the reverse proxy.
func TestUngatedAppsAndTheDashboardsGateAnswers(t *testing.T) {
	plain := newGatedApp(t, nil)
	dash := newGateDashboard(t)
	du, _ := url.Parse(dash.srv.URL)
	srv := httptest.NewServer(newRouter(fixed(routes.Table{Apps: []routes.App{fronted("other", "briard-other.local", plain.srv.URL)}}), du))
	t.Cleanup(srv.Close)
	if code, _ := getHost(t, srv.URL, "/", "briard-other.local"); code != http.StatusForbidden || plain.seen.Header.Get(assertionHeader) != "" {
		t.Fatalf("an ungated app was signed into (status %d, header %q)", code, plain.seen.Header.Get(assertionHeader))
	}
	if code, _ := getHost(t, srv.URL, "/gate/redeem", "briard.local"); code != http.StatusNotFound {
		t.Fatalf("the dashboard's gate answer was served through the reverse proxy: %d", code)
	}
}

// A trusted device the dashboard has nobody to name for is refused here, never sent to the
// dashboard, whose /open would send it straight back.
func TestATrustedDeviceWithNoUserIsRefusedNotBounced(t *testing.T) {
	r := newGateRig(t)
	r.dash.valid["tok-1"] = true
	r.dash.user = ""
	resp := r.do(t, "GET", "/", filesHost, &http.Cookie{Name: sessionCookie, Value: "tok-1"})
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("answered %d (to %q), want 503", resp.StatusCode, resp.Header.Get("Location"))
	}
	if r.app.seen != nil {
		t.Fatal("the app was reached with nobody to sign in")
	}
}

// An app gated by header gets the user and the secret itself, in place of whatever a client sent in
// either header, and no token and no dashboard cookie.
func TestAHeaderGatedAppGetsTheUserAndTheSecret(t *testing.T) {
	key := filepath.Join(t.TempDir(), "REVERSE_PROXY_AUTH_SECRET")
	if err := os.WriteFile(key, []byte("s3cret"), 0o400); err != nil {
		t.Fatal(err)
	}
	var seen *http.Request
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { seen = r }))
	t.Cleanup(app.Close)
	dash := newGateDashboard(t)
	dash.valid["tok-1"] = true
	s := fronted("mealie", "briard-mealie.local", app.URL)
	s.Gate, s.GateHeader = key, "X-Mealie-Reverse-Proxy-Secret"
	du, _ := url.Parse(dash.srv.URL)
	proxy := httptest.NewServer(newRouter(fixed(routes.Table{Apps: []routes.App{s}}), du))
	t.Cleanup(proxy.Close)

	req, _ := http.NewRequest("GET", proxy.URL+"/api/auth/reverse-proxy", nil)
	req.Host = "briard-mealie.local"
	req.Header.Set("X-Forwarded-User", "admin")
	req.Header.Set("X-Mealie-Reverse-Proxy-Secret", "guessed")
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: "tok-1"})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || seen == nil {
		t.Fatalf("status %d, reached the app: %v", resp.StatusCode, seen != nil)
	}
	if u, sec := seen.Header.Values("X-Forwarded-User"), seen.Header.Values("X-Mealie-Reverse-Proxy-Secret"); !slices.Equal(u, []string{"kostas"}) || !slices.Equal(sec, []string{"s3cret"}) {
		t.Fatalf("the app was told user %v with secret %v; want only kostas and the file's secret", u, sec)
	}
	if seen.Header.Get(assertionHeader) != "" {
		t.Fatal("a header-gated app was also handed a token")
	}
	if _, err := seen.Cookie(sessionCookie); err == nil {
		t.Fatal("the dashboard's session token reached the app")
	}
}
