package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// get issues a GET against a test server and returns the status and body.
func get(t *testing.T, base, path string) (int, string) {
	t.Helper()
	resp, err := http.Get(base + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

// The shipped state of a fresh node: nothing installed. The VIP must still ANSWER — Briard's
// own page at the root — and /healthz must say the node is ready rather than sick.
// A node reporting unhealthy forever because no service was ever installed is the zombie
// state this replaces.
func TestZeroServiceFrontDoor(t *testing.T) {
	srv := httptest.NewServer(newFrontDoor(nil, "/healthz"))
	defer srv.Close()

	if code, body := get(t, srv.URL, "/healthz"); code != http.StatusOK || !strings.Contains(body, "no service installed") {
		t.Errorf("/healthz with nothing installed = %d %q, want 200 saying no service is installed", code, body)
	}
	if code, body := get(t, srv.URL, "/"); code != http.StatusOK || !strings.Contains(body, "No service is installed") {
		t.Errorf("/ with nothing installed = %d %q, want 200 and Briard's page", code, body)
	}
	// The page belongs at the root, not under every URL a stranger mistypes.
	if code, _ := get(t, srv.URL, "/nope"); code != http.StatusNotFound {
		t.Errorf("/nope with nothing installed = %d, want 404", code)
	}
}

// With a service installed, "the node is healthy" must keep meaning "the service is
// serving": /healthz forwards the question to the backend and reports its answer. This is
// what stops the widened front door from papering over a wedged service — the broken-upgrade
// fixture (BRIARD_BROKEN) holds /healthz at 503 forever, and the health gate depends on
// seeing that through the proxy.
func TestHealthTracksTheBackend(t *testing.T) {
	var healthy atomic.Bool
	healthy.Store(true)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" && !healthy.Load() {
			http.Error(w, "starting", http.StatusServiceUnavailable)
			return
		}
		fmt.Fprint(w, "payload-ok")
	}))
	bu, _ := url.Parse(backend.URL)
	srv := httptest.NewServer(newFrontDoor(bu, "/healthz"))
	defer srv.Close()

	if code, body := get(t, srv.URL, "/healthz"); code != http.StatusOK || !strings.Contains(body, "healthy") {
		t.Errorf("/healthz on a serving backend = %d %q, want 200", code, body)
	}
	// Everything that is not /healthz belongs to the service.
	if code, body := get(t, srv.URL, "/state"); code != http.StatusOK || body != "payload-ok" {
		t.Errorf("/state = %d %q, want it proxied to the service", code, body)
	}
	healthy.Store(false)
	if code, _ := get(t, srv.URL, "/healthz"); code != http.StatusServiceUnavailable {
		t.Errorf("/healthz on an unhealthy backend = %d, want 503", code)
	}
	// A service that is gone entirely (not merely unhealthy) is also 503, not a proxy error.
	backend.Close()
	if code, _ := get(t, srv.URL, "/healthz"); code != http.StatusServiceUnavailable {
		t.Errorf("/healthz on an unreachable backend = %d, want 503", code)
	}
}

// What the proxy passes through matters as much as that it does. Home Assistant answers 400
// to an X-Forwarded-For header from a proxy it has not been told to trust, so adding one --
// which net/http does automatically under Director -- silently breaks the flagship service
// behind the front door. And the client's Host has to survive, because a service builds its
// absolute URLs from it and routing by domain will key on it.
func TestProxyPassesTheClientHostAndNoForwardedHeaders(t *testing.T) {
	var gotHost string
	var gotXFF []string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHost, gotXFF = r.Host, r.Header.Values("X-Forwarded-For")
		fmt.Fprint(w, "payload-ok")
	}))
	defer backend.Close()
	bu, _ := url.Parse(backend.URL)
	srv := httptest.NewServer(newFrontDoor(bu, "/healthz"))
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/", nil)
	req.Host = "home.example.test"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if gotHost != "home.example.test" {
		t.Errorf("backend saw Host %q, want the client's own", gotHost)
	}
	if len(gotXFF) != 0 {
		t.Errorf("backend saw X-Forwarded-For %v, want none", gotXFF)
	}
}

// genCert writes a self-signed cert/key (with the given serial, to tell reloads apart) to
// the paths, stamped with mtime mt so the reloader's mtime check is deterministic.
func genCert(t *testing.T, certPath, keyPath string, serial int64, mt time.Time) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(serial),
		Subject:      pkix.Name{CommonName: "briard-test"},
		DNSNames:     []string{"briard-test.local"},
		NotBefore:    mt.Add(-time.Hour),
		NotAfter:     mt.Add(24 * time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(certPath, certPEM, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	// Force a deterministic mtime so changed() is not at the mercy of fs timestamp resolution.
	if err := os.Chtimes(certPath, mt, mt); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(keyPath, mt, mt); err != nil {
		t.Fatal(err)
	}
}

// servedSerial dials TLS to addr and returns the serial of the leaf cert the server served.
func servedSerial(t *testing.T, addr string) int64 {
	t.Helper()
	conn, err := tls.Dial("tcp", addr, &tls.Config{InsecureSkipVerify: true})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	return conn.ConnectionState().PeerCertificates[0].SerialNumber.Int64()
}

// The proxy terminates TLS and forwards to the backend, and HOT-RELOADS the cert when it
// changes on disk (no restart) -- the property that makes renewal gap-free. A
// failed reload keeps serving the last good cert.
func TestCertHotReloadAndProxy(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := dir+"/fullchain.pem", dir+"/key.pem"
	t0 := time.Now()
	genCert(t, certPath, keyPath, 1, t0)

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "payload-ok")
	}))
	defer backend.Close()
	bu, _ := url.Parse(backend.URL)

	r := &certReloader{certPath: certPath, keyPath: keyPath}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: newFrontDoor(bu, "/healthz"), TLSConfig: &tls.Config{GetCertificate: r.getCertificate}}
	go srv.ServeTLS(ln, "", "")
	defer srv.Close()
	addr := ln.Addr().String()

	// Handshake 1: serves cert serial 1, and proxies to the backend over TLS.
	if got := servedSerial(t, addr); got != 1 {
		t.Fatalf("initial served serial = %d, want 1", got)
	}
	resp, err := (&http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}).Get("https://" + addr + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "payload-ok" {
		t.Errorf("proxied body = %q, want payload-ok", body)
	}

	// Renewal: drop a new cert (serial 2) with a later mtime -> next handshake serves it.
	genCert(t, certPath, keyPath, 2, t0.Add(2*time.Second))
	if got := servedSerial(t, addr); got != 2 {
		t.Fatalf("after renewal served serial = %d, want the hot-reloaded 2", got)
	}

	// A half-written (garbage) cert must NOT drop TLS: keep serving the last good one.
	if err := os.WriteFile(certPath, []byte("not a pem"), 0o644); err != nil {
		t.Fatal(err)
	}
	os.Chtimes(certPath, t0.Add(4*time.Second), t0.Add(4*time.Second))
	if got := servedSerial(t, addr); got != 2 {
		t.Errorf("on a bad cert write, served serial = %d, want the last good 2", got)
	}
}
