package main

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"briard.io/shared/casa"
	"briard.io/shared/dashboard"
	"briard.io/shared/routes"
)

// fakeNavidrome is Navidrome as "On your phone" meets it: its database through the sqlite3 in its
// container (exec), and its API, signed in by the user header. A header sign-in at /app/ makes the
// account with a generated password, and a change without the current password is taken only from
// a generated one, as 0.64.2 does.
type fakeNavidrome struct {
	mu       sync.Mutex
	accounts []map[string]any // id, userName, isAdmin, name
	password map[string]string
	puts     []map[string]any
	readErr  error
}

func (f *fakeNavidrome) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	user := r.Header.Get("X-Forwarded-User")
	if user == "" {
		http.Error(w, "Not authenticated", http.StatusUnauthorized)
		return
	}
	acct := slices.IndexFunc(f.accounts, func(a map[string]any) bool { return strings.EqualFold(a["userName"].(string), user) })
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/app/":
		if acct < 0 {
			id := fmt.Sprintf("id-%d", len(f.accounts)+1)
			f.accounts = append(f.accounts, map[string]any{"id": id, "userName": user, "name": user, "isAdmin": len(f.accounts) == 0})
			f.password[id] = navidromeGenerated + "0ldwjrjMRAt8udZfm7Af02"
		}
		fmt.Fprint(w, "<html>Navidrome</html>")
	case acct < 0:
		http.Error(w, "Not authenticated", http.StatusUnauthorized)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/user/"):
		_ = json.NewEncoder(w).Encode(f.accounts[acct])
	case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/api/user/"):
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.puts = append(f.puts, body)
		id := strings.TrimPrefix(r.URL.Path, "/api/user/")
		if !strings.HasPrefix(f.password[id], navidromeGenerated) {
			http.Error(w, `{"errors":{"currentPassword":"ra.validation.passwordDoesNotMatch"}}`, http.StatusBadRequest)
			return
		}
		f.password[id] = body["password"].(string)
		_ = json.NewEncoder(w).Encode(body)
	default:
		http.NotFound(w, r)
	}
}

// exec answers the two commands the dashboard runs in Navidrome's container: its own `user list`
// and sqlite3's read of a row, with the password encrypted as Navidrome stores it.
func (f *fakeNavidrome) exec(_ context.Context, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch args[0] {
	case "navidrome":
		var list []map[string]any
		for i, a := range f.accounts {
			list = append(list, map[string]any{"username": a["userName"], "admin": a["isAdmin"], "createdAt": time.Date(2026, 11, 1+i, 0, 0, 0, 0, time.UTC)})
		}
		if list == nil {
			return []byte("[]\n"), nil
		}
		return json.Marshal(list)
	case "sqlite3":
		if f.readErr != nil {
			return nil, f.readErr
		}
		q := args[len(args)-1]
		for _, a := range f.accounts {
			if strings.Contains(strings.ToLower(q), "'"+strings.ToLower(a["userName"].(string))+"'") {
				return json.Marshal([]map[string]string{{"id": a["id"].(string), "password": navidromeEncrypt(f.password[a["id"].(string)])}})
			}
		}
		return nil, nil
	}
	return nil, errors.New("unexpected command")
}

// navidromeEncrypt is Navidrome's encryption, for the fake's rows.
func navidromeEncrypt(plain string) string {
	block, _ := aes.NewCipher(navidromeKey[:])
	gcm, _ := cipher.NewGCM(block)
	nonce := make([]byte, gcm.NonceSize())
	_, _ = io.ReadFull(rand.Reader, nonce)
	return base64.StdEncoding.EncodeToString(gcm.Seal(nonce, nonce, []byte(plain), nil))
}

// phoneRig is a dashboard with Navidrome installed and answered by a fake, and a trusted session.
func phoneRig(t *testing.T) (*rig, *http.Cookie, *fakeNavidrome) {
	r := newRig(t)
	f := installNavidrome(t, r)
	r.app.casaPath = filepath.Join(r.dir, "casa.json")
	return r, r.trust(), f
}

// installNavidrome routes Navidrome on the rig, answered by a fake with no account yet.
func installNavidrome(t *testing.T, r *rig) *fakeNavidrome {
	t.Helper()
	f := &fakeNavidrome{password: map[string]string{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	raw, _ := os.ReadFile(filepath.Join(r.dir, "routes.json"))
	var tbl routes.Table
	must(t, json.Unmarshal(raw, &tbl))
	tbl.Apps = append(tbl.Apps, routes.App{
		Name: "navidrome", Hosts: routes.HostNames("brave-elf", "navidrome"), Address: u.Hostname(),
		Health: "http://:" + u.Port() + "/ping", Routes: []routes.Route{{Listen: routes.ListenName, To: "http://:" + u.Port()}},
		GateByAddress: true, Open: []string{"/rest/"},
	})
	raw, _ = json.Marshal(tbl)
	must(t, os.WriteFile(filepath.Join(r.dir, "routes.json"), raw, 0o644))
	r.app.navidromeExec = f.exec
	return f
}

// press is the button: htmx's request, or the form's without a script.
func press(t *testing.T, r *rig, path string, c *http.Cookie, htmx bool) (int, string) {
	t.Helper()
	req, _ := http.NewRequest("POST", r.srv.URL+path, nil)
	if c != nil {
		req.AddCookie(c)
	}
	if htmx {
		req.Header.Set("HX-Request", "true")
	}
	resp, err := noRedirect.Do(req)
	must(t, err)
	return resp.StatusCode, bodyOf(t, resp)
}

// keys are the dialog's three values, in order.
func keys(body string) []string {
	var out []string
	for _, part := range strings.Split(body, `<p class="key">`)[1:] {
		out = append(out, part[:strings.Index(part, "</p>")])
	}
	return out
}

// A Navidrome nobody opened is given its account, the way opening it would, as the record's
// username; its generated password is replaced through Navidrome's API by three of the list's
// words, sent with the account's own record so it stays an admin; and the dialog shows what
// Navidrome then holds, at the `.local` name. The next press shows the same password and changes
// nothing.
func TestOnYourPhoneReplacesTheGeneratedPasswordOnce(t *testing.T) {
	r, c, f := phoneRig(t)
	status, body := press(t, r, "/phone/navidrome", c, true)
	if status != http.StatusOK || !strings.HasPrefix(body, `<dialog class="dialog"`) {
		t.Fatalf("press = %d %s, want the dialog", status, body)
	}
	got := keys(body)
	if len(got) != 3 || got[0] != "http://briard-navidrome.local" || got[1] != "kostas" {
		t.Fatalf("dialog = %q, want the .local address, the record's username and a password", got)
	}
	parts := strings.Split(got[2], " ")
	if len(parts) != 3 || !slices.Contains(words, parts[0]) || !slices.Contains(words, parts[1]) || !slices.Contains(words, parts[2]) {
		t.Errorf("password %q is not three of the list's words", got[2])
	}
	if f.password["id-1"] != got[2] {
		t.Errorf("Navidrome holds %q, the dialog shows %q", f.password["id-1"], got[2])
	}
	if len(f.puts) != 1 || f.puts[0]["isAdmin"] != true || f.puts[0]["userName"] != "kostas" {
		t.Errorf("the change sent %v, want the account's own record with the password", f.puts)
	}
	link := `<a href="https://www.navidrome.org/apps/" target="_blank" rel="noopener">Compatible players</a>`
	if strings.Contains(body, "<script") || !strings.Contains(body, "Navidrome on your phone</h2>") || !strings.Contains(body, link) {
		t.Errorf("the dialog is not the fields under its title, with the link to the players: %s", body)
	}
	if _, again := press(t, r, "/phone/navidrome", c, true); !slices.Equal(keys(again), got) || len(f.puts) != 1 {
		t.Errorf("a second press shows %q after %d changes, want %q after one", keys(again), len(f.puts), got)
	}
}

// A password the person set is shown as it is and never replaced, and the casa name, once it
// resolves with a certificate, is the address, by https.
func TestOnYourPhoneShowsAPasswordThePersonSet(t *testing.T) {
	r, c, f := phoneRig(t)
	f.accounts = []map[string]any{{"id": "id-1", "userName": "kc", "name": "kc", "isAdmin": true}}
	f.password["id-1"] = "my own one"
	r.hostSays(t, dashboard.Casa{State: casa.ClaimRegistered, Name: "brave-elf.briard.casa", Resolves: dashboard.CasaResolves, CertUntil: time.Date(2027, 1, 2, 0, 0, 0, 0, time.UTC)})
	_, body := press(t, r, "/phone/navidrome", c, true)
	if got := keys(body); !slices.Equal(got, []string{"https://navidrome.brave-elf.briard.casa", "kc", "my own one"}) {
		t.Errorf("dialog = %q", got)
	}
	if len(f.puts) != 0 {
		t.Errorf("a password the person set was changed: %v", f.puts)
	}
	// A name the house cannot resolve opens nothing: the .local name, until it does.
	r.hostSays(t, dashboard.Casa{State: casa.ClaimRegistered, Name: "brave-elf.briard.casa", Resolves: dashboard.CasaBlocked, CertUntil: time.Date(2027, 1, 2, 0, 0, 0, 0, time.UTC)})
	if _, body := press(t, r, "/phone/navidrome", c, true); keys(body)[0] != "http://briard-navidrome.local" {
		t.Errorf("a blocked name is the address: %q", keys(body))
	}
}

// Without a script the form's POST is a page with the same fields; a password that cannot be read
// is said, with no fields; and the button is a trusted device's POST, for an app that has one.
func TestOnYourPhoneIsAPageWithoutAScriptAndRefusesTheRest(t *testing.T) {
	r, c, f := phoneRig(t)
	f.accounts = []map[string]any{{"id": "id-1", "userName": "kc", "name": "kc", "isAdmin": true}}
	f.password["id-1"] = "my own one"
	if status, body := press(t, r, "/phone/navidrome", c, false); status != http.StatusOK || !strings.Contains(body, "<h1>Navidrome on your phone</h1>") || len(keys(body)) != 3 || !strings.Contains(body, `href="https://www.navidrome.org/apps/"`) || strings.Contains(body, "<dialog") {
		t.Errorf("the page = %d %s", status, body)
	}
	f.readErr = errors.New("exit status 1: no such table: user")
	if _, body := press(t, r, "/phone/navidrome", c, true); len(keys(body)) != 0 || !strings.Contains(body, "no such table: user") {
		t.Errorf("an unreadable password = %s", body)
	}
	if status, body := press(t, r, "/phone/navidrome", nil, true); status == http.StatusOK && strings.Contains(body, "dialog") {
		t.Error("an untrusted browser was answered")
	}
	if resp, _ := get(t, r, "/phone/navidrome", c); resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET = %d, want 404: a page load never changes the password", resp.StatusCode)
	}
	for _, app := range []string{"files", "nothing"} {
		if status, _ := press(t, r, "/phone/"+app, c, true); status != http.StatusNotFound {
			t.Errorf("%s = %d, want 404", app, status)
		}
	}
}

// What Navidrome 0.64.2 itself stored for the password "test-pass-one", set through its API: the
// dashboard reads it with Navidrome's default key.
func TestNavidromesOwnCiphertextDecrypts(t *testing.T) {
	if got, err := navidromeDecrypt("W8VLsN0V5fY5soWlXYhK4XjLdp5lmI349wuLgG31XZIJCgMRsonqd6k="); err != nil || got != "test-pass-one" {
		t.Errorf("decrypt = %q, %v", got, err)
	}
	if _, err := navidromeDecrypt(navidromeEncrypt("x")[:10]); err == nil {
		t.Error("a truncated row decrypted")
	}
}

// The list is the EFF's whole short list, each word distinct.
func TestThePassphraseListIsTheEFFShortList(t *testing.T) {
	seen := map[string]bool{}
	for _, w := range words {
		seen[w] = true
	}
	if len(words) != 1296 || len(seen) != 1296 || words[0] != "acid" || words[1295] != "zoom" {
		t.Errorf("%d words, %d distinct, first %q, last %q", len(words), len(seen), words[0], words[len(words)-1])
	}
}

// The browser's half: on the Apps page the phone's button opens the dialog over the page, by htmx
// and the dialog line under the page's own policy, rather than leaving for the page without a
// script; and a click outside it, on the backdrop, closes it. The one thing added is the
// clicking, by its hash. Skipped unless DASHBOARD_BROWSER names a chromium (a test tool's
// switch, like the gallery's).
func TestOnYourPhoneOpensADialogInABrowser(t *testing.T) {
	chromium := os.Getenv("DASHBOARD_BROWSER")
	if chromium == "" {
		t.Skip("set DASHBOARD_BROWSER=<chromium> to prove the dialog in a browser")
	}
	// After htmx has read the page (it does at DOMContentLoaded, after the body's own scripts).
	press := `document.querySelector('form[action="/phone/navidrome"] button').click()`
	// Once the dialog is on the page and open (the page's own line runs first): a click on it, not
	// on what it holds, is a click on the backdrop.
	outside := `document.addEventListener('htmx:load',function(e){var d=e.detail.elt;if(d.tagName==='DIALOG')setTimeout(function(){document.body.setAttribute('data-clicked',d.open);d.dispatchEvent(new MouseEvent('click',{bubbles:true}))},100)});`
	dom := dialogInBrowser(t, chromium, `addEventListener('DOMContentLoaded',function(){setTimeout(function(){`+press+`},100)})`, nil)
	if !strings.Contains(dom, `<h2 class="group-title">Installed</h2>`) || !strings.Contains(dom, `<dialog class="dialog" aria-labelledby="phone-title" open="">`) || len(keys(dom)) != 3 {
		t.Errorf("the Apps page has no open dialog with the three fields: %s", dom)
	}
	dom = dialogInBrowser(t, chromium, `addEventListener('DOMContentLoaded',function(){setTimeout(function(){`+press+`},100)});`+outside, nil)
	// Closed, not yet gone: headless chromium on a virtual clock never fires a dialog's close
	// event, which takes it off the page, so the dump still holds it, shut.
	if !strings.Contains(dom, `<h2 class="group-title">Installed</h2>`) || !strings.Contains(dom, `data-clicked="true"`) || strings.Contains(dom, `open=""`) {
		t.Errorf("a click outside the dialog left it open: %s", dom)
	}
}

// dialogInBrowser is the Apps page, with Navidrome installed, as a browser leaves it once script
// has run in it, under the page's own policy and script's hash; host, when given, plays the host
// behind the admin port.
func dialogInBrowser(t *testing.T, chromium, script string, host func(*rig, *fakePort)) string {
	t.Helper()
	r, _, port := galleryRig(t)
	everyday(t, r)
	installNavidrome(t, r)
	if host != nil {
		host(r, port)
	}
	// The browser redeems its own code: a fresh handoff, since the rig's cookie spent the first.
	raw, _ := json.Marshal(dashboard.Handoff{Code: "browser-code", Issued: r.app.now()})
	must(t, os.WriteFile(r.app.handoffPath, raw, 0o600))
	sum := sha256.Sum256([]byte(script))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/go" {
			// The code is redeemed at the home page; the browser is sent on to the Apps page.
			rec := httptest.NewRecorder()
			r.app.ServeHTTP(rec, httptest.NewRequest("GET", "/?code=browser-code", nil))
			w.Header()["Set-Cookie"] = rec.Header()["Set-Cookie"]
			http.Redirect(w, req, "/apps", http.StatusSeeOther)
			return
		}
		if req.URL.Path != "/apps" {
			r.app.ServeHTTP(w, req)
			return
		}
		rec := httptest.NewRecorder()
		r.app.ServeHTTP(rec, req)
		for k, v := range rec.Header() {
			w.Header()[k] = v
		}
		w.Header().Del("Content-Length")
		w.Header().Set("Content-Security-Policy", csp+" 'sha256-"+base64.StdEncoding.EncodeToString(sum[:])+"'")
		w.WriteHeader(rec.Code)
		_, _ = io.WriteString(w, strings.Replace(rec.Body.String(), "</body>", "<script>"+script+"</script></body>", 1))
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, chromium, "--headless=new", "--disable-gpu", "--no-first-run", "--no-default-browser-check",
		"--user-data-dir="+t.TempDir(), "--window-size=1200,900", "--virtual-time-budget=5000", "--dump-dom", srv.URL+"/go").Output()
	must(t, err)
	return string(out)
}

// haPhoneRig is a dashboard whose Home Assistant was set up from it: the first open made the
// owner with four words and handed them to the integration to keep; then the rest of onboarding.
func haPhoneRig(t *testing.T) (*rig, *http.Cookie) {
	r := newRig(t)
	c := r.trust()
	if resp := r.do("POST", "/open/home-assistant", c, nil); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("first open = %d", resp.StatusCode)
	}
	r.ha.mu.Lock()
	for k := range r.ha.done {
		r.ha.done[k] = true
	}
	r.ha.mu.Unlock()
	return r, c
}

// fourWords says whether a password is four of the list's words.
func fourWords(p string) bool {
	parts := strings.Split(p, " ")
	return len(parts) == 4 && !slices.ContainsFunc(parts, func(w string) bool { return !slices.Contains(words, w) })
}

// The words the first open set are what the dialog shows, while they still sign in: the owner's
// username, the `.local` address and the link to the companion apps; the card has the button once
// Home Assistant has its owner, and opening the dialog changes nothing.
func TestOnYourPhoneShowsHomeAssistantsKeptWords(t *testing.T) {
	r := newRig(t)
	c := r.trust()
	if _, apps := get(t, r, "/apps", c); strings.Contains(apps, `action="/phone/home-assistant"`) {
		t.Error("the card offers the phone before Home Assistant has an owner")
	}
	r, c = haPhoneRig(t)
	if _, apps := get(t, r, "/apps", c); !strings.Contains(apps, `action="/phone/home-assistant"`) {
		t.Error("a set-up Home Assistant's card has no On your phone")
	}
	status, body := press(t, r, "/phone/home-assistant", c, true)
	r.ha.mu.Lock()
	first, sets := r.ha.user["password"], r.ha.phoneSets
	r.ha.mu.Unlock()
	if got := keys(body); status != http.StatusOK || !slices.Equal(got, []string{"http://briard-home-assistant.local", "kostas", first}) || !fourWords(first) {
		t.Fatalf("dialog = %d %q, want the .local address, the owner and the first open's four words %q", status, got, first)
	}
	if !strings.Contains(body, `<a href="https://companion.home-assistant.io/" target="_blank" rel="noopener">Companion apps</a>`) || strings.Contains(body, "New password") {
		t.Errorf("the dialog is not the kept words with the link to the companion apps: %s", body)
	}
	if _, again := press(t, r, "/phone/home-assistant", c, true); !slices.Equal(keys(again), keys(body)) || r.ha.phoneSets != sets {
		t.Errorf("a second open shows %q after %d sets, want the same after %d", keys(again), r.ha.phoneSets, sets)
	}
}

// A password changed in Home Assistant, or none ever kept (a node set up before this), is a
// button; the button sets four new words, the dialog shows them in place, and the next open
// shows the same ones.
func TestOnYourPhoneGivesHomeAssistantANewPassword(t *testing.T) {
	r, c := haPhoneRig(t)
	r.ha.mu.Lock()
	r.ha.ownerPassword = "changed in Home Assistant"
	r.ha.mu.Unlock()
	_, body := press(t, r, "/phone/home-assistant", c, true)
	button := `<form method="post" action="/phone/home-assistant?new" hx-post="/phone/home-assistant?new" hx-target="closest dialog" hx-swap="outerHTML"><button type="submit" class="btn" data-variant="outline">New password</button></form>`
	if got := keys(body); !slices.Equal(got, []string{"http://briard-home-assistant.local", "kostas"}) || !strings.Contains(body, button) {
		t.Fatalf("a changed password = %q %s, want the button in the password's place", got, body)
	}
	if r.ha.ownerPassword != "changed in Home Assistant" {
		t.Error("opening the dialog changed the password")
	}
	_, body = press(t, r, "/phone/home-assistant?new", c, true)
	got := keys(body)
	if len(got) != 3 || !fourWords(got[2]) || got[2] != r.ha.ownerPassword || !strings.HasPrefix(body, `<dialog class="dialog"`) {
		t.Fatalf("after New password = %q (HA holds %q): %s", got, r.ha.ownerPassword, body)
	}
	if _, again := press(t, r, "/phone/home-assistant", c, true); !slices.Equal(keys(again), got) {
		t.Errorf("the next open shows %q, want %q", keys(again), got)
	}
	// Without a script the button's POST is the page again, with the new words.
	r.ha.mu.Lock()
	r.ha.kept = ""
	r.ha.mu.Unlock()
	if status, page := press(t, r, "/phone/home-assistant?new", c, false); status != http.StatusOK || !strings.Contains(page, "<h1>Home Assistant on your phone</h1>") || len(keys(page)) != 3 || !fourWords(keys(page)[2]) {
		t.Errorf("the page after New password = %d %s", status, page)
	}
}

// What the integration refuses is said, with no fields: no owner, and a Home Assistant still
// running an integration from before the view (it loads the new one when it next starts).
func TestOnYourPhoneSaysWhyHomeAssistantsSignInCannotBeRead(t *testing.T) {
	r, c := haPhoneRig(t)
	r.ha.mu.Lock()
	r.ha.noOwner = true
	r.ha.mu.Unlock()
	if _, body := press(t, r, "/phone/home-assistant", c, true); len(keys(body)) != 0 || !strings.Contains(body, "Could not read the sign-in from Home Assistant: it has no owner account") {
		t.Errorf("no owner = %s", body)
	}
	r.ha.mu.Lock()
	r.ha.noOwner, r.ha.noMinter = false, true
	r.ha.mu.Unlock()
	if _, body := press(t, r, "/phone/home-assistant", c, true); len(keys(body)) != 0 || !strings.Contains(body, "not running Briard&#39;s integration") {
		t.Errorf("an integration without the view = %s", body)
	}
}

// The browser's half of New password: the button's htmx POST puts the dialog with the new words in
// place of the open one, open over the page, and leaves no second dialog behind. Skipped unless
// DASHBOARD_BROWSER names a chromium, as above.
func TestHomeAssistantsNewPasswordReplacesTheDialogInABrowser(t *testing.T) {
	chromium := os.Getenv("DASHBOARD_BROWSER")
	if chromium == "" {
		t.Skip("set DASHBOARD_BROWSER=<chromium> to prove the dialog in a browser")
	}
	press := `document.querySelector('form[action="/phone/home-assistant"] button').click()`
	renew := `document.addEventListener('htmx:load',function(e){var b=e.detail.elt.tagName==='DIALOG'&&e.detail.elt.querySelector('form[action$="?new"] button');if(b)setTimeout(function(){b.click()},100)});`
	var ha *fakeHA
	dom := dialogInBrowser(t, chromium, `addEventListener('DOMContentLoaded',function(){setTimeout(function(){`+press+`},100)});`+renew, func(r *rig, _ *fakePort) {
		ha = r.ha
		r.ha.mu.Lock()
		r.ha.user, r.ha.ownerPassword = map[string]string{"username": "kostas"}, "changed in Home Assistant"
		r.ha.mu.Unlock()
	})
	got := keys(dom)
	if strings.Count(dom, "<dialog") != 1 || !strings.Contains(dom, `<h2 id="phone-title">Home Assistant on your phone</h2>`) || !strings.Contains(dom, `open=""`) || len(got) != 3 || !fourWords(got[2]) {
		t.Fatalf("after New password the page holds %d dialogs, fields %q: %s", strings.Count(dom, "<dialog"), got, dom)
	}
	ha.mu.Lock()
	defer ha.mu.Unlock()
	if ha.ownerPassword != got[2] || ha.phoneSets != 1 {
		t.Errorf("Home Assistant holds %q after %d sets; the dialog shows %q", ha.ownerPassword, ha.phoneSets, got[2])
	}
}
