package host

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"briard.io/shared/api"
	"briard.io/shared/casa"
	"briard.io/shared/dashboard"
)

// fakeCasaCloud is the cloud + Worker as the runner sees them: it remembers the key a claim
// carried and verifies every later signature with it, the way the real ones do.
type fakeCasaCloud struct {
	mu       sync.Mutex
	claims   []casa.ClaimRequest
	refuse   *casa.ClaimStatus
	claimErr error
	status   casa.ClaimStatus // what polls answer
	polls    int
	pub      ed25519.PublicKey
	addrs    []string // addresses written, in order
	addrErr  error
	certs    int
	certErr  error
	notAfter time.Time
}

func (f *fakeCasaCloud) Claim(_ context.Context, req casa.ClaimRequest) (string, *casa.ClaimStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.claims = append(f.claims, req)
	if f.claimErr != nil {
		return "", nil, f.claimErr
	}
	if f.refuse != nil {
		return "", f.refuse, nil
	}
	f.pub = ed25519.PublicKey(req.PubKey)
	f.status = casa.ClaimStatus{State: casa.ClaimPending}
	return "claim-1", nil, nil
}

func (f *fakeCasaCloud) ClaimStatus(_ context.Context, id string) (casa.ClaimStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.polls++
	if id != "claim-1" {
		return casa.ClaimStatus{}, errors.New("no such claim")
	}
	return f.status, nil
}

func (f *fakeCasaCloud) SetA(_ context.Context, req casa.ARequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.addrErr != nil {
		return f.addrErr
	}
	if !ed25519.Verify(f.pub, casa.CanonicalA(req.Name, req.IP, req.TS), req.Sig) {
		return errors.New("bad signature")
	}
	f.addrs = append(f.addrs, req.IP)
	return nil
}

func (f *fakeCasaCloud) Cert(_ context.Context, req casa.CertRequest) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.certs++
	if f.certErr != nil {
		return "", f.certErr
	}
	if !ed25519.Verify(f.pub, casa.CanonicalCert(req.Flock, req.CSR, req.TS), req.Sig) {
		return "", errors.New("bad signature")
	}
	csr, err := x509.ParseCertificateRequest(req.CSR)
	if err != nil {
		return "", err
	}
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: csr.Subject, DNSNames: csr.DNSNames,
		NotBefore: f.notAfter.Add(-90 * 24 * time.Hour), NotAfter: f.notAfter}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, csr.PublicKey, caKey)
	if err != nil {
		return "", err
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), nil
}

func (f *fakeCasaCloud) snapshot() (addrs []string, certs, polls int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.addrs...), f.certs, f.polls
}

// fakeCasaGuest is the guest binding's slice: a VIP, a volume with one cert file, and the page.
type fakeCasaGuest struct {
	vip     string
	vipErr  error
	cert    string
	written int
	views   []dashboard.Casa
}

func (g *fakeCasaGuest) VIP(context.Context, string) (string, error) { return g.vip, g.vipErr }
func (g *fakeCasaGuest) WriteCert(_ context.Context, cert, key string) error {
	if !strings.Contains(key, "EC PRIVATE KEY") {
		return errors.New("no key")
	}
	g.cert = cert
	g.written++
	return nil
}
func (g *fakeCasaGuest) ReadCert(context.Context) (string, error) { return g.cert, nil }
func (g *fakeCasaGuest) DashboardCasa(_ context.Context, c dashboard.Casa) error {
	g.views = append(g.views, c)
	return nil
}
func (g *fakeCasaGuest) SupportsDashboardCasa() bool { return true }

func (g *fakeCasaGuest) lastView(t *testing.T) dashboard.Casa {
	t.Helper()
	if len(g.views) == 0 {
		t.Fatal("no view pushed")
	}
	return g.views[len(g.views)-1]
}

type casaRig struct {
	cfg   Config
	cloud *fakeCasaCloud
	guest *fakeCasaGuest
	now   time.Time
	cs    *casaRunner
}

func newCasaRig(t *testing.T) *casaRig {
	t.Helper()
	r := &casaRig{
		cfg:   Config{FlockName: "alert-fox", VIPDev: "eth1", AssignmentCache: filepath.Join(t.TempDir(), "assignment.json")},
		guest: &fakeCasaGuest{vip: "192.168.1.50/24"},
		now:   time.Unix(1_700_000_000, 0).UTC(),
	}
	r.cloud = &fakeCasaCloud{notAfter: r.now.Add(90 * 24 * time.Hour)}
	r.restart()
	return r
}

// restart is what a host restart does to the runner: everything re-read from the state dir.
func (r *casaRig) restart() {
	r.cs = r.cfg.newCasaRunner(r.cloud)
	r.cs.now = func() time.Time { return r.now }
}

func (r *casaRig) tick(t *testing.T) { r.cs.tick(context.Background(), r.guest, t.Logf) }

func (r *casaRig) claim(t *testing.T, email string) api.DirectiveOutcome {
	t.Helper()
	return r.cs.claim(context.Background(), api.Directive{ID: "d1", Kind: api.DirectiveCasaClaim, Payload: email}, t.Logf)
}

// tickUntilWritten drives ticks (with real time passing, for the issuance goroutine) until the
// guest has a cert or the deadline passes.
func (r *casaRig) tickUntilWritten(t *testing.T, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for r.guest.written < want && time.Now().Before(deadline) {
		r.tick(t)
		time.Sleep(10 * time.Millisecond)
	}
	if r.guest.written < want {
		t.Fatalf("cert not written (written=%d, want %d)", r.guest.written, want)
	}
}

// The whole life of a name, from the page's click on: claim -> pending -> registered -> the
// address written under the node's signature -> a certificate issued and on the volume -> the
// page told at every step -> the address re-written when the VIP moves -> a restart losing
// nothing -> renewal at 30 days out.
func TestCasaClaimRegisterAddressCertificateRenew(t *testing.T) {
	r := newCasaRig(t)

	o := r.claim(t, " Owner@Example.org ")
	if o.State != api.OutcomeDone || !strings.Contains(o.Detail, "Owner@Example.org") {
		t.Fatalf("claim = %+v", o)
	}
	if len(r.cloud.claims) != 1 || r.cloud.claims[0].Flock != "alert-fox" || r.cloud.claims[0].Email != "Owner@Example.org" || len(r.cloud.claims[0].PubKey) != 32 {
		t.Fatalf("cloud saw %+v", r.cloud.claims)
	}
	dir := filepath.Dir(r.cfg.AssignmentCache)
	if st, err := os.Stat(filepath.Join(dir, casaKeyName)); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("casa key file: %v %v", st, err)
	}

	r.tick(t)
	if v := r.guest.lastView(t); v.State != casa.ClaimPending || v.Name != "alert-fox.briard.casa" || v.Email != "Owner@Example.org" {
		t.Fatalf("view while pending = %+v", v)
	}
	if addrs, certs, _ := r.cloud.snapshot(); len(addrs) != 0 || certs != 0 {
		t.Fatal("nothing may be written before the claim is registered")
	}
	// Polls are paced: two ticks a second apart is one poll.
	r.now = r.now.Add(time.Second)
	r.tick(t)
	if _, _, polls := r.cloud.snapshot(); polls != 1 {
		t.Fatalf("polls = %d, want 1 (paced)", polls)
	}

	r.cloud.status = casa.ClaimStatus{State: casa.ClaimRegistered}
	r.now = r.now.Add(casaPollEvery)
	r.tickUntilWritten(t, 1)
	addrs, certs, _ := r.cloud.snapshot()
	if len(addrs) != 1 || addrs[0] != "192.168.1.50" {
		t.Fatalf("addresses written = %v, want [192.168.1.50]", addrs)
	}
	if certs != 1 {
		t.Fatalf("certs issued = %d, want 1", certs)
	}
	if casaCertExpiry(r.guest.cert, "alert-fox").IsZero() {
		t.Fatal("the written cert does not cover the flock's apex")
	}
	if v := r.guest.lastView(t); v.State != casa.ClaimRegistered || !v.CertUntil.Equal(r.cloud.notAfter) {
		t.Fatalf("view after registration = %+v", v)
	}

	// A steady VIP writes nothing more; a moved one is written once.
	r.now = r.now.Add(time.Minute)
	r.tick(t)
	r.guest.vip = "192.168.1.77/24"
	r.tick(t)
	r.tick(t)
	if addrs, _, _ := r.cloud.snapshot(); len(addrs) != 2 || addrs[1] != "192.168.1.77" {
		t.Fatalf("addresses after a move = %v", addrs)
	}

	// Restart: the key and the registration come back from disk; the address is re-asserted
	// once, the cert is read back and found fresh, so nothing is issued.
	r.restart()
	r.tick(t)
	time.Sleep(20 * time.Millisecond)
	r.tick(t)
	addrs, certs, _ = r.cloud.snapshot()
	if len(addrs) != 3 || addrs[2] != "192.168.1.77" {
		t.Fatalf("addresses after restart = %v, want one re-assertion", addrs)
	}
	if certs != 1 {
		t.Fatalf("certs after restart = %d, want still 1 (the volume's is fresh)", certs)
	}
	if v := r.guest.lastView(t); v.State != casa.ClaimRegistered || v.CertUntil.IsZero() {
		t.Fatalf("view after restart = %+v", v)
	}

	// Renewal: 61 days on, the volume's cert has 29 left -> re-issued and re-written.
	r.now = r.now.Add(61 * 24 * time.Hour)
	r.cloud.notAfter = r.now.Add(90 * 24 * time.Hour)
	r.tickUntilWritten(t, 2)
	if _, certs, _ := r.cloud.snapshot(); certs != 2 {
		t.Fatalf("certs after renewal = %d, want 2", certs)
	}
	if v := r.guest.lastView(t); !v.CertUntil.Equal(r.cloud.notAfter) {
		t.Fatalf("view after renewal = %+v", v)
	}
}

func TestCasaClaimRefusedAndExpired(t *testing.T) {
	r := newCasaRig(t)
	r.cloud.refuse = &casa.ClaimStatus{State: casa.ClaimRefused, Reason: "this name belongs to another account"}
	o := r.claim(t, "b@x.org")
	if o.State != api.OutcomeFailed || o.Detail != "this name belongs to another account" {
		t.Fatalf("refused claim = %+v", o)
	}
	r.tick(t)
	if v := r.guest.lastView(t); v.State != casa.ClaimRefused || v.Reason == "" {
		t.Fatalf("view after refusal = %+v", v)
	}

	// The link's 20 minutes pass unclicked.
	r.cloud.refuse = nil
	if o := r.claim(t, "a@x.org"); o.State != api.OutcomeDone {
		t.Fatalf("claim = %+v", o)
	}
	r.cloud.status = casa.ClaimStatus{State: casa.ClaimExpired}
	r.tick(t)
	if v := r.guest.lastView(t); v.State != casa.ClaimExpired {
		t.Fatalf("view after expiry = %+v", v)
	}
	if r.cs.st.Claim != "" {
		t.Fatal("an expired claim is still being polled")
	}
	// The state survives a restart, and no address or cert work happens for an unregistered name.
	r.restart()
	r.tick(t)
	if addrs, certs, _ := r.cloud.snapshot(); len(addrs) != 0 || certs != 0 {
		t.Fatal("wrote for an unregistered name")
	}
}

func TestCasaClaimRefusals(t *testing.T) {
	r := newCasaRig(t)
	if o := r.claim(t, ""); o.State != api.OutcomeFailed {
		t.Fatal("an empty email was accepted")
	}
	r.cloud.claimErr = errors.New("connection refused")
	if o := r.claim(t, "a@x.org"); o.State != api.OutcomeFailed || !strings.Contains(o.Detail, "connection refused") {
		t.Fatalf("unreachable cloud: %+v", o)
	}
	nameless := Config{AssignmentCache: r.cfg.AssignmentCache}.newCasaRunner(r.cloud)
	if o := nameless.claim(context.Background(), api.Directive{Kind: api.DirectiveCasaClaim, Payload: "a@x.org"}, t.Logf); o.State != api.OutcomeFailed {
		t.Fatal("a flock with no name claimed one")
	}
	stateless := Config{FlockName: "alert-fox"}.newCasaRunner(r.cloud)
	if o := stateless.claim(context.Background(), api.Directive{Kind: api.DirectiveCasaClaim, Payload: "a@x.org"}, t.Logf); o.State != api.OutcomeFailed {
		t.Fatal("a node with no state dir claimed a name it cannot hold the key for")
	}
}

// Failures are retried on their own clocks, and the guest is never lied to: an address the
// Worker refused is not remembered as written, a cert the cloud did not issue is asked for again.
func TestCasaRetriesAfterFailures(t *testing.T) {
	r := newCasaRig(t)
	r.claim(t, "a@x.org")
	r.cloud.status = casa.ClaimStatus{State: casa.ClaimRegistered}
	r.cloud.addrErr = errors.New("worker down")
	r.cloud.certErr = errors.New("cloud down")
	r.tick(t)
	time.Sleep(20 * time.Millisecond)
	r.tick(t) // collects the failed issuance
	r.tick(t)
	if addrs, certs, _ := r.cloud.snapshot(); len(addrs) != 0 || certs != 1 {
		t.Fatalf("addrs=%v certs=%d: a failed address write must not be repeated within the minute, a failed issuance not within the hour", addrs, certs)
	}
	r.cloud.addrErr, r.cloud.certErr = nil, nil
	r.now = r.now.Add(casaRenewRetry + time.Second)
	r.tickUntilWritten(t, 1)
	if addrs, certs, _ := r.cloud.snapshot(); len(addrs) != 1 || certs != 2 {
		t.Fatalf("after recovery: addrs=%v certs=%d", addrs, certs)
	}
}

// The directive is local-only and guest-askable: the household's page or CLI, never the cloud.
func TestCasaClaimIsLocalOnly(t *testing.T) {
	cfg := Config{}
	o := cfg.dispatch(context.Background(), api.Directive{ID: "d1", Kind: api.DirectiveCasaClaim, Payload: "a@x.org"}, originCloud, nil, nil, nil, nil, nil, nil, t.Logf)
	if o.State != api.OutcomeFailed || !strings.Contains(o.Detail, "local admin socket") {
		t.Fatalf("from the cloud: %+v", o)
	}
	if !guestMayAsk(api.DirectiveCasaClaim) {
		t.Fatal("the dashboard cannot ask for a name")
	}
}

// A new channel session is told the view again though nothing changed: the guest may have been
// restarted under the same runner, and its tmpfs then holds no view. Within a session an
// unchanged view is not re-sent.
func TestCasaViewIsPushedAgainEachSession(t *testing.T) {
	r := newCasaRig(t)
	r.tick(t)
	r.tick(t)
	if len(r.guest.views) != 1 {
		t.Fatalf("one session pushed %d views, want 1", len(r.guest.views))
	}
	r.cs.newSession() // what observe does when the loop re-dials
	r.tick(t)
	if len(r.guest.views) != 2 || r.guest.views[1] != r.guest.views[0] {
		t.Fatalf("after a new session the views are %+v; want the same view pushed again", r.guest.views)
	}
}
