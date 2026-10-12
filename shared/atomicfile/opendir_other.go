//go:build !windows

package atomicfile

import "os"

// openDir opens a directory for fsync: read-only is the portable spelling.
func openDir(dir string) (*os.File, error) { return os.Open(dir) }
