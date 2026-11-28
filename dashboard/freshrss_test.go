package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"briard.io/agent/freshrss"
	"briard.io/shared/routes"
)

// fakeFreshRSS is FreshRSS's container as the dashboard reaches it: its tools by exec, and the
// Google Reader API's sign-in over HTTP. users is what `user-info --json` prints; apiPassword is
// the API password FreshRSS would check ("" none), kept the file briard keeps beside the user.
type fakeFreshRSS struct {
	users       string
	fail        error
	apiPassword string
	kept        string
	setTo       []string // every password update-user was given
	lag         int      // sign-ins after a change that still check the old password, as opcache makes them
	logins      int
	stale       int
}

func (f *fakeFreshRSS) exec(_ context.Context, stdin string, args ...string) ([]byte, error) {
	if f.fail != nil {
		return nil, f.fail
	}
	switch {
	case len(args) >= 1 && strings.HasSuffix(args[0], "/cli/user-info.php"):
		return []byte(f.users), nil
	case len(args) >= 1 && strings.HasSuffix(args[0], "/cli/update-user.php"):
		if i := slices.Index(args, "--api-password"); i > 0 {
			f.apiPassword = args[i+1]
			f.setTo = append(f.setTo, args[i+1])
			f.stale = f.lag
		}
		return nil, nil
	case len(args) >= 2 && args[0] == "php":
		if f.apiPassword == "" {
			return []byte("none"), nil
		}
		return []byte("set"), nil
	case len(args) >= 3 && args[0] == "sh" && strings.Contains(args[2], "cat >"):
		if !strings.HasSuffix(args[len(args)-1], "/users/owner/briard-phone") {
			return nil, errors.New("kept somewhere else: " + args[len(args)-1])
		}
		f.kept = stdin
		return nil, nil
	case len(args) >= 3 && args[0] == "sh":
		return []byte(f.kept), nil
	}
	return nil, errors.New("unexpected command " + strings.Join(args, " "))
}

func (f *fakeFreshRSS) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/api/greader.php/accounts/ClientLogin" || r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	f.logins++
	if f.stale > 0 {
		f.stale--
		http.Error(w, "Unauthorized!", http.StatusUnauthorized)
		return
	}
	if r.FormValue("Email") == "owner" && f.apiPassword != "" && r.FormValue("Passwd") == f.apiPassword {
		w.Write([]byte("SID=owner/x\nAuth=owner/x\n"))
		return
	}
	http.Error(w, "Unauthorized!", http.StatusUnauthorized)
}

// installFreshRSS routes FreshRSS on the rig, answered by a fake whose installer made `owner`.
func installFreshRSS(t *testing.T, r *rig) *fakeFreshRSS {
	t.Helper()
	f := &fakeFreshRSS{users: `[{"default":true,"user":"owner","admin":false,"enabled":true}]`}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	raw, _ := os.ReadFile(filepath.Join(r.dir, "routes.json"))
	var tbl routes.Table
	must(t, json.Unmarshal(raw, &tbl))
	tbl.Apps = append(tbl.Apps, routes.App{
		Name: "freshrss", Hosts: routes.HostNames("brave-elf", "freshrss"), Address: u.Hostname(),
		Health: "http://:" + u.Port() + "/api/", Routes: []routes.Route{{Listen: routes.ListenName, To: "http://:" + u.Port()}},
		GateByAddress: true, Open: []string{"/api/greader.php/"},
	})
	raw, _ = json.Marshal(tbl)
	must(t, os.WriteFile(filepath.Join(r.dir, "routes.json"), raw, 0o644))
	r.app.appExec[freshrss.Name] = f.exec
	return f
}

// The reverse proxy's check for FreshRSS names its default user, whatever it is called and
// wherever it stands in the list, never the record's username; a list with no default user, or a
// tool that fails, is a 503 and never a name.
func TestTheCheckNamesFreshRSSsDefaultUser(t *testing.T) {
	r := newRig(t)
	f := installFreshRSS(t, r)
	c := r.trust()
	if status, user := r.loopback("GET", "/gate/check?app=freshrss", c.Value, nil); status != http.StatusOK || user != "owner" {
		t.Fatalf("check = %d %q, want 200 owner", status, user)
	}
	f.users = `[{"default":false,"user":"guest"},{"default":true,"user":"reader"}]`
	if status, user := r.loopback("GET", "/gate/check?app=freshrss", c.Value, nil); status != http.StatusOK || user != "reader" {
		t.Fatalf("check = %d %q, want 200 reader, the default", status, user)
	}
	for _, users := range []string{`[{"default":false,"user":"guest"}]`, `[{"default":true,"user":"../x"}]`, "not json"} {
		f.users = users
		if status, _ := r.loopback("GET", "/gate/check?app=freshrss", c.Value, nil); status != http.StatusServiceUnavailable {
			t.Errorf("check on %s = %d, want 503", users, status)
		}
	}
	f.users, f.fail = `[{"default":true,"user":"owner"}]`, errors.New("exit status 1")
	if status, _ := r.loopback("GET", "/gate/check?app=freshrss", c.Value, nil); status != http.StatusServiceUnavailable {
		t.Errorf("check with the tool failing = %d, want 503", status)
	}
}

// A user with no API password is given three of the list's words at the first press, kept beside
// the user and checked by FreshRSS's own sign-in; the dialog gives the Google Reader API's address
// and the link to FreshRSS's readers, once its sign-in takes them, which right after the change
// it may not yet. The next press shows the same words and sets nothing.
func TestOnYourPhoneGivesFreshRSSThreeKeptWords(t *testing.T) {
	r := newRig(t)
	f := installFreshRSS(t, r)
	// FreshRSS still checks the old password for its first sign-ins after a change.
	f.lag = 3
	c := r.trust()
	_, body := press(t, r, "/phone/freshrss", c, true)
	got := keys(body)
	if len(got) != 3 || got[0] != "http://briard-freshrss.local/api/greader.php" || got[1] != "owner" {
		t.Fatalf("dialog = %q, want the API's .local address, owner and a password", got)
	}
	parts := strings.Split(got[2], " ")
	if len(parts) != 3 || !slices.Contains(words, parts[0]) || !slices.Contains(words, parts[1]) || !slices.Contains(words, parts[2]) {
		t.Errorf("password %q is not three of the list's words", got[2])
	}
	if f.apiPassword != got[2] || f.kept != got[2] {
		t.Errorf("FreshRSS checks %q and %q is kept; the dialog shows %q", f.apiPassword, f.kept, got[2])
	}
	if !strings.Contains(body, `<a href="https://netnewswire.com/" target="_blank" rel="noopener noreferrer">NetNewsWire for iPhone</a>`) || !strings.Contains(body, `<a href="https://github.com/FreshRSS/FreshRSS#apis--native-apps" target="_blank" rel="noopener noreferrer">Other readers</a>`) {
		t.Errorf("the dialog lacks the recommended readers and the others: %s", body)
	}
	if _, again := press(t, r, "/phone/freshrss", c, true); !slices.Equal(keys(again), got) || len(f.setTo) != 1 {
		t.Errorf("a second press shows %q after %d changes, want %q after one", keys(again), len(f.setTo), got)
	}
}

// Kept words that no longer sign in (the household changed the password in FreshRSS), and a
// password the household set with nothing kept, are the button and are never replaced by opening
// the dialog; the button sets three new words, kept and shown.
func TestOnYourPhoneOffersFreshRSSANewPasswordWhenTheKeptOneFails(t *testing.T) {
	r := newRig(t)
	f := installFreshRSS(t, r)
	c := r.trust()
	for _, kept := range []string{"old kept words", ""} {
		f.apiPassword, f.kept, f.setTo = "their own", kept, nil
		_, body := press(t, r, "/phone/freshrss", c, true)
		if got := keys(body); len(got) != 2 || !strings.Contains(body, `hx-post="/phone/freshrss?new"`) || len(f.setTo) != 0 {
			t.Errorf("kept %q: dialog %q after %d changes, want the address, the username and the button", kept, got, len(f.setTo))
		}
	}
	_, body := press(t, r, "/phone/freshrss?new", c, true)
	got := keys(body)
	if len(got) != 3 || len(f.setTo) != 1 || f.setTo[0] != got[2] || f.kept != got[2] {
		t.Errorf("New password: dialog %q, set %q, kept %q", got, f.setTo, f.kept)
	}
}

// Words FreshRSS's sign-in never takes are said, not shown: a password nobody can sign in with is
// worse than none.
func TestOnYourPhoneSaysWhenFreshRSSNeverTakesTheWords(t *testing.T) {
	r := newRig(t)
	f := installFreshRSS(t, r)
	f.lag = 1 << 30
	settles := freshrss.Settles
	freshrss.Settles = 300 * time.Millisecond
	t.Cleanup(func() { freshrss.Settles = settles })
	_, body := press(t, r, "/phone/freshrss", r.trust(), true)
	if got := keys(body); len(got) != 0 || !strings.Contains(body, "FreshRSS did not take the new password") {
		t.Errorf("dialog %q: %s", got, body)
	}
}
