package main

// THE GATE: how the door signs the household into an app that has no login of its own worth
// trusting (a route with a Gate, shared/routes). Files is the live case.
//
// THE SESSION IS THE DASHBOARD'S, carried to the app's own name. A browser arriving at a gated
// app without the dashboard's cookie for that name is sent to the dashboard's /open/<app>, which
// -- on a browser it trusts -- hands back a one-time code for the device's OWN session token. The
// door redeems the code with the dashboard and sets the same token as the app name's cookie, so a
// device is one row on the Trusted devices page and revoking it closes the app too. A browser the
// dashboard does not trust meets the dashboard's ordinary way in, and nothing else.
//
// THE APP IS TOLD WHO IT IS IN A SIGNATURE, NOT A HEADER IT BELIEVES. After the dashboard says the
// token stands (asked over loopback, the answer kept for half a minute), the door forwards the
// request with a token signed with the app's secret (the route's Gate file) naming the household,
// valid for a minute. The app verifies it on every request, so nothing that can reach the app's
// address -- another workload on this node, a LAN host routing to the pod network -- can sign in
// without the key, which only root on this node can read.
//
// THE DASHBOARD'S TOKEN NEVER REACHES THE APP. The cookie is the household's credential for the
// dashboard itself, so the door takes it off every request it forwards.

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"briard.io/shared/casa"
	"briard.io/shared/routes"
)

const (
	// sessionCookie is the dashboard's own cookie, set again on a gated app's name.
	sessionCookie = "briard_session"
	// hopParam carries the one-time code from the dashboard back to the app's name.
	hopParam = "briard_hop"
	// assertionHeader is where the app finds the signed token; assertionQuery is the query
	// parameter it would also read one from, which a client may not use to bring its own.
	assertionHeader = "X-JWT-Assertion"
	assertionQuery  = "jwt"
	// household is who the token names: one household, one person to the app.
	household = "household"
	// assertionLife is how long a signed token is good for: one request's worth, with room for a
	// clock that is a little off.
	assertionLife = time.Minute
	// standsFor is how long the dashboard's word that a token stands is kept. A revoked device
	// is shut out within it.
	standsFor = 30 * time.Second
	// sessionLife matches the dashboard's own cookie.
	sessionLife = 365 * 24 * time.Hour
)

// gate holds what admitting a request needs: the dashboard to ask, and what it last said.
type gate struct {
	dashboard *url.URL // nil: there is no dashboard to ask, so nothing is admitted
	client    *http.Client
	now       func() time.Time

	mu    sync.Mutex
	stood map[string]time.Time // a token's hash -> when the dashboard last said it stands
	keys  map[string]gateKey   // a Gate file -> the secret read from it
}

type gateKey struct {
	mod time.Time
	key []byte
}

func newGate(dashboard *url.URL) *gate {
	return &gate{
		dashboard: dashboard,
		client:    &http.Client{Timeout: 5 * time.Second},
		now:       time.Now,
		stood:     map[string]time.Time{},
		keys:      map[string]gateKey{},
	}
}

// admit decides one request for a gated app. True: the request is rewritten and goes on. False:
// the gate has answered it itself.
func (g *gate) admit(w http.ResponseWriter, req *http.Request, r route) bool {
	if g.dashboard == nil {
		http.Error(w, "this app opens from the Briard dashboard, which is not running here\n", http.StatusServiceUnavailable)
		return false
	}
	q := req.URL.Query()
	if q.Has(hopParam) {
		tok, err := g.redeem(req.Context(), q.Get(hopParam))
		if err != nil {
			log.Printf("reverse-proxy: %s: a sign-in code was refused (%v)", r.name, err)
			http.Error(w, "This link has expired. Open the app again from the Briard dashboard.\n", http.StatusForbidden)
			return false
		}
		http.SetCookie(w, &http.Cookie{
			Name: sessionCookie, Value: tok, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode,
			Secure: req.TLS != nil, Expires: g.now().Add(sessionLife),
		})
		q.Del(hopParam)
		u := *req.URL
		u.RawQuery = q.Encode()
		http.Redirect(w, req, u.RequestURI(), http.StatusSeeOther)
		return false
	}
	tok := ""
	if c, err := req.Cookie(sessionCookie); err == nil {
		tok = c.Value
	}
	if tok == "" || !g.stands(req.Context(), tok) {
		if req.Method != http.MethodGet && req.Method != http.MethodHead {
			// A page's own request after its session went: a redirect would be followed into
			// HTML it cannot use. The next navigation is sent to the dashboard.
			http.Error(w, "open this app again from the Briard dashboard\n", http.StatusUnauthorized)
			return false
		}
		http.Redirect(w, req, openURL(req, r.name), http.StatusSeeOther)
		return false
	}
	key, err := g.key(r.gate)
	if err != nil {
		log.Printf("reverse-proxy: %s: %v", r.name, err)
		http.Error(w, "this app cannot be signed in to right now\n", http.StatusServiceUnavailable)
		return false
	}
	dropCookie(req, sessionCookie)
	if q.Has(assertionQuery) {
		q.Del(assertionQuery)
		req.URL.RawQuery = q.Encode()
	}
	req.Header.Set(assertionHeader, sign(key, household, g.now()))
	return true
}

// openURL is where a browser without a session for an app is sent: the dashboard under the name
// it trusts for this kind of address -- the household's own name for its HTTPS name, the alias
// for every .local one -- asking to open the app where the browser was going.
func openURL(req *http.Request, app string) string {
	host := routes.Normalise(req.Host)
	base := "http://" + routes.BareFlockHostName
	if strings.HasSuffix(host, "."+casa.Zone) && strings.HasPrefix(host, app+".") {
		base = "https://" + strings.TrimPrefix(host, app+".")
	}
	return base + "/open/" + app + "?" + url.Values{"host": {host}, "to": {req.URL.RequestURI()}}.Encode()
}

// redeem trades a one-time code for the device's session token, at the dashboard.
func (g *gate) redeem(ctx context.Context, code string) (string, error) {
	body, status, err := g.ask(ctx, http.MethodPost, "/gate/redeem", url.Values{"code": {code}}, "")
	if err != nil {
		return "", err
	}
	if status != http.StatusOK || strings.TrimSpace(body) == "" {
		return "", fmt.Errorf("the dashboard answered %d", status)
	}
	return strings.TrimSpace(body), nil
}

// stands reports whether a session token is still a trusted device's, from the dashboard's last
// answer while it is fresh.
func (g *gate) stands(ctx context.Context, tok string) bool {
	h := sha256.Sum256([]byte(tok))
	id := hex.EncodeToString(h[:])
	g.mu.Lock()
	at, ok := g.stood[id]
	g.mu.Unlock()
	if ok && g.now().Sub(at) < standsFor {
		return true
	}
	_, status, err := g.ask(ctx, http.MethodGet, "/gate/check", nil, tok)
	if err != nil {
		log.Printf("reverse-proxy: the dashboard could not be asked about a session (%v)", err)
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	for k, t := range g.stood {
		if g.now().Sub(t) >= standsFor {
			delete(g.stood, k)
		}
	}
	if status != http.StatusNoContent {
		delete(g.stood, id)
		return false
	}
	g.stood[id] = g.now()
	return true
}

// ask is one request to the dashboard over loopback.
func (g *gate) ask(ctx context.Context, method, path string, form url.Values, session string) (string, int, error) {
	u := *g.dashboard
	u.Path = path
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return "", 0, err
	}
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if session != "" {
		req.Header.Set("X-Briard-Session", session)
	}
	resp, err := g.client.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return string(b), resp.StatusCode, nil
}

// key is the app's signing secret, read again whenever its file changes.
func (g *gate) key(path string) ([]byte, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("gate secret: %w", err)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if k, ok := g.keys[path]; ok && k.mod.Equal(fi.ModTime()) {
		return k.key, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("gate secret: %w", err)
	}
	k := []byte(strings.TrimSpace(string(raw)))
	if len(k) == 0 {
		return nil, fmt.Errorf("gate secret %s is empty", path)
	}
	g.keys[path] = gateKey{mod: fi.ModTime(), key: k}
	return k, nil
}

// sign is an HS256 JSON Web Token naming sub, issued now and good for assertionLife.
func sign(key []byte, sub string, now time.Time) string {
	enc := base64.RawURLEncoding
	head, _ := json.Marshal(map[string]string{"alg": "HS256", "typ": "JWT"})
	claims, _ := json.Marshal(map[string]any{"sub": sub, "iat": now.Unix(), "exp": now.Add(assertionLife).Unix()})
	unsigned := enc.EncodeToString(head) + "." + enc.EncodeToString(claims)
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(unsigned))
	return unsigned + "." + enc.EncodeToString(mac.Sum(nil))
}

// dropCookie takes one cookie off a request, keeping the rest as the browser sent them.
func dropCookie(req *http.Request, name string) {
	cs := req.Cookies()
	req.Header.Del("Cookie")
	for _, c := range cs {
		if c.Name != name {
			req.AddCookie(c)
		}
	}
}
