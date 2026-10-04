package platform

import (
	"fmt"
	"os"
)

// THE DISKS A NODE'S GUEST RUNS ON, ALLOCATED BY THE AGENT.
//
// install.sh laid them down with `fallocate`, `truncate` and a `dd` fallback, behind a `noclobber`
// trick -- three shell calls and a subtlety, none of which a Windows host can reuse. Here they are
// a few primitives with one per-OS seam (alloc_linux.go / alloc_windows.go), which is the shape the
// rest of this package already has for routes and units.
//
// ⚠️ CREATION NEVER OVERWRITES. A disk that exists is never recreated or rewritten: these are the
// PET volumes, the ones a reinstall deliberately keeps, and the guest owns what is inside them.
// The one change a disk may undergo is to GROW (ExtendThick), which adds reserved bytes at its end
// and leaves every byte before them alone.

// AllocateThick creates path at exactly size bytes with the space RESERVED, and does nothing if it
// already exists.
//
// THICK, and this is the one volume whose failure mode is unacceptable: DRBD replicates it and the
// guest writes service data into it, so a sparse file the host cannot actually back becomes ENOSPC
// *underneath a replicated filesystem*, mid-write, on the node holding the primary role. Reserving
// up front makes "is there room for this node's data?" a question answered once, by a call that
// either succeeds or refuses, rather than months later by a write that fails.
//
// ⚠️ THE CREATION IS THE PROOF OF ABSENCE, which is why this opens with O_EXCL rather
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
	if err := allocate(f, 0, size); err != nil {
		// Leave nothing half-made: a zero-length or partly-reserved file at this path would be
		// taken for a real volume by the next start, which is worse than not having one.
		_ = os.Remove(path)
		return fmt.Errorf("platform: allocating %d bytes at %s: %w (out of disk?)", size, path, err)
	}
	return nil
}

// ExtendThick grows the thick file at path to size bytes with the new range RESERVED, and does
// nothing when it is already that large. It never shrinks and never creates: the disk it grows is
// a pet the guest owns, so a missing one is an error, not something to make.
//
// The one allocation that may happen while the file is in use -- the guest's state disk grows
// under a running VM, which sees the new size only once QEMU is told (Guest.ResizeStateDisk). A
// reservation that fails half-way is cut back to the old size, so the file never claims bytes
// its blocks do not back.
func ExtendThick(path string, size int64) error {
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return fmt.Errorf("platform: opening %s to grow it: %w", path, err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return fmt.Errorf("platform: %s: %w", path, err)
	}
	if fi.Size() >= size {
		return nil
	}
	if err := allocate(f, fi.Size(), size); err != nil {
		_ = f.Truncate(fi.Size())
		return fmt.Errorf("platform: growing %s from %d to %d bytes: %w (out of disk?)", path, fi.Size(), size, err)
	}
	return f.Sync()
}

// SetNoCOW asks the filesystem holding dir not to copy-on-write the files created in it from now
// on (`chattr +C`; inherited, and only honoured on a file while it is empty). THICK IS NOT A
// PROMISE ON A COPY-ON-WRITE FILESYSTEM: on btrfs every overwrite of a reserved block allocates a
// new one, so a reservation made at install does not cover a write made a month later. Setting it
// on the directory before the disks are created is what makes their reservation mean what it says.
// Filesystems that never copy (ext4, XFS) have no such flag and are not an error.
func SetNoCOW(dir string) error {
	if err := setNoCOW(dir); err != nil {
		return fmt.Errorf("platform: marking %s no-copy-on-write: %w", dir, err)
	}
	return nil
}
