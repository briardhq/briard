// Command reverse-proxy is Briard's front door at the service VIP. It answers plain HTTP
// on :80, terminates TLS on :443, and forwards each request to the service the request's
// HOSTNAME names — completing v0's deferred half of "reachable by name with TLS" (proved
// issuance; nothing served it).
//
// It is promoter-owned (runs on the DRBD primary alongside the VIP), reads its cert/key
// from the DRBD volume — so they replicate and survive failover with the data — and
// HOT-RELOADS them when they change on disk. That last property is what lets the renewal
// loop drop a fresh cert in with no restart and no handshake gap: the next TLS handshake
// picks it up. A reload that fails (a half-written cert) keeps serving the last good one,
// so renewal is crash-safe without needing atomic writes.
//
// IT ROUTES FROM A TABLE IT DOES NOT BUILD ([B.48]). `-routes` names the file the guest's converge
// writes at every promotion, install and reboot (shared/routes), and this reloads it by mtime with
// exactly the same discipline as the cert: a table that will not parse keeps the last good one,
// because dropping every route because one write was caught half-done would take a household's
// services down for a file it is about to be able to read.
//
// It does NOT build the table itself out of the volume's manifests, and the reason decides the
// shape of everything here: a service's ADDRESS is a rendering fact, not a manifest fact — the
// manifest names a port, and what host answers on it is whatever the renderer wired the pod for.
// So this process knows names and forwards; it never knows podman.
//
// It answers when NOTHING is routed — the shipped state of a fresh node — with Briard's own page,
// and /healthz is ALWAYS its own: a node with nothing routed to it is *ready*, not sick, which is
// what keeps the host agent's health probe honest. That stays true with N services routed, and it
// is a deliberate reversal of what one backend used to do (forward /healthz to it): with N
// services there is no single answer to forward, and a service that is down must alert without
// making the node it runs on look broken enough to fail over ([V3b.3](f) — a service error
// reports, it never demotes). Per-service health is a separate question, asked per service,
// through this same table.
package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"html"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"briard.io/shared/routes"
)

func main() {
	httpAddr := flag.String("http", ":80", "plain-HTTP listen address (on the VIP)")
	listen := flag.String("listen", ":443", "TLS listen address (on the VIP)")
	routesPath := flag.String("routes", routes.Path, "the routing table written by the guest's converge")
	certPath := flag.String("cert", "/var/lib/briard/tls/fullchain.pem", "certificate chain PEM (on the DRBD volume)")
	keyPath := flag.String("key", "/var/lib/briard/tls/key.pem", "private key PEM (on the DRBD volume)")
	flag.Parse()

	r := &certReloader{certPath: *certPath, keyPath: *keyPath}
	if _, err := r.load(); err != nil {
		// Not fatal: a node may never have a cert (no domain yet), and the renewal loop may
		// issue one after we start. We come up listening; getCertificate serves TLS once the
		// cert appears (handshakes error until then — plain HTTP on -http is unaffected).
		log.Printf("reverse-proxy: no cert yet (%v); listening, will serve once it appears", err)
	}
	tbl := &routeReloader{path: *routesPath}
	// Not fatal either, and for a stronger reason than the cert's: this unit starts after
	// briard-services on the promoter chain, so a missing table means a node that converged to
	// nothing — the shipped state — and refusing to start would replace "no service routed" with
	// "no front door at all", which is the same node with nothing to tell the household.
	if _, err := tbl.load(); err != nil {
		log.Printf("reverse-proxy: no routing table yet (%v); serving Briard's page until one appears", err)
	}

	h := newFrontDoor(tbl.current)
	// Sane timeout so a stuck client can't pin a connection forever.
	plain := &http.Server{Addr: *httpAddr, Handler: h, ReadHeaderTimeout: 10 * time.Second}
	tlsSrv := &http.Server{
		Addr:              *listen,
		Handler:           h,
		TLSConfig:         &tls.Config{GetCertificate: r.getCertificate},
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Printf("reverse-proxy: serving %s (plain) + %s (TLS, cert %s); routes from %s: %s",
		*httpAddr, *listen, *certPath, *routesPath, tbl.current().describe())
	// Either listener dying is fatal: the front door is promoter-owned, so systemd restarts
	// it on the primary rather than leaving half a door open.
	go func() {
		// Cert/key come from TLSConfig.GetCertificate, so ListenAndServeTLS takes empty paths.
		log.Fatalf("reverse-proxy: %v", tlsSrv.ListenAndServeTLS("", ""))
	}()
	log.Fatalf("reverse-proxy: %v", plain.ListenAndServe())
}

// frontDoor routes the VIP: /healthz is always Briard's own answer, a request whose Host names a
// routed service goes to that service, and everything else gets Briard's page.
//
// Owning /healthz (rather than passing it through) is what kills the zombie state a fresh
// node used to land in: the host agent probed the *payload's* port, so a node with nothing
// installed reported unhealthy forever, and no reflex could tell that apart from a broken
// service. The probe target is stable across zero, one and N services — which is the whole
// point of it not being a service's answer.
type frontDoor struct {
	routes func() *routing
	proxy  http.Handler
}

// backendKey carries the resolved backend from ServeHTTP into the proxy's Rewrite hook. One
// ReverseProxy serves every route (so they share http.DefaultTransport's connection pooling and
// the header doctrine below is written once); the per-request target rides the context.
type backendKey struct{}

func newFrontDoor(current func() *routing) *frontDoor {
	f := &frontDoor{routes: current}
	f.proxy = &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(r.In.Context().Value(backendKey{}).(*url.URL))
			// Keep the client's Host. It is what a service uses to build absolute URLs, and
			// it is now also the thing that SELECTED this backend — rewriting it to 127.0.0.1
			// would hand the service a name nobody typed.
			r.Out.Host = r.In.Host
			// Deliberately NO X-Forwarded-*. We do not own the service's configuration, and
			// a service that is not expecting a proxy must keep working when put behind one:
			// Home Assistant answers 400 to a forwarded header from a proxy it has not been
			// told to trust, so sending one would break the flagship service out of the box.
			// (Using Rewrite rather than Director is what makes this the default — Director
			// appends X-Forwarded-For automatically.) Revisit when per-service trust is
			// something the catalog can state and we can pair the header with it.
		},
	}
	return f
}

func (f *frontDoor) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	t := f.routes()
	// THE NAME IS ASKED FIRST, AND /healthz IS NOT AN EXCEPTION TO IT. Under a name we route,
	// every path belongs to that service — /healthz included, because that is the service's own
	// readiness endpoint and the thing its install gate reads. Answering it ourselves would tell a
	// client "the node is fine" to a question asked about a service, which is the same conflation
	// the single baked backend used to make in reverse. The node's own /healthz is reached the way
	// the node itself is: by address, or by any name that is not a service's.
	r, ok := t.lookup(req.Host)
	if !ok {
		if req.URL.Path == "/healthz" {
			f.health(w, t)
			return
		}
		// Nobody's name — the bare IP, a stale name, or a typo. Briard's page, which is where the
		// household finds out what names this node DOES answer to.
		f.page(w, req, t)
		return
	}
	if r.backend == nil {
		// A routed service with no HTTP front door. Nothing in the catalog is this today; MQTT is
		// the first thing that will be, and a proxy cannot serve it. Saying so beats a 404 that
		// reads as "you typed the name wrong".
		http.Error(w, fmt.Sprintf("%s does not answer HTTP; reach it on its own port\n", r.name), http.StatusNotImplemented)
		return
	}
	f.proxy.ServeHTTP(w, req.WithContext(context.WithValue(req.Context(), backendKey{}, r.backend)))
}

// health answers the NODE's readiness, and only ever its own.
//
// It says what it can see — how many services are routed — and never whether they are well. Those
// are different questions with different consequences: this one is read by the OS health gate and
// the rollback reflex, where a wrong answer costs a failover, while "is home-assistant serving" is
// read by a household and an alert. The front door used to conflate them by forwarding /healthz to
// its single backend, and with N services there is no single answer to forward. Per-service health
// is asked per service, against this same table, by the guest's own probe ([B.48]).
//
// A NODE WITH NOTHING ROUTED IS READY. That is the shipped state of a fresh install — running,
// replicating, able to fail over — and calling it unhealthy is the zombie state this replaced.
//
// It NAMES what it routes, which is new and is not a leak: those same names are published on the
// LAN over mDNS by the node itself, so anyone who can reach this port can already enumerate them.
// It earns its place by making the one question a human asks here answerable in one curl — "does
// this node know about my service?" — which was previously only visible by reading a file inside
// the guest.
func (f *frontDoor) health(w http.ResponseWriter, t *routing) {
	fmt.Fprintf(w, "ok: front door up, %s\n", t.describe())
}

// page is Briard's own page: what a bare IP gets, and what an unrecognised name gets.
//
// It is deliberately plain and self-contained (no assets to fetch, nothing to break on a node with
// no internet), and it LISTS THE NAMES THIS NODE ANSWERS TO, because of who meets it — someone who
// just ran `briard service install` and opened the address the docs gave them. The bare IP is
// meant to become the local UI; until that exists, the honest minimum is telling the household
// where their services actually are instead of asserting that nothing is here.
func (f *frontDoor) page(w http.ResponseWriter, req *http.Request, t *routing) {
	// The page belongs at the root, not under every URL a stranger mistypes.
	if req.URL.Path != "/" {
		http.NotFound(w, req)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	var b strings.Builder
	b.WriteString("<!doctype html>\n<html lang=\"en\">\n<head><meta charset=\"utf-8\"><title>Briard</title></head>\n<body>\n<h1>Briard</h1>\n")
	if len(t.table.Services) == 0 {
		b.WriteString("<p>This node is running. Nothing is installed on it yet.</p>\n")
	} else {
		b.WriteString("<p>This node is running. It serves:</p>\n<ul>\n")
		for _, s := range t.table.Services {
			b.WriteString("<li>" + html.EscapeString(s.Name))
			switch {
			case len(s.Hosts) == 0:
				// No name yet: the flock has none, so neither does the service. Say that rather
				// than print a URL that resolves nowhere.
				b.WriteString(" — installed, not yet reachable by name")
			case !s.Fronted || s.Backend == "":
				b.WriteString(" — " + html.EscapeString(s.Hosts[0]) + " (not served over HTTP)")
			default:
				u := "http://" + s.Hosts[0] + "/"
				b.WriteString(` — <a href="` + html.EscapeString(u) + `">` + html.EscapeString(u) + "</a>")
			}
			b.WriteString("</li>\n")
		}
		b.WriteString("</ul>\n")
	}
	b.WriteString("</body>\n</html>\n")
	w.Write([]byte(b.String()))
}

// routing is one loaded routing table, prepared for lookup: hostnames resolved to backends once
// per reload rather than per request, so a request costs a map read and nothing else.
type routing struct {
	table  routes.Table
	byHost map[string]route
}

// route is one resolved destination. A nil backend is a service that is routed by name but not
// served over HTTP — see frontDoor.ServeHTTP.
type route struct {
	name    string
	backend *url.URL
}

func newRouting(t routes.Table) *routing {
	r := &routing{table: t, byHost: make(map[string]route, len(t.Services))}
	for _, s := range t.Services {
		e := route{name: s.Name}
		// A service the table says is NOT fronted keeps its name and gets no backend here. Its
		// address is real and the node probes it; what it must not have is this process forwarding
		// LAN traffic to it (mosquitto's management API is bound to the guest's loopback on
		// purpose, and the door lives inside that guest).
		if s.Fronted && s.Backend != "" {
			u, err := url.Parse(s.Backend)
			if err != nil {
				// One unusable backend must not cost the other services their routes. The name
				// still resolves and says the service is not served, which is true.
				log.Printf("reverse-proxy: %s has an unusable backend %q (%v); routing its name to nothing", s.Name, s.Backend, err)
			} else {
				e.backend = u
			}
		}
		for _, h := range s.Hosts {
			r.byHost[routes.Normalise(h)] = e
		}
	}
	return r
}

func (r *routing) lookup(host string) (route, bool) {
	e, ok := r.byHost[routes.Normalise(host)]
	return e, ok
}

// describe is the one-line summary the log line and /healthz both use.
func (r *routing) describe() string {
	if len(r.table.Services) == 0 {
		return "no services routed"
	}
	names := make([]string, 0, len(r.table.Services))
	for _, s := range r.table.Services {
		names = append(names, s.Name)
	}
	sort.Strings(names)
	return fmt.Sprintf("%d service(s) routed: %s", len(names), strings.Join(names, " "))
}

// routeReloader serves the current routing table, reloading it from disk when the file changes
// (mtime) and keeping the last good one when a reload fails. Deliberately the same shape as
// certReloader, for the same two reasons: converge rewrites this file while we are serving, so a
// restart must not be needed to pick it up; and a read that catches a write half-done must cost
// nothing, because the alternative is dropping every route a household has.
type routeReloader struct {
	path string

	mu     sync.RWMutex
	cached *routing
	mtime  time.Time
}

// current returns the table to route this request against, reloading first if the file changed.
func (r *routeReloader) current() *routing {
	if !r.changed() {
		r.mu.RLock()
		c := r.cached
		r.mu.RUnlock()
		if c != nil {
			return c
		}
	}
	c, err := r.load()
	if err != nil {
		r.mu.RLock()
		prev := r.cached
		r.mu.RUnlock()
		if prev != nil {
			log.Printf("reverse-proxy: routes reload failed, keeping the current table: %v", err)
			return prev
		}
		// Nothing has ever loaded: an empty table, which serves Briard's page. The node is not
		// broken, it simply has nothing to say yet.
		return newRouting(routes.Table{})
	}
	return c
}

func (r *routeReloader) changed() bool {
	fi, err := os.Stat(r.path)
	if err != nil {
		return true
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.cached == nil || !fi.ModTime().Equal(r.mtime)
}

func (r *routeReloader) load() (*routing, error) {
	raw, err := os.ReadFile(r.path)
	if err != nil {
		return nil, err
	}
	t, err := routes.Parse(raw)
	if err != nil {
		return nil, err
	}
	fi, _ := os.Stat(r.path)
	cur := newRouting(t)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cached = cur
	if fi != nil {
		r.mtime = fi.ModTime()
	}
	return cur, nil
}

// certReloader serves the current cert, reloading from disk when the files change (mtime),
// and keeps the last good cert if a reload fails — so a renewal is picked up live without a
// restart, and a half-written cert never drops TLS.
type certReloader struct {
	certPath, keyPath string

	mu       sync.RWMutex
	cached   *tls.Certificate
	certTime time.Time // cert file mtime when cached
	keyTime  time.Time // key file mtime when cached
}

// GetCertificate is the tls.Config.GetCertificate hook (called per handshake): it reloads
// from disk when the cert/key mtimes changed, otherwise serves the cached cert.
func (r *certReloader) getCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	if !r.changed() {
		r.mu.RLock()
		c := r.cached
		r.mu.RUnlock()
		if c != nil {
			return c, nil
		}
	}
	c, err := r.load()
	if err != nil {
		// A failed reload (e.g. a half-written cert mid-renewal) must not drop TLS: keep
		// serving the last good cert if we have one.
		r.mu.RLock()
		prev := r.cached
		r.mu.RUnlock()
		if prev != nil {
			log.Printf("reverse-proxy: cert reload failed, keeping the current cert: %v", err)
			return prev, nil
		}
		return nil, err
	}
	return c, nil
}

// Changed reports whether the cert or key file mtime differs from the cached load (or
// nothing is cached / the files can't be stat'd -> attempt a reload).
func (r *certReloader) changed() bool {
	ci, cerr := os.Stat(r.certPath)
	ki, kerr := os.Stat(r.keyPath)
	if cerr != nil || kerr != nil {
		return true
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.cached == nil || !ci.ModTime().Equal(r.certTime) || !ki.ModTime().Equal(r.keyTime)
}

// Load reads the cert/key from disk and caches them with their mtimes.
func (r *certReloader) load() (*tls.Certificate, error) {
	cert, err := tls.LoadX509KeyPair(r.certPath, r.keyPath)
	if err != nil {
		return nil, err
	}
	ci, _ := os.Stat(r.certPath)
	ki, _ := os.Stat(r.keyPath)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cached = &cert
	if ci != nil {
		r.certTime = ci.ModTime()
	}
	if ki != nil {
		r.keyTime = ki.ModTime()
	}
	return &cert, nil
}
