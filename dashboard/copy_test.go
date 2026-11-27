package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"html"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"briard.io/shared/casa"
	"briard.io/shared/dashboard"
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
	if m == nil || !strings.Contains(body, "Scan to copy and paste on your phone") {
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
	if !strings.Contains(page, "<h1>Copy these settings into your "+r.app.appTitle("freshrss")+" client</h1>") {
		t.Errorf("the page lacks its title: %s", page)
	}
	dialog := keys(body)
	if got := pageKeys(page); len(got) != 3 || got[0] != dialog[0] || got[1] != dialog[1] || got[2] != dialog[2] {
		t.Errorf("the page shows %q, the dialog %q", got, dialog)
	}
	for _, id := range []string{"address", "username", "password"} {
		if !strings.Contains(page, `id="`+id+`"`) || !strings.Contains(page, `data-copy="`+id+`"`) {
			t.Errorf("%s has no Copy button", id)
		}
	}
	if !strings.Contains(page, `href="https://github.com/FreshRSS/FreshRSS#apis--native-apps"`) {
		t.Error("the page lacks the link to the readers")
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
	if resp.StatusCode != http.StatusNotFound || !strings.Contains(page, "This code has expired") || len(pageKeys(page)) != 0 {
		t.Errorf("the replaced token answered %d: %s", resp.StatusCode, page)
	}
	if resp, _ := open(t, r, second); resp.StatusCode != http.StatusOK {
		t.Errorf("the live token answered %d", resp.StatusCode)
	}
	now := time.Now()
	r.app.now = func() time.Time { return now.Add(copyFor + time.Second) }
	if resp, page := open(t, r, second); resp.StatusCode != http.StatusNotFound || len(pageKeys(page)) != 0 {
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
	if _, page := open(t, r, tok); len(pageKeys(page)) != 2 || strings.Contains(page, `data-copy="password"`) {
		t.Errorf("the page of a dialog with no password shows %q", pageKeys(page))
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

var pageKey = regexp.MustCompile(`<p class="key"[^>]*>([^<]*)</p>`)

// pageKeys are the copy page's values, in order.
func pageKeys(page string) []string {
	var out []string
	for _, m := range pageKey.FindAllStringSubmatch(page, -1) {
		out = append(out, html.UnescapeString(m[1]))
	}
	return out
}

// galleryCasa gives the rig a registered, resolving, certified casa name, the household the
// gallery's FreshRSS pages show.
func galleryCasa(t *testing.T, r *rig) {
	t.Helper()
	r.app.casaPath = filepath.Join(r.dir, "casa.json")
	r.hostSays(t, dashboard.Casa{State: casa.ClaimRegistered, Name: "brave-elf.briard.casa", Resolves: dashboard.CasaResolves, CertUntil: time.Date(2027, 1, 2, 0, 0, 0, 0, time.UTC)})
}
