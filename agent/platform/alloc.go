package platform

import (
	"fmt"
	"os"
)

// THE DISKS A NODE'S GUEST RUNS ON, ALLOCATED BY THE AGENT ([B.157]).
//
// install.sh laid them down with `fallocate`, `truncate` and a `dd` fallback, behind a `noclobber`
// trick -- three shell calls and a subtlety, none of which a Windows host can reuse. Here they are
// two primitives with one per-OS seam (alloc_linux.go / alloc_windows.go), which is the shape the
// rest of this package already has for routes and units.
//
// ⚠️ EACH IS CREATE-ONLY. A disk that exists is never touched, resized or rewritten: these are the
// PET volumes, the ones a reinstall deliberately keeps, and the guest owns what is inside them.

// AllocateThick creates path at exactly size bytes with the space RESERVED, and does nothing if it
// already exists.
//
// THICK, and this is the one volume whose failure mode is unacceptable: DRBD replicates it and the
// guest writes service data into it, so a sparse file the host cannot actually back becomes ENOSPC
// *underneath a replicated filesystem*, mid-write, on the node holding the primary role. Reserving
// up front makes "is there room for this node's data?" a question answered once, by a call that
// either succeeds or refuses, rather than months later by a write that fails.
//
// ⚠️ THE CREATION IS THE PROOF OF ABSENCE ([B.126]), which is why this opens with O_EXCL rather
// than asking `Stat` first. "Absent" and "present but unstat-able" are different facts that a stat
// cannot separate, and the allocation below writes from byte 0 -- so a check-then-create would let
// an unreadable-but-present data volume be flattened. O_EXCL makes the kernel answer, atomically:
// anything already there fails the open, and the refusal says so instead of proceeding.
func AllocateThick(path string, size int64) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		if os.IsExist(err) {
			return nil // this node already has its data volume; it is the guest's, not ours
		}
		return fmt.Errorf("platform: creating %s: %w -- refusing to touch it; move it aside if "+
			"this node really is new", path, err)
	}
	defer f.Close()
	if err := allocate(f, size); err != nil {
		// Leave nothing half-made: a zero-length or partly-reserved file at this path would be
		// taken for a real volume by the next start, which is worse than not having one.
		_ = os.Remove(path)
		return fmt.Errorf("platform: allocating %d bytes at %s: %w (out of disk?)", size, path, err)
	}
	return nil
}

// AllocateSparse creates path as a sparse file of size bytes, and does nothing if it exists.
//
// Sparse is right here and wrong above: the state disk holds the guest's podman storage, its
// journal and the deadman's backoff state -- things it is fine to grow into and wrong to charge
// against the report card's free-space floor up front. The guest formats it on first boot when it
// finds no filesystem, so there is no mkfs to do.
func AllocateSparse(path string, size int64) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		if os.IsExist(err) {
			return nil
		}
		return fmt.Errorf("platform: creating %s: %w", path, err)
	}
	defer f.Close()
	if err := f.Truncate(size); err != nil {
		_ = os.Remove(path)
		return fmt.Errorf("platform: sizing %s to %d bytes: %w", path, size, err)
	}
	return nil
}
