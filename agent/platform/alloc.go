package platform

import (
	"fmt"
	"os"
	"path/filepath"

	"briard.io/shared/atomicfile"
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
// ⚠️ THE DISK APPEARS WHOLE OR NOT AT ALL. The bytes are reserved under path+".partial", flushed,
// and only then published at path. An allocation can be interrupted where no error path runs -- a
// watchdog past its lease, a power cut, minutes into a zero-write -- and a half-made file AT path
// would be taken for this node's volume by the next start: a short disk under the guest, and no
// error anywhere. A ".partial" is never anything else: only this function writes that name and
// nothing attaches it, so finding one is the positive proof of an interrupted allocation, and it is
// discarded and made again.
//
// ⚠️ THE PUBLISH IS THE PROOF OF ABSENCE. It is a hard link, which the kernel refuses when anything
// is already at path, atomically -- "absent" and "present but unstat-able" are different facts a
// stat cannot separate, and a rename would replace either. The Lstat before the allocation only
// saves the work when the volume is plainly there; it decides nothing destructive.
func AllocateThick(path string, size int64) error {
	partial := path + ".partial"
	if _, err := os.Lstat(path); err == nil {
		// This node already has its volume; it is the guest's, not ours. A ".partial" beside it is
		// the second name of the same file, left by a publish interrupted before its last step.
		_ = os.Remove(partial)
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("platform: %s: %w -- refusing to touch it; move it aside if this node "+
			"really is new", path, err)
	}
	if err := os.Remove(partial); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("platform: discarding the interrupted allocation %s: %w", partial, err)
	}
	f, err := os.OpenFile(partial, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("platform: creating %s: %w", partial, err)
	}
	err = allocate(f, 0, size)
	if err == nil {
		err = f.Sync() // the reservation is durable BEFORE the name that vouches for it exists
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(partial)
		return fmt.Errorf("platform: allocating %d bytes at %s: %w (out of disk?)", size, partial, err)
	}
	if err := os.Link(partial, path); err != nil && !os.IsExist(err) {
		_ = os.Remove(partial)
		return fmt.Errorf("platform: publishing %s: %w", path, err)
	}
	// IsExist: something reached path during the allocation. It is left alone, like any volume
	// that is already there, and the allocation is discarded.
	if err := os.Remove(partial); err != nil {
		return fmt.Errorf("platform: %s is made, but its allocation name %s stays: %w", path, partial, err)
	}
	return atomicfile.SyncDir(filepath.Dir(path))
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

// zeroReader is an endless source of zero bytes -- io.CopyN's buffer does the chunking, so the
// write-the-bytes allocation (the Linux fallback, the Windows arm) streams rather than holding
// the volume in memory.
type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}
