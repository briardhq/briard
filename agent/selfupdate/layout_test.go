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

// seed lays a committed release (agent, manifest, net-wrap, a qemu link) and stages a candidate
// for each, the way an install and then a run leave them.
func seed(t *testing.T, l Layout) {
	t.Helper()
	if err := os.MkdirAll(l.Base, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"v1", "v2"} {
		if err := os.MkdirAll(l.QEMUTree(rel), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for name, body := range map[string]string{"briard-agent": "agent v1", "manifest.json": "m v1", "briard-net-wrap": "wrap v1"} {
		if err := os.WriteFile(filepath.Join(l.Base, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("qemu-v1", l.QEMUPath()); err != nil {
		t.Fatal(err)
	}
	if err := l.StageNext(strings.NewReader("agent v2")); err != nil {
		t.Fatal(err)
	}
	if err := l.StageNextManifest([]byte("m v2")); err != nil {
		t.Fatal(err)
	}
	if err := l.StageNextNetWrap(strings.NewReader("wrap v2")); err != nil {
		t.Fatal(err)
	}
	if err := l.StageNextQEMU(l.QEMUTree("v2")); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, p string) string {
	t.Helper()
	if fi, err := os.Lstat(p); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		s, _ := os.Readlink(p)
		return s
	}
	b, _ := os.ReadFile(p)
	return string(b)
}

// The swap puts every staged entry under its committed name and keeps the displaced one as
// .prev; the agent's .prev is the trial marker, and a commit removes every .prev.
func TestSwapThenCommit(t *testing.T) {
	l := newLayout(t)
	seed(t, l)
	if l.Interrupted() {
		t.Fatal("Interrupted before any swap")
	}
	moved, err := l.Swap()
	if err != nil {
		t.Fatalf("Swap: %v", err)
	}
	if strings.Join(moved, " ") != "briard-agent manifest.json briard-net-wrap qemu" {
		t.Errorf("moved %v", moved)
	}
	if read(t, l.AgentPath()) != "agent v2" || read(t, l.QEMUPath()) != "qemu-v2" || read(t, l.NetWrapPath()) != "wrap v2" {
		t.Error("the candidate is not in place after the swap")
	}
	if read(t, l.PrevPath()) != "agent v1" || read(t, l.QEMUPath()+".prev") != "qemu-v1" {
		t.Error("the displaced entries were not kept as .prev")
	}
	if !l.Interrupted() {
		t.Error("a swap without a verdict is not reported Interrupted")
	}
	if err := l.Commit(); err != nil {
		t.Fatal(err)
	}
	ents, _ := os.ReadDir(l.Base)
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".prev") || strings.HasSuffix(e.Name(), ".next") {
			t.Errorf("%s survived the commit", e.Name())
		}
	}
	if l.Interrupted() {
		t.Error("Interrupted after the commit")
	}
	if _, err := os.Stat(l.QEMUTree("v1")); err != nil {
		t.Error("the commit removed the displaced tree; only its link goes")
	}
}

// A revert puts every displaced entry back over the candidate, and the node is exactly the
// committed release again.
func TestSwapThenRevert(t *testing.T) {
	l := newLayout(t)
	seed(t, l)
	if _, err := l.Swap(); err != nil {
		t.Fatal(err)
	}
	back, err := l.Revert()
	if err != nil {
		t.Fatalf("Revert: %v", err)
	}
	if back[len(back)-1] != "briard-agent" {
		t.Errorf("the agent was not put back last: %v", back)
	}
	if read(t, l.AgentPath()) != "agent v1" || read(t, l.ManifestPath()) != "m v1" ||
		read(t, l.NetWrapPath()) != "wrap v1" || read(t, l.QEMUPath()) != "qemu-v1" {
		t.Error("the revert did not restore the committed release")
	}
	if l.Interrupted() {
		t.Error("Interrupted after a revert")
	}
}

// A swap without a staged agent is refused: the agent's .prev is the only trial marker, and a
// swap that moved anything without it could never be found and reverted.
func TestSwapRefusesWithoutAStagedAgent(t *testing.T) {
	l := newLayout(t)
	seed(t, l)
	os.Remove(l.NextPath())
	if _, err := l.Swap(); err == nil {
		t.Fatal("swapped with no staged agent")
	}
	if read(t, l.QEMUPath()) != "qemu-v1" {
		t.Error("a refused swap moved qemu")
	}
}

// A commit cut short after the agent's .prev went is not read as Interrupted, and a second
// Commit finishes it: reverting the remaining entries would pair the new agent with old parts.
func TestACommitCutShortIsFinishedNotReverted(t *testing.T) {
	l := newLayout(t)
	seed(t, l)
	if _, err := l.Swap(); err != nil {
		t.Fatal(err)
	}
	os.Remove(l.PrevPath()) // the commit got this far
	if l.Interrupted() {
		t.Fatal("a commit cut short reads as an interrupted swap")
	}
	if err := l.Commit(); err != nil {
		t.Fatal(err)
	}
	if read(t, l.QEMUPath()) != "qemu-v2" {
		t.Error("finishing the commit moved qemu back")
	}
	if _, err := os.Lstat(l.QEMUPath() + ".prev"); !os.IsNotExist(err) {
		t.Error("qemu.prev survived the finished commit")
	}
}

// The run's verdict is read, never consumed: the CLI and the alerter both read it.
func TestResultIsReadNotTaken(t *testing.T) {
	l := newLayout(t)
	if _, ok := l.ReadResult(); ok {
		t.Fatal("a result with none written")
	}
	os.MkdirAll(l.RunDir, 0o755)
	os.WriteFile(l.ResultPath(), []byte("failed could not fetch a bootstrap agent\n"), 0o644)
	for range 2 {
		r, ok := l.ReadResult()
		if !ok || r.OK || r.Line != "could not fetch a bootstrap agent" || r.At.IsZero() {
			t.Errorf("ReadResult = %+v, %v", r, ok)
		}
	}
	os.WriteFile(l.ResultPath(), []byte("ok already at v3.20260906.aaaaaaa\n"), 0o644)
	if r, _ := l.ReadResult(); !r.OK || r.Line != "already at v3.20260906.aaaaaaa" {
		t.Errorf("ReadResult = %+v", r)
	}
}

// The bundle siblings: net-wrap stages beside the agent, qemu stages as a RELATIVE
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
	if _, staged := l.NextQEMUTree(); staged {
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
	if _, staged := l.NextQEMUTree(); !staged {
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
	if _, staged := l.NextQEMUTree(); staged {
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
	if err := os.Symlink("qemu-cur", l.QEMUPath()); err != nil { // as a swap leaves it
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

// The good guest mark is a release id in a plain file: written over whatever was there (the link
// an older agent left included), refused for a tree that is not on disk, and its tree survives the
// pruner beside the committed one.
func TestTheGoodGuestIsAnIdThePrunerKeeps(t *testing.T) {
	l := newLayout(t)
	for _, rel := range []string{"good", "cur", "old"} {
		if err := os.MkdirAll(l.GuestTree(rel), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// The link an older agent left, made the way it made it (a file link on Windows).
	if err := l.stageTreeLink(l.GuestTree("old"), guestTree, l.GoodGuestPath(), goodGuest); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("guest-cur", l.GuestPath()); err != nil {
		t.Fatal(err)
	}
	if err := l.MarkGuestGood("missing"); err == nil {
		t.Error("a good mark for a tree that is not there was accepted")
	}
	if err := l.MarkGuestGood("good"); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Lstat(l.GoodGuestPath()); err != nil || !fi.Mode().IsRegular() {
		t.Fatalf("guest.good is not a plain file: %v %v", fi, err)
	}
	if got := l.GoodGuestRelease(); got != "good" {
		t.Fatalf("GoodGuestRelease = %q", got)
	}
	removed, err := l.PruneGuestTrees()
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 || removed[0] != "guest-old" {
		t.Errorf("removed %v, want [guest-old]", removed)
	}
}

// A reverted run's failure message: written by the updater, taken (read + unlinked) once by the
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
}
