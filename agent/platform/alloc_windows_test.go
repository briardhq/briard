package platform

import (
	"io/fs"
	"testing"
)

// statBlocks has no Windows arm yet: on Windows the thick/sparse question is
// GetFileInformationByHandleEx, not a stat field. Skipping is honest: the allocator's other
// properties (size, never-overwrite) are asserted on both arms (mode on Linux only), and this is
// the one assertion that needs a per-OS reader. statBlocks exists at all so
// `GOOS=windows go vet ./...` compiles the package's tests.
func statBlocks(t *testing.T, fi fs.FileInfo) int64 {
	t.Helper()
	t.Skip("no per-file allocation reader on Windows yet: cannot tell thick from sparse here")
	return 0
}
