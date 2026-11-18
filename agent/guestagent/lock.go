//go:build !windows

package guestagent

import (
	"fmt"
	"os"
	"syscall"
)

// lockFile takes an exclusive lock on path, across processes, and returns its release. The lock
// is the open file's, so a process that dies releases it with its descriptors.
func lockFile(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	return func() { f.Close() }, nil
}
