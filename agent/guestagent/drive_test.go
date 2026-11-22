package guestagent

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"briard.io/agent/files"
)

// TestTheDriveAccountVerbWritesTheHostsAccount: the verb is one the guest advertises, and what it
// is handed becomes the drive's one account, as a hash, root's alone, installed by rename.
func TestTheDriveAccountVerbWritesTheHostsAccount(t *testing.T) {
	if !slices.Contains(guestCapabilities, verbSetDriveAccount) {
		t.Fatalf("the guest does not advertise %s, so no host would send it", verbSetDriveAccount)
	}
	f := &fakeExec{files: map[string]string{}}
	payload, _ := json.Marshal(driveAccountRequest{Password: "a password the host minted"})
	if _, err := dispatch(f)(context.Background(), verbSetDriveAccount, payload); err != nil {
		t.Fatalf("%s: %v", verbSetDriveAccount, err)
	}
	accounts := files.DriveDir + "/smbpasswd"
	line := f.files[accounts+".new"] + f.files[accounts]
	if !strings.HasPrefix(line, files.DriveUser+":") || strings.Count(line, "\n") != 1 {
		t.Errorf("the accounts file does not hold the host's one line: %q", line)
	}
	if strings.Contains(line, "a password the host minted") {
		t.Error("the password itself was written to the guest")
	}
	if !f.ran("chmod", "0600", accounts+".new") || !f.ran("mv", "-f", accounts+".new", accounts) {
		t.Errorf("the accounts file was not made root's alone and installed by rename: %v", f.runs)
	}
}

// TestAnEmptyDrivePasswordIsRefused: an account with no password is one anybody opens.
func TestAnEmptyDrivePasswordIsRefused(t *testing.T) {
	f := &fakeExec{files: map[string]string{}}
	if _, err := dispatch(f)(context.Background(), verbSetDriveAccount, json.RawMessage(`{"password":""}`)); err == nil {
		t.Fatal("an empty password was accepted")
	}
	if len(f.files) != 0 {
		t.Errorf("an empty password wrote %v", f.files)
	}
}
