package files

import (
	"context"
	_ "embed"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
	"unicode/utf16"

	"briard.io/shared/manifest"
)

// The drive: the same view Files shows in a browser, served over SMB by a Samba container in
// Files' pod, so the machine Briard runs on can have the household's files as a drive.
//
// It is a second container and not a second app, because what it serves is the view, and Files
// is the one app licensed to read other apps' areas. Mounting the view once more gives the drive
// everything the browser has: one folder per app, read-only where the tier is, re-bound by the
// guest agent after a restore.
//
// Every session needs a password, there is no guest access, and nothing announces the share. The
// one account is the host's own: a machine credential the host agent mints and hands the guest
// (SetDriveAccount), which no person is shown.

// DriveContainer is the name the catalog gives the Samba container in Files' pod.
const DriveContainer = "samba"

// DriveDir is Samba's directory on the guest, mounted read-only at /briard in its container: its
// configuration, the system users its accounts map to, and the accounts. Apart from Dir, so the
// signing secret the browser holds is not in the drive's container. /run, so every boot
// re-derives it.
const DriveDir = "/run/briard/files-drive"

// driveMount is where Samba finds DriveDir, the path its configuration names.
const driveMount = "/briard"

// DriveShare is the share's name, which a desktop shows beside the drive. DrivePort is where
// Samba listens, the port the catalog publishes.
const (
	DriveShare = "Briard"
	DrivePort  = 445
)

// DriveUser is the host's account, and driveUID the system user Samba maps it to. The user
// exists only in the rendered passwd; the share forces every write to root, as Files writes.
const (
	DriveUser = "host"
	driveUID  = 1001
)

const (
	driveConfPath     = DriveDir + "/smb.conf"
	drivePasswdPath   = DriveDir + "/passwd"
	driveAccountsPath = DriveDir + "/smbpasswd"
)

//go:embed smb.conf
var driveConfSource string

// usersToken is the placeholder smb.conf carries for the accounts the share admits.
const usersToken = "@USERS@"

// hasDrive reports whether this version of Files runs the Samba container.
func hasDrive(m manifest.Manifest) bool {
	for _, c := range m.Containers {
		if c.Name == DriveContainer {
			return true
		}
	}
	return false
}

// prepareDrive materialises Samba's directory before its container starts: the configuration
// and the users. The accounts file is the host's to hand over (SetDriveAccount) and is never
// written here, so the two cannot write over each other; without one Samba starts and admits
// nobody.
func prepareDrive(ctx context.Context, x Executor) error {
	if _, err := x.Run(ctx, "mkdir", "-p", DriveDir); err != nil {
		return fmt.Errorf("files: %s: %w", DriveDir, err)
	}
	if strings.Count(driveConfSource, usersToken) != 1 {
		return fmt.Errorf("files: the drive's config template must carry %s exactly once", usersToken)
	}
	conf := strings.Replace(driveConfSource, usersToken, DriveUser, 1)
	if err := write(ctx, x, driveConfPath, []byte(conf), "0644"); err != nil {
		return err
	}
	// Samba maps every account to a system user and refuses a logon whose user is missing;
	// `nobody` it looks up at start, guest access or not.
	passwd := fmt.Sprintf("root:x:0:0:root:/:/noshell\n%s:x:%d:0::/:/noshell\nnobody:x:65534:65534::/:/noshell\n", DriveUser, driveUID)
	return write(ctx, x, drivePasswdPath, []byte(passwd), "0644")
}

// SetDriveAccount makes password the one that opens the drive, for the host's account. Samba
// reads the file at each logon, so a running server takes the change with no restart, and the
// password itself is kept nowhere on the guest: the file holds its hash.
//
// It does not wait for Files: a host hands the password over at every bring-up, whatever is
// installed, and the directory it lands in is there for a Files installed later.
func SetDriveAccount(ctx context.Context, x Executor, password string, now time.Time) error {
	if password == "" {
		return fmt.Errorf("files: the drive's password is empty")
	}
	if _, err := x.Run(ctx, "mkdir", "-p", DriveDir); err != nil {
		return fmt.Errorf("files: %s: %w", DriveDir, err)
	}
	// One smbpasswd line: name, uid, the LAN Manager hash (none: 32 X), the NT hash, the flags
	// and the time the password was set. Samba refuses the logon with "password must change"
	// unless the flags say a user account (U) whose password does not expire (X) and the time
	// is a real one; the flags field is thirteen characters wide between its brackets.
	line := fmt.Sprintf("%s:%d:%s:%s:[UX         ]:LCT-%08X:\n",
		DriveUser, driveUID, strings.Repeat("X", 32), ntHash(password), now.Unix())
	// 0600: Samba will not read an accounts file anyone else can.
	return write(ctx, x, driveAccountsPath, []byte(line), "0600")
}

// ntHash is the form Samba keeps a password in: MD4 over its UTF-16LE encoding, in upper-case
// hex.
func ntHash(password string) string {
	units := utf16.Encode([]rune(password))
	buf := make([]byte, 0, 2*len(units))
	for _, u := range units {
		buf = append(buf, byte(u), byte(u>>8))
	}
	sum := md4(buf)
	return strings.ToUpper(hex.EncodeToString(sum[:]))
}
