package guest

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"briard.io/agent/guestagent"
	"briard.io/shared/model"
)

// fakeControl records the guest-channel calls Manager makes and returns canned data.
type fakeControl struct {
	started, stopped        []string
	active                  bool
	activeErr               error
	system                  string   // current system path (SystemPath)
	switches                []string // closures passed to Switch, in order
	switchErr               error    // Switch returns this (converge can't reach the staged closure)
	stagedBoot              []string // closures passed to StageBoot, in order
	stageBootErr            error
	snapDataDir, snapTo     string
	snapErr                 error
	restoreFrom, restoreDir string
	restored                bool
	startErr                error                                  // PayloadStart returns this (to trigger a rollback)
	components              map[string]guestagent.SystemComponents // keyed by closure ("" = booted), V3.17c1
	componentsFor           []string                               // closures Components was asked about, in order
	componentsErr           error
	collected               int // CollectGarbage calls
	collectErr              error
	staged                  []string // closures passed to Stage, in order
	stagedFrom              []guestagent.StageSource
	stageErr                error
	certs                   []string // cert|key pairs passed to WriteCert, in order
	ops                     []string // ordered log of maintenance/lifecycle ops (for sequencing)
	paused, resumed         int
	cluster                 model.Cluster // what OSReady reads about this node
	clusterErr              error
	activeHook              func(ctx context.Context) // inspect the ctx PayloadActive receives
	health                  bool                      // PayloadHealth in-guest result (used when healthVerb is set)
	healthVerb              bool                      // when false, PayloadHealth errors so probeReady falls back to the host-side GET (the default keeps HTTP-server-based tests exercising the fallback)
	vip                     string                    // net.vip: the address the named device actually holds (CIDR); "" = it holds none
	vipErr                  error
	probed                  []string // URLs PayloadHealth was asked to probe, in order
}

func (f *fakeControl) PayloadStart(_ context.Context, unit string) error {
	f.started = append(f.started, unit)
	f.ops = append(f.ops, "start")
	return f.startErr
}
func (f *fakeControl) PayloadStop(_ context.Context, unit string) error {
	f.stopped = append(f.stopped, unit)
	f.ops = append(f.ops, "stop")
	return nil
}
func (f *fakeControl) PayloadActive(ctx context.Context, _ string) (bool, error) {
	if f.activeHook != nil {
		f.activeHook(ctx)
	}
	return f.active, f.activeErr
}
func (f *fakeControl) PayloadHealth(_ context.Context, url string) (bool, error) {
	f.probed = append(f.probed, url)
	if !f.healthVerb {
		return false, errors.New(`guestagent: unknown verb "payload.health"`) // old guest -> probeReady falls back
	}
	return f.health, nil
}
func (f *fakeControl) VIP(_ context.Context, _ string) (string, error) { return f.vip, f.vipErr }
func (f *fakeControl) SystemPath(context.Context) (string, error)      { return f.system, nil }
func (f *fakeControl) Components(_ context.Context, closure string) (guestagent.SystemComponents, error) {
	f.componentsFor = append(f.componentsFor, closure)
	return f.components[closure], f.componentsErr
}
func (f *fakeControl) Stage(_ context.Context, closure string, src guestagent.StageSource) error {
	f.staged = append(f.staged, closure)
	f.stagedFrom = append(f.stagedFrom, src)
	f.ops = append(f.ops, "stage")
	return f.stageErr
}
func (f *fakeControl) Switch(_ context.Context, closure string) error {
	f.switches = append(f.switches, closure)
	f.ops = append(f.ops, "switch")
	return f.switchErr
}
func (f *fakeControl) StageBoot(_ context.Context, closure string) error {
	f.stagedBoot = append(f.stagedBoot, closure)
	f.ops = append(f.ops, "stageboot")
	return f.stageBootErr
}

func (f *fakeControl) WriteCert(_ context.Context, cert, key string) error {
	f.certs = append(f.certs, cert+"|"+key)
	f.ops = append(f.ops, "cert")
	return nil
}
func (f *fakeControl) Snapshot(_ context.Context, dataDir, dest string) error {
	f.snapDataDir, f.snapTo = dataDir, dest
	f.ops = append(f.ops, "snapshot")
	return f.snapErr
}
func (f *fakeControl) Restore(_ context.Context, dataDir, src string) error {
	f.restoreDir, f.restoreFrom, f.restored = dataDir, src, true
	f.ops = append(f.ops, "restore")
	return nil
}
func (f *fakeControl) ReactorPause(context.Context, string) error {
	f.paused++
	f.ops = append(f.ops, "pause")
	return nil
}
func (f *fakeControl) ReactorResume(context.Context, string) error {
	f.resumed++
	f.ops = append(f.ops, "resume")
	return nil
}

// "storegc", not "gc": the nix store and a btrfs snapshot are different things, and this log
// is read by eye.
func (f *fakeControl) CollectGarbage(context.Context) error {
	f.collected++
	f.ops = append(f.ops, "storegc")
	return f.collectErr
}

// Cluster is what OSReady asks about this node. Every pre-existing test leaves
// Config.Resource empty, so OSReady short-circuits and never reaches this.
func (f *fakeControl) Cluster(context.Context, string) (model.Cluster, error) {
	return f.cluster, f.clusterErr
}

var haSpec = model.ServiceSpec{Name: "home-assistant", Image: "ghcr.io/home-assistant:pinned", DataDir: "/data/ha"}

func newManager(ctl control, healthURL string) *Manager {
	return NewManager(ctl, Config{
		HealthURL:    healthURL,
		gateInterval: time.Millisecond, // poll fast in tests; total wait bounded by ctx
		idFn:         func() string { return "ID" },
	})
}

func codeServer(t *testing.T, status int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// newPromoterManager configures promoter-coordinated quiesce.
func newPromoterManager(ctl control, healthURL string) *Manager {
	return NewManager(ctl, Config{
		HealthURL:      healthURL,
		gateInterval:   time.Millisecond,
		ReactorSnippet: "briard",
		idFn:           func() string { return "ID" },
	})
}

func TestStartStopDrivePayloadUnit(t *testing.T) {
	f := &fakeControl{}
	m := newManager(f, "")
	if err := m.Start(context.Background(), haSpec); err != nil {
		t.Fatal(err)
	}
	if err := m.Stop(context.Background(), haSpec); err != nil {
		t.Fatal(err)
	}
	if len(f.started) != 1 || f.started[0] != "podman-home-assistant.service" {
		t.Errorf("started = %v", f.started)
	}
	if len(f.stopped) != 1 || f.stopped[0] != "podman-home-assistant.service" {
		t.Errorf("stopped = %v", f.stopped)
	}
}

func TestHealthReadyOnActiveUnitAnd200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	m := newManager(&fakeControl{active: true}, srv.URL)
	h, err := m.Health(context.Background(), haSpec)
	if err != nil {
		t.Fatal(err)
	}
	if !h.Running || !h.Ready {
		t.Errorf("Health = %+v, want Running && Ready", h)
	}
}

// A running unit whose /healthz fails is a rollback trigger: Running but not Ready.
func TestHealthNotReadyWhenProbeFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	m := newManager(&fakeControl{active: true}, srv.URL)
	h, err := m.Health(context.Background(), haSpec)
	if err != nil {
		t.Fatal(err)
	}
	if !h.Running || h.Ready {
		t.Errorf("Health = %+v, want Running && !Ready", h)
	}
}

// An inactive unit is never probed and never ready.
func TestHealthInactiveUnitSkipsProbe(t *testing.T) {
	m := newManager(&fakeControl{active: false}, "http://127.0.0.1:1/should-not-be-hit")
	h, err := m.Health(context.Background(), haSpec)
	if err != nil {
		t.Fatal(err)
	}
	if h.Running || h.Ready {
		t.Errorf("Health = %+v, want all false", h)
	}
}

func TestHealthPropagatesControlError(t *testing.T) {
	m := newManager(&fakeControl{activeErr: errors.New("channel down")}, "")
	if _, err := m.Health(context.Background(), haSpec); err == nil {
		t.Fatal("expected error")
	}
}

// When the guest supports payload.health, readiness follows the IN-GUEST probe, NOT a host-side
// GET of the VIP — so the health-gate works under a substrate where the host can't reach the VIP
// (macvtap). A failable pair: the host URL always says the opposite of the verb.
func TestHealthPrefersInGuestProbe(t *testing.T) {
	// Host-side GET of this URL would 200, but the in-guest verb says sick: Ready must be false.
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer up.Close()
	m := newManager(&fakeControl{active: true, healthVerb: true, health: false}, up.URL)
	if h, err := m.Health(context.Background(), haSpec); err != nil || !h.Running || h.Ready {
		t.Errorf("Health = %+v, err=%v; want Running && !Ready (in-guest false wins over host 200)", h, err)
	}
	// Converse: verb true -> Ready even though the host URL is unreachable (never touched).
	m = newManager(&fakeControl{active: true, healthVerb: true, health: true}, "http://127.0.0.1:1/unreachable")
	if h, err := m.Health(context.Background(), haSpec); err != nil || !h.Ready {
		t.Errorf("Health = %+v, err=%v; want Ready (in-guest true, host URL never touched)", h, err)
	}
}

// AwaitReady must poll on a context detached from the upgrade deadline. Otherwise a
// deadline that fires *inside* a control-channel call closes the channel mid-call, and the
// rollback that reuses that channel then fails to restore. Assert the ctx PayloadActive
// receives does not observe the upgrade ctx's cancellation.
func TestAwaitReadyPollsOnDetachedContext(t *testing.T) {
	srv := codeServer(t, http.StatusServiceUnavailable) // running but never ready → keep polling
	f := &fakeControl{active: true}
	m := newManager(f, srv.URL)

	ctx, cancel := context.WithCancel(context.Background())
	var pollCtxCancelled, fired bool
	f.activeHook = func(hctx context.Context) {
		if fired {
			return
		}
		fired = true
		cancel()                             // simulate the upgrade deadline firing during the call
		pollCtxCancelled = hctx.Err() != nil // fixed: poll ctx is detached, so still live here
	}

	if err := m.AwaitReady(ctx, haSpec); err == nil {
		t.Fatal("awaitReady should return the health-gate deadline error")
	}
	if pollCtxCancelled {
		t.Fatal("health poll ran on the upgrade ctx: a mid-call deadline would close the channel and break rollback")
	}
}

// Snapshot cuts at the service's DataDir, names a per-service subvolume, and pins the
// current generation into a self-contained ref.
func TestSnapshotProducesPinnedRef(t *testing.T) {
	f := &fakeControl{system: "/nix/store/old-nixos-system"}
	m := newManager(f, "")
	ref, err := m.Snapshot(context.Background(), haSpec)
	if err != nil {
		t.Fatal(err)
	}
	want := SnapshotRef{
		Service:   "home-assistant",
		DataDir:   "/data/ha",
		Subvolume: "/data/.snapshots/home-assistant-ID",
		System:    "/nix/store/old-nixos-system",
	}
	if ref != want {
		t.Errorf("ref = %+v, want %+v", ref, want)
	}
	if f.snapDataDir != "/data/ha" || f.snapTo != want.Subvolume {
		t.Errorf("snapshot(%q, %q), want (/data/ha, %q)", f.snapDataDir, f.snapTo, want.Subvolume)
	}
}

func TestSnapshotFailsBeforeReturningRef(t *testing.T) {
	m := newManager(&fakeControl{snapErr: errors.New("btrfs boom")}, "")
	if _, err := m.Snapshot(context.Background(), haSpec); err == nil {
		t.Fatal("expected error")
	}
}

// Restore is driven entirely by the self-contained ref (DataDir + Subvolume).
func TestRestoreUsesRefFields(t *testing.T) {
	f := &fakeControl{}
	m := newManager(f, "")
	ref := SnapshotRef{DataDir: "/data/ha", Subvolume: "/data/ha/.snapshots/home-assistant-ID"}
	if err := m.Restore(context.Background(), ref); err != nil {
		t.Fatal(err)
	}
	if f.restoreDir != ref.DataDir || f.restoreFrom != ref.Subvolume {
		t.Errorf("restore(%q, %q), want (%q, %q)", f.restoreDir, f.restoreFrom, ref.DataDir, ref.Subvolume)
	}
}

// THE OS-UPGRADE SEQUENCE TESTS MOVED OUT, with the sequence itself: it is
// agent/host's now, because an OS upgrade rolls back to a snapshot of the OS disk and only the
// host can take or restore one. Their replacements are TestSwitchUpgrade* in
// agent/host/osupgrade_test.go, and they assert the shape gave the path -- no payload
// stop/start, no data snapshot/restore, one activation on a failing run.
//
// What stays here is what the guest still owns and this file can still fail on: the health
// floor (including the zero-service case), the S1 gate's verdicts, the primitives, and the
// PAYLOAD upgrade -- which is unchanged, because {image + data} rolling back together is the
// whole point of that one.

// fakeAssessor drives the differential S1 gate: a canned verdict + a record of when it
// was consulted, to assert the baseline is taken before quiesce and Assess after the
// floor.
type fakeAssessor struct {
	verdict     Verdict
	baselineErr error
	assessErr   error
	baseCalls   int
	assessCalls int
	baseline    Baseline
}

func (a *fakeAssessor) Baseline(context.Context) (Baseline, error) {
	a.baseCalls++
	return "pre-sample", a.baselineErr
}

func (a *fakeAssessor) Assess(_ context.Context, base Baseline) (Verdict, string, error) {
	a.assessCalls++
	a.baseline = base
	return a.verdict, string(a.verdict) + "-reason", a.assessErr
}

func managerWithAssessor(ctl control, healthURL string, a ReadinessAssessor) *Manager {
	return NewManager(ctl, Config{
		HealthURL:         healthURL,
		gateInterval:      time.Millisecond,
		ReadinessAssessor: a,
		idFn:              func() string { return "ID" },
	})
}

// The S1 gate's verdicts, driven on the pair the upgrade paths actually call. Every upgrade
// path consults it identically -- capture a baseline while the old code still serves, then
// assess once the floor has passed -- so the semantics are asserted once, here, and each
// sequence is left to prove only that it consults them (agent/host/osupgrade_test.go).
//
// A non-nil error means "roll back"; everything else keeps the upgrade. The degrade cases are
// the load-bearing ones: S1 must never revert a node because its own telemetry broke.
func TestAssessVerdicts(t *testing.T) {
	for _, tc := range []struct {
		name        string
		a           *fakeAssessor
		wantErr     bool
		wantAssesse int
	}{
		{"rollback trips the gate", &fakeAssessor{verdict: VerdictRollback}, true, 1},
		{"hold keeps (surfaced, not reverted)", &fakeAssessor{verdict: VerdictHold}, false, 1},
		{"pass keeps silently", &fakeAssessor{verdict: VerdictPass}, false, 1},
		{"a baseline that never captured is never assessed",
			&fakeAssessor{verdict: VerdictRollback, baselineErr: errors.New("HA API down")}, false, 0},
		{"an assess failure degrades to keep",
			&fakeAssessor{verdict: VerdictRollback, assessErr: errors.New("settle timed out")}, false, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := managerWithAssessor(&fakeControl{}, "", tc.a)
			err := m.Assess(context.Background(), m.CaptureBaseline(context.Background()))
			if (err != nil) != tc.wantErr {
				t.Errorf("Assess error = %v, want error: %v", err, tc.wantErr)
			}
			if tc.a.baseCalls != 1 {
				t.Errorf("baseline captured %d times, want 1", tc.a.baseCalls)
			}
			if tc.a.assessCalls != tc.wantAssesse {
				t.Errorf("assess calls = %d, want %d", tc.a.assessCalls, tc.wantAssesse)
			}
			if tc.wantAssesse > 0 && tc.a.baseline != "pre-sample" {
				t.Errorf("Assess got baseline %v, want the token Baseline returned", tc.a.baseline)
			}
		})
	}
}

// Every boot-critical component, one at a time, must force a reboot — the table
// is exhaustive over the struct on purpose. A
// field added to SystemComponents without a case here is the drift this guards against:
// it would silently be treated as switch-safe, which is the one wrong answer that cannot
// be recovered from, since by the time it matters the running system is already replaced.
func TestActivationForEachComponentForcesReboot(t *testing.T) {
	base := guestagent.SystemComponents{
		Kernel:        "/nix/store/aaa-linux/bzImage",
		Initrd:        "/nix/store/bbb-initrd/initrd",
		KernelModules: "/nix/store/ccc-modules",
		Systemd:       "/nix/store/ddd-systemd",
		KernelParams:  "console=ttyS0 loglevel=4",
	}
	for _, tc := range []struct {
		name  string
		mutit func(*guestagent.SystemComponents)
	}{
		{"kernel", func(c *guestagent.SystemComponents) { c.Kernel = "/nix/store/zzz-linux/bzImage" }},
		{"initrd", func(c *guestagent.SystemComponents) { c.Initrd = "/nix/store/zzz-initrd/initrd" }},
		{"kernel-modules", func(c *guestagent.SystemComponents) { c.KernelModules = "/nix/store/zzz-modules" }},
		{"systemd", func(c *guestagent.SystemComponents) { c.Systemd = "/nix/store/zzz-systemd" }},
		{"kernel-params", func(c *guestagent.SystemComponents) { c.KernelParams = "console=ttyS0 loglevel=7" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			target := base
			tc.mutit(&target)
			method, reasons := ActivationFor(base, target)
			if method != ActivateReboot {
				t.Errorf("%s changed but method = %q, want reboot", tc.name, method)
			}
			if !reflect.DeepEqual(reasons, []string{tc.name}) {
				t.Errorf("reasons = %v, want [%s] (the decision must say what forced it)", reasons, tc.name)
			}
		})
	}
}

// An identical target is a switch. This is the assertion that keeps the reboot rule from
// being vacuously "always reboot", which would pass every test above while making every
// update an outage on a single-node green.
func TestActivationForIdenticalIsSwitch(t *testing.T) {
	c := guestagent.SystemComponents{Kernel: "/nix/store/aaa", Initrd: "/nix/store/bbb", KernelModules: "/nix/store/ccc", Systemd: "/nix/store/ddd", KernelParams: "quiet"}
	method, reasons := ActivationFor(c, c)
	if method != ActivateSwitch || reasons != nil {
		t.Errorf("identical components = %q %v, want switch with no reasons", method, reasons)
	}
}

// A real incremental release — new units, new etc, same kernel — is a switch. This is the
// common case an OS update delivers (measured 74 MB of a 1.6 GB closure), so if it ever reads
// as a reboot then every routine update costs a boot.
func TestActivationForUserlandOnlyChangeIsSwitch(t *testing.T) {
	booted := guestagent.SystemComponents{Kernel: "/nix/store/aaa", Initrd: "/nix/store/bbb", KernelModules: "/nix/store/ccc", Systemd: "/nix/store/ddd", KernelParams: "quiet"}
	target := booted // the toplevel/etc/units differ, but none of THESE do
	method, _ := ActivationFor(booted, target)
	if method != ActivateSwitch {
		t.Errorf("userland-only change = %q, want switch", method)
	}
}

// Several at once (a nixpkgs bump) reports all of them, not just the first — the log has
// to be honest about the size of the change it is about to make.
func TestActivationForReportsEveryReason(t *testing.T) {
	booted := guestagent.SystemComponents{Kernel: "a", Initrd: "b", KernelModules: "c", Systemd: "d", KernelParams: "p"}
	target := guestagent.SystemComponents{Kernel: "A", Initrd: "B", KernelModules: "C", Systemd: "d", KernelParams: "p"}
	method, reasons := ActivationFor(booted, target)
	if method != ActivateReboot || !reflect.DeepEqual(reasons, []string{"kernel", "initrd", "kernel-modules"}) {
		t.Errorf("= %q %v, want reboot [kernel initrd kernel-modules]", method, reasons)
	}
}

// ActivationMethod diffs the target against the BOOTED generation, not the current one —
// asserted by which closures it asks about, since after a switch-only update the two differ
// and only the booted one describes the kernel actually running.
func TestActivationMethodDiffsAgainstBooted(t *testing.T) {
	f := &fakeControl{components: map[string]guestagent.SystemComponents{
		"":             {Kernel: "a", Initrd: "b", KernelModules: "c", Systemd: "d", KernelParams: "p"},
		"/nix/store/t": {Kernel: "a", Initrd: "b", KernelModules: "c", Systemd: "d", KernelParams: "p"},
	}}
	m := newManager(f, "")
	method, _, err := m.ActivationMethod(context.Background(), "/nix/store/t")
	if err != nil {
		t.Fatal(err)
	}
	if method != ActivateSwitch {
		t.Errorf("method = %q, want switch", method)
	}
	if !reflect.DeepEqual(f.componentsFor, []string{"", "/nix/store/t"}) {
		t.Errorf("asked about %v, want [\"\" target] — \"\" is the booted generation", f.componentsFor)
	}
}

// A ZERO-SERVICE node -- the shipped state -- must health-gate on the front
// door alone. Before the fix, unitOf on an empty spec named the nonexistent
// `podman-.service`, systemd said "inactive", and Ready was false forever: every OS upgrade a
// fresh install was offered rolled back, while the same node reported Healthy: true to the
// cloud (cfg.snapshot takes the probe's answer with no such conjunct).
func TestHealthZeroServiceGatesOnTheFrontDoorAlone(t *testing.T) {
	srv := codeServer(t, http.StatusOK)
	f := &fakeControl{active: false} // no payload unit exists, so it cannot be active
	m := newManager(f, srv.URL)

	h, err := m.Health(context.Background(), model.ServiceSpec{})
	if err != nil {
		t.Fatal(err)
	}
	if !h.Ready {
		t.Error("a zero-service node whose front door answers 200 must be Ready")
	}
	if !h.Running {
		t.Error("Running should be vacuously true with no service -- there is no payload to be down")
	}
}

// The converse, so the above is not "always ready": a zero-service node whose FRONT DOOR is
// down is NOT ready. This is exactly the failure the rollback demo breaks deliberately, so if this
// went vacuous the broken-generation test would too.
func TestHealthZeroServiceUnreadyWhenTheFrontDoorIsDown(t *testing.T) {
	srv := codeServer(t, http.StatusServiceUnavailable)
	m := newManager(&fakeControl{}, srv.URL)
	h, err := m.Health(context.Background(), model.ServiceSpec{})
	if err != nil {
		t.Fatal(err)
	}
	if h.Ready {
		t.Error("front door 503 on a zero-service node must NOT be Ready")
	}
}

// A service-bearing node keeps the old meaning: the unit must be active AND the probe pass.
// Without this, the fix could have been "drop the conjunct entirely".
func TestHealthWithAServiceStillRequiresTheUnitActive(t *testing.T) {
	srv := codeServer(t, http.StatusOK)
	m := newManager(&fakeControl{active: false}, srv.URL) // probe green, unit down
	h, err := m.Health(context.Background(), haSpec)
	if err != nil {
		t.Fatal(err)
	}
	if h.Ready {
		t.Error("a service node with its unit down must not be Ready however green the probe is")
	}
}

// The end-to-end half -- a zero-service node must COMMIT an OS upgrade rather than roll it
// back -- moved to agent/host with the sequence, and got stronger on the way. The
// OS path no longer takes a spec at all, so "no service" is not a case it can get wrong; what
// the host test asserts instead is the harder version of the same thing, on a node whose units
// all report INACTIVE: it commits, and it never asks about a unit at all ( deletion 5).
// The Health-level halves above stay here, where the conjunct they guard lives.

// ── ResolveHealthURL: what we probe, decided per call ──────────────────────────────
//
// V3.19c takes the service address away from build time and gives it to the LAN, so "what do we
// probe?" stops being config and becomes a question. It is asked on the ROLLBACK PATH (the OS
// health gate), which is why the whole predicate is enumerated here rather than sampled: a wrong
// answer does not fail loudly, it reverts a healthy node.
func TestResolveHealthURL(t *testing.T) {
	const configured = "http://192.168.1.100/healthz"
	for _, tc := range []struct {
		name       string
		diskless   bool
		vipDev     string
		configured string
		vip        string // what net.vip answers
		vipErr     error
		want       string
	}{
		// A witness probes nothing, whatever else is true. This is the ONLY "" that means
		// "health follows quorum", and role is what says so.
		{"witness probes nothing", true, "eth2", configured, "192.168.9.50/24", nil, ""},

		// An address we set is the address we probe: reading it back could only differ, and
		// the way it differs is the dhcpcd lease sharing the device with the VIP.
		{"configured address wins", false, "eth2", configured, "192.168.9.50/24", nil, configured},

		// No configured address: the VIP came from DHCP inside the guest, so ask the guest.
		{"reported lease is the target", false, "eth2", "", "192.168.9.50/24", nil, "http://192.168.9.50/healthz"},
		{"a bare address needs no prefix", false, "eth2", "", "192.168.9.50", nil, "http://192.168.9.50/healthz"},

		// Holding no address is "nothing to probe yet" -- a Secondary, or a promotion in
		// flight. It reads as not-ready, never as healthy.
		{"no address held", false, "eth2", "", "", nil, ""},

		// A device we did not NAME is a device we know nothing about: with VIP_DEV unset this
		// node claims no VIP, so there is no address to ask about. (Before [V3b.16a] it fell back
		// to a baked eth1, which in the agent-less harnesses also carries the DRBD address --
		// "the first address on eth1" would have been the replication link.)
		{"never ask about an unnamed device", false, "", configured, "192.168.9.50/24", nil, configured},

		// An old guest predating net.vip is one that still has a baked address to fall back
		// to -- the same compatibility posture probeReady takes for payload.health.
		{"verb error falls back", false, "eth2", configured, "", errors.New("unknown verb"), configured},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeControl{vip: tc.vip, vipErr: tc.vipErr}
			got := ResolveHealthURL(context.Background(), f, tc.diskless, tc.vipDev, tc.configured)
			if got != tc.want {
				t.Errorf("ResolveHealthURL = %q, want %q", got, tc.want)
			}
		})
	}
}

// A data node that holds no service address is NOT ready -- it is the node nobody in the house
// can reach, which is the whole of V3.19. The assertion is that it does not probe at all: an
// empty URL handed to a probe is a request that fails for an incidental reason, and this must
// fail for the reason it actually has.
func TestHealthOnANodeWithNoAddressDoesNotProbe(t *testing.T) {
	f := &fakeControl{healthVerb: true, health: true, active: true}
	m := NewManager(f, Config{VIPDev: "eth2"}) // no configured URL, guest reports none

	h, err := m.Health(context.Background(), model.ServiceSpec{Name: "dummy", Unit: "podman-briard-payload.service"})
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if h.Ready {
		t.Error("a node holding no service address must not read ready")
	}
	if len(f.probed) != 0 {
		t.Errorf("nothing to probe, yet it probed %v", f.probed)
	}
}

// ── OSReady: the OS-upgrade gate ───────────────────────────────────────────────────
//
// The defect it fixes, measured live: the OS gate probed HealthURL — the VIP — which a SECONDARY
// does not own. Its peer answers, so a Secondary health-gated its own upgrade against another
// machine's front door and committed a broken generation. The table below is the whole predicate,
// enumerated, and the Secondary rows are the ones that were wrong.
func TestOSReadyAsksEachNodeAboutTheJobItHolds(t *testing.T) {
	for _, tc := range []struct {
		name    string
		cluster model.Cluster
		doorOK  bool
		want    bool
	}{
		// Primary ⇒ /healthz. This is the only case that may consult the front door.
		{"primary serving", model.Cluster{QuorumState: model.QuorumState{Primary: true, Quorate: true, Diskful: true, UpToDate: true}}, true, true},
		{"primary with a dead front door", model.Cluster{QuorumState: model.QuorumState{Primary: true, Quorate: true, Diskful: true, UpToDate: true}}, false, false},

		// Diskful ∧ quorate ⇒ UpToDate. A Secondary is judged on being a viable failover
		// target, and NOT on the front door — the row that used to pass by asking the peer.
		{"secondary uptodate, front door dead", model.Cluster{QuorumState: model.QuorumState{Quorate: true, Diskful: true, UpToDate: true}}, false, true},
		{"secondary NOT uptodate", model.Cluster{QuorumState: model.QuorumState{Quorate: true, Diskful: true}}, true, false},

		// The vacuous rows, and they are correct rather than tolerated: a node with no present
		// obligation cannot fail one. Both shapes MUST stay upgradable — in particular a degraded
		// node may need the update to RECOVER, so gating on quorum would deadlock exactly the
		// case that needs fixing.
		{"diskless witness (never UpToDate by design)", model.Cluster{QuorumState: model.QuorumState{Quorate: true}}, false, true},
		{"non-quorate anchor (may need the update to recover)", model.Cluster{QuorumState: model.QuorumState{Diskful: true}}, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tc.doorOK {
					w.WriteHeader(http.StatusOK)
					return
				}
				w.WriteHeader(http.StatusServiceUnavailable)
			}))
			defer srv.Close()
			f := &fakeControl{cluster: tc.cluster}
			m := NewManager(f, Config{Resource: "r0", HealthURL: srv.URL})

			got, err := m.OSReady(context.Background())
			if err != nil {
				t.Fatalf("OSReady: %v", err)
			}
			if got != tc.want {
				t.Errorf("OSReady = %v, want %v", got, tc.want)
			}
		})
	}
}

// A Secondary must not consult the front door AT ALL — not merely reach the right verdict by
// coincidence. That is the actual defect: the probe went to the VIP, which the peer answers, so
// any front-door result read on a Secondary is a statement about a different machine.
func TestOSReadySecondaryNeverProbesTheFrontDoor(t *testing.T) {
	var probes int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		probes++
		w.WriteHeader(http.StatusOK) // the PEER's front door, healthy — the trap
	}))
	defer srv.Close()
	f := &fakeControl{cluster: model.Cluster{QuorumState: model.QuorumState{Quorate: true, Diskful: true}}} // not UpToDate
	m := NewManager(f, Config{Resource: "r0", HealthURL: srv.URL})                                          // a configured ANCHOR

	ok, err := m.OSReady(context.Background())
	if err != nil {
		t.Fatalf("OSReady: %v", err)
	}
	if ok {
		t.Error("a Secondary that is not UpToDate is not a viable failover target; it must fail its gate")
	}
	if probes != 0 {
		t.Errorf("the front door was probed %d times on a Secondary; it belongs to whoever holds the VIP", probes)
	}
}

// An unreadable cluster is not a pass. Not knowing what this node is responsible for is not the
// same as knowing it has no obligations — the same reasoning as the reboot gate's.
func TestOSReadyUnreadableClusterIsNotReady(t *testing.T) {
	f := &fakeControl{clusterErr: errors.New("drbdsetup: boom")}
	m := NewManager(f, Config{Resource: "r0", HealthURL: "http://unused.invalid/healthz"})

	if ok, err := m.OSReady(context.Background()); err == nil || ok {
		t.Errorf("OSReady = (%v, %v), want not-ready with an error", ok, err)
	}
}
