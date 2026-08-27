package host

import (
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"briard.io/agent/drbd"
	"briard.io/agent/guestagent"
	"briard.io/shared/api"
	"briard.io/shared/manifest"
	"briard.io/shared/model"
)

// fakeInstaller records the ORDER of everything the install does — the bracket's correctness is
// an ordering property, so order is what the tests assert.
type fakeInstaller struct {
	hold      func() error // blocks inside the budget (see beat_test.go)
	steps     []string
	primary   bool
	active    bool
	healthy   bool
	chains    [][]string
	prior     string // the manifest already on the volume; "" = fresh install
	manifests []string
	healthURL string
	renderEr  error
	provEr    error
	adjustEr  error
	resumeEr  error
	warmEr    error
	restoreEr error
}

func (f *fakeInstaller) Status(context.Context, string) (model.QuorumState, error) {
	f.steps = append(f.steps, "status")
	return model.QuorumState{Primary: f.primary, Quorate: true}, nil
}

func (f *fakeInstaller) ServiceRender(context.Context, map[string]string, []string) error {
	if f.hold != nil {
		if err := f.hold(); err != nil {
			return err
		}
	}
	f.steps = append(f.steps, "render")
	return f.renderEr
}
func (f *fakeInstaller) ServiceProvision(_ context.Context, _ string, _ []string, manifest string) error {
	f.steps = append(f.steps, "provision")
	f.manifests = append(f.manifests, manifest) // records what identity the volume ends up holding
	return f.provEr
}
func (f *fakeInstaller) ServiceManifest(context.Context) (string, error) {
	f.steps = append(f.steps, "manifest?")
	return f.prior, nil
}
func (f *fakeInstaller) PayloadStop(_ context.Context, unit string) error {
	f.steps = append(f.steps, "stop:"+unit)
	return nil
}
func (f *fakeInstaller) Snapshot(_ context.Context, _, dest string) error {
	f.steps = append(f.steps, "snapshot:"+dest)
	return nil
}
func (f *fakeInstaller) Restore(_ context.Context, _, src string) error {
	f.steps = append(f.steps, "restore:"+src)
	return f.restoreEr
}
func (f *fakeInstaller) ReactorActive(context.Context) (bool, error) {
	f.steps = append(f.steps, "active?")
	return f.active, nil
}
func (f *fakeInstaller) ReactorPause(context.Context, string) error {
	f.steps = append(f.steps, "pause")
	return nil
}
func (f *fakeInstaller) ReactorResume(context.Context, string) error {
	f.steps = append(f.steps, "resume")
	return f.resumeEr
}
func (f *fakeInstaller) Adjust(_ context.Context, req guestagent.ProvisionRequest) error {
	f.steps = append(f.steps, "adjust")
	f.chains = append(f.chains, parseChain(req.ReactorConfig))
	if req.ResConfig == "" {
		// Adjust writes the .res file unconditionally, so an empty ResConfig TRUNCATES the
		// resource definition. Catching it here is cheaper than catching it on a real node.
		f.steps = append(f.steps, "TRUNCATED-RES")
	}
	return f.adjustEr
}
func (f *fakeInstaller) PayloadStart(_ context.Context, unit string) error {
	f.steps = append(f.steps, "warm:"+unit)
	return f.warmEr
}
func (f *fakeInstaller) PayloadHealth(_ context.Context, url string) (bool, error) {
	f.steps = append(f.steps, "health")
	f.healthURL = url // the gate must probe the SERVICE, not the front door
	if !f.healthy {
		return false, errors.New("not ready")
	}
	return true, nil
}

// The service-install path has no opinion about names. "" -- this node publishes none -- is the
// honest answer here; a fixture name could be mistaken for an assertion that one was published.
func (f *fakeInstaller) MDNSPublished(context.Context) (string, error) { return "", nil }

// parseChain pulls the unit list back out of a rendered promoter snippet.
func parseChain(snippet string) []string {
	_, rest, ok := strings.Cut(snippet, "start = [ ")
	if !ok {
		return nil
	}
	list, _, _ := strings.Cut(rest, " ]")
	var out []string
	for _, u := range strings.Split(list, ", ") {
		out = append(out, strings.Trim(u, `"`))
	}
	return out
}

// catalogFor stands up a signed one-service catalog and returns a Config wired to it.
func catalogFor(t *testing.T, m manifest.Manifest) Config {
	t.Helper()
	body, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	sig := ed25519.Sign(priv, body)
	mux := http.NewServeMux()
	mux.HandleFunc("/"+m.Name+".json", func(w http.ResponseWriter, _ *http.Request) { w.Write(body) })
	mux.HandleFunc("/"+m.Name+".json.sig", func(w http.ResponseWriter, _ *http.Request) { w.Write(sig) })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return Config{
		CatalogURL:    srv.URL,
		UpdateKeyring: pemKey(t, pub),
		Resource:      drbd.Resource{Name: "r0", Device: "/dev/drbd0", Peers: []drbd.Peer{{Name: "n1", Address: "127.0.0.1:7789", Disk: "/dev/vdb"}}},
		Promoter:      promoterUnits(nil),
		HealthURL:     "http://192.168.1.100/healthz",
		ServiceCache:  "", // off: these tests assert orchestration, not persistence
	}
}

// pemKey encodes a public key the way the release keyring expects it (PKIX "PUBLIC KEY").
func pemKey(t *testing.T, pub ed25519.PublicKey) []byte {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
}

func testManifest() manifest.Manifest {
	return manifest.Manifest{
		Name: "home-assistant", Version: "2026.7.1",
		Containers: []manifest.Container{{
			Name: "ha", Image: "ghcr.io/x/ha@sha256:" + strings.Repeat("a", 64),
			Mount: "/config", Primary: true, Port: 8123, HealthPath: "/manifest.json",
		}},
	}
}

func install(cfg Config, f *fakeInstaller) api.DirectiveOutcome {
	d := api.Directive{ID: "d1", Kind: api.DirectiveServiceInstall, Payload: "home-assistant"}
	return cfg.applyServiceInstall(context.Background(), f, d, func(string, ...any) {})
}

// TestInstallOrdersTheBracket is the core contract: nothing touches the promoter until the units
// and storage exist, and the chain rewrite happens strictly BETWEEN a pause and a resume. A
// rewrite outside the bracket is drbd-reactor reading a half-written start-list on a live node.
func TestInstallOrdersTheBracket(t *testing.T) {
	f := &fakeInstaller{primary: true, active: true, healthy: true}
	if o := install(catalogFor(t, testManifest()), f); o.State != api.OutcomeDone {
		t.Fatalf("outcome = %+v, want done", o)
	}
	// A fresh install: read the (empty) installed manifest, render+warm, provision, then the
	// bracket — pause, quiesce the CONTAINER (never the pod — stopping the pod unmounts the shared
	// volume; here a no-op anyway, nothing is running), rewrite, resume, gate. No snapshot:
	// nothing is installed yet, so there is no rollback point to take.
	want := []string{
		"active?", "manifest?", "render", "warm:briard-home-assistant-ha-image.service", "status", "provision",
		"pause", "stop:briard-home-assistant-ha.service", "adjust", "resume", "health",
	}
	if strings.Join(f.steps, ",") != strings.Join(want, ",") {
		t.Fatalf("steps = %v\nwant   %v", f.steps, want)
	}
	// The gate probes the SERVICE's own endpoint (manifest port + healthPath, in-guest), NOT the
	// VIP front door — the front door doesn't reflect a runtime-installed service.
	if f.healthURL != "http://127.0.0.1:8123/manifest.json" {
		t.Fatalf("health probe = %q, want the service endpoint (not the VIP front door)", f.healthURL)
	}
	// The new chain names the pod and its member, between the data mount and the VIP.
	got := f.chains[0]
	want = []string{"briard-data.service", "briard-home-assistant-pod.service", "briard-home-assistant-ha.service", "briard-vip.service"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("chain = %v, want %v", got, want)
	}
}

// TestInstallWarmsBeforeTouchingThePromoter is the offline case, and the reason the warm exists
// at all: the .image units are WantedBy=multi-user.target, so a service installed AFTER boot never
// has them run, and the containers' Pull=never would then fail the chain at promotion.
//
// Warming at install time is also where it belongs — a human is watching and the CLI can report it
// — and the assertion here is that an unreachable image aborts BEFORE the bracket. A node whose
// install failed for want of a network must be exactly as it was, not left with a chain naming a
// service whose image it does not have.
func TestInstallWarmsBeforeTouchingThePromoter(t *testing.T) {
	f := &fakeInstaller{primary: true, active: true, healthy: true, warmEr: errors.New("no route to host")}
	o := install(catalogFor(t, testManifest()), f)
	if o.State != api.OutcomeFailed || !strings.Contains(o.Detail, "warm image") {
		t.Fatalf("outcome = %+v, want a failure naming the image warm", o)
	}
	joined := strings.Join(f.steps, ",")
	for _, forbidden := range []string{"pause", "adjust", "provision"} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("a failed warm still ran %q — the node was changed by a failed install: %v", forbidden, f.steps)
		}
	}
}

// TestInstallRefusesWhenTheBracketIsOpen is the interim guard, and the assertion that it
// refuses BEFORE writing anything — a refusal must leave the node exactly as it was.
func TestInstallRefusesWhenTheBracketIsOpen(t *testing.T) {
	f := &fakeInstaller{primary: true, active: false, healthy: true}
	o := install(catalogFor(t, testManifest()), f)
	if o.State != api.OutcomeFailed || !strings.Contains(o.Detail, "already paused") {
		t.Fatalf("outcome = %+v, want a failure naming the open bracket", o)
	}
	for _, s := range f.steps {
		if s == "render" || s == "provision" || s == "pause" || s == "adjust" {
			t.Fatalf("refusal still touched the node: %v", f.steps)
		}
	}
}

// TestInstallRevertsOnAFailedHealthGate: a service that does not come up must leave the node as it
// was — chain restored, promoter running. "Failed install" and "broken node" must not be the same
// event, which is why the outcome is rolled-back rather than failed.
func TestInstallRevertsOnAFailedHealthGate(t *testing.T) {
	cfg := catalogFor(t, testManifest())
	cfg.HealthURL = "http://192.168.1.100/healthz"
	f := &fakeInstaller{primary: true, active: true, healthy: false}
	// A short budget rather than a cancelled context: the fetch must still succeed, and the point
	// is that the gate gives up and the REVERT still runs — on its own detached deadline, since
	// the one it inherited is exactly what just expired.
	o := func() api.DirectiveOutcome {
		d := api.Directive{ID: "d1", Kind: api.DirectiveServiceInstall, Payload: "home-assistant"}
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		defer cancel()
		return cfg.applyServiceInstall(ctx, f, d, func(string, ...any) {})
	}()
	if o.State != api.OutcomeRolledBack {
		t.Fatalf("outcome = %+v, want rolled-back", o)
	}
	if len(f.chains) != 2 {
		t.Fatalf("chain written %d times, want 2 (install then revert)", len(f.chains))
	}
	reverted := strings.Join(f.chains[1], ",")
	if reverted != "briard-data.service,briard-vip.service" {
		t.Fatalf("reverted chain = %q, want the original zero-service chain", reverted)
	}
	if n := strings.Count(strings.Join(f.steps, ","), "resume"); n != 2 {
		t.Fatalf("resumed %d times, want 2 — a paused promoter is a node that cannot fail over: %v", n, f.steps)
	}
}

// priorManifest is the same service at an earlier version — a real UPGRADE target (same name ⇒
// same unit names + DataRoot, different bytes so priorService treats it as a rollback point).
func priorManifest(t *testing.T) string {
	t.Helper()
	m := testManifest()
	m.Version = "2026.6.0"
	body, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

// TestUpgradeRollsBackDataAndManifest is the (i) contract at the orchestration layer: a broken
// UPGRADE must put back the prior service's DATA and IDENTITY, not merely take the new one out of
// the chain. So the path snapshots before the switch, and on a tripped gate it restores the
// subvolume and re-records the prior manifest — the service-level twin of the {code+data} OS
// rollback. (The real btrfs/quadlet/front-door mechanics are the job of service-install-broken.nix;
// this asserts the host drives them in the right order.)
func TestUpgradeRollsBackDataAndManifest(t *testing.T) {
	prior := priorManifest(t)
	cfg := catalogFor(t, testManifest())
	f := &fakeInstaller{primary: true, active: true, healthy: false, prior: prior}
	o := func() api.DirectiveOutcome {
		d := api.Directive{ID: "d1", Kind: api.DirectiveServiceInstall, Payload: "home-assistant"}
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		defer cancel()
		return cfg.applyServiceInstall(ctx, f, d, func(string, ...any) {})
	}()
	if o.State != api.OutcomeRolledBack {
		t.Fatalf("outcome = %+v, want rolled-back", o)
	}
	joined := strings.Join(f.steps, ",")
	snap := "snapshot:" + "/var/lib/briard/.snapshots/home-assistant-preupgrade"
	// The rollback point is taken BEFORE the volume is mutated (provision) and before the switch.
	si, pi := strings.Index(joined, snap), strings.Index(joined, "provision")
	if si < 0 || pi < 0 || si > pi {
		t.Fatalf("snapshot must precede provision: %v", f.steps)
	}
	// The data is rolled back from that exact snapshot.
	if !strings.Contains(joined, "restore:/var/lib/briard/.snapshots/home-assistant-preupgrade") {
		t.Fatalf("a failed upgrade did not restore the data subvolume: %v", f.steps)
	}
	// The volume ends up holding the PRIOR manifest again — the identity is reverted, not just the
	// chain. provision is called with the new manifest, then again with the prior on rollback.
	if n := len(f.manifests); n < 2 || f.manifests[n-1] != prior {
		t.Fatalf("volume manifest not reverted to the prior: got %v", f.manifests)
	}
	// The promoter is resumed on both brackets (switch + revert) — a paused promoter cannot fail over.
	if n := strings.Count(joined, "resume"); n != 2 {
		t.Fatalf("resumed %d times, want 2 (switch then revert): %v", n, f.steps)
	}
	// The POD is NEVER stopped — only its container. Stopping the pod unmounts the shared DRBD
	// volume and takes every other service on the node down. This is the invariant the
	// whole quiesce design turns on, so assert it directly.
	if strings.Contains(joined, "stop:briard-home-assistant-pod.service") {
		t.Fatalf("quiesce stopped the POD — that unmounts the shared volume: %v", f.steps)
	}
	if !strings.Contains(joined, "stop:briard-home-assistant-ha.service") {
		t.Fatalf("quiesce never stopped the container, so the data bind was never released: %v", f.steps)
	}
}

// TestUpgradeLeavesPromoterPausedIfDataCannotRestore: if the data rollback itself fails, the revert
// must NOT resume the promoter onto the prior units — they would run on the poisoned data. A node
// that will not fail over is recoverable; silent data corruption is not. So it stops, reports both
// failures, and leaves the bracket open for a human.
func TestUpgradeLeavesPromoterPausedIfDataCannotRestore(t *testing.T) {
	cfg := catalogFor(t, testManifest())
	f := &fakeInstaller{primary: true, active: true, healthy: false, prior: priorManifest(t), restoreEr: errors.New("btrfs: cannot delete subvolume")}
	o := func() api.DirectiveOutcome {
		d := api.Directive{ID: "d1", Kind: api.DirectiveServiceInstall, Payload: "home-assistant"}
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		defer cancel()
		return cfg.applyServiceInstall(ctx, f, d, func(string, ...any) {})
	}()
	if o.State != api.OutcomeFailed || !strings.Contains(o.Detail, "restore the data subvolume") {
		t.Fatalf("outcome = %+v, want a failure naming the data restore", o)
	}
	// Only the forward switch resumed; the revert deliberately did not — the promoter is left paused.
	if n := strings.Count(strings.Join(f.steps, ","), "resume"); n != 1 {
		t.Fatalf("resumed %d times, want 1 — revert must NOT resume onto un-restored data: %v", n, f.steps)
	}
}

// TestInstallAlwaysResumes: even when the chain rewrite itself fails, the promoter must come back.
// Leaving it down is strictly worse than a failed install.
func TestInstallAlwaysResumes(t *testing.T) {
	f := &fakeInstaller{primary: true, active: true, healthy: true, adjustEr: errors.New("write failed")}
	o := install(catalogFor(t, testManifest()), f)
	if o.State != api.OutcomeFailed {
		t.Fatalf("outcome = %+v, want failed", o)
	}
	if !strings.Contains(strings.Join(f.steps, ","), "resume") {
		t.Fatalf("promoter left paused after a failed rewrite: %v", f.steps)
	}
}

// TestInstallNeverTruncatesTheResourceConfig: Adjust writes the .res file unconditionally, so the
// full resource config must ride along with the reactor snippet. Omitting it would erase the DRBD
// resource definition on a live node.
func TestInstallNeverTruncatesTheResourceConfig(t *testing.T) {
	f := &fakeInstaller{primary: true, active: true, healthy: true}
	install(catalogFor(t, testManifest()), f)
	if strings.Contains(strings.Join(f.steps, ","), "TRUNCATED-RES") {
		t.Fatalf("Adjust was called with an empty ResConfig: %v", f.steps)
	}
}

// TestInstallOnSecondaryRendersButDoesNotProvision: the volume is mounted only on the Primary, so
// a secondary takes its units and stops. It must NOT touch the promoter — the primary owns that.
func TestInstallOnSecondaryRendersButDoesNotProvision(t *testing.T) {
	f := &fakeInstaller{primary: false, active: true, healthy: true}
	o := install(catalogFor(t, testManifest()), f)
	if o.State != api.OutcomeDone {
		t.Fatalf("outcome = %+v, want done (units rendered)", o)
	}
	joined := strings.Join(f.steps, ",")
	if !strings.Contains(joined, "render") {
		t.Fatalf("secondary did not render its units: %v", f.steps)
	}
	for _, forbidden := range []string{"provision", "pause", "adjust"} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("secondary ran %q: %v", forbidden, f.steps)
		}
	}
}

// TestInstallRefusesAnUnsignedCatalog: the manifest's trust root is the release keyring, and a
// node with none must refuse rather than install unverified content.
func TestInstallRefusesAnUnsignedCatalog(t *testing.T) {
	cfg := catalogFor(t, testManifest())
	cfg.UpdateKeyring = nil
	f := &fakeInstaller{primary: true, active: true, healthy: true}
	if o := install(cfg, f); o.State != api.OutcomeFailed {
		t.Fatalf("outcome = %+v, want a refusal with no keyring", o)
	}
	if len(f.steps) != 0 {
		t.Fatalf("touched the node despite refusing: %v", f.steps)
	}
}

func TestInstallRefusesNoName(t *testing.T) {
	f := &fakeInstaller{primary: true, active: true}
	d := api.Directive{ID: "d1", Kind: api.DirectiveServiceInstall}
	if o := catalogFor(t, testManifest()).applyServiceInstall(context.Background(), f, d, func(string, ...any) {}); o.State != api.OutcomeFailed {
		t.Fatalf("outcome = %+v, want failed", o)
	}
}

func prewarm(cfg Config, f *fakeInstaller) api.DirectiveOutcome {
	d := api.Directive{ID: "p1", Kind: api.DirectiveServicePrewarm, Payload: "home-assistant"}
	return cfg.applyServicePrewarm(context.Background(), f, d, func(string, ...any) {})
}

// Prewarm is the warm-standby half ALONE. The assertion is the exact step list, because
// what makes this safe to fan out to every anchor — including the one currently serving — is that
// it touches nothing else: no provision (that writes the volume), no pause/adjust/resume (that is
// the promoter), no health gate. An extra step here is a directive that can disturb a serving home.
func TestPrewarmRendersAndWarmsAndNothingElse(t *testing.T) {
	f := &fakeInstaller{primary: true, active: true, healthy: true}
	if o := prewarm(catalogFor(t, testManifest()), f); o.State != api.OutcomeDone {
		t.Fatalf("outcome = %+v, want done", o)
	}
	want := []string{"render", "warm:briard-home-assistant-ha-image.service"}
	if strings.Join(f.steps, ",") != strings.Join(want, ",") {
		t.Fatalf("steps = %v\nwant   %v", f.steps, want)
	}
	if len(f.chains) != 0 {
		t.Errorf("prewarm rewrote the promoter chain: %v", f.chains)
	}
	if len(f.manifests) != 0 {
		t.Errorf("prewarm wrote a manifest to the volume: %v", f.manifests)
	}
}

// Role-independence is the SAFETY property, and this is its guard. A directive sits queued between
// enqueue and execution, and a node's role can change in that window — drbd-reactor promotes on
// quorum events, which is nobody's decision to schedule. Under the old shape, where a prewarm was
// just "an install on a node that happens to be Secondary", a promotion in that window silently
// converted a DOWNLOAD into provision + chain rewrite + health gate, on a node mid-failover. If
// these two step lists ever diverge, that race is back.
func TestPrewarmIsIdenticalOnAPrimaryAndASecondary(t *testing.T) {
	var steps [2]string
	for i, primary := range []bool{true, false} {
		f := &fakeInstaller{primary: primary, active: true, healthy: true}
		if o := prewarm(catalogFor(t, testManifest()), f); o.State != api.OutcomeDone {
			t.Fatalf("primary=%v: outcome = %+v, want done", primary, o)
		}
		steps[i] = strings.Join(f.steps, ",")
	}
	if steps[0] != steps[1] {
		t.Errorf("prewarm differs by role:\n primary   %s\n secondary %s", steps[0], steps[1])
	}
}

// A prewarm that cannot pull must FAIL, not report done. The cloud gates the install on these
// outcomes, so a peer that quietly reported success while holding no image would let the whole
// two-phase sequence certify a home that cannot fail over — the exact thing it exists to prevent.
func TestPrewarmFailsWhenTheImageCannotBeWarmed(t *testing.T) {
	f := &fakeInstaller{primary: false, active: true, warmEr: errors.New("no route to host")}
	o := prewarm(catalogFor(t, testManifest()), f)
	if o.State != api.OutcomeFailed || !strings.Contains(o.Detail, "warm image") {
		t.Fatalf("outcome = %+v, want failed naming the warm", o)
	}
}

// cacheService must never damage the manifest it already holds. The bare WriteFile it used to be
// truncated the target first, so a crash inside the write left half a manifest -- which
// installedServices reads as "this service is not installed", silently, on a node whose replicated
// manifest still names what it is meant to be running.
//
// Blocking the temp path with a directory forces the failure after the caller has committed to
// the write. The assertion is deliberately two-sided: the old implementation returns nil AND
// overwrites, so it fails on the first check, not merely on the second.
func TestCacheServiceFailureKeepsThePriorManifest(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{ServiceCache: dir}
	path := cfg.manifestPath("home-assistant")
	prior := []byte(`{"name":"home-assistant"}`)
	if err := cfg.cacheService("home-assistant", prior); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + ".tmp"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("temp file lingered after a good write: %v", err)
	}
	if err := os.Mkdir(path+".tmp", 0o755); err != nil { // the staging path is now unusable
		t.Fatal(err)
	}
	if err := cfg.cacheService("home-assistant", []byte(`{"name":"home-assistant","v":2}`)); err == nil {
		t.Fatal("cacheService onto a blocked temp path returned nil, want an error")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("prior manifest unreadable after a failed cache write: %v", err)
	}
	if string(got) != string(prior) {
		t.Errorf("failed cache write damaged the manifest: got %q, want the prior %q", got, prior)
	}
}

// A service installed at RUNTIME must land in the live config immediately, not at the next agent
// restart. The gap was invisible in the log and total in effect: cfg.resources() is gated on there
// being a service, so between installing one and restarting, the node reported no appliance
// telemetry at all -- payload footprint, volume usage, snapshots, load, journal and store sizes.
func TestAdoptInstalledServiceRefreshesLiveConfig(t *testing.T) {
	dir := t.TempDir()
	raw := []byte(`{"name":"home-assistant","version":"2026.7.1","containers":[` +
		`{"name":"app","image":"ghcr.io/home-assistant/home-assistant@sha256:` +
		`f73512ba4fe06bb4d57636fe3578d0820cdec46f81e8f837ab59e451662ff3cb",` +
		`"mount":"/config","primary":true,"port":8123,"healthPath":"/manifest.json"}]}`)
	if err := os.WriteFile(filepath.Join(dir, "home-assistant.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	logf := func(string, ...any) {}
	done := api.DirectiveOutcome{State: api.OutcomeDone}
	install := api.Directive{Kind: api.DirectiveServiceInstall}

	t.Run("adopts on a completed install", func(t *testing.T) {
		cfg := Config{ServiceCache: dir}
		cfg.adoptInstalledServices(install, done, logf)
		if len(cfg.Services) != 1 {
			t.Fatalf("Services = %+v, want the one installed service", cfg.Services)
		}
		if cfg.Services[0].Name != "home-assistant" {
			t.Fatalf("Services[0].Name = %q, want the installed service", cfg.Services[0].Name)
		}
		if got := cfg.Services[0].ServingUnit(); got != "briard-home-assistant-app.service" {
			t.Errorf("ServingUnit() = %q, want the rendered container unit", got)
		}
		if len(cfg.Promoter) == 0 || len(cfg.ServiceRendered.Units) == 0 {
			t.Errorf("promoter chain / rendered units not adopted: %v %v", cfg.Promoter, cfg.ServiceRendered.Units)
		}
	})
	// The failable half: adopting on anything other than a completed install would let a failed
	// or rolled-back install rewrite the live config from a cache that install did not write.
	t.Run("ignores other kinds and non-Done outcomes", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			d    api.Directive
			o    api.DirectiveOutcome
		}{
			{"rolled back", install, api.DirectiveOutcome{State: api.OutcomeRolledBack}},
			{"failed", install, api.DirectiveOutcome{State: api.OutcomeFailed}},
			{"another kind", api.Directive{Kind: api.DirectiveUpgrade}, done},
		} {
			t.Run(tc.name, func(t *testing.T) {
				cfg := Config{ServiceCache: dir}
				cfg.adoptInstalledServices(tc.d, tc.o, logf)
				if len(cfg.Services) != 0 {
					t.Errorf("live config changed on %s: %+v", tc.name, cfg.Services)
				}
			})
		}
	})
	// A cache that cannot be read is not evidence of an uninstall: keep what we had.
	t.Run("unreadable cache leaves the previous view", func(t *testing.T) {
		cfg := Config{ServiceCache: filepath.Join(dir, "absent"),
			Services: []model.ServiceSpec{{Name: "already-installed"}}}
		cfg.adoptInstalledServices(install, done, logf)
		if len(cfg.Services) != 1 || cfg.Services[0].Name != "already-installed" {
			t.Errorf("Services was cleared by an unreadable cache: %+v", cfg.Services)
		}
	})
}

// TestInstallSaysWhereToReachIt: a successful install hands back the address. The node holds both
// halves -- the manifest's port and the name its own guest publishes -- and the operator holds
// neither, so an install that reports success without them leaves the address to be guessed. A
// stranger guessing it on 2026-08-23 landed on the front door's "nothing is routed here" page and
// read a working install as a broken one.
func TestInstallSaysWhereToReachIt(t *testing.T) {
	cfg := catalogFor(t, testManifest())
	cfg.FlockName = "picked-hornet"
	o := install(cfg, &fakeInstaller{primary: true, active: true, healthy: true})
	if o.State != api.OutcomeDone {
		t.Fatalf("outcome = %+v, want done", o)
	}
	if want := "reach it at http://briard-picked-hornet.local:8123/"; o.Detail != want {
		t.Fatalf("Detail = %q, want %q", o.Detail, want)
	}
}

// ...and with NO published name there is no URL to promise. A witness, or a node whose FLOCK_NAME
// is unset, publishes nothing over mDNS, so naming the port is the most that can be said
// truthfully -- inventing a host for it is exactly the plausible-but-wrong address [V3.17] exists
// to end. Asserted as an absence AND a presence: the port must be named, the URL must not appear.
func TestInstallWithoutAPublishedNameNamesOnlyThePort(t *testing.T) {
	cfg := catalogFor(t, testManifest()) // FlockName left zero on purpose
	o := install(cfg, &fakeInstaller{primary: true, active: true, healthy: true})
	if o.State != api.OutcomeDone {
		t.Fatalf("outcome = %+v, want done", o)
	}
	if want := "it answers on port 8123"; o.Detail != want {
		t.Fatalf("Detail = %q, want %q", o.Detail, want)
	}
	if strings.Contains(o.Detail, "://") {
		t.Fatalf("promised a URL on a node that publishes no name: %q", o.Detail)
	}
}

// manifestJSON is a minimal valid manifest for `name`, used to populate the cache directory
// directly. Written by hand rather than through the install path on purpose: these tests are about
// what the node does with a SET, and the install path cannot currently produce one (see the
// one-at-a-time gate in applyServiceInstall).
func manifestJSON(name, digest string) []byte {
	return []byte(`{"name":"` + name + `","version":"1","containers":[` +
		`{"name":"app","image":"ghcr.io/x/` + name + `@sha256:` + digest + `",` +
		`"mount":"/data","primary":true,"port":8123,"healthPath":"/healthz"}]}`)
}

const (
	digestA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	digestB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// THE PLURAL CUT ITSELF ([V3b.3](a)): a node holding TWO services must assemble one promoter chain
// containing both, between the data mount and the VIP, and must carry both renderings so a guest
// reboot can be replayed. Everything under this used to be singular by construction -- one cache
// file, one spec, one chain -- so nothing could have failed this test; it could only have been
// unwritten.
//
// Order is by service name and nothing more. It is asserted because "deterministic across
// restarts" is a real property the chain needs (a promoter rewritten into a different order on
// every boot is a promoter nobody can reason about), NOT because alphabetical is meaningful --
// making it a dependency order is [V3b.3](c).
func TestInstalledServicesAssemblesTheChainFromAll(t *testing.T) {
	dir := t.TempDir()
	// Written b-then-a so a pass cannot come from the write order.
	for _, s := range []struct{ name, digest string }{{"mosquitto", digestB}, {"home-assistant", digestA}} {
		if err := os.WriteFile(filepath.Join(dir, s.name+".json"), manifestJSON(s.name, s.digest), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cfg := Config{ServiceCache: dir}
	specs, chain, rendered, ok := cfg.installedServices(func(string, ...any) {})
	if !ok {
		t.Fatal("installedServices reported nothing installed, want two services")
	}
	if len(specs) != 2 {
		t.Fatalf("specs = %+v, want two", specs)
	}
	if specs[0].Name != "home-assistant" || specs[1].Name != "mosquitto" {
		t.Errorf("order = %q,%q, want home-assistant,mosquitto (by name, deterministic)", specs[0].Name, specs[1].Name)
	}
	// Each spec keeps its OWN paths -- the per-service derivation that already existed and was
	// only ever called once.
	for _, s := range specs {
		if want := "/var/lib/briard/" + s.Name; s.DataDir != want {
			t.Errorf("%s DataDir = %q, want %q", s.Name, s.DataDir, want)
		}
		if want := "briard-" + s.Name + "-app.service"; s.ServingUnit() != want {
			t.Errorf("%s ServingUnit = %q, want %q", s.Name, s.ServingUnit(), want)
		}
	}
	// The chain: data first, VIP last, BOTH services' units in between.
	if chain[0] != "briard-data.service" || chain[len(chain)-1] != "briard-vip.service" {
		t.Fatalf("chain = %v, want briard-data first and briard-vip last", chain)
	}
	for _, name := range []string{"home-assistant", "mosquitto"} {
		if !slices.ContainsFunc(chain, func(u string) bool { return strings.Contains(u, name) }) {
			t.Errorf("chain = %v, missing %s -- a service outside the chain is a service the promoter never starts", chain, name)
		}
	}
	// And the union carries both renderings, which is what restoreService replays after a guest
	// reboot empties /run. One service's files surviving is not enough.
	for _, name := range []string{"home-assistant", "mosquitto"} {
		found := false
		for file := range rendered.Files {
			if strings.Contains(file, name) {
				found = true
			}
		}
		if !found {
			t.Errorf("rendered union has no files for %s: %v", name, slices.Sorted(maps.Keys(rendered.Files)))
		}
	}
}

// One bad file must cost exactly one service. The cache is read at BRING-UP, so failing the whole
// read on an unparseable file would take a healthy service out of the promoter chain because an
// unrelated one was corrupt -- and a node that comes back serving nothing is the outcome this
// path exists to prevent.
func TestInstalledServicesSkipsOnlyTheBadFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "home-assistant.json"), manifestJSON("home-assistant", digestA), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "mosquitto.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := Config{ServiceCache: dir}
	specs, chain, _, ok := cfg.installedServices(func(string, ...any) {})
	if !ok || len(specs) != 1 || specs[0].Name != "home-assistant" {
		t.Fatalf("specs = %+v (ok=%v), want just home-assistant", specs, ok)
	}
	if slices.ContainsFunc(chain, func(u string) bool { return strings.Contains(u, "mosquitto") }) {
		t.Errorf("chain = %v, must not name units from a manifest that does not parse", chain)
	}
}

// The node can HOLD a set; the install path may not yet CREATE one. Everything that describes a
// service through the seam is still a scalar ([V3b.3](b)), so a second distinct service must be
// refused loudly rather than run-and-under-reported.
//
// Two-sided on purpose: the same-name case must still pass, because install and UPGRADE are one
// path and refusing an upgrade would break the shipped verb.
func TestInstallRefusesASecondDistinctService(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "mosquitto.json"), manifestJSON("mosquitto", digestB), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := catalogFor(t, testManifest()) // testManifest is home-assistant
	cfg.ServiceCache = dir
	o := install(cfg, &fakeInstaller{primary: true, active: true, healthy: true})
	if o.State != api.OutcomeFailed {
		t.Fatalf("outcome = %+v, want failed: a second distinct service must be refused", o)
	}
	if !strings.Contains(o.Detail, "mosquitto") || !strings.Contains(o.Detail, "one service at a time") {
		t.Errorf("Detail = %q, want it to name mosquitto and say why", o.Detail)
	}

	// ...and an UPGRADE of the service already installed is not a second service.
	up := t.TempDir()
	if err := os.WriteFile(filepath.Join(up, "home-assistant.json"), manifestJSON("home-assistant", digestA), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg2 := catalogFor(t, testManifest())
	cfg2.ServiceCache = up
	if o := install(cfg2, &fakeInstaller{primary: true, active: true, healthy: true}); o.State != api.OutcomeDone {
		t.Fatalf("outcome = %+v, want done: re-installing the same service is an upgrade, not a second service", o)
	}
}

// A cache directory that cannot be read must not be spelled the same way as an empty one. On the
// path whose whole job is to refuse a second service, "I could not tell" reading as "nothing is
// installed" is what would let the second one through.
func TestOtherInstalledDistinguishesUnreadableFromEmpty(t *testing.T) {
	if got, err := (Config{ServiceCache: filepath.Join(t.TempDir(), "absent")}).otherInstalled("x"); got != "" || err != nil {
		t.Errorf("absent cache: got %q, %v -- want the shipped state, no error", got, err)
	}
	// A file where the directory should be: readable path, unusable as a directory.
	blocked := filepath.Join(t.TempDir(), "notadir")
	if err := os.WriteFile(blocked, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := (Config{ServiceCache: blocked}).otherInstalled("x"); err == nil {
		t.Error("unreadable cache returned nil error, want the failure surfaced")
	}
}
