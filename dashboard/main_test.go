package main

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"briard.io/shared/dashboard"
	"briard.io/shared/routes"
)

// fakeHA is Home Assistant's onboarding surface as the dashboard uses it, with the shapes
// [V3b.31a](f) measured: unauthenticated status, a user step that returns a code and refuses
// once done, an analytics step behind a bearer, the token exchange, and the core state.
type fakeHA struct {
	mu       sync.Mutex
	done     map[string]bool
	state    string
	user     map[string]string // what the user step received
	analytic string            // the bearer analytics was marked with
	// The in-HA integration's minter ([V3b.31d]): absent (404) when noMinter, refusing when
	// noOwner, otherwise a code bound to the client_id it was given.
	noMinter, noOwner bool
	mintClient        string // the client_id the minter was asked for
	mintBearer        string // the bearer it was asked with
}

func newFakeHA() *fakeHA {
	return &fakeHA{done: map[string]bool{"user": false, "core_config": false, "analytics": false, "integration": false}, state: "RUNNING"}
}

func (f *fakeHA) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch r.URL.Path {
	case "/api/onboarding":
		var out []map[string]any
		for _, s := range []string{"user", "core_config", "analytics", "integration"} {
			out = append(out, map[string]any{"step": s, "done": f.done[s]})
		}
		json.NewEncoder(w).Encode(out)
	case "/api/onboarding/users":
		if f.done["user"] {
			http.Error(w, "User step already done", http.StatusForbidden)
			return
		}
		var u map[string]string
		json.NewDecoder(r.Body).Decode(&u)
		f.user = u
		f.done["user"] = true
		json.NewEncoder(w).Encode(map[string]string{"auth_code": "code-for-the-browser"})
	case "/api/onboarding/analytics":
		f.analytic = r.Header.Get("Authorization")
		if f.done["analytics"] {
			http.Error(w, "already", http.StatusForbidden)
			return
		}
		f.done["analytics"] = true
		w.Write([]byte("{}"))
	case "/auth/token":
		r.ParseForm()
		if r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("refresh_token") == "" {
			http.Error(w, "bad", http.StatusBadRequest)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"access_token": "sys-access", "expires_in": 1800})
	case "/api/config":
		json.NewEncoder(w).Encode(map[string]string{"state": f.state})
	case "/api/briard/login":
		if f.noMinter {
			http.NotFound(w, r)
			return
		}
		f.mintBearer = r.Header.Get("Authorization")
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		f.mintClient = body["client_id"]
		if f.noOwner {
			w.WriteHeader(http.StatusConflict)
			json.NewEncoder(w).Encode(map[string]string{"message": "Home Assistant has no owner account", "code": "no_owner"})
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"auth_code": "code-for-the-owner"})
	default:
		http.NotFound(w, r)
	}
}

// rig is a dashboard wired to a fake HA through a routes table, with a fresh handoff on tmpfs.
type rig struct {
	t     *testing.T
	ha    *fakeHA
	app   *app
	srv   *httptest.Server
	dir   string
	code  string
	haURL *url.URL
}

func newRig(t *testing.T) *rig {
	t.Helper()
	ha := newFakeHA()
	haSrv := httptest.NewServer(ha)
	t.Cleanup(haSrv.Close)
	u, _ := url.Parse(haSrv.URL)
	dir := t.TempDir()
	tbl := routes.Table{Services: []routes.Service{{
		Name: "home-assistant", Hosts: []string{"briard-brave-elf-home-assistant.local"},
		Address: u.Hostname(), Health: "http://:" + u.Port() + "/manifest.json",
		Routes: []routes.Route{{Listen: routes.ListenName, To: "http://:" + u.Port()}},
	}}}
	raw, _ := json.Marshal(tbl)
	must(t, os.WriteFile(filepath.Join(dir, "routes.json"), raw, 0o644))
	must(t, os.WriteFile(filepath.Join(dir, "token"), []byte("the-control-token\n"), 0o600))
	code, _ := dashboard.NewCode()
	h := dashboard.Handoff{Code: code, Name: "Kostas", Username: "kostas", Language: "el", Issued: time.Now()}
	raw, _ = json.Marshal(h)
	must(t, os.WriteFile(filepath.Join(dir, "handoff.json"), raw, 0o600))
	a := newApp(filepath.Join(dir, "routes.json"), filepath.Join(dir, "handoff.json"), filepath.Join(dir, "state"), filepath.Join(dir, "token"))
	srv := httptest.NewServer(a)
	t.Cleanup(srv.Close)
	return &rig{t: t, ha: ha, app: a, srv: srv, dir: dir, code: code, haURL: u}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// noRedirect is a client that hands redirects back rather than following them: the Location is
// the assertion.
var noRedirect = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

func (r *rig) do(method, path string, cookie *http.Cookie, hdr map[string]string) *http.Response {
	r.t.Helper()
	req, _ := http.NewRequest(method, r.srv.URL+path, nil)
	if cookie != nil {
		req.AddCookie(cookie)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := noRedirect.Do(req)
	must(r.t, err)
	resp.Body.Close()
	return resp
}

// trust redeems the code and returns the session cookie.
func (r *rig) trust() *http.Cookie {
	r.t.Helper()
	resp := r.do("GET", "/?code="+r.code, nil, nil)
	if resp.StatusCode != http.StatusSeeOther {
		r.t.Fatalf("redeem = %d, want 303", resp.StatusCode)
	}
	for _, c := range resp.Cookies() {
		if c.Name == cookieName {
			return c
		}
	}
	r.t.Fatal("no session cookie set")
	return nil
}

// THE CODE IS THE WHOLE OF THE BOOTSTRAP: nothing without it, a trusted device with it, and it
// works exactly once.
func TestCodeIsTheOnlyDoorAndItIsOneTime(t *testing.T) {
	r := newRig(t)
	if resp := r.do("GET", "/", nil, nil); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("/ with no session = %d, want 401", resp.StatusCode)
	}
	if resp := r.do("GET", "/?code=not-it", nil, nil); resp.StatusCode != http.StatusForbidden {
		t.Errorf("wrong code = %d, want 403", resp.StatusCode)
	}
	// A wrong guess does not burn the right code.
	if _, err := os.Stat(r.app.handoffPath); err != nil {
		t.Fatalf("a wrong guess consumed the handoff: %v", err)
	}
	c := r.trust()
	if !c.HttpOnly || c.SameSite != http.SameSiteLaxMode || c.Secure {
		t.Errorf("cookie flags = HttpOnly:%v SameSite:%v Secure:%v; want HttpOnly, Lax, not Secure over http", c.HttpOnly, c.SameSite, c.Secure)
	}
	if _, err := os.Stat(r.app.handoffPath); !os.IsNotExist(err) {
		t.Errorf("the handoff survived its redemption (%v); the code must be one-time", err)
	}
	if resp := r.do("GET", "/?code="+r.code, nil, nil); resp.StatusCode != http.StatusForbidden {
		t.Errorf("second redemption = %d, want 403", resp.StatusCode)
	}
	resp := r.do("GET", "/", c, nil)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("/ with the session = %d, want 200", resp.StatusCode)
	}
	// The registry holds a hash, never the token.
	reg, _ := os.ReadFile(filepath.Join(r.dir, "state", devicesFile))
	if strings.Contains(string(reg), c.Value) {
		t.Error("the registry stores the raw token")
	}
	if !strings.Contains(string(reg), hashToken(c.Value)) {
		t.Error("the registry does not hold the token's hash")
	}
	// The account the host handed over outlives the code.
	acct, _ := os.ReadFile(filepath.Join(r.dir, "state", accountFile))
	if !strings.Contains(string(acct), `"username": "kostas"`) || strings.Contains(string(acct), r.code) {
		t.Errorf("account kept = %s; want the OS account without the code", acct)
	}
}

func TestExpiredCodeIsRefusedAndDropped(t *testing.T) {
	r := newRig(t)
	r.app.now = func() time.Time { return time.Now().Add(dashboard.TTL + time.Minute) }
	if resp := r.do("GET", "/?code="+r.code, nil, nil); resp.StatusCode != http.StatusForbidden {
		t.Errorf("expired code = %d, want 403", resp.StatusCode)
	}
	if _, err := os.Stat(r.app.handoffPath); !os.IsNotExist(err) {
		t.Error("an expired handoff was kept")
	}
}

// THE BUTTON, on a fresh Home Assistant: the first user is made from the account the host handed
// over, analytics is marked with the CONTROL CHANNEL's token (the browser's code stays unspent),
// and the browser is sent to HA's own onboarding page carrying that code and the state its
// frontend checks -- hassUrl without the trailing slash, clientId with it ([V3b.31a](f)1).
func TestOpenLandsOnHomeAssistantsOnboardingWithTheCode(t *testing.T) {
	r := newRig(t)
	c := r.trust()
	body := r.do("GET", "/", c, nil)
	if body.StatusCode != http.StatusOK {
		t.Fatalf("/ = %d", body.StatusCode)
	}
	resp := r.do("POST", "/open/home-assistant", c, nil)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("open = %d, want 303", resp.StatusCode)
	}
	loc, err := url.Parse(resp.Header.Get("Location"))
	must(t, err)
	if loc.Scheme != "http" || loc.Host != "briard-brave-elf-home-assistant.local" || loc.Path != "/onboarding.html" {
		t.Errorf("landed on %s; want http://briard-brave-elf-home-assistant.local/onboarding.html", loc)
	}
	q := loc.Query()
	if q.Get("auth_callback") != "1" || q.Get("code") != "code-for-the-browser" {
		t.Errorf("query = %v; want auth_callback=1 and the user step's code", q)
	}
	stateRaw, err := base64.StdEncoding.DecodeString(q.Get("state"))
	must(t, err)
	var state struct{ HassURL, ClientID string }
	must(t, json.Unmarshal(stateRaw, &state))
	if state.HassURL != "http://briard-brave-elf-home-assistant.local" || state.ClientID != "http://briard-brave-elf-home-assistant.local/" {
		t.Errorf("state = %+v; want hassUrl without and clientId with the trailing slash", state)
	}
	// What HA was asked for.
	r.ha.mu.Lock()
	u, an := r.ha.user, r.ha.analytic
	r.ha.mu.Unlock()
	if u["name"] != "Kostas" || u["username"] != "kostas" || u["language"] != "el" || u["client_id"] != state.ClientID {
		t.Errorf("user step got %v; want the handed-over account bound to the browser's client_id", u)
	}
	if len(u["password"]) < 16 {
		t.Errorf("password %q is too short", u["password"])
	}
	if an != "Bearer sys-access" {
		t.Errorf("analytics was marked with %q; want the control channel's token", an)
	}
	// The starting password is kept, and the card shows it -- never invented-and-hidden.
	pw, err := os.ReadFile(filepath.Join(r.dir, "state", passwordFile))
	must(t, err)
	if string(pw) != u["password"] {
		t.Errorf("kept password %q != the one HA got %q", pw, u["password"])
	}
	req, _ := http.NewRequest("GET", r.srv.URL+"/", nil)
	req.AddCookie(c)
	page, err := http.DefaultClient.Do(req)
	must(t, err)
	defer page.Body.Close()
	var sb strings.Builder
	buf := make([]byte, 64<<10)
	n, _ := page.Body.Read(buf)
	sb.Write(buf[:n])
	if !strings.Contains(sb.String(), u["password"]) {
		t.Error("the page does not show the starting password")
	}
	// User step done, the rest not: a code MINTED for the owner ([V3b.31d]), and the onboarding
	// page resumes with it -- the same callback shape as the first open, no user created.
	resp = r.do("POST", "/open/home-assistant", c, nil)
	loc, _ = url.Parse(resp.Header.Get("Location"))
	if resp.StatusCode != http.StatusSeeOther || loc.Host != "briard-brave-elf-home-assistant.local" || loc.Path != "/onboarding.html" || loc.Query().Get("code") != "code-for-the-owner" {
		t.Errorf("open after the user step = %d %q; want 303 to the onboarding page with the minted code", resp.StatusCode, loc)
	}
	if r.ha.mintClient != "http://briard-brave-elf-home-assistant.local/" || r.ha.mintBearer != "Bearer sys-access" {
		t.Errorf("the mint was asked for client %q with %q; want the browser's origin/ and the control channel's bearer", r.ha.mintClient, r.ha.mintBearer)
	}
	// Everything done: HA's own auth callback on its front page, with the tokens stored.
	r.ha.mu.Lock()
	for k := range r.ha.done {
		r.ha.done[k] = true
	}
	r.ha.mu.Unlock()
	resp = r.do("POST", "/open/home-assistant", c, nil)
	loc, _ = url.Parse(resp.Header.Get("Location"))
	q = loc.Query()
	if resp.StatusCode != http.StatusSeeOther || loc.Host != "briard-brave-elf-home-assistant.local" || loc.Path != "/" ||
		q.Get("auth_callback") != "1" || q.Get("code") != "code-for-the-owner" || q.Get("storeToken") != "true" {
		t.Errorf("open when onboarded = %d %q; want 303 to HA's front page with auth_callback, the minted code and storeToken", resp.StatusCode, loc)
	}
	stateRaw, _ = base64.StdEncoding.DecodeString(q.Get("state"))
	state = struct{ HassURL, ClientID string }{}
	must(t, json.Unmarshal(stateRaw, &state))
	if state.HassURL != "http://briard-brave-elf-home-assistant.local" || state.ClientID != "http://briard-brave-elf-home-assistant.local/" {
		t.Errorf("later-open state = %+v", state)
	}
	// The page offers the button on a set-up HA too: that IS the later open.
	req, _ = http.NewRequest("GET", r.srv.URL+"/", nil)
	req.AddCookie(c)
	page, err = http.DefaultClient.Do(req)
	must(t, err)
	n, _ = page.Body.Read(buf)
	page.Body.Close()
	if !strings.Contains(string(buf[:n]), `<button type="submit">Open Home Assistant`) {
		t.Error("the page on a set-up HA does not offer Open Home Assistant")
	}
}

// THE MINTER MINTS FOR THE OWNER AND NOBODY ELSE ([V3b.31a](e)): with no owner it refuses, and the
// dashboard SAYS SO with the plain address, rather than minting for some admin. A Home Assistant
// our integration is not loaded on gets the same honesty and its own login screen.
func TestOpenSurfacesARefusedMint(t *testing.T) {
	r := newRig(t)
	c := r.trust()
	r.ha.mu.Lock()
	for k := range r.ha.done {
		r.ha.done[k] = true
	}
	r.ha.noOwner = true
	r.ha.mu.Unlock()
	open := func() (*http.Response, string) {
		req, _ := http.NewRequest("POST", r.srv.URL+"/open/home-assistant", nil)
		req.AddCookie(c)
		resp, err := noRedirect.Do(req)
		must(t, err)
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp, string(body)
	}
	resp, body := open()
	if resp.StatusCode != http.StatusConflict || !strings.Contains(body, "no owner") || !strings.Contains(body, "http://briard-brave-elf-home-assistant.local/") {
		t.Errorf("open with no owner = %d %q; want 409 naming the missing owner and the address", resp.StatusCode, body)
	}
	if resp.Header.Get("Location") != "" {
		t.Error("a refused mint must not redirect anywhere")
	}
	r.ha.mu.Lock()
	r.ha.noOwner, r.ha.noMinter = false, true
	r.ha.mu.Unlock()
	resp, body = open()
	if resp.StatusCode != http.StatusBadGateway || !strings.Contains(body, "integration") || !strings.Contains(body, "http://briard-brave-elf-home-assistant.local/") {
		t.Errorf("open with no minter = %d %q; want 502 naming the integration and the address", resp.StatusCode, body)
	}
}

// RUNNING IS THE BOUNDARY, NOT A 200 ([B.127]): a Home Assistant that answers HTTP but has not
// reached RUNNING gets no user step.
func TestOpenRefusesWhileHomeAssistantIsStarting(t *testing.T) {
	r := newRig(t)
	c := r.trust()
	r.ha.mu.Lock()
	r.ha.state = "STARTING"
	r.ha.mu.Unlock()
	resp := r.do("POST", "/open/home-assistant", c, nil)
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("open while STARTING = %d, want 409", resp.StatusCode)
	}
	r.ha.mu.Lock()
	created := r.ha.user != nil
	r.ha.mu.Unlock()
	if created {
		t.Error("a user was created on a Home Assistant that was not RUNNING")
	}
}

// The door says https on this one path, and only the door: the cookie goes Secure and Home
// Assistant's origin follows the scheme the browser actually used.
func TestSchemeFollowsTheDoor(t *testing.T) {
	r := newRig(t)
	resp := r.do("GET", "/?code="+r.code, nil, map[string]string{"X-Forwarded-Proto": "https"})
	var c *http.Cookie
	for _, k := range resp.Cookies() {
		if k.Name == cookieName {
			c = k
		}
	}
	if c == nil || !c.Secure {
		t.Fatalf("cookie over https = %+v; want Secure", c)
	}
	resp = r.do("POST", "/open/home-assistant", c, map[string]string{"X-Forwarded-Proto": "https"})
	if got := resp.Header.Get("Location"); !strings.HasPrefix(got, "https://briard-brave-elf-home-assistant.local/onboarding.html?") {
		t.Errorf("landed on %q; want the https origin", got)
	}
}

func TestNotInstalled(t *testing.T) {
	r := newRig(t)
	c := r.trust()
	must(t, os.Remove(filepath.Join(r.dir, "routes.json")))
	resp := r.do("POST", "/open/home-assistant", c, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("open with nothing installed = %d, want 404", resp.StatusCode)
	}
	if resp := r.do("GET", "/", c, nil); resp.StatusCode != http.StatusOK {
		t.Errorf("/ with nothing installed = %d, want 200 (the page says so)", resp.StatusCode)
	}
}
