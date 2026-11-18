package guestagent

import (
	"os"
	"path/filepath"
	"testing"
)

// TestMain moves the guest's cross-process locks out of /run, which a test may not write: every
// test that reaches a converge or the Files view takes a lock. A test that needs a lock of its
// own still sets one.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "guestagent-locks")
	if err != nil {
		panic(err)
	}
	convergeLock = filepath.Join(dir, "converge.lock")
	filesViewLock = filepath.Join(dir, "files-view.lock")
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}
