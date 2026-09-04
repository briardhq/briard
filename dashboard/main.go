// Command dashboard is the HOUSEHOLD DASHBOARD ([V3b.31b]): the page a home opens at its own name,
// served from the guest at the VIP behind the front door, which forwards to it every name it does
// not route. It is the first-open slice of [V3b.31a] -- the one-time code, a trusted device, and
// "Open Home Assistant" landing on Home Assistant's own onboarding page, logged in.
//
// WHAT AUTHENTICATES A BROWSER IS THE CODE THE HOST MINTED, and nothing else. Proof of identity is
// proof of access to the `briard` CLI (whoever can drive it owns the node), so the CLI has the
// agent mint a code, the agent hands it to this guest (shared/dashboard), and the first browser to
// present it becomes a trusted device: a per-device token in an HttpOnly cookie, its hash on the
// replicated volume. No briard password exists; `briard dashboard` re-mints, which is the reset.
// The page lists the trusted devices and revokes one -- from the registry only ([V3b.31f]).
//
// WHY THE BUTTON NEEDS THAT: "Open Home Assistant" creates HA's first user and hands out its
// login. Unauthenticated, that is a LAN race -- whoever reaches the URL first during the install
// window owns the household's Home Assistant. The code closes it.
//
// It is promoter-owned and a chain member like the door, because the registry lives on the
// volume and only the primary has it. It reads the routing table the guest's converge writes,
// which is how it finds Home Assistant -- the same way the door does, so the two cannot disagree
// about where a service is.
//
// DELIBERATELY PLAIN: templates and a stylesheet, no build step, nothing fetched from anywhere --
// it has to render on a node with no internet, and it is a supervisor next to Home Assistant, not
// a rival to it ([V3b.31a](g)).
package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"log"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"briard.io/agent/hass"
	"briard.io/shared/dashboard"
	"briard.io/shared/routes"
)

//go:embed page.html
var pageFS embed.FS

var page = template.Must(template.ParseFS(pageFS, "page.html"))

func main() {
	listen := flag.String("listen", "127.0.0.1:8087", "listen address (loopback: the front door forwards to it)")
	routesPath := flag.String("routes", routes.Path, "the routing table written by the guest's converge")
	handoffPath := flag.String("handoff", dashboard.HandoffPath, "the one-time code the host handed over")
	statePath := flag.String("state", "/var/lib/briard/dashboard", "device registry + the handed-over account (on the volume)")
	tokenPath := flag.String("hass-token", hass.TokenPath, "the control channel's Home Assistant token")
	adminPortPath := flag.String("admin-port", dashboard.AdminPortDev, "the guest end of the host's admin port (a service install rides it)")
	layersPath := flag.String("layers", defaultPullPaths.layers, "podman's layer store, for pull progress")
	tmpPath := flag.String("pull-tmp", defaultPullPaths.tmp, "where the pull units' PrivateTmp roots live, for pull progress")
	flag.Parse()
	a := newApp(*routesPath, *handoffPath, *statePath, *tokenPath)
	a.port = &serialPort{path: *adminPortPath}
	a.pulls = pullPaths{records: dashboard.Dir, layers: *layersPath, tmp: *tmpPath}
	srv := &http.Server{Addr: *listen, Handler: a, ReadHeaderTimeout: 10 * time.Second}
	log.Printf("dashboard: serving %s; routes from %s, state at %s", *listen, *routesPath, *statePath)
	log.Fatalf("dashboard: %v", srv.ListenAndServe())
}

// app is the dashboard: four files it reads and one directory it owns.
type app struct {
	routesPath, handoffPath, statePath, tokenPath string
	now                                           func() time.Time
	// mu serialises the things that must not race: consuming the code, writing the registry,
	// and the pending quick-connect requests. Everything else is read-mostly.
	mu sync.Mutex
	// pending is quick-connect ([V3b.31g]): the devices asking to be let in, by the code each
	// one shows. In memory on purpose -- a request outlives neither its five minutes nor this
	// process, and the volume keeps only what is trusted.
	pending map[string]*pending
	// port is the host's admin port ([V3b.31i]); installs is what was asked through it, by
	// service, from the click until the routes table lists the service.
	port     adminPort
	installs map[string]*install
	// pulls is where a pull's progress is read from ([V3b.31j]): podman's layer store and the
	// pull units' private tmp.
	pulls pullPaths
}

func newApp(routesPath, handoffPath, statePath, tokenPath string) *app {
	return &app{routesPath: routesPath, handoffPath: handoffPath, statePath: statePath, tokenPath: tokenPath, now: time.Now,
		pending: map[string]*pending{}, installs: map[string]*install{}, port: &serialPort{path: dashboard.AdminPortDev}, pulls: defaultPullPaths}
}

const (
	cookieName  = "briard_session"
	joinCookie  = "briard_join"
	devicesFile = "devices.json"
	accountFile = "account.json"
	// joinTTL bounds a quick-connect request; maxPending bounds how many may wait at once, so a
	// stranger spamming the form fills a small table and nothing else.
	joinTTL    = 5 * time.Minute
	maxPending = 8
)

func (a *app) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/healthz":
		fmt.Fprint(w, "ok: dashboard up\n")
	case r.URL.Path == "/" && r.Method == http.MethodGet:
		if code := r.URL.Query().Get("code"); code != "" {
			a.redeem(w, r, code)
			return
		}
		d, ok := a.session(r)
		if !ok {
			a.refuse(w)
			return
		}
		a.render(w, r, d)
	case r.URL.Path == "/open/home-assistant" && r.Method == http.MethodPost:
		if _, ok := a.session(r); !ok {
			a.refuse(w)
			return
		}
		a.openHomeAssistant(w, r)
	case r.URL.Path == "/devices/revoke" && r.Method == http.MethodPost:
		if _, ok := a.session(r); !ok {
			a.refuse(w)
			return
		}
		a.revoke(w, r)
	case strings.HasPrefix(r.URL.Path, "/install/") && r.Method == http.MethodPost:
		if _, ok := a.session(r); !ok {
			a.refuse(w)
			return
		}
		a.requestInstall(w, r, strings.TrimPrefix(r.URL.Path, "/install/"))
	case r.URL.Path == "/join" && r.Method == http.MethodPost:
		a.ask(w, r)
	case r.URL.Path == "/join" && r.Method == http.MethodGet:
		a.wait(w, r)
	case r.URL.Path == "/devices/approve" && r.Method == http.MethodPost:
		if _, ok := a.session(r); !ok {
			a.refuse(w)
			return
		}
		a.approve(w, r)
	default:
		http.NotFound(w, r)
	}
}

// refuse is what a browser with no trusted session gets: no information, one instruction. The
// page says where a code comes from and nothing about what is installed, because reaching this
// port is not authentication ([V3b.31a](a)).
func (a *app) refuse(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusUnauthorized)
	_ = page.ExecuteTemplate(w, "refused", nil)
}

// redeem turns the one-time code into a trusted device. The code is compared in constant time,
// consumed on success (the file goes), and refused past its TTL. A wrong code does not consume
// the right one: at 256 bits nobody is guessing it, and burning it would let a stranger deny the
// household its own bootstrap by spamming the URL.
func (a *app) redeem(w http.ResponseWriter, r *http.Request, code string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	raw, err := os.ReadFile(a.handoffPath)
	if err != nil {
		http.Error(w, "no code is outstanding; run `briard dashboard` on the node to get one\n", http.StatusForbidden)
		return
	}
	var h dashboard.Handoff
	if err := json.Unmarshal(raw, &h); err != nil {
		http.Error(w, "the handoff does not parse; run `briard dashboard` again\n", http.StatusForbidden)
		return
	}
	if h.Expired(a.now()) {
		_ = os.Remove(a.handoffPath)
		http.Error(w, "that code has expired; run `briard dashboard` on the node for a fresh one\n", http.StatusForbidden)
		return
	}
	if subtle.ConstantTimeCompare([]byte(h.Code), []byte(code)) != 1 {
		http.Error(w, "that is not the code\n", http.StatusForbidden)
		return
	}
	if err := os.Remove(a.handoffPath); err != nil {
		log.Printf("dashboard: consume handoff: %v", err)
		http.Error(w, "could not consume the code\n", http.StatusInternalServerError)
		return
	}
	// What the host knew about the OS account outlives the code: Home Assistant's first user is
	// made from it, possibly minutes later, once HA is running.
	h.Code = ""
	if err := a.writeState(accountFile, h); err != nil {
		log.Printf("dashboard: keep account: %v", err)
	}
	tok, err := newSecret()
	if err != nil {
		http.Error(w, "could not mint a session\n", http.StatusInternalServerError)
		return
	}
	if err := a.addDevice(tok, r.UserAgent()); err != nil {
		log.Printf("dashboard: register device: %v", err)
		http.Error(w, "could not register this device\n", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: tok, Path: "/",
		HttpOnly: true, SameSite: http.SameSiteLaxMode,
		// The door terminates TLS and says so on this one path; a cookie marked Secure over
		// plain http would never come back.
		Secure:  scheme(r) == "https",
		Expires: a.now().Add(365 * 24 * time.Hour),
	})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// session says whether the request carries a registered device's token, and which device.
func (a *app) session(r *http.Request) (device, bool) {
	c, err := r.Cookie(cookieName)
	if err != nil || c.Value == "" {
		return device{}, false
	}
	var reg registry
	if err := a.readState(devicesFile, &reg); err != nil {
		return device{}, false
	}
	want := hashToken(c.Value)
	for _, d := range reg.Devices {
		if subtle.ConstantTimeCompare([]byte(d.Hash), []byte(want)) == 1 {
			return d, true
		}
	}
	return device{}, false
}

// registry is the trusted-device list, on the volume so it follows the VIP. Tokens are stored
// hashed: the file replicates and gets backed up, and a hash is all a check needs.
type registry struct {
	Devices []device `json:"devices"`
}

type device struct {
	ID      string    `json:"id"`
	Hash    string    `json:"hash"`
	Agent   string    `json:"agent,omitempty"`
	Created time.Time `json:"created"`
}

func (a *app) addDevice(tok, agent string) error {
	var reg registry
	if err := a.readState(devicesFile, &reg); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	id, err := newSecret()
	if err != nil {
		return err
	}
	reg.Devices = append(reg.Devices, device{ID: id[:16], Hash: hashToken(tok), Agent: agent, Created: a.now()})
	return a.writeState(devicesFile, reg)
}

// revoke takes a device out of the registry, and out of nothing else ([V3b.31a](a), decided
// 2026-09-04): the Home Assistant session that device already holds is HA's to end, in HA's own
// profile -- a minted code becomes a refresh token that carries no per-device handle, so there
// is nothing here to look it up by, and the page says so next to the list. A device revoking
// ITSELF is a sign-out: the cookie goes with it. The last device may go too -- an empty
// registry is the state the install starts in, and `briard dashboard` is the way back.
func (a *app) revoke(w http.ResponseWriter, r *http.Request) {
	id := r.FormValue("id")
	a.mu.Lock()
	defer a.mu.Unlock()
	var reg registry
	if err := a.readState(devicesFile, &reg); err != nil {
		http.Error(w, "no device registry\n", http.StatusInternalServerError)
		return
	}
	// An empty list, not null: the last device leaving must read as "no devices" to every
	// consumer of the file, not as a registry that was never written.
	kept := make([]device, 0, len(reg.Devices))
	var gone *device
	for i := range reg.Devices {
		if reg.Devices[i].ID == id && gone == nil {
			gone = &reg.Devices[i]
			continue
		}
		kept = append(kept, reg.Devices[i])
	}
	if gone == nil {
		http.Error(w, "no such device\n", http.StatusNotFound)
		return
	}
	reg.Devices = kept
	if err := a.writeState(devicesFile, reg); err != nil {
		log.Printf("dashboard: revoke device: %v", err)
		http.Error(w, "could not revoke the device\n", http.StatusInternalServerError)
		return
	}
	if c, err := r.Cookie(cookieName); err == nil && hashToken(c.Value) == gone.Hash {
		http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: scheme(r) == "https", MaxAge: -1})
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// QUICK-CONNECT ([V3b.31g], [V3b.31a](a)): a browser that is not trusted asks; a browser that is
// lets it in. The new device SHOWS a six-digit code, and the household TYPES that code on any
// trusted device. That binds the approval to the device in the person's hand, not to "the
// request that is pending": a stranger asking at the same moment has a different code, so
// nothing can be approved by accident, and no request is ever approved without a human on a
// trusted device acting. The code is a SELECTOR, never a credential -- the session goes to
// whoever holds the request's own cookie, so a code read off the LAN collects nothing. Nothing
// here auto-approves, and reaching the form is not authentication: it produces, at most, a
// number on the asker's own screen.
type pending struct {
	code   string    // what the asker shows and the approver types
	secret string    // hash of the asker's cookie: the only thing that collects the approval
	agent  string    // the asker's user agent, the device's label once trusted
	asked  time.Time // the clock the TTL runs on, approved or not
	token  string    // the session token, from approval until the asker collects it
}

// ask opens a request: a code for the screen, a cookie for the collection, a bound on how many
// may wait. Called with the lock NOT held.
func (a *app) ask(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sweep()
	if len(a.pending) >= maxPending {
		http.Error(w, "too many devices are asking right now; try again in a few minutes\n", http.StatusTooManyRequests)
		return
	}
	secret, err := newSecret()
	if err != nil {
		http.Error(w, "could not open a request\n", http.StatusInternalServerError)
		return
	}
	var code string
	for {
		if code, err = newJoinCode(); err != nil {
			http.Error(w, "could not open a request\n", http.StatusInternalServerError)
			return
		}
		if _, taken := a.pending[code]; !taken {
			break
		}
	}
	a.pending[code] = &pending{code: code, secret: hashToken(secret), agent: r.UserAgent(), asked: a.now()}
	http.SetCookie(w, &http.Cookie{
		Name: joinCookie, Value: secret, Path: "/join",
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: scheme(r) == "https",
		MaxAge: int(joinTTL / time.Second),
	})
	http.Redirect(w, r, "/join", http.StatusSeeOther)
}

// wait is the asker's page: the code while nobody has approved, the session once somebody has
// (the request is spent on collection), and "ask again" once it has expired. The page refreshes
// itself; no script.
func (a *app) wait(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sweep()
	var p *pending
	if c, err := r.Cookie(joinCookie); err == nil && c.Value != "" {
		want := hashToken(c.Value)
		for _, q := range a.pending {
			if subtle.ConstantTimeCompare([]byte(q.secret), []byte(want)) == 1 {
				p = q
			}
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	switch {
	case p == nil:
		w.WriteHeader(http.StatusGone)
		_ = page.ExecuteTemplate(w, "join", joinView{Expired: true})
	case p.token != "":
		delete(a.pending, p.code)
		http.SetCookie(w, &http.Cookie{Name: joinCookie, Value: "", Path: "/join", HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: scheme(r) == "https", MaxAge: -1})
		http.SetCookie(w, &http.Cookie{
			Name: cookieName, Value: p.token, Path: "/",
			HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: scheme(r) == "https",
			Expires: a.now().Add(365 * 24 * time.Hour),
		})
		http.Redirect(w, r, "/", http.StatusSeeOther)
	default:
		_ = page.ExecuteTemplate(w, "join", joinView{Code: p.code[:3] + " " + p.code[3:]})
	}
}

type joinView struct {
	Code    string
	Expired bool
}

// approve is the trusted device letting one in: the typed code names the request, the device is
// registered NOW under the asker's agent, and the token waits for the asker to collect it. A
// code nobody is showing is refused by name; a request already approved cannot be approved twice
// (its code is gone with it once collected, and until then it already holds its token).
func (a *app) approve(w http.ResponseWriter, r *http.Request) {
	code := strings.Map(func(c rune) rune {
		if c >= '0' && c <= '9' {
			return c
		}
		return -1
	}, r.FormValue("code"))
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sweep()
	p, ok := a.pending[code]
	if !ok || p.token != "" {
		http.Error(w, "no device is showing that code; check the new device's screen and try again\n", http.StatusNotFound)
		return
	}
	tok, err := newSecret()
	if err != nil {
		http.Error(w, "could not mint a session\n", http.StatusInternalServerError)
		return
	}
	if err := a.addDevice(tok, p.agent); err != nil {
		log.Printf("dashboard: register approved device: %v", err)
		http.Error(w, "could not register the device\n", http.StatusInternalServerError)
		return
	}
	p.token = tok
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// sweep drops requests past their TTL, approved or not. Called with the lock held.
func (a *app) sweep() {
	for code, p := range a.pending {
		if a.now().Sub(p.asked) > joinTTL {
			delete(a.pending, code)
		}
	}
}

// newJoinCode is six digits: typed by a person on another device, so short and unambiguous;
// a selector among at most maxPending requests, so six is plenty.
func newJoinCode() (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(1_000_000))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%06d", n), nil
}

func hashToken(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:])
}

func newSecret() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// readState / writeState are the dashboard's own directory on the volume: 0700, files 0600,
// written whole and renamed into place so a crash mid-write leaves the last good file.
func (a *app) readState(name string, v any) error {
	raw, err := os.ReadFile(filepath.Join(a.statePath, name))
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, v)
}

func (a *app) writeState(name string, v any) error {
	if err := os.MkdirAll(a.statePath, 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(a.statePath, name)
	tmp := path + ".new"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// homeAssistant is what the routing table says about Home Assistant on this node: where the
// guest reaches it, and the name the household does.
type homeAssistant struct {
	base string // in-guest, e.g. http://127.0.0.1:8123
	port int
	host string // the mDNS name the door routes, e.g. briard-brave-elf-home-assistant.local
}

// findHomeAssistant reads the table the way the door does. nil means not installed (or not yet
// converged), which is a state the page shows rather than an error.
func (a *app) findHomeAssistant() *homeAssistant {
	raw, err := os.ReadFile(a.routesPath)
	if err != nil {
		return nil
	}
	t, err := routes.Parse(raw)
	if err != nil {
		log.Printf("dashboard: routes: %v", err)
		return nil
	}
	for _, s := range t.Services {
		if s.Name != hass.Name {
			continue
		}
		rt, ok := s.Route(routes.ListenName)
		if !ok || len(s.Hosts) == 0 {
			return nil
		}
		u, err := s.Resolve(rt.To)
		if err != nil {
			return nil
		}
		port, _ := strconv.Atoi(u.Port())
		return &homeAssistant{base: u.String(), port: port, host: s.Hosts[0]}
	}
	return nil
}

// view is what the page renders.
type view struct {
	Services []string
	HA       *haView
	Devices  []deviceView
	// Install is a Home Assistant install the household asked for from this page, while it
	// runs or after it failed; Refresh makes the page poll itself while something is moving.
	Install *installView
	Refresh bool
}

// deviceView is one row of the trusted-device list: a label a person can tell apart, when it
// was added, and whether it is the browser looking at the page.
type deviceView struct {
	ID, Label, Agent string
	Added            string
	This             bool
}

// deviceLabel is the browser and the OS, read off the user agent the device registered with --
// enough to tell the lost laptop from the phone in hand, never a fingerprint. Unknown shapes
// keep the raw agent, which the row shows on hover anyway.
func deviceLabel(ua string) string {
	pick := func(pairs ...string) string {
		for i := 0; i+1 < len(pairs); i += 2 {
			if strings.Contains(ua, pairs[i]) {
				return pairs[i+1]
			}
		}
		return ""
	}
	browser := pick("Firefox", "Firefox", "Edg", "Edge", "OPR", "Opera", "SamsungBrowser", "Samsung Internet", "Chrome", "Chrome", "Safari", "Safari", "curl", "curl")
	os := pick("Windows", "Windows", "Android", "Android", "iPhone", "iPhone", "iPad", "iPad", "Mac OS", "Mac", "CrOS", "ChromeOS", "Linux", "Linux")
	switch {
	case browser != "" && os != "":
		return browser + " on " + os
	case browser != "":
		return browser
	case os != "":
		return os
	case ua == "":
		return "Unknown device"
	}
	return ua
}

type haView struct {
	Host      string
	URL       string
	Reachable bool // answers HTTP
	Running   bool // HA's own RUNNING state -- the boundary an action may be taken on ([B.127])
	Onboarded bool // every onboarding step done
}

func (a *app) render(w http.ResponseWriter, r *http.Request, self device) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	v := view{}
	var reg registry
	if err := a.readState(devicesFile, &reg); err == nil {
		for _, d := range reg.Devices {
			v.Devices = append(v.Devices, deviceView{ID: d.ID, Label: deviceLabel(d.Agent), Agent: d.Agent, Added: d.Created.Format("2 Jan 2006"), This: d.ID == self.ID})
		}
	}
	if raw, err := os.ReadFile(a.routesPath); err == nil {
		if t, err := routes.Parse(raw); err == nil {
			for _, s := range t.Services {
				v.Services = append(v.Services, s.Name)
			}
		}
	}
	if ha := a.findHomeAssistant(); ha != nil {
		hv := &haView{Host: ha.host, URL: scheme(r) + "://" + ha.host + "/"}
		if steps, err := hass.OnboardingSteps(ctx, ha.base); err == nil {
			hv.Reachable = true
			hv.Onboarded = steps.Done()
			if _, access, err := hass.SystemAccess(ctx, a.exec(), ha.port); err == nil {
				if st, err := hass.CoreState(ctx, ha.base, access); err == nil {
					hv.Running = st == "RUNNING"
				}
			}
		}
		v.HA = hv
	}
	v.Install = a.installState(hass.Name, v.HA != nil)
	// Poll while something is on its way: an install in flight, or a Home Assistant that is
	// routed but not yet RUNNING.
	v.Refresh = (v.Install != nil && v.Install.Running) || (v.HA != nil && !v.HA.Running)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := page.ExecuteTemplate(w, "page", v); err != nil {
		log.Printf("dashboard: render: %v", err)
	}
}

// openHomeAssistant is the button. By HA's own onboarding state:
//   - user step not done: create the first user from the account the host handed over, mark
//     analytics done with the control channel's token (the user's code is single-use and
//     belongs to the browser), and send the browser to HA's onboarding page with that code --
//     it resumes at the location step, then discovered devices, then logs in ([V3b.31a](d),
//     measured in (f));
//   - user step done: MINT a login for HA's owner through the in-HA integration ([V3b.31d]) and
//     send the browser to HA's own auth callback -- its front page once onboarding is done,
//     the onboarding page (which resumes) while it is not. Minted at click time, so the code's
//     ten minutes are never in play. Every trusted device is the owner: the unit is the device,
//     not the person ([V3b.31a](a)).
//
// The first open refuses while HA is not RUNNING: a Home Assistant serving HTTP is not yet one
// that will act on a user step, and the code it returned would be for a user in a store that is
// still loading. The mint needs only the auth manager and refuses on its own.
func (a *app) openHomeAssistant(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	ha := a.findHomeAssistant()
	if ha == nil {
		http.Error(w, "Home Assistant is not installed on this node\n", http.StatusNotFound)
		return
	}
	origin := scheme(r) + "://" + ha.host
	steps, err := hass.OnboardingSteps(ctx, ha.base)
	if err != nil {
		http.Error(w, "Home Assistant is not answering yet; try again in a moment\n", http.StatusServiceUnavailable)
		return
	}
	_, access, err := hass.SystemAccess(ctx, a.exec(), ha.port)
	if err != nil {
		http.Error(w, "Home Assistant's control channel is not up yet; try again in a moment\n", http.StatusServiceUnavailable)
		return
	}
	if steps[hass.StepUser] {
		a.openAsOwner(ctx, w, r, ha, origin, access, steps.Done())
		return
	}
	if st, err := hass.CoreState(ctx, ha.base, access); err != nil || st != "RUNNING" {
		http.Error(w, "Home Assistant is still starting; try again in a moment\n", http.StatusConflict)
		return
	}
	var acct dashboard.Handoff
	if err := a.readState(accountFile, &acct); err != nil {
		log.Printf("dashboard: no account from the handoff (%v); using defaults", err)
	}
	pw, err := newPassword()
	if err != nil {
		http.Error(w, "could not generate a password\n", http.StatusInternalServerError)
		return
	}
	u := hass.NewUser{
		Name: or(acct.Name, "Home"), Username: or(acct.Username, "home"), Password: pw,
		ClientID: hass.ClientID(origin), Language: or(acct.Language, "en"),
	}
	// The password is GENERATED AND FORGOTTEN ([V3b.31e]): every later open is minted, so no
	// session ever depends on it, and the household sets its own -- for the companion app -- in
	// Home Assistant's People settings, where the owner needs no old password. briard keeps no
	// copy: one it could not keep current would only ever lie.
	code, err := hass.CreateUser(ctx, ha.base, u)
	if err != nil {
		log.Printf("dashboard: user step: %v", err)
		http.Error(w, "Home Assistant refused the first user; it may already be set up\n", http.StatusBadGateway)
		return
	}
	if err := hass.MarkAnalytics(ctx, ha.base, access); err != nil {
		// Not fatal: HA then shows its analytics page, whose toggles default to off.
		log.Printf("dashboard: analytics step: %v (HA will ask; the defaults are off)", err)
	}
	http.Redirect(w, r, hass.OnboardingURL(origin, code), http.StatusSeeOther)
}

// openAsOwner is every open after the first: a code minted for HA's owner, and HA's own callback.
// A refusal is SURFACED, with the plain address as the way in: the minter mints for the owner
// and nobody else, so with no owner (deleted -- measured, [V3b.31a](f)4) the household logs in
// by hand, and a Home Assistant our integration did not load on gets its own login screen.
func (a *app) openAsOwner(ctx context.Context, w http.ResponseWriter, r *http.Request, ha *homeAssistant, origin, access string, done bool) {
	landing := origin + "/onboarding.html"
	if done {
		landing = origin + "/"
	}
	code, err := hass.MintLogin(ctx, ha.base, access, hass.ClientID(origin))
	switch {
	case errors.Is(err, hass.ErrNoOwner):
		http.Error(w, "Home Assistant has no owner account to log you in as; open "+landing+" and log in with a password\n", http.StatusConflict)
		return
	case errors.Is(err, hass.ErrNoMinter):
		http.Error(w, "this Home Assistant is not running the briard integration; open "+landing+" and log in with a password\n", http.StatusBadGateway)
		return
	case err != nil:
		log.Printf("dashboard: mint login: %v", err)
		http.Error(w, "Home Assistant did not issue a login; open "+landing+" and log in with a password\n", http.StatusBadGateway)
		return
	}
	if done {
		http.Redirect(w, r, hass.LoginURL(origin, code), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, hass.OnboardingURL(origin, code), http.StatusSeeOther)
}

// scheme is how the BROWSER reached us: the door terminates TLS and says so on its forward to
// the dashboard, and to the dashboard alone.
func scheme(r *http.Request) string {
	if r.Header.Get("X-Forwarded-Proto") == "https" {
		return "https"
	}
	return "http"
}

func or(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// newPassword is a starting credential: 20 characters from a set with no look-alikes, shown to
// the household and meant to be changed. Never invented-and-hidden.
func newPassword() (string, error) {
	const alphabet = "abcdefghjkmnpqrstuvwxyz23456789"
	var b [20]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(b[:]), nil
}

// fileExec is the hass.Executor the dashboard hands the control-channel exchange: it can read
// files and nothing else, and the one file that path reads is mapped to -hass-token.
type fileExec struct{ token string }

func (a *app) exec() hass.Executor { return fileExec{token: a.tokenPath} }

func (e fileExec) ReadFile(path string) ([]byte, error) {
	if path == hass.TokenPath {
		path = e.token
	}
	return os.ReadFile(path)
}

func (fileExec) WriteFile(string, []byte) error {
	return errors.New("dashboard: does not write files through the executor")
}

func (fileExec) Run(context.Context, string, ...string) ([]byte, error) {
	return nil, errors.New("dashboard: does not run commands")
}
