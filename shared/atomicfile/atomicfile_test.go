package atomicfile

import (
	"os"
	"path/filepath"
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
	if err := os.WriteFile(filepath.Join(dir, "tree", "bin", "qemu"), []byte("y"), 0o555); err != nil {
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
