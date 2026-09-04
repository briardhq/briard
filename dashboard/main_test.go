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
	// The password is generated and FORGOTTEN ([V3b.31e]): nothing on the volume, nothing on the
	// page. Every later open is minted, and the household sets its own in HA's People settings.
	entries, _ := os.ReadDir(filepath.Join(r.dir, "state"))
	for _, e := range entries {
		if strings.Contains(e.Name(), "password") {
			t.Errorf("a password is kept on the volume: %s", e.Name())
		}
	}
	req, _ := http.NewRequest("GET", r.srv.URL+"/", nil)
	req.AddCookie(c)
	page, err := http.DefaultClient.Do(req)
	must(t, err)
	defer page.Body.Close()
	buf := make([]byte, 64<<10)
	n, _ := page.Body.Read(buf)
	if strings.Contains(string(buf[:n]), u["password"]) {
		t.Error("the page shows the generated password")
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

// reissue writes a fresh handoff, the way `briard dashboard` does for the next device, and
// redeems it with the given user agent.
func (r *rig) reissue(agent string) *http.Cookie {
	r.t.Helper()
	code, _ := dashboard.NewCode()
	raw, _ := json.Marshal(dashboard.Handoff{Code: code, Name: "Kostas", Username: "kostas", Language: "el", Issued: time.Now()})
	must(r.t, os.WriteFile(r.app.handoffPath, raw, 0o600))
	resp := r.do("GET", "/?code="+code, nil, map[string]string{"User-Agent": agent})
	if resp.StatusCode != http.StatusSeeOther {
		r.t.Fatalf("reissued redeem = %d, want 303", resp.StatusCode)
	}
	for _, c := range resp.Cookies() {
		if c.Name == cookieName {
			return c
		}
	}
	r.t.Fatal("no session cookie set")
	return nil
}

func (r *rig) page(c *http.Cookie) string {
	r.t.Helper()
	req, _ := http.NewRequest("GET", r.srv.URL+"/", nil)
	req.AddCookie(c)
	resp, err := http.DefaultClient.Do(req)
	must(r.t, err)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return string(body)
}

func (r *rig) registry() registry {
	r.t.Helper()
	var reg registry
	must(r.t, r.app.readState(devicesFile, &reg))
	return reg
}

// THE DEVICE LIST AND REVOKE ([V3b.31f]): every trusted device is on the page with a label a
// person can tell apart and "this device" on the one looking; revoking one takes it out of the
// registry and nothing else -- the revoked cookie is refused, the others keep working, and the
// page points at Home Assistant's own profile for the HA session it still holds. A device
// revoking itself is signed out, cookie cleared; the last one may go, and a fresh code is the
// way back in.
func TestDevicesAreListedAndRevokedFromTheRegistryOnly(t *testing.T) {
	r := newRig(t)
	laptop := r.reissue("Mozilla/5.0 (X11; Linux x86_64; rv:128.0) Gecko/20100101 Firefox/128.0")
	phone := r.reissue("Mozilla/5.0 (iPhone; CPU iPhone OS 17_5 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.5 Mobile/15E148 Safari/604.1")
	reg := r.registry()
	if len(reg.Devices) != 2 {
		t.Fatalf("registry holds %d devices, want 2", len(reg.Devices))
	}
	laptopID, phoneID := reg.Devices[0].ID, reg.Devices[1].ID

	body := r.page(phone)
	for _, want := range []string{"Firefox on Linux", "Safari on iPhone", `name="id" value="` + laptopID + `"`, `name="id" value="` + phoneID + `"`,
		"/profile/security", `<a href="http://briard-brave-elf-home-assistant.local/profile/security">`} {
		if !strings.Contains(body, want) {
			t.Errorf("the phone's page lacks %q", want)
		}
	}
	if strings.Count(body, "this device") != 1 || !strings.Contains(body, "Safari on iPhone</span> <span class=\"muted\">— added") {
		t.Errorf("the page does not mark exactly the viewing device")
	}
	if strings.Contains(body, laptop.Value) || strings.Contains(body, phone.Value) || strings.Contains(body, reg.Devices[0].Hash) {
		t.Error("the page leaks a token or its hash")
	}

	// No session, no revoke -- and the registry is untouched.
	req, _ := http.NewRequest("POST", r.srv.URL+"/devices/revoke", strings.NewReader("id="+laptopID))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := noRedirect.Do(req)
	must(t, err)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized || len(r.registry().Devices) != 2 {
		t.Errorf("revoke with no session = %d and %d devices left; want 401 and 2", resp.StatusCode, len(r.registry().Devices))
	}
	revoke := func(c *http.Cookie, id string) *http.Response {
		req, _ := http.NewRequest("POST", r.srv.URL+"/devices/revoke", strings.NewReader("id="+id))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(c)
		resp, err := noRedirect.Do(req)
		must(t, err)
		resp.Body.Close()
		return resp
	}
	if resp := revoke(phone, "nope"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("revoke of an unknown id = %d, want 404", resp.StatusCode)
	}

	// The phone revokes the laptop: the laptop is refused, the phone is not, and its own cookie
	// was not touched.
	resp = revoke(phone, laptopID)
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/" {
		t.Errorf("revoke = %d %q; want 303 home", resp.StatusCode, resp.Header.Get("Location"))
	}
	if len(resp.Cookies()) != 0 {
		t.Errorf("revoking another device touched the cookie: %v", resp.Cookies())
	}
	if resp := r.do("GET", "/", laptop, nil); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("the revoked laptop = %d, want 401", resp.StatusCode)
	}
	if resp := r.do("POST", "/open/home-assistant", laptop, nil); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("the revoked laptop can still open Home Assistant: %d", resp.StatusCode)
	}
	if resp := r.do("GET", "/", phone, nil); resp.StatusCode != http.StatusOK {
		t.Errorf("the phone after revoking the laptop = %d, want 200", resp.StatusCode)
	}
	reg = r.registry()
	if len(reg.Devices) != 1 || reg.Devices[0].ID != phoneID {
		t.Errorf("registry after revoke = %+v; want the phone alone", reg.Devices)
	}
	// Nothing was asked of Home Assistant: the revoke is briard's registry and no more.
	if r.ha.mintClient != "" {
		t.Error("revoking a device touched Home Assistant")
	}

	// The phone signs itself out: cookie cleared, refused after, registry empty -- and the way
	// back is a fresh code, as at install.
	resp = revoke(phone, phoneID)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("self-revoke = %d, want 303", resp.StatusCode)
	}
	cleared := false
	for _, c := range resp.Cookies() {
		if c.Name == cookieName && c.MaxAge < 0 && c.Value == "" {
			cleared = true
		}
	}
	if !cleared {
		t.Errorf("self-revoke did not clear the cookie: %v", resp.Cookies())
	}
	if resp := r.do("GET", "/", phone, nil); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("the signed-out phone = %d, want 401", resp.StatusCode)
	}
	if raw, _ := os.ReadFile(filepath.Join(r.dir, "state", devicesFile)); !strings.Contains(string(raw), `"devices": []`) {
		t.Errorf("registry after the last device left = %s; want an empty list, not null", raw)
	}
	if c := r.reissue("curl/8.0"); r.do("GET", "/", c, nil).StatusCode != http.StatusOK {
		t.Error("a fresh code does not trust a device again after the registry emptied")
	}
}

func TestDeviceLabel(t *testing.T) {
	for ua, want := range map[string]string{
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36 Edg/126.0.0.0":  "Edge on Windows",
		"Mozilla/5.0 (Linux; Android 14; Pixel 8) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Mobile Safari/537.36":          "Chrome on Android",
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 14_5) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.5 Safari/605.1.15":             "Safari on Mac",
		"Mozilla/5.0 (iPad; CPU OS 17_5 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.5 Mobile/15E148 Safari/604.1": "Safari on iPad",
		"curl/8.5.0":      "curl",
		"":                "Unknown device",
		"SomethingElse/1": "SomethingElse/1",
	} {
		if got := deviceLabel(ua); got != want {
			t.Errorf("deviceLabel(%q) = %q, want %q", ua, got, want)
		}
	}
}
