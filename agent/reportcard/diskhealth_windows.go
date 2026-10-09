package reportcard

// DataDisks has no Windows reader yet, and no smartctl travels in the Windows bundle: no disks,
// so neither the doctor nor the agent says anything about disk health there.
func DataDisks(string) ([]string, error) { return nil, nil }
