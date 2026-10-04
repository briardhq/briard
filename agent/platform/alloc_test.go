package platform

import (
	"os"
	"path/filepath"
	"testing"
)

// The data volume is the one file in this product whose contents cannot be rebuilt from anywhere,
// so the allocator's two dangerous properties are asserted directly: it reserves what it says, and
// it never writes over something that is already there.

// Thick means the space is RESERVED, not merely declared. A sparse file passes a size check and
// fails months later as ENOSPC under a replicated filesystem, so the size check is not the
// assertion -- the blocks on disk are.
func TestAllocateThickReservesTheSpace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data.img")
	const size = 8 << 20 // 8 MiB: enough to be several blocks, small enough to be instant
	if err := AllocateThick(path, size); err != nil {
		t.Fatalf("AllocateThick: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() != size {
		t.Errorf("size = %d, want %d", fi.Size(), size)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode = %04o, want 0600 -- the household's service data is root's business", perm)
	}
	if blocks := allocatedBytes(t, path); blocks < size {
		t.Errorf("only %d bytes are actually allocated of %d -- the volume is SPARSE, which is the "+
			"state this function exists to prevent", blocks, size)
	}
}

// ⚠️ THE DESTRUCTIVE CASE. A data volume that is already there is never written over --
// and the guard has to hold for "present but unstat-able" too, which is why the creation itself is
// the proof of absence rather than a check before it. Here the file is present and readable, which
// is the case a stat WOULD catch; the O_EXCL open is what makes the unreadable one safe as well.
func TestAllocateThickNeverOverwrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data.img")
	body := []byte("a household's service data")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := AllocateThick(path, 8<<20); err != nil {
		t.Fatalf("AllocateThick over an existing volume: %v, want it to leave the disk alone", err)
	}
	// Compared by SIZE first. An overwrite makes this file the whole allocation, and a %q of eight
	// million zero bytes is a failure message nobody can read.
	fi, err := os.Stat(path)
	if err != nil || fi.Size() != int64(len(body)) {
		t.Fatalf("the existing data volume was rewritten: %d bytes now, want its original %d (%v)",
			fi.Size(), len(body), err)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != string(body) {
		t.Fatalf("the existing data volume's contents changed: %q (%v)", got, err)
	}
}

// Growing is the one change a disk that exists may undergo, and it carries both properties above:
// the new range is RESERVED (blocks, not a size), and what was already in the file is untouched.
func TestExtendThickReservesTheNewRangeAndKeepsTheOld(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.img")
	const before, after = 4 << 20, 12 << 20
	if err := AllocateThick(path, before); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte("formatted, with the guest's storage in it")
	if _, err := f.WriteAt(body, 0); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if err := ExtendThick(path, after); err != nil {
		t.Fatalf("ExtendThick: %v", err)
	}
	if fi, _ := os.Stat(path); fi.Size() != after {
		t.Errorf("size = %d, want %d", fi.Size(), after)
	}
	if got := allocatedBytes(t, path); got < after {
		t.Errorf("only %d of %d bytes are allocated -- the grown range is sparse, so the host was "+
			"not charged for it and the guest can still meet ENOSPC underneath it", got, after)
	}
	got := make([]byte, len(body))
	r, _ := os.Open(path)
	defer r.Close()
	if _, err := r.ReadAt(got, 0); err != nil || string(got) != string(body) {
		t.Errorf("the disk's existing contents changed: %q", got)
	}
}

// A grow never shrinks and never creates: a smaller target is a no-op, and a missing disk is an
// error rather than a fresh file.
func TestExtendThickNeverShrinksOrCreates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.img")
	if err := ExtendThick(path, 1<<20); err == nil {
		t.Error("ExtendThick created a disk that did not exist")
	}
	if err := AllocateThick(path, 8<<20); err != nil {
		t.Fatal(err)
	}
	if err := ExtendThick(path, 4<<20); err != nil {
		t.Fatalf("ExtendThick to a smaller size: %v", err)
	}
	if fi, _ := os.Stat(path); fi.Size() != 8<<20 {
		t.Errorf("size = %d after a smaller target, want the original %d", fi.Size(), 8<<20)
	}
}

// allocatedBytes is what the filesystem has actually committed, which is the only way to tell a
// thick file from a sparse one of the same size.
func allocatedBytes(t *testing.T, path string) int64 {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return statBlocks(t, fi)
}
