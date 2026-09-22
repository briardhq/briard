package host

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"briard.io/agent/drbd"
	"briard.io/agent/hass"
	"briard.io/agent/mosquitto"
	"briard.io/agent/quadlet"
	"briard.io/shared/api"
	"briard.io/shared/manifest"
	"briard.io/shared/model"
)

// fakeInstaller records the ORDER of everything the install does — the bracket's correctness is
// an ordering property, so order is what the tests assert.
type fakeInstaller struct {
	warmedRefs []string     // refs handed to ServiceWarm, in order
	hold       func() error // blocks inside the budget (see beat_test.go)
	steps      []string
	primary    bool
	active     bool
	healthy    bool
	// prior is the volume's recorded manifests, KEYED BY SERVICE NAME — absent = fresh install.
	// A map rather than one string because that is the shape the volume now has: a fake holding
	// another service's manifest must not satisfy a read for this one ([V3b.3](b)).
	prior        map[string]string
	oldGuest     bool     // does not advertise service.installed -- an install must refuse it outright
	noMember     bool     // advertises everything BUT data.member: a guest older than the ring ([B.143])
	free         int64    // what storage.free reports as free (0 = plenty is NOT implied; tests set it)
	freeErr      error    // storage.free failing: the gate logs and the install proceeds unmeasured
	staleRemoved []string // unit files a render was told to remove: the collateral surface
	manifests    []string
	healthURL    string
	healthOf     string
	renderEr     error
	provEr       error
	convergeEr   error
	// convergeSkips are the services converge reports it could not PREPARE, having started the
	// rest. An install whose own service is named there must fail: the node is still serving the
	// prior version.
	convergeSkips []string
	forgetEr      error
	warmEr        error
	restoreEr     error
	members       []quadlet.SnapshotEntry // the ring data.members answers with
	membersEr     error
	ensureEr      error    // an image that cannot be fetched: the restore must refuse before stopping
	sidecars      []string // the member metadata handed to Snapshot, in order
	snapEr        error    // the rollback-point snapshot failing: [B.143] stops the service before taking it, so the undo must restart it
	// readiness is the S1 differential sample, queued: the first call answers with the first
	// element, the next with the second. Two calls per gated install (baseline, then settled),
	// so a two-element queue is one whole verdict.
	readiness   [][]hass.Entry
	readinessEr error
	// the probe half ([V3b.4]): what the service is holding, and the two ways it can be broken.
	stored       string
	notServing   bool
	loseState    bool
	readinessHit int
	// the nudge half ([B.131]): a guest whose volume has no Home Assistant to tell, and one whose
	// Home Assistant would not take the event.
	noHass  bool
	nudgeEr error
}

func (f *fakeInstaller) HassReadiness(_ context.Context, port int) ([]hass.Entry, error) {
	f.steps = append(f.steps, fmt.Sprintf("readiness:%s:%d", hass.Name, port))
	f.readinessHit++
	if f.readinessEr != nil {
		return nil, f.readinessEr
	}
	if f.readinessHit <= len(f.readiness) {
		return f.readiness[f.readinessHit-1], nil
	}
	return nil, nil
}

// The probe half ([V3b.4]). Nothing in this file's tests installs mosquitto, so the default is a
// service that holds what it is given -- present so the fake satisfies the seam, and so a test
// that DOES install a broker can make it lose its state by setting loseState.
func (f *fakeInstaller) MosquittoProbe(_ context.Context, token string) (mosquitto.Sample, error) {
	if token != "" {
		f.stored = token
	}
	if f.loseState {
		f.stored = ""
	}
	return mosquitto.Sample{Serving: !f.notServing, Token: f.stored}, nil
}

// The push direction ([B.131]). `told` is what a guest with Home Assistant on its volume answers;
// the default here is a node that has one, so the install path's call is visible in `steps`.
func (f *fakeInstaller) HassNudge(context.Context) (bool, error) {
	f.steps = append(f.steps, "nudge")
	if f.nudgeEr != nil {
		return false, f.nudgeEr
	}
	return !f.noHass, nil
}

func (f *fakeInstaller) Status(context.Context, string) (model.QuorumState, error) {
	f.steps = append(f.steps, "status")
	return model.QuorumState{Primary: f.primary, Quorate: true}, nil
}

func (f *fakeInstaller) ServiceRender(_ context.Context, _ map[string]string, stale []string) error {
	if f.hold != nil {
		if err := f.hold(); err != nil {
			return err
		}
	}
	f.steps = append(f.steps, "render")
	f.staleRemoved = append(f.staleRemoved, stale...)
	return f.renderEr
}
func (f *fakeInstaller) ServiceProvision(_ context.Context, name, _ string, _ []string, manifest string) error {
	f.steps = append(f.steps, "provision:"+name) // the NAME decides which identity file is written
	f.manifests = append(f.manifests, manifest)  // records what identity the volume ends up holding
	return f.provEr
}

// ServiceInstalled answers for the service it is ASKED about, which is the whole property the
// per-service split adds: another service's manifest can no longer stand in for this one's.
func (f *fakeInstaller) ServiceInstalled(_ context.Context, name string) (string, error) {
	f.steps = append(f.steps, "installed?:"+name)
	return f.prior[name], nil
}
func (f *fakeInstaller) SupportsServiceInstalled() bool { return !f.oldGuest }
func (f *fakeInstaller) SupportsSnapshotMember() bool   { return !f.oldGuest && !f.noMember }
func (f *fakeInstaller) ServiceStop(_ context.Context, unit string) error {
	f.steps = append(f.steps, "stop:"+unit)
	return nil
}
func (f *fakeInstaller) Snapshot(_ context.Context, _, dest, sidecar string) error {
	f.steps = append(f.steps, "snapshot:"+dest)
	f.sidecars = append(f.sidecars, sidecar) // what the picker reads back ([B.143])
	return f.snapEr
}
func (f *fakeInstaller) Restore(_ context.Context, _, src string) error {
	f.steps = append(f.steps, "restore:"+src)
	return f.restoreEr
}
func (f *fakeInstaller) ReactorActive(context.Context) (bool, error) {
	f.steps = append(f.steps, "active?")
	return f.active, nil
}

// ServiceConverge is what an install does instead of rewriting the promoter chain: the volume
// is written first, and this tells the node to re-read it ([V3b.3](f)).
func (f *fakeInstaller) ServiceConverge(context.Context) ([]string, error) {
	f.steps = append(f.steps, "converge")
	return f.convergeSkips, f.convergeEr
}
func (f *fakeInstaller) SupportsServiceConverge() bool { return !f.oldGuest }
func (f *fakeInstaller) ServiceForget(_ context.Context, name string) error {
	f.steps = append(f.steps, "forget:"+name)
	return f.forgetEr
}
func (f *fakeInstaller) ServiceStart(_ context.Context, unit string) error {
	f.steps = append(f.steps, "warm:"+unit)
	return f.warmEr
}

// ServiceWarm records the REF alongside the unit, because the whole point of the verb is that the
// two are checked and acted on separately -- a fake that dropped the ref could not tell an
// ensure-present apart from an unconditional start.
func (f *fakeInstaller) ServiceWarm(_ context.Context, unit, ref string) error {
	f.steps = append(f.steps, "warm:"+unit)
	f.warmedRefs = append(f.warmedRefs, ref)
	return f.warmEr
}
func (f *fakeInstaller) ServiceHealth(_ context.Context, url string) (bool, error) {
	f.steps = append(f.steps, "health")
	f.healthURL = url // the gate must probe the SERVICE, not the front door
	if !f.healthy {
		return false, errors.New("not ready")
	}
	return true, nil
}

// ServiceHealthOf is the by-name probe the install gate uses ([B.48]): the guest resolves the
// address from its routing table, so what the host is asserted to have asked for is a SERVICE,
// never a URL it assembled.
func (f *fakeInstaller) ServiceHealthOf(_ context.Context, service string) (bool, error) {
	f.steps = append(f.steps, "health")
	f.healthOf = service
	if !f.healthy {
		return false, errors.New("not ready")
	}
	return true, nil
}

// The service-install path has no opinion about names. "" -- this node publishes none -- is the
// honest answer here; a fixture name could be mistaken for an assertion that one was published.
func (f *fakeInstaller) MDNSPublished(context.Context) (string, error) { return "", nil }

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
		Resource:      drbd.Resource{Name: "r0", Device: "/dev/drbd0", Peers: []drbd.Peer{{Name: "n1", Address: "127.0.0.1:7789", Disk: drbd.DataDevice}}},
		Promoter:      promoterUnits(),
		HealthURL:     "http://192.168.1.100/healthz",
		ServiceCache:  "", // off: these tests assert orchestration, not persistence
		clock:         func() time.Time { return fixedNow },
	}
}

// fixedNow pins the moment a ring member is named for. A member's NAME carries its timestamp
// ([B.143]), so without a pinned clock a test can only assert the shape of the name it was
// handed — which is not an assertion about the name the product chose.
var fixedNow = time.Date(2026, 9, 22, 10, 30, 0, 0, time.UTC)

// wantMember is the member the product must name, DERIVED the way the product derives it.
// Restating the path as a literal is what the two rollback assertions used to do, and it is
// exactly what a rename makes vacuous: a literal asserts that somebody typed the same string
// twice, never that the caller and the renderer agree.
func wantMember(service string, tr quadlet.Trigger) string {
	return quadlet.SnapshotMember(service, tr, fixedNow)
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

func installService(cfg Config, f *fakeInstaller) api.DirectiveOutcome {
	d := api.Directive{ID: "d1", Kind: api.DirectiveServiceInstall, Payload: "home-assistant"}
	return cfg.applyServiceInstall(context.Background(), f, d, func(string, ...any) {})
}

// TestInstallOrdersTheSteps is the core contract, and what it asserts CHANGED with [V3b.3](f):
// there is no maintenance bracket. An install writes the volume and then tells the node to
// re-read it, so the ordering that matters is that nothing reaches the volume until the units and
// the image exist, and that converge comes after the manifest it is meant to find.
func TestInstallOrdersTheSteps(t *testing.T) {
	f := &fakeInstaller{primary: true, active: true, healthy: true}
	cfg := catalogFor(t, testManifest())
	if o := installService(cfg, f); o.State != api.OutcomeDone {
		t.Fatalf("outcome = %+v, want done", o)
	}
	// A fresh install: read the (empty) installed manifest, render+warm (which is also the
	// PREWARM every node does), provision the volume, converge, gate. No snapshot: nothing is
	// installed yet, so there is no rollback point to take. No pause/quiesce/rewrite: the chain is
	// static, and stopping the running containers is converge's job on the services whose
	// rendering actually changed.
	want := []string{
		"active?", "installed?:home-assistant", "render", "warm:briard-home-assistant-ha-image.service", "status", "provision:home-assistant",
		"converge", "health", "nudge",
	}
	if strings.Join(f.steps, ",") != strings.Join(want, ",") {
		t.Fatalf("steps = %v\nwant   %v", f.steps, want)
	}
	// The gate probes the SERVICE's own endpoint (manifest port + healthPath, in-guest), NOT the
	// VIP front door — the front door doesn't reflect a runtime-installed service.
	if f.healthOf != "home-assistant" {
		t.Fatalf("health probe asked about %q, want the installed service by name", f.healthOf)
	}
	// THE CHAIN IS NOT TOUCHED, and that is the whole shape change. It is static
	// (data -> services -> vip -> door, [B.125]; the door carries the household's names, [B.152])
	// and the installed service is not a member of it, which is what makes a service crash unable
	// to demote the node.
	if !slices.Equal(cfg.Promoter, []string{
		"briard-primary-storage.service", "briard-services.service", "briard-vip.service",
		"briard-reverse-proxy.service", "briard-dashboard.service",
	}) {
		t.Fatalf("install changed the promoter chain to %v — it must be static", cfg.Promoter)
	}
}

// TestAFailedNudgeDoesNotFailTheInstall ([B.131]). The service IS installed and serving by the
// time this runs, and the fallback for a Home Assistant that would not take the event is the
// behaviour that existed before the nudge did — it picks the change up at its next start. Failing
// the install would REVERT a working service because a courtesy call did not connect.
func TestAFailedNudgeDoesNotFailTheInstall(t *testing.T) {
	f := &fakeInstaller{primary: true, active: true, healthy: true, nudgeEr: errors.New("connection refused")}
	if o := installService(catalogFor(t, testManifest()), f); o.State != api.OutcomeDone {
		t.Fatalf("outcome = %+v, want done — a refused nudge reverted a healthy install", o)
	}
}

// TestTheNudgeIsFiredAfterTheGates, and both halves of that matter. AFTER, because a service that
// failed its gate is reverted and there is nothing to tell Home Assistant about. And on EVERY
// install, with no condition on which service it was: the caller holds no table of which service
// implies what, the signal carries nothing, and the guest answers "nobody to tell" for a node with
// no Home Assistant on its volume — modelled here by noHass.
func TestTheNudgeIsFiredAfterTheGates(t *testing.T) {
	cfg := catalogFor(t, testManifest())
	f := &fakeInstaller{primary: true, active: true, healthy: false}
	// A short budget rather than the 5-minute health gate, the way the revert tests below bound
	// theirs: what is under test is which steps ran, not how long the gate waits.
	o := func() api.DirectiveOutcome {
		d := api.Directive{ID: "d1", Kind: api.DirectiveServiceInstall, Payload: "home-assistant"}
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		defer cancel()
		return cfg.applyServiceInstall(ctx, f, d, func(string, ...any) {})
	}()
	if o.State != api.OutcomeRolledBack {
		t.Fatalf("outcome = %+v, want rolled-back", o)
	}
	if slices.Contains(f.steps, "nudge") {
		t.Fatalf("an install that failed its health gate still nudged Home Assistant: %v", f.steps)
	}
	f = &fakeInstaller{primary: true, active: true, healthy: true, noHass: true}
	if o := installService(catalogFor(t, testManifest()), f); o.State != api.OutcomeDone {
		t.Fatalf("outcome = %+v, want done", o)
	}
	if !slices.Contains(f.steps, "nudge") {
		t.Fatalf("a node with no Home Assistant was not asked; the caller is deciding what only the volume knows: %v", f.steps)
	}
}

// TestInstallWarmsBeforeTouchingThePromoter is the offline case, and the reason the warm exists
// at all: the .image units are WantedBy=multi-user.target, so a service installed AFTER boot never
// has them run, and the containers' Pull=never would then fail the chain at promotion.
//
// Warming at install time is also where it belongs — a human is watching and the CLI can report
// it — and the assertion here is that an unreachable image aborts BEFORE the volume is written. A
// node whose install failed for want of a network must be exactly as it was, not left with the
// volume naming a service whose image nobody has ([V3b.3](f) makes that worse than it was: the
// volume is what every future promotion, on every node, renders from).
func TestInstallWarmsBeforeTouchingTheVolume(t *testing.T) {
	f := &fakeInstaller{primary: true, active: true, healthy: true, warmEr: errors.New("no route to host")}
	o := installService(catalogFor(t, testManifest()), f)
	if o.State != api.OutcomeFailed || !strings.Contains(o.Detail, "warm image") {
		t.Fatalf("outcome = %+v, want a failure naming the image warm", o)
	}
	joined := strings.Join(f.steps, ",")
	for _, forbidden := range []string{"converge", "provision"} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("a failed warm still ran %q — the node was changed by a failed install: %v", forbidden, f.steps)
		}
	}
}

// TestInstallRefusesWhenTheBracketIsOpen is the interim guard, and the assertion that it refuses
// BEFORE writing anything — a refusal must leave the node exactly as it was.
//
// It SURVIVES the bracket's deletion ([V3b.3](f)) even though an install no longer pauses the
// promoter itself, and deliberately: a paused promoter means an OS upgrade is in
// flight, and starting a second operation on that node is the overlap [V3b.5](b) exists to
// serialise. Dropping this check because we no longer take the bracket would quietly WIDEN
// concurrency at the moment the item narrowed it.
func TestInstallRefusesWhenTheBracketIsOpen(t *testing.T) {
	f := &fakeInstaller{primary: true, active: false, healthy: true}
	o := installService(catalogFor(t, testManifest()), f)
	if o.State != api.OutcomeFailed || !strings.Contains(o.Detail, "already paused") {
		t.Fatalf("outcome = %+v, want a failure naming the open bracket", o)
	}
	for _, s := range f.steps {
		if s == "render" || strings.HasPrefix(s, "provision") || s == "converge" {
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
	// The revert converges a SECOND time — that is what puts the node back, now that there is no
	// chain to rewrite: the volume is corrected first (here, by forgetting a fresh install's
	// manifest) and then re-read.
	joined := strings.Join(f.steps, ",")
	if n := strings.Count(joined, "converge"); n != 2 {
		t.Fatalf("converged %d times, want 2 (install then revert): %v", n, f.steps)
	}
	// The correction reaches the VOLUME before the re-converge, not after it. Reversed, the node
	// would re-render the broken service it was meant to be dropping.
	if fi, ci := strings.Index(joined, "forget:"), strings.LastIndex(joined, "converge"); fi < 0 || fi > ci {
		t.Fatalf("the volume was not corrected before the re-converge: %v", f.steps)
	}
}

// priorManifest is the same service at an earlier version — a real UPGRADE target (same name ⇒
// same unit names + DataRoot, different bytes so priorService treats it as a rollback point).
// priorManifest is the volume's recorded state before an upgrade, in the shape the volume now
// has: keyed by service name. The raw bytes come back too, because the assertions compare against
// exactly what the rollback must re-record.
func priorManifest(t *testing.T) (map[string]string, string) {
	t.Helper()
	m := testManifest()
	m.Version = "2026.6.0"
	body, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return map[string]string{m.Name: string(body)}, string(body)
}

// TestUpgradeRollsBackDataAndManifest is the (i) contract at the orchestration layer: a broken
// UPGRADE must put back the prior service's DATA and IDENTITY, not merely take the new one out of
// the chain. So the path snapshots before the switch, and on a tripped gate it restores the
// subvolume and re-records the prior manifest — the service-level twin of the {code+data} OS
// rollback. (The real btrfs/quadlet/front-door mechanics are the job of service-install-broken.nix;
// this asserts the host drives them in the right order.)
func TestUpgradeRollsBackDataAndManifest(t *testing.T) {
	prior, priorRaw := priorManifest(t)
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
	snap := "snapshot:" + wantMember("home-assistant", quadlet.TriggerUpgrade)
	// The rollback point is taken BEFORE the volume is mutated (provision) and before the switch.
	si, pi := strings.Index(joined, snap), strings.Index(joined, "provision")
	if si < 0 || pi < 0 || si > pi {
		t.Fatalf("snapshot must precede provision: %v", f.steps)
	}
	// The data is rolled back from that exact snapshot.
	if !strings.Contains(joined, "restore:"+wantMember("home-assistant", quadlet.TriggerUpgrade)) {
		t.Fatalf("a failed upgrade did not restore the data subvolume: %v", f.steps)
	}
	// The volume ends up holding the PRIOR manifest again — the identity is reverted, not just the
	// chain. provision is called with the new manifest, then again with the prior on rollback.
	if n := len(f.manifests); n < 2 || f.manifests[n-1] != priorRaw {
		t.Fatalf("volume manifest not reverted to the prior: got %v", f.manifests)
	}
	// Converged twice: forward (onto the new manifest) and again on the revert (onto the prior
	// one, re-recorded above). No pause/resume pair, because there is no bracket left to hold.
	if n := strings.Count(joined, "converge"); n != 2 {
		t.Fatalf("converged %d times, want 2 (install then revert): %v", n, f.steps)
	}
	// The POD is NEVER stopped — only its container. Stopping the pod makes podman kill its
	// members out from under their units, so each lands in `failed` rather than stopping cleanly,
	// and the container is what holds the data bind the restore needs released.
	if strings.Contains(joined, "stop:briard-home-assistant-pod.service") {
		t.Fatalf("quiesce stopped the POD — that unmounts the shared volume: %v", f.steps)
	}
	if !strings.Contains(joined, "stop:briard-home-assistant-ha.service") {
		t.Fatalf("quiesce never stopped the container, so the data bind was never released: %v", f.steps)
	}
}

// TestUpgradeDoesNotConvergeIfDataCannotRestore: if the data rollback itself fails, the revert
// must NOT put the prior service back — it would run on the poisoned data. So it stops and
// reports both failures, leaving the service down for a human.
//
// The blast radius SHRANK with the bracket's deletion ([V3b.3](f)). This used to leave the
// promoter PAUSED — a node that could not fail over at all — because that was the only lever
// available for "do not start the service again". Now the lever is simply not converging: the one
// service stays stopped, and the node keeps serving everything else and keeps its ability to fail
// over. Same refusal, a fraction of the cost.
func TestUpgradeDoesNotConvergeIfDataCannotRestore(t *testing.T) {
	cfg := catalogFor(t, testManifest())
	f := &fakeInstaller{primary: true, active: true, healthy: false, prior: mustPrior(t), restoreEr: errors.New("btrfs: cannot delete subvolume")}
	o := func() api.DirectiveOutcome {
		d := api.Directive{ID: "d1", Kind: api.DirectiveServiceInstall, Payload: "home-assistant"}
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		defer cancel()
		return cfg.applyServiceInstall(ctx, f, d, func(string, ...any) {})
	}()
	if o.State != api.OutcomeFailed || !strings.Contains(o.Detail, "restore the data subvolume") {
		t.Fatalf("outcome = %+v, want a failure naming the data restore", o)
	}
	// Only the forward converge ran; the revert deliberately did not converge back.
	if n := strings.Count(strings.Join(f.steps, ","), "converge"); n != 1 {
		t.Fatalf("converged %d times, want 1 — the revert must NOT restart the service onto un-restored data: %v", n, f.steps)
	}
}

// TestAFailedFreshInstallLeavesNothingOnTheVolume is a requirement [V3b.3](f) CREATED. Reverting
// used to mean putting the node-local promoter chain back, and that chain simply did not name the
// new service -- so the manifest the install had written to the volume could be left behind
// harmlessly. Under converge the volume is the truth, so a manifest nobody removed is a service
// every future promotion, on every node in the flock, renders and tries to start. A failed FRESH
// install must therefore remove the identity, not merely stop the units.
func TestAFailedFreshInstallLeavesNothingOnTheVolume(t *testing.T) {
	cfg := catalogFor(t, testManifest())
	f := &fakeInstaller{primary: true, active: true, healthy: false} // no prior => a fresh install
	o := func() api.DirectiveOutcome {
		d := api.Directive{ID: "d1", Kind: api.DirectiveServiceInstall, Payload: "home-assistant"}
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		defer cancel()
		return cfg.applyServiceInstall(ctx, f, d, func(string, ...any) {})
	}()
	if o.State != api.OutcomeRolledBack {
		t.Fatalf("outcome = %+v, want rolled-back", o)
	}
	if !strings.Contains(strings.Join(f.steps, ","), "forget:home-assistant") {
		t.Fatalf("the failed service is still recorded on the volume: %v", f.steps)
	}
}

// TestInstallOnSecondaryRendersButDoesNotProvision: the volume is mounted only on the Primary, so
// a secondary takes its units and stops. It must NOT touch the promoter — the primary owns that.
func TestInstallOnSecondaryRendersButDoesNotProvision(t *testing.T) {
	f := &fakeInstaller{primary: false, active: true, healthy: true}
	o := installService(catalogFor(t, testManifest()), f)
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
	if o := installService(cfg, f); o.State != api.OutcomeFailed {
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
	if strings.Contains(strings.Join(f.steps, ","), "converge") {
		t.Errorf("prewarm converged the node — it must change nothing this node is serving: %v", f.steps)
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
// telemetry at all -- per-service footprint, volume usage, snapshots, load, journal and store sizes.
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
		cfg := Config{ServiceCache: dir, Promoter: promoterUnits()}
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
		if len(cfg.ServiceRendered.Units) == 0 {
			t.Errorf("rendered units not adopted: %v", cfg.ServiceRendered.Units)
		}
		// The chain is NOT adopted — it is static ([V3b.3](f)). An install that moved it would be
		// the node-was-told model converge exists to remove, and it is what let a survivor promote
		// onto whatever it happened to have rendered.
		if !slices.Equal(cfg.Promoter, promoterUnits()) {
			t.Errorf("an install moved the promoter chain to %v — it must stay static", cfg.Promoter)
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
			{"another kind", api.Directive{Kind: api.DirectiveUpgradeSystem}, done},
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
	o := installService(cfg, &fakeInstaller{primary: true, active: true, healthy: true})
	if o.State != api.OutcomeDone {
		t.Fatalf("outcome = %+v, want done", o)
	}
	// The SERVICE's own name, on the door's :80 ([B.48]). The port that used to be in this
	// sentence was the address of a service reached around the front door rather than through it.
	if want := "reach it at http://briard-picked-hornet-home-assistant.local/"; o.Detail != want {
		t.Fatalf("Detail = %q, want %q", o.Detail, want)
	}
}

// ...and with NO published name there is no URL to promise. A witness, or a node whose FLOCK_NAME
// is unset, publishes nothing over mDNS, so naming the port is the most that can be said
// truthfully -- inventing a host for it is exactly the plausible-but-wrong address [V3.17] exists
// to end. Asserted as an absence AND a presence: the port must be named, the URL must not appear.
func TestInstallWithoutAPublishedNameNamesOnlyThePort(t *testing.T) {
	cfg := catalogFor(t, testManifest()) // FlockName left zero on purpose
	o := installService(cfg, &fakeInstaller{primary: true, active: true, healthy: true})
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
	specs, rendered, ok := cfg.installedServices(func(string, ...any) {})
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
	specs, rendered, ok := cfg.installedServices(func(string, ...any) {})
	if !ok || len(specs) != 1 || specs[0].Name != "home-assistant" {
		t.Fatalf("specs = %+v (ok=%v), want just home-assistant", specs, ok)
	}
	for file := range rendered.Files {
		if strings.Contains(file, "mosquitto") {
			t.Errorf("rendered %q from a manifest that does not parse", file)
		}
	}
}

// A SECOND SERVICE IS NOW ALLOWED, and it must not disturb the first. This test used to assert
// the opposite: the install path refused a second distinct service, because nothing on any seam
// could name WHICH service it meant, so a node running two and describing one would have had the
// cloud confirm a rollout against whichever came first and a crash-loop in the other go unseen.
// All three of those now name a service -- NodeStatus.Services (per-service manifest identity),
// the volume's .services/<name>.json, and telemetry's per-service Payloads -- so the refusal is
// gone ([V3b.3](b)).
//
// What matters is not that it succeeds but that the FIRST service survives it, which is the
// failure the refusal was standing in for: the install must ask the volume about ITS OWN service,
// and must not remove the other's rendered units as a renamed prior's orphans.
func TestInstallAcceptsASecondServiceAndLeavesTheFirstAlone(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "mosquitto.json"), manifestJSON("mosquitto", digestB), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := catalogFor(t, testManifest()) // testManifest is home-assistant
	cfg.ServiceCache = dir
	// The volume holds mosquitto's manifest; home-assistant's install must not read it as its own
	// prior, which is what made the second install destructive.
	f := &fakeInstaller{primary: true, active: true, healthy: true,
		prior: map[string]string{"mosquitto": string(manifestJSON("mosquitto", digestB))}}
	o := installService(cfg, f)
	if o.State != api.OutcomeDone {
		t.Fatalf("outcome = %+v, want done: a node may now run more than one service", o)
	}
	if !slices.Contains(f.steps, "installed?:home-assistant") {
		t.Errorf("steps = %v, want the volume read to NAME home-assistant", f.steps)
	}
	// No stale list: mosquitto's units are not home-assistant's prior, so nothing of its is removed.
	for _, s := range f.staleRemoved {
		if strings.Contains(s, "mosquitto") {
			t.Errorf("installing home-assistant removed mosquitto's unit %q -- the first service was collateral", s)
		}
	}
	// Both services end up in the cache, so bring-up knows about both.
	specs, _, ok := cfg.installedServices(func(string, ...any) {})
	if !ok || len(specs) != 2 {
		t.Fatalf("cache holds %+v (ok=%v), want both services", specs, ok)
	}
}

// ...and an UPGRADE of a service already installed is still one path with install, which is why
// the same-name case was always the other half of this assertion.
func TestInstallOfTheSameServiceIsAnUpgrade(t *testing.T) {
	up := t.TempDir()
	if err := os.WriteFile(filepath.Join(up, "home-assistant.json"), manifestJSON("home-assistant", digestA), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := catalogFor(t, testManifest())
	cfg.ServiceCache = up
	if o := installService(cfg, &fakeInstaller{primary: true, active: true, healthy: true}); o.State != api.OutcomeDone {
		t.Fatalf("outcome = %+v, want done: re-installing the same service is an upgrade", o)
	}
}

// A cache directory that cannot be read must not be spelled the same way as an empty one. On the
// path whose whole job is to refuse a second service, "I could not tell" reading as "nothing is
// installed" is what would let the second one through.

// The install path ENSURES the image is present rather than starting its .image unit blindly.
// Starting one is a `podman image pull`, so an unconditional start cannot install a service whose
// image was staged locally (there is no registry to pull from) and needlessly re-fetches one a
// prewarm already placed. The ref must reach the guest, because the check is on the ref and the
// action is on the unit -- a call that passed only the unit could not tell the two apart.
//
// Measured, not hypothesised: the fleet's first catalog install died on
// `warm image (briard-dummy-app-image.service): ... Job for briard-dummy-app-image.service failed`
// for exactly this reason ([V3b.3](e1)).
func TestInstallEnsuresTheImageRatherThanPullingIt(t *testing.T) {
	f := &fakeInstaller{primary: true, active: true, healthy: true}
	if o := installService(catalogFor(t, testManifest()), f); o.State != api.OutcomeDone {
		t.Fatalf("outcome = %+v, want done", o)
	}
	if len(f.warmedRefs) == 0 {
		t.Fatal("no image was warmed at all")
	}
	for _, ref := range f.warmedRefs {
		if ref == "" {
			t.Errorf("ServiceWarm got an empty ref (%v) — the guest cannot check presence without it", f.warmedRefs)
		}
	}
	want := testManifest().Primary().Image
	if f.warmedRefs[0] != want {
		t.Errorf("warmed ref = %q, want the manifest's image %q", f.warmedRefs[0], want)
	}
}

// THE WIRE-CONTRACT WIDENING'S GROUND TRUTH ([V3b.3](b)): a spec must carry the identity of the
// manifest it was installed from, and that identity must be the hash of the BYTES ON DISK.
//
// The distinction is the whole test. shared/manifest.Parse hashes the exact signed document
// precisely because re-encoding a parsed struct can reorder or reformat and would mint a
// different identity for the same signed bytes -- so an implementation that re-marshalled the
// Manifest and hashed that would look entirely reasonable, produce a stable-looking value, and
// disagree with every identity the catalog ever published. The expectation here is therefore
// computed from the file, never from the struct.
func TestInstalledServicesCarriesTheManifestIdentity(t *testing.T) {
	dir := t.TempDir()
	want := map[string]string{}
	for _, s := range []struct{ name, digest string }{{"home-assistant", digestA}, {"mosquitto", digestB}} {
		// INDENTED, which is what makes this test bite. A catalog document is a signed FILE, and
		// a file is formatted; compact fixture bytes that happen to equal json.Marshal's output
		// cannot distinguish hashing them from hashing a re-marshalling, so the sabotage that
		// proves this assertion would pass against a compact one.
		raw := indentJSON(t, manifestJSON(s.name, s.digest))
		if err := os.WriteFile(filepath.Join(dir, s.name+".json"), raw, 0o600); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(raw)
		want[s.name] = "sha256:" + hex.EncodeToString(sum[:])
	}
	cfg := Config{ServiceCache: dir}
	specs, _, ok := cfg.installedServices(func(string, ...any) {})
	if !ok || len(specs) != 2 {
		t.Fatalf("specs = %+v (ok=%v), want two", specs, ok)
	}
	for _, s := range specs {
		if s.Manifest != want[s.Name] {
			t.Errorf("%s Manifest = %q, want %q (the hash of the bytes on disk)", s.Name, s.Manifest, want[s.Name])
		}
	}
	// Two different manifests must not share an identity -- a constant would satisfy every
	// assertion above and confirm any rollout against any other.
	if specs[0].Manifest == specs[1].Manifest {
		t.Errorf("both services report identity %q; a shared identity confirms one rollout with another's evidence", specs[0].Manifest)
	}
}

// What the node PUTS ON THE WIRE for its services, and the one thing it must leave off.
//
// NodeStatus.Services is compared by the cloud against a catalog hash, so an entry whose identity
// is the empty string is worse than no entry at all: dropping it says "this node runs something I
// cannot name", publishing it says "this node runs the service whose identity is nothing", and a
// confirmation loop reads the second as a mismatch it can never resolve. specOf builds every spec
// from signed manifest bytes so the case is unreachable -- asserted anyway, because it is the
// guard that keeps it unreachable.
func TestServiceStatusesReportOnlyServicesItCanName(t *testing.T) {
	cfg := Config{Services: []model.ServiceSpec{
		{Name: "home-assistant", Manifest: "sha256:aa"},
		{Name: "nameless"}, // no manifest identity: must never reach the wire
		{Name: "mosquitto", Manifest: "sha256:bb"},
	}}
	got := cfg.serviceStatuses(context.Background(), fakeStatus{}, false)
	want := []api.ServiceStatus{
		{Name: "home-assistant", Manifest: "sha256:aa"},
		{Name: "mosquitto", Manifest: "sha256:bb"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("serviceStatuses() = %+v, want %+v", got, want)
	}
}

// PER-SERVICE STATE, the [V3b.3](f) half. It exists because the promoter no longer watches these
// units -- taking them out of the chain is what stops a crashed container from demoting the node,
// and it also means nothing else notices one has died.
//
// The two answers are asserted together because they are only meaningful against each other: a
// running service and a dead one, on the same node, in the same report.
func TestServiceStatusesReportPerServiceState(t *testing.T) {
	cfg := Config{Services: []model.ServiceSpec{
		{Name: "home-assistant", Manifest: "sha256:aa", Unit: "briard-home-assistant-ha.service"},
		{Name: "mosquitto", Manifest: "sha256:bb", Unit: "briard-mosquitto-mq.service"},
	}}
	r := fakeStatus{active: map[string]bool{"briard-home-assistant-ha.service": true}}
	got := cfg.serviceStatuses(context.Background(), r, true)
	want := []api.ServiceStatus{
		{Name: "home-assistant", Manifest: "sha256:aa", State: api.StateRunning},
		{Name: "mosquitto", Manifest: "sha256:bb", State: api.StateStopped},
	}
	if !slices.Equal(got, want) {
		t.Errorf("serviceStatuses() = %+v, want %+v", got, want)
	}
}

// A SECONDARY REPORTS NO STATE, and that is not a detail. The services run on whoever holds the
// volume, so a standby is SUPPOSED to be running nothing -- reporting `stopped` there would make
// an ordinary secondary indistinguishable from a primary whose service died, which is exactly the
// confusion this field exists to end.
func TestServiceStatusesLeaveStateEmptyOnASecondary(t *testing.T) {
	cfg := Config{Services: []model.ServiceSpec{
		{Name: "home-assistant", Manifest: "sha256:aa", Unit: "briard-home-assistant-ha.service"},
	}}
	got := cfg.serviceStatuses(context.Background(), fakeStatus{}, false)
	if len(got) != 1 || got[0].State != "" {
		t.Errorf("serviceStatuses() on a secondary = %+v, want the state left empty", got)
	}
}

// "We could not ask" is not "it is down" either, and a read error must cost only the ONE service
// it happened to. Losing the whole set to a transient verb error is how a real outage hides.
func TestServiceStatusesLeaveStateEmptyWhenTheGuestCannotAnswer(t *testing.T) {
	cfg := Config{Services: []model.ServiceSpec{
		{Name: "home-assistant", Manifest: "sha256:aa", Unit: "briard-home-assistant-ha.service"},
	}}
	r := fakeStatus{activeErr: errors.New("channel down")}
	got := cfg.serviceStatuses(context.Background(), r, true)
	if len(got) != 1 {
		t.Fatalf("a failed state read dropped the service entirely: %+v", got)
	}
	if got[0].State != "" {
		t.Errorf("State = %q after a failed read, want empty — an unanswerable question is not a `stopped` answer", got[0].State)
	}
	if got[0].Manifest != "sha256:aa" {
		t.Errorf("the identity was lost with the state: %+v", got[0])
	}
}

// A witness and the shipped zero-service node report NOTHING here, and they must report it as an
// absent field rather than an empty list: `services: []` on the wire says "I looked and there are
// none", which is a claim a node with no service cache is in no position to make.
func TestServiceStatusesAreAbsentWithNoServices(t *testing.T) {
	if got := (Config{}).serviceStatuses(context.Background(), fakeStatus{}, true); got != nil {
		t.Errorf("serviceStatuses() = %+v, want nil so the field is omitted entirely", got)
	}
}

// indentJSON re-formats a manifest fixture the way a published catalog document is: whitespace a
// re-marshalling does not reproduce. Its only job is to make the identity assertion above able to
// fail; see there.
func indentJSON(t *testing.T, raw []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "", "  "); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// mustPrior is priorManifest for callers that only need the volume's state, not its bytes.
func mustPrior(t *testing.T) map[string]string {
	t.Helper()
	p, _ := priorManifest(t)
	return p
}

// The install asks the volume about THIS service, and a manifest belonging to a DIFFERENT one
// must not answer for it. That is the whole of the per-service split at the host layer
// ([V3b.3](b)): reading the volume's single unnamed manifest made an install of mosquitto see
// home-assistant's manifest as its own prior -- so it snapshotted the wrong rollback point, and
// filesToRemove then deleted home-assistant's rendered units as a renamed prior's orphans.
func TestPriorServiceReadsOnlyItsOwnService(t *testing.T) {
	other, _ := priorManifest(t) // home-assistant's manifest, on the volume
	cfg := catalogFor(t, testManifest())
	f := &fakeInstaller{primary: true, active: true, healthy: true, prior: other}
	// Install a DIFFERENT service; the volume holds only home-assistant's manifest.
	prior, subdirs, raw, _ := cfg.priorService(context.Background(), f, "mosquitto", []byte(`{}`), func(string, ...any) {})
	if prior != nil || subdirs != nil || raw != "" {
		t.Fatalf("prior = %v/%v/%q for mosquitto, want none -- another service's manifest answered for it", prior, subdirs, raw)
	}
	// And the read named the service it was asked about, rather than asking for "the" manifest.
	if !slices.Contains(f.steps, "installed?:mosquitto") {
		t.Errorf("steps = %v, want the volume read to NAME mosquitto", f.steps)
	}
}

// A guest that cannot name a service's identity on the volume must be refused BEFORE anything is
// written, not driven into recording this service's manifest over whatever it already runs. Alpha
// is reinstall-only, so "refuse loudly" is the whole answer -- there is no compat path to build.
func TestInstallRefusesAGuestThatCannotNameAService(t *testing.T) {
	cfg := catalogFor(t, testManifest())
	f := &fakeInstaller{primary: true, active: true, healthy: true, oldGuest: true}
	o := installService(cfg, f)
	if o.State != api.OutcomeFailed || !strings.Contains(o.Detail, "service.installed") {
		t.Fatalf("outcome = %+v, want a failure naming the missing verb", o)
	}
	if len(f.steps) != 0 {
		t.Fatalf("the refusal still touched the guest: %v", f.steps)
	}
}

// volumeGuest is the slice of the guest adoptVolumeServices reads: what the replicated volume
// says this node runs, keyed by service name.
type volumeGuest struct {
	manifests map[string]string
	listErr   error
	tooOld    bool
}

func (v volumeGuest) ServiceList(context.Context) ([]string, error) {
	names := make([]string, 0, len(v.manifests))
	for n := range v.manifests {
		names = append(names, n)
	}
	sort.Strings(names)
	return names, v.listErr
}

func (v volumeGuest) ServiceInstalled(_ context.Context, name string) (string, error) {
	return v.manifests[name], nil
}

func (v volumeGuest) SupportsServiceList() bool { return !v.tooOld }

// THE CONVERGED SURVIVOR ([V3b.3](e1), measured on a fleet run 2026-08-28). A node that promoted
// into somebody else's install has an EMPTY node-local cache -- only a completed install on a
// Primary writes one -- while converge-at-promotion has it running what the volume names. Reading
// the volume is what lets it say so; without this it served the fixture at the VIP while
// reporting no services at all, which is a household degraded to nothing in every view.
func TestAdoptVolumeServicesOnANodeThatInstalledNothing(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{ServiceCache: dir}
	g := volumeGuest{manifests: map[string]string{
		"home-assistant": string(manifestJSON("home-assistant", digestA)),
		"mosquitto":      string(manifestJSON("mosquitto", digestB)),
	}}
	cfg.adoptVolumeServices(context.Background(), g, func(string, ...any) {})

	if len(cfg.Services) != 2 {
		t.Fatalf("services = %+v, want the two the volume names", cfg.Services)
	}
	if cfg.Services[0].Name != "home-assistant" || cfg.Services[1].Name != "mosquitto" {
		t.Errorf("names = %q,%q, want home-assistant,mosquitto", cfg.Services[0].Name, cfg.Services[1].Name)
	}
	// The IDENTITY is what the cloud confirms a rollout against, so it must be the hash of the
	// volume's own bytes rather than anything re-derived.
	_, want, err := manifest.Parse(manifestJSON("home-assistant", digestA))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Services[0].Manifest != string(want) {
		t.Errorf("manifest = %q, want the volume's identity %q", cfg.Services[0].Manifest, want)
	}
	if len(cfg.ServiceRendered.Units) == 0 {
		t.Error("the rendered units must come with the specs -- a standby replays them after a guest reboot")
	}
	// CACHED, so this node's own restart brings up what it is running rather than what it once
	// installed (which here is nothing at all).
	for _, name := range []string{"home-assistant", "mosquitto"} {
		if _, err := os.ReadFile(filepath.Join(dir, name+".json")); err != nil {
			t.Errorf("the volume's manifest for %s was not cached: %v", name, err)
		}
	}
}

// Every failure is SOFT, because this is a report and not a decision: a guest too old to list, or
// a read that fails, leaves the previous view standing rather than blanking what the node says it
// runs. A stale answer beats an invented one.
func TestAdoptVolumeServicesKeepsTheOldViewOnFailure(t *testing.T) {
	prior := []model.ServiceSpec{{Name: "home-assistant", Manifest: "sha256:prior"}}
	for _, tc := range []struct {
		name string
		g    volumeGuest
	}{
		{"a guest too old to list", volumeGuest{tooOld: true, manifests: map[string]string{"mosquitto": string(manifestJSON("mosquitto", digestB))}}},
		{"a failed read", volumeGuest{listErr: errors.New("channel down")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &Config{ServiceCache: t.TempDir(), Services: prior}
			cfg.adoptVolumeServices(context.Background(), tc.g, func(string, ...any) {})
			if len(cfg.Services) != 1 || cfg.Services[0].Manifest != "sha256:prior" {
				t.Fatalf("services = %+v, want the prior view untouched", cfg.Services)
			}
		})
	}
}

// A manifest the volume carries but this build cannot parse costs THAT service and no other: one
// unusable file must not blank a node's whole report.
func TestAdoptVolumeServicesSkipsOnlyTheBadManifest(t *testing.T) {
	cfg := &Config{ServiceCache: t.TempDir()}
	g := volumeGuest{manifests: map[string]string{
		"home-assistant": string(manifestJSON("home-assistant", digestA)),
		"mosquitto":      `{"name":"mosquitto","containers":[]}`, // no primary container
	}}
	cfg.adoptVolumeServices(context.Background(), g, func(string, ...any) {})
	if len(cfg.Services) != 1 || cfg.Services[0].Name != "home-assistant" {
		t.Fatalf("services = %+v, want only the one that parses", cfg.Services)
	}
}

// upgradeWith runs an UPGRADE (a prior manifest is installed) whose floor passes, with the S1
// samples the fake will answer. The settle window is squeezed to nothing: the production minute
// is there to let HA's retry backoff run, and a fake has nothing to wait for.
func upgradeWith(t *testing.T, f *fakeInstaller) api.DirectiveOutcome {
	t.Helper()
	cfg := catalogFor(t, testManifest())
	cfg.readinessSettle = time.Millisecond
	f.prior = mustPrior(t)
	f.primary, f.active, f.healthy = true, true, true
	d := api.Directive{ID: "d1", Kind: api.DirectiveServiceInstall, Payload: "home-assistant"}
	return cfg.applyServiceInstall(context.Background(), f, d, func(string, ...any) {})
}

func sample(states ...string) []hass.Entry {
	out := make([]hass.Entry, len(states))
	for i, st := range states {
		out[i] = hass.Entry{ID: string(rune('a' + i)), Domain: "d" + string(rune('a'+i)), State: st}
	}
	return out
}

// TestUpgradeRollsBackOnAReadinessRegression is the whole point of [V3b.29]: the floor PASSES —
// the fake reports healthy, exactly as Home Assistant answering /manifest.json with a 200 does —
// and the upgrade is reverted anyway, because the integrations that were working stopped working.
// Without this layer that install reports success and the household quietly loses half its house.
func TestUpgradeRollsBackOnAReadinessRegression(t *testing.T) {
	f := &fakeInstaller{readiness: [][]hass.Entry{
		sample("loaded", "loaded"),
		sample("setup_error", "setup_error"),
	}}
	o := upgradeWith(t, f)
	if o.State != api.OutcomeRolledBack {
		t.Fatalf("outcome = %+v, want rolled-back (the floor passed; the gate above it must not)", o)
	}
	joined := strings.Join(f.steps, ",")
	// The SAME {code + data} revert a failed floor drives — the verdict changes, the undo does not.
	if !strings.Contains(joined, "restore:"+wantMember("home-assistant", quadlet.TriggerUpgrade)) {
		t.Fatalf("a tripped readiness gate did not restore the data subvolume: %v", f.steps)
	}
	if n := strings.Count(joined, "converge"); n != 2 {
		t.Fatalf("converged %d times, want 2 (install then revert): %v", n, f.steps)
	}
}

// TestUpgradeCapturesTheBaselineBeforeTheSnapshot: the baseline is the confounder control, so it
// must be a sample of the service as it was — before the rollback point is taken and before
// anything is written to the volume.
//
// ⚠️ It must also stay above the `stop` [B.143] put before the snapshot: a baseline captured
// after a stop is a baseline of a service that is not running, which reads as every integration
// having regressed and would revert every upgrade. That is the `ri > qi` assertion below.
func TestUpgradeCapturesTheBaselineBeforeTheSnapshot(t *testing.T) {
	f := &fakeInstaller{readiness: [][]hass.Entry{sample("loaded"), sample("loaded")}}
	if o := upgradeWith(t, f); o.State != api.OutcomeDone {
		t.Fatalf("outcome = %+v, want done", o)
	}
	joined := strings.Join(f.steps, ",")
	ri := strings.Index(joined, "readiness:")
	qi := strings.Index(joined, "stop:")
	si := strings.Index(joined, "snapshot:")
	pi := strings.Index(joined, "provision")
	if ri < 0 {
		t.Fatalf("no baseline was captured on an upgrade: %v", f.steps)
	}
	if qi < 0 || ri > qi {
		t.Fatalf("the baseline must precede the stop — a stopped service samples as wholly regressed: %v", f.steps)
	}
	if si < 0 || ri > si {
		t.Fatalf("the baseline must precede the snapshot: %v", f.steps)
	}
	if pi < 0 || ri > pi {
		t.Fatalf("the baseline must precede provision — the volume is already mutated by then: %v", f.steps)
	}
	if f.readinessHit != 2 {
		t.Fatalf("sampled %d times, want 2 (baseline, then settled)", f.readinessHit)
	}
}

// TestUpgradeStopsTheServiceBeforeSnapshottingIt is [B.143]'s rollback-point rule, and
// `services-pair.nix` measured why it is one: taken live, the point did not contain the retained
// message mosquitto had already accepted, so the rollback restored an empty broker ([V3b.4](c)).
// A live snapshot is crash-consistent at best, and only Home Assistant is measured to survive that.
//
// It asserts the CONTAINER unit by name, never the pod: stopping the pod makes podman kill the
// containers, their units fail, and the target unmounts the shared volume under every other
// service (quiesce's own comment carries the trace).
func TestUpgradeStopsTheServiceBeforeSnapshottingIt(t *testing.T) {
	f := &fakeInstaller{readiness: [][]hass.Entry{sample("loaded"), sample("loaded")}}
	if o := upgradeWith(t, f); o.State != api.OutcomeDone {
		t.Fatalf("outcome = %+v, want done", o)
	}
	joined := strings.Join(f.steps, ",")
	qi := strings.Index(joined, "stop:briard-home-assistant-ha.service")
	si := strings.Index(joined, "snapshot:")
	if qi < 0 {
		t.Fatalf("the upgrade never stopped the prior container before snapshotting it: %v", f.steps)
	}
	if si < 0 || qi > si {
		t.Fatalf("the rollback point was taken on a RUNNING service: %v", f.steps)
	}
	if strings.Contains(joined, "stop:briard-home-assistant.pod") {
		t.Fatalf("the pod was stopped — that unmounts the shared volume under every other service: %v", f.steps)
	}
}

// TestFreshInstallStopsNothing: there is no prior service to stop, and a stop of something that
// was never installed would be the install path inventing an outage for a node that had none.
func TestFreshInstallStopsNothing(t *testing.T) {
	cfg := catalogFor(t, testManifest())
	cfg.readinessSettle = time.Millisecond
	f := &fakeInstaller{primary: true, active: true, healthy: true} // no prior => fresh
	if o := installService(cfg, f); o.State != api.OutcomeDone {
		t.Fatalf("outcome = %+v, want done", o)
	}
	for _, s := range f.steps {
		if strings.HasPrefix(s, "stop:") {
			t.Fatalf("a fresh install stopped %q: %v", s, f.steps)
		}
	}
}

// TestAFailedRollbackPointRestartsTheService: [B.121]'s ruling is that a failed upgrade snapshot
// INTERRUPTS the service — not that it abandons it. The stop has already happened by then, so the
// undo is a re-converge against the volume, which still names the PRIOR manifest because provision
// has not run. Without this the household loses Home Assistant because a btrfs snapshot failed.
func TestAFailedRollbackPointRestartsTheService(t *testing.T) {
	f := &fakeInstaller{
		readiness: [][]hass.Entry{sample("loaded"), sample("loaded")},
		snapEr:    errors.New("no space left on device"),
	}
	o := upgradeWith(t, f)
	if o.State != api.OutcomeFailed {
		t.Fatalf("outcome = %+v, want failed", o)
	}
	if !strings.Contains(o.Detail, "no space left on device") {
		t.Fatalf("the outcome does not name what failed: %q", o.Detail)
	}
	if !strings.Contains(o.Detail, "restarted") {
		t.Fatalf("the outcome does not tell the household the service came back: %q", o.Detail)
	}
	joined := strings.Join(f.steps, ",")
	qi := strings.Index(joined, "stop:")
	si := strings.Index(joined, "snapshot:")
	ci := strings.Index(joined, "converge")
	if qi < 0 || si < 0 {
		t.Fatalf("the test did not reach the stop and the snapshot: %v", f.steps)
	}
	if ci < 0 || ci < si {
		t.Fatalf("the service was left stopped after the snapshot failed: %v", f.steps)
	}
	if strings.Contains(joined, "provision") {
		t.Fatalf("the volume was mutated after the rollback point could not be taken: %v", f.steps)
	}
}

// TestFreshInstallHasNoBaseline: a differential gate has nothing to differ against on a first
// install — the service was not running, so there is no "was loaded" to regress from. The floor
// is the honest gate there, and this is deliberate rather than incidental: it is also the case
// where sampling would be asking a service that does not exist yet.
func TestFreshInstallHasNoBaseline(t *testing.T) {
	cfg := catalogFor(t, testManifest())
	cfg.readinessSettle = time.Millisecond
	f := &fakeInstaller{primary: true, active: true, healthy: true} // no prior => fresh
	d := api.Directive{ID: "d1", Kind: api.DirectiveServiceInstall, Payload: "home-assistant"}
	if o := cfg.applyServiceInstall(context.Background(), f, d, func(string, ...any) {}); o.State != api.OutcomeDone {
		t.Fatalf("outcome = %+v, want done", o)
	}
	if f.readinessHit != 0 {
		t.Fatalf("a fresh install sampled readiness %d times, want 0", f.readinessHit)
	}
}

// TestUpgradeKeepsOnAHold: one terminal regression is the ambiguous middle. The upgrade stands —
// reverting a household's service on a single entry that might legitimately have been removed is
// the worse error — and the rollback window plus the user remain the backstop.
func TestUpgradeKeepsOnAHold(t *testing.T) {
	f := &fakeInstaller{readiness: [][]hass.Entry{
		sample("loaded", "loaded"),
		sample("setup_error", "loaded"),
	}}
	if o := upgradeWith(t, f); o.State != api.OutcomeDone {
		t.Fatalf("outcome = %+v, want done — a hold keeps the upgrade", o)
	}
}

// TestUpgradeKeepsWhenReadinessCannotBeSampled: S1 must never revert a working upgrade because
// its own telemetry failed. This is the safe direction and the one that keeps a broken gate from
// becoming a broken update path.
func TestUpgradeKeepsWhenReadinessCannotBeSampled(t *testing.T) {
	f := &fakeInstaller{readinessEr: errors.New("connection refused")}
	if o := upgradeWith(t, f); o.State != api.OutcomeDone {
		t.Fatalf("outcome = %+v, want done — a failed sample is not evidence of a regression", o)
	}
}

// TestUpgradeOfAnUnknownServiceIsFloorOnly: the registry's default, at the level that matters.
// A service the product holds no knowledge about is upgraded exactly as it was before the gate
// existed — no sample taken, no minute spent settling.
func TestUpgradeOfAnUnknownServiceIsFloorOnly(t *testing.T) {
	m := testManifest()
	m.Name = "mosquitto"
	cfg := catalogFor(t, m)
	body, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeInstaller{
		primary: true, active: true, healthy: true,
		prior:     map[string]string{"mosquitto": string(body)},
		readiness: [][]hass.Entry{sample("loaded"), sample("setup_error")},
	}
	d := api.Directive{ID: "d1", Kind: api.DirectiveServiceInstall, Payload: "mosquitto"}
	if o := cfg.applyServiceInstall(context.Background(), f, d, func(string, ...any) {}); o.State != api.OutcomeDone {
		t.Fatalf("outcome = %+v, want done", o)
	}
	if f.readinessHit != 0 {
		t.Fatalf("a service with no assessor sampled readiness %d times, want 0", f.readinessHit)
	}
}

// TestInstallFailsWhenItsOwnServiceWasSkipped is the other half of converge's containment. A
// preparation failure is contained there so a promoting node keeps the services that work — which
// means converge SUCCEEDS while declining to start ours. For an install that is the wrong answer:
// the old container is still serving, so the health gate below would pass on it and the install
// would report a version nothing is running.
func TestInstallFailsWhenItsOwnServiceWasSkipped(t *testing.T) {
	f := &fakeInstaller{convergeSkips: []string{"home-assistant"}}
	o := upgradeWith(t, f)
	if o.State != api.OutcomeRolledBack {
		t.Fatalf("outcome = %+v, want rolled-back — converge did not start our service", o)
	}
	if !strings.Contains(o.Detail, "could not prepare") {
		t.Fatalf("the outcome does not say why: %q", o.Detail)
	}
}

// TestInstallIgnoresAnotherServiceBeingSkipped: containment cuts both ways. A DIFFERENT service
// failing to prepare is not this install's problem, and failing here would make one broken
// catalog entry block every other install on the node.
func TestInstallIgnoresAnotherServiceBeingSkipped(t *testing.T) {
	f := &fakeInstaller{
		convergeSkips: []string{"mosquitto"},
		readiness:     [][]hass.Entry{sample("loaded"), sample("loaded")},
	}
	if o := upgradeWith(t, f); o.State != api.OutcomeDone {
		t.Fatalf("outcome = %+v, want done — another service's skip is not ours", o)
	}
}

// PER-SERVICE HEALTH, the [B.48] half, and the hole it closes is the one State could not: a
// container that is UP while the application inside it does not serve. State says `running`, the
// node-level Healthy is the front door answering for the NODE, and before this nothing in the
// system could see the difference.
//
// It is only asked of a RUNNING service, and an unresolvable one leaves the field empty rather
// than calling it unhealthy -- a node mid-converge has no route yet, and a false alarm on a field
// whose whole value is that someone acts on it is worse than silence.
func TestServiceStatusesReportPerServiceHealth(t *testing.T) {
	cfg := Config{Services: []model.ServiceSpec{
		{Name: "serving", Manifest: "sha256:aa", Unit: "briard-serving-app.service"},
		{Name: "wedged", Manifest: "sha256:bb", Unit: "briard-wedged-app.service"},
		{Name: "unrouted", Manifest: "sha256:cc", Unit: "briard-unrouted-app.service"},
		{Name: "stopped", Manifest: "sha256:dd", Unit: "briard-stopped-app.service"},
	}}
	r := fakeStatus{
		active: map[string]bool{
			"briard-serving-app.service":  true,
			"briard-wedged-app.service":   true,
			"briard-unrouted-app.service": true,
		},
		// "unrouted" is deliberately absent: the guest cannot resolve it.
		svcHealth: map[string]bool{"serving": true, "wedged": false, "stopped": true},
	}
	got := cfg.serviceStatuses(context.Background(), r, true)
	want := []api.ServiceStatus{
		{Name: "serving", Manifest: "sha256:aa", State: api.StateRunning, Health: api.StateHealthy},
		// THE ONE THAT MATTERS: up, and not serving.
		{Name: "wedged", Manifest: "sha256:bb", State: api.StateRunning, Health: api.StateUnhealthy},
		{Name: "unrouted", Manifest: "sha256:cc", State: api.StateRunning},
		// Not probed at all: State already says stopped, and "unhealthy" would say it twice.
		{Name: "stopped", Manifest: "sha256:dd", State: api.StateStopped},
	}
	if !slices.Equal(got, want) {
		t.Errorf("serviceStatuses() = %+v, want %+v", got, want)
	}
}

// The free-space gate and the pull record ([V3b.31j]), as the fake sees them.
func (f *fakeInstaller) StorageFree(context.Context) (int64, int64, error) {
	f.steps = append(f.steps, "storage.free")
	return f.free, f.free + 4<<30, f.freeErr
}
func (f *fakeInstaller) SupportsStorageFree() bool { return !f.oldGuest }
func (f *fakeInstaller) ServicePulling(_ context.Context, service string, size, installed int64) error {
	f.steps = append(f.steps, fmt.Sprintf("pulling %s %d/%d", service, size, installed))
	return nil
}
func (f *fakeInstaller) ServicePulled(_ context.Context, service string) error {
	f.steps = append(f.steps, "pulled "+service)
	return nil
}
func (f *fakeInstaller) SupportsServicePulling() bool { return !f.oldGuest }

// THE FREE-SPACE GATE ([V3b.31j]): a manifest that says what it will hold, a guest that says
// what it has, and a pull that could only end in a full disk refused BEFORE anything is
// warmed, rendered or touched -- with both numbers in the sentence. Enough space installs and
// leaves the pull record for the dashboard's bar, cleared on the way out; a guest that cannot
// measure installs unmeasured; an entry without sizes gets no gate and no bar.
func TestInstallRefusesWhatWouldNotFit(t *testing.T) {
	m := testManifest()
	m.Size, m.InstalledSize = 600e6, 2500e6
	f := &fakeInstaller{primary: true, active: true, healthy: true, free: 2000e6}
	o := installService(catalogFor(t, m), f)
	if o.State != api.OutcomeFailed || !strings.Contains(o.Detail, "not enough space") || !strings.Contains(o.Detail, "2.50 GB") || !strings.Contains(o.Detail, "2.00 GB") {
		t.Fatalf("outcome = %+v, want a refusal naming what is needed and what is free", o)
	}
	joined := strings.Join(f.steps, ",")
	for _, forbidden := range []string{"warm", "render", "provision", "converge", "pulling"} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("a refused install still ran %q: %v", forbidden, f.steps)
		}
	}
	// 2.5 GB installed + 1 GiB headroom fits in 4 GB: installs, and the bar's record brackets
	// the warm.
	f = &fakeInstaller{primary: true, active: true, healthy: true, free: 4000e6}
	if o := installService(catalogFor(t, m), f); o.State != api.OutcomeDone {
		t.Fatalf("outcome with room = %+v", o)
	}
	joined = strings.Join(f.steps, ",")
	pulling, warm, pulled := strings.Index(joined, "pulling "+m.Name+" 600000000/2500000000"), strings.Index(joined, "warm"), strings.Index(joined, "pulled "+m.Name)
	if pulling < 0 || warm < 0 || pulled < 0 || !(pulling < warm && warm < pulled) {
		t.Fatalf("the pull record does not bracket the warm: %v", f.steps)
	}
	// Cannot measure: installs, unmeasured.
	f = &fakeInstaller{primary: true, active: true, healthy: true, freeErr: errors.New("df: no such file")}
	if o := installService(catalogFor(t, m), f); o.State != api.OutcomeDone {
		t.Fatalf("outcome when the store cannot be measured = %+v; want the install to proceed", o)
	}
	// No sizes in the entry: no gate, no record.
	plain := testManifest()
	f = &fakeInstaller{primary: true, active: true, healthy: true, free: 1}
	if o := installService(catalogFor(t, plain), f); o.State != api.OutcomeDone {
		t.Fatalf("outcome for an unsized entry = %+v; want no gate", o)
	}
	if s := strings.Join(f.steps, ","); strings.Contains(s, "storage.free") || strings.Contains(s, "pulling") {
		t.Fatalf("an unsized entry was measured or recorded: %v", f.steps)
	}
}

// THE INSTALL'S OWN BUDGET FOLLOWS THE PULL'S ([V3b.31k]): an unsized entry keeps the fixed 15
// minutes; a sized one gets at least its pull bound plus the health gate, so the operation
// cannot expire before the pull it is waiting on is allowed to finish.
func TestInstallBudgetFollowsThePullBound(t *testing.T) {
	if got := installBudgetFor(testManifest()); got != installBudget {
		t.Errorf("unsized = %s, want %s", got, installBudget)
	}
	small := testManifest()
	small.Size, small.InstalledSize = 9981691, 23163392
	if got := installBudgetFor(small); got != installBudget {
		t.Errorf("a 10 MB entry = %s, want the fixed %s (its bound + gate is shorter)", got, installBudget)
	}
	big := testManifest()
	big.Size, big.InstalledSize = 621628919, 2486168064
	want := quadlet.PullTimeout(big.Size) + healthGate
	if got := installBudgetFor(big); got != want || got <= installBudget {
		t.Errorf("a 622 MB entry = %s, want %s (> %s)", got, want, installBudget)
	}
}

// TestInstallRefusesAGuestThatCannotTakeAMember: the ring is gated on the CAPABILITY, and the
// refusal has to be its own -- a guest can be current in every other respect and still predate
// data.member, because the host agent self-updates independently of the guest OS ([V3.4]).
//
// Falling back to data.snapshot would be the tempting workaround and is the one thing that must
// not happen: it takes an UNLABELLED member, which is precisely what the sidecar exists to
// prevent, and the picker could then offer a household a rollback point it cannot identify.
//
// ⚠️ It is a capability and not a protocol floor deliberately. [B.143] tried the floor and gate 3
// refused the release: every host refuses every not-yet-rolled guest, the health gate reverts the
// self-update, and the node cannot reach the image that would have fixed it.
func TestInstallRefusesAGuestThatCannotTakeAMember(t *testing.T) {
	cfg := catalogFor(t, testManifest())
	f := &fakeInstaller{primary: true, active: true, healthy: true, noMember: true}
	o := installService(cfg, f)
	if o.State != api.OutcomeFailed {
		t.Fatalf("outcome = %+v, want failed", o)
	}
	if !strings.Contains(o.Detail, "data.member") {
		t.Errorf("the refusal does not name the missing verb: %q", o.Detail)
	}
	for _, s := range f.steps {
		if strings.HasPrefix(s, "snapshot:") || strings.HasPrefix(s, "provision") {
			t.Errorf("the install acted before refusing: %v", f.steps)
		}
	}
}

// The restore's three ([B.143]). members is the ring the fake offers; ensureErr is an image that
// cannot be fetched, which is the case the restore must refuse BEFORE it stops anything.
func (f *fakeInstaller) Members(_ context.Context, service string) ([]quadlet.SnapshotEntry, error) {
	f.steps = append(f.steps, "members:"+service)
	return f.members, f.membersEr
}
func (f *fakeInstaller) SupportsMembers() bool { return !f.oldGuest }
func (f *fakeInstaller) EnsureImage(_ context.Context, ref string) error {
	f.steps = append(f.steps, "ensure:"+ref)
	return f.ensureEr
}
func (f *fakeInstaller) SupportsImageEnsure() bool { return !f.oldGuest }

// ringWith is a node running testManifest() whose ring holds one member pinned to `pinned`.
func ringWith(t *testing.T, pinned manifest.Manifest, ensureEr error) (Config, *fakeInstaller, string) {
	t.Helper()
	raw, err := json.Marshal(pinned)
	if err != nil {
		t.Fatal(err)
	}
	member := quadlet.SnapshotMember("home-assistant", quadlet.TriggerUpgrade, fixedNow.Add(-48*time.Hour))
	cfg := catalogFor(t, testManifest())
	cfg.readinessSettle = time.Millisecond
	f := &fakeInstaller{
		primary: true, active: true, healthy: true,
		prior:    mustPrior(t),
		ensureEr: ensureEr,
		members: []quadlet.SnapshotEntry{{Member: member, Meta: quadlet.SnapshotMeta{
			Service: "home-assistant", Trigger: quadlet.TriggerUpgrade,
			Title: "2026.6.0, before upgrading to 2026.7.1", Manifest: string(raw),
		}}},
	}
	return cfg, f, member
}

func restore(cfg Config, f *fakeInstaller, member string) api.DirectiveOutcome {
	d := api.Directive{ID: "d1", Kind: api.DirectiveServiceRestore, Payload: member}
	return cfg.applyServiceRestore(context.Background(), f, d, func(string, ...any) {})
}

// TestRestoreRefusesBeforeItStopsAnything is the property the whole order exists for ([B.143]).
//
// A revert runs from a BROKEN state and may commit -- the household already lost the thing it is
// undoing. A restore runs from a HEALTHY one, chosen by somebody looking at a list, so a member
// whose images can no longer be fetched (a digest three months old that the registry has dropped)
// must leave the service running and say so.
func TestRestoreRefusesBeforeItStopsAnything(t *testing.T) {
	cfg, f, member := ringWith(t, testManifest(), errors.New("manifest unknown"))
	o := restore(cfg, f, member)
	if o.State != api.OutcomeFailed {
		t.Fatalf("outcome = %+v, want failed", o)
	}
	if !strings.Contains(o.Detail, "nothing was changed") {
		t.Errorf("the refusal does not say the node is untouched: %q", o.Detail)
	}
	joined := strings.Join(f.steps, ",")
	for _, forbidden := range []string{"stop:", "restore:", "provision", "snapshot:"} {
		if strings.Contains(joined, forbidden) {
			t.Errorf("a restore that could not get its images did %q: %v", forbidden, f.steps)
		}
	}
}

// TestRestoreTakesTheUndoBeforeTheStop: the undo is the only way back from a mis-click, so it has
// to exist before anything is torn down -- not after, where a failure in between would leave the
// household with neither the old state nor the new one.
func TestRestoreTakesTheUndoBeforeTheStop(t *testing.T) {
	cfg, f, member := ringWith(t, testManifest(), nil)
	if o := restore(cfg, f, member); o.State != api.OutcomeDone {
		t.Fatalf("outcome = %+v, want done", o)
	}
	joined := strings.Join(f.steps, ",")
	ei := strings.Index(joined, "ensure:")
	ui := strings.Index(joined, "snapshot:"+quadlet.SnapshotMember("home-assistant", quadlet.TriggerRestoreBefore, fixedNow))
	si := strings.Index(joined, "stop:")
	ri := strings.Index(joined, "restore:")
	if ei < 0 || ui < 0 || si < 0 || ri < 0 {
		t.Fatalf("the restore did not run its whole sequence: %v", f.steps)
	}
	if !(ei < ui && ui < si && si < ri) {
		t.Errorf("order was %v, want ensure -> undo -> stop -> restore", f.steps)
	}
	// And the waypoint, which is what keeps the timeline from appearing to jump backwards.
	if !strings.Contains(joined, "snapshot:"+quadlet.SnapshotMember("home-assistant", quadlet.TriggerRestoreAfter, fixedNow)) {
		t.Errorf("no waypoint was taken after the restore: %v", f.steps)
	}
}

// TestRestorePutsBackTheMembersOwnManifest: {manifest + data} move together, which is what makes
// this the DESIGN §8 rollback reached deliberately rather than through a failed upgrade.
func TestRestorePutsBackTheMembersOwnManifest(t *testing.T) {
	// The node runs 2026.6.0 (priorManifest's); the member is pinned OLDER, so the two differ and
	// the restore has to move the code as well as the data.
	old := testManifest()
	old.Version = "2026.5.0"
	cfg, f, member := ringWith(t, old, nil)
	o := restore(cfg, f, member)
	if o.State != api.OutcomeDone {
		t.Fatalf("outcome = %+v, want done", o)
	}
	if n := len(f.manifests); n == 0 || !strings.Contains(f.manifests[n-1], "2026.5.0") {
		t.Fatalf("the volume does not end up holding the member's manifest: %v", f.manifests)
	}
	if !strings.Contains(o.Detail, "code too") {
		t.Errorf("detail = %q, want it to say the code moved with the data", o.Detail)
	}
}

// TestRestoreRefusesAMemberTheRingNoLongerHas: the picker's list can be stale -- pruning runs on
// every take -- so the member is read back from the RING rather than trusted from the payload.
func TestRestoreRefusesAMemberTheRingNoLongerHas(t *testing.T) {
	cfg, f, _ := ringWith(t, testManifest(), nil)
	gone := quadlet.SnapshotMember("home-assistant", quadlet.TriggerStart, fixedNow.Add(-99*time.Hour))
	o := restore(cfg, f, gone)
	if o.State != api.OutcomeFailed {
		t.Fatalf("outcome = %+v, want failed", o)
	}
	if !strings.Contains(o.Detail, "pruned") {
		t.Errorf("detail = %q, want it to say the member is gone", o.Detail)
	}
}
