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
	if status, _ := r.loopback("GET", "/gate/check", c.Value, nil); status != http.StatusNoContent {
		t.Fatalf("check on a trusted device's token = %d, want 204", status)
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
