package reportcard

// diskFreeMB has no Windows reader yet: 0 is what every other unreadable fact in this package
// reports, so the report card degrades to "unknown" rather than lying. The real one is
// GetDiskFreeSpaceExW and belongs with the rest of the Windows host agent ([V5.5]).
func diskFreeMB(string) int { return 0 }
