package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"briard.io/shared/casa"
	"briard.io/shared/dashboard"
	"briard.io/shared/manifest"
	"briard.io/shared/routes"
)

// gateRig is the dashboard rig with Files in its routing table, gated.
func gateRig(t *testing.T) *rig {
	r := newRig(t)
	raw, _ := os.ReadFile(filepath.Join(r.dir, "routes.json"))
	var tbl routes.Table
	must(t, json.Unmarshal(raw, &tbl))
	tbl.Apps = append(tbl.Apps, routes.App{
		Name: "files", Hosts: routes.HostNames("brave-elf", "files"), Address: "10.12.7.2",
		Health: "http://:80/health", Routes: []routes.Route{{Listen: routes.ListenName, To: "http://:80"}},
		Gate: "/run/briard/files/gate.key",
	})
	raw, _ = json.Marshal(tbl)
	must(t, os.WriteFile(filepath.Join(r.dir, "routes.json"), raw, 0o644))
	return r
}

// loopback is the door asking: no X-Forwarded-Proto, which the door sets on all it forwards.
func (r *rig) loopback(method, path, session string, form url.Values) (int, string) {
	r.t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, _ := http.NewRequest(method, r.srv.URL+path, body)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if session != "" {
		req.Header.Set("X-Briard-Session", session)
	}
	resp, err := noRedirect.Do(req)
	must(r.t, err)
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// A trusted browser opening Files is sent to Files' own name with a one-time code, which the door
// trades -- once -- for the device's own token: one device, one row, one revoke.
func TestOpeningAGatedAppCarriesTheSessionToItsName(t *testing.T) {
	r := gateRig(t)
	c := r.trust()
	resp := r.on(routes.BareFlockHostName, "/open/files?host=briard-files.local&to=/files/Home%20Assistant", c)
	loc, _ := url.Parse(resp.Header.Get("Location"))
	if resp.StatusCode != http.StatusSeeOther || loc.Scheme != "http" || loc.Host != "briard-files.local" || loc.Path != "/files/Home Assistant" {
		t.Fatalf("open = %d %q", resp.StatusCode, loc)
	}
	code := loc.Query().Get(gateHopParam)
	if code == "" {
		t.Fatalf("no code in %q", loc)
	}
	status, tok := r.loopback("POST", "/gate/redeem", "", url.Values{"code": {code}})
	if status != http.StatusOK || tok != c.Value {
		t.Fatalf("redeem = %d %q, want the device's own token", status, tok)
	}
	if status, _ := r.loopback("POST", "/gate/redeem", "", url.Values{"code": {code}}); status != http.StatusNotFound {
		t.Fatalf("a spent code redeemed again: %d", status)
	}
	// The check names the user the door signs in: the record the open wrote, never "household".
	if status, user := r.loopback("GET", "/gate/check", c.Value, nil); status != http.StatusOK || user != "kostas" {
		t.Fatalf("check on a trusted device's token = %d %q, want 200 kostas", status, user)
	}
	// A trusted device with no record (one that could not be written) is a conflict, not a stranger.
	if err := os.Remove(filepath.Join(r.dir, "state", accountFile)); err != nil {
		t.Fatal(err)
	}
	if status, _ := r.loopback("GET", "/gate/check", c.Value, nil); status != http.StatusConflict {
		t.Fatalf("check with no record = %d, want 409", status)
	}
	if status, _ := r.loopback("GET", "/gate/check", "not-a-token", nil); status != http.StatusUnauthorized {
		t.Fatalf("check on a stranger's token = %d, want 401", status)
	}
}

// Opening from the household's name lands on Files' household name, over HTTPS; a host that is
// not one of the app's is never a destination, and a scheme-relative path is the app's home.
func TestOpeningFollowsTheNameAndGoesNowhereElse(t *testing.T) {
	r := gateRig(t)
	r.app.casaPath = r.dir + "/casa.json"
	r.hostSays(t, dashboard.Casa{State: casa.ClaimRegistered, Name: hopName, Resolves: dashboard.CasaResolves})
	c := r.trust()
	resp := r.on(hopName, "/open/files?host=evil.example&to=//evil.example/x", c)
	loc, _ := url.Parse(resp.Header.Get("Location"))
	if resp.StatusCode != http.StatusSeeOther || loc.Scheme != "https" || loc.Host != "files.brave-elf.briard.casa" || loc.Path != "/" {
		t.Fatalf("open on the name = %d %q, want https://files.brave-elf.briard.casa/", resp.StatusCode, loc)
	}
}

// Only a gated app opens this way, only for a trusted browser, and the gate's answers are never
// given to a browser through the door.
func TestTheGateIsShut(t *testing.T) {
	r := gateRig(t)
	c := r.trust()
	if resp := r.on(routes.BareFlockHostName, "/open/home-assistant", c); resp.StatusCode != http.StatusNotFound {
		t.Errorf("an app with its own login opened through the gate: %d", resp.StatusCode)
	}
	if resp := r.on(routes.BareFlockHostName, "/open/files"); resp.StatusCode != http.StatusUnauthorized || resp.Header.Get("Location") != "" {
		t.Errorf("a stranger was sent on: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	for _, path := range []string{"/gate/check", "/gate/redeem"} {
		method := "GET"
		if path == "/gate/redeem" {
			method = "POST"
		}
		if resp := r.do(method, path, nil, map[string]string{"X-Forwarded-Proto": "http", "X-Briard-Session": c.Value}); resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s answered a request through the door: %d", path, resp.StatusCode)
		}
	}
}

// Files' card opens it, in a new tab, from the dashboard; Home Assistant's keeps its own button.
func TestAGatedAppsCardOpensIt(t *testing.T) {
	r := gateRig(t)
	c := r.trust()
	_, body := get(t, r, "/apps", c)
	if !strings.Contains(body, `href="/open/files" target="_blank"`) {
		t.Fatalf("Files' card does not open it:\n%s", body)
	}
	if strings.Contains(body, `href="/open/home-assistant"`) {
		t.Fatal("Home Assistant's card opens through the gate")
	}
}

// THE HOME LISTS THE APPS: one card, a row per installed app with Open where it has a page and
// Details where its card now lives, and a few suggestions, Files first while it is not
// installed. The per-app cards are the Apps page's and each app's own.
func TestTheHomeListsTheAppsAndSuggestsMore(t *testing.T) {
	r := newRig(t)
	c := r.trust()
	r.ha.mu.Lock()
	for k := range r.ha.done {
		r.ha.done[k] = true
	}
	r.ha.mu.Unlock()
	cat := testCatalog()
	cat.Apps = append(cat.Apps, dashboard.CatalogApp{App: manifest.App{Name: "files", Title: "Files", Functionality: "Web file browser", Description: "Every app's files.", Icon: testIcon, Homepage: "https://example.org", Available: true}})
	r.hostLists(t, cat)
	_, home := get(t, r, "/", c)
	for _, want := range []string{"<h2>Apps running</h2>", `action="/open/home-assistant" target="_blank"`, `href="/apps/home-assistant">Details</a>`, "<h2>Suggested apps</h2>", `action="/install/files"`, `action="/install/mosquitto"`} {
		if !strings.Contains(home, want) {
			t.Errorf("the home lacks %q", want)
		}
	}
	if i, j := strings.Index(home, `action="/install/files"`), strings.Index(home, `action="/install/mosquitto"`); i < 0 || j < i {
		t.Error("Files is not the first suggestion")
	}
	for _, never := range []string{`hx-get="/f/app?app=`, `action="/install/home-assistant"`, `action="/install/paperless-ngx"`} {
		if strings.Contains(home, never) {
			t.Errorf("the home has %q: a per-app card, an installed app suggested, or one with no manifest", never)
		}
	}
	// A list with nothing moving does not poll; its fragment says so by refreshing the page.
	if strings.Contains(home, `hx-get="/f/apps-running"`) {
		t.Error("an idle list polls")
	}
	if resp := r.on("briard.local", "/f/apps-running", c); resp.Header.Get("HX-Refresh") != "true" {
		t.Errorf("the idle list's fragment = %d, HX-Refresh %q", resp.StatusCode, resp.Header.Get("HX-Refresh"))
	}
	// The Apps page keeps the cards.
	if _, apps := get(t, r, "/apps", c); !strings.Contains(apps, "Opening from here signs you in.") || strings.Contains(home, "Opening from here signs you in.") {
		t.Error("Home Assistant's card is not on the Apps page alone")
	}
}
