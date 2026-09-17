package platform

import (
	"io/fs"
	"testing"
)

// statBlocks has no Windows arm yet: the thick/sparse distinction is asked of the filesystem, and
// on Windows that is GetFileInformationByHandleEx rather than a stat field. Skipping is honest --
// the allocator's OTHER properties (size, mode, never-overwrite) are asserted on both arms, and
// this is the one assertion that needs a per-OS reader to mean anything.
//
// It exists at all so `GOOS=windows go vet ./...` compiles the package's tests, which is the gate
// that caught its absence.
func statBlocks(t *testing.T, fi fs.FileInfo) int64 {
	t.Helper()
	t.Skip("no per-file allocation reader on Windows yet: cannot tell thick from sparse here")
	return 0
}
