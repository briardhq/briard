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
// a captured fixture, and refuses everything else. Refusing the rest is the point: it proves the
// gate decides before the sequence touches anything, because a gate that ran later would trip
// over one of these instead.
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

// The handover gate. A reboot is a failover on an HA pair, and scheduling a failover is the cloud's
// job -- so a node that would hand its work to a peer declines instead of demoting itself.
func TestRebootUpgradeDeclinesWhenAPeerCanTakeOver(t *testing.T) {
	u := newTestUpgrade(t, "status-primary.json") // node2 is connected, diskful, UpToDate

	back, err := u.RebootUpgrade(context.Background(), "/nix/store/target")
	if !errors.Is(err, ErrHandoverRequired) {
		t.Fatalf("want ErrHandoverRequired, got %v", err)
	}
	if !back {
		t.Error("a declined upgrade leaves the node on its previous generation, so rolledBack must be true")
	}
	// The refusal has to name the successor, or a human reading the log has to reproduce the
	// state to learn why their upgrade did not happen.
	if !strings.Contains(err.Error(), "node2") || !strings.Contains(err.Error(), "CAN take over") {
		t.Errorf("the refusal should name the peer it declined for; got %v", err)
	}
	// ...and it must NOT have been the witness that decided it: a diskless peer is quorate
	// company, not a successor.
	if !strings.Contains(err.Error(), "witness[Secondary connected=true diskful=false") {
		t.Errorf("the witness should be reported as unable to take over; got %v", err)
	}
}

// The other half, and the one that makes the test above mean something: a node with no possible
// successor is exactly where a local reboot upgrade IS allowed, because it stays Primary
// throughout and the payload-shaped health gate is then the right question.
//
// status-lone-anchor is the case that pins the predicate down. A sole diskful anchor beside a
// connected witness is Primary AND fully quorate AND has nobody to take over -- so it is the one
// fixture where "quorate without me" and "someone can take over" disagree. Swap the gate to the
// quorum-shaped test this gate warns against, and this test goes red while every other one
// stays green:
// the node would sail through, reboot, and take the house down to install an update.
func TestRebootUpgradeProceedsWithNoSuccessor(t *testing.T) {
	for _, fixture := range []string{
		"status-noquorum.json",    // peers unreachable: no quorum and no successor
		"status-lone-anchor.json", // quorate, and still no successor
	} {
		t.Run(fixture, func(t *testing.T) {
			u := newTestUpgrade(t, fixture)

			_, err := u.RebootUpgrade(context.Background(), "/nix/store/target")
			if errors.Is(err, ErrHandoverRequired) {
				t.Fatalf("a node with no possible successor must not be declined: %v", err)
			}
			// It fails, of course -- statusExec answers nothing else -- and that failure is the
			// proof it got past the gate and into the sequence.
			if err == nil {
				t.Fatal("want the sequence to proceed (and then fail on the refusing guest), got success")
			}
		})
	}
}

// A node that cannot read its own cluster state declines too. Not knowing whether a peer would
// take over is not the same as knowing none would, and the two answers are a reboot apart.
func TestRebootUpgradeDeclinesWhenTheClusterIsUnreadable(t *testing.T) {
	cconn, sconn := net.Pipe()
	// A guest whose drbdsetup fails: every verb errors, including the status read.
	go guestagent.Serve(context.Background(), sconn, &statusExec{status: []byte("not json")})
	c := guestagent.NewClient(cconn)
	t.Cleanup(func() { c.Close() })
	cfg := Config{}
	cfg.Resource.Name = "r0"
	u := newOSUpgrade(cfg, nil, c, guest.Config{}, func(string, ...any) {})

	back, err := u.RebootUpgrade(context.Background(), "/nix/store/target")
	if err == nil {
		t.Fatal("want a refusal when the cluster cannot be read")
	}
	if !strings.Contains(err.Error(), "read cluster") {
		t.Errorf("the refusal should say the read is what failed; got %v", err)
	}
	if !back {
		t.Error("nothing moved, so the node is still on its previous generation")
	}
}

// THE SWITCH METHOD. What these two tests are for is less the sequence than the
// shape gave it: an OS upgrade that does not touch the workload, and one rollback for
// both methods. Both are things the code can lose silently, so both are asserted directly --
// no payload verb, no data snapshot, and exactly one activation on the failing run.
//
// What they cannot do is prove the rollback LEG. Restoring the point means stopping a real VM
// and reverting a real qcow2, neither of which exists in a unit test; that is the lab7(f)'s, on
// the shipped disk. What is proven here is the decision to go there, and that nothing was put
// back in band on the way.

// recExec is a guest that answers every command benignly and records what it was asked. The
// opposite of statusExec above, and for the opposite purpose: that one refuses everything so
// the gate has to decide first, this one permits everything so the whole sequence runs and the
// test can read back what it did -- and, more to the point, what it did not.
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
		// Any unit asked about is reported DOWN, deliberately. The OS gate must ask the front
		// door alone (and deletion 5), so a gate that consults a unit cannot pass
		// here -- which is what stops this from being a test that would survive the regression.
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
}

func startQMP(t *testing.T) (*fakeQMP, string) {
	t.Helper()
	q := &fakeQMP{}
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
		if _, err := c.Write([]byte("{\"return\":{}}\n")); err != nil {
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
// QMP endpoint, and a front door that answers healthy or does not.
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
		ReactorSnippet: "briard", // the promoter hold IS the quiesce now
		Logf:           t.Logf,
	}, t.Logf)
	return u, x, q
}

// The commit leg, and the principle it has to hold to: the new OS runs the same containers on
// the same data, so the sequence takes a rollback point, activates, gates on the front door,
// collects and drops the point -- and says nothing to the workload at any step.
func TestSwitchUpgradeCommitsWithoutTouchingTheWorkload(t *testing.T) {
	u, x, q := newSwitchUpgrade(t, true)

	back, err := u.Upgrade(context.Background(), "/nix/store/target")
	if err != nil {
		t.Fatalf("a healthy switch upgrade must commit: %v", err)
	}
	if back {
		t.Error("nothing rolled back, so rolledBack must be false")
	}

	if !x.ran("/nix/store/target/bin/switch-to-configuration switch") {
		t.Errorf("the target was never activated: %q", x.all())
	}
	// Phases A and C: the point is taken live before the switch and dropped at commit, and it
	// is the ONLY thing this path asks of QEMU.
	if got := q.got(); len(got) != 2 ||
		got[0] != "blockdev-snapshot-internal-sync" || got[1] != "blockdev-snapshot-delete-internal-sync" {
		t.Errorf("want the rollback point taken then dropped, got %q", got)
	}
	if !x.ran("nix-collect-garbage") {
		t.Error("the displaced generation was never collected (commit is GC's trigger)")
	}
	// The quiesce is the promoter hold, and nothing else.
	if !x.ran("systemctl stop drbd-reactor.service") || !x.ran("systemctl start drbd-reactor.service") {
		t.Errorf("the promoter must be held across the switch and released after: %q", x.all())
	}
	assertLeftTheWorkloadAlone(t, x)
	// The gate asked the front door and nothing else. A unit-shaped conjunct would have found
	// "inactive" above and never passed -- so this run reaching commit is itself the proof, and
	// the explicit check names what would have caused it.
	if x.ran("is-active") {
		t.Errorf("the OS gate asked a unit whether it was running; it must ask the front door alone: %q", x.all())
	}
}

// The failing leg. Two claims: the gate still trips (the sequence did not simply stop gating),
// and the rollback goes through the DISK -- one path for both methods -- rather than switching
// back in band the way the retired guest-side sequence did.
func TestSwitchUpgradeRollsBackThroughTheDiskNotAnInBandSwitchBack(t *testing.T) {
	u, x, q := newSwitchUpgrade(t, false) // the front door never comes back green

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := u.Upgrade(ctx, "/nix/store/target")
	if err == nil {
		t.Fatal("a gate that never passes must not commit")
	}
	// It went to the rollback leg. That leg then fails here, because a unit test has no disk to
	// restore -- proving it was ATTEMPTED is the assertion this rig can carry; the lab demo (f) proves
	// it works, on the shipped artifact.
	if !strings.Contains(err.Error(), "rollback") {
		t.Errorf("a tripped gate must go to the rollback leg; got %v", err)
	}
	// Exactly one activation, and it is the target. The retired shape activated twice -- target,
	// then back to prev -- so a second one here would mean the in-band ladder had come back.
	if n := x.count("switch-to-configuration"); n != 1 {
		t.Errorf("want exactly one activation (the target), got %d: %q", n, x.all())
	}
	if x.ran("/nix/store/prev/bin/switch-to-configuration") {
		t.Error("the switch path rolled back in band; the rollback point is the OS disk, for both methods")
	}
	// The rollback point was taken and NOT dropped: the upgrade did not commit, and the tag's
	// presence is what tells a restarting agent an upgrade was in flight (snapshot.go).
	if got := q.got(); len(got) != 1 || got[0] != "blockdev-snapshot-internal-sync" {
		t.Errorf("want the rollback point taken and left standing, got %q", got)
	}
	if x.ran("nix-collect-garbage") {
		t.Error("a rolled-back upgrade must not collect: the generation it would be free to drop is the one the node is going back to")
	}
	assertLeftTheWorkloadAlone(t, x)
}

// assertLeftTheWorkloadAlone is as a check: no payload lifecycle, no service data. It
// reads the recorded commands rather than the code because that is where a regression would
// show up -- a re-added quiesce or data snapshot is a command, whoever calls it.
func assertLeftTheWorkloadAlone(t *testing.T, x *recExec) {
	t.Helper()
	for _, c := range x.all() {
		switch {
		case strings.Contains(c, "podman-"):
			t.Errorf("the OS path drove the payload unit: %q", c)
		case strings.Contains(c, "btrfs subvolume"):
			t.Errorf("the OS path touched service data: %q", c)
		}
	}
}

// THE CLEAN STOP. Two callers depend on it now -- the reboot path, so the guest's
// freshly rewritten bootloader survives, and the rollback leg, so DRBD records its quorum state
// on the way down -- and they depend on different halves of the same contract: that
// the agent route is tried first, and that the power button really is an independent fallback
// rather than a comment. Both are asserted here, because in the field they fail apart.
//
// What this rig cannot show is restore() CHOOSING the clean stop: that needs a running VM and a
// real qcow2, so it is asserted where those exist (our end-to-end rollback demo, on the log
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

// The case that defeated live: `os.poweroff` is `--no-block` so its reply can lose the race
// with systemd tearing the machine down, and the host then sees EOF — indistinguishable from a dead
// agent. Escalating found no monitor socket either (the guest was already gone), the two errors
// joined, and the caller force-killed a guest that had ALREADY stopped cleanly. Both errors are
// about a REQUEST; only "is the VM still there?" is about the machine.
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

func TestStopCleanlyFallsBackToThePowerButton(t *testing.T) {
	// StatusExec refuses every command, which is what a guest whose agent is present but broken
	// looks like -- and near enough to one that is gone for this route's purpose.
	cconn, sconn := net.Pipe()
	go guestagent.Serve(context.Background(), sconn, &statusExec{})
	c := guestagent.NewClient(cconn)
	t.Cleanup(func() { c.Close() })
	g, q := newStopRig(t)

	if err := stopCleanly(context.Background(), g, c, t.Logf); err != nil {
		t.Fatalf("a refused os.poweroff must fall through to ACPI, not fail: %v", err)
	}
	if got := q.got(); len(got) != 1 || got[0] != "system_powerdown" {
		t.Errorf("want the power button pressed exactly once, got %q", got)
	}
}

// A node that is NOT SERVING has nothing to hand over, so the gate must let it through.
// The gate's own doc says the refusal is for "this node is serving, a peer could take the work" —
// but the predicate only ever asked about PEERS, never about this node's own role, so a Secondary
// (and even the diskless witness, which holds nothing at all) was declined too.
//
// That matters for the two ordinary update shapes: a single node, and — the common one — rolling
// an HA pair by upgrading the SECONDARY first. Both are exactly where a reboot is safe: the peer
// stays up, so the rebooting node comes back to two visible storage nodes and re-quorates.
func TestRebootUpgradeProceedsWhenThisNodeIsNotServing(t *testing.T) {
	// Status-witness.json: this node is Secondary (a diskless witness), with a healthy anchor peer
	// that "can take over" — the condition that used to decline the upgrade.
	u := newTestUpgrade(t, "status-witness.json")

	_, err := u.RebootUpgrade(context.Background(), "/nix/store/target")
	if errors.Is(err, ErrHandoverRequired) {
		t.Fatalf("a node that is not Primary hands nothing over; it must not be declined: %v", err)
	}
	// It still fails afterwards — statusExec answers nothing else — and that failure is the proof
	// it got past the gate rather than being refused by it.
	if err == nil {
		t.Fatal("want the sequence to proceed (and then fail on the refusing guest), got success")
	}
}
