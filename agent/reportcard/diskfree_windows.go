package reportcard

import "golang.org/x/sys/windows"

// diskFreeMB is the space the caller can use on path's volume, in MB; 0 when unreadable, which the
// report card reads as "unknown" rather than as a full disk.
func diskFreeMB(path string) int {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0
	}
	var free uint64
	if windows.GetDiskFreeSpaceEx(p, &free, nil, nil) != nil {
		return 0
	}
	return int(free >> 20)
}
