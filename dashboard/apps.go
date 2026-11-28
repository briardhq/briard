package main

// What the dashboard knows about each app's accounts, one entry per app: whose account the gate
// opens, and what a client on a phone signs in with. Each app's own knowledge is in its package
// (agent/mealie, agent/navidrome, agent/freshrss, agent/hass); what is here is where the
// dashboard finds the app on this node, and the rule every app shares.
//
// The rule (userIn): the gate signs the user in as the account the app's own data names as its
// admin, read at every check and never kept. An app with no account yet, or one that keeps none
// worth reading, is given the record's username, which makes the account. An app whose accounts
// hold no admin has none of the user's to open, and says so. An app with no entry is refused:
// nothing here falls through to another app's answer.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"

	"briard.io/agent/files"
	"briard.io/agent/freshrss"
	"briard.io/agent/hass"
	"briard.io/agent/mealie"
	"briard.io/agent/navidrome"
	"briard.io/agent/quadlet"
	"briard.io/shared/routes"
)

// appEntry is one app's entry. The zero entry is an app the dashboard signs nobody in to and
// shows no "On your phone" for.
type appEntry struct {
	// gated says the gate signs the user in to this app.
	gated bool
	// admin reads the username of the app's admin account off the app itself: none for an app
	// with no account at all yet, "" for one whose accounts hold no admin. nil for an app that
	// keeps no account worth reading, which is always signed in under the record's username.
	admin func(a *app, ctx context.Context, s routes.App, acct account) (user string, none bool, err error)
	// clients is the "On your phone" dialog's first step, nil for an app with no dialog. phone
	// is what a client signs in with, the address it is given included; nil for a web app,
	// which a phone opens and has nothing to copy. `replace` sets a new password first, for an
	// app whose kept one can stop signing in.
	clients *phoneClients
	phone   func(a *app, ctx context.Context, replace bool) (address, user, password string, err error)
}

// appEntries is every app the dashboard knows, by catalog name. The clients are one recommended
// for Android and one for iPhone, each free and open source and from the list the app's own
// project keeps, and that list for the rest. Navidrome's Android players are two: Symfonium, the
// one its users recommend most (paid), and Tempus, free. Home Assistant's is its own companion
// app, in each store. Mealie and Files are web apps.
var appEntries map[string]appEntry

// Filled at start: the entries name functions that read the table back.
func init() {
	appEntries = map[string]appEntry{
		hass.Name: {
			clients: &phoneClients{"Home Assistant", []phoneLink{
				{"Android", "https://play.google.com/store/apps/details?id=io.homeassistant.companion.android"},
				{"iPhone", "https://apps.apple.com/app/home-assistant/id1099568401"},
			}, nil, false},
			phone: homeAssistantPhone,
		},
		files.Name: {
			gated:   true,
			clients: &phoneClients{Web: true},
		},
		mealie.Name: {
			gated:   true,
			admin:   mealieAdmin,
			clients: &phoneClients{Web: true},
		},
		navidrome.Name: {
			gated: true,
			admin: func(a *app, ctx context.Context, _ routes.App, _ account) (string, bool, error) {
				return navidrome.Admin(ctx, a.inContainer(navidrome.Name, navidrome.Container, ""))
			},
			clients: &phoneClients{"a player", []phoneLink{
				{"Symfonium for Android", "https://symfonium.app/"},
				{"Tempus for Android", "https://eddyizm.github.io/tempus/"},
				{"Amperfy for iPhone", "https://apps.apple.com/app/amperfy-music/id1530145038"},
			}, &phoneLink{"Other players", "https://www.navidrome.org/apps/"}, false},
			phone: navidromePhone,
		},
		freshrss.Name: {
			gated: true,
			admin: func(a *app, ctx context.Context, _ routes.App, _ account) (string, bool, error) {
				user, err := freshrss.Admin(ctx, a.inContainer(freshrss.Name, freshrss.Container, freshrss.User))
				return user, false, err
			},
			clients: &phoneClients{"a reader", []phoneLink{
				{"Capy Reader for Android", "https://capyreader.com/"},
				{"NetNewsWire for iPhone", "https://netnewswire.com/"},
			}, &phoneLink{"Other readers", "https://github.com/FreshRSS/FreshRSS#apis--native-apps"}, false},
			phone: freshrssPhone,
		},
	}
}

// webApp says an app is one a phone installs from its own page.
func webApp(name string) bool {
	c := appEntries[name].clients
	return c != nil && c.Web
}

// errNotInstalled is an app the routing table does not have on this node.
var errNotInstalled = errors.New("the app is not installed here")

// userIn is the username the reverse proxy signs the user into a gated app as. The app's reader
// is handed what the routing table says of it, nothing for an app this node does not route.
func (a *app) userIn(ctx context.Context, name string) (string, error) {
	e := appEntries[name]
	if !e.gated {
		return "", fmt.Errorf("this dashboard has no sign-in for %s", name)
	}
	acct, err := a.account()
	if err != nil {
		return "", err
	}
	user, none := "", true
	if e.admin != nil {
		if user, none, err = e.admin(a, ctx, gatedApps(a.routesPath)[name], acct); err != nil {
			return "", err
		}
	}
	switch {
	case user != "":
		return user, nil
	case !none:
		title := a.appTitle(name)
		return "", fmt.Errorf("%s has no admin account, so there is no account of yours to open; undo %s to before it lost yours, or reinstall it", title, title)
	}
	return acct.Username, nil
}

// appBase is an app's address on this guest, where the reverse proxy forwards to.
func appBase(s routes.App) (string, error) {
	rt, ok := s.Route(routes.ListenName)
	if !ok {
		return "", fmt.Errorf("the routing table has no route to %s", s.Name)
	}
	base, err := s.Resolve(rt.To)
	if err != nil {
		return "", err
	}
	return base.String(), nil
}

// appLock is one app's lock: it holds a second read or change of the app's accounts while the
// first runs, so two checks never race through one password change, and two presses of "On your
// phone" never set two passwords and show one.
func (a *app) appLock(name string) *sync.Mutex {
	mu, _ := a.appLocks.LoadOrStore(name, &sync.Mutex{})
	return mu.(*sync.Mutex)
}

// inContainer runs a command in one of an app's containers, as user ("" for the image's own),
// with stdin, and returns what it printed to stdout; a test puts its own in appExec.
func (a *app) inContainer(app, container, user string) func(ctx context.Context, stdin string, args ...string) ([]byte, error) {
	if f := a.appExec[app]; f != nil {
		return f
	}
	return func(ctx context.Context, stdin string, args ...string) ([]byte, error) {
		argv := []string{"exec", "-i"}
		if user != "" {
			argv = append(argv, "--user", user)
		}
		cmd := exec.CommandContext(ctx, "podman", append(append(argv, quadlet.ContainerName(app, container)), args...)...)
		cmd.Stdin = strings.NewReader(stdin)
		var stderr strings.Builder
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			return nil, fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
		}
		return out, nil
	}
}

// mealieAdmin reads Mealie's admin account with the reverse proxy's secret, one check at a time.
func mealieAdmin(a *app, ctx context.Context, s routes.App, acct account) (string, bool, error) {
	base, err := appBase(s)
	if err != nil {
		return "", false, err
	}
	key, err := os.ReadFile(s.Gate)
	if err != nil {
		return "", false, fmt.Errorf("read the reverse proxy's secret for Mealie: %w", err)
	}
	mu := a.appLock(mealie.Name)
	mu.Lock()
	defer mu.Unlock()
	m := mealie.Accounts{
		Base: base, Secret: strings.TrimSpace(string(key)),
		In: a.inContainer(mealie.Name, mealie.Container, ""), NewPassword: newPassword,
	}
	user, err := m.Admin(ctx, acct.Name, acct.Username)
	return user, false, err
}

// typed is a password of n words for a phone's client (passphrase).
func typed(n int) func() (string, error) {
	return func() (string, error) { return passphrase(n) }
}

// navidromePhone is what a phone's music app signs in to Navidrome with: Navidrome's own
// password, three words once the generated one is replaced.
func navidromePhone(a *app, ctx context.Context, _ bool) (address, user, password string, err error) {
	s, ok := gatedApps(a.routesPath)[navidrome.Name]
	if !ok {
		return "", "", "", errNotInstalled
	}
	address = a.phoneAddress(s)
	mu := a.appLock(navidrome.Name)
	mu.Lock()
	defer mu.Unlock()
	if user, err = a.userIn(ctx, s.Name); err != nil {
		return address, "", "", err
	}
	base, err := appBase(s)
	if err != nil {
		return address, "", "", err
	}
	password, err = navidrome.SignIn(ctx, a.inContainer(navidrome.Name, navidrome.Container, ""), base, user, typed(3))
	return address, user, password, err
}

// freshrssPhone is what a phone's feed reader signs in to FreshRSS with: the Google Reader API's
// own address, as FreshRSS's profile page gives it, and the API password, three words.
func freshrssPhone(a *app, ctx context.Context, replace bool) (address, user, password string, err error) {
	s, ok := gatedApps(a.routesPath)[freshrss.Name]
	if !ok {
		return "", "", "", errNotInstalled
	}
	address = a.phoneAddress(s) + freshrss.API
	mu := a.appLock(freshrss.Name)
	mu.Lock()
	defer mu.Unlock()
	if user, err = a.userIn(ctx, s.Name); err != nil {
		return address, "", "", err
	}
	base, err := appBase(s)
	if err != nil {
		return address, "", "", err
	}
	password, err = freshrss.SignIn(ctx, a.inContainer(freshrss.Name, freshrss.Container, freshrss.User), base, user, replace, typed(3))
	return address, user, password, err
}

// homeAssistantPhone is what Home Assistant's companion app signs in with, on Home Assistant's
// own login page: the owner's username and four kept words.
func homeAssistantPhone(a *app, ctx context.Context, replace bool) (address, user, password string, err error) {
	ha := a.findHomeAssistant()
	if ha == nil {
		return "", "", "", errNotInstalled
	}
	user, password, err = a.homeAssistantSignIn(ctx, ha, replace)
	return a.phoneAddress(ha.app), user, password, err
}
