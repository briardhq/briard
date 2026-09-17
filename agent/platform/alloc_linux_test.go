package platform

import (
	"io/fs"
	"syscall"
	"testing"
)

// statBlocks reads the 512-byte blocks the filesystem has committed to a file. Linux-only, like
// the thick allocation it verifies; a Windows arm would ask GetFileInformationByHandleEx.
func statBlocks(t *testing.T, fi fs.FileInfo) int64 {
	t.Helper()
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		t.Skip("no stat_t on this platform: cannot tell a thick file from a sparse one")
	}
	return st.Blocks * 512
}
