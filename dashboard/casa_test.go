package main

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"briard.io/shared/api"
	"briard.io/shared/casa"
	"briard.io/shared/dashboard"
)

// hostSays is the host pushing its view into the guest (dashboard.casa): the file the page reads.
func (r *rig) hostSays(t *testing.T, v dashboard.Casa) {
	t.Helper()
	raw, _ := json.Marshal(v)
	must(t, os.WriteFile(r.app.casaPath, raw, 0o644))
}

func (r *rig) form(path string, c *http.Cookie, values url.Values) *http.Response {
	return postForm(r.t, r, path, c, values)
}

// The card from first open to a registered name: offered first, the claim relayed to the host as
// one casa-claim with the email, the page polling while the host has not yet said pending, the
// host's own states rendered as they arrive, and nothing polled once the name is the household's.
func TestCasaClaimIsOfferedRelayedAndFollowed(t *testing.T) {
	r := newRig(t)
	r.app.casaPath = filepath.Join(r.dir, "casa.json")
	c := r.trust()
	port := newFakePort()
	r.app.port = port
	r.hostSays(t, dashboard.Casa{Name: "brave-elf.briard.casa"})

	body := r.page(c)
	if !strings.Contains(body, `action="/casa/claim"`) || !strings.Contains(body, "brave-elf.briard.casa") || !strings.Contains(body, ">Not now<") || strings.Contains(body, `http-equiv="refresh"`) {
		t.Fatalf("the first page does not offer the name, or polls: %s", body)
	}
	if resp := r.form("/casa/claim", nil, url.Values{"email": {"a@x.org"}}); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("claim with no session = %d, want 401", resp.StatusCode)
	}
	if resp := r.form("/casa/claim", c, url.Values{"email": {""}}); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("claim with no email = %d, want 400", resp.StatusCode)
	}

	port.answer <- api.DirectiveOutcome{State: api.OutcomeDone, Detail: "a confirmation link was sent to a@x.org; open it within 20 minutes"}
	if resp := r.form("/casa/claim", c, url.Values{"email": {" a@x.org "}}); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("claim = %d, want 303", resp.StatusCode)
	}
	port.mu.Lock()
	asked := append([]api.Directive(nil), port.asked...)
	port.mu.Unlock()
	if len(asked) != 1 || asked[0].Kind != api.DirectiveCasaClaim || asked[0].Payload != "a@x.org" {
		t.Fatalf("the host was asked %+v; want one casa-claim carrying the email", asked)
	}
	body = r.page(c)
	if !strings.Contains(body, "Sending the link to a@x.org") || !strings.Contains(body, `hx-get="/f/address-step"`) {
		t.Errorf("the page after asking does not say so and poll: %s", body)
	}

	r.hostSays(t, dashboard.Casa{State: casa.ClaimPending, Name: "brave-elf.briard.casa", Email: "a@x.org"})
	body = r.page(c)
	if !strings.Contains(body, "Check your email") || !strings.Contains(body, "a@x.org") || !strings.Contains(body, `hx-get="/f/address-step"`) || strings.Contains(body, "Sending the link") {
		t.Errorf("the pending page: %s", body)
	}

	// Registered, but the household's network not yet found to resolve the name: the name is not
	// linked anywhere, and the page polls for the check's answer.
	reg := dashboard.Casa{State: casa.ClaimRegistered, Name: "brave-elf.briard.casa", Email: "a@x.org", CertUntil: time.Date(2027, 1, 2, 0, 0, 0, 0, time.UTC)}
	r.hostSays(t, reg)
	body = r.page(c)
	if strings.Contains(body, "https://brave-elf.briard.casa") || strings.Contains(body, "home-assistant.brave-elf.briard.casa") || !strings.Contains(body, "Checking that your home network finds the name") || !strings.Contains(body, `hx-get="/f/address-card"`) {
		t.Errorf("the registered page, check unanswered: %s", body)
	}

	reg.Resolves = dashboard.CasaBlocked
	r.hostSays(t, reg)
	body = r.page(c)
	if strings.Contains(body, "https://brave-elf.briard.casa") || !strings.Contains(body, "your router blocks the name") || strings.Contains(body, `hx-get=`) {
		t.Errorf("the registered page, name blocked: %s", body)
	}

	reg.Resolves = dashboard.CasaResolves
	r.hostSays(t, reg)
	body = r.page(c)
	if !strings.Contains(body, `href="https://brave-elf.briard.casa/"`) || !strings.Contains(body, "valid until 2 Jan 2027") || strings.Contains(body, `hx-get=`) || strings.Contains(body, `action="/casa/claim"`) {
		t.Errorf("the registered page: %s", body)
	}
}

// The host's refusals reach the person in the host's words, with the form kept for another try.
func TestCasaRefusalIsShownWithTheForm(t *testing.T) {
	r := newRig(t)
	r.app.casaPath = filepath.Join(r.dir, "casa.json")
	c := r.trust()
	port := newFakePort()
	r.app.port = port
	r.hostSays(t, dashboard.Casa{Name: "brave-elf.briard.casa"})

	port.answer <- api.DirectiveOutcome{State: api.OutcomeFailed, Detail: "this name belongs to another account; rename your flock and claim again"}
	if resp := r.form("/casa/claim", c, url.Values{"email": {"b@x.org"}}); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("claim = %d", resp.StatusCode)
	}
	body := r.page(c)
	if !strings.Contains(body, "belongs to another account") || !strings.Contains(body, `value="b@x.org"`) || !strings.Contains(body, `action="/casa/claim"`) || strings.Contains(body, `http-equiv="refresh"`) {
		t.Errorf("the refused page: %s", body)
	}
	// A refusal the host learnt at the click reaches the page through its view.
	r.app.casaAsked = nil
	r.hostSays(t, dashboard.Casa{State: casa.ClaimRefused, Name: "brave-elf.briard.casa", Reason: "taken"})
	if body := r.page(c); !strings.Contains(body, "taken") || !strings.Contains(body, `action="/casa/claim"`) {
		t.Errorf("the host's refusal is not shown with the form: %s", body)
	}
	r.hostSays(t, dashboard.Casa{State: casa.ClaimExpired, Name: "brave-elf.briard.casa"})
	if body := r.page(c); !strings.Contains(body, "link expired") {
		t.Errorf("an expired link is not explained: %s", body)
	}
}

// "Not now" takes the step off the home page for five days, on the volume so every device
// agrees; the Secure access page offers the name throughout, with nothing to put off; asking
// again, or undo, brings it back.
func TestCasaSkipHidesTheStepFromTheHomeForAWhile(t *testing.T) {
	r := newRig(t)
	r.app.casaPath = filepath.Join(r.dir, "casa.json")
	c := r.trust()
	r.hostSays(t, dashboard.Casa{Name: "brave-elf.briard.casa"})
	if _, home := get(t, r, "/", c); !strings.Contains(home, `formaction="/casa/skip" formnovalidate`) {
		t.Errorf("the step's Not now would be blocked by the email field's validation: %s", home)
	}

	if resp := r.form("/casa/skip", c, nil); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("skip = %d", resp.StatusCode)
	}
	if _, home := get(t, r, "/", c); strings.Contains(home, "<h2>Secure access</h2>") {
		t.Errorf("the home still shows the step after Not now: %s", home)
	}
	if _, addr := get(t, r, "/secure", c); !strings.Contains(addr, "<h2>Secure access</h2>") || strings.Contains(addr, ">Not now<") {
		t.Errorf("the Secure access page does not offer the name after Not now, or offers Not now itself: %s", addr)
	}
	r.app.now = func() time.Time { return time.Now().Add(casaSkipFor + time.Hour) }
	if _, home := get(t, r, "/", c); !strings.Contains(home, "<h2>Secure access</h2>") {
		t.Errorf("five days on, the step is still off the home: %s", home)
	}
	r.app.now = time.Now
	if resp := r.form("/casa/skip", c, url.Values{"undo": {"1"}}); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("undo = %d", resp.StatusCode)
	}
	if _, home := get(t, r, "/", c); !strings.Contains(home, "<h2>Secure access</h2>") {
		t.Errorf("undo did not bring the step back: %s", home)
	}
	if resp := r.form("/casa/skip", nil, nil); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("skip with no session = %d, want 401", resp.StatusCode)
	}
}

// "Check again" is offered only while the name is found blocked, is relayed to the host as one
// casa-recheck, and turns the card to checking until the host's view moves; a refusal is shown,
// and a check that comes back blocked again shows the button again.
func TestCasaRecheckIsOfferedOnlyWhenBlocked(t *testing.T) {
	r := newRig(t)
	r.app.casaPath = filepath.Join(r.dir, "casa.json")
	c := r.trust()
	port := newFakePort()
	r.app.port = port
	reg := dashboard.Casa{State: casa.ClaimRegistered, Name: "brave-elf.briard.casa", Email: "a@x.org"}
	for _, res := range []string{"", dashboard.CasaResolves} {
		reg.Resolves = res
		r.hostSays(t, reg)
		if body := r.page(c); strings.Contains(body, `action="/casa/recheck"`) {
			t.Errorf("resolves=%q offers Check again: %s", res, body)
		}
	}
	reg.Resolves = dashboard.CasaBlocked
	r.hostSays(t, reg)
	if body := r.page(c); !strings.Contains(body, `action="/casa/recheck"`) || strings.Contains(body, `hx-get=`) {
		t.Fatalf("the blocked card does not offer Check again, or polls: %s", body)
	}
	if resp := r.form("/casa/recheck", nil, nil); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("recheck with no session = %d, want 401", resp.StatusCode)
	}

	port.answer <- api.DirectiveOutcome{State: api.OutcomeDone, Detail: "checking again"}
	if resp := r.form("/casa/recheck", c, nil); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("recheck = %d, want 303", resp.StatusCode)
	}
	port.mu.Lock()
	asked := append([]api.Directive(nil), port.asked...)
	port.mu.Unlock()
	if len(asked) != 1 || asked[0].Kind != api.DirectiveCasaRecheck {
		t.Fatalf("the host was asked %+v; want one casa-recheck", asked)
	}
	// The host has not pushed yet: the card says checking and polls, never the old verdict.
	if body := r.page(c); strings.Contains(body, `action="/casa/recheck"`) || !strings.Contains(body, "Checking that your home network finds the name") || !strings.Contains(body, `hx-get="/f/address-card"`) {
		t.Errorf("right after asking: %s", body)
	}
	// The new check came back blocked before the page saw it restart: once the wait has passed,
	// the verdict and the button are back.
	r.app.casaRecheck.at = time.Now().Add(-casaRecheckWait)
	if body := r.page(c); !strings.Contains(body, `action="/casa/recheck"`) || strings.Contains(body, `hx-get=`) {
		t.Errorf("after a blocked answer again: %s", body)
	}

	// A refusal is read on the card.
	port.answer <- api.DirectiveOutcome{State: api.OutcomeFailed, Detail: "this home has no registered name to check"}
	r.form("/casa/recheck", c, nil)
	if body := r.page(c); !strings.Contains(body, "this home has no registered name to check") || !strings.Contains(body, `action="/casa/recheck"`) {
		t.Errorf("a refused recheck: %s", body)
	}
	// And the host restarting the check supersedes the ask.
	reg.Resolves = ""
	r.hostSays(t, reg)
	r.page(c)
	if r.app.casaRecheck != nil {
		t.Error("the ask outlived the host's restarted check")
	}
}

// The offer is made in the browser's own terms: it names the host the browser is on -- the one
// its address bar marks "Not secure" -- whichever of the dashboard's names or addresses that is.
func TestCasaOfferNamesTheHostTheBrowserIsOn(t *testing.T) {
	r := newRig(t)
	r.app.casaPath = filepath.Join(r.dir, "casa.json")
	c := r.trust()
	r.hostSays(t, dashboard.Casa{Name: "brave-elf.briard.casa"})
	for _, host := range []string{"briard.local", "192.168.1.20:8080"} {
		req, _ := http.NewRequest("GET", r.srv.URL+"/", nil)
		req.Host = host
		req.AddCookie(c)
		resp, err := http.DefaultClient.Do(req)
		must(t, err)
		body := bodyOf(t, resp)
		if want := "Your browser marks <code>" + strings.TrimSuffix(host, ":8080") + "</code> as"; !strings.Contains(string(body), want) || !strings.Contains(string(body), "<code>https://brave-elf.briard.casa</code></p>") {
			t.Errorf("on %s, the offer does not name the host and the address: %s", host, body)
		}
	}
}
