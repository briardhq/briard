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
	if !strings.Contains(body, `action="/casa/claim"`) || !strings.Contains(body, "brave-elf.briard.casa") || !strings.Contains(body, ">Skip<") || strings.Contains(body, `http-equiv="refresh"`) {
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
	if !strings.Contains(body, "Sending the link to a@x.org") || !strings.Contains(body, `http-equiv="refresh"`) {
		t.Errorf("the page after asking does not say so and poll: %s", body)
	}

	r.hostSays(t, dashboard.Casa{State: casa.ClaimPending, Name: "brave-elf.briard.casa", Email: "a@x.org"})
	body = r.page(c)
	if !strings.Contains(body, "Check your email") || !strings.Contains(body, "a@x.org") || !strings.Contains(body, `http-equiv="refresh"`) || strings.Contains(body, "Sending the link") {
		t.Errorf("the pending page: %s", body)
	}

	r.hostSays(t, dashboard.Casa{State: casa.ClaimRegistered, Name: "brave-elf.briard.casa", Email: "a@x.org", CertUntil: time.Date(2027, 1, 2, 0, 0, 0, 0, time.UTC)})
	body = r.page(c)
	if !strings.Contains(body, `href="https://brave-elf.briard.casa/"`) || !strings.Contains(body, "valid until 2 Jan 2027") || strings.Contains(body, `http-equiv="refresh"`) || strings.Contains(body, `action="/casa/claim"`) {
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

// Skip folds the card to one line, on the volume so every device agrees; asking again unfolds it.
func TestCasaSkipIsRememberedAndUndone(t *testing.T) {
	r := newRig(t)
	r.app.casaPath = filepath.Join(r.dir, "casa.json")
	c := r.trust()
	r.hostSays(t, dashboard.Casa{Name: "brave-elf.briard.casa"})

	if resp := r.form("/casa/skip", c, nil); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("skip = %d", resp.StatusCode)
	}
	body := r.page(c)
	if strings.Contains(body, "Give this home an address") || !strings.Contains(body, "Register a name") {
		t.Errorf("the skipped page still shows the card, or no way back: %s", body)
	}
	if resp := r.form("/casa/skip", c, url.Values{"undo": {"1"}}); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("undo = %d", resp.StatusCode)
	}
	if body := r.page(c); !strings.Contains(body, "Give this home an address") {
		t.Errorf("undo did not bring the card back: %s", body)
	}
	if resp := r.form("/casa/skip", nil, nil); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("skip with no session = %d, want 401", resp.StatusCode)
	}
}
