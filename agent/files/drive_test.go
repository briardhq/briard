package files

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"briard.io/shared/manifest"
)

// withDrive is a version of Files whose pod runs the Samba container too.
func withDrive() manifest.Manifest {
	m := browser()
	m.Ports = []int{DrivePort}
	m.Containers = append(m.Containers, manifest.Container{
		Name:  DriveContainer,
		Image: "ghcr.io/briardhq/catalog/samba@sha256:2222222222222222222222222222222222222222222222222222222222222222",
	})
	return m
}

// A version with the Samba container gets its directory: the configuration naming the host's
// account in place of the placeholder, and the system users Samba maps accounts to.
func TestPrepareRendersTheDrive(t *testing.T) {
	x := &fake{}
	if err := Prepare(context.Background(), x, withDrive()); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	conf := x.files[driveConfPath]
	if !strings.Contains(conf, "valid users = "+DriveUser+"\n") || strings.Contains(conf, usersToken) {
		t.Fatalf("the drive's config does not name the host's account in place of the placeholder:\n%s", conf)
	}
	passwd := x.files[drivePasswdPath]
	for _, want := range []string{"root:x:0:0:", fmt.Sprintf("%s:x:%d:0:", DriveUser, driveUID), "nobody:x:65534:65534:"} {
		if !strings.Contains(passwd, want) {
			t.Errorf("the drive's passwd lacks %q:\n%s", want, passwd)
		}
	}
	if !slices.Contains(x.runs, "mkdir -p "+DriveDir) {
		t.Errorf("Prepare did not make %s: %v", DriveDir, x.runs)
	}
	if _, ok := x.files[driveAccountsPath]; ok {
		t.Error("Prepare wrote the accounts file, which is the host's to hand over")
	}
}

// The configuration is a template the constants have to agree with: the share's name, the view
// it serves, the port the catalog publishes, and the accounts file where the container finds it.
// And it holds what keeps the share closed: SMB3 at least, no guest, no anonymous listing.
func TestTheDrivesConfigAgreesWithTheConstants(t *testing.T) {
	for _, want := range []string{
		"[" + DriveShare + "]\n",
		"  path = " + ViewMount + "\n",
		fmt.Sprintf("  smb ports = %d\n", DrivePort),
		"  passdb backend = smbpasswd:" + driveMount + "/smbpasswd\n",
		"  server min protocol = SMB3\n",
		"  map to guest = never\n",
		"  restrict anonymous = 2\n",
	} {
		if !strings.Contains(driveConfSource, want) {
			t.Errorf("smb.conf lacks %q", want)
		}
	}
	if !strings.HasSuffix(driveAccountsPath, "/smbpasswd") || !strings.HasPrefix(driveAccountsPath, DriveDir+"/") {
		t.Errorf("the accounts file %s is not the one the config names under %s", driveAccountsPath, DriveDir)
	}
}

// A version without the Samba container gets no drive directory.
func TestPrepareMakesNoDriveWithoutItsContainer(t *testing.T) {
	x := &fake{}
	if err := Prepare(context.Background(), x, browser()); err != nil {
		t.Fatal(err)
	}
	for p := range x.files {
		if strings.HasPrefix(p, DriveDir) {
			t.Errorf("Prepare wrote %s for a version with no drive", p)
		}
	}
}

// The drive's container binds the view as the browser does, and its own directory read-only. The
// browser's binds are as they were, and no other container of the pod is handed either.
func TestVolumesForTheDrive(t *testing.T) {
	m := withDrive()
	view := ViewDir + ":" + ViewMount + ":rslave"
	if got := Volumes(m, m.Containers[0]); !slices.Equal(got, []string{view}) {
		t.Errorf("the browser's binds = %v, want the view alone", got)
	}
	if got := Volumes(m, m.Containers[1]); !slices.Equal(got, []string{view, DriveDir + ":/briard:ro"}) {
		t.Errorf("the drive's binds = %v, want the view and its directory read-only", got)
	}
	if got := Volumes(m, manifest.Container{Name: "something-else"}); got != nil {
		t.Errorf("another container of the pod was handed %v", got)
	}
}

// One account, in the line Samba reads: the name, the uid its passwd gives it, no LAN Manager
// hash, the NT hash, the flags thirteen wide, and the time the password was set. Root's alone,
// and installed by rename.
func TestSetDriveAccountWritesTheHostsLine(t *testing.T) {
	x := &fake{}
	now := time.Unix(0x6AF9B889, 0)
	if err := SetDriveAccount(context.Background(), x, "password", now); err != nil {
		t.Fatalf("SetDriveAccount: %v", err)
	}
	want := "host:1001:XXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXX:8846F7EAEE8FB117AD06BDD830B7586C:[UX         ]:LCT-6AF9B889:\n"
	if got := x.files[driveAccountsPath]; got != want {
		t.Fatalf("the accounts file:\n got %q\nwant %q", got, want)
	}
	for _, run := range []string{"mkdir -p " + DriveDir, "chmod 0600 " + driveAccountsPath + ".new"} {
		if !slices.Contains(x.runs, run) {
			t.Errorf("SetDriveAccount did not run %q: %v", run, x.runs)
		}
	}
	if strings.Contains(fmt.Sprint(x.files), "password") {
		t.Error("the password itself was written to the guest")
	}
}

// A converge after the host handed the password over leaves the account as it is, and a second
// password replaces the first.
func TestTheAccountOutlivesAConvergeAndIsReplaced(t *testing.T) {
	x := &fake{}
	ctx := context.Background()
	if err := SetDriveAccount(ctx, x, "first", time.Unix(1, 0)); err != nil {
		t.Fatal(err)
	}
	first := x.files[driveAccountsPath]
	if err := Prepare(ctx, x, withDrive()); err != nil {
		t.Fatal(err)
	}
	if x.files[driveAccountsPath] != first {
		t.Fatalf("a converge changed the account: %q -> %q", first, x.files[driveAccountsPath])
	}
	if err := SetDriveAccount(ctx, x, "second", time.Unix(2, 0)); err != nil {
		t.Fatal(err)
	}
	if got := x.files[driveAccountsPath]; got == first || strings.Count(got, "\n") != 1 {
		t.Fatalf("a second password did not replace the first: %q", got)
	}
}

// No password is no account: an empty one would be an account anybody opens.
func TestSetDriveAccountRefusesAnEmptyPassword(t *testing.T) {
	x := &fake{}
	if err := SetDriveAccount(context.Background(), x, "", time.Unix(1, 0)); err == nil {
		t.Fatal("an empty password was accepted")
	}
	if len(x.files) != 0 {
		t.Fatalf("an empty password wrote %v", x.files)
	}
}

// The NT hash against values from another implementation: the well-known one, lengths on each
// side of MD4's padding boundary (27, 28 and 32 characters are 54, 56 and 64 bytes), and
// characters outside ASCII.
func TestNTHash(t *testing.T) {
	for pw, want := range map[string]string{
		"password":              "8846F7EAEE8FB117AD06BDD830B7586C",
		strings.Repeat("q", 27): "7AFB092952D552580BF85E0B560FE116",
		strings.Repeat("q", 28): "ECB7052FA39B644FC1D9E6571420B7D0",
		strings.Repeat("q", 32): "9D72CC71E9F81B9658DA6C62A0BF4135",
		"Πάσσγουορντ":           "E1B4A750EE077236826302259F8B5EFB",
	} {
		if got := ntHash(pw); got != want {
			t.Errorf("ntHash(%q) = %s, want %s", pw, got, want)
		}
	}
}
