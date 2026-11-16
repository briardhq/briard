package platform

import (
	"io"
	"os"
	"syscall"
	"unsafe"
)

// allocate reserves the bytes of f from `from` up to `size`: the Linux arm of the one seam in
// alloc.go.
//
// fallocate(2) is the fast path: it reserves extents in the filesystem's allocator and writes no
// data, so a 4 GiB volume costs milliseconds. Filesystems without it (older ext, several network
// ones) say so with ENOTSUP/EOPNOTSUPP rather than lying, and the fallback writes the bytes: slow
// and correct.
//
// ⚠️ The fallback must actually write. Truncate would return instantly and leave a sparse file,
// precisely the state AllocateThick exists to prevent: the space unreserved, and the failure
// arriving later, under a replicated filesystem, as ENOSPC mid-write.
func allocate(f *os.File, from, size int64) error {
	err := syscall.Fallocate(int(f.Fd()), 0, from, size-from)
	if err == nil {
		return nil
	}
	if err != syscall.ENOTSUP && err != syscall.EOPNOTSUPP {
		return err
	}
	if _, err := f.Seek(from, io.SeekStart); err != nil {
		return err
	}
	if _, err := io.CopyN(f, zeroReader{}, size-from); err != nil {
		return err
	}
	return f.Sync()
}

// The flag bits of FS_IOC_GETFLAGS / FS_IOC_SETFLAGS (linux/fs.h), for the one flag we set.
const (
	fsIocGetFlags = 0x80086601
	fsIocSetFlags = 0x40086602
	fsNoCOWFlag   = 0x00800000
)

// setNoCOW marks dir NOCOW (`chattr +C`) where the filesystem has the flag, so the disks created
// in it afterwards inherit it. The Linux arm; see SetNoCOW. A filesystem without the flag (ext4,
// XFS: they never copy on write) answers ENOTTY/EOPNOTSUPP/EINVAL, which is not an error here.
func setNoCOW(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	var flags int32
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, d.Fd(), fsIocGetFlags, uintptr(unsafe.Pointer(&flags))); e != 0 {
		return ignoreNoFlags(e)
	}
	if flags&fsNoCOWFlag != 0 {
		return nil
	}
	flags |= fsNoCOWFlag
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, d.Fd(), fsIocSetFlags, uintptr(unsafe.Pointer(&flags))); e != 0 {
		return ignoreNoFlags(e)
	}
	return nil
}

func ignoreNoFlags(e syscall.Errno) error {
	if e == syscall.ENOTTY || e == syscall.EOPNOTSUPP || e == syscall.EINVAL {
		return nil
	}
	return e
}

// diskCache is every guest disk's qemu cache mode. `none` is O_DIRECT: the guest keeps a page cache
// of its own, so the host's would be a second copy of the same bytes, paid out of RAM a small board
// does not have, and guest flushes are honoured either way.
const diskCache = "none"

// machineOpts is what the `-machine` line carries after the accel list. `hpet=off`: qemu's
// emulated HPET ticks at 1 kHz in legacy mode, a pair of interrupt ioctls every millisecond on
// qemu's main thread, for a device this guest never reads (kvm-clock is its clocksource, the
// local APIC its timers). Measured on an idle node: qemu at ~15% of a core with it, ~7% without,
// the main thread from ~1000 wakeups a second to 4.
const machineOpts = ",hpet=off"
