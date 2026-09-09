package guestagent

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// mountFake answers the two questions PrimaryStorage asks the machine: is the volume already
// mounted, and can the format marker be read.
type mountFake struct {
	*fakeExec
	mounted  bool
	readErr  error // returned by ReadFile for the format marker only
	mkfsFail bool
}

func newMountFake() *mountFake {
	m := &mountFake{fakeExec: &fakeExec{}}
	m.runFn = func(name string, args []string) ([]byte, error) {
		switch {
		case name == "mountpoint":
			if !m.mounted {
				return nil, errors.New("exit status 32")
			}
		case name == "mkfs.btrfs" && m.mkfsFail:
			return []byte("device busy"), errors.New("exit status 1")
		}
		return nil, nil
	}
	return m
}

func (m *mountFake) ReadFile(path string) ([]byte, error) {
	if path == dataFormatMarker && m.readErr != nil {
		return nil, m.readErr
	}
	return m.fakeExec.ReadFile(path)
}

func (m *mountFake) ran(words ...string) bool {
	for _, r := range m.runs {
		if len(r) >= len(words) && strings.Join(r[:len(words)], " ") == strings.Join(words, " ") {
			return true
		}
	}
	return false
}

// ★ THE NEGATIVE THIS WHOLE UNIT EXISTS FOR: with no marker, nothing formats. This runs at EVERY
// promotion, forever, on a volume holding a household's data.
func TestPrimaryStorageNeverFormatsUnarmed(t *testing.T) {
	f := newMountFake()
	if err := PrimaryStorage(context.Background(), f); err != nil {
		t.Fatal(err)
	}
	if f.ran("mkfs.btrfs") {
		t.Fatalf("an unarmed promotion formatted the replicated volume: %v", f.runs)
	}
	if !f.ran("mount", drbdDevice, dataMountRoot) {
		t.Errorf("the volume was not mounted: %v", f.runs)
	}
	if !f.ran("mkdir", "-p", snapshotsDir()) {
		t.Errorf("the snapshots dir was not made: %v", f.runs)
	}
}

// Armed by storage bring-up: format once, and consume the marker BEFORE the format so a format
// that fails is never retried.
func TestPrimaryStorageFormatsWhenArmed(t *testing.T) {
	f := newMountFake()
	_ = f.WriteFile(dataFormatMarker, []byte("r0\n"))
	if err := PrimaryStorage(context.Background(), f); err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{"mkdir", "-p", dataMountRoot},
		{"rm", "-f", dataFormatMarker},
		{"mkfs.btrfs", "-f", drbdDevice},
		{"mountpoint", "-q", dataMountRoot},
		{"mount", drbdDevice, dataMountRoot},
		{"mkdir", "-p", snapshotsDir()},
	}
	for i, w := range want {
		if i >= len(f.runs) || strings.Join(f.runs[i], " ") != strings.Join(w, " ") {
			t.Fatalf("step %d = %v, want %v (all: %v)", i, f.runs[i:], w, f.runs)
		}
	}
}

// The consume-first order, stated as its own property: a failed mkfs leaves nothing behind that
// would make the NEXT promotion try again.
func TestPrimaryStorageConsumesTheMarkerBeforeFormatting(t *testing.T) {
	f := newMountFake()
	f.mkfsFail = true
	_ = f.WriteFile(dataFormatMarker, []byte("r0\n"))
	if err := PrimaryStorage(context.Background(), f); err == nil {
		t.Fatal("a failed mkfs was not reported; the chain would continue onto an unformatted volume")
	}
	rm, mkfs := -1, -1
	for i, r := range f.runs {
		if r[0] == "rm" {
			rm = i
		}
		if r[0] == "mkfs.btrfs" {
			mkfs = i
		}
	}
	if rm < 0 || mkfs < 0 || rm > mkfs {
		t.Errorf("the marker was not consumed before the format: %v", f.runs)
	}
}

// A RETRY MUST NOT STACK A SECOND MOUNT ([B.125](b)): mounting an already-mounted path succeeds
// and hides the first mount, so the unit has to ask.
func TestPrimaryStorageDoesNotRemount(t *testing.T) {
	f := newMountFake()
	f.mounted = true
	if err := PrimaryStorage(context.Background(), f); err != nil {
		t.Fatal(err)
	}
	if f.ran("mount", drbdDevice, dataMountRoot) {
		t.Errorf("an already-mounted volume was mounted again: %v", f.runs)
	}
}

// A marker we could not READ is not a marker that is absent. Refusing is the same direction the
// rest of this unit takes: fail loudly, demote, rather than guess about a destructive act.
func TestPrimaryStorageRefusesAnUnreadableMarker(t *testing.T) {
	f := newMountFake()
	f.readErr = errors.New("input/output error")
	err := PrimaryStorage(context.Background(), f)
	if err == nil {
		t.Fatal("an unreadable marker was treated as absent")
	}
	if f.ran("mkfs.btrfs") {
		t.Error("...and it formatted the volume")
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
