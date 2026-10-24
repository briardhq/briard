//go:build !windows

package atomicfile

import "os"

// openDir opens a directory for fsync: read-only is the portable spelling.
func openDir(dir string) (*os.File, error) { return os.Open(dir) }

// openFile opens a regular file for fsync: read-only is enough.
func openFile(path string) (*os.File, error) { return os.Open(path) }
