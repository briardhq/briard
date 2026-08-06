package selfupdate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newLayout(t *testing.T) Layout {
	t.Helper()
	root := t.TempDir()
	return New(filepath.Join(root, "state"), filepath.Join(root, "run"))
}

func TestStageNextWritesExecutableCandidateAtomically(t *testing.T) {
	l := newLayout(t)
	if err := os.MkdirAll(l.Base, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := l.StageNext(strings.NewReader("NEWBINARY")); err != nil {
		t.Fatalf("StageNext: %v", err)
	}
	b, err := os.ReadFile(l.NextPath())
	if err != nil {
		t.Fatalf("read agent.next: %v", err)
	}
	if string(b) != "NEWBINARY" {
		t.Errorf("agent.next = %q, want NEWBINARY", b)
	}
	fi, _ := os.Stat(l.NextPath())
	if fi.Mode().Perm()&0o111 == 0 {
		t.Errorf("agent.next not executable: %v", fi.Mode())
	}
	if _, err := os.Stat(l.NextPath() + ".tmp"); !os.IsNotExist(err) {
		t.Errorf(".tmp left behind after StageNext")
	}
}

// The load-bearing property: staging does NOT touch the committed binary. A power loss (or a
// failed trial) after staging leaves systemd running the committed agent — .next is inert.
func TestStageNextLeavesCommittedBinaryUntouched(t *testing.T) {
	l := newLayout(t)
	if err := os.MkdirAll(l.Base, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(l.AgentPath(), []byte("COMMITTED"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := l.StageNext(strings.NewReader("CANDIDATE")); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(l.AgentPath())
	if string(b) != "COMMITTED" {
		t.Errorf("committed agent changed on a mere stage: got %q — power-loss safety broken", b)
	}
	// And agent.next is on the SAME directory (so the wrapper's commit is an atomic same-fs rename).
	if filepath.Dir(l.NextPath()) != filepath.Dir(l.AgentPath()) {
		t.Errorf("agent.next (%s) not colocated with agent (%s) — commit rename would cross fs",
			l.NextPath(), l.AgentPath())
	}
}

func TestStageNextIsIdempotentOverwrite(t *testing.T) {
	l := newLayout(t)
	os.MkdirAll(l.Base, 0o755)
	if err := l.StageNext(strings.NewReader("A")); err != nil {
		t.Fatal(err)
	}
	if err := l.StageNext(strings.NewReader("B")); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(l.NextPath())
	if string(b) != "B" {
		t.Errorf("re-stage did not overwrite: got %q", b)
	}
}

func TestArmRequiresStagedCandidate(t *testing.T) {
	l := newLayout(t)
	os.MkdirAll(l.Base, 0o755)
	if err := l.Arm(); err == nil {
		t.Error("Arm without a staged agent.next must fail (else it trials a missing binary)")
	}
	if l.Armed() {
		t.Error("Armed() true after a failed Arm")
	}
}

func TestArmSetsUpdateFlag(t *testing.T) {
	l := newLayout(t)
	os.MkdirAll(l.Base, 0o755)
	if err := l.StageNext(strings.NewReader("X")); err != nil {
		t.Fatal(err)
	}
	if l.Armed() {
		t.Error("Armed() true before Arm")
	}
	if err := l.Arm(); err != nil {
		t.Fatalf("Arm: %v", err)
	}
	if !l.Armed() {
		t.Error("Armed() false after Arm")
	}
	// The flag lives in the tmpfs run dir (cleared on reboot -> power-loss revert for free).
	if filepath.Dir(l.UpdateFlagPath()) != l.RunDir {
		t.Errorf("update flag %s not under RunDir %s", l.UpdateFlagPath(), l.RunDir)
	}
}

// The tmpfs flags and the on-disk binaries live on separate trees — the invariant behind
// power-loss safety (decisions ephemeral, binaries durable).
func TestFlagsAreSeparateFromBinaries(t *testing.T) {
	l := newLayout(t)
	if strings.HasPrefix(l.UpdateFlagPath(), l.Base) || strings.HasPrefix(l.TrialMarkerPath(), l.Base) {
		t.Errorf("trial flags must not live under the state base (they must be ephemeral tmpfs)")
	}
}
