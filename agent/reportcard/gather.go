package reportcard

import "os"

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// DiskFreeMB is the free space on the filesystem holding path, in MB; 0 when it cannot be read.
// The host agent checks the same number before it grows the guest's state disk.
func DiskFreeMB(path string) int { return diskFreeMB(path) }
