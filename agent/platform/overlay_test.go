package platform

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// qemuImgOrSkip finds a real qemu-img. These tests drive the actual tool rather than a fake,
// because what is being proven is how qemu-img reports a backing chain -- a fake would just be
// this file's own assumptions about that, restated and then asserted.
func qemuImgOrSkip(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("qemu-img")
	if err != nil {
		t.Skip("qemu-img not on PATH")
	}
	return p
}

// specFor builds a QEMUSpec whose qemuImg() resolves to the real binary (it is derived from
// Binary's directory), pointing at disk.
func specFor(t *testing.T, disk string) QEMUSpec {
	t.Helper()
	img := qemuImgOrSkip(t)
	return QEMUSpec{
		DiskImage: disk,
		// qemuImg() takes the directory of Binary; qemu-img lives beside qemu-system-*.
		Binary: filepath.Join(filepath.Dir(img), "qemu-system-x86_64"),
		Unit:   "briard-guest-overlay-test.service", // never started; Running() reports inactive
	}
}

func makeBase(t *testing.T, dir string) string {
	t.Helper()
	base := filepath.Join(dir, "nixos.qcow2")
	out, err := exec.Command(qemuImgOrSkip(t), "create", "-f", "qcow2", base, "64M").CombinedOutput()
	if err != nil {
		t.Fatalf("create base: %v: %s", err, out)
	}
	return base
}

func makeOverlay(t *testing.T, dir, base string) string {
	t.Helper()
	ov := filepath.Join(dir, "guest.qcow2")
	out, err := exec.Command(qemuImgOrSkip(t), "create", "-f", "qcow2", "-b", base, "-F", "qcow2", ov).CombinedOutput()
	if err != nil {
		t.Fatalf("create overlay: %v: %s", err, out)
	}
	return ov
}

// The shipped shape: an overlay reports the image beneath it.
func TestBackingFileReadsTheOverlaysBase(t *testing.T) {
	dir := t.TempDir()
	base := makeBase(t, dir)
	spec := specFor(t, makeOverlay(t, dir, base))

	got, err := spec.BackingFile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got != base {
		t.Errorf("BackingFile() = %q, want %q", got, base)
	}
}

// THE SAFETY GATE, and the assertion this whole verb rests on. A standalone disk is not an
// overlay: it is the only copy of itself, and rebuilding it would destroy the guest with nothing
// to restore from. BackingFile must report "" and RebuildOverlay must REFUSE -- and, critically,
// must refuse without having deleted anything first.
func TestRebuildRefusesADiskWithNoBackingImage(t *testing.T) {
	dir := t.TempDir()
	standalone := makeBase(t, dir) // a plain qcow2: no backing file
	spec := specFor(t, standalone)

	if got, err := spec.BackingFile(context.Background()); err != nil || got != "" {
		t.Fatalf("BackingFile() on a standalone disk = (%q, %v), want (\"\", nil)", got, err)
	}
	_, err := spec.RebuildOverlay(context.Background())
	if err == nil {
		t.Fatal("RebuildOverlay on a standalone disk returned nil -- it would have destroyed the only copy")
	}
	if !strings.Contains(err.Error(), "only copy") {
		t.Errorf("refusal = %q, want it to say why (the only copy)", err)
	}
	// The disk is STILL THERE. A refusal that had already removed the file would be the exact
	// disaster the check exists to prevent, and every other assertion here would still pass.
	if _, statErr := os.Stat(standalone); statErr != nil {
		t.Fatalf("the disk was removed despite the refusal: %v", statErr)
	}
}

// A rebuild discards what the overlay held and comes back on the same base. Proven by writing a
// recognisable byte into the overlay and showing the rebuilt one no longer has it, rather than by
// comparing file sizes -- a fresh overlay and a written-to one can be the same size.
func TestRebuildDiscardsTheOverlayAndKeepsTheBase(t *testing.T) {
	dir := t.TempDir()
	base := makeBase(t, dir)
	ov := makeOverlay(t, dir, base)
	spec := specFor(t, ov)

	before, err := os.Stat(ov)
	if err != nil {
		t.Fatal(err)
	}
	// Dirty the overlay so "it was replaced" is observable.
	if out, err := exec.Command(qemuImgOrSkip(t), "snapshot", "-c", "dirty", ov).CombinedOutput(); err != nil {
		t.Fatalf("dirty the overlay: %v: %s", err, out)
	}
	list, _ := exec.Command(qemuImgOrSkip(t), "snapshot", "-l", ov).CombinedOutput()
	if !strings.Contains(string(list), "dirty") {
		t.Fatalf("could not dirty the overlay; the test would prove nothing. got: %s", list)
	}

	got, err := spec.RebuildOverlay(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got != base {
		t.Errorf("RebuildOverlay reported base %q, want %q", got, base)
	}
	// A NEW overlay: the marker is gone.
	list, _ = exec.Command(qemuImgOrSkip(t), "snapshot", "-l", ov).CombinedOutput()
	if strings.Contains(string(list), "dirty") {
		t.Error("the rebuilt overlay still carries the old snapshot -- it was not replaced")
	}
	// Still an overlay on the SAME base, so the node boots what it was installed with.
	if b, err := spec.BackingFile(context.Background()); err != nil || b != base {
		t.Errorf("rebuilt overlay's backing = (%q, %v), want (%q, nil)", b, err, base)
	}
	// And the base itself was not touched.
	if after, err := os.Stat(base); err != nil {
		t.Errorf("the backing image is gone: %v", err)
	} else if after.Size() == 0 {
		t.Error("the backing image was truncated")
	}
	_ = before
}

// An unreadable backing image is a refusal too, and again before anything is discarded: rebuilding
// onto an image that is not there produces a node with no disk at all.
func TestRebuildRefusesWhenTheBaseIsMissing(t *testing.T) {
	dir := t.TempDir()
	base := makeBase(t, dir)
	ov := makeOverlay(t, dir, base)
	spec := specFor(t, ov)

	if err := os.Remove(base); err != nil {
		t.Fatal(err)
	}
	if _, err := spec.RebuildOverlay(context.Background()); err == nil {
		t.Fatal("RebuildOverlay with a missing base returned nil")
	}
	if _, err := os.Stat(ov); err != nil {
		t.Fatalf("the overlay was discarded even though the base was missing: %v", err)
	}
}

// The backing-file query runs against a LIVE guest, whose disk QEMU holds a write lock on, so it
// must declare shared access or qemu-img declines the image. Without the flag the check fails on
// every running node -- every node a rescue is ever asked about -- and the verb refuses with
// "exit status 1" instead of doing anything.
//
// Asserted on the argv rather than against a real locked image: taking a QEMU-compatible lock from
// a test needs a running VM, which is what the nixosTest is for. This is the cheap sentinel for the
// specific regression, since the tests above all inspect STOPPED images, and a stopped image is
// not locked -- which is exactly how this shipped and cost a runner cycle to find.
func TestInfoArgsDeclareSharedAccess(t *testing.T) {
	args := infoArgs("/tmp/guest.qcow2")
	var shared bool
	for _, a := range args {
		if a == "--force-share" {
			shared = true
		}
	}
	if !shared {
		t.Errorf("infoArgs = %v, missing --force-share: reading the backing file of a RUNNING "+
			"guest fails without it, and that is the only time it is read", args)
	}
	if args[len(args)-1] != "/tmp/guest.qcow2" {
		t.Errorf("infoArgs = %v, want the disk last", args)
	}
}
