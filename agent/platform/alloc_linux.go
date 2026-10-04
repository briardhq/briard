package platform

import (
	"io"
	"os"
	"syscall"
	"unsafe"
)

// allocate reserves the bytes of f from `from` up to `size`. THE LINUX ARM of the one seam in
// alloc.go.
//
// fallocate(2) is the fast path: it reserves extents in the filesystem's allocator and writes no
// data, so a 4 GiB volume costs milliseconds. Not every filesystem implements it (older ext,
// several network ones), and the kernel says so with ENOTSUP/EOPNOTSUPP rather than lying -- so the
// fallback writes the bytes, which is slow and correct.
//
// ⚠️ THE FALLBACK MUST ACTUALLY WRITE. Truncate would return instantly and leave a sparse file,
// which is precisely the state AllocateThick exists to prevent: the space would not be reserved and
// the failure would arrive later, under a replicated filesystem, as ENOSPC mid-write.
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
// in it afterwards inherit it. THE LINUX ARM; see SetNoCOW. A filesystem without the flag (ext4,
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

// zeroReader is an endless source of zero bytes -- io.CopyN's buffer does the chunking, so the
// fallback streams rather than allocating the volume in memory.
type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}
