package platform

import (
	"io"
	"os"
)

// allocate reserves size bytes for f. THE WINDOWS ARM, and deliberately the slow one.
//
// Windows can reserve without writing (SetFileValidData after SetEndOfFile), but only for a process
// holding SE_MANAGE_VOLUME_NAME -- and the bytes it exposes are whatever was previously on the
// disk, so the privilege exists because the call leaks. A briard host is somebody's desktop; taking
// that privilege to save a minute once, at install, is not a trade worth making.
//
// So this writes the zeros. It is slower and it is correct, which is the same answer the Linux arm
// falls back to on a filesystem without fallocate.
func allocate(f *os.File, size int64) error {
	if _, err := io.CopyN(f, zeroReader{}, size); err != nil {
		return err
	}
	return f.Sync()
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}
