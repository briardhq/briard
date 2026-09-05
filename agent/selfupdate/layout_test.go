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

// The bundle siblings of [B.86b]: net-wrap stages beside the agent, qemu stages as a RELATIVE
// link to a tree under Base (replacing an earlier link atomically), a link to anything else
// is refused, and the two resolve back to absolute tree paths.
func TestBundleStagesNetWrapAndAQEMULink(t *testing.T) {
	l := newLayout(t)
	if err := os.MkdirAll(l.Base, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := l.StageNextNetWrap(strings.NewReader("#!/bin/sh\nexec qemu")); err != nil {
		t.Fatalf("StageNextNetWrap: %v", err)
	}
	if fi, err := os.Stat(l.NextNetWrapPath()); err != nil || fi.Mode().Perm()&0o111 == 0 {
		t.Fatalf("net-wrap.next = %v, %v (want an executable)", fi, err)
	}
	if l.NextQEMUStaged() {
		t.Fatal("qemu.next reported staged before anything was staged")
	}
	for _, rel := range []string{"v1", "v2"} {
		if err := os.MkdirAll(l.QEMUTree(rel), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := l.StageNextQEMU(l.QEMUTree(rel)); err != nil {
			t.Fatalf("StageNextQEMU(%s): %v", rel, err)
		}
	}
	if !l.NextQEMUStaged() {
		t.Fatal("qemu.next not staged")
	}
	if target, _ := os.Readlink(l.NextQEMUPath()); target != "qemu-v2" {
		t.Errorf("qemu.next -> %q, want the RELATIVE qemu-v2 (replaced v1)", target)
	}
	if tree, ok := l.NextQEMUTree(); !ok || tree != l.QEMUTree("v2") {
		t.Errorf("NextQEMUTree = %q, %v", tree, ok)
	}
	if _, ok := l.CommittedQEMUTree(); ok {
		t.Error("a committed qemu tree was reported with no committed link")
	}
	// Refused: a tree outside Base, a directory not named qemu-*, a missing directory.
	for _, bad := range []string{filepath.Join(t.TempDir(), "qemu-x"), filepath.Join(l.Base, "other"), l.QEMUTree("missing")} {
		os.MkdirAll(filepath.Join(l.Base, "other"), 0o755)
		if err := l.StageNextQEMU(bad); err == nil {
			t.Errorf("StageNextQEMU(%s) accepted", bad)
		}
	}
	if target, _ := os.Readlink(l.NextQEMUPath()); target != "qemu-v2" {
		t.Errorf("a refused stage moved qemu.next to %q", target)
	}
	ents, _ := os.ReadDir(l.Base)
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("temp %s left behind", e.Name())
		}
	}
	// Discard clears both staged siblings and is idempotent; the trees stay.
	if err := l.DiscardNextBundle(); err != nil {
		t.Fatal(err)
	}
	if err := l.DiscardNextBundle(); err != nil {
		t.Fatal(err)
	}
	if l.NextQEMUStaged() {
		t.Error("qemu.next survived DiscardNextBundle")
	}
	if _, err := os.Lstat(l.NextNetWrapPath()); !os.IsNotExist(err) {
		t.Error("net-wrap.next survived DiscardNextBundle")
	}
	if _, err := os.Stat(l.QEMUTree("v2")); err != nil {
		t.Error("DiscardNextBundle removed a tree; it must only drop the link")
	}
}

// Prune keeps exactly the trees the two links point at (committed and staged) and removes
// the rest, and leaves everything that is not a qemu-* directory alone.
func TestPruneQEMUTreesKeepsCommittedAndStaged(t *testing.T) {
	l := newLayout(t)
	for _, rel := range []string{"old", "cur", "next", "orphan"} {
		if err := os.MkdirAll(filepath.Join(l.QEMUTree(rel), "bin"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	os.WriteFile(filepath.Join(l.Base, "qemu-notadir"), nil, 0o644)
	os.MkdirAll(filepath.Join(l.Base, "services"), 0o755)
	if err := os.Symlink("qemu-cur", l.QEMUPath()); err != nil { // as briard-commit leaves it
		t.Fatal(err)
	}
	if err := l.StageNextQEMU(l.QEMUTree("next")); err != nil {
		t.Fatal(err)
	}
	if tree, ok := l.CommittedQEMUTree(); !ok || tree != l.QEMUTree("cur") {
		t.Fatalf("CommittedQEMUTree = %q, %v", tree, ok)
	}
	removed, err := l.PruneQEMUTrees()
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 2 || removed[0] != "qemu-old" || removed[1] != "qemu-orphan" {
		t.Errorf("removed %v, want [qemu-old qemu-orphan]", removed)
	}
	for _, keep := range []string{l.QEMUTree("cur"), l.QEMUTree("next"), filepath.Join(l.Base, "qemu-notadir"), filepath.Join(l.Base, "services")} {
		if _, err := os.Stat(keep); err != nil {
			t.Errorf("%s was pruned", keep)
		}
	}
	// A second pass finds nothing.
	if removed, _ := l.PruneQEMUTrees(); len(removed) != 0 {
		t.Errorf("second prune removed %v", removed)
	}
}

// The trial's failure message: written by the candidate, taken (read + unlinked) once by the
// agent that comes back, and gone thereafter.
func TestFailureMessageIsTakenOnce(t *testing.T) {
	l := newLayout(t)
	if _, _, ok := l.TakeFailure(); ok {
		t.Fatal("a failure was reported with none written")
	}
	if err := l.WriteFailure("v3.20260906.aaaaaaa", "qemu smoke test: -version: exit status 1\nsecond line"); err != nil {
		t.Fatal(err)
	}
	rel, reason, ok := l.TakeFailure()
	if !ok || rel != "v3.20260906.aaaaaaa" || reason != "qemu smoke test: -version: exit status 1 second line" {
		t.Errorf("TakeFailure = %q, %q, %v", rel, reason, ok)
	}
	if _, _, ok := l.TakeFailure(); ok {
		t.Error("the failure message survived being taken")
	}
	if l.InTrial() {
		t.Error("InTrial with no trial marker")
	}
	os.MkdirAll(l.RunDir, 0o755)
	os.WriteFile(l.TrialMarkerPath(), nil, 0o644)
	if !l.InTrial() {
		t.Error("InTrial false with the marker present")
	}
}
