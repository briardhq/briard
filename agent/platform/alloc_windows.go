package platform

import (
	"io"
	"os"
)

// allocate reserves the bytes of f from `from` up to `size`: the Windows arm, and deliberately the
// slow one.
//
// SetEndOfFile alone reserves the clusters with no privilege, and reads past the file's valid-data
// length return zeros, but the saving is only deferred: NTFS keeps that length, and a write past it
// zero-fills the gap first, synchronously, inside qemu's write. That is the first bring-up's mkfs,
// which scatters its backup superblocks over the whole state disk, and on the data volume a stall
// as long as the gap whenever the filesystem allocates further out, under a running app. Moving the
// length up front (SetFileValidData) needs SE_MANAGE_VOLUME_NAME because it exposes whatever was
// previously on the disk; a briard host is somebody's desktop, and that trade is not worth making.
//
// So this writes the zeros once, before the guest exists, where nothing is waiting on the cost:
// the answer the Linux arm falls back to on a filesystem without fallocate.
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

// machineOpts is what the `-machine` line carries after the accel list: nothing on Windows. The
// HPET's 1 kHz tick that Linux drops costs here too, but WHPX gives the guest no kvm-clock, and
// without the HPET the guest's clocksource could fall to acpi_pm, a port exit on every clock read,
// which would trade the tick for something worse. It stays until a WHPX guest is seen on tsc.
const machineOpts = ""
