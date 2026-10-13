package host

// selfResources has no Windows reader yet: both fields stay zero, which the leak signal reads
// as "unmeasured", the same answer a /proc read miss gives on Linux.
func selfResources() (rssKB int64, fds int) { return 0, 0 }
