package main

// "On your phone": what a client app on another device signs in to an app with -- its address, a
// username and a password -- shown in a dialog to copy, beside the app's Open, with a link to the
// clients the app's project lists. Only Navidrome has one (navidrome.go): a phone's music app
// speaks its Subsonic API, on the one path the reverse proxy leaves open. The dialog holds the
// three fields, the link and no prose; the button is a POST, because the first press may change
// the password, so no page load ever does.
//
// The address is the household's casa name once it resolves with a certificate, the one a phone
// reaches from away too; the app's `.local` name before that.

import (
	"context"
	"crypto/rand"
	_ "embed"
	"log"
	"math/big"
	"net/http"
	"strings"
	"time"

	"briard.io/agent/navidrome"
	"briard.io/shared/casa"
	"briard.io/shared/dashboard"
	"briard.io/shared/routes"
)

// phoneApps are the apps with sign-in details for a client on another device, each with where its
// clients are listed: a link in the dialog, since which client to install is the step before
// signing in, and the app's own project keeps that list current.
var phoneApps = map[string]phoneClients{
	navidrome.Name: {"Compatible players", "https://www.navidrome.org/apps/"},
}

// phoneClients is the link to an app's clients: its words and where it goes.
type phoneClients struct{ Label, URL string }

// phoneView is the dialog: the app, what its client signs in with or why it could not be read, and
// the link to its clients.
type phoneView struct {
	Name, Title                 string
	Address, Username, Password string
	Error                       string
	Clients                     phoneClients
}

// phone answers the button: the dialog for htmx, which adds it to the page and opens it
// (fragment.go), or the same fields as a page of their own without a script.
func (a *app) phone(w http.ResponseWriter, r *http.Request, name string) {
	s, ok := gatedApps(a.routesPath)[name]
	clients, phone := phoneApps[name]
	if !ok || !phone {
		http.NotFound(w, r)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	v := phoneView{Name: name, Title: a.appTitle(name), Address: a.phoneAddress(s), Clients: clients}
	var err error
	v.Username, v.Password, err = a.navidromeSignIn(ctx, s)
	if err != nil {
		log.Printf("dashboard: %s on a phone: %v", name, err)
		v.Username, v.Password, v.Error = "", "", "Could not read the sign-in from "+v.Title+": "+err.Error()
	}
	tmpl := "phone-page"
	if r.Header.Get("HX-Request") == "true" {
		tmpl = "phone-dialog"
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := page.ExecuteTemplate(w, tmpl, v); err != nil {
		log.Printf("dashboard: render %s: %v", tmpl, err)
	}
}

// phoneAddress is where a client reaches the app: the casa name, by https, once the household's
// network resolves it and a certificate is in place; the `.local` name, by http, otherwise.
func (a *app) phoneAddress(s routes.App) string {
	h := a.hostCasa()
	if h.State == casa.ClaimRegistered && h.Resolves == dashboard.CasaResolves && !h.CertUntil.IsZero() {
		for _, host := range s.Hosts {
			if strings.HasSuffix(host, "."+casa.Zone) {
				return "https://" + host
			}
		}
	}
	return "http://" + routes.BareHostName(s.Name)
}

// effShortWords is the EFF's short word list (1,296 words of at most five letters, picked to be
// told apart when typed), as the EFF publishes it: a dice number and a word on each line.
//
//go:embed eff_short_wordlist_1.txt
var effShortWords string

var words = func() []string {
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(effShortWords), "\n") {
		if f := strings.Fields(line); len(f) == 2 {
			out = append(out, f[1])
		}
	}
	return out
}()

// passphrase is three words of the list, joined by hyphens: about 31 bits, typed on a phone
// once. Guessing is held off by the app's own sign-in limit, and the app is reached only from
// the home's network or a private network into it.
func passphrase() (string, error) {
	n := big.NewInt(int64(len(words)))
	var picked []string
	for range 3 {
		i, err := rand.Int(rand.Reader, n)
		if err != nil {
			return "", err
		}
		picked = append(picked, words[i.Int64()])
	}
	return strings.Join(picked, "-"), nil
}
