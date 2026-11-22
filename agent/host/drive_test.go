package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"briard.io/agent/files"
	"briard.io/agent/quadlet"
	"briard.io/shared/api"
	"briard.io/shared/manifest"
)

// driveCfg is a node whose pet state is dir.
func driveCfg(dir string) Config {
	return Config{AssignmentCache: filepath.Join(dir, "assignment.json")}
}

// TestTheDrivesPasswordIsMintedOnce: the first ask mints and records it, and every later one
// reads the record, across a restart too (a second Config over the same directory).
func TestTheDrivesPasswordIsMintedOnce(t *testing.T) {
	dir := t.TempDir()
	first, err := driveCfg(dir).driveKey()
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != driveKeyLen || strings.Trim(first, "0123456789abcdef") != "" {
		t.Fatalf("the password is %q, want %d hex characters", first, driveKeyLen)
	}
	second, err := driveCfg(dir).driveKey()
	if err != nil || second != first {
		t.Fatalf("a second ask gave %q, %v; want the first, %q", second, err, first)
	}
	other, err := driveCfg(t.TempDir()).driveKey()
	if err != nil || other == first {
		t.Fatalf("another node minted %q, %v; want its own", other, err)
	}
}

// TestTheDrivesPasswordIsNotMintedOverOneThatCannotBeRead: a file that is there and unreadable
// is an error and stays as it is, because something may hold the password in it. A file that is
// read and holds no password is replaced.
func TestTheDrivesPasswordIsNotMintedOverOneThatCannotBeRead(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, driveKeyName)
	// A directory in the file's place: reading it fails with something other than "not there".
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if k, err := driveCfg(dir).driveKey(); err == nil {
		t.Fatalf("an unreadable record gave %q and no error", k)
	}
	if fi, err := os.Stat(path); err != nil || !fi.IsDir() {
		t.Fatal("the unreadable record was replaced")
	}

	dir = t.TempDir()
	path = filepath.Join(dir, driveKeyName)
	if err := os.WriteFile(path, []byte("not a password\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	k, err := driveCfg(dir).driveKey()
	if err != nil || len(k) != driveKeyLen {
		t.Fatalf("a record holding no password gave %q, %v; want a new one", k, err)
	}
	if raw, _ := os.ReadFile(path); strings.TrimSpace(string(raw)) != k {
		t.Fatalf("the new password was not recorded: %q", raw)
	}
}

// TestANodeWithNoPetStateHasNoDrivePassword: nowhere to keep one, so none, and no error.
func TestANodeWithNoPetStateHasNoDrivePassword(t *testing.T) {
	if k, err := (Config{}).driveKey(); k != "" || err != nil {
		t.Fatalf("got %q, %v; want none", k, err)
	}
}

type fakeDriveGuest struct {
	old  bool
	err  error
	told []string
}

func (f *fakeDriveGuest) SetDriveAccount(_ context.Context, password string) error {
	f.told = append(f.told, password)
	return f.err
}
func (f *fakeDriveGuest) SupportsDriveAccount() bool { return !f.old }

// TestTheGuestIsHandedTheDrivesPassword, and the password is in no log line. A password that
// cannot be had, a node with none, an older guest and a guest that refuses all leave the drive
// closed, never a failed bring-up (pushDriveAccount has no error to return).
func TestTheGuestIsHandedTheDrivesPassword(t *testing.T) {
	const password = "0123456789abcdef0123456789abcdef"
	var lines []string
	logf := func(f string, a ...any) { lines = append(lines, fmt.Sprintf(f, a...)) }
	have := func() (string, error) { return password, nil }

	g := &fakeDriveGuest{}
	pushDriveAccount(context.Background(), g, have, logf)
	if len(g.told) != 1 || g.told[0] != password {
		t.Fatalf("the guest was handed %v, want the password once", g.told)
	}
	for _, tc := range []struct {
		name  string
		g     *fakeDriveGuest
		key   func() (string, error)
		told  int
		quiet bool
	}{
		{"the password cannot be had", &fakeDriveGuest{}, func() (string, error) { return "", errors.New("disk") }, 0, false},
		{"a node with no pet state", &fakeDriveGuest{}, func() (string, error) { return "", nil }, 0, true},
		{"an older guest", &fakeDriveGuest{old: true}, have, 0, false},
		{"a guest that refuses", &fakeDriveGuest{err: errors.New("no")}, have, 1, false},
	} {
		before := len(lines)
		pushDriveAccount(context.Background(), tc.g, tc.key, logf)
		if len(tc.g.told) != tc.told {
			t.Errorf("%s: the guest was handed %d passwords, want %d", tc.name, len(tc.g.told), tc.told)
		}
		if said := len(lines) > before; said == tc.quiet {
			t.Errorf("%s: logged=%v, want quiet=%v", tc.name, said, tc.quiet)
		}
	}
	for _, l := range lines {
		if strings.Contains(l, password) {
			t.Errorf("the password is in a log line: %q", l)
		}
	}
}

// TestTheDriveIsUpOnlyWithAnAddressFilesWithSambaAndAPassword enumerates the three things the
// answer depends on. Anything missing is "not up", and a not-up answer carries no password.
func TestTheDriveIsUpOnlyWithAnAddressFilesWithSambaAndAPassword(t *testing.T) {
	const addr, password = "192.168.1.50", "0123456789abcdef0123456789abcdef"
	withSamba := []string{"briard-files-pod.service", "briard-files-app.service", driveUnit}
	browserOnly := []string{"briard-files-pod.service", "briard-files-app.service"}
	for _, tc := range []struct {
		name     string
		addr     string
		units    []string
		password string
		up       bool
	}{
		{"everything there", addr, withSamba, password, true},
		{"the guest holds no address", "", withSamba, password, false},
		{"Files is not installed", addr, nil, password, false},
		{"Files runs no Samba", addr, browserOnly, password, false},
		{"no password", addr, withSamba, "", false},
		{"nothing at all", "", nil, "", false},
	} {
		got := driveAnswer(tc.addr, tc.units, tc.password)
		if got.Up != tc.up {
			t.Errorf("%s: up=%v, want %v", tc.name, got.Up, tc.up)
		}
		if !tc.up && got != (LocalDrive{}) {
			t.Errorf("%s: a drive that is not up answered %+v, want nothing", tc.name, got)
		}
		if tc.up && (got.Address != addr || got.Share != files.DriveShare || got.User != files.DriveUser || got.Password != password) {
			t.Errorf("%s: answered %+v", tc.name, got)
		}
	}
}

// TestTheDriveUnitIsTheOneTheRendererNames: the host recognises a Files that runs Samba by its
// container's unit, so the name here has to be the one a rendered manifest carries.
func TestTheDriveUnitIsTheOneTheRendererNames(t *testing.T) {
	m := manifest.Manifest{Name: files.Name, Version: "2.0.2-4.25.1", Network: "private", Ports: []int{files.DrivePort},
		Containers: []manifest.Container{
			{Name: "app", Image: "docker.io/gtstef/filebrowser@sha256:1111111111111111111111111111111111111111111111111111111111111111", Primary: true, Port: 80, HealthPath: "/health"},
			{Name: files.DriveContainer, Image: "ghcr.io/briardhq/catalog/samba@sha256:2222222222222222222222222222222222222222222222222222222222222222"},
		}}
	r, err := quadlet.Render(m, "192.168.1.50")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(r.Units, driveUnit) {
		t.Fatalf("the renderer names no %s: %v", driveUnit, r.Units)
	}
}

// TestTheDrivesAnswerLeavesByThisMachinesSocketAlone: the answer carries a password, so the
// cloud may not ask for it and neither may anything in the guest, the dashboard included.
func TestTheDrivesAnswerLeavesByThisMachinesSocketAlone(t *testing.T) {
	if !localOnlyKinds[api.DirectiveDrive] {
		t.Error("the cloud's down-channel may carry the drive directive")
	}
	if guestMayAsk(api.Directive{Kind: api.DirectiveDrive}) {
		t.Error("the guest's admin port may ask for the drive's password")
	}
}

// TestApplyDriveAnswersFromWhatTheHostHolds: no address held, so not up, and the password stays
// home; the answer is a LocalDrive either way.
func TestApplyDriveAnswersFromWhatTheHostHolds(t *testing.T) {
	cfg := driveCfg(t.TempDir())
	o := cfg.applyDrive(api.Directive{ID: "d1", Kind: api.DirectiveDrive}, func(string, ...any) {})
	if o.State != api.OutcomeDone || o.ID != "d1" {
		t.Fatalf("outcome %+v", o)
	}
	var got LocalDrive
	if err := json.Unmarshal([]byte(o.Detail), &got); err != nil || got != (LocalDrive{}) {
		t.Fatalf("a node whose guest holds no address answered %q (%v)", o.Detail, err)
	}
	if key, _ := cfg.driveKey(); key == "" || strings.Contains(o.Detail, key) {
		t.Fatalf("the password is in a not-up answer, or was never minted: %q", o.Detail)
	}
}
