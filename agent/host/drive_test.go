package host

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
