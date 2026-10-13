package host

import "os"

// selfResources measures this agent process's own resident set (KB) and open fd count from
// /proc/self -- the host-side half of the leak signal. Both best-effort: a read miss leaves
// the field zero (read as "unmeasured"), never an error on the observe path.
func selfResources() (rssKB int64, fds int) {
	if b, err := os.ReadFile("/proc/self/status"); err == nil {
		rssKB = parseSelfVmRSSKB(b)
	}
	if ents, err := os.ReadDir("/proc/self/fd"); err == nil {
		fds = len(ents)
	}
	return
}
