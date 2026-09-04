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
	"net/http"
	"os"
	"path/filepath"
	"strconv"
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
	statePath := flag.String("state", "/var/lib/briard/dashboard", "device registry + Home Assistant's starting password (on the volume)")
	tokenPath := flag.String("hass-token", hass.TokenPath, "the control channel's Home Assistant token")
	flag.Parse()
	a := newApp(*routesPath, *handoffPath, *statePath, *tokenPath)
	srv := &http.Server{Addr: *listen, Handler: a, ReadHeaderTimeout: 10 * time.Second}
	log.Printf("dashboard: serving %s; routes from %s, state at %s", *listen, *routesPath, *statePath)
	log.Fatalf("dashboard: %v", srv.ListenAndServe())
}

// app is the dashboard: four files it reads and one directory it owns.
type app struct {
	routesPath, handoffPath, statePath, tokenPath string
	now                                           func() time.Time
	// mu serialises the two things that must not race: consuming the code, and writing the
	// registry. Everything else is read-mostly.
	mu sync.Mutex
}

func newApp(routesPath, handoffPath, statePath, tokenPath string) *app {
	return &app{routesPath: routesPath, handoffPath: handoffPath, statePath: statePath, tokenPath: tokenPath, now: time.Now}
}

const (
	cookieName   = "briard_session"
	devicesFile  = "devices.json"
	accountFile  = "account.json"
	passwordFile = "home-assistant-password"
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
		if !a.trusted(r) {
			a.refuse(w)
			return
		}
		a.render(w, r)
	case r.URL.Path == "/open/home-assistant" && r.Method == http.MethodPost:
		if !a.trusted(r) {
			a.refuse(w)
			return
		}
		a.openHomeAssistant(w, r)
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

// trusted says whether the request carries a registered device's token.
func (a *app) trusted(r *http.Request) bool {
	c, err := r.Cookie(cookieName)
	if err != nil || c.Value == "" {
		return false
	}
	var reg registry
	if err := a.readState(devicesFile, &reg); err != nil {
		return false
	}
	want := hashToken(c.Value)
	for _, d := range reg.Devices {
		if subtle.ConstantTimeCompare([]byte(d.Hash), []byte(want)) == 1 {
			return true
		}
	}
	return false
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
}

type haView struct {
	Host      string
	URL       string
	Reachable bool   // answers HTTP
	Running   bool   // HA's own RUNNING state -- the boundary an action may be taken on ([B.127])
	Onboarded bool   // every onboarding step done
	Password  string // the starting password, while the dashboard cannot mint a login (interim)
}

func (a *app) render(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	v := view{}
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
		if raw, err := os.ReadFile(filepath.Join(a.statePath, passwordFile)); err == nil {
			hv.Password = string(raw)
		}
		v.HA = hv
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := page.ExecuteTemplate(w, "page", v); err != nil {
		log.Printf("dashboard: render: %v", err)
	}
}

// openHomeAssistant is the button. By HA's own onboarding state:
//   - not started: create the first user from the account the host handed over, mark analytics
//     done with the control channel's token (the user's code is single-use and belongs to the
//     browser), and send the browser to HA's onboarding page with that code -- it resumes at the
//     location step, then discovered devices, then logs in ([V3b.31a](d), measured in (f));
//   - started but unfinished: HA's onboarding page, which resumes with the tokens the browser
//     kept, or asks for the password the card shows;
//   - done: Home Assistant itself. (A minted login for later opens is the in-HA integration's
//     job, not this slice's.)
//
// It refuses while HA is not RUNNING: a Home Assistant serving HTTP is not yet one that will act
// on a user step, and the code it returned would be for a user in a store that is still loading.
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
	if steps.Done() {
		http.Redirect(w, r, origin+"/", http.StatusSeeOther)
		return
	}
	if steps[hass.StepUser] {
		http.Redirect(w, r, origin+"/onboarding.html", http.StatusSeeOther)
		return
	}
	_, access, err := hass.SystemAccess(ctx, a.exec(), ha.port)
	if err != nil {
		http.Error(w, "Home Assistant's control channel is not up yet; try again in a moment\n", http.StatusServiceUnavailable)
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
	// The password is kept BEFORE the user exists: a user made with a password nobody can read is
	// the lockout [V3b.31] warns about, and the companion app logs in with exactly this.
	if err := os.MkdirAll(a.statePath, 0o700); err == nil {
		if err := os.WriteFile(filepath.Join(a.statePath, passwordFile), []byte(pw), 0o600); err != nil {
			log.Printf("dashboard: keep password: %v", err)
		}
	}
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
