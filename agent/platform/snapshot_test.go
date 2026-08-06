package platform

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// fakeQEMUImg plants a qemu-img script BESIDE a qemu-system binary in a temp dir and returns
// a spec pointing at it. The layout is the point: qemuImg() must find its sibling, because
// picking qemu-img off $PATH could pair the bundled qemu with a distro qemu-img of a
// different vintage. The script records its argv and exits with `code`.
func fakeQEMUImg(t *testing.T, stdout string, code int) (QEMUSpec, string) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("shell-script fake needs a POSIX shell")
	}
	dir := t.TempDir()
	argvLog := filepath.Join(dir, "argv")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + argvLog + "\n" +
		"cat <<'EOF'\n" + stdout + "\nEOF\n" +
		"exit " + strconv.Itoa(code) + "\n"
	if err := os.WriteFile(filepath.Join(dir, "qemu-img"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return QEMUSpec{
		Binary:    filepath.Join(dir, "qemu-system-x86_64"),
		DiskImage: "/var/lib/briard/guest.qcow2",
	}, argvLog
}

func readArgv(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("fake qemu-img was never invoked: %v", err)
	}
	return strings.TrimSpace(string(b))
}

func TestQEMUImgIsTheSiblingOfTheConfiguredQEMU(t *testing.T) {
	s := QEMUSpec{Binary: "/opt/briard/bin/qemu-system-x86_64"}
	if got, want := s.qemuImg(), "/opt/briard/bin/qemu-img"; got != want {
		t.Errorf("qemuImg() = %q, want %q (the bundle's own qemu-img, not $PATH's)", got, want)
	}
	// A bare binary name means "whatever is on $PATH" -- qemu-img gets the same treatment
	// rather than being resolved against the current working directory.
	bare := QEMUSpec{Binary: "qemu-system-x86_64"}
	if got, want := bare.qemuImg(), "qemu-img"; got != want {
		t.Errorf("qemuImg() = %q, want %q", got, want)
	}
}

func TestSnapshotVerbsRunTheRightCommand(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(QEMUSpec) error
		want string
	}{
		{"create", func(s QEMUSpec) error { return s.SnapshotCreate(context.Background()) },
			"snapshot -c " + UpgradeSnapshot + " /var/lib/briard/guest.qcow2"},
		{"restore", func(s QEMUSpec) error { return s.SnapshotRestore(context.Background()) },
			"snapshot -a " + UpgradeSnapshot + " /var/lib/briard/guest.qcow2"},
		{"delete", func(s QEMUSpec) error { return s.SnapshotDelete(context.Background()) },
			"snapshot -d " + UpgradeSnapshot + " /var/lib/briard/guest.qcow2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, argv := fakeQEMUImg(t, "", 0)
			if err := tc.run(s); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			if got := readArgv(t, argv); got != tc.want {
				t.Errorf("argv = %q, want %q", got, tc.want)
			}
		})
	}
}

// A failing qemu-img must surface as an error carrying its output. Reverting a tag that does
// not exist is the case that matters: real qemu-img exits 1 there, and a rollback that
// silently "succeeded" without restoring anything is the worst possible outcome on this path.
func TestSnapshotRestoreSurfacesFailure(t *testing.T) {
	s, _ := fakeQEMUImg(t, "qemu-img: Could not apply snapshot 'briard-preupgrade'", 1)
	err := s.SnapshotRestore(context.Background())
	if err == nil {
		t.Fatal("restore of a missing snapshot returned nil -- a rollback that restored nothing would read as success")
	}
	if !strings.Contains(err.Error(), "Could not apply snapshot") {
		t.Errorf("error %q does not carry qemu-img's own message", err)
	}
}

// Every verb refuses a spec with no disk rather than shelling out to qemu-img with an empty
// path, where it would act on the current directory's idea of a filename.
func TestSnapshotRequiresADisk(t *testing.T) {
	s := QEMUSpec{Binary: "/opt/briard/bin/qemu-system-x86_64"}
	if err := s.SnapshotCreate(context.Background()); err == nil {
		t.Error("SnapshotCreate with no DiskImage returned nil")
	}
	if _, err := s.SnapshotExists(context.Background()); err == nil {
		t.Error("SnapshotExists with no DiskImage returned nil")
	}
}

func TestSnapshotExists(t *testing.T) {
	// Real `qemu-img snapshot -l` output, kept verbatim so the parser is tested against the
	// format it actually meets rather than an idealised one.
	const listing = `Snapshot list:
ID      TAG               VM_SIZE                DATE        VM_CLOCK     ICOUNT
1       briard-preupgrade     0 B 2026-07-31 23:01:32  0000:00:00.000          0`

	s, argv := fakeQEMUImg(t, listing, 0)
	ok, err := s.SnapshotExists(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Error("SnapshotExists = false with the rollback point listed")
	}
	if got, want := readArgv(t, argv), "snapshot -l /var/lib/briard/guest.qcow2"; got != want {
		t.Errorf("argv = %q, want %q", got, want)
	}

	// An image with no snapshots prints nothing at all -- the common case, and it must read
	// as absent rather than erroring.
	empty, _ := fakeQEMUImg(t, "", 0)
	if ok, err := empty.SnapshotExists(context.Background()); err != nil || ok {
		t.Errorf("SnapshotExists on an empty listing = (%v, %v), want (false, nil)", ok, err)
	}
}

func TestHasSnapshotTagMatchesTheTagColumn(t *testing.T) {
	for _, tc := range []struct {
		name    string
		listing string
		want    bool
	}{
		{"present", "ID  TAG  VM_SIZE\n1   briard-preupgrade  0 B", true},
		{"absent", "ID  TAG  VM_SIZE\n1   someone-elses-snap  0 B", false},
		{"empty listing", "", false},
		// The tag must be matched in the TAG column, not anywhere in the blob: a snapshot
		// merely NAMED after ours (or a date/description mentioning it) is not ours.
		{"substring elsewhere", "ID  TAG  VM_SIZE\n1   other  0 B briard-preupgrade", false},
		{"prefix is not a match", "ID  TAG  VM_SIZE\n1   briard-preupgrade-old  0 B", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := hasSnapshotTag(tc.listing, UpgradeSnapshot); got != tc.want {
				t.Errorf("hasSnapshotTag(%q) = %v, want %v", tc.listing, got, tc.want)
			}
		})
	}
}
