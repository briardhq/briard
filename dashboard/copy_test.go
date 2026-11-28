package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"html"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"briard.io/agent/files"
	"briard.io/shared/casa"
	"briard.io/shared/dashboard"
	"briard.io/shared/routes"
	"rsc.io/qr"
)

var qrImage = regexp.MustCompile(`<img class="qr" src="data:image/png;base64,([^"]+)"`)

// pressForCopy presses "On your phone" for FreshRSS and returns the dialog and the one token it
// minted.
func pressForCopy(t *testing.T, r *rig, c *http.Cookie) (string, string) {
	t.Helper()
	_, body := press(t, r, "/phone/freshrss", c, true)
	r.app.copyMu.Lock()
	defer r.app.copyMu.Unlock()
	if len(r.app.copyPages) != 1 {
		t.Fatalf("%d live copy pages after one press, want 1", len(r.app.copyPages))
	}
	for tok := range r.app.copyPages {
		return body, tok
	}
	return body, ""
}

// open is a phone's GET of a copy page: no cookie, no session.
func open(t *testing.T, r *rig, tok string) (*http.Response, string) {
	t.Helper()
	resp, err := noRedirect.Get(r.srv.URL + "/copy/" + tok)
	must(t, err)
	return resp, bodyOf(t, resp)
}

// The dialog's QR code is the copy page's address, at the alias while the household has no
// certified casa name; the page opens without a session and shows the dialog's three values,
// each with a Copy button, under the one title, with the link to the readers -- and nothing
// about the home. It is never cached and sends no referrer, and its script is one the policy
// allows.
func TestOnYourPhonesQRCodeOpensACopyPage(t *testing.T) {
	r := newRig(t)
	installFreshRSS(t, r)
	body, tok := pressForCopy(t, r, r.trust())
	m := qrImage.FindStringSubmatch(body)
	if m == nil || !strings.Contains(body, `<div class="scan"><p>Scan with your phone`) {
		t.Fatalf("the dialog has no QR code with its caption: %s", body)
	}
	want, err := qr.Encode("http://briard.local/copy/"+tok, qr.M)
	must(t, err)
	if got, _ := base64.StdEncoding.DecodeString(html.UnescapeString(m[1])); !bytes.Equal(got, want.PNG()) {
		t.Error("the QR code is not the copy page's address")
	}
	if raw, _ := base64.RawURLEncoding.DecodeString(tok); len(raw) != 24 {
		t.Errorf("token %q is not 24 random bytes", tok)
	}

	resp, page := open(t, r, tok)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the copy page answered %d: %s", resp.StatusCode, page)
	}
	if !strings.Contains(page, "<h1>"+r.app.appTitle("freshrss")+" on your phone</h1>") || !strings.Contains(page, "<h3>Install a reader</h3>") || !strings.Contains(page, "<h3>Copy these settings into it</h3>") {
		t.Errorf("the page lacks its title or its two steps: %s", page)
	}
	dialog := keys(body)
	if got := keys(page); len(got) != 3 || got[0] != dialog[0] || got[1] != dialog[1] || got[2] != dialog[2] {
		t.Errorf("the page shows %q, the dialog %q", got, dialog)
	}
	for _, id := range []string{"address", "username", "password"} {
		if !strings.Contains(page, `id="copy-`+id+`"`) || !strings.Contains(page, `data-copy="copy-`+id+`"`) {
			t.Errorf("%s has no Copy button", id)
		}
	}
	if !strings.Contains(page, `<a href="https://capyreader.com/" target="_blank" rel="noopener noreferrer">Capy Reader for Android</a>`) || !strings.Contains(page, `>Other readers</a>`) {
		t.Error("the page lacks the recommended reader and the others")
	}
	for _, leak := range []string{"Trusted devices", "Backup", "hx-"} {
		if strings.Contains(page, leak) {
			t.Errorf("the page says %q, something about the home", leak)
		}
	}
	if resp.Header.Get("Cache-Control") != "no-store" || resp.Header.Get("Referrer-Policy") != "no-referrer" {
		t.Errorf("headers: Cache-Control %q, Referrer-Policy %q", resp.Header.Get("Cache-Control"), resp.Header.Get("Referrer-Policy"))
	}
	sum := sha256.Sum256([]byte(copyScript))
	if !strings.Contains(page, "<script>"+copyScript+"</script>") || !strings.Contains(resp.Header.Get("Content-Security-Policy"), base64.StdEncoding.EncodeToString(sum[:])) {
		t.Error("the Copy script is not on the page under the policy's hash")
	}
	// A preview's fetch does not spend it: a second open shows the same page.
	if resp, _ := open(t, r, tok); resp.StatusCode != http.StatusOK {
		t.Errorf("a second open answered %d", resp.StatusCode)
	}
}

// One page per app: a second press replaces the first token. A token past copyFor, and one that
// never was, are the expired page, which shows nothing.
func TestACopyPageEndsWithTheNextPressOrItsTime(t *testing.T) {
	r := newRig(t)
	installFreshRSS(t, r)
	c := r.trust()
	_, first := pressForCopy(t, r, c)
	_, second := pressForCopy(t, r, c)
	if first == second {
		t.Fatal("the second press reused the token")
	}
	resp, page := open(t, r, first)
	if resp.StatusCode != http.StatusNotFound || !strings.Contains(page, "This code has expired") || len(keys(page)) != 0 {
		t.Errorf("the replaced token answered %d: %s", resp.StatusCode, page)
	}
	if resp, _ := open(t, r, second); resp.StatusCode != http.StatusOK {
		t.Errorf("the live token answered %d", resp.StatusCode)
	}
	now := time.Now()
	r.app.now = func() time.Time { return now.Add(copyFor + time.Second) }
	if resp, page := open(t, r, second); resp.StatusCode != http.StatusNotFound || len(keys(page)) != 0 {
		t.Errorf("a token past its time answered %d", resp.StatusCode)
	}
	if resp, _ := open(t, r, "never-was"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("an unknown token answered %d", resp.StatusCode)
	}
}

// The QR code is on every dialog with fields: one whose password is the New password button
// still has it, and its page shows the address and username alone. A dialog that could not read
// the sign-in has none. Once the casa name is certified, the page is on it by https.
func TestTheQRCodeIsOnEveryDialogWithFields(t *testing.T) {
	r := newRig(t)
	f := installFreshRSS(t, r)
	c := r.trust()
	r.app.casaPath = filepath.Join(r.dir, "casa.json")
	f.apiPassword, f.kept = "their own", ""
	body, tok := pressForCopy(t, r, c)
	if !qrImage.MatchString(body) {
		t.Fatalf("the New password dialog has no QR code: %s", body)
	}
	if _, page := open(t, r, tok); len(keys(page)) != 2 || strings.Contains(page, `data-copy="copy-password"`) {
		t.Errorf("the page of a dialog with no password shows %q", keys(page))
	}

	r.hostSays(t, dashboard.Casa{State: casa.ClaimRegistered, Name: "brave-elf.briard.casa", Resolves: dashboard.CasaResolves, CertUntil: time.Date(2027, 1, 2, 0, 0, 0, 0, time.UTC)})
	body, tok = pressForCopy(t, r, c)
	m := qrImage.FindStringSubmatch(body)
	want, err := qr.Encode("https://brave-elf.briard.casa/copy/"+tok, qr.M)
	must(t, err)
	if got, _ := base64.StdEncoding.DecodeString(html.UnescapeString(m[1])); !bytes.Equal(got, want.PNG()) {
		t.Error("with a certified casa name the QR code is not its https address")
	}

	f.fail = errors.New("exit status 1")
	_, body = press(t, r, "/phone/freshrss", c, true)
	if qrImage.MatchString(body) {
		t.Error("a dialog with no fields has a QR code")
	}
}

// galleryCasa gives the rig a registered, resolving, certified casa name, the household the
// gallery's FreshRSS pages show.
func galleryCasa(t *testing.T, r *rig) {
	t.Helper()
	r.app.casaPath = filepath.Join(r.dir, "casa.json")
	r.hostSays(t, dashboard.Casa{State: casa.ClaimRegistered, Name: "brave-elf.briard.casa", Resolves: dashboard.CasaResolves, CertUntil: time.Date(2027, 1, 2, 0, 0, 0, 0, time.UTC)})
}

// The dialog carries its turn button, which only the dialog's script reads; after New password it
// opens on its settings, whose new words the person came for. The copy page carries the seconds
// its token has left and the expired text it clears into, and no New password button, which
// posts with a session the phone does not have.
func TestTheDialogTurnsAndThePageCountsDown(t *testing.T) {
	r := newRig(t)
	f := installFreshRSS(t, r)
	c := r.trust()
	body, tok := pressForCopy(t, r, c)
	if !strings.Contains(body, `data-turn>Show settings</button>`) || strings.Contains(body, `data-start=`) {
		t.Errorf("the dialog's turn button, or a start it should not have: %s", body)
	}
	_, page := open(t, r, tok)
	if m := regexp.MustCompile(`<div data-left="(\d+)">`).FindStringSubmatch(page); m == nil || m[1] < "1190" || m[1] > "1200" || !strings.Contains(page, `<template id="gone"><header class="page-head"><h1>This code has expired</h1>`) {
		t.Errorf("the page lacks its twenty minutes or its expired text: %s", page)
	}
	f.apiPassword, f.kept = "their own", ""
	_, body = press(t, r, "/phone/freshrss", c, true)
	_, body = press(t, r, "/phone/freshrss?new", c, true)
	if !strings.Contains(body, `data-start="settings"`) {
		t.Errorf("after New password the dialog does not open on its settings: %s", body)
	}
	f.apiPassword, f.kept = "their own", ""
	_, tok = pressForCopy(t, r, c)
	if _, page := open(t, r, tok); strings.Contains(page, "New password") || strings.Contains(page, "/phone/") {
		t.Errorf("the page offers New password, which needs a session: %s", page)
	}
}

// In a browser the page clears itself when its token runs out, into the expired text. Skipped
// unless DASHBOARD_BROWSER names a chromium, as the dialog's own browser test is.
func TestTheCopyPageClearsItselfInABrowser(t *testing.T) {
	chromium := os.Getenv("DASHBOARD_BROWSER")
	if chromium == "" {
		t.Skip("set DASHBOARD_BROWSER=<chromium> to prove the page's countdown in a browser")
	}
	r := newRig(t)
	installFreshRSS(t, r)
	_, tok := pressForCopy(t, r, r.trust())
	at := time.Now().Add(copyFor - 2*time.Second)
	r.app.now = func() time.Time { return at }
	dom, err := exec.Command(chromium, "--headless=new", "--no-sandbox", "--user-data-dir="+t.TempDir(), "--virtual-time-budget=5000", "--dump-dom", r.srv.URL+"/copy/"+tok).Output()
	must(t, err)
	if !strings.Contains(string(dom), "This code has expired") || len(keys(string(dom))) != 0 {
		t.Errorf("the page did not clear itself: %s", dom)
	}
}

// pressForWebApp presses "On your phone" for Files, a web app, and returns the dialog and its
// copy page's token.
func pressForWebApp(t *testing.T, r *rig, c *http.Cookie) (string, string) {
	t.Helper()
	_, body := press(t, r, "/phone/files", c, true)
	r.app.copyMu.Lock()
	defer r.app.copyMu.Unlock()
	for tok, p := range r.app.copyPages {
		if p.app == "files" {
			return body, tok
		}
	}
	t.Fatalf("no copy page after the press: %s", body)
	return "", ""
}

// A web app's steps are Open and the line that adds it to the home screen, with nothing to copy.
// In the dialog Open is the app's own (the browser is a trusted device); on the copy page it is a
// POST that spends the page on a session of Files alone, carried to Files' name by the gate's
// one-time code. That session opens Files, and not Mealie, the dashboard or the page again; it is
// listed in Trusted devices as Files' alone, under a key an older dashboard never compares, and
// revoked there like a device.
func TestAWebAppsPageOpensItWithASessionOfItsOwn(t *testing.T) {
	r := gateRig(t)
	galleryCasa(t, r)
	c := r.trust()
	body, tok := pressForWebApp(t, r, c)
	if !qrImage.MatchString(body) || !strings.Contains(body, `<a class="btn" href="/open/files" target="_blank" rel="noopener">Open</a>`) || !strings.Contains(body, "<h3>Install on your home screen</h3>") || len(keys(body)) != 0 {
		t.Fatalf("the dialog lacks its QR code or its two steps, or has fields: %s", body)
	}
	_, page := open(t, r, tok)
	if !strings.Contains(page, `<form method="post" action="/copy/`+tok+`"><button type="submit" class="btn">Open</button></form>`) || strings.Contains(page, "/open/files") || !strings.Contains(page, "Android: ⋮ › Install and create shortcut › Install") || !strings.Contains(page, "iPhone: Share › Add to Home Screen") {
		t.Fatalf("the page's Open is not its own POST: %s", page)
	}

	resp, err := noRedirect.Post(r.srv.URL+"/copy/"+tok, "application/x-www-form-urlencoded", nil)
	must(t, err)
	resp.Body.Close()
	loc, _ := url.Parse(resp.Header.Get("Location"))
	if resp.StatusCode != http.StatusSeeOther || loc.Scheme != "https" || loc.Host != "files.brave-elf.briard.casa" || loc.Path != "/" {
		t.Fatalf("Open = %d %q, want https://files.brave-elf.briard.casa/", resp.StatusCode, loc)
	}
	status, session := r.loopback("POST", "/gate/redeem", "", url.Values{"code": {loc.Query().Get(gateHopParam)}})
	if status != http.StatusOK || session == "" || session == c.Value {
		t.Fatalf("redeem = %d %q, want a session of its own", status, session)
	}
	if status, user := r.loopback("GET", "/gate/check?app=files", session, nil); status != http.StatusOK || user != files.User {
		t.Errorf("Files' session at Files = %d %q", status, user)
	}
	if status, _ := r.loopback("GET", "/gate/check?app=mealie", session, nil); status != http.StatusUnauthorized {
		t.Errorf("Files' session at Mealie = %d, want 401", status)
	}
	if resp := r.on(routes.BareFlockHostName, "/", &http.Cookie{Name: cookieName, Value: session}); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("Files' session opened the dashboard: %d", resp.StatusCode)
	}
	if resp, _ := open(t, r, tok); resp.StatusCode != http.StatusNotFound {
		t.Errorf("the page after its Open answered %d, want the expired page", resp.StatusCode)
	}
	if resp, err := noRedirect.Post(r.srv.URL+"/copy/"+tok, "application/x-www-form-urlencoded", nil); err != nil || resp.StatusCode != http.StatusNotFound {
		t.Errorf("a second Open: %v %v", resp.StatusCode, err)
	}

	raw, err := os.ReadFile(filepath.Join(r.dir, "state", devicesFile))
	must(t, err)
	var rows []map[string]any
	var reg struct{ Devices *[]map[string]any }
	reg.Devices = &rows
	must(t, json.Unmarshal(raw, &reg))
	var id string
	for _, d := range rows {
		if d["app"] == "files" {
			id, _ = d["id"].(string)
			if _, has := d["hash"]; has || d["appHash"] == "" {
				t.Errorf("Files' session's row has a whole device's key: %v", d)
			}
		}
	}
	if id == "" {
		t.Fatalf("no row for Files' session: %s", raw)
	}
	if devices := r.get(t, "/devices", c); !strings.Contains(devices, `<span class="badge" data-variant="outline">`+r.app.appTitle("files")+` only</span>`) {
		t.Errorf("Trusted devices does not say the session is Files' alone: %s", devices)
	}
	if resp := r.form("/devices/revoke", c, url.Values{"id": {id}}); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("revoke = %d", resp.StatusCode)
	}
	if status, _ := r.loopback("GET", "/gate/check?app=files", session, nil); status != http.StatusUnauthorized {
		t.Errorf("a revoked session still opens Files: %d", status)
	}
}

// By the `.local` name a web app's "On your phone" opens it as by the casa name, over http, and in
// place of the install lines says that a phone installs it only by https, linking the dialog to
// the casa name's page (the copy page has no session to follow it with). And a page with settings
// to copy has no Open to spend it on.
func TestAWebAppOpensByTheLocalNameAndOnlyItsPageOpens(t *testing.T) {
	r := gateRig(t)
	installFreshRSS(t, r)
	c := r.trust()
	if card := r.get(t, "/apps/files", c); !strings.Contains(card, `action="/phone/files"`) {
		t.Errorf("Files does not offer On your phone by the .local name: %s", card)
	}
	body, tok := pressForWebApp(t, r, c)
	if !strings.Contains(body, `<p>Not supported without encryption. <a href="/secure">Get a .casa address</a> to enable it.</p>`) || strings.Contains(body, "Android:") {
		t.Errorf("the dialog by .local offers the install or does not say why not: %s", body)
	}
	_, page := open(t, r, tok)
	if !strings.Contains(page, "<p>Not supported without encryption. Get a .casa address to enable it.</p>") || strings.Contains(page, "Android:") {
		t.Errorf("the page by .local offers the install or does not say why not: %s", page)
	}
	resp, err := noRedirect.Post(r.srv.URL+"/copy/"+tok, "application/x-www-form-urlencoded", nil)
	must(t, err)
	resp.Body.Close()
	loc, _ := url.Parse(resp.Header.Get("Location"))
	if resp.StatusCode != http.StatusSeeOther || loc.Scheme != "http" || loc.Host != routes.BareHostName("files") || loc.Path != "/" {
		t.Fatalf("Open by .local = %d %q, want http://%s/", resp.StatusCode, loc, routes.BareHostName("files"))
	}
	status, session := r.loopback("POST", "/gate/redeem", "", url.Values{"code": {loc.Query().Get(gateHopParam)}})
	if status != http.StatusOK || session == "" {
		t.Fatalf("redeem = %d %q", status, session)
	}
	if status, _ := r.loopback("GET", "/gate/check?app=files", session, nil); status != http.StatusOK {
		t.Errorf("Files' session by .local at Files = %d", status)
	}

	galleryCasa(t, r)
	_, tok = pressForCopy(t, r, c)
	devices := len(r.registry().Devices)
	resp, err = noRedirect.Post(r.srv.URL+"/copy/"+tok, "application/x-www-form-urlencoded", nil)
	must(t, err)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound || len(r.registry().Devices) != devices {
		t.Errorf("FreshRSS's page opened something: %d, %d devices", resp.StatusCode, len(r.registry().Devices))
	}
	if resp, _ := open(t, r, tok); resp.StatusCode != http.StatusOK {
		t.Errorf("FreshRSS's page after a refused Open answered %d", resp.StatusCode)
	}
}
