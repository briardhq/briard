package host

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"briard.io/shared/atomicfile"
)

// The drive's password: what this host opens the household's files with, as a drive, over SMB
// (agent/files/drive.go has the server's side). A machine credential, minted here and shown to
// nobody: the host holds it, hands it to its guest at every bring-up, and uses it itself.
//
// Node-scoped and permanent. It is this host's, so it lives in this node's pet state and not
// on the replicated volume; and it is minted once, because whatever mounts the drive may keep
// the password, and a password that changed under it would close the drive.

// driveKeyName is the password's file in the node's pet state dir.
const driveKeyName = "drive-key"

// driveKeyLen is the password's length: sixteen random bytes in hex. Hex, so it survives every
// place a password is written into (a command line, a mount option) unquoted.
const driveKeyLen = 32

// driveKey is this host's drive password: the recorded one, or a new one recorded now. "" on a
// node with no pet state, which has nowhere to keep one.
//
// A new one is minted only when the file is known to be missing or holds something that is not
// a password. A file that could not be read is an error and is left alone: minting over it would
// change a password something already holds.
func (cfg Config) driveKey() (string, error) {
	dir := cfg.stateDir()
	if dir == "" {
		return "", nil
	}
	path := filepath.Join(dir, driveKeyName)
	raw, err := os.ReadFile(path)
	switch {
	case err == nil:
		if k := strings.TrimSpace(string(raw)); len(k) == driveKeyLen {
			if _, herr := hex.DecodeString(k); herr == nil {
				return k, nil
			}
		}
	case !errors.Is(err, fs.ErrNotExist):
		return "", fmt.Errorf("reading the drive's password: %w", err)
	}
	buf := make([]byte, driveKeyLen/2)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("minting the drive's password: %w", err)
	}
	k := hex.EncodeToString(buf)
	if err := atomicfile.Write(path, []byte(k+"\n"), 0o600, 0o700); err != nil {
		return "", fmt.Errorf("recording the drive's password: %w", err)
	}
	return k, nil
}

// driveAccountSetter is the slice of the guest the password's push costs.
type driveAccountSetter interface {
	SetDriveAccount(ctx context.Context, password string) error
	SupportsDriveAccount() bool
}

// pushDriveAccount hands the guest the drive's password at bring-up, a node-scoped fact the host
// holds and the guest keeps only for the boot it was pushed into. Pushed whether or not Files is
// installed, so a Files installed later finds its account in place. It warns and never fails:
// without it the drive admits nobody, worth a log line and not a node that will not come up.
// The password is never in what it logs.
func pushDriveAccount(ctx context.Context, g driveAccountSetter, key func() (string, error), logf func(string, ...any)) {
	password, err := key()
	switch {
	case err != nil:
		logf("drive: %v; the drive stays closed", err)
	case password == "":
		// No pet state: nothing to keep a password in, and nothing to say about it.
	case !g.SupportsDriveAccount():
		logf("drive: this guest cannot be handed the drive's password; the drive stays closed")
	default:
		if err := g.SetDriveAccount(ctx, password); err != nil {
			logf("drive: %v; the drive stays closed", err)
			return
		}
		logf("drive: the guest holds this host's account")
	}
}
