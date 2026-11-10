package main

import (
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

	"briard.io/shared/routes"
)

// fakeMealie is Mealie's API as measured on v3.30.0: one first account, `admin` with the shipped
// password and email, that says it is a first login while that email is held; a token for the
// right password; and an admin's rename and a password change.
type fakeMealie struct {
	mu                        sync.Mutex
	username, email, fullName string
	password                  string
	logins                    int
	failRename                bool
}

func newFakeMealie() *fakeMealie {
	return &fakeMealie{username: "admin", email: "changeme@example.com", fullName: "Change Me", password: mealieFirstPassword}
}

func (f *fakeMealie) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	authed := r.Header.Get("Authorization") == "Bearer tok"
	switch {
	case r.URL.Path == "/api/app/about/startup-info":
		_ = json.NewEncoder(w).Encode(map[string]bool{"isFirstLogin": f.email == "changeme@example.com", "isDemo": false})
	case r.URL.Path == "/api/auth/token" && r.Method == http.MethodPost:
		f.logins++
		if r.FormValue("username") != f.username || r.FormValue("password") != f.password {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "tok"})
	case r.URL.Path == "/api/users/self" && authed:
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "u-1", "username": f.username, "email": f.email, "fullName": f.fullName, "admin": true, "authMethod": "Mealie"})
	case r.URL.Path == "/api/users/password" && r.Method == http.MethodPut && authed:
		var b struct{ CurrentPassword, NewPassword string }
		_ = json.NewDecoder(r.Body).Decode(&b)
		if b.CurrentPassword != f.password || len(b.NewPassword) < 8 {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		f.password = b.NewPassword
	case r.URL.Path == "/api/admin/users/u-1" && r.Method == http.MethodPut && authed:
		if f.failRename {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		if b["authMethod"] != "Mealie" || b["admin"] != true {
			w.WriteHeader(http.StatusBadRequest) // the rename must keep the account what it was
			return
		}
		f.username, f.email, f.fullName = b["username"].(string), b["email"].(string), b["fullName"].(string)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// mealieRig is the dashboard rig with Mealie in its routing table, gated by header, answering at
// a fake of its API.
func mealieRig(t *testing.T) (*rig, *fakeMealie) {
	r := newRig(t)
	f := newFakeMealie()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	raw, _ := os.ReadFile(filepath.Join(r.dir, "routes.json"))
	var tbl routes.Table
	must(t, json.Unmarshal(raw, &tbl))
	tbl.Apps = append(tbl.Apps, routes.App{
		Name: "mealie", Hosts: routes.HostNames("brave-elf", "mealie"), Address: u.Hostname(),
		Health: "http://:" + u.Port() + "/api/app/about", Routes: []routes.Route{{Listen: routes.ListenName, To: "http://:" + u.Port()}},
		Gate: "/run/briard/mealie/settings/REVERSE_PROXY_AUTH_SECRET", GateHeader: "X-Mealie-Reverse-Proxy-Secret",
	})
	raw, _ = json.Marshal(tbl)
	must(t, os.WriteFile(filepath.Join(r.dir, "routes.json"), raw, 0o644))
	return r, f
}

// THE FIRST OPEN MAKES MEALIE'S FIRST ACCOUNT THE USER'S: the shipped password no longer works,
// the account carries the record's username, name and placeholder email, and the browser goes on
// to Mealie's name. A later open asks Mealie nothing it could count as a failed login.
func TestTheFirstOpenTakesOverMealiesFirstAccount(t *testing.T) {
	r, f := mealieRig(t)
	c := r.trust()
	resp := r.on(routes.BareFlockHostName, "/open/mealie", c)
	loc, _ := url.Parse(resp.Header.Get("Location"))
	if resp.StatusCode != http.StatusSeeOther || loc.Host != "briard-mealie.local" || loc.Query().Get(gateHopParam) == "" {
		t.Fatalf("open = %d %q", resp.StatusCode, loc)
	}
	if f.username != "kostas" || f.fullName != "Kostas" || f.email != "kostas@briard.local" {
		t.Fatalf("Mealie's first account is %q %q %q, want the record's", f.username, f.fullName, f.email)
	}
	if f.password == mealieFirstPassword || len(f.password) < 20 {
		t.Fatalf("the first account's password is %q, want a generated one", f.password)
	}
	logins := f.logins
	if resp := r.on(routes.BareFlockHostName, "/open/mealie", c); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("second open = %d", resp.StatusCode)
	}
	if f.logins != logins {
		t.Fatal("a second open tried a login, which Mealie counts toward locking the account")
	}
}

// A takeover that stopped after the password changed leaves an account nobody can sign in to: the
// next open says so and lets nobody through, never quietly carrying on.
func TestATakeoverCutShortRefusesTheOpen(t *testing.T) {
	r, f := mealieRig(t)
	f.failRename = true
	c := r.trust()
	if resp := r.on(routes.BareFlockHostName, "/open/mealie", c); resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("open with a failed rename = %d, want 503", resp.StatusCode)
	}
	if f.password == mealieFirstPassword {
		t.Fatal("the rename was tried before the password changed")
	}
	f.failRename = false
	req, _ := http.NewRequest("GET", r.srv.URL+"/open/mealie", nil)
	req.Host = routes.BareFlockHostName
	req.AddCookie(c)
	resp, err := noRedirect.Do(req)
	must(t, err)
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	body := string(raw)
	if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(body, "reinstall Mealie") {
		t.Fatalf("the next open = %d %q, want a refusal that says what to do", resp.StatusCode, body)
	}
}
