package platform

import (
	"io"
	"os"
)

// allocate reserves the bytes of f from `from` up to `size`. THE WINDOWS ARM, and deliberately
// the slow one.
//
// Setting the end of the file alone (SetEndOfFile) reserves the clusters with no privilege, and
// reads past the file's valid-data length return zeros. What it saves is only deferred: NTFS keeps
// that length, and a write past it zero-fills the gap first, synchronously, inside the write --
// qemu's write. That is the first bring-up's mkfs, which scatters its backup superblocks over the
// whole state disk, and on the data volume a stall as long as the gap whenever the filesystem
// allocates further out, under a running service. Moving the length up front instead
// (SetFileValidData) needs SE_MANAGE_VOLUME_NAME and exposes whatever was previously on the disk,
// which is why it needs the privilege; a briard host is somebody's desktop, and that is not a
// trade worth making.
//
// So this writes the zeros, once, before the guest exists: the cost is paid where nothing is
// waiting on it. It is the same answer the Linux arm falls back to on a filesystem without
// fallocate.
func allocate(f *os.File, from, size int64) error {
	if _, err := f.Seek(from, io.SeekStart); err != nil {
		return err
	}
	if _, err := io.CopyN(f, zeroReader{}, size-from); err != nil {
		return err
	}
	return f.Sync()
}

// setNoCOW is the Windows arm of SetNoCOW: NTFS does not copy on write, so there is nothing to set.
func setNoCOW(string) error { return nil }

// diskCache is `writeback` on Windows, because the Linux answer does not open: with cache.direct on
// (`none`, `directsync`) the Windows qemu reads the image unbuffered and refuses a good qcow2 as
// "not in qcow2 format" (measured on the shipped bundle). Guest flushes are honoured in writeback
// too, so what this costs is the host's second copy of the bytes, not durability.
const diskCache = "writeback"
