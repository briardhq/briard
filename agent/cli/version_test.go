package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"briard.io/agent/host"
)

// THE PIN: `version` reads the VM record the AGENT writes, so the two must name the same file.
func TestGuestReleaseRecordMatchesTheAgents(t *testing.T) {
	os.Unsetenv("GUEST_RELEASE_CACHE")
	t.Setenv("BRIARD_CONFIG", filepath.Join(t.TempDir(), "no-such-config.env"))
	if got := host.ConfigFromEnv().GuestReleaseCache; got != guestReleaseRecord {
		t.Errorf("the CLI reads %q while the agent writes %q", guestReleaseRecord, got)
	}
}

func TestVersionLines(t *testing.T) {
	dir := t.TempDir()
	rec := filepath.Join(dir, "guest-release.json")
	if err := os.WriteFile(rec, []byte(`{"chain":"vm","version":"vm.20260923.e8face8"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	defer func(v string) { Version = v }(Version)

	Version = "v3.20260928.fdb6289"
	if got, want := versionLines(rec), "briard v3.20260928.fdb6289\nvm     vm.20260923.e8face8\n"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}

	// An unstamped binary and a missing record each SAY so -- never a blank a reporter would drop.
	Version = ""
	got := versionLines(filepath.Join(dir, "absent.json"))
	for _, want := range []string{"development build", "vm     unknown (", "absent.json"} {
		if !strings.Contains(got, want) {
			t.Errorf("%q is missing %q", got, want)
		}
	}
}
