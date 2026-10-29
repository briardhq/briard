package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"briard.io/shared/api"
	"briard.io/shared/casa"
	"briard.io/shared/dashboard"
)

// get is one GET with its body, which the rig's do does not keep.
func get(t *testing.T, r *rig, path string, c *http.Cookie) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest("GET", r.srv.URL+path, nil)
	if c != nil {
		req.AddCookie(c)
	}
	resp, err := noRedirect.Do(req)
	must(t, err)
	return resp, bodyOf(t, resp)
}

// A moving part polls itself and nothing else does: the card carries the poll only while its
// part moves, the fragment answers the same card, and once the part has stopped moving the
// fragment says refresh -- the page's shape may have changed around it.
func TestFragmentsPollOnlyWhileMovingThenRefreshThePage(t *testing.T) {
	r, c, port := galleryRig(t)

	// An install in flight: the card polls, the fragment is the card; done, the fragment refreshes.
	notInstalled(t, r)
	startInstall(t, r, c, port)
	if body := r.page(c); !strings.Contains(body, `hx-get="/f/install?app=home-assistant" hx-trigger="every 5s"`) || strings.Contains(body, `http-equiv="refresh"`) {
		t.Errorf("the installing card does not poll itself, or the page still reloads itself: %s", body)
	}
	if resp, body := get(t, r, "/f/install?app=home-assistant", c); resp.StatusCode != 200 || !strings.Contains(body, "Installing") || strings.Contains(body, "<html") || resp.Header.Get("HX-Refresh") != "" {
		t.Errorf("the install fragment while moving = %d %q: %s", resp.StatusCode, resp.Header.Get("HX-Refresh"), body)
	}
	port.answer <- api.DirectiveOutcome{State: api.OutcomeFailed, Detail: "no room"}
	deadline := time.Now().Add(5 * time.Second)
	for strings.Contains(r.page(c), `hx-get="/f/install`) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if resp, _ := get(t, r, "/f/install?app=home-assistant", c); resp.StatusCode != http.StatusNoContent || resp.Header.Get("HX-Refresh") != "true" {
		t.Errorf("the install fragment once done = %d %q, want 204 + HX-Refresh", resp.StatusCode, resp.Header.Get("HX-Refresh"))
	}

	// Home Assistant still starting: its card polls; RUNNING, the fragment refreshes.
	r2, c2, port2 := galleryRig(t)
	r2.ha.mu.Lock()
	r2.ha.state = "STARTING"
	r2.ha.mu.Unlock()
	if body := r2.page(c2); !strings.Contains(body, `hx-get="/f/app?app=home-assistant"`) {
		t.Errorf("a starting Home Assistant's card does not poll: %s", body)
	}
	if resp, body := get(t, r2, "/f/app?app=home-assistant", c2); resp.StatusCode != 200 || !strings.Contains(body, "Open Home Assistant") {
		t.Errorf("the app fragment while starting = %d: %s", resp.StatusCode, body)
	}
	r2.ha.mu.Lock()
	r2.ha.state = "RUNNING"
	r2.ha.mu.Unlock()
	if body := r2.page(c2); strings.Contains(body, `hx-get="/f/app`) {
		t.Errorf("a RUNNING Home Assistant's card still polls: %s", body)
	}
	if resp, _ := get(t, r2, "/f/app?app=home-assistant", c2); resp.Header.Get("HX-Refresh") != "true" {
		t.Errorf("the app fragment once RUNNING = %d, no HX-Refresh", resp.StatusCode)
	}

	// A claim waiting on its link polls the step; the link opened, the step is over and its
	// fragment refreshes the page (never the offer again, over a registered name); a registered
	// name waiting on its check polls the card; resolved, neither.
	r2.hostSays(t, dashboard.Casa{State: casa.ClaimPending, Name: galleryName, Email: "ana@example.org"})
	if resp, body := get(t, r2, "/f/address-step?home", c2); resp.StatusCode != 200 || !strings.Contains(body, "Check your email") || !strings.Contains(body, `hx-get="/f/address-step?home"`) {
		t.Errorf("the address-step fragment while pending = %d: %s", resp.StatusCode, body)
	}
	r2.hostSays(t, dashboard.Casa{State: casa.ClaimRegistered, Name: galleryName, Email: "ana@example.org"})
	if resp, body := get(t, r2, "/f/address-step?home", c2); resp.StatusCode != http.StatusNoContent || resp.Header.Get("HX-Refresh") != "true" {
		t.Errorf("the address-step fragment once registered = %d %q, want 204 + HX-Refresh: %s", resp.StatusCode, resp.Header.Get("HX-Refresh"), body)
	}
	if resp, body := get(t, r2, "/f/address-card", c2); resp.StatusCode != 200 || !strings.Contains(body, "Checking that your home network") {
		t.Errorf("the address-card fragment while checking = %d: %s", resp.StatusCode, body)
	}
	r2.hostSays(t, dashboard.Casa{State: casa.ClaimRegistered, Name: galleryName, Email: "ana@example.org", Resolves: dashboard.CasaResolves})
	if resp, _ := get(t, r2, "/f/address-card", c2); resp.Header.Get("HX-Refresh") != "true" {
		t.Error("the address-card fragment once resolving does not refresh")
	}
	if body := r2.page(c2); strings.Contains(body, "hx-get=") {
		t.Errorf("a home with nothing moving still polls somewhere: %s", body)
	}

	// An undo in flight: the history card polls; done, the fragment refreshes.
	answerHistory(port2, galleryRing(false))
	r2.app.mu.Lock()
	r2.app.restores["home-assistant"] = &restoreOp{Started: galleryClock, What: "Updated to 2026.8.0"}
	r2.app.mu.Unlock()
	if _, body := get(t, r2, "/apps/home-assistant", c2); !strings.Contains(body, `hx-get="/f/history?app=home-assistant"`) || !strings.Contains(body, "Undoing") {
		t.Errorf("the history card does not poll while an undo runs: %s", body)
	}
	if resp, body := get(t, r2, "/f/history?app=home-assistant", c2); resp.StatusCode != 200 || !strings.Contains(body, "Undoing") || strings.Contains(body, "<html") {
		t.Errorf("the history fragment while undoing = %d: %s", resp.StatusCode, body)
	}
	r2.app.mu.Lock()
	r2.app.restores["home-assistant"].Done = true
	r2.app.mu.Unlock()
	if resp, _ := get(t, r2, "/f/history?app=home-assistant", c2); resp.Header.Get("HX-Refresh") != "true" {
		t.Errorf("the history fragment once the undo is done = %d, no HX-Refresh", resp.StatusCode)
	}

	// Not a part, not an app, not trusted.
	if resp := r2.do("GET", "/f/weather", c2, nil); resp.StatusCode != 404 {
		t.Errorf("an unknown part = %d", resp.StatusCode)
	}
	if resp := r2.do("GET", "/f/install?app=immich", c2, nil); resp.StatusCode != 404 {
		t.Errorf("an app not offered = %d", resp.StatusCode)
	}
	if resp := r2.do("GET", "/f/address-step", nil, nil); resp.StatusCode != 401 {
		t.Errorf("a fragment with no session = %d", resp.StatusCode)
	}
}

// The script budget: htmx from this origin on trusted pages, the one inline theme line by its
// hash, and a policy on every answer that allows nothing else.
func TestThePageCarriesOneScriptAndAPolicyThatAllowsNothingElse(t *testing.T) {
	r, c, _ := galleryRig(t)
	for _, path := range []string{"/", "/apps", "/f/address-step", htmxPath, "/app.css"} {
		if got := r.do("GET", path, c, nil).Header.Get("Content-Security-Policy"); got != csp || !strings.HasPrefix(got, "script-src 'self' 'sha256-") {
			t.Errorf("%s: Content-Security-Policy = %q", path, got)
		}
	}
	if body := r.page(c); strings.Count(body, "<script") != 2*6 || !strings.Contains(body, `<script src="`+htmxPath+`"></script>`) || !strings.Contains(body, "<script>"+themeScript+"</script>") {
		t.Errorf("a trusted page's scripts are not htmx and the theme line: %s", body)
	}
	if _, body := get(t, r, "/", nil); strings.Contains(body, htmxPath) || !strings.Contains(body, "<script>"+themeScript+"</script>") {
		t.Errorf("an untrusted page loads htmx, or lost its theme line: %s", body)
	}
	if resp, body := get(t, r, htmxPath, nil); resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/javascript") || !strings.Contains(body, "htmx") {
		t.Errorf("htmx is not served to every browser: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	// The tab's icon, named by every page and served to every browser.
	if _, body := get(t, r, "/", nil); !strings.Contains(body, `<link rel="icon" type="image/png" href="/favicon.png">`) {
		t.Errorf("the page names no icon: %s", body)
	}
	if resp, body := get(t, r, "/favicon.png", nil); resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "image/png" || !strings.HasPrefix(body, "\x89PNG") {
		t.Errorf("the icon is not served to every browser: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
}

// The browser's half, which no string test can see: with the page open in a real browser, a
// pending claim's card polls, and when the host registers the name the fragment's refresh
// reloads the page, which then shows the name and no longer the step. Skipped unless
// DASHBOARD_BROWSER names a chromium (a test tool's switch, like the gallery's); the lab's
// gallery script runs it.
func TestTheCardPollsAndThePageReloadsInABrowser(t *testing.T) {
	chromium := os.Getenv("DASHBOARD_BROWSER")
	if chromium == "" {
		t.Skip("set DASHBOARD_BROWSER=<chromium> to prove the poll in a browser")
	}
	r, _, _ := galleryRig(t)
	// The browser redeems its own code: a fresh handoff, since the rig's cookie spent the first.
	raw, _ := json.Marshal(dashboard.Handoff{Code: "browser-code", Issued: r.app.now()})
	must(t, os.WriteFile(r.app.handoffPath, raw, 0o600))
	r.hostSays(t, dashboard.Casa{State: casa.ClaimPending, Name: galleryName, Email: "ana@example.org"})

	// The host registers the name the moment the page first polls its step, so that poll is
	// answered with a refresh and the reloaded page is what the browser dumps. Chromium runs the
	// page on a virtual clock, so the five-second poll fires at once and the dump follows it.
	var once sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/f/address-step" {
			once.Do(func() {
				r.hostSays(t, dashboard.Casa{State: casa.ClaimRegistered, Name: galleryName, Email: "ana@example.org",
					CertUntil: galleryClock.Add(80 * 24 * time.Hour), Resolves: dashboard.CasaResolves})
			})
		}
		r.app.ServeHTTP(w, req)
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, chromium, "--headless=new", "--disable-gpu", "--no-first-run", "--no-default-browser-check",
		"--user-data-dir="+t.TempDir(), "--window-size=1200,900", "--virtual-time-budget=20000", "--dump-dom", srv.URL+"/?code=browser-code").Output()
	must(t, err)
	dom := string(out)
	if strings.Contains(dom, "Check your email") || !strings.Contains(dom, "certificate until") {
		t.Errorf("after the host registered the name the browser still shows the step, or not the name: %s", dom)
	}
}
