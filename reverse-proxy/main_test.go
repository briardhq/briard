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

	"os"
	"strings"
	"testing"

	"briard.io/shared/routes"
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

// getHost is the same GET with a chosen Host header — which is the whole input to routing, so
// nearly every assertion below is one of these.
func getHost(t *testing.T, base, path, host string) (int, string) {
	t.Helper()
	req, err := http.NewRequest("GET", base+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = host
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

// fixed is a routing table that never reloads — the shape most tests want.
func fixed(t routes.Table) func() *routing {
	r := newRouting(t)
	return func() *routing { return r }
}

// The shipped state of a fresh node: nothing installed. The VIP must still ANSWER — Briard's
// own page at the root — and /healthz must say the node is ready rather than sick.
// A node reporting unhealthy forever because no service was ever installed is the zombie
// state this replaces.
func TestZeroServiceFrontDoor(t *testing.T) {
	srv := httptest.NewServer(newFrontDoor(fixed(routes.Table{})))
	defer srv.Close()

	if code, body := get(t, srv.URL, "/healthz"); code != http.StatusOK || !strings.Contains(body, "no services routed") {
		t.Errorf("/healthz with nothing routed = %d %q, want 200 saying so", code, body)
	}
	if code, body := get(t, srv.URL, "/"); code != http.StatusOK || !strings.Contains(body, "Nothing is installed") {
		t.Errorf("/ with nothing routed = %d %q, want 200 and Briard's page", code, body)
	}
	// The page belongs at the root, not under every URL a stranger mistypes.
	if code, _ := get(t, srv.URL, "/nope"); code != http.StatusNotFound {
		t.Errorf("/nope with nothing installed = %d, want 404", code)
	}
}

// THE ASSERTION hass-payload.nix LOST, at unit scale ([B.48]): a request whose Host names a
// runtime-installed service reaches that service through the front door. From [V3.15] until
// [V3b.3](e2) the door had ONE backend baked at guest-build time, so no runtime-installed service
// was ever reachable on :80/:443; this is the property that replaces it, and it is keyed on the
// name rather than on there being exactly one thing to forward to.
func TestRoutesByHostToTheNamedService(t *testing.T) {
	ha := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "home-assistant-ok")
	}))
	defer ha.Close()
	mq := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "mosquitto-ok")
	}))
	defer mq.Close()

	srv := httptest.NewServer(newFrontDoor(fixed(routes.Table{Services: []routes.Service{
		{Name: "home-assistant", Hosts: []string{"briard-brave-elf-home-assistant.local"}, Address: ha.URL, HealthPath: "/", Fronted: true},
		{Name: "mosquitto", Hosts: []string{"briard-brave-elf-mosquitto.local"}, Address: mq.URL, HealthPath: "/", Fronted: true},
	}})))
	defer srv.Close()

	// Each name reaches ITS OWN service. Two services behind one address is the whole reason the
	// door needed a table rather than a backend.
	for host, want := range map[string]string{
		"briard-brave-elf-home-assistant.local": "home-assistant-ok",
		"briard-brave-elf-mosquitto.local":      "mosquitto-ok",
		// What a client actually sends: the port when it is not the scheme's default, a trailing
		// dot from a fully-qualified resolver, and whatever case the user typed.
		"briard-brave-elf-home-assistant.local:80": "home-assistant-ok",
		"briard-brave-elf-home-assistant.local.":   "home-assistant-ok",
		"BRIARD-Brave-Elf-Home-Assistant.local":    "home-assistant-ok",
		"briard-brave-elf-mosquitto.local.:80":     "mosquitto-ok",
	} {
		if code, body := getHost(t, srv.URL, "/", host); code != http.StatusOK || body != want {
			t.Errorf("Host %q = %d %q, want 200 %q", host, code, body, want)
		}
	}
	// A name we do not serve is not a service's 404: it is the bare-IP page, which is where the
	// household reads which names this node DOES answer to.
	code, body := getHost(t, srv.URL, "/", "192.168.1.100")
	if code != http.StatusOK || !strings.Contains(body, "home-assistant") || !strings.Contains(body, "mosquitto") {
		t.Errorf("the bare address = %d %q, want Briard's page listing both services", code, body)
	}
}

// /healthz IS THE NODE'S, ALWAYS, and this is the reversal that N services forces. One backend
// could forward the question to itself; N cannot, and a service that is down must not make the
// node it runs on read as broken — the OS health gate and the rollback reflex read this, and
// [V3b.3](f) puts service failures deliberately outside the promoter's reach.
func TestHealthIsTheNodesOwnAnswerNotAServices(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "wedged", http.StatusServiceUnavailable)
	}))
	defer dead.Close()
	srv := httptest.NewServer(newFrontDoor(fixed(routes.Table{Services: []routes.Service{
		{Name: "home-assistant", Hosts: []string{"briard-brave-elf-home-assistant.local"}, Address: dead.URL, HealthPath: "/healthz", Fronted: true},
	}})))
	defer srv.Close()

	code, body := get(t, srv.URL, "/healthz")
	if code != http.StatusOK {
		t.Errorf("/healthz with a wedged service = %d, want 200: the NODE is fine", code)
	}
	if !strings.Contains(body, "1 service(s) routed") || !strings.Contains(body, "home-assistant") {
		t.Errorf("/healthz = %q, want it to name what it routes", body)
	}
	// The service's own 503 is still visible where it belongs — through its own name.
	if code, _ := getHost(t, srv.URL, "/healthz", "briard-brave-elf-home-assistant.local"); code != http.StatusServiceUnavailable {
		t.Errorf("the service's own /healthz = %d, want its 503 forwarded", code)
	}
}

// A service routed by name whose backend the door must NOT expose. mosquitto is the live case, and
// it is a security property, not a nicety: its manifest port is the broker's management API, which
// mosquitto.conf binds to 127.0.0.1 on purpose — and the door runs inside that same guest, so
// forwarding to it by name would republish a loopback-only endpoint on the LAN. The name is still
// ours (it resolves to the VIP, where MQTT is listening), so the answer says so instead of 404ing.
func TestRoutedButNotFronted(t *testing.T) {
	var reached bool
	mgmt := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		fmt.Fprint(w, "management-api")
	}))
	defer mgmt.Close()
	srv := httptest.NewServer(newFrontDoor(fixed(routes.Table{Services: []routes.Service{
		// A REACHABLE backend, deliberately: the node can and does probe this address (it is what
		// the health floor GETs), so a test with no backend at all would prove only that the door
		// cannot reach what does not exist.
		{Name: "mosquitto", Hosts: []string{"briard-brave-elf-mosquitto.local"},
			Address: mgmt.URL, HealthPath: "/api/v1/listeners", Fronted: false},
	}})))
	defer srv.Close()

	code, body := getHost(t, srv.URL, "/", "briard-brave-elf-mosquitto.local")
	if code != http.StatusNotImplemented || !strings.Contains(body, "mosquitto") {
		t.Errorf("a not-fronted service = %d %q, want 501 naming it", code, body)
	}
	if reached {
		t.Error("the door forwarded to a backend the table says it must not expose")
	}
	if _, body := get(t, srv.URL, "/"); !strings.Contains(body, "not served over HTTP") {
		t.Errorf("Briard's page = %q, want it to say the service is not served over HTTP", body)
	}
}

// A node whose flock has no minted name has no per-service names either (routes.HostName), so it
// routes nothing — and must SAY the service is installed rather than pretend the node is empty.
func TestInstalledButUnnamed(t *testing.T) {
	srv := httptest.NewServer(newFrontDoor(fixed(routes.Table{Services: []routes.Service{
		{Name: "home-assistant", Address: "http://127.0.0.1:8123", HealthPath: "/", Fronted: true},
	}})))
	defer srv.Close()

	code, body := get(t, srv.URL, "/")
	if code != http.StatusOK || !strings.Contains(body, "not yet reachable by name") {
		t.Errorf("an unnamed service = %d %q, want the page to say it is installed but unnamed", code, body)
	}
}

// The table is rewritten UNDER a running door — that is what an install is — so it must be picked
// up without a restart, and a write caught half-done must cost nothing. Same discipline as the
// cert, and for a bigger stake: a bad parse that emptied the table would take every service in the
// household off its name at once.
func TestRoutesHotReloadAndSurviveABadWrite(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "payload-ok")
	}))
	defer backend.Close()

	dir := t.TempDir()
	path := dir + "/routes.json"
	t0 := time.Now()
	write := func(raw []byte, mt time.Time) {
		t.Helper()
		if err := os.WriteFile(path, raw, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, mt, mt); err != nil {
			t.Fatal(err)
		}
	}
	marshal := func(tb routes.Table) []byte {
		t.Helper()
		raw, err := tb.Marshal()
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}

	// A door that starts before anything is installed: no file at all, and it still answers.
	rr := &routeReloader{path: path}
	srv := httptest.NewServer(newFrontDoor(rr.current))
	defer srv.Close()
	if code, body := get(t, srv.URL, "/healthz"); code != http.StatusOK || !strings.Contains(body, "no services routed") {
		t.Fatalf("/healthz with no table = %d %q, want 200 saying nothing is routed", code, body)
	}

	// The install writes the table. No restart, no reload signal: the next request routes.
	write(marshal(routes.Table{Services: []routes.Service{
		{Name: "fixture", Hosts: []string{"briard-brave-elf-fixture.local"}, Address: backend.URL, HealthPath: "/healthz", Fronted: true},
	}}), t0.Add(2*time.Second))
	if code, body := getHost(t, srv.URL, "/", "briard-brave-elf-fixture.local"); code != http.StatusOK || body != "payload-ok" {
		t.Fatalf("after the install = %d %q, want it routed to the service", code, body)
	}

	// A half-written table keeps the last good one: the household's service stays reachable.
	write([]byte("{\"services\": [{\"nam"), t0.Add(4*time.Second))
	if code, body := getHost(t, srv.URL, "/", "briard-brave-elf-fixture.local"); code != http.StatusOK || body != "payload-ok" {
		t.Errorf("on a half-written table = %d %q, want the last good route kept", code, body)
	}

	// An UNINSTALL is an empty table, and it must take effect — "keep the last good one" is for a
	// table that will not parse, never for one that parses and says less than it used to.
	write(marshal(routes.Table{}), t0.Add(6*time.Second))
	if code, _ := getHost(t, srv.URL, "/", "briard-brave-elf-fixture.local"); code != http.StatusOK {
		t.Errorf("after an uninstall = %d, want Briard's page", code)
	}
	if _, body := getHost(t, srv.URL, "/", "briard-brave-elf-fixture.local"); strings.Contains(body, "payload-ok") {
		t.Errorf("after an uninstall the name still reaches the service: %q", body)
	}
}

// What the proxy passes through matters as much as that it does. Home Assistant answers 400
// to an X-Forwarded-For header from a proxy it has not been told to trust, so adding one --
// which net/http does automatically under Director -- silently breaks the flagship service
// behind the front door. And the client's Host has to survive: a service builds its absolute
// URLs from it, and it is now also the thing that CHOSE this backend.
func TestProxyPassesTheClientHostAndNoForwardedHeaders(t *testing.T) {
	var gotHost string
	var gotXFF []string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHost, gotXFF = r.Host, r.Header.Values("X-Forwarded-For")
		fmt.Fprint(w, "payload-ok")
	}))
	defer backend.Close()
	srv := httptest.NewServer(newFrontDoor(fixed(routes.Table{Services: []routes.Service{
		{Name: "fixture", Hosts: []string{"home.example.test"}, Address: backend.URL, HealthPath: "/healthz", Fronted: true},
	}})))
	defer srv.Close()

	if code, _ := getHost(t, srv.URL, "/", "home.example.test"); code != http.StatusOK {
		t.Fatalf("proxied request = %d, want 200", code)
	}
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

	r := &certReloader{certPath: certPath, keyPath: keyPath}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: newFrontDoor(fixed(routes.Table{Services: []routes.Service{{Name: "fixture", Hosts: []string{"briard-brave-elf-fixture.local"}, Address: backend.URL, HealthPath: "/healthz", Fronted: true}}})), TLSConfig: &tls.Config{GetCertificate: r.getCertificate}}
	go srv.ServeTLS(ln, "", "")
	defer srv.Close()
	addr := ln.Addr().String()

	// Handshake 1: serves cert serial 1, and proxies to the backend over TLS.
	if got := servedSerial(t, addr); got != 1 {
		t.Fatalf("initial served serial = %d, want 1", got)
	}
	// Through the front door over TLS, under the service's own name: the route and the cert are
	// independent, and a request has to pass both to reach the service.
	req, _ := http.NewRequest("GET", "https://"+addr+"/", nil)
	req.Host = "briard-brave-elf-fixture.local"
	resp, err := (&http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}).Do(req)
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
