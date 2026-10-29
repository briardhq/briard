package main

// The household's name on its page: `<flock>.briard.casa`, with a real certificate,
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

// casaSkip is "not now" on the home page: the address step stays off the home for casaSkipFor,
// then returns; the Secure access page offers the name throughout, so nothing is ever lost.
type casaSkip struct {
	Skipped bool      `json:"skipped"`
	At      time.Time `json:"at"`
}

const casaSkipFor = 5 * 24 * time.Hour

// casaAsk is the last claim asked from this page, kept until the host's view moves past it.
type casaAsk struct {
	Email  string
	Failed bool
	Detail string
}

// casaRecheck is the last "check again" asked from this page. Until the host's view shows the
// check restarted (or casaRecheckWait passes, the check having come back blocked again before the
// page saw it), the card reads as checking rather than showing the old verdict.
type casaRecheck struct {
	at     time.Time
	Failed bool
	Detail string
}

const casaRecheckWait = 30 * time.Second

// casaView is the card's data.
type casaView struct {
	State      string // "" / pending / registered / refused / expired
	Name       string
	Email      string
	Reason     string
	CertUntil  string
	Skipped    bool
	Resolves   string // dashboard.Casa.Resolves: the name is linked only on "yes"
	RecheckErr string // why the last "check again" was not taken
	Asked      *casaAsk
	OnHome     bool   // the step is on the home page, the one place "Not now" exists (main.go, view)
	Host       string // the host the browser is on, which it marks "Not secure" (main.go, view)
}

// Refresh says whether the page should poll itself: a claim is in flight, or one was just asked
// and the host has not yet said pending, or a registered name's resolve check has not answered.
func (v *casaView) Refresh() bool {
	if v == nil {
		return false
	}
	return v.State == casa.ClaimPending || (v.Asked != nil && !v.Asked.Failed && v.State != casa.ClaimRegistered) ||
		(v.State == casa.ClaimRegistered && v.Resolves == "")
}

// casaName is the household's name as the host last said it, "" for a host that never did. The
// one host the hop carries trust to (hop.go), read off the same file as the view.
func (a *app) casaName() string {
	var h dashboard.Casa
	if raw, err := os.ReadFile(a.casaPath); err == nil {
		_ = json.Unmarshal(raw, &h)
	}
	return h.Name
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
	v.State, v.Name, v.Email, v.Reason, v.Resolves = h.State, h.Name, h.Email, h.Reason, h.Resolves
	if !h.CertUntil.IsZero() {
		v.CertUntil = h.CertUntil.In(zone()).Format("2 Jan 2006")
	}
	var skip casaSkip
	if err := a.readState(casaSkipFile, &skip); err == nil {
		v.Skipped = skip.Skipped && a.now().Sub(skip.At) < casaSkipFor
	}
	a.mu.Lock()
	// A successful ask is superseded by the host's own word on it; a failed one stays until
	// the next ask, so its reason is read.
	if a.casaAsked != nil && !a.casaAsked.Failed && v.State != "" && v.State != casa.ClaimExpired && v.State != casa.ClaimRefused {
		a.casaAsked = nil
	}
	v.Asked = a.casaAsked
	if rc := a.casaRecheck; rc != nil {
		switch {
		case h.Resolves != dashboard.CasaBlocked:
			a.casaRecheck = nil // the host restarted the check, or it already answered yes
		case rc.Failed:
			v.RecheckErr = rc.Detail
		case time.Since(rc.at) < casaRecheckWait:
			v.Resolves = "" // asked, and the host has not shown the new check yet
		default:
			a.casaRecheck = nil // the new check came back blocked too
		}
	}
	a.mu.Unlock()
	return v
}

// recheckCasa is the "check again" button: one directive, and the card reads as checking until
// the host's view says otherwise.
func (a *app) recheckCasa(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	o, err := a.port.Submit(ctx, api.Directive{Kind: api.DirectiveCasaRecheck})
	rc := &casaRecheck{at: time.Now()}
	switch {
	case err != nil:
		rc.Failed, rc.Detail = true, err.Error()
	case o.State != api.OutcomeDone:
		rc.Failed, rc.Detail = true, o.Detail
	}
	log.Printf("dashboard: casa recheck: outcome=%+v err=%v", o, err)
	a.mu.Lock()
	a.casaRecheck = rc
	a.mu.Unlock()
	http.Redirect(w, r, back(r), http.StatusSeeOther)
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
	http.Redirect(w, r, back(r), http.StatusSeeOther)
}

// skipCasa takes the step off the home page for a while (or brings it back, with undo=1).
func (a *app) skipCasa(w http.ResponseWriter, r *http.Request) {
	if err := a.writeState(casaSkipFile, casaSkip{Skipped: r.FormValue("undo") == "", At: a.now()}); err != nil {
		http.Error(w, "could not record that\n", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, back(r), http.StatusSeeOther)
}
