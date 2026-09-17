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

// ⚠️ THE DESTRUCTIVE CASE ([B.126]). A data volume that is already there is never written over --
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

// Sparse is the right answer for the state disk and the wrong one above, so it is asserted as
// explicitly: the file is the full size and costs almost nothing on disk.
func TestAllocateSparseCostsNothingYet(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.img")
	const size = 8 << 30 // the shipped ceiling, which is the point: it must not cost 8 GiB
	if err := AllocateSparse(path, size); err != nil {
		t.Fatalf("AllocateSparse: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() != size {
		t.Errorf("size = %d, want %d", fi.Size(), size)
	}
	if blocks := allocatedBytes(t, path); blocks > 1<<20 {
		t.Errorf("%d bytes are allocated for an empty state disk -- it is not sparse, and the "+
			"report card's free-space floor would be charged for a ceiling", blocks)
	}
}

// A state disk that exists is the one a reinstall deliberately kept: podman's storage, the journal,
// the deadman's backoff. Recreating it would cost every service image on the node.
func TestAllocateSparseNeverOverwrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.img")
	body := []byte("formatted, with the guest's storage in it")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := AllocateSparse(path, 8<<30); err != nil {
		t.Fatalf("AllocateSparse over an existing disk: %v", err)
	}
	if fi, _ := os.Stat(path); fi.Size() != int64(len(body)) {
		t.Errorf("the existing state disk was resized to %d, want its original %d -- it should have "+
			"been left alone", fi.Size(), len(body))
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
