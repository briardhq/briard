package main

import (
	"encoding/json"
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

// THE DOOR'S CHECK MAKES MEALIE'S FIRST ACCOUNT THE USER'S: the shipped password no longer works,
// and the account carries the record's username, name and placeholder email before the answer is
// yes. A later check asks Mealie nothing it could count as a failed login; a check for another app
// asks Mealie nothing at all.
func TestTheCheckTakesOverMealiesFirstAccount(t *testing.T) {
	r, f := mealieRig(t)
	c := r.trust()
	if status, _ := r.loopback("GET", "/gate/check?app=files", c.Value, nil); status != http.StatusOK || f.username != "admin" {
		t.Fatalf("a check for another app = %d and Mealie's first account is %q", status, f.username)
	}
	if status, user := r.loopback("GET", "/gate/check?app=mealie", c.Value, nil); status != http.StatusOK || user != "kostas" {
		t.Fatalf("check = %d %q", status, user)
	}
	if f.username != "kostas" || f.fullName != "Kostas" || f.email != "kostas@briard.local" {
		t.Fatalf("Mealie's first account is %q %q %q, want the record's", f.username, f.fullName, f.email)
	}
	if f.password == mealieFirstPassword || len(f.password) < 20 {
		t.Fatalf("the first account's password is %q, want a generated one", f.password)
	}
	logins := f.logins
	if status, _ := r.loopback("GET", "/gate/check?app=mealie", c.Value, nil); status != http.StatusOK {
		t.Fatalf("second check = %d", status)
	}
	if f.logins != logins {
		t.Fatal("a second check tried a login, which Mealie counts toward locking the account")
	}
	// A stranger's token never reaches Mealie.
	if status, _ := r.loopback("GET", "/gate/check?app=mealie", "not-a-token", nil); status != http.StatusUnauthorized || f.logins != logins {
		t.Fatalf("a stranger's check = %d, logins %d -> %d", status, logins, f.logins)
	}
}

// A Mealie back at its first account -- undone to before the takeover, reinstalled without its
// data -- is taken over again at the next check, with no open from the dashboard: a browser that
// kept its cookie goes straight to the app.
func TestAMealieBackAtItsFirstAccountIsTakenOverAgain(t *testing.T) {
	r, f := mealieRig(t)
	c := r.trust()
	if status, _ := r.loopback("GET", "/gate/check?app=mealie", c.Value, nil); status != http.StatusOK {
		t.Fatalf("check = %d", status)
	}
	f.mu.Lock()
	f.username, f.email, f.fullName, f.password = "admin", "changeme@example.com", "Change Me", mealieFirstPassword
	f.mu.Unlock()
	if status, _ := r.loopback("GET", "/gate/check?app=mealie", c.Value, nil); status != http.StatusOK || f.username != "kostas" || f.password == mealieFirstPassword {
		t.Fatalf("check on a Mealie back at its first account = %d, account %q", status, f.username)
	}
}

// A takeover that stopped after the password changed leaves an account nobody can sign in to: the
// next check says so and lets nobody through, never quietly carrying on.
func TestATakeoverCutShortRefusesTheCheck(t *testing.T) {
	r, f := mealieRig(t)
	f.failRename = true
	c := r.trust()
	if status, _ := r.loopback("GET", "/gate/check?app=mealie", c.Value, nil); status != http.StatusServiceUnavailable {
		t.Fatalf("check with a failed rename = %d, want 503", status)
	}
	if f.password == mealieFirstPassword {
		t.Fatal("the rename was tried before the password changed")
	}
	f.failRename = false
	if status, body := r.loopback("GET", "/gate/check?app=mealie", c.Value, nil); status != http.StatusServiceUnavailable || !strings.Contains(body, "reinstall Mealie") {
		t.Fatalf("the next check = %d %q, want a refusal that says what to do", status, body)
	}
}
