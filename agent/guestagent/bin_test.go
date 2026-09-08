package guestagent

import (
	"bytes"
	"context"
	"crypto/rand"
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

// A binary larger than the frame cap streams in chunks and lands as <name>.next, verified
// against the digest the LAST chunk carries, executable.
func TestBinStageStreamsAndVerifies(t *testing.T) {
	dir, _ := binDirs(t)
	want := make([]byte, 9<<20+123) // three chunks, the last one partial
	if _, err := rand.Read(want); err != nil {
		t.Fatal(err)
	}
	g := dial(t, &fakeExec{})
	if err := g.BinStage(context.Background(), "briard-guest-agent", bytes.NewReader(want)); err != nil {
		t.Fatalf("BinStage: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "briard-guest-agent.next"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("staged bytes differ (%d vs %d)", len(got), len(want))
	}
	if fi, _ := os.Stat(filepath.Join(dir, "briard-guest-agent.next")); fi.Mode().Perm()&0o111 == 0 {
		t.Error("the staged binary is not executable")
	}
	if _, err := os.Stat(filepath.Join(dir, "briard-guest-agent.part")); !os.IsNotExist(err) {
		t.Error("the .part file was left behind")
	}
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
	if err := g.BinActivate(context.Background(), "v3.20260907.abc1234", BinNames); err == nil {
		t.Fatal("activated with nothing staged")
	}
	if len(fx.runs) != 0 {
		t.Fatalf("restarted units for an unstaged set: %v", fx.runs)
	}
	stageSet(t, dir)
	if err := g.BinActivate(context.Background(), "v3.20260907.abc1234", []string{"briard-dashboard", "briard-reverse-proxy"}); err == nil {
		t.Fatal("a set without the guest agent was activated -- nothing would trial it")
	}
	if len(fx.runs) != 0 {
		t.Fatalf("restarted units for an agent-less set: %v", fx.runs)
	}
	if err := g.BinActivate(context.Background(), "v3.20260907.abc1234", BinNames); err != nil {
		t.Fatalf("BinActivate: %v", err)
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
	if err := g.BinActivate(context.Background(), "bad id/", BinNames); err == nil {
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
	if err := g.BinTest(context.Background(), BinNames); err == nil {
		t.Fatal("tested with nothing staged")
	}
	stageSet(t, dir)
	if err := g.BinTest(context.Background(), BinNames); err != nil {
		t.Fatalf("BinTest: %v", err)
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
	err := g.BinTest(context.Background(), BinNames)
	if err == nil || !strings.Contains(err.Error(), "briard-reverse-proxy failed its test launch") || !strings.Contains(err.Error(), "no such device") {
		t.Fatalf("BinTest error = %v; want the door named, with its output", err)
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
		s.mark(bin, "trial")
	}
	return nil, nil
}

func (s *systemctlFake) mark(bin, what string) {
	if s.run_ != "" {
		_ = os.WriteFile(filepath.Join(s.run_, bin+".ran"), []byte(what+"\n"), 0o644)
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
	if err := os.WriteFile(filepath.Join(run, "briard-guest-agent.trial"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
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

	// A FULL START BUDGET BEFORE EACH DOOR IS TRIALLED, and this is the whole of "a failed
	// upgrade never demotes": the hook fires on an exhausted start limit, never on one failed
	// start, and a trial costs up to three of the five. `reset-failed` must therefore come BEFORE
	// the restart of that same door, not after and not for some other unit.
	s = &systemctlFake{run_: run, active: map[string]bool{"briard-dashboard.service": true, "briard-reverse-proxy.service": true}}
	if err := BinStartup(context.Background(), &fakeExec{runFn: s.run}, logf); err != nil {
		t.Fatalf("primary: %v", err)
	}
	for _, unit := range []string{"briard-dashboard.service", "briard-reverse-proxy.service"} {
		reset, restart := -1, -1
		for i, r := range s.runs {
			if len(r) < 3 || r[len(r)-1] != unit {
				continue
			}
			if r[1] == "reset-failed" && reset < 0 {
				reset = i
			}
			if r[1] == "try-restart" && restart < 0 {
				restart = i
			}
		}
		if reset < 0 || restart < 0 || reset > restart {
			t.Errorf("%s: reset-failed at %d, try-restart at %d; the budget must be cleared before the trial spends it", unit, reset, restart)
		}
	}

	// The primary, both doors running and both taking the staged copy: verdict pass, each door
	// restarted once, in set order.
	s = &systemctlFake{run_: run, active: map[string]bool{"briard-dashboard.service": true, "briard-reverse-proxy.service": true}}
	if err := BinStartup(context.Background(), &fakeExec{runFn: s.run}, logf); err != nil {
		t.Fatalf("primary: %v", err)
	}
	var restarted []string
	for _, r := range s.runs {
		if r[1] == "try-restart" {
			restarted = append(restarted, r[len(r)-1])
		}
	}
	if !reflect.DeepEqual(restarted, []string{"briard-dashboard.service", "briard-reverse-proxy.service"}) {
		t.Errorf("restarted %v, want both doors in set order", restarted)
	}

	// The primary, the door's real launch fails: the verdict is a refusal that names it, and
	// the trial agent leaves the set alone (the committed agent's start is what cleans up).
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
	if err := os.WriteFile(filepath.Join(run, "briard-guest-agent.trial"), nil, 0o644); err != nil {
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
	if err := BinStartup(context.Background(), &fakeExec{runFn: s.run}, logf); err != nil {
		t.Fatalf("aftermath: %v", err)
	}
	want := [][]string{
		{"systemctl", "reset-failed", "briard-dashboard.service"},
		{"systemctl", "try-restart", "--no-block", "briard-dashboard.service"},
		{"systemctl", "reset-failed", "briard-reverse-proxy.service"},
		{"systemctl", "try-restart", "--no-block", "briard-reverse-proxy.service"},
	}
	if !reflect.DeepEqual(s.runs, want) {
		t.Errorf("aftermath ran %v, want %v", s.runs, want)
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		t.Errorf("the staged set survived the aftermath: %v", ents)
	}
	if ents, _ := os.ReadDir(run); len(ents) != 0 {
		t.Errorf("flags survived the aftermath: %v", ents)
	}
}

// The handshake advertises both verbs, so a host can tell a dressable guest from a firmware
// that predates the protocol.
func TestHandshakeAdvertisesBinPush(t *testing.T) {
	g := dial(t, &fakeExec{})
	if _, err := g.Handshake(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !g.SupportsBinPush() {
		t.Error("bin.stage/bin.activate are not advertised")
	}
}
