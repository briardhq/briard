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
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"briard.io/agent/guest"
	"briard.io/agent/nic"
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

// casaPublicResolvers are asked, in turn, whether the name has reached public DNS. Two
// operators, because a network may block either one; one answer with the VIP is enough.
var casaPublicResolvers = []string{"1.0.0.1", "8.8.8.8"}

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
	// lookup answers name's IPv4 addresses from server; server "" is this machine's own
	// resolver, the one the household's network handed out. casaLookup, faked by the tests.
	lookup func(ctx context.Context, server, name string) ([]string, error)

	// mu guards st, key and the per-session fields below: tick runs them on the observe loop
	// and claim runs off it (acts.go), the one directive that writes what tick reads.
	mu  sync.Mutex
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
	// The other two cloud calls, the same way: a goroutine makes the call and hands the answer
	// back here for the next tick to apply, so a slow Worker never holds the observe loop. The
	// loop keeps every write to the runner's state; the goroutines touch none of it.
	polling chan casaPolled // a claim-status poll in flight; nil when none
	setting chan casaSetA   // an address write in flight; nil when none
	pushed  *dashboard.Casa // what the guest was last told, this channel session (newSession)

	// The resolve check, for the address last written (resolveFor): first public DNS, then the
	// household's resolver. resolves is "" until that answers positively (dashboard.Casa.Resolves),
	// and the name is used nowhere until it is CasaResolves; resolveNote says what is awaited.
	resolveFor  string
	resolves    string
	resolveNote string
	nextResolve time.Time
	resolving   chan casaResolved // a check in flight; nil when none
	// The network the verdict was found on (watchLAN): a new network is a new resolver, so the
	// verdict is forgotten and both steps run again. lan reads it; cfg.lanKey, faked by tests.
	lan     func(ctx context.Context) string
	lanKey  string
	nextLAN time.Time
}

// casaResolved is what a resolve check hands back. public is whether 1.0.0.1 or 8.8.8.8 answered
// the VIP; local is the household's verdict ("" when it could not tell), err why not.
type casaResolved struct {
	addr   string
	public bool
	local  string
	err    error
}

// casaPolled is what a claim-status poll hands back; casaSetA what an address write does.
type casaPolled struct {
	st  casa.ClaimStatus
	err error
}

type casaSetA struct {
	addr string
	err  error
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
	c := &casaRunner{cfg: cfg, cl: cl, dir: cfg.stateDir(), now: time.Now, lookup: casaLookup, lan: cfg.lanKey}
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
	c.mu.Lock()
	key, err := c.ensureKey()
	c.mu.Unlock()
	if err != nil {
		return fail(err.Error())
	}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	id, refused, err := c.cl.Claim(cctx, casa.ClaimRequest{Flock: c.cfg.FlockName, Email: email, PubKey: key.Public().(ed25519.PublicKey)})
	if err != nil {
		return fail("could not reach the name service: " + err.Error())
	}
	c.mu.Lock()
	defer c.mu.Unlock()
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

// recheck is the casa-recheck directive: forget the resolve check's verdict for the current
// address, so the next tick runs both steps again. For a household that changed its router after
// the name was found blocked; the name stays unused until the new answer is positive. A check
// already in flight is left to land, and its answer counts.
func (c *casaRunner) recheck(d api.Directive, logf func(string, ...any)) api.DirectiveOutcome {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.st.Registered || c.cfg.FlockName == "" {
		return api.DirectiveOutcome{ID: d.ID, State: api.OutcomeFailed, Detail: "this home has no registered name to check"}
	}
	c.resolves, c.resolveNote, c.nextResolve = "", "", time.Time{}
	logf("casa: checking again whether this network resolves %s", casa.Names(c.cfg.FlockName)[0])
	return api.DirectiveOutcome{ID: d.ID, State: api.OutcomeDone, Detail: "checking again; this takes up to a minute"}
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
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if c.st.Claim != "" && !now.Before(c.nextPoll) {
		c.poll(ctx, now, logf)
	}
	if c.st.Registered && c.key != nil {
		c.address(ctx, vip, now, logf)
		c.watchLAN(ctx, now, logf)
		c.resolve(ctx, now, logf)
		c.certificate(ctx, g, now, logf)
	}
	c.push(ctx, g, logf)
}

func (c *casaRunner) poll(ctx context.Context, now time.Time, logf func(string, ...any)) {
	if c.polling == nil {
		// Start one, off the loop; its answer lands on a later tick. Never two at once.
		c.polling = make(chan casaPolled, 1)
		go func(out chan<- casaPolled, claim string) {
			pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			st, err := c.cl.ClaimStatus(pctx, claim)
			out <- casaPolled{st: st, err: err}
		}(c.polling, c.st.Claim)
		return
	}
	var res casaPolled
	select {
	case res = <-c.polling:
		c.polling = nil
	default:
		return // still asking
	}
	st, err := res.st, res.err
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
	if c.setting != nil {
		// A write in flight: take its answer if it has one, else let it finish.
		select {
		case res := <-c.setting:
			c.setting = nil
			if res.err != nil {
				c.nextA = now.Add(casaAddrRetry)
				logf("casa: address %s -> %s: %v (retrying in %s)", c.cfg.FlockName, res.addr, res.err, casaAddrRetry)
				return
			}
			c.lastA = res.addr
			logf("casa: %s -> %s", casa.Names(c.cfg.FlockName)[0], res.addr)
		default:
			return
		}
	}
	if addr == c.lastA || now.Before(c.nextA) {
		return
	}
	ts := now.Unix()
	req := casa.ARequest{Name: c.cfg.FlockName, IP: addr, TS: ts, Sig: ed25519.Sign(c.key, casa.CanonicalA(c.cfg.FlockName, addr, ts))}
	c.setting = make(chan casaSetA, 1)
	go func(out chan<- casaSetA) {
		actx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		out <- casaSetA{addr: addr, err: c.cl.SetA(actx, req)}
	}(c.setting)
}

// casaLANEvery is how often the network is read for watchLAN: the read is a few netlink
// questions, and a minute's lag on a network change is nothing against the check it starts.
const casaLANEvery = time.Minute

// watchLAN forgets the resolve check's verdict when the host finds itself on another network --
// the household moved the machine, or the router it answers is a different box: a new network
// is a new resolver, and the verdict was that resolver's. The verdict found on the first network
// seen is kept until a second one is; a network that cannot be read (none owned, the cable out)
// says nothing and changes nothing. Called with the lock held.
func (c *casaRunner) watchLAN(ctx context.Context, now time.Time, logf func(string, ...any)) {
	if c.lan == nil || now.Before(c.nextLAN) {
		return
	}
	c.nextLAN = now.Add(casaLANEvery)
	key := c.lan(ctx)
	if key == "" || key == c.lanKey {
		return
	}
	if c.lanKey != "" && c.resolves != "" {
		c.resolves, c.resolveNote, c.nextResolve = "", "", time.Time{}
		logf("casa: this host is on another network now (%s); checking again whether it resolves %s", key, casa.Names(c.cfg.FlockName)[0])
	}
	c.lanKey = key
}

// lanKey names the network the host is on, for watchLAN: its own address and prefix on the
// parent and the router it points at. "" when the agent owns no network or the reading is not
// one (nic.Fingerprint.Usable).
func (cfg Config) lanKey(ctx context.Context) string {
	if cfg.net == nil || cfg.net.Parent == "" {
		return ""
	}
	fp := nic.Read(ctx, cfg.net.Parent)
	if !fp.Usable() {
		return ""
	}
	return fp.HostCIDR + " via " + fp.GatewayIP
}

// resolve checks, for the address last written, that the household's network finds the name,
// and is the gate on using it (the page's link, HA's external_url): routers with DNS rebinding
// protection drop a public answer that points at a private address, and then a valid
// certificate opens nothing. Two steps, in order. Public DNS first (1.0.0.1, else 8.8.8.8),
// waited on until it answers the VIP, so the local resolver is never asked early and left
// caching a "no such name". Then the household's resolver, whose answer is final for this
// address in this session. A step that could not tell is asked again; only a positive answer
// is recorded. Runs off the loop and lands on a later tick, like the cloud calls.
func (c *casaRunner) resolve(ctx context.Context, now time.Time, logf func(string, ...any)) {
	if c.lastA == "" {
		return // nothing written yet, so nothing to look for
	}
	if c.lastA != c.resolveFor {
		// A new address: what was found for the old one says nothing about it.
		c.resolveFor, c.resolves, c.resolveNote, c.nextResolve = c.lastA, "", "", time.Time{}
	}
	if c.resolving != nil {
		var res casaResolved
		select {
		case res = <-c.resolving:
			c.resolving = nil
		default:
			return // still asking
		}
		if res.addr != c.resolveFor {
			return // asked about an address since replaced; the next tick asks about this one
		}
		name := casa.Names(c.cfg.FlockName)[0]
		switch {
		case !res.public:
			c.resolveNote = "public DNS (" + strings.Join(casaPublicResolvers, ", ") + ") does not answer " + name + " with " + res.addr + " yet"
			c.nextResolve = now.Add(casaPollEvery)
		case res.local == "":
			c.resolveNote = "could not ask this network's DNS about " + name + ": " + res.err.Error()
			c.nextResolve = now.Add(casaAddrRetry)
			logf("casa: %s (retrying in %s)", c.resolveNote, casaAddrRetry)
		default:
			c.resolves, c.resolveNote = res.local, ""
			if res.local == dashboard.CasaResolves {
				logf("casa: this network resolves %s to %s", name, res.addr)
			} else {
				logf("casa: public DNS resolves %s to %s but this network does not (%v): DNS rebind protection; the name is not used", name, res.addr, res.err)
			}
		}
		return
	}
	if c.resolves != "" || now.Before(c.nextResolve) {
		return
	}
	c.resolving = make(chan casaResolved, 1)
	go func(out chan<- casaResolved, name, addr string) {
		out <- casaCheck(ctx, c.lookup, name, addr)
	}(c.resolving, casa.Names(c.cfg.FlockName)[0], c.resolveFor)
}

// casaCheck runs the two steps once. The household's verdict is CasaBlocked only on a positive
// answer that is not the VIP: no such name, or another address. A resolver that timed out or
// failed could not tell, and is reported as such (local "").
func casaCheck(ctx context.Context, lookup func(context.Context, string, string) ([]string, error), name, addr string) casaResolved {
	res := casaResolved{addr: addr}
	for _, server := range casaPublicResolvers {
		lctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		ips, _ := lookup(lctx, server, name)
		cancel()
		if slices.Contains(ips, addr) {
			res.public = true
			break
		}
	}
	if !res.public {
		return res
	}
	lctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	ips, err := lookup(lctx, "", name)
	cancel()
	var dnsErr *net.DNSError
	switch {
	case err == nil && slices.Contains(ips, addr):
		res.local = dashboard.CasaResolves
	case err == nil:
		res.local, res.err = dashboard.CasaBlocked, fmt.Errorf("answered %s", strings.Join(ips, ", "))
	case errors.As(err, &dnsErr) && dnsErr.IsNotFound:
		res.local, res.err = dashboard.CasaBlocked, errors.New("answered no such name")
	default:
		res.err = err
	}
	return res
}

// casaLookup asks server (host, port 53) for name's IPv4 addresses, or this machine's own
// resolver when server is "".
func casaLookup(ctx context.Context, server, name string) ([]string, error) {
	r := net.DefaultResolver
	if server != "" {
		r = &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, net.JoinHostPort(server, "53"))
		}}
	}
	ips, err := r.LookupIP(ctx, "ip4", name+".")
	out := make([]string, 0, len(ips))
	for _, ip := range ips {
		out = append(out, ip.String())
	}
	return out, err
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
		v.State, v.Resolves = casa.ClaimRegistered, c.resolves
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
		c.mu.Lock()
		c.pushed = nil
		c.mu.Unlock()
	}
}

// doctor hands `briard doctor` the registered name and its resolve check.
func (c *casaRunner) doctor(f *doctorFacts) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.st.Registered || c.cfg.FlockName == "" {
		return
	}
	f.Casa, f.CasaAddr, f.CasaResolves, f.CasaNote = casa.Names(c.cfg.FlockName)[0], c.resolveFor, c.resolves, c.resolveNote
}
