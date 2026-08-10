// Command reverse-proxy is Briard's front door at the service VIP. It answers plain HTTP
// on :80, terminates TLS on :443, and forwards both to the local service — completing v0's
// deferred half of "reachable by name with TLS" (proved issuance; nothing served it).
//
// It is promoter-owned (runs on the DRBD primary alongside the VIP), reads its cert/key
// from the DRBD volume — so they replicate and survive failover with the data — and
// HOT-RELOADS them when they change on disk. That last property is what lets the renewal
// loop drop a fresh cert in with no restart and no handshake gap: the next TLS handshake
// picks it up. A reload that fails (a half-written cert) keeps serving the last good one,
// so renewal is crash-safe without needing atomic writes.
//
// It answers
// when there is NO backend at all — the shipped state of a fresh node. With -backend empty
// it serves Briard's own page, and /healthz is always its own: a node with nothing routed to it
// is *ready*, not sick, which is what keeps the host agent's health probe honest. With a
// backend it proxies everything through, and /healthz reports the backend's own answer — so
// "the node is healthy" keeps meaning "the service is serving" wherever one is routed.
//
// It speaks only of its own BACKEND, never of the node's service inventory, which it cannot see.
package main

import (
	"crypto/tls"
	"flag"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"sync"
	"time"
)

func main() {
	httpAddr := flag.String("http", ":80", "plain-HTTP listen address (on the VIP)")
	listen := flag.String("listen", ":443", "TLS listen address (on the VIP)")
	backend := flag.String("backend", "", "the local service to proxy to; empty = no backend, serve our own page")
	backendHealth := flag.String("backend-health", "/healthz", "path probed on -backend to answer /healthz")
	certPath := flag.String("cert", "/var/lib/briard/tls/fullchain.pem", "certificate chain PEM (on the DRBD volume)")
	keyPath := flag.String("key", "/var/lib/briard/tls/key.pem", "private key PEM (on the DRBD volume)")
	flag.Parse()

	var backendURL *url.URL
	if *backend != "" {
		u, err := url.Parse(*backend)
		if err != nil {
			log.Fatalf("reverse-proxy: bad -backend %q: %v", *backend, err)
		}
		backendURL = u
	}
	r := &certReloader{certPath: *certPath, keyPath: *keyPath}
	if _, err := r.load(); err != nil {
		// Not fatal: a node may never have a cert (no domain yet), and the renewal loop may
		// issue one after we start. We come up listening; getCertificate serves TLS once the
		// cert appears (handshakes error until then — plain HTTP on -http is unaffected).
		log.Printf("reverse-proxy: no cert yet (%v); listening, will serve once it appears", err)
	}

	h := newFrontDoor(backendURL, *backendHealth)
	// Sane timeout so a stuck client can't pin a connection forever.
	plain := &http.Server{Addr: *httpAddr, Handler: h, ReadHeaderTimeout: 10 * time.Second}
	tlsSrv := &http.Server{
		Addr:              *listen,
		Handler:           h,
		TLSConfig:         &tls.Config{GetCertificate: r.getCertificate},
		ReadHeaderTimeout: 10 * time.Second,
	}
	if backendURL == nil {
		log.Printf("reverse-proxy: serving %s (plain) + %s (TLS, cert %s); no backend configured", *httpAddr, *listen, *certPath)
	} else {
		log.Printf("reverse-proxy: serving %s (plain) + %s (TLS, cert %s) -> %s", *httpAddr, *listen, *certPath, backendURL)
	}
	// Either listener dying is fatal: the front door is promoter-owned, so systemd restarts
	// it on the primary rather than leaving half a door open.
	go func() {
		// Cert/key come from TLSConfig.GetCertificate, so ListenAndServeTLS takes empty paths.
		log.Fatalf("reverse-proxy: %v", tlsSrv.ListenAndServeTLS("", ""))
	}()
	log.Fatalf("reverse-proxy: %v", plain.ListenAndServe())
}

// frontDoor routes the VIP: /healthz is always Briard's own answer, everything else goes to
// the installed service — or, when none is installed, to Briard's page.
//
// Owning /healthz (rather than passing it through) is what kills the zombie state a fresh
// node used to land in: the host agent probed the *payload's* port, so a node with nothing
// installed reported unhealthy forever, and no reflex could tell that apart from a broken
// service. The probe target is now stable across zero and one service, and the answer still
// tracks the service when there is one, because that case forwards the question to it.
type frontDoor struct {
	backend    *url.URL // nil = no backend configured (NOT "no service installed" — see health)
	healthPath string
	proxy      http.Handler
	client     *http.Client
}

func newFrontDoor(backend *url.URL, healthPath string) *frontDoor {
	f := &frontDoor{
		backend:    backend,
		healthPath: healthPath,
		// Short timeout: a health probe that hangs is a failed probe, not a slow one — the
		// agent polls this on a cadence and must not queue up behind a wedged service.
		client: &http.Client{Timeout: 5 * time.Second},
	}
	if backend != nil {
		f.proxy = &httputil.ReverseProxy{
			Rewrite: func(r *httputil.ProxyRequest) {
				r.SetURL(backend)
				// Keep the client's Host. It is what a service uses to build absolute URLs, and
				// it is what routing by domain will key on once a node runs more than one
				// service; rewriting it to 127.0.0.1 throws that away at the door.
				r.Out.Host = r.In.Host
				// Deliberately NO X-Forwarded-*. We do not own the service's configuration, and
				// a service that is not expecting a proxy must keep working when put behind one:
				// Home Assistant answers 400 to a forwarded header from a proxy it has not been
				// told to trust, so sending one would break the flagship service out of the box.
				// (Using Rewrite rather than Director is what makes this the default — Director
				// appends X-Forwarded-For automatically.) Revisit when the front door starts
				// routing by domain and can be paired with documented per-service trust.
			},
		}
	}
	return f
}

func (f *frontDoor) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if req.URL.Path == "/healthz" {
		f.health(w)
		return
	}
	if f.proxy != nil {
		f.proxy.ServeHTTP(w, req)
		return
	}
	// Nothing installed. The root is Briard's own page (a service directory lands here in v5); any other path is honestly a 404 rather than the page under a wrong URL.
	if req.URL.Path != "/" {
		http.NotFound(w, req)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(landingPage))
}

// Health answers the node's readiness. With no backend the node is ready by definition — it is
// running, replicating and able to fail over, which is the whole of what a fresh install
// promises. With one, the service's own health IS the answer.
//
// It says "no backend configured" rather than "no service installed", and the difference is not
// pedantry: THIS PROCESS CANNOT KNOW THE NODE'S SERVICE INVENTORY. Its backend is fixed by the
// flag it was started with (baked at guest-build time from cfg.image), and a service installed at
// RUNTIME never rewires it — so on the shipped zero-service image this branch answers forever, and
// the old wording made it a false statement the moment anyone ran `briard service install`.
// Measured 2026-08-10 on a real node running Home Assistant: `/healthz` was still announcing that
// nothing was installed while HA served on :8123.
//
// The forwarding below is deliberately NOT removed with it. Where a backend exists, "the node is
// healthy" must keep meaning "the service is serving" — that is what the OS health gate and the
// rollback reflex read, and the HA upgrade tests gate on exactly this branch.
func (f *frontDoor) health(w http.ResponseWriter) {
	if f.backend == nil {
		w.Write([]byte("ok: front door up, no backend configured\n"))
		return
	}
	u := *f.backend
	u.Path = f.healthPath
	resp, err := f.client.Get(u.String())
	if err != nil {
		http.Error(w, "service unreachable: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		http.Error(w, "service unhealthy: "+resp.Status, http.StatusServiceUnavailable)
		return
	}
	w.Write([]byte("ok: service healthy\n"))
}

// The no-backend page. Deliberately plain and self-contained (no assets to fetch, nothing
// to break on a node with no internet): a stranger who installs Briard and opens the VIP
// should see that it worked, not a connection refused.
//
// Same correction as health() above, and it matters more here because this is the page a human
// actually reads: it used to assert that no service was installed, which is a claim this process
// has no way to make. The second line exists because of who meets this page — someone who just ran
// `briard service install` and came to the address the docs gave them. Telling them only that
// nothing is routed here reads as a failure; naming the port is the difference between "broken"
// and "expected". It goes when per-domain routing lands and this page stops being what they get.
const landingPage = `<!doctype html>
<html lang="en">
<head><meta charset="utf-8"><title>Briard</title></head>
<body>
<h1>Briard</h1>
<p>This node is running. Nothing is routed to this address yet.</p>
<p>Anything you install here answers on its own port until per-domain routing lands.</p>
</body>
</html>
`

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
