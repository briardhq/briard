package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"briard.io/shared/routes"
)

// fakeMealie is Mealie's API as measured on v3.30.0, with its accounts in the order they were
// made: the first one, `admin` with the shipped password and email, which Mealie calls a first
// login while that email is held; a password sign-in, whose token a password change makes
// worthless; a header sign-in with the reverse proxy's secret, by username then email, which makes
// an ordinary account for a name it does not know; and what an admin may do -- list the accounts,
// oldest first only when asked, and rename one.
type fakeMealie struct {
	mu         sync.Mutex
	users      []*fakeUser
	tokens     []string // tok-<n> is issued to tokens[n]'s id
	pwLogins   int      // password sign-ins tried, which Mealie counts toward locking an account
	made       int
	failRename bool
	beatenTo   bool // another check's password change lands just before the next one
	failChange bool // a password change fails with the password unchanged (Mealie's "Failed to update")
	secret     string
}

type fakeUser struct {
	id, username, email, fullName, password, authMethod string
	admin                                               bool
	since                                               int // tokens issued before the last password change are refused
}

func newFakeMealie() *fakeMealie {
	f := &fakeMealie{secret: "proxy-secret"}
	f.add("admin", mealieFirstEmail, "Change Me", mealieFirstPassword, "Mealie", true)
	return f
}

func (f *fakeMealie) add(username, email, fullName, password, method string, admin bool) *fakeUser {
	f.made++
	u := &fakeUser{id: fmt.Sprintf("u-%d", f.made), username: username, email: email, fullName: fullName, password: password, authMethod: method, admin: admin}
	f.users = append(f.users, u)
	return u
}

func (f *fakeMealie) find(by func(*fakeUser) string, v string) *fakeUser {
	for _, u := range f.users {
		if strings.EqualFold(by(u), v) {
			return u
		}
	}
	return nil
}

func (f *fakeMealie) user(name string) *fakeUser {
	return f.find(func(u *fakeUser) string { return u.username }, name)
}

func (f *fakeMealie) issue(u *fakeUser) string {
	f.tokens = append(f.tokens, u.id)
	return fmt.Sprintf("tok-%d", len(f.tokens)-1)
}

func (u *fakeUser) out() map[string]any {
	return map[string]any{"id": u.id, "username": u.username, "email": u.email, "fullName": u.fullName, "admin": u.admin, "authMethod": u.authMethod}
}

func (f *fakeMealie) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var me *fakeUser
	var n int
	if _, err := fmt.Sscanf(r.Header.Get("Authorization"), "Bearer tok-%d", &n); err == nil && n < len(f.tokens) {
		if u := f.find(func(u *fakeUser) string { return u.id }, f.tokens[n]); u != nil && n >= u.since {
			me = u
		}
	}
	switch {
	case r.URL.Path == "/api/app/about/startup-info":
		_ = json.NewEncoder(w).Encode(map[string]bool{"isFirstLogin": f.find(func(u *fakeUser) string { return u.email }, mealieFirstEmail) != nil, "isDemo": false})
	case r.URL.Path == "/api/auth/token" && r.Method == http.MethodPost:
		f.pwLogins++
		u := f.user(r.FormValue("username"))
		if u == nil || u.password != r.FormValue("password") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"access_token": f.issue(u)})
	case r.URL.Path == "/api/auth/reverse-proxy" && r.Method == http.MethodGet:
		name := r.Header.Get("X-Forwarded-User")
		if r.Header.Get("X-Mealie-Reverse-Proxy-Secret") != f.secret || name == "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		u := f.user(name)
		if u == nil {
			u = f.find(func(u *fakeUser) string { return u.email }, name)
		}
		if u == nil {
			u = f.add(name, name+"@reverse-proxy.local", name, "REVERSE_PROXY", "ReverseProxy", false)
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"access_token": f.issue(u)})
	case me == nil:
		w.WriteHeader(http.StatusUnauthorized)
	case r.URL.Path == "/api/users/self":
		_ = json.NewEncoder(w).Encode(me.out())
	case r.URL.Path == "/api/users/password" && r.Method == http.MethodPut:
		var b struct{ CurrentPassword, NewPassword string }
		_ = json.NewDecoder(r.Body).Decode(&b)
		if f.beatenTo {
			me.password, me.since = "set-by-the-check-before", len(f.tokens)
		}
		if f.failChange || b.CurrentPassword != me.password || len(b.NewPassword) < 8 {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		me.password, me.since = b.NewPassword, len(f.tokens)
	case !me.admin:
		w.WriteHeader(http.StatusForbidden)
	case r.URL.Path == "/api/admin/users" && r.Method == http.MethodGet:
		q := r.URL.Query()
		order := slices.Clone(f.users)
		if q.Get("orderBy") != "created_at" || q.Get("orderDirection") != "asc" {
			slices.Reverse(order) // an order nobody asked for is not the age
		}
		if q.Get("perPage") != "-1" {
			order = order[:min(len(order), 1)]
		}
		items := []map[string]any{}
		for _, u := range order {
			items = append(items, u.out())
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"items": items})
	case strings.HasPrefix(r.URL.Path, "/api/admin/users/") && r.Method == http.MethodPut:
		u := f.find(func(u *fakeUser) string { return u.id }, strings.TrimPrefix(r.URL.Path, "/api/admin/users/"))
		if u == nil || f.failRename {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		if b["authMethod"] != u.authMethod || b["admin"] != u.admin {
			w.WriteHeader(http.StatusBadRequest) // the rename must keep the account what it was
			return
		}
		u.username, u.email, u.fullName = b["username"].(string), b["email"].(string), b["fullName"].(string)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// makeAdmin is Mealie's make_admin tool, run in its container: an email on stdin, and that
// account made an admin.
func (f *fakeMealie) makeAdmin(_ context.Context, stdin string, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !slices.Equal(args, []string{"python", "-m", "mealie.scripts.make_admin"}) {
		return nil, fmt.Errorf("ran %v in Mealie's container", args)
	}
	u := f.find(func(u *fakeUser) string { return u.email }, strings.TrimSpace(strings.SplitN(stdin, "\n", 2)[0]))
	if u == nil {
		return []byte("no user found"), errors.New("exit status 1")
	}
	u.admin = true
	return nil, nil
}

// mealieRig is the dashboard rig with Mealie in its routing table, gated by header, answering at
// a fake of its API, with its admin tool in its container.
func mealieRig(t *testing.T) (*rig, *fakeMealie) {
	r := newRig(t)
	f := newFakeMealie()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	r.app.mealieExec = f.makeAdmin
	u, _ := url.Parse(srv.URL)
	secret := filepath.Join(r.dir, "REVERSE_PROXY_AUTH_SECRET")
	must(t, os.WriteFile(secret, []byte(f.secret), 0o400))
	raw, _ := os.ReadFile(filepath.Join(r.dir, "routes.json"))
	var tbl routes.Table
	must(t, json.Unmarshal(raw, &tbl))
	tbl.Apps = append(tbl.Apps, routes.App{
		Name: "mealie", Hosts: routes.HostNames("brave-elf", "mealie"), Address: u.Hostname(),
		Health: "http://:" + u.Port() + "/api/app/about", Routes: []routes.Route{{Listen: routes.ListenName, To: "http://:" + u.Port()}},
		Gate: secret, GateHeader: "X-Mealie-Reverse-Proxy-Secret",
	})
	raw, _ = json.Marshal(tbl)
	must(t, os.WriteFile(filepath.Join(r.dir, "routes.json"), raw, 0o644))
	return r, f
}

// check is the reverse proxy's check for Mealie, with a trusted device's session.
func check(r *rig, c *http.Cookie) (int, string) {
	return r.loopback("GET", "/gate/check?app=mealie", c.Value, nil)
}

// THE REVERSE PROXY'S CHECK MAKES MEALIE'S FIRST ACCOUNT THE USER'S AND NAMES IT: the shipped
// password no longer works, the account keeps its username and carries the record's name and
// placeholder email, and briard's own account is an admin beside it. A later check asks Mealie
// nothing it could count as a failed login; a check for another app asks Mealie nothing at all.
func TestTheCheckTakesOverMealiesFirstAccount(t *testing.T) {
	r, f := mealieRig(t)
	c := r.trust()
	if status, _ := r.loopback("GET", "/gate/check?app=files", c.Value, nil); status != http.StatusOK || len(f.users) != 1 || f.users[0].username != "admin" {
		t.Fatalf("a check for another app = %d and Mealie has %d accounts", status, len(f.users))
	}
	if status, user := check(r, c); status != http.StatusOK || user != "admin" {
		t.Fatalf("check = %d %q", status, user)
	}
	first := f.users[0]
	if first.username != "admin" || first.fullName != "Kostas" || first.email != "kostas@briard.local" || !first.admin || first.authMethod != "Mealie" {
		t.Fatalf("Mealie's first account is %+v, want the record's", *first)
	}
	if first.password == mealieFirstPassword || len(first.password) < 20 {
		t.Fatalf("the first account's password is %q, want a generated one", first.password)
	}
	if own := f.user(mealieOwnUser); own == nil || !own.admin || len(f.users) != 2 {
		t.Fatalf("briard's own account is %+v among %d", own, len(f.users))
	}
	logins := f.pwLogins
	if status, user := check(r, c); status != http.StatusOK || user != "admin" || f.pwLogins != logins || len(f.users) != 2 {
		t.Fatalf("second check = %d %q, password sign-ins %d -> %d, %d accounts", status, user, logins, f.pwLogins, len(f.users))
	}
	// A stranger's token never reaches Mealie.
	issued := len(f.tokens)
	if status, _ := r.loopback("GET", "/gate/check?app=mealie", "not-a-token", nil); status != http.StatusUnauthorized || len(f.tokens) != issued {
		t.Fatalf("a stranger's check = %d, tokens issued %d -> %d", status, issued, len(f.tokens))
	}
}

// WHATEVER MEALIE'S ACCOUNTS SAY IS WHAT THE REVERSE PROXY DOES: the user renamed inside Mealie,
// briard's own account deleted or no longer an admin -- each is followed at the next check, with
// nothing kept on briard's side to fall out of step.
func TestTheCheckFollowsMealiesAccounts(t *testing.T) {
	r, f := mealieRig(t)
	c := r.trust()
	if status, _ := check(r, c); status != http.StatusOK {
		t.Fatalf("check = %d", status)
	}
	f.users[0].username = "kchatzi"
	if status, user := check(r, c); status != http.StatusOK || user != "kchatzi" {
		t.Fatalf("after a rename in Mealie, check = %d %q", status, user)
	}
	f.users = slices.DeleteFunc(f.users, func(u *fakeUser) bool { return u.username == mealieOwnUser })
	if status, user := check(r, c); status != http.StatusOK || user != "kchatzi" {
		t.Fatalf("after briard's own account was deleted, check = %d %q", status, user)
	}
	own := f.user(mealieOwnUser)
	if own == nil || !own.admin {
		t.Fatalf("briard's own account is %+v, want it made again, an admin", own)
	}
	own.admin = false
	if status, user := check(r, c); status != http.StatusOK || user != "kchatzi" || !own.admin {
		t.Fatalf("after briard's own account lost admin, check = %d %q, admin %v", status, user, own.admin)
	}
	if len(f.users) != 2 || f.users[0].username != "kchatzi" {
		t.Fatalf("the user's account is %+v among %d; nothing but briard's own may be made or renamed", *f.users[0], len(f.users))
	}
}

// A MEALIE THE USER BROUGHT -- a backup of a Mealie they ran before, imported or restored --
// opens as its oldest admin, unrenamed: there is no first account to take over, and nothing is
// signed in to by password.
func TestABroughtMealieOpensAsItsOldestAdmin(t *testing.T) {
	r, f := mealieRig(t)
	f.users = nil
	f.add("bob", "bob@example.org", "Bob", "x", "Mealie", false)
	f.add("jane", "jane@example.org", "Jane", "y", "Mealie", true)
	f.add("ann", "ann@example.org", "Ann", "z", "Mealie", true)
	c := r.trust()
	if status, user := check(r, c); status != http.StatusOK || user != "jane" {
		t.Fatalf("check = %d %q, want the oldest admin", status, user)
	}
	if f.pwLogins != 0 || f.users[1].username != "jane" || f.users[1].email != "jane@example.org" {
		t.Fatalf("a brought Mealie was signed in to by password (%d) or renamed (%+v)", f.pwLogins, *f.users[1])
	}
}

// A Mealie back at its first account -- undone to before the takeover, reinstalled without its
// data -- is taken over again at the next check, with no open from the dashboard: a browser that
// kept its cookie goes straight to the app.
func TestAMealieBackAtItsFirstAccountIsTakenOverAgain(t *testing.T) {
	r, f := mealieRig(t)
	c := r.trust()
	if status, _ := check(r, c); status != http.StatusOK {
		t.Fatalf("check = %d", status)
	}
	fresh := newFakeMealie()
	f.users, f.made = fresh.users, 100
	if status, user := check(r, c); status != http.StatusOK || user != "admin" || f.users[0].password == mealieFirstPassword {
		t.Fatalf("check on a Mealie back at its first account = %d %q", status, user)
	}
}

// A takeover cut short after the password changed refuses that check, and the next one finishes
// it: the shipped password no longer answers, which is no hole, and the rename needs none.
func TestATakeoverCutShortIsFinishedByTheNextCheck(t *testing.T) {
	r, f := mealieRig(t)
	f.failRename = true
	c := r.trust()
	if status, _ := check(r, c); status != http.StatusServiceUnavailable {
		t.Fatalf("check with a failed rename = %d, want 503", status)
	}
	if f.users[0].password == mealieFirstPassword {
		t.Fatal("the rename was tried before the password changed")
	}
	f.failRename = false
	if status, user := check(r, c); status != http.StatusOK || user != "admin" || f.users[0].email != "kostas@briard.local" {
		t.Fatalf("the next check = %d %q", status, user)
	}
}

// A PASSWORD CHANGE BEATEN TO IT BY AN EARLIER CHECK'S -- one the reverse proxy stopped waiting
// for, which Mealie finished all the same -- is that same end: the takeover goes on to the rename.
// A change that failed with the shipped password still answering refuses the check.
func TestAPasswordChangeBeatenToItFinishesTheTakeover(t *testing.T) {
	r, f := mealieRig(t)
	f.beatenTo = true
	c := r.trust()
	if status, user := check(r, c); status != http.StatusOK || user != "admin" || f.users[0].email != "kostas@briard.local" {
		t.Fatalf("check beaten to the password change = %d %q", status, user)
	}

	r, f = mealieRig(t)
	f.failChange = true
	c = r.trust()
	if status, body := check(r, c); status != http.StatusServiceUnavailable || !strings.Contains(body, "change the password") {
		t.Fatalf("check whose password change failed = %d %q, want 503", status, body)
	}
	if f.users[0].password != mealieFirstPassword || f.users[0].email != mealieFirstEmail {
		t.Fatalf("a failed password change went on to the rename: %+v", *f.users[0])
	}
}

// A CHECK THE REVERSE PROXY STOPPED WAITING FOR RUNS TO ITS END: the reverse proxy gives up while
// Mealie's admin tool runs, and the takeover is finished all the same.
func TestACheckTheProxyStoppedWaitingForStillFinishes(t *testing.T) {
	r, f := mealieRig(t)
	r.trust()
	ctx, cancel := context.WithCancel(context.Background())
	r.app.mealieExec = func(c context.Context, stdin string, args ...string) ([]byte, error) {
		cancel()
		return f.makeAdmin(c, stdin, args...)
	}
	user, err := r.app.mealieUser(ctx, gatedApps(r.app.routesPath)["mealie"])
	if err != nil || user != "admin" || f.users[0].password == mealieFirstPassword || f.users[0].email != "kostas@briard.local" {
		t.Fatalf("a check the reverse proxy stopped waiting for = %q, %v; first account %+v", user, err, *f.users[0])
	}
}

// A Mealie with no admin but briard's own has no account of the user's to open, and says what
// to do; one whose admin tool fails says that.
func TestAMealieWithNoAccountOfTheUsersRefusesTheCheck(t *testing.T) {
	r, f := mealieRig(t)
	c := r.trust()
	if status, _ := check(r, c); status != http.StatusOK {
		t.Fatalf("check = %d", status)
	}
	f.users[0].admin = false
	if status, body := check(r, c); status != http.StatusServiceUnavailable || !strings.Contains(body, "undo Mealie") {
		t.Fatalf("check with no admin of the user's = %d %q", status, body)
	}
	f.users[0].admin = true
	f.user(mealieOwnUser).admin = false
	r.app.mealieExec = func(context.Context, string, ...string) ([]byte, error) {
		return []byte("boom"), errors.New("exit status 1")
	}
	if status, body := check(r, c); status != http.StatusServiceUnavailable || !strings.Contains(body, "boom") {
		t.Fatalf("check with a failing admin tool = %d %q", status, body)
	}
}
