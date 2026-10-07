package host

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"briard.io/agent/guest"
	"briard.io/shared/api"
	"briard.io/shared/atomicfile"
	"briard.io/shared/casa"
	"briard.io/shared/dashboard"
)

// The node's half of the casa name service (shared/casa): `<flock>.briard.casa`, claimed
// from the household's own dashboard, confirmed by an emailed link, and from then on kept by
// this loop -- the address written directly at the Worker whenever the VIP moves, and a
// wildcard certificate renewed at 30 days out through the cloud. Nothing periodic goes up.
//
// WHAT THE HOST HOLDS, AND WHY IT IS THE HOST. The claim produces two durable facts, both
// node-scoped pet state beside the identity files: the Ed25519 key that IS this node's
// authority over the name (generated here, never sent anywhere -- the cloud binds its public
// half once, after the email says yes), and the claim's own state. The guest keeps nothing that
// outlives a push: it renders what the host tells it (dashboard.Casa) and serves the certificate
// the host writes to the volume. Identity and decisions on the host; the guest is disposable.
//
// Everything else here is re-derived: the address is re-written once per session (an
// idempotent upsert), the certificate's expiry is read back from the volume. A restart loses
// nothing that matters.

const (
	casaKeyName   = "casa-key"  // the Ed25519 seed, hex, 0600
	casaStateName = "casa.json" // casaState, 0600

	casaPollEvery   = 10 * time.Second // while a claim is pending
	casaPollRetry   = 30 * time.Second // after a failed poll
	casaAddrRetry   = time.Minute      // after a failed address write
	casaCertRecheck = time.Hour        // how often the volume's cert is read back
	casaRenewWindow = 30 * 24 * time.Hour
	casaRenewRetry  = time.Hour // after a failed issuance
)

// casaState is the claim's state on disk.
type casaState struct {
	Email      string `json:"email,omitempty"`
	Claim      string `json:"claim,omitempty"` // the polling id of a claim in flight
	Registered bool   `json:"registered,omitempty"`
	// Last is how the previous claim ended when it did not register (refused / expired), with
	// the cloud's reason; cleared by the next claim.
	Last   string `json:"last,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// casaClient is the slice of cloud.Casa the runner drives -- narrow so the loop is unit-testable
// against a scripted cloud + Worker.
type casaClient interface {
	Claim(ctx context.Context, req casa.ClaimRequest) (id string, refused *casa.ClaimStatus, err error)
	ClaimStatus(ctx context.Context, id string) (casa.ClaimStatus, error)
	Cert(ctx context.Context, req casa.CertRequest) (certPEM string, err error)
	SetA(ctx context.Context, req casa.ARequest) error
}

// casaGuest is the slice of the guest binding the runner needs: where the VIP is, the
// certificate on the volume, and the page's view. *guestagent.Client satisfies it.
type casaGuest interface {
	WriteCert(ctx context.Context, cert, key string) error
	ReadCert(ctx context.Context) (string, error)
	DashboardCasa(ctx context.Context, c dashboard.Casa) error
	SupportsDashboardCasa() bool
}

type casaRunner struct {
	cfg Config
	cl  casaClient
	dir string // pet state; "" = nowhere to keep a key, so no claim is possible
	now func() time.Time

	st  casaState
	key ed25519.PrivateKey // nil until minted or loaded

	// Per-session, re-derived: nothing below survives a restart, and nothing needs to.
	lastA     string    // the address last written at the Worker
	nextA     time.Time // no address write before this (after a failure)
	nextPoll  time.Time
	certUntil time.Time // the volume's cert expiry for the casa names; zero = none
	certRead  time.Time // when certUntil was last read back
	nextRenew time.Time
	renewing  chan casaIssued // an issuance in flight, off the loop; nil when none
	pushed    *dashboard.Casa // what the guest was last told, this channel session (newSession)
}

// casaIssued is what an issuance goroutine hands back.
type casaIssued struct {
	cert, key string
	until     time.Time
	err       error
}

// newCasaRunner loads whatever the last run left in the state dir. cl == nil is a build with no
// casa client (tests that do not care); every claim is then refused.
func (cfg Config) newCasaRunner(cl casaClient) *casaRunner {
	c := &casaRunner{cfg: cfg, cl: cl, dir: cfg.stateDir(), now: time.Now}
	if c.dir == "" {
		return c
	}
	if raw, err := os.ReadFile(filepath.Join(c.dir, casaStateName)); err == nil {
		_ = json.Unmarshal(raw, &c.st)
	}
	if raw, err := os.ReadFile(filepath.Join(c.dir, casaKeyName)); err == nil {
		if seed, err := hex.DecodeString(strings.TrimSpace(string(raw))); err == nil && len(seed) == ed25519.SeedSize {
			c.key = ed25519.NewKeyFromSeed(seed)
		}
	}
	return c
}

func (c *casaRunner) save() error {
	raw, err := json.Marshal(c.st)
	if err != nil {
		return err
	}
	return atomicfile.Write(filepath.Join(c.dir, casaStateName), raw, 0o600, 0o700)
}

// ensureKey returns the node's casa key, minting and recording it on first use. Minted from
// the OS's randomness and never sent anywhere; its public half rides the claim.
func (c *casaRunner) ensureKey() (ed25519.PrivateKey, error) {
	if c.key != nil {
		return c.key, nil
	}
	seed := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(seed); err != nil {
		return nil, fmt.Errorf("minting the casa key: %w", err)
	}
	if err := atomicfile.Write(filepath.Join(c.dir, casaKeyName), []byte(hex.EncodeToString(seed)+"\n"), 0o600, 0o700); err != nil {
		return nil, fmt.Errorf("recording the casa key: %w", err)
	}
	c.key = ed25519.NewKeyFromSeed(seed)
	return c.key, nil
}

// claim is the casa-claim directive: open a claim at the cloud for the email in the payload.
// The outcome tells the person what happened next (a link is in their inbox, or why not).
func (c *casaRunner) claim(ctx context.Context, d api.Directive, logf func(string, ...any)) api.DirectiveOutcome {
	fail := func(detail string) api.DirectiveOutcome {
		return api.DirectiveOutcome{ID: d.ID, State: api.OutcomeFailed, Detail: detail}
	}
	email := strings.TrimSpace(d.Payload)
	switch {
	case c.cl == nil:
		return fail("this node has no casa client")
	case email == "":
		return fail("an email address is needed to claim a name")
	case c.cfg.FlockName == "":
		return fail("this machine has no name yet, so there is nothing to claim")
	case c.dir == "":
		return fail("this node keeps no state, so it cannot hold a name's key")
	}
	key, err := c.ensureKey()
	if err != nil {
		return fail(err.Error())
	}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	id, refused, err := c.cl.Claim(cctx, casa.ClaimRequest{Flock: c.cfg.FlockName, Email: email, PubKey: key.Public().(ed25519.PublicKey)})
	if err != nil {
		return fail("could not reach the name service: " + err.Error())
	}
	if refused != nil {
		c.st.Claim, c.st.Last, c.st.Reason = "", casa.ClaimRefused, refused.Reason
		_ = c.save()
		return fail(refused.Reason)
	}
	c.st.Claim, c.st.Email, c.st.Last, c.st.Reason = id, email, "", ""
	if err := c.save(); err != nil {
		return fail("recording the claim: " + err.Error())
	}
	c.nextPoll = time.Time{}
	logf("casa: claimed %s for %s -- a confirmation link is on its way", casa.Names(c.cfg.FlockName)[0], email)
	return api.DirectiveOutcome{ID: d.ID, State: api.OutcomeDone,
		Detail: "a confirmation link was sent to " + email + "; open it within 20 minutes"}
}

// tick is the runner's step in the observe loop: poll a pending claim, keep a registered name's
// address and certificate current, and keep the page told. Every call out is short and
// ctx-bounded; the one long one (issuance, a real ACME solve) runs off the loop. vip is the
// cycle's one answer to net.vip, read by the loop and shared with the address step.
func (c *casaRunner) tick(ctx context.Context, r any, vip guest.VIPReader, logf func(string, ...any)) {
	if c == nil {
		return // a loop built without a runner (tests)
	}
	g, ok := r.(casaGuest)
	if !ok || c.cl == nil || c.cfg.FlockName == "" {
		return
	}
	now := c.now()
	if c.st.Claim != "" && !now.Before(c.nextPoll) {
		c.poll(ctx, now, logf)
	}
	if c.st.Registered && c.key != nil {
		c.address(ctx, vip, now, logf)
		c.certificate(ctx, g, now, logf)
	}
	c.push(ctx, g, logf)
}

func (c *casaRunner) poll(ctx context.Context, now time.Time, logf func(string, ...any)) {
	pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	st, err := c.cl.ClaimStatus(pctx, c.st.Claim)
	if err != nil {
		c.nextPoll = now.Add(casaPollRetry)
		logf("casa: claim status: %v (retrying in %s)", err, casaPollRetry)
		return
	}
	switch st.State {
	case casa.ClaimPending:
		c.nextPoll = now.Add(casaPollEvery)
	case casa.ClaimRegistered:
		c.st.Claim, c.st.Registered, c.st.Last, c.st.Reason = "", true, "", ""
		_ = c.save()
		// A fresh registration (or a re-bind): write the address and look at the cert now.
		c.lastA, c.nextA, c.certRead, c.nextRenew = "", time.Time{}, time.Time{}, time.Time{}
		logf("casa: %s is registered for %s", casa.Names(c.cfg.FlockName)[0], c.st.Email)
	default: // refused, expired
		c.st.Claim, c.st.Last, c.st.Reason = "", st.State, st.Reason
		_ = c.save()
		logf("casa: claim %s: %s", st.State, st.Reason)
	}
}

// address points the name at the VIP, directly at the Worker, whenever the VIP the guest holds
// differs from what was last written. Idempotent, so once per session is the floor and a moved
// address the trigger.
func (c *casaRunner) address(ctx context.Context, vip guest.VIPReader, now time.Time, logf func(string, ...any)) {
	vctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	cidr, err := vip.VIP(vctx, c.cfg.VIPDev)
	cancel()
	if err != nil || cidr == "" {
		return // no address to publish yet; the guest says so every tick until there is
	}
	addr, _, _ := strings.Cut(cidr, "/")
	if addr == c.lastA || now.Before(c.nextA) {
		return
	}
	ts := now.Unix()
	req := casa.ARequest{Name: c.cfg.FlockName, IP: addr, TS: ts, Sig: ed25519.Sign(c.key, casa.CanonicalA(c.cfg.FlockName, addr, ts))}
	actx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := c.cl.SetA(actx, req); err != nil {
		c.nextA = now.Add(casaAddrRetry)
		logf("casa: address %s -> %s: %v (retrying in %s)", c.cfg.FlockName, addr, err, casaAddrRetry)
		return
	}
	c.lastA = addr
	logf("casa: %s -> %s", casa.Names(c.cfg.FlockName)[0], addr)
}

// certificate keeps a certificate for the flock's two names on the volume: read back hourly,
// renewed at 30 days out. The issuance runs off the loop and lands on a later tick.
func (c *casaRunner) certificate(ctx context.Context, g casaGuest, now time.Time, logf func(string, ...any)) {
	if c.renewing != nil {
		select {
		case res := <-c.renewing:
			c.renewing = nil
			if res.err != nil {
				c.nextRenew = now.Add(casaRenewRetry)
				logf("casa: certificate: %v (retrying in %s)", res.err, casaRenewRetry)
				return
			}
			wctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			err := g.WriteCert(wctx, res.cert, res.key)
			cancel()
			if err != nil {
				c.nextRenew = now.Add(casaRenewRetry)
				logf("casa: certificate issued but not written: %v (retrying in %s)", err, casaRenewRetry)
				return
			}
			c.certUntil, c.certRead = res.until, now
			logf("casa: certificate for %s written, valid until %s", casa.Names(c.cfg.FlockName)[0], res.until.Format(time.RFC3339))
		default:
		}
		return
	}
	if c.certRead.IsZero() || now.Sub(c.certRead) >= casaCertRecheck {
		rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		pemText, err := g.ReadCert(rctx)
		cancel()
		if err != nil {
			return // the guest did not answer; ask again next tick
		}
		c.certUntil = casaCertExpiry(pemText, c.cfg.FlockName)
		c.certRead = now
	}
	if (c.certUntil.IsZero() || c.certUntil.Sub(now) < casaRenewWindow) && !now.Before(c.nextRenew) {
		c.nextRenew = now.Add(casaRenewRetry) // no second issuance while this one runs
		c.renewing = make(chan casaIssued, 1)
		go c.issue(ctx, now, c.renewing)
		logf("casa: requesting a certificate for %s", casa.Names(c.cfg.FlockName)[0])
	}
}

// issue mints a TLS keypair + CSR for the flock's names, signs the request with the casa key, and
// asks the cloud. The TLS key stays here until the cert arrives; both then go to the volume.
func (c *casaRunner) issue(ctx context.Context, now time.Time, out chan<- casaIssued) {
	names := casa.Names(c.cfg.FlockName)
	tlsKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		out <- casaIssued{err: err}
		return
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: names[0]}, DNSNames: names,
	}, tlsKey)
	if err != nil {
		out <- casaIssued{err: err}
		return
	}
	keyDER, err := x509.MarshalECPrivateKey(tlsKey)
	if err != nil {
		out <- casaIssued{err: err}
		return
	}
	ts := now.Unix()
	req := casa.CertRequest{Flock: c.cfg.FlockName, CSR: csr, TS: ts, Sig: ed25519.Sign(c.key, casa.CanonicalCert(c.cfg.FlockName, csr, ts))}
	certPEM, err := c.cl.Cert(ctx, req)
	if err != nil {
		out <- casaIssued{err: err}
		return
	}
	until := casaCertExpiry(certPEM, c.cfg.FlockName)
	if until.IsZero() {
		out <- casaIssued{err: errors.New("the issued certificate does not cover the flock's names")}
		return
	}
	out <- casaIssued{
		cert:  certPEM,
		key:   string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})),
		until: until,
	}
}

// casaCertExpiry is the leaf's expiry when it covers the flock's apex, else zero -- a managed
// cert for some other name on the volume is not this name's.
func casaCertExpiry(pemText, flock string) time.Time {
	block, _ := pem.Decode([]byte(pemText))
	if block == nil {
		return time.Time{}
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil || leaf.VerifyHostname(casa.Names(flock)[0]) != nil {
		return time.Time{}
	}
	return leaf.NotAfter
}

// view is what the page is told.
func (c *casaRunner) view() dashboard.Casa {
	v := dashboard.Casa{Name: casa.Names(c.cfg.FlockName)[0], Email: c.st.Email, CertUntil: c.certUntil}
	switch {
	case c.st.Claim != "":
		v.State = casa.ClaimPending
	case c.st.Registered:
		v.State = casa.ClaimRegistered
	case c.st.Last != "":
		v.State, v.Reason = c.st.Last, c.st.Reason
	}
	return v
}

// push tells the guest the view when it changed -- and once per session regardless, because the
// guest is disposable and a fresh one knows nothing until told.
func (c *casaRunner) push(ctx context.Context, g casaGuest, logf func(string, ...any)) {
	if !g.SupportsDashboardCasa() {
		return
	}
	v := c.view()
	if c.pushed != nil && *c.pushed == v {
		return
	}
	pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := g.DashboardCasa(pctx, v); err != nil {
		logf("casa: dashboard view: %v", err)
		return
	}
	c.pushed = &v
}

// newSession forgets what the guest was told. Called at the start of every channel session:
// the runner outlives the guest (an OS upgrade, a recovery or a memory relaunch restarts it
// under the same agent), and a fresh guest's tmpfs holds no view until it is pushed again.
func (c *casaRunner) newSession() {
	if c != nil {
		c.pushed = nil
	}
}
