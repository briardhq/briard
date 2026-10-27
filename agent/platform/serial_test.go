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
// launch.

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

// THE LAST WORDS. The ladder decides on silence and then reads the logs to say why; what it
// reads has to be the lines that carry the cause -- the end of the file, speaking lines only, in
// order, with a console's carriage returns and escapes dropped and a runaway line cut -- and it
// has to cost one small read however large the capture has grown.
func TestLastLinesKeepsTheSpeakingTail(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "console.log")
	long := make([]byte, 400)
	for i := range long {
		long[i] = 'x'
	}
	// Twice the read window of padding ahead of the tail, so a read of the whole file and a
	// read of its end give different answers only if the implementation got the window wrong.
	pad := make([]byte, 40<<10)
	for i := range pad {
		pad[i] = 'p'
		if i%100 == 99 {
			pad[i] = '\n'
		}
	}
	body := string(pad) + "\n[    3.2] booting\r\n\x1b[31m[    4.0] Kernel panic - not syncing: Attempted to kill init!\x1b[0m\r\n\n" +
		string(long) + "\n[    4.1] ---[ end Kernel panic ]---\n\n\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	got := tailFile(path, 3)
	want := "[    4.0] Kernel panic - not syncing: Attempted to kill init! | " + string(long[:lastLineWidth]) + "… | [    4.1] ---[ end Kernel panic ]---"
	if got != want {
		t.Errorf("tailFile:\n got %q\nwant %q", got, want)
	}
	if tailFile(filepath.Join(dir, "absent.log"), 3) != "" {
		t.Error("a log that is not there must contribute nothing, not an error")
	}
	if lastLines("\n\n  \r\n", 3) != "" {
		t.Error("a log of blanks has no last words")
	}
}
