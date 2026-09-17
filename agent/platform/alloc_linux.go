package platform

import (
	"io"
	"os"
	"syscall"
)

// allocate reserves size bytes for f. THE LINUX ARM of the one seam in alloc.go.
//
// fallocate(2) is the fast path: it reserves extents in the filesystem's allocator and writes no
// data, so a 4 GiB volume costs milliseconds. Not every filesystem implements it (older ext,
// several network ones), and the kernel says so with ENOTSUP/EOPNOTSUPP rather than lying -- so the
// fallback writes the bytes, which is slow and correct.
//
// ⚠️ THE FALLBACK MUST ACTUALLY WRITE. Truncate would return instantly and leave a sparse file,
// which is precisely the state AllocateThick exists to prevent: the space would not be reserved and
// the failure would arrive later, under a replicated filesystem, as ENOSPC mid-write.
func allocate(f *os.File, size int64) error {
	err := syscall.Fallocate(int(f.Fd()), 0, 0, size)
	if err == nil {
		return nil
	}
	if err != syscall.ENOTSUP && err != syscall.EOPNOTSUPP {
		return err
	}
	if _, err := io.CopyN(f, zeroReader{}, size); err != nil {
		return err
	}
	return f.Sync()
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
