package platform

import (
	"os"
	"path/filepath"
	"testing"
)

// prepareSerialLog is a disk-fill guard and a permission guard on a file qemu opens, and both
// halves are things a node only finds out about months later -- so they are asserted here rather
// than left to the one rig that boots a guest.
//
// Its whole contract is: bound the capture, own the mode, and NEVER be a reason a guest does not
// launch ([B.157]).

// A capture past the cap rolls to .prev, and the file the next boot writes to starts empty. One
// generation, so the pair costs at most 2x the cap.
func TestPrepareSerialLogRollsPastTheCap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "console.log")
	if err := os.WriteFile(path, make([]byte, serialLogMax+1), 0o640); err != nil {
		t.Fatal(err)
	}
	prepareSerialLog(path)

	prev, err := os.Stat(path + ".prev")
	if err != nil {
		t.Fatalf("the oversized capture was not rolled to .prev: %v", err)
	}
	if prev.Size() != serialLogMax+1 {
		t.Errorf(".prev is %d bytes, want the %d the rolled file had", prev.Size(), serialLogMax+1)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("no capture was created for the next boot: %v", err)
	}
	if fi.Size() != 0 {
		t.Errorf("the new capture starts at %d bytes, want 0", fi.Size())
	}
}

// ⚠️ A capture UNDER the cap is left alone, and this is the assertion that keeps the guard from
// eating the evidence: rolling on every launch would discard the boot before the one that failed,
// which is the exact loss appending exists to prevent (serialArgs).
func TestPrepareSerialLogLeavesASmallCaptureAlone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "console.log")
	body := []byte("[    0.000000] Linux version 6.12\n")
	if err := os.WriteFile(path, body, 0o640); err != nil {
		t.Fatal(err)
	}
	prepareSerialLog(path)

	if _, err := os.Stat(path + ".prev"); !os.IsNotExist(err) {
		t.Errorf("a capture under the cap was rolled anyway (.prev exists)")
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != string(body) {
		t.Errorf("the capture was disturbed: %q (%v)", got, err)
	}
}

// The mode is OURS, on every launch. A guest console carries the household's hostnames and
// addresses: not secret, not public. Asserted on a file that already exists with a looser mode,
// because that is the case a one-shot pre-create at install time would never reach -- an install
// that predates this, or anything else that touched the file.
func TestPrepareSerialLogTightensAnExistingMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "console.log")
	if err := os.WriteFile(path, []byte("boot\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	prepareSerialLog(path)

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o640 {
		t.Errorf("mode = %04o, want 0640 -- the console is world-readable", perm)
	}
}

// A node that captures no console (BRIARD_CONSOLE=) gets nothing done to it, and in particular no
// file appears at the default path by accident.
func TestPrepareSerialLogWithNoCaptureCreatesNothing(t *testing.T) {
	dir := t.TempDir()
	prepareSerialLog("")
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 0 {
		t.Errorf("an empty path created %d entries", len(ents))
	}
}

// ⚠️ NEVER A REASON A GUEST DOES NOT LAUNCH. A host with a read-only or absent /var/log is a host
// whose logging convenience fails; it is not a host that should refuse to run its service. The
// shell this replaced learned that the hard way (a POSIX special builtin made a redirection
// failure fatal to the whole script), so the property is pinned rather than assumed.
func TestPrepareSerialLogSurvivesAnUnwritablePath(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ro")
	if err := os.Mkdir(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	// Returns at all = did not panic, did not exit, has no error to hand its caller.
	prepareSerialLog(filepath.Join(dir, "console.log"))
}
