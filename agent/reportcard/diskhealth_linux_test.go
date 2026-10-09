package reportcard

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestMountSource(t *testing.T) {
	mi := `22 1 259:2 / / rw,relatime shared:1 - btrfs /dev/nvme0n1p2 rw,subvol=/root
25 22 0:22 / /proc rw shared:5 - proc proc rw
40 22 253:0 / /var/lib rw shared:20 - ext4 /dev/mapper/vg-var rw
41 40 253:1 / /var/lib/briard rw shared:21 - xfs /dev/mapper/vg-briard rw
42 40 253:2 / /var/lib/briard rw shared:22 - xfs /dev/mapper/vg-over rw
43 22 0:40 / /mnt/my\040disk rw shared:23 - ext4 /dev/sdc1 rw
`
	for path, want := range map[string]string{
		"/var/lib/briard":          "/dev/mapper/vg-over", // mounted on top wins
		"/var/lib/briard/data.img": "/dev/mapper/vg-over",
		"/var/lib/briardx":         "/dev/mapper/vg-var", // a prefix of the name is not a parent
		"/opt/briard":              "/dev/nvme0n1p2",
		"/mnt/my disk/x":           "/dev/sdc1",
	} {
		if got := mountSource(mi, path); got != want {
			t.Errorf("%s: %q, want %q", path, got, want)
		}
	}
}

// LUKS on LVM on a partition of each of two disks: the walk ends at the two whole disks.
func TestLeafDisks(t *testing.T) {
	root := t.TempDir()
	sys := filepath.Join(root, "class/block")
	mk := func(p string) {
		if err := os.MkdirAll(filepath.Join(root, p), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	link := func(name, target string) {
		mk("class/block")
		if err := os.Symlink(filepath.Join(root, target), filepath.Join(sys, name)); err != nil {
			t.Fatal(err)
		}
	}
	touch := func(p string) {
		if err := os.WriteFile(filepath.Join(root, p), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, d := range []string{"devices/nvme0n1/nvme0n1p3", "devices/sda/sda1", "devices/dm-0/slaves/dm-1", "devices/dm-1/slaves/nvme0n1p3", "devices/dm-1/slaves/sda1"} {
		mk(d)
	}
	touch("devices/nvme0n1/nvme0n1p3/partition")
	touch("devices/sda/sda1/partition")
	link("nvme0n1", "devices/nvme0n1")
	link("nvme0n1p3", "devices/nvme0n1/nvme0n1p3")
	link("sda1", "devices/sda/sda1")
	link("dm-0", "devices/dm-0")
	link("dm-1", "devices/dm-1")

	got := leafDisks(sys, "dm-0")
	slices.Sort(got)
	if !slices.Equal(got, []string{"nvme0n1", "sda"}) {
		t.Fatalf("dm-0 rests on %v, want [nvme0n1 sda]", got)
	}
	if got := leafDisks(sys, "nvme0n1"); !slices.Equal(got, []string{"nvme0n1"}) {
		t.Fatalf("a whole disk is itself, got %v", got)
	}
}
