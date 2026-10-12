package reportcard

import (
	"bufio"
	"os"
	"strconv"
	"strings"
)

// MemTotalMB is the machine's installed RAM (MemTotal), in MB; 0 when it cannot be read. The host
// agent sizes its guest's growth ceiling from the same number the report card grades.
func MemTotalMB() int {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "MemTotal:") {
			continue
		}
		fields := strings.Fields(line) // MemTotal:  16384000 kB
		if len(fields) >= 2 {
			if kb, err := strconv.Atoi(fields[1]); err == nil {
				return kb / 1024
			}
		}
	}
	_ = sc.Err()
	return 0
}
