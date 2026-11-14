package reportcard

import "syscall"

// diskFreeMB returns free space on the filesystem holding path, in MB (0 if unreadable --
// best-effort, like every other reader here). Bfree, not Bavail: the installer and the agent run as
// root, so the root-reserved blocks really are available to them and Bavail would under-report by
// ~5%.
func diskFreeMB(path string) int {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0
	}
	return int(uint64(st.Bfree) * uint64(st.Bsize) >> 20)
}
