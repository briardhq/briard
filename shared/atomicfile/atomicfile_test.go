package atomicfile

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// SyncTree takes a single file as well as a tree: the guest image is staged as one file, the qemu
// bundle as a tree, and both go through it.
func TestSyncTreeTakesAFileOrATree(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "image")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "tree", "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A release tree's executables are read-only on Linux. On Windows a mode without the owner's
	// write bit is the read-only attribute, which the writable open FlushFileBuffers needs refuses;
	// no tree the agent unpacks there carries it (tar.exe sets none, a fetch writes 0644 or 0755).
	exe := os.FileMode(0o555)
	if runtime.GOOS == "windows" {
		exe = 0o755
	}
	if err := os.WriteFile(filepath.Join(dir, "tree", "bin", "qemu"), []byte("y"), exe); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("bin/qemu", filepath.Join(dir, "tree", "link")); err != nil {
		t.Fatal(err)
	}
	for _, root := range []string{file, filepath.Join(dir, "tree")} {
		if err := SyncTree(root); err != nil {
			t.Errorf("SyncTree(%s): %v", root, err)
		}
	}
	// A missing root is an error, not a silent success: the caller is about to rename it.
	if err := SyncTree(filepath.Join(dir, "absent")); err == nil {
		t.Error("SyncTree on a missing path returned nil")
	}
}
