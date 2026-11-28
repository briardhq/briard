package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"briard.io/agent/navidrome"
	"briard.io/shared/routes"
)

// navidromeRig is a dashboard with Navidrome installed, whose `user list` prints list (or fails
// with err), and a trusted device's session.
func navidromeRig(t *testing.T) (*rig, *http.Cookie, *string, *error, *[][]string) {
	r := newRig(t)
	raw, _ := os.ReadFile(filepath.Join(r.dir, "routes.json"))
	var tbl routes.Table
	must(t, json.Unmarshal(raw, &tbl))
	tbl.Apps = append(tbl.Apps, routes.App{
		Name: "navidrome", Hosts: routes.HostNames("brave-elf", "navidrome"), Address: "10.12.7.3",
		Health: "http://:4533/ping", Routes: []routes.Route{{Listen: routes.ListenName, To: "http://:4533"}},
		GateByAddress: true, Open: []string{"/rest/"},
	})
	raw, _ = json.Marshal(tbl)
	must(t, os.WriteFile(filepath.Join(r.dir, "routes.json"), raw, 0o644))
	list, fail, calls := new(string), new(error), new([][]string)
	r.app.appExec[navidrome.Name] = func(_ context.Context, _ string, args ...string) ([]byte, error) {
		*calls = append(*calls, args)
		if *fail != nil {
			return nil, *fail
		}
		return []byte(*list), nil
	}
	return r, r.trust(), list, fail, calls
}

// The reverse proxy's check for Navidrome names the account Navidrome's own data says is the
// user's, read by Navidrome's own tool: on a Navidrome with no account yet, the record's username,
// which the first sign-in makes its admin; then its oldest admin, whatever it is called and
// wherever it stands in the list, never an older account that is not an admin.
func TestTheCheckNamesNavidromesOldestAdmin(t *testing.T) {
	r, c, list, _, calls := navidromeRig(t)
	*list = "[]\n"
	if status, user := r.loopback("GET", "/gate/check?app=navidrome", c.Value, nil); status != http.StatusOK || user != "kostas" {
		t.Fatalf("check on a Navidrome with no account = %d %q, want 200 kostas, the record's", status, user)
	}
	if want := []string{"navidrome", "--nobanner", "user", "list", "--format", "json"}; len(*calls) != 1 || !slices.Equal((*calls)[0], want) {
		t.Fatalf("ran %q, want %q", *calls, want)
	}
	*list = `[{"username":"guest","admin":false,"createdAt":"2026-11-01T10:00:00Z"},
		{"username":"later","admin":true,"createdAt":"2026-11-20T10:00:00Z"},
		{"username":"kc","admin":true,"createdAt":"2026-11-02T10:00:00Z"}]`
	if status, user := r.loopback("GET", "/gate/check?app=navidrome", c.Value, nil); status != http.StatusOK || user != "kc" {
		t.Fatalf("check = %d %q, want 200 kc, the oldest admin", status, user)
	}
	// Files' check asks Navidrome nothing.
	if status, user := r.loopback("GET", "/gate/check?app=files", c.Value, nil); status != http.StatusOK || user != "kostas" || len(*calls) != 2 {
		t.Fatalf("Files' check = %d %q after %d reads of Navidrome", status, user, len(*calls))
	}
}

// A Navidrome whose accounts cannot be read is never taken for one with none, which the record's
// name would then be given a second account in; one with accounts but no admin has no account of
// the user's to open, and says what to do.
func TestANavidromeWhoseAccountsCannotBeReadRefusesTheCheck(t *testing.T) {
	r, c, list, fail, _ := navidromeRig(t)
	*fail = errors.New("exit status 1: no such table: user")
	if status, body := r.loopback("GET", "/gate/check?app=navidrome", c.Value, nil); status != http.StatusServiceUnavailable || !strings.Contains(body, "no such table") {
		t.Fatalf("check with a failing tool = %d %q, want 503 saying why", status, body)
	}
	*fail = nil
	*list = "Navidrome is starting"
	if status, _ := r.loopback("GET", "/gate/check?app=navidrome", c.Value, nil); status != http.StatusServiceUnavailable {
		t.Fatalf("check with a list it cannot read = %d, want 503", status)
	}
	*list = `[{"username":"guest","admin":false,"createdAt":"2026-11-01T10:00:00Z"}]`
	if status, body := r.loopback("GET", "/gate/check?app=navidrome", c.Value, nil); status != http.StatusServiceUnavailable || !strings.Contains(body, "undo Navidrome") {
		t.Fatalf("check with no admin = %d %q, want 503 naming undo", status, body)
	}
}
