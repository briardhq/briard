package main

import (
	"encoding/base64"
	"encoding/json"
	"html"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"briard.io/shared/api"
	"briard.io/shared/dashboard"
	"briard.io/shared/manifest"
)

// testIcon is a mark as the index carries one: an SVG, base64, a data URI.
var testIcon = "data:image/svg+xml;base64," + base64.StdEncoding.EncodeToString([]byte(
	`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24"><circle cx="12" cy="12" r="10" fill="#41bdf5"/></svg>`))

// testCatalog is the host's view of an index of four: the two apps this product installs, with
// manifests, and two it lists and cannot install yet.
func testCatalog() dashboard.Catalog {
	app := func(name, title, fn, desc, home string, available bool) dashboard.CatalogApp {
		return dashboard.CatalogApp{App: manifest.App{Name: name, Title: title, Functionality: fn, Description: desc, Icon: testIcon, Homepage: home, Available: available}}
	}
	return dashboard.Catalog{Fetched: time.Date(2026, 10, 30, 3, 0, 0, 0, time.UTC), Apps: []dashboard.CatalogApp{
		app("home-assistant", "Home Assistant", "Smart home control", "Runs your home from one place: lights, heating, cameras, locks and thousands of other devices.", "https://www.home-assistant.io", true),
		app("mosquitto", "Mosquitto", "Smart device messaging", "The message broker many smart devices and sensors talk through.", "https://mosquitto.org", true),
		app("paperless-ngx", "Paperless-ngx", "Document management system", "Scan or drop in letters, bills, contracts and receipts; it reads them, tags them and makes every word searchable.", "https://docs.paperless-ngx.com", false),
		app("vaultwarden", "Vaultwarden", "Password manager", "Stores passwords, passkeys, cards and notes for the whole family, with the Bitwarden apps you may already use.", "https://github.com/dani-garcia/vaultwarden", false),
	}}
}

// hostLists is the host pushing its view of the catalog's index (dashboard.catalog).
func (r *rig) hostLists(t *testing.T, c dashboard.Catalog) {
	t.Helper()
	raw, _ := json.Marshal(c)
	must(t, os.WriteFile(r.app.catalogPath, raw, 0o644))
}

// wished flips one app's wish in the pushed view, the way the host's next cycle does.
func (r *rig) wished(t *testing.T, name string, on bool) {
	t.Helper()
	c := testCatalog()
	for i := range c.Apps {
		if c.Apps[i].Name == name {
			c.Apps[i].Wished = on
		}
	}
	r.hostLists(t, c)
}

// Before the host has pushed an index the page knows only the routes table: the installed app
// under a title made from its slug, no Available, no Coming soon, and the home's chooser with
// nothing to choose -- never a list from some other clock.
func TestBeforeAnyIndexThePageShowsOnlyWhatIsInstalled(t *testing.T) {
	r := newRig(t)
	c := r.trust()
	must(t, os.Remove(r.app.catalogPath))
	body := r.get(t, "/apps", c)
	for _, want := range []string{"Home Assistant", "has not read its catalog yet"} {
		if !strings.Contains(body, want) {
			t.Errorf("apps page lacks %q: %s", want, body)
		}
	}
	for _, not := range []string{"Available", "Coming soon", "Mosquitto", "Paperless"} {
		if strings.Contains(body, not) {
			t.Errorf("apps page shows %q with no index: %s", not, body)
		}
	}
	if resp := r.do("POST", "/install/mosquitto", c, nil); resp.StatusCode != http.StatusNotFound {
		t.Errorf("an install with no index = %d; want 404", resp.StatusCode)
	}
	notInstalled(t, r)
	home := r.page(c)
	if !strings.Contains(home, "has not read its catalog yet") || strings.Contains(home, "Install Home Assistant") {
		t.Errorf("the chooser with no index: %s", home)
	}
}

// The index's words and marks reach every card: the installed app's title and function, the
// available one's description, the icon as an image the browser may load.
func TestCardsAreBuiltFromTheIndex(t *testing.T) {
	r := newRig(t)
	c := r.trust()
	body := html.UnescapeString(r.get(t, "/apps", c)) // the data URI's + is an entity in the attribute, as a browser reads it back
	for _, want := range []string{"Smart home control", "Smart device messaging", "The message broker many smart devices",
		`<img class="app-icon" src="` + testIcon + `"`, "Coming soon", "Document management system", "I want this",
		"tells the Briard team which app you want", `href="https://docs.paperless-ngx.com"`} {
		if !strings.Contains(body, want) {
			t.Errorf("apps page lacks %q", want)
		}
	}
	if strings.Contains(body, "ZgotmplZ") {
		t.Error("the icon's data URI was refused by the template")
	}
	if resp := r.do("GET", "/apps/paperless-ngx", c, nil); resp.StatusCode != http.StatusOK {
		t.Errorf("a listed app's page = %d", resp.StatusCode)
	}
	if resp := r.do("GET", "/apps/radarr", c, nil); resp.StatusCode != http.StatusNotFound {
		t.Errorf("an unlisted app's page = %d; want 404", resp.StatusCode)
	}
}

// "I want this" relays one wish directive to the host, waits for the view it pushes back, and
// answers the one card -- recorded -- when the page's script asks; the second press is the same
// relay and the card returns to its first state. Without the script the page is reloaded.
func TestComingSoonWishIsRelayedAndTheCardFollowsTheHost(t *testing.T) {
	r := newRig(t)
	r.app.viewWait = 2 * time.Second
	c := r.trust()
	port := newFakePort()
	r.app.port = port
	hx := map[string]string{"HX-Request": "true"}
	// The host records the wish and pushes the view as its outcome lands.
	go func() {
		<-port.waiting
		r.wished(t, "paperless-ngx", true)
		port.answer <- api.DirectiveOutcome{State: api.OutcomeDone, Detail: "the wish for Paperless-ngx is recorded"}
	}()
	req, _ := http.NewRequest("POST", r.srv.URL+"/wish/paperless-ngx", nil)
	req.AddCookie(c)
	for k, v := range hx {
		req.Header.Set(k, v)
	}
	resp, err := noRedirect.Do(req)
	must(t, err)
	card := bodyOf(t, resp)
	if resp.StatusCode != http.StatusOK || strings.Contains(card, "<html") {
		t.Fatalf("the hx answer = %d, a whole page? %s", resp.StatusCode, card)
	}
	for _, want := range []string{"Your wish is recorded. Briard will notify you when this app becomes available.", "Never mind", "Paperless-ngx"} {
		if !strings.Contains(card, want) {
			t.Errorf("the wished card lacks %q: %s", want, card)
		}
	}
	port.mu.Lock()
	asked := port.asked
	port.mu.Unlock()
	if len(asked) != 1 || asked[0].Kind != api.DirectiveWish || asked[0].Payload != "paperless-ngx" {
		t.Fatalf("asked the host %+v; want one wish for paperless-ngx", asked)
	}
	// The whole page agrees with the card.
	if body := r.get(t, "/apps", c); !strings.Contains(body, "Your wish is recorded") {
		t.Error("the apps page does not show the wish")
	}
	// The second press, as a plain form post: the host removes it, the page reloads.
	go func() {
		<-port.waiting
		r.wished(t, "paperless-ngx", false)
		port.answer <- api.DirectiveOutcome{State: api.OutcomeDone, Detail: "the wish for Paperless-ngx is removed"}
	}()
	resp = r.form("/wish/paperless-ngx", c, url.Values{})
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("a plain press = %d; want 303 back to the page", resp.StatusCode)
	}
	if body := r.get(t, "/apps", c); strings.Contains(body, "Your wish is recorded") || !strings.Contains(body, "I want this") {
		t.Error("the apps page still shows the wish after the second press")
	}
	// An app the index does not list has no button to press.
	if resp := r.do("POST", "/wish/radarr", c, nil); resp.StatusCode != http.StatusNotFound {
		t.Errorf("a wish for an unlisted app = %d; want 404", resp.StatusCode)
	}
}

// The host said no (the port is gone, say): the card is answered as it stands, with no claim
// that anything was recorded.
func TestAWishTheHostRefusedChangesNothing(t *testing.T) {
	r := newRig(t)
	c := r.trust()
	port := newFakePort()
	port.noPort = true
	r.app.port = port
	req, _ := http.NewRequest("POST", r.srv.URL+"/wish/vaultwarden", nil)
	req.AddCookie(c)
	req.Header.Set("HX-Request", "true")
	resp, err := noRedirect.Do(req)
	must(t, err)
	card := bodyOf(t, resp)
	if strings.Contains(card, "Your wish is recorded") || !strings.Contains(card, "I want this") {
		t.Errorf("a refused wish shows as recorded: %s", card)
	}
}

// A wished app that gained a manifest sits under Available with the line that says so, the
// install button, and a way to take the wish back; the notice itself is the host's alert.
func TestANewlyAvailableWishedAppSaysSo(t *testing.T) {
	r := newRig(t)
	c := r.trust()
	cat := testCatalog()
	cat.Apps[2].Available, cat.Apps[2].Wished = true, true // paperless-ngx
	r.hostLists(t, cat)
	body := r.get(t, "/apps", c)
	i := strings.Index(body, "Available")
	j := strings.Index(body, "Coming soon")
	if i < 0 || j < 0 || i > j {
		t.Fatalf("sections out of order or missing: available at %d, coming soon at %d", i, j)
	}
	available := body[i:j]
	for _, want := range []string{"Paperless-ngx", "You asked for this. It is available now.", "Install Paperless-ngx", "Never mind"} {
		if !strings.Contains(available, want) {
			t.Errorf("the available section lacks %q", want)
		}
	}
	if strings.Contains(body[j:], "Paperless-ngx") {
		t.Error("the app is still under Coming soon")
	}
}

// A slug the index does not list is titled from its words.
func TestTitleOfSlug(t *testing.T) {
	for slug, want := range map[string]string{"home-assistant": "Home Assistant", "mosquitto": "Mosquitto", "z-wave-js-ui": "Z Wave Js Ui"} {
		if got := titleOf(slug); got != want {
			t.Errorf("titleOf(%q) = %q, want %q", slug, got, want)
		}
	}
}

func init() {
	// Every rig has the host's view of the index unless a test takes it away: the product's
	// two apps were on every page before the index existed, and still are.
	rigCatalog = func(t *testing.T, r *rig) {
		r.app.catalogPath = filepath.Join(r.dir, "catalog.json")
		r.hostLists(t, testCatalog())
	}
}

// offering is the host's view with Home Assistant installed at 2026.9.4 and, when update, the
// catalog's 2026.10.1 on offer.
func offering(update bool) dashboard.Catalog {
	c := testCatalog()
	c.Apps[0].Installed, c.Apps[0].Running = true, "2026.9.4"
	c.Apps[0].Available, c.Apps[0].Version = true, "2026.10.1"
	c.Apps[0].Update = update
	if !update {
		c.Apps[0].Running = "2026.10.1"
	}
	return c
}

// AN UPDATE THE CATALOG OFFERS is a badge, a line naming both versions and the undo, and an
// Update button on the app's card wherever it shows; the app's own page also says what Briard
// does around it. The press relays one service-install, as Install does; while it runs the card
// says it is updating and polls itself, and the home is not "setting up". An update that did
// not finish says so with the host's reason and a retry; one that did hides the offer until
// the host's view drops it.
func TestAnOfferedUpdateIsShownPressedAndTaken(t *testing.T) {
	r := newRig(t)
	c := r.trust()
	r.hostLists(t, offering(true))
	// The app's page asks the host for its history; a port that is not there answers at once.
	r.app.port = &fakePort{noPort: true}
	if _, app := get(t, r, "/apps/home-assistant", c); !strings.Contains(app, "puts it back by itself if not") || !strings.Contains(app, "· 2026.10.1 is available") {
		t.Errorf("the app's page does not explain the update or name the version on offer: %s", app)
	}
	port := newFakePort()
	r.app.port = port
	body := r.page(c)
	for _, want := range []string{"Update available", "2026.10.1 is available; you have 2026.9.4.", "You can undo the update afterwards from its History.", `action="/update/home-assistant"`} {
		if !strings.Contains(body, want) {
			t.Errorf("the offer does not show %q", want)
		}
	}
	if strings.Contains(body, `action="/update/mosquitto"`) {
		t.Error("an app with nothing on offer has an Update button")
	}
	if resp := r.do("POST", "/update/home-assistant", nil, nil); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("update with no session = %d, want 401", resp.StatusCode)
	}
	if resp := r.do("POST", "/update/mosquitto", c, nil); resp.StatusCode != http.StatusNotFound {
		t.Errorf("update of an app with no offer = %d, want 404", resp.StatusCode)
	}

	if resp := r.do("POST", "/update/home-assistant", c, nil); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("press = %d, want 303", resp.StatusCode)
	}
	<-port.waiting
	body = r.page(c)
	if !strings.Contains(body, "Updating") || !strings.Contains(body, `hx-get="/f/app?app=home-assistant`) || strings.Contains(body, `action="/update/home-assistant"`) || strings.Contains(body, "Setting up") {
		t.Errorf("the page while updating does not say so on the card and poll, or offers the button, or is setting up: %s", body)
	}
	port.mu.Lock()
	asked := append([]api.Directive(nil), port.asked...)
	port.mu.Unlock()
	if len(asked) != 1 || asked[0].Kind != api.DirectiveServiceInstall || asked[0].Payload != "home-assistant" {
		t.Fatalf("the host was asked %+v; want exactly one service-install of home-assistant", asked)
	}

	port.answer <- api.DirectiveOutcome{ID: asked[0].ID, State: api.OutcomeRolledBack, Detail: "home-assistant did not come up"}
	waitFor := func(want string) string {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for {
			body := r.page(c)
			if strings.Contains(body, want) || time.Now().After(deadline) {
				return body
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	body = waitFor("The update did not finish")
	if !strings.Contains(body, "The update did not finish, and Home Assistant is as it was: home-assistant did not come up") || !strings.Contains(body, "Try again") {
		t.Errorf("a rolled-back update is not surfaced with its reason and a retry: %s", body)
	}

	if resp := r.do("POST", "/update/home-assistant", c, nil); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("retry = %d", resp.StatusCode)
	}
	<-port.waiting
	port.answer <- api.DirectiveOutcome{State: api.OutcomeDone, Detail: "installed"}
	deadline := time.Now().Add(5 * time.Second)
	for body = r.page(c); strings.Contains(body, "Updating ·") && time.Now().Before(deadline); body = r.page(c) {
		time.Sleep(20 * time.Millisecond)
	}
	if strings.Contains(body, `action="/update/home-assistant"`) || strings.Contains(body, "Updating ·") {
		t.Errorf("a taken update is still offered, or still updating, before the host's push: %s", body)
	}
	r.hostLists(t, offering(false))
	if body = r.page(c); strings.Contains(body, "Update available") || strings.Contains(body, "The update did not finish") {
		t.Errorf("once the host's view drops the offer the card is the ordinary one: %s", body)
	}
}
