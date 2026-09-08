package guestfirmware

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func binDirs(t *testing.T) (string, string) {
	t.Helper()
	dir, run := t.TempDir(), t.TempDir()
	t.Setenv("BRIARD_BIN_DIR", dir)
	t.Setenv("BRIARD_BIN_RUN", run)
	return dir, run
}

// The name table is closed, and a digest that does not match leaves no .next behind.
func TestBinStageRefusesStrangersAndBadDigests(t *testing.T) {
	dir, _ := binDirs(t)
	if err := stageChunk(BinChunk{Name: "evil", Seq: 0, Data: []byte("x"), Last: true, SHA256: "00"}); err == nil {
		t.Error("a name outside the table was accepted")
	}
	if err := stageChunk(BinChunk{Name: "briard-reverse-proxy", Seq: 0, Data: []byte("x"), Last: true, SHA256: "0000"}); err == nil {
		t.Error("a wrong digest was accepted")
	}
	if err := stageChunk(BinChunk{Name: "briard-reverse-proxy", Seq: 0, Data: []byte("x"), Last: true}); err == nil {
		t.Error("a last chunk with no digest was accepted")
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		t.Errorf("refused stages left files behind: %v", ents)
	}
}

// stageSet writes a staged file for every name of the set.
func stageSet(t *testing.T, dir string) {
	t.Helper()
	for _, n := range BinNames {
		if err := os.WriteFile(filepath.Join(dir, n+".next"), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

// Activation arms a trial flag per binary, records the release for the commit, and restarts
// ONE unit: the guest agent's own, whose start is the verdict on the rest ([B.138]). Nothing is
// armed or restarted when any named binary is not staged, or when the set leaves the agent out.
func TestBinActivateArmsTheSetAndRestartsTheAgentAlone(t *testing.T) {
	dir, run := binDirs(t)
	fx := &fakeExec{}
	g := dial(t, fx)
	if err := g.Call(context.Background(), VerbBinActivate, BinActivation{Release: "v3.20260907.abc1234", Names: BinNames}, nil); err == nil {
		t.Fatal("activated with nothing staged")
	}
	if len(fx.runs) != 0 {
		t.Fatalf("restarted units for an unstaged set: %v", fx.runs)
	}
	stageSet(t, dir)
	if err := g.Call(context.Background(), VerbBinActivate, BinActivation{Release: "v3.20260907.abc1234", Names: []string{"briard-dashboard", "briard-reverse-proxy"}}, nil); err == nil {
		t.Fatal("a set without the guest agent was activated -- nothing would trial it")
	}
	if len(fx.runs) != 0 {
		t.Fatalf("restarted units for an agent-less set: %v", fx.runs)
	}
	if err := g.Call(context.Background(), VerbBinActivate, BinActivation{Release: "v3.20260907.abc1234", Names: BinNames}, nil); err != nil {
		t.Fatalf("bin.activate: %v", err)
	}
	want := [][]string{
		{"systemd-run", "--quiet", "--collect", "--on-active=1", "--timer-property=AccuracySec=100ms", "systemctl", "restart", "briard-guest-agent.service"},
	}
	if !reflect.DeepEqual(fx.runs, want) {
		t.Errorf("restarts = %v, want the agent's unit alone: %v", fx.runs, want)
	}
	for _, n := range BinNames {
		if _, err := os.Stat(filepath.Join(run, n+".update")); err != nil {
			t.Errorf("%s was not armed: %v", n, err)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "RELEASE.next")); string(b) != "v3.20260907.abc1234\n" {
		t.Errorf("RELEASE.next = %q", b)
	}
	if err := g.Call(context.Background(), VerbBinActivate, BinActivation{Release: "bad id/", Names: BinNames}, nil); err == nil {
		t.Error("a release id with a slash was accepted")
	}
}

// THE CHEAP GATE ([B.138]): bin.test runs every staged copy's --test-launch in order; the first
// failure names the binary and discards the WHOLE staged set, so nothing half-proven is ever
// armed and a later non-trial start finds nothing to clean up.
func TestBinTestProvesTheSetOrDiscardsIt(t *testing.T) {
	dir, _ := binDirs(t)
	fx := &fakeExec{}
	g := dial(t, fx)
	if err := g.Call(context.Background(), VerbBinTest, BinTest{Names: BinNames}, nil); err == nil {
		t.Fatal("tested with nothing staged")
	}
	stageSet(t, dir)
	if err := g.Call(context.Background(), VerbBinTest, BinTest{Names: BinNames}, nil); err != nil {
		t.Fatalf("bin.test: %v", err)
	}
	want := [][]string{
		{filepath.Join(dir, "briard-dashboard.next"), "--test-launch"},
		{filepath.Join(dir, "briard-reverse-proxy.next"), "--test-launch"},
		{filepath.Join(dir, "briard-guest-agent.next"), "--test-launch"},
	}
	if !reflect.DeepEqual(fx.runs, want) {
		t.Errorf("test launches = %v, want %v", fx.runs, want)
	}
	if got := stagedSet(); !reflect.DeepEqual(got, BinNames) {
		t.Errorf("a passing test discarded the set: %v", got)
	}

	// The door fails: the error names it, and the set is gone -- every staged file, the release.
	if err := os.WriteFile(filepath.Join(dir, "RELEASE.next"), []byte("v3.x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fx.runFn = func(name string, args []string) ([]byte, error) {
		if strings.HasSuffix(name, "briard-reverse-proxy.next") {
			return []byte("reverse-proxy: test launch: listen: no such device"), errors.New("exit status 1")
		}
		return nil, nil
	}
	err := g.Call(context.Background(), VerbBinTest, BinTest{Names: BinNames}, nil)
	if err == nil || !strings.Contains(err.Error(), "briard-reverse-proxy failed its test launch") || !strings.Contains(err.Error(), "no such device") {
		t.Fatalf("bin.test error = %v; want the door named, with its output", err)
	}
	if got := stagedSet(); got != nil {
		t.Errorf("a failed test left staged files: %v", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "RELEASE.next")); !os.IsNotExist(err) {
		t.Error("a failed test left RELEASE.next")
	}
}

// systemctlFake answers is-active/try-restart for the doors the way a guest would: `active`
// names the units that are running, `fail` the one whose real launch fails.
type systemctlFake struct {
	active  map[string]bool
	fail    string // a unit whose staged copy fails its start and whose restart job reports it
	reverts string // a unit whose staged copy fails and whose AUTO-RESTART repairs it: the job succeeds
	run_    string // where the picker's markers go
	runs    [][]string
}

func (s *systemctlFake) run(name string, args []string) ([]byte, error) {
	s.runs = append(s.runs, append([]string{name}, args...))
	if name != "systemctl" || len(args) == 0 {
		return nil, nil
	}
	unit := args[len(args)-1]
	bin := strings.TrimSuffix(unit, ".service")
	switch args[0] {
	case "is-active":
		if s.active[unit] {
			return []byte("active\n"), nil
		}
		return []byte("inactive\n"), errors.New("exit status 3")
	case "try-restart":
		if !s.active[unit] {
			return nil, nil // not running here: try-restart does nothing at all
		}
		if unit == s.fail {
			s.active[unit] = false // the failed start, reported by the job
			s.mark(bin, "trial")
			return []byte("Job for " + unit + " failed because the control process exited with error code."), errors.New("exit status 1")
		}
		if unit == s.reverts {
			// THE TRAP: the staged copy failed, systemd's auto-restart put the unit back on the
			// committed binary, and the restart job reports success. Only the picker's marker
			// says what is actually running.
			s.mark(bin, "pushed")
			return nil, nil
		}
		s.mark(bin, s.pick(bin)) // the picker chooses by the arm flag, staged or committed
	}
	return nil, nil
}

// mark models what restarting a promoter chain member actually does: that member AND everything
// ordered after it go down and come back in the same transaction, each through its own picker.
// A fake without this cannot show why restarting the doors in the wrong order cancels a start
// job -- which is the demote this item measured.
func (s *systemctlFake) mark(bin, what string) {
	if s.run_ == "" {
		return
	}
	carried := false
	for _, n := range doorNames {
		if n == bin {
			carried = true
			s.write(n, what)
			continue
		}
		if carried {
			// A member carried by the restart goes through its OWN picker, which chooses by the
			// arm flag: staged while one is there (consuming it), committed once it is gone.
			s.write(n, s.pick(n))
		}
	}
}

// pick is the picker's choice for a carried unit, and consumes the single-use arm flag exactly
// as the real one does.
func (s *systemctlFake) pick(n string) string {
	if err := os.Remove(filepath.Join(s.run_, n+".update")); err == nil {
		return "trial"
	}
	return "pushed"
}

func (s *systemctlFake) write(n, what string) {
	_ = os.WriteFile(filepath.Join(s.run_, n+".ran"), []byte(what+"\n"), 0o644)
}

// clearMarkers wipes what the pickers left, as the commit or a fresh boot would.
func clearMarkers(t *testing.T, run string) {
	t.Helper()
	for _, n := range doorNames {
		if err := os.Remove(filepath.Join(run, n+".ran")); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
}

// THE VERDICT ([B.138]): a trial start try-restarts each door that is running and passes only
// if every one comes back active; on a secondary (nothing running) it passes at once. A door
// that fails refuses the trial -- and the trial agent restarts nothing itself: the failed
// door's own auto-restart is the revert.
func TestBinStartupTrialVerdict(t *testing.T) {
	dir, run := binDirs(t)
	stageSet(t, dir)
	if err := os.WriteFile(filepath.Join(dir, "RELEASE.next"), []byte("v3.20260908.trial000\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(run, "briard-guest-agent.ran"), []byte("trial\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Armed, as bin.activate leaves things: every name of the set, single-use. Each door's picker
	// consumes its own on the start that carries it.
	arm := func() {
		for _, n := range doorNames {
			if err := os.WriteFile(filepath.Join(run, n+".update"), nil, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	arm()
	logf := func(string, ...any) {}

	// A secondary: no door is running, the verdict is immediate, nothing is restarted.
	s := &systemctlFake{active: map[string]bool{}}
	if err := BinStartup(context.Background(), &fakeExec{runFn: s.run}, logf); err != nil {
		t.Fatalf("secondary: %v", err)
	}
	for _, r := range s.runs {
		if r[1] == "try-restart" {
			t.Errorf("a secondary's trial restarted %v", r)
		}
	}
	if got := stagedSet(); !reflect.DeepEqual(got, BinNames) {
		t.Errorf("a passing verdict discarded the set: %v", got)
	}

	// THE PRIMARY, both doors running and both taking the staged copy. ONE restart, at the
	// EARLIEST member of the chain: restarting it carries everything ordered after it in the same
	// transaction, so the dashboard is verified where it stands rather than restarted again. Two
	// restarts across an ordered chain are how a start job gets cancelled, and a cancelled start
	// job fires OnFailure= -- which demoted a node mid-upgrade when this loop ran dashboard-first
	// (install-macvtap, [B.138]). And the budget reset must precede the restart it pays for.
	clearMarkers(t, run)
	arm()
	s = &systemctlFake{run_: run, active: map[string]bool{"briard-dashboard.service": true, "briard-reverse-proxy.service": true}}
	if err := BinStartup(context.Background(), &fakeExec{runFn: s.run}, logf); err != nil {
		t.Fatalf("primary: %v", err)
	}
	var restarted []string
	reset, restart := -1, -1
	for i, r := range s.runs {
		if r[1] == "try-restart" {
			restarted = append(restarted, r[len(r)-1])
			if restart < 0 && r[len(r)-1] == "briard-reverse-proxy.service" {
				restart = i
			}
		}
		if r[1] == "reset-failed" && r[len(r)-1] == "briard-reverse-proxy.service" && reset < 0 {
			reset = i
		}
	}
	if !reflect.DeepEqual(restarted, []string{"briard-reverse-proxy.service"}) {
		t.Errorf("restarted %v, want the earliest chain member alone", restarted)
	}
	if reset < 0 || restart < 0 || reset > restart {
		t.Errorf("reset-failed at %d, try-restart at %d; the budget must be cleared before the trial spends it", reset, restart)
	}

	// The primary, the door's real launch fails: the verdict is a refusal that names it, and
	// the trial agent leaves the set alone (the committed agent's start is what cleans up).
	clearMarkers(t, run)
	arm()
	s = &systemctlFake{run_: run, active: map[string]bool{"briard-dashboard.service": true, "briard-reverse-proxy.service": true}, fail: "briard-reverse-proxy.service"}
	err := BinStartup(context.Background(), &fakeExec{runFn: s.run}, logf)
	if err == nil || !strings.Contains(err.Error(), "REFUSED") || !strings.Contains(err.Error(), "briard-reverse-proxy failed its real launch") {
		t.Fatalf("a failed door: err = %v", err)
	}
	if got := stagedSet(); !reflect.DeepEqual(got, BinNames) {
		t.Errorf("the trial agent discarded the set itself: %v", got)
	}

	// THE TRAP THE FIRST RIG RUN FOUND: the staged door fails, systemd's own auto-restart puts
	// the unit back on the committed binary, and the restart job reports SUCCESS -- active, exit
	// 0. Only the picker's marker says the door is running what it ran before, and the verdict
	// must refuse on it, or the set commits a door that has already reverted.
	clearMarkers(t, run)
	arm()
	s = &systemctlFake{run_: run, active: map[string]bool{"briard-dashboard.service": true, "briard-reverse-proxy.service": true}, reverts: "briard-reverse-proxy.service"}
	err = BinStartup(context.Background(), &fakeExec{runFn: s.run}, logf)
	if err == nil || !strings.Contains(err.Error(), "REFUSED") || !strings.Contains(err.Error(), "running the pushed binary") {
		t.Fatalf("a door that reverted under a successful restart job: err = %v", err)
	}
}

// THE HANDSHAKE MUST NOT REPORT AN OLD RELEASE ([B.138]): between the trial agent's READY and
// its commit the host reconnects and asks what the guest runs. A trial that has passed reports
// the release it is trialling; every other start reports the committed one. Reading the
// committed file first was a good dress recorded as a permanent revert.
func TestRunningBundlePrefersTheTrialledRelease(t *testing.T) {
	dir, run := binDirs(t)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// runningBundle only speaks for a process running FROM the pushed directory.
	t.Setenv("BRIARD_BIN_DIR", filepath.Dir(exe))
	dir = filepath.Dir(exe)
	t.Cleanup(func() {
		os.Remove(filepath.Join(dir, "RELEASE"))
		os.Remove(filepath.Join(dir, "RELEASE.next"))
	})
	if err := os.WriteFile(filepath.Join(dir, "RELEASE"), []byte("v3.20260901.old00000\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "RELEASE.next"), []byte("v3.20260908.new00000\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := runningBundle(); got != "v3.20260901.old00000" {
		t.Errorf("an ordinary start reports %q, want the committed release", got)
	}
	if err := os.WriteFile(filepath.Join(run, "briard-guest-agent.ran"), []byte("trial\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := runningBundle(); got != "v3.20260908.new00000" {
		t.Errorf("a passed trial reports %q, want the release it is trialling", got)
	}
}

// THE AFTERMATH ([B.138]): a non-trial start that finds a staged set discards it and puts both
// doors back on the committed files -- whichever of the three failed, because it cannot know.
// A non-trial start with nothing staged does nothing at all.
func TestBinStartupAftermathDiscardsAStaleSet(t *testing.T) {
	dir, run := binDirs(t)
	logf := func(string, ...any) {}
	s := &systemctlFake{}
	if err := BinStartup(context.Background(), &fakeExec{runFn: s.run}, logf); err != nil || len(s.runs) != 0 {
		t.Fatalf("a clean start: err %v, ran %v", err, s.runs)
	}
	stageSet(t, dir)
	for _, f := range []string{"RELEASE.next", "briard-dashboard.part"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, n := range BinNames {
		if err := os.WriteFile(filepath.Join(run, n+".update"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Both doors are running the STAGED copies the dead trial left them on, which is what the
	// aftermath has to undo.
	s = &systemctlFake{run_: run, active: map[string]bool{"briard-dashboard.service": true, "briard-reverse-proxy.service": true}}
	s.mark("briard-reverse-proxy", "trial")
	s.runs = nil
	if err := BinStartup(context.Background(), &fakeExec{runFn: s.run}, logf); err != nil {
		t.Fatalf("aftermath: %v", err)
	}
	// ONE restart, at the earliest chain member. It carries the dashboard with it, and the loop
	// stops there rather than restarting the dashboard too -- a second restart across an ordered
	// chain is what cancelled a start job and demoted a node mid-upgrade.
	want := [][]string{
		{"systemctl", "is-active", "briard-reverse-proxy.service"},
		{"systemctl", "reset-failed", "briard-reverse-proxy.service"},
		{"systemctl", "try-restart", "briard-reverse-proxy.service"},
		// The dashboard is looked at and left alone: the door's restart carried it back, so its
		// marker no longer says it is on a staged copy.
		{"systemctl", "is-active", "briard-dashboard.service"},
	}
	if !reflect.DeepEqual(s.runs, want) {
		t.Errorf("aftermath ran %v, want %v", s.runs, want)
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		t.Errorf("the staged set survived the aftermath: %v", ents)
	}
	// Nothing is armed any more, and BOTH doors are back on their committed binaries -- the
	// dashboard because the door's restart carried it.
	for _, n := range doorNames {
		if _, err := os.Stat(filepath.Join(run, n+".update")); !os.IsNotExist(err) {
			t.Errorf("%s is still armed after the aftermath", n)
		}
		if got := pickerRan(n); got != "pushed" {
			t.Errorf("%s ran %q after the aftermath, want the committed binary", n, got)
		}
	}
}
