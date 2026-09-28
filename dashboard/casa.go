package main

// The household's name on its page ([V3c.4]): `<flock>.briard.casa`, with a real certificate,
// claimed by typing an email and clicking the link it receives. The first thing the page
// offers, and skippable -- the anonymous install stays first-class, and "Skip" is the whole of
// what that costs.
//
// The page decides nothing. What it shows is the host's view, pushed into dashboard.CasaPath
// through the guest (the host holds the key and the claim; the guest is disposable), and what
// it asks for is one directive through the admin port, casa-claim with the email, which the
// host answers with what happened: a link is on its way, or why not. "Skip" is the page's own
// state, on the volume beside the device registry, because it is a preference of the household
// and not a fact about the name.

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"briard.io/shared/api"
	"briard.io/shared/casa"
	"briard.io/shared/dashboard"
)

const casaSkipFile = "casa-skip.json"

type casaSkip struct {
	Skipped bool `json:"skipped"`
}

// casaAsk is the last claim asked from this page, kept until the host's view moves past it.
type casaAsk struct {
	Email  string
	Failed bool
	Detail string
}

// casaView is the card's data.
type casaView struct {
	State     string // "" / pending / registered / refused / expired
	Name      string
	Email     string
	Reason    string
	CertUntil string
	Skipped   bool
	Asked     *casaAsk
}

// Refresh says whether the page should poll itself: a claim is in flight, or one was just asked
// and the host has not yet said pending.
func (v *casaView) Refresh() bool {
	if v == nil {
		return false
	}
	return v.State == casa.ClaimPending || (v.Asked != nil && !v.Asked.Failed && v.State != casa.ClaimRegistered)
}

// casaState reads the host's view, the household's skip, and this page's last ask. Nil until
// the host has said what the name would be -- a guest the host never told (an older host, or
// a flock with no name) shows no card rather than an empty one.
func (a *app) casaState() *casaView {
	var h dashboard.Casa
	if raw, err := os.ReadFile(a.casaPath); err == nil {
		_ = json.Unmarshal(raw, &h)
	}
	if h.Name == "" {
		return nil
	}
	v := &casaView{}
	v.State, v.Name, v.Email, v.Reason = h.State, h.Name, h.Email, h.Reason
	if !h.CertUntil.IsZero() {
		v.CertUntil = h.CertUntil.Format("2 Jan 2006")
	}
	var skip casaSkip
	if err := a.readState(casaSkipFile, &skip); err == nil {
		v.Skipped = skip.Skipped
	}
	a.mu.Lock()
	// A successful ask is superseded by the host's own word on it; a failed one stays until
	// the next ask, so its reason is read.
	if a.casaAsked != nil && !a.casaAsked.Failed && v.State != "" && v.State != casa.ClaimExpired && v.State != casa.ClaimRefused {
		a.casaAsked = nil
	}
	v.Asked = a.casaAsked
	a.mu.Unlock()
	return v
}

// requestCasa is the form: one email, one directive, one answer shown on the page.
func (a *app) requestCasa(w http.ResponseWriter, r *http.Request) {
	email := strings.TrimSpace(r.FormValue("email"))
	if email == "" || !strings.Contains(email, "@") {
		http.Error(w, "an email address is needed to register a name\n", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	o, err := a.port.Submit(ctx, api.Directive{Kind: api.DirectiveCasaClaim, Payload: email})
	ask := &casaAsk{Email: email}
	switch {
	case err != nil:
		ask.Failed, ask.Detail = true, err.Error()
	case o.State != api.OutcomeDone:
		ask.Failed, ask.Detail = true, o.Detail
	default:
		ask.Detail = o.Detail
	}
	log.Printf("dashboard: casa claim for %s: outcome=%+v err=%v", email, o, err)
	a.mu.Lock()
	a.casaAsked = ask
	a.mu.Unlock()
	// Asking again is un-skipping: the household changed its mind.
	if err := a.writeState(casaSkipFile, casaSkip{}); err != nil {
		log.Printf("dashboard: casa skip: %v", err)
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// skipCasa folds the card away (or brings it back, with undo=1).
func (a *app) skipCasa(w http.ResponseWriter, r *http.Request) {
	if err := a.writeState(casaSkipFile, casaSkip{Skipped: r.FormValue("undo") == ""}); err != nil {
		http.Error(w, "could not record that\n", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}
