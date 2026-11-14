package host

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"briard.io/agent/drbd"
	"briard.io/agent/guest"
	"briard.io/agent/guestagent"
	"briard.io/agent/platform"
	"briard.io/internal/testsock"
)

// statusExec is a guest that answers one question honestly -- `drbdsetup status --json` -- from
// a captured fixture, and refuses everything else. The stop tests below use it as a guest whose
// agent refuses what it is asked.
type statusExec struct{ status []byte }

func (e *statusExec) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	if name == "drbdsetup" && len(args) > 0 && args[0] == "status" {
		return e.status, nil
	}
	return nil, errors.New("statusExec: refusing " + name + " " + strings.Join(args, " "))
}

func (e *statusExec) WriteFile(string, []byte) error {
	return errors.New("statusExec: refusing WriteFile")
}

func (e *statusExec) ReadFile(string) ([]byte, error) {
	return nil, errors.New("statusExec: refusing ReadFile")
}

func (e *statusExec) Sethostname(string) error {
	return errors.New("statusExec: refusing Sethostname")
}

// dialStatus wires a real client to a real Serve over a pipe, so the fixture travels the whole
// path the field uses -- parse, verb, wire, client -- rather than being handed to the gate.
func dialStatus(t *testing.T, fixture string) *guestagent.Client {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "drbd", "testdata", fixture))
	if err != nil {
		t.Fatal(err)
	}
	// The fixture is the ground truth this test rests on: read it the way the product does and
	// fail loudly if it ever stops saying what the case names.
	if _, err := drbd.ParseCluster(raw, "r0"); err != nil {
		t.Fatal(err)
	}
	cconn, sconn := net.Pipe()
	go guestagent.Serve(context.Background(), sconn, &statusExec{status: raw})
	c := guestagent.NewClient(cconn)
	t.Cleanup(func() { c.Close() })
	return c
}

func newTestUpgrade(t *testing.T, fixture string) *osUpgrade {
	t.Helper()
	cfg := Config{}
	cfg.Resource.Name = "r0"
	return newOSUpgrade(cfg, nil, dialStatus(t, fixture), guest.Config{}, func(string, ...any) {})
}

// THE OS UPGRADE. What matters is less the sequence than its shape: an OS upgrade that does
// not touch the workload. newSwitchUpgrade and assertLeftTheWorkloadAlone below are the rig for
// asserting that directly (no app verb, no data snapshot); no test in this file calls them.
//
// No unit test can prove the rollback LEG. Restoring the point means stopping a real VM and
// reverting a real qcow2, neither of which exists in a unit test; that is an end-to-end rollback
// test's, on the shipped disk.

// recExec is a guest that answers every command benignly and records what it was asked. The
// opposite of statusExec above: that one refuses everything, this one permits everything so the
// whole sequence runs and the test can read back what it did -- and, more to the point, what it
// did not.
type recExec struct {
	mu      sync.Mutex
	cmds    []string
	current string // what /run/current-system resolves to
}

func (e *recExec) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	e.mu.Lock()
	e.cmds = append(e.cmds, strings.TrimSpace(name+" "+strings.Join(args, " ")))
	e.mu.Unlock()
	switch {
	case name == "readlink":
		return []byte(e.current + "\n"), nil
	case name == "systemctl" && len(args) > 0 && args[0] == "is-active":
		// Any unit asked about is reported DOWN, deliberately, so a path that passes only on an
		// active unit cannot pass here.
		return []byte("inactive\n"), nil
	}
	return nil, nil
}

func (e *recExec) WriteFile(path string, _ []byte) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.cmds = append(e.cmds, "write "+path)
	return nil
}

func (e *recExec) ReadFile(string) ([]byte, error) { return nil, nil }
func (e *recExec) Sethostname(string) error        { return nil }

func (e *recExec) all() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.cmds...)
}

func (e *recExec) count(substr string) int {
	n := 0
	for _, c := range e.all() {
		if strings.Contains(c, substr) {
			n++
		}
	}
	return n
}

func (e *recExec) ran(substr string) bool { return e.count(substr) > 0 }

// fakeQMP is QEMU's monitor, reduced to what the protocol requires: an unprompted greeting, a
// reply per command, and a record of what was asked. Serving it over a real unix socket keeps
// the whole of qmp.go in the path -- dial, greeting, capabilities handshake, framing -- rather
// than stubbing the one call under test.
type fakeQMP struct {
	mu   sync.Mutex
	cmds []string
	// replies overrides the default `{"return":{}}` for named commands -- query-accelerators
	// answers with a payload, and the name in it is the whole point of asking. Fixed at construction
	// so the serving goroutine only ever reads it; a test that set it later would race its fake.
	replies map[string]string
}

func startQMP(t *testing.T) (*fakeQMP, string) { return startQMPWith(t, nil) }

func startQMPWith(t *testing.T, replies map[string]string) (*fakeQMP, string) {
	t.Helper()
	q := &fakeQMP{replies: replies}
	path := filepath.Join(testsock.Dir(t), "qmp.sock")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go q.serve(c)
		}
	}()
	return q, path
}

func (q *fakeQMP) serve(c net.Conn) {
	defer c.Close()
	if _, err := c.Write([]byte("{\"QMP\":{\"version\":{}}}\n")); err != nil {
		return
	}
	dec := json.NewDecoder(c)
	for {
		var req struct {
			Execute string `json:"execute"`
		}
		if err := dec.Decode(&req); err != nil {
			return
		}
		if req.Execute != "qmp_capabilities" { // handshake, not an operation
			q.mu.Lock()
			q.cmds = append(q.cmds, req.Execute)
			q.mu.Unlock()
		}
		reply, ok := q.replies[req.Execute]
		if !ok {
			reply = `{"return":{}}`
		}
		if _, err := c.Write([]byte(reply + "\n")); err != nil {
			return
		}
	}
}

func (q *fakeQMP) got() []string {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]string(nil), q.cmds...)
}

// newSwitchUpgrade wires a whole node: a permissive guest over a real control channel, a real
// QMP endpoint, and a reverse proxy that answers healthy or does not.
func newSwitchUpgrade(t *testing.T, healthy bool) (*osUpgrade, *recExec, *fakeQMP) {
	t.Helper()
	x := &recExec{current: "/nix/store/prev"}
	cconn, sconn := net.Pipe()
	go guestagent.Serve(context.Background(), sconn, x)
	c := guestagent.NewClient(cconn)
	t.Cleanup(func() { c.Close() })

	code := http.StatusOK
	if !healthy {
		code = http.StatusServiceUnavailable
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(code)
	}))
	t.Cleanup(srv.Close)

	q, sock := startQMP(t)
	cfg := Config{QMPSock: sock}
	cfg.Resource.Name = "r0"
	u := newOSUpgrade(cfg, platform.Adopt(cfg.guestSpec()), c, guest.Config{
		HealthURL:      srv.URL,
		ReactorSnippet: "briard", // the promoter hold IS the quiesce
		Logf:           t.Logf,
	}, t.Logf)
	return u, x, q
}

// assertLeftTheWorkloadAlone checks the OS path left the workload alone: no app lifecycle, no app
// data. It reads the recorded commands rather than the code because that is where a regression
// would show up -- a quiesce or data snapshot is a command, whoever calls it.
func assertLeftTheWorkloadAlone(t *testing.T, x *recExec) {
	t.Helper()
	for _, c := range x.all() {
		switch {
		case strings.Contains(c, "podman-"):
			t.Errorf("the OS path drove an app unit: %q", c)
		case strings.Contains(c, "btrfs subvolume"):
			t.Errorf("the OS path touched app data: %q", c)
		}
	}
}

// THE CLEAN STOP. Every stop of a guest that comes back goes through it -- the image upgrade, the
// rollback leg, the recovery ladder's power cycle and the rescue -- so DRBD records its quorum
// state on the way down. They depend on the two halves of one contract: that the agent route is
// tried first, and that the power button really is an independent fallback rather than a comment.
// Both are asserted here, because in the field they fail apart.
//
// What this rig cannot show is restoreImage CHOOSING the clean stop: that needs a running VM and
// a real qcow2, so it is asserted where those exist (an end-to-end rollback test, on the log
// line the else-branch emits).

// newStopRig gives a guest that is already down -- so WaitStopped returns the moment it is asked
// -- beside a live QMP endpoint. That combination is what lets the ordering be read off cleanly:
// whether QMP was touched at all is the whole answer.
func newStopRig(t *testing.T) (*platform.Guest, *fakeQMP) {
	t.Helper()
	q, sock := startQMP(t)
	spec := platform.QEMUSpec{QMPSock: sock, Unit: "briard-test-absent.service"}
	return platform.Adopt(spec), q
}

func TestStopCleanlyAsksTheAgentBeforeThePowerButton(t *testing.T) {
	x := &recExec{current: "/nix/store/prev"}
	cconn, sconn := net.Pipe()
	go guestagent.Serve(context.Background(), sconn, x)
	c := guestagent.NewClient(cconn)
	t.Cleanup(func() { c.Close() })
	g, q := newStopRig(t)

	if err := stopCleanly(context.Background(), g, c, t.Logf); err != nil {
		t.Fatalf("a guest whose agent answers must stop cleanly: %v", err)
	}
	if !x.ran("systemctl poweroff") {
		t.Errorf("the guest OS was never asked to shut itself down: %q", x.all())
	}
	// The point of the ordering: ACPI is the fallback for an agent that is gone, so a working
	// agent must mean the power button is never pressed.
	if got := q.got(); len(got) != 0 {
		t.Errorf("the agent route worked, so QMP must not have been touched; got %q", got)
	}
}

// The lost reply: `os.poweroff` is `--no-block` so its reply can lose the race with systemd
// tearing the machine down, and the host then sees EOF — indistinguishable from a dead agent.
// Escalating finds no monitor socket either (the guest is already gone), and reading the two
// joined errors as a refusal force-kills a guest that had ALREADY stopped cleanly. Both errors
// are about a REQUEST; only "is the VM still there?" is about the machine. Here the agent
// refuses rather than hangs up, and the unit is absent, so it reads inactive.
func TestStopCleanlyTreatsAnAlreadyStoppedGuestAsClean(t *testing.T) {
	// A guest that answers nothing and whose monitor socket does not exist — i.e. one that is
	// gone. Its unit is inactive, which is the only fact that actually settles it.
	cconn, sconn := net.Pipe()
	go guestagent.Serve(context.Background(), sconn, &statusExec{})
	c := guestagent.NewClient(cconn)
	t.Cleanup(func() { c.Close() })
	g := platform.Adopt(platform.QEMUSpec{
		QMPSock: filepath.Join(testsock.Dir(t), "absent.sock"), // never listened on
		Unit:    "briard-test-absent.service",
	})

	if err := stopCleanly(context.Background(), g, c, t.Logf); err != nil {
		t.Fatalf("a guest that is already down has stopped cleanly; got %v", err)
	}
}

// fakeGuest is a guest whose STATE the test controls, which the socket-and-unit rig above
// cannot do: `newStopRig`'s unit does not exist, so its machine reads as already-stopped no
// matter what, and the one state the lost reply lives in -- STOPPING, i.e. QEMU gone but the unit not
// yet reaped -- has no expression there. The QMP wiring these fakes stand in for is asserted
// directly in agent/platform/qmp_test.go; what is under test here is the ESCALATION DECISION.
//
//	stopsAfter  how long until the machine is down (guestStays = never on its own)
//	shutdownErr what the power button does; nil means it works, and a working press stops it
type fakeGuest struct {
	stopsAfter  time.Duration
	shutdownErr error
	started     time.Time
	shutdowns   int
}

const guestStays = time.Hour // longer than any grace: this machine will not stop by itself

// WaitStopped answers from the fake's own clock, and COMPRESSES the grace it is handed: a real
// shutdownGrace is 30s, and a test that genuinely waited two of them to prove a timeout would
// cost a minute to say something the fake already knows. The distinction the tests actually
// need survives compression -- a zero grace is a glance and must still see a running machine,
// while any real grace is a wait and must see it stop.
func (g *fakeGuest) WaitStopped(ctx context.Context, grace time.Duration) error {
	if grace > 200*time.Millisecond {
		grace = 200 * time.Millisecond
	}
	deadline := time.Now().Add(grace)
	for {
		if time.Since(g.started) >= g.stopsAfter {
			return nil
		}
		if time.Now().After(deadline) {
			return errors.New("guest still running")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func (g *fakeGuest) Shutdown(context.Context, time.Duration) error {
	g.shutdowns++
	if g.shutdownErr != nil {
		return g.shutdownErr
	}
	g.stopsAfter = 0 // the press landed and the machine went down
	return nil
}

// THE LOST REPLY, on a guest that is STILL STOPPING rather than already stopped.
//
// `os.poweroff` is `--no-block`, so a guest doing exactly as it was told can close the channel
// before the reply lands: the host sees EOF, which is indistinguishable from a dead agent. Both
// routes then fail — the agent is unreachable, and the power button has nothing to press because
// QEMU has already exited — and the verdict comes down to the last probe. Asked with a ZERO
// grace, it would answer "still running" during the seconds between QEMU exiting and the unit going
// inactive, so a guest that had shut down perfectly would be recorded as having refused and its
// upgrade abandoned.
//
// Only a STANDBY hits it: a node with a workload to quiesce takes long enough that the reply
// wins the race, so a live tier can stay green while this path is broken.
//
// Note what is NOT asserted: that the power button went unpressed. Pressing it is harmless here
// and costs nothing — the dial fails at once precisely because QEMU is gone — and forbidding it
// would mean waiting before escalating, which would slow every genuinely dead agent down by a
// full grace for no gain. The failure is not the press; it is the verdict after it.
func TestStopCleanlyWaitsOutAGuestThatIsStillStopping(t *testing.T) {
	// A channel that is closed under us: PowerOff gets EOF, the shape of a lost reply.
	cconn, sconn := net.Pipe()
	sconn.Close()
	c := guestagent.NewClient(cconn)
	t.Cleanup(func() { c.Close() })

	// QEMU has already exited, so there is no monitor socket left to dial -- the error is
	// `dial ...guest.sock: no such file or directory`. The machine finishes stopping shortly
	// after, which is the window a zero-grace probe falls into: a glance says "running", the
	// wait says "gone".
	g := &fakeGuest{
		stopsAfter:  50 * time.Millisecond,
		shutdownErr: errors.New("dial QMP: no such file or directory"),
		started:     time.Now(),
	}
	if err := stopCleanly(context.Background(), g, c, t.Logf); err != nil {
		t.Fatalf("a guest that goes down on its own has stopped cleanly; got %v", err)
	}
}

// The fallback still has to fire for the case it exists for: the agent route is unusable AND the
// machine is genuinely still up.
//
// It runs on a fakeGuest, not `newStopRig`: that rig's unit does not exist, so its machine
// always reads as ALREADY STOPPED and cannot express "still up". A guest that must be stopped is
// modelled here as one that does not stop by itself; the QMP wiring the press travels over is
// asserted in agent/platform/qmp_test.go.
func TestStopCleanlyFallsBackToThePowerButton(t *testing.T) {
	// statusExec refuses every command it is sent here, which is what a guest whose agent is
	// present but broken looks like -- and near enough to one that is gone for this route's purpose.
	cconn, sconn := net.Pipe()
	go guestagent.Serve(context.Background(), sconn, &statusExec{})
	c := guestagent.NewClient(cconn)
	t.Cleanup(func() { c.Close() })
	g := &fakeGuest{stopsAfter: guestStays, started: time.Now()} // and the press works

	if err := stopCleanly(context.Background(), g, c, t.Logf); err != nil {
		t.Fatalf("a refused os.poweroff must fall through to ACPI, not fail: %v", err)
	}
	if g.shutdowns != 1 {
		t.Errorf("want the power button pressed exactly once, got %d presses", g.shutdowns)
	}
}

// And it must still FAIL when neither route works on a machine that stays up. This is the
// outcome the image upgrade depends on being loud: it abandons the upgrade and leaves the node
// serving its current image rather than power-cutting it. Worth pinning next to the two
// clean-stop cases above, because the wait for a lost reply lengthens how long this takes to
// decide and it would be easy to lose the refusal itself.
func TestStopCleanlyReportsAGuestThatWillNotGoDown(t *testing.T) {
	cconn, sconn := net.Pipe()
	go guestagent.Serve(context.Background(), sconn, &statusExec{})
	c := guestagent.NewClient(cconn)
	t.Cleanup(func() { c.Close() })
	g := &fakeGuest{
		stopsAfter:  guestStays,
		shutdownErr: errors.New("dial QMP: no such file or directory"),
		started:     time.Now(),
	}

	err := stopCleanly(context.Background(), g, c, t.Logf)
	if err == nil {
		t.Fatal("both routes failed on a running guest — that must be reported, not passed off as a clean stop")
	}
	// Both routes' evidence, not just the last one: the caller's message says "clean shutdown
	// refused", and which route refused is the first thing anyone reading it needs.
	if !strings.Contains(err.Error(), "refusing systemctl poweroff") || !strings.Contains(err.Error(), "dial QMP") {
		t.Errorf("want both the agent and the power-button failure in the error, got %v", err)
	}
}

// stateSeq answers systemRunning's reads in order, repeating the last one.
type stateSeq struct {
	answers []guestagent.SystemState
	errs    []error
	reads   int
}

func (s *stateSeq) SystemState(context.Context) (guestagent.SystemState, error) {
	i := min(s.reads, len(s.answers)-1)
	s.reads++
	return s.answers[i], s.errs[i]
}

// The OS gate's generic check: only `running` passes; boot jobs still queued and a channel error
// are waited out; any other state is the answer on FIRST sight -- a degraded guest is never
// polled until something resets its failed units into looking healthy.
func TestSystemRunningReadsTheAnswerOnce(t *testing.T) {
	st := func(s string, failed ...string) guestagent.SystemState {
		return guestagent.SystemState{State: s, Failed: failed}
	}
	chanErr := errors.New("channel reset")
	for _, tc := range []struct {
		name      string
		answers   []guestagent.SystemState
		errs      []error
		wantErr   string // "" = pass
		wantReads int
	}{
		{"running passes", []guestagent.SystemState{st("running")}, []error{nil}, "", 1},
		{"degraded fails and names its units",
			[]guestagent.SystemState{st("degraded", "systemd-modules-load.service")}, []error{nil},
			"failed units systemd-modules-load.service", 1},
		{"a standby's failed promote unit passes",
			[]guestagent.SystemState{st("degraded", "drbd-promote@r0.service")}, []error{nil}, "", 1},
		{"a promote unit does not hide another failure",
			[]guestagent.SystemState{st("degraded", "drbd-promote@r0.service", "z.service")}, []error{nil},
			"failed units z.service", 1},
		{"degraded is final even if a reset would follow",
			[]guestagent.SystemState{st("degraded", "x.service"), st("running")}, []error{nil, nil}, "degraded", 1},
		{"starting is waited out, then judged",
			[]guestagent.SystemState{st("starting"), st("starting"), st("running")}, []error{nil, nil, nil}, "", 3},
		{"starting then degraded fails",
			[]guestagent.SystemState{st("starting"), st("degraded", "y.service")}, []error{nil, nil}, "y.service", 2},
		{"a channel error is retried",
			[]guestagent.SystemState{{}, st("running")}, []error{chanErr, nil}, "", 2},
		{"any other state fails", []guestagent.SystemState{st("maintenance")}, []error{nil}, "maintenance", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := &stateSeq{answers: tc.answers, errs: tc.errs}
			err := systemRunning(context.Background(), g, time.Millisecond)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Errorf("err = %v, want pass", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Errorf("err = %v, want one containing %q", err, tc.wantErr)
			}
			if g.reads != tc.wantReads {
				t.Errorf("reads = %d, want %d", g.reads, tc.wantReads)
			}
		})
	}
}

// A guest that never leaves `starting` fails when the gate's budget runs out, not never.
func TestSystemRunningGivesUpWithTheBudget(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	g := &stateSeq{answers: []guestagent.SystemState{{State: "starting"}}, errs: []error{nil}}
	if err := systemRunning(ctx, g, time.Millisecond); err == nil || !strings.Contains(err.Error(), "never settled") {
		t.Errorf("err = %v, want never settled", err)
	}
}

// The recovery ladder entered from a bring-up that never got a channel stops the guest with NO
// client at all. The agent route is then known unavailable before it is tried -- straight to the
// button, and the verdict is the unit's state as ever. Without the nil check this is a nil
// dereference on the first call.
func TestStopCleanlyWithoutAChannelGoesToTheButton(t *testing.T) {
	g := platform.Adopt(platform.QEMUSpec{
		QMPSock: filepath.Join(testsock.Dir(t), "absent.sock"), // never listened on: the button finds nothing
		Unit:    "briard-test-absent.service",
	})
	if err := stopCleanly(context.Background(), g, nil, t.Logf); err != nil {
		t.Fatalf("no channel, no monitor, no unit: the guest is down, which is clean; got %v", err)
	}
}
