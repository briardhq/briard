package guestagent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"briard.io/shared/nodestorage"
)

// mountFake answers the one question PrimaryStorage asks the machine -- is the volume already
// mounted -- and holds the spec it reads the device from.
type mountFake struct {
	*fakeExec
	mounted bool
}

// fakeVolumeUUID is the mounted volume's filesystem uuid, which names its sysfs directory.
const fakeVolumeUUID = "5f0e3c2a-0000-4000-8000-000000000001"

func quotaModePath() string { return "/sys/fs/btrfs/" + fakeVolumeUUID + "/qgroups/mode" }

func newMountFake() *mountFake {
	m := &mountFake{fakeExec: &fakeExec{}}
	m.runFn = func(name string, args []string) ([]byte, error) {
		if name == "mountpoint" && !m.mounted {
			return nil, errors.New("exit status 32")
		}
		if name == "findmnt" {
			return []byte(fakeVolumeUUID + "\n"), nil
		}
		return nil, nil
	}
	_ = m.WriteFile(quotaModePath(), []byte("squota\n"))
	raw, err := demoSpec(nodestorage.ModeAuto, true).Marshal()
	if err != nil {
		panic(err)
	}
	_ = m.WriteFile(nodestorage.Path, raw)
	return m
}

func (m *mountFake) ran(words ...string) bool {
	for _, r := range m.runs {
		if len(r) >= len(words) && strings.Join(r[:len(words)], " ") == strings.Join(words, " ") {
			return true
		}
	}
	return false
}

// THE LOAD-BEARING NEGATIVE: nothing on the promotion path formats, whatever the spec says about
// seeding. The format is briard-node-storage's, so a promotion -- which happens on every
// failover, on every node, forever -- has no destructive operation on it at all.
func TestPrimaryStorageNeverFormats(t *testing.T) {
	f := newMountFake()
	if err := PrimaryStorage(context.Background(), f); err != nil {
		t.Fatal(err)
	}
	if f.ran("mkfs.btrfs") {
		t.Fatalf("a promotion formatted the replicated volume: %v", f.runs)
	}
	want := [][]string{
		{"mkdir", "-p", dataMountRoot},
		{"mountpoint", "-q", dataMountRoot},
		{"mount", "-o", "compress=zstd:1", "/dev/drbd0", dataMountRoot},
		{"findmnt", "-no", "UUID", dataMountRoot},
		{"mkdir", "-p", snapshotsDir()},
	}
	for i, w := range want {
		if i >= len(f.runs) || strings.Join(f.runs[i], " ") != strings.Join(w, " ") {
			t.Fatalf("step %d = %v, want %v (all: %v)", i, f.runs[i:], w, f.runs)
		}
	}
}

// The device comes off the spec, not a constant: a spec naming another device mounts that one.
func TestPrimaryStorageMountsTheDeviceTheSpecNames(t *testing.T) {
	f := newMountFake()
	spec := demoSpec(nodestorage.ModeAuto, false)
	spec.Resource.Device = "/dev/drbd7"
	raw, _ := spec.Marshal()
	_ = f.WriteFile(nodestorage.Path, raw)
	if err := PrimaryStorage(context.Background(), f); err != nil {
		t.Fatal(err)
	}
	if !f.ran("mount", "-o", "compress=zstd:1", "/dev/drbd7", dataMountRoot) {
		t.Errorf("runs = %v", f.runs)
	}
}

func TestPrimaryStorageDoesNotRemount(t *testing.T) {
	f := newMountFake()
	f.mounted = true
	if err := PrimaryStorage(context.Background(), f); err != nil {
		t.Fatal(err)
	}
	if f.ran("mount") {
		t.Errorf("an already-mounted volume was mounted again: %v", f.runs)
	}
}

// A volume not in simple-quota mode is refused and left unmounted, and nothing is made on it:
// classic qgroups (the slow mode), quotas off (no mode file at all), and a mode the kernel
// names that we have never heard of. Each is a different read, and each must refuse.
func TestPrimaryStorageRefusesAVolumeWithoutSimpleQuotas(t *testing.T) {
	for _, mode := range []string{"qgroup\n", "", "something-newer\n"} {
		f := newMountFake()
		if mode == "" {
			delete(f.files, quotaModePath())
		} else {
			_ = f.WriteFile(quotaModePath(), []byte(mode))
		}
		err := PrimaryStorage(context.Background(), f)
		if err == nil || !strings.Contains(err.Error(), "reinstall") {
			t.Errorf("mode %q: err = %v, want a refusal that says reinstall", mode, err)
		}
		if !f.ran("umount", dataMountRoot) {
			t.Errorf("mode %q: the refused volume was left mounted: %v", mode, f.runs)
		}
		if f.ran("mkdir", "-p", snapshotsDir()) {
			t.Errorf("mode %q: the refused volume was written to: %v", mode, f.runs)
		}
	}
}

// No spec means no device to mount: fail the chain rather than guess one.
func TestPrimaryStorageRefusesWithoutASpec(t *testing.T) {
	f := &mountFake{fakeExec: &fakeExec{}}
	if err := PrimaryStorage(context.Background(), f); err == nil {
		t.Fatal("a missing spec was not reported")
	}
	if f.ran("mount") {
		t.Error("...and something was mounted anyway")
	}
}

func TestPrimaryStorageStopUnmounts(t *testing.T) {
	f := newMountFake()
	if err := PrimaryStorageStop(context.Background(), f); err != nil {
		t.Fatal(err)
	}
	if !f.ran("umount", dataMountRoot) {
		t.Errorf("runs = %v", f.runs)
	}
}
