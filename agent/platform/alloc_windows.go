package platform

import (
	"io"
	"os"
)

// allocate reserves the bytes of f from `from` up to `size`. THE WINDOWS ARM, and deliberately
// the slow one.
//
// Windows can reserve without writing (SetFileValidData after SetEndOfFile), but only for a process
// holding SE_MANAGE_VOLUME_NAME -- and the bytes it exposes are whatever was previously on the
// disk, so the privilege exists because the call leaks. A briard host is somebody's desktop; taking
// that privilege to save a minute once, at install, is not a trade worth making.
//
// So this writes the zeros. It is slower and it is correct, which is the same answer the Linux arm
// falls back to on a filesystem without fallocate.
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
