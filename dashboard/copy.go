package main

// The phone's copy page: "On your phone" (phone.go) shows a QR code beside its fields, and the
// phone that scans it opens a page of the same fields, each with a Copy button, so nothing is
// typed on a phone's keyboard. No client app takes its settings from a QR code in any standard
// way, so the page is for the person, and it serves every app the dialog does.
//
// The page is the one thing the dashboard serves without a session: the phone that scans it is
// rarely a trusted device. So its address carries a bearer token, 192 random bits, minted when the
// dialog is drawn and good for copyFor -- long enough to find the app on the phone and paste, short
// enough that a photo of the screen is soon worth nothing. It is not spent on the first open: a
// phone's camera often fetches the address to show a preview before the person opens it. It shows
// exactly what the dialog showed, which is the same exposure as the screen. One token per app at a
// time: drawing the dialog again replaces it. Tokens live in this process only, so a restart
// ends them.
//
// The page goes out with no-store and no referrer, so neither a cache nor the link to the app's
// clients carries the token anywhere.

import (
	"crypto/rand"
	"encoding/base64"
	"html/template"
	"log"
	"net/http"
	"strings"
	"time"

	"briard.io/shared/casa"
	"briard.io/shared/dashboard"
	"briard.io/shared/routes"
	"rsc.io/qr"
)

// copyFor is how long a copy page stays open after the dialog that drew it.
const copyFor = 10 * time.Minute

// copyPath is where the copy pages are served, a token after it.
const copyPath = "/copy/"

// copyPage is one live token's page: the fields and the link the dialog showed, until when.
type copyPage struct {
	app   string
	view  phoneView
	until time.Time
}

// copyScript is the Copy buttons: the clipboard API where the page is https, and the older
// copy command where it is not (the `.local` name, plain http), which a phone's browser still
// offers. The button says it worked.
const copyScript = "document.addEventListener('click',function(e){var b=e.target.closest('[data-copy]');if(!b)return;" +
	"var t=document.getElementById(b.dataset.copy).textContent;" +
	"function ok(){b.textContent='Copied';setTimeout(function(){b.textContent='Copy'},1500)}" +
	"if(navigator.clipboard&&window.isSecureContext){navigator.clipboard.writeText(t).then(ok);return}" +
	"var a=document.createElement('textarea');a.value=t;a.setAttribute('readonly','');a.style.position='fixed';a.style.opacity='0';" +
	"document.body.appendChild(a);a.select();document.execCommand('copy');a.remove();ok()})"

func copyJS() template.JS { return template.JS(copyScript) }

// copyQR mints the token for the dialog's fields and returns the QR code of its page's address, as
// an image the dialog shows inline, or "" when there is nothing to show.
func (a *app) copyQR(v phoneView) template.URL {
	if v.Error != "" || v.Address == "" {
		return ""
	}
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		log.Printf("dashboard: copy page for %s: %v", v.Name, err)
		return ""
	}
	tok := base64.RawURLEncoding.EncodeToString(buf)
	code, err := qr.Encode(a.copyBase()+copyPath+tok, qr.M)
	if err != nil {
		log.Printf("dashboard: copy page for %s: %v", v.Name, err)
		return ""
	}
	now := a.now()
	a.copyMu.Lock()
	for t, p := range a.copyPages {
		if p.app == v.Name || !now.Before(p.until) {
			delete(a.copyPages, t)
		}
	}
	a.copyPages[tok] = copyPage{app: v.Name, view: v, until: now.Add(copyFor)}
	a.copyMu.Unlock()
	return template.URL("data:image/png;base64," + base64.StdEncoding.EncodeToString(code.PNG()))
}

// copyBase is where a phone reaches this dashboard: the household's casa name, by https, once its
// network resolves it and a certificate is in place, as "On your phone" decides for an app's
// address; the alias by http otherwise.
func (a *app) copyBase() string {
	h := a.hostCasa()
	if h.State == casa.ClaimRegistered && h.Resolves == dashboard.CasaResolves && !h.CertUntil.IsZero() && h.Name != "" {
		return "https://" + h.Name
	}
	return "http://" + routes.BareFlockHostName
}

// copyView is the page: the dialog's fields, or Gone for a token that is spent or never was.
type copyView struct {
	phoneView
	Gone bool
}

// serveCopy answers a copy page, with no session asked for.
func (a *app) serveCopy(w http.ResponseWriter, r *http.Request, tok string) {
	a.copyMu.Lock()
	p, ok := a.copyPages[tok]
	if ok && !a.now().Before(p.until) {
		delete(a.copyPages, tok)
		ok = false
	}
	a.copyMu.Unlock()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Robots-Tag", "noindex")
	v := copyView{phoneView: p.view}
	if !ok || strings.ContainsAny(tok, "/?#") {
		v = copyView{Gone: true}
		w.WriteHeader(http.StatusNotFound)
	}
	if err := page.ExecuteTemplate(w, "copy", v); err != nil {
		log.Printf("dashboard: render copy: %v", err)
	}
}

// copyFieldView is one field of the copy page: its element id, label and value.
type copyFieldView struct{ ID, Label, Value string }

func copyField(id, label, value string) copyFieldView { return copyFieldView{id, label, value} }
