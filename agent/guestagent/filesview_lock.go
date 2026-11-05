//go:build !windows

package guestagent

import (
	"fmt"
	"os"
	"syscall"
)

// lockFilesView takes the view's lock, across processes, and returns its release.
func lockFilesView() (func(), error) {
	f, err := os.OpenFile(filesViewLock, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("files view: lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, fmt.Errorf("files view: lock: %w", err)
	}
	return func() { f.Close() }, nil
}
