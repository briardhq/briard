package main

// "On your phone": what a client app on another device signs in to an app with -- its address, a
// username and a password -- shown in a dialog to copy, beside the app's Open, with a link to the
// clients the app's project lists. Three apps have one, and two web apps a different dialog
// (below). Navidrome (navidrome.go): a phone's music
// app speaks its Subsonic API, on the one path the reverse proxy leaves open. FreshRSS
// (freshrss.go): a phone's feed reader speaks its Google Reader API, likewise, with three words
// kept in FreshRSS's data. Home Assistant: its companion app signs in on Home Assistant's own
// login page, with the owner's username and four words the in-HA integration keeps (agent/hass).
// The dialog has two views, the QR code of the phone's copy page (copy.go) and the settings: two
// steps, the client to install and the three fields, each with its Copy button. It opens on the
// QR code where the pointer is a mouse and on the settings where it is a finger, and one button
// turns from one to the other; without a script both show. The button that opens it is a POST,
// because the first press may set Navidrome's or FreshRSS's password, so no page load ever does.
//
// The address is the household's casa name once it resolves with a certificate, the one a phone
// reaches from away too; the app's `.local` name before that.
//
// Mealie and Files are web apps a phone installs from their own page, with no client worth
// recommending over them. Their steps are to open the app on the phone -- signed in by a session
// of that app alone, minted from the copy page (copy.go) -- and to add it to the home screen, one
// line per phone, since no page can start that for the person. An app with clients of its own
// (Navidrome, FreshRSS) gives those only: a native client is the better app on a phone.

import (
	"context"
	"crypto/rand"
	_ "embed"
	"errors"
	"fmt"
	"html/template"
	"log"
	"math/big"
	"net/http"
	"strings"
	"time"

	"briard.io/agent/files"
	"briard.io/agent/freshrss"
	"briard.io/agent/hass"
	"briard.io/agent/mealie"
	"briard.io/agent/navidrome"
	"briard.io/shared/casa"
	"briard.io/shared/routes"
)

// phoneApps are the apps with sign-in details for a client on another device, each with the client
// to install, the step before signing in: one recommended for Android and one for iPhone, each
// free and open source and from the list the app's own project keeps, and that list for the rest.
// Home Assistant's is its own companion app, in each store.
var phoneApps = map[string]phoneClients{
	navidrome.Name: {"a player", []phoneLink{
		{"Tempus for Android", "https://eddyizm.github.io/tempus/"},
		{"Amperfy for iPhone", "https://apps.apple.com/app/amperfy-music/id1530145038"},
	}, &phoneLink{"Other players", "https://www.navidrome.org/apps/"}, false},
	freshrss.Name: {"a reader", []phoneLink{
		{"Capy Reader for Android", "https://capyreader.com/"},
		{"NetNewsWire for iPhone", "https://netnewswire.com/"},
	}, &phoneLink{"Other readers", "https://github.com/FreshRSS/FreshRSS#apis--native-apps"}, false},
	hass.Name: {"Home Assistant", []phoneLink{
		{"Android", "https://play.google.com/store/apps/details?id=io.homeassistant.companion.android"},
		{"iPhone", "https://apps.apple.com/app/home-assistant/id1099568401"},
	}, nil, false},
	mealie.Name: {Web: true},
	files.Name:  {Web: true},
}

// phoneClients is what the first step installs: its words ("Install a reader"), the recommended
// clients, and the link to the others, nil for none. Web is a web app's, which has none.
type phoneClients struct {
	Noun        string
	Recommended []phoneLink
	Others      *phoneLink
	Web         bool
}

// phoneLink is one link: its words and where it goes.
type phoneLink struct{ Label, URL string }

// phoneView is the dialog: the app, what its client signs in with or why it could not be read, and
// the link to its clients. Replace is Home Assistant's or FreshRSS's password row as a button: no
// kept password signs in any more. QR is the code of the phone's copy page for these fields
// (copy.go), "" when there are none. Settings opens the dialog on its settings, as after New
// password, whose words the person came for; otherwise the pointer decides. Copy is the copy
// page's token, on that page alone: its Open posts to it. A web app's Address is where Open goes,
// and the steps do not show it.
type phoneView struct {
	Name, Title                 string
	Address, Username, Password string
	Replace                     bool
	Error                       string
	Clients                     phoneClients
	QR                          template.URL
	Settings                    bool
	Copy                        string
}

// phone answers the button: the dialog for htmx, which adds it to the page and opens it
// (fragment.go), or the same fields as a page of their own without a script. For Home Assistant
// and FreshRSS, `?new` is the dialog's "New password", which sets new words first.
func (a *app) phone(w http.ResponseWriter, r *http.Request, name string) {
	clients, ok := phoneApps[name]
	if !ok {
		http.NotFound(w, r)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	v := phoneView{Name: name, Title: a.appTitle(name), Clients: clients}
	var err error
	if name == hass.Name {
		ha := a.findHomeAssistant()
		if ha == nil {
			http.NotFound(w, r)
			return
		}
		v.Address = a.phoneAddress(ha.app)
		v.Username, v.Password, err = a.homeAssistantSignIn(ctx, ha, r.URL.Query().Has("new"))
	} else {
		s, ok := gatedApps(a.routesPath)[name]
		if !ok {
			http.NotFound(w, r)
			return
		}
		v.Address = a.phoneAddress(s)
		switch {
		case clients.Web:
			// Installed from https only, so offered only there (view).
			if !strings.HasPrefix(v.Address, "https://") {
				http.NotFound(w, r)
				return
			}
		case name == freshrss.Name:
			// A Google Reader client is given the API's own address, as FreshRSS's profile page
			// gives it.
			v.Address += freshrssAPI
			v.Username, v.Password, err = a.freshrssSignIn(ctx, s, r.URL.Query().Has("new"))
		default:
			v.Username, v.Password, err = a.navidromeSignIn(ctx, s)
		}
	}
	v.Replace = err == nil && v.Password == "" && !clients.Web
	v.Settings = r.URL.Query().Has("new")
	if err != nil {
		log.Printf("dashboard: %s on a phone: %v", name, err)
		v.Username, v.Password, v.Error = "", "", "Could not read the sign-in from "+v.Title+": "+err.Error()
	}
	v.QR = a.copyQR(v)
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

// homeAssistantSignIn is the owner's username and the words kept for them while they still sign
// in, "" when they do not; `replace` first sets four new words on the owner, which the integration
// keeps. The password returned is read back from Home Assistant after any change, never the one
// briard meant to set.
func (a *app) homeAssistantSignIn(ctx context.Context, ha *homeAssistant, replace bool) (user, password string, err error) {
	_, access, err := hass.SystemAccess(ctx, a.exec(), ha.port)
	if err != nil {
		return "", "", fmt.Errorf("its control channel is not up yet: %w", err)
	}
	if replace {
		words, err := passphrase(4)
		if err != nil {
			return "", "", err
		}
		if _, err := hass.SetPhonePassword(ctx, ha.base, access, words); err != nil {
			return "", "", homeAssistantRefusal(err)
		}
	}
	user, password, err = hass.PhoneSignIn(ctx, ha.base, access)
	return user, password, homeAssistantRefusal(err)
}

// homeAssistantRefusal says the integration's two refusals in the dialog's words.
func homeAssistantRefusal(err error) error {
	switch {
	case errors.Is(err, hass.ErrNoOwner):
		return errors.New("it has no owner account")
	case errors.Is(err, hass.ErrNoMinter):
		return errors.New("it is not running Briard's integration, which it loads when it next starts")
	}
	return err
}

// phoneAddress is where a client reaches the app: the casa name, by https, once the household's
// network resolves it and a certificate is in place; the `.local` name, by http, otherwise.
func (a *app) phoneAddress(s routes.App) string {
	if a.casaCertified() {
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

// passphrase is `n` words of the list, joined by spaces, about 10.3 bits a word, typed on a
// phone once: a space is on a phone keyboard's first page, a hyphen is not. Navidrome's are
// three (about 31 bits), behind its own sign-in limit. FreshRSS's are three too: it limits
// nothing, and its bcrypt hash costs about 20 ms of a core per guess, but they open a feed reader
// and nothing else. Home Assistant's are four (about 41 bits): it limits nothing unless the
// household sets it, so the brake is its bcrypt hash alone, about a quarter second of one core
// per guess, from the home's network and from away alike.
func passphrase(n int) (string, error) {
	size := big.NewInt(int64(len(words)))
	var picked []string
	for range n {
		i, err := rand.Int(rand.Reader, size)
		if err != nil {
			return "", err
		}
		picked = append(picked, words[i.Int64()])
	}
	return strings.Join(picked, " "), nil
}
