package reportcard

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// DataDisks names the physical disks under the filesystem holding path, as /dev paths: the
// mount's source, through any device-mapper or md layers (LUKS, LVM, RAID), to the whole disks
// beneath -- SMART belongs to a disk, never to a partition or a mapping.
func DataDisks(path string) ([]string, error) {
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, err
	}
	mi, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return nil, err
	}
	src := mountSource(string(mi), real)
	if !strings.HasPrefix(src, "/dev/") {
		return nil, fmt.Errorf("%s is on %q, not on a disk this machine can name", path, src)
	}
	dev, err := filepath.EvalSymlinks(src) // /dev/mapper/x -> /dev/dm-0
	if err != nil {
		return nil, err
	}
	var out []string
	for _, d := range leafDisks("/sys/class/block", filepath.Base(dev)) {
		out = append(out, "/dev/"+d)
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}

// mountSource is the source device of the mount holding path, from /proc/self/mountinfo: the
// longest mount point that contains it, the later line winning a tie (it is mounted on top).
func mountSource(mountinfo, path string) string {
	var best, src string
	for line := range strings.SplitSeq(mountinfo, "\n") {
		pre, post, ok := strings.Cut(line, " - ")
		f, g := strings.Fields(pre), strings.Fields(post)
		if !ok || len(f) < 5 || len(g) < 2 {
			continue
		}
		mp := strings.ReplaceAll(f[4], `\040`, " ")
		if path != mp && mp != "/" && !strings.HasPrefix(path, mp+"/") {
			continue
		}
		if len(mp) >= len(best) {
			best, src = mp, g[1]
		}
	}
	return src
}

// leafDisks walks a block device down to the whole disks beneath it: a mapping's slaves, and a
// partition's parent.
func leafDisks(sys, name string) []string {
	if slaves, _ := os.ReadDir(filepath.Join(sys, name, "slaves")); len(slaves) > 0 {
		var out []string
		for _, s := range slaves {
			out = append(out, leafDisks(sys, s.Name())...)
		}
		return out
	}
	if _, err := os.Stat(filepath.Join(sys, name, "partition")); err == nil {
		if real, err := filepath.EvalSymlinks(filepath.Join(sys, name)); err == nil {
			return []string{filepath.Base(filepath.Dir(real))}
		}
	}
	return []string{name}
}
