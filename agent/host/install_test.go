package host

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testRoots(t *testing.T) installRoots {
	d := t.TempDir()
	return installRoots{prefix: filepath.Join(d, "opt"), state: filepath.Join(d, "var"), run: filepath.Join(d, "run"),
		unitDir: filepath.Join(d, "units"), cliLink: filepath.Join(d, "bin", "briard")}
}

// A reinstall over a node keeps what the household changed with `briard config set`: a backup
// turned off stays off rather than coming back on in the default folder, and a static address
// stays rather than going back to DHCP. The environment still wins over the old file, and nothing
// else in it is carried: a setting the old install's environment gave is gone unless this one
// gives it again.
func TestReinstallKeepsWhatConfigSetChanged(t *testing.T) {
	t.Setenv("SUDO_USER", "")
	r := testRoots(t)
	if err := os.MkdirAll(r.prefix, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(r.prefix, "config.env")
	reinstall := func(old string) string {
		t.Helper()
		if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := r.writeConfig(context.Background(), r.bundleLines("/w"), quiet); err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	got := reinstall("GUEST_IMAGE=/old\nCPU=qemu64\n VIP_ADDR = 192.168.1.50/24\nBACKUP_DIR=\n")
	for _, want := range []string{"\nVIP_ADDR=192.168.1.50/24\n", "\nBACKUP_DIR=\n", "\nGUEST_IMAGE=" + filepath.Join(r.prefix, "guest-image", "nixos.qcow2") + "\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("config.env lacks %q:\n%s", want, got)
		}
	}
	for _, not := range []string{"CPU=", "GUEST_IMAGE=/old"} {
		if strings.Contains(got, "\n"+not) {
			t.Errorf("config.env carries %q over:\n%s", not, got)
		}
	}
	if n := strings.Count(got, "\nBACKUP_DIR="); n != 1 {
		t.Errorf("config.env has %d BACKUP_DIR lines:\n%s", n, got)
	}

	t.Setenv("BRIARD_VIP_ADDR", "10.0.0.9/24")
	t.Setenv("BRIARD_BACKUP_DIR", "/mnt/nas")
	got = reinstall("VIP_ADDR=192.168.1.50/24\nBACKUP_DIR=\n")
	for _, want := range []string{"\nVIP_ADDR=10.0.0.9/24\n", "\nBACKUP_DIR=/mnt/nas\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("the environment did not win: config.env lacks %q:\n%s", want, got)
		}
	}
	for _, k := range []string{"VIP_ADDR", "BACKUP_DIR"} {
		if n := strings.Count(got, "\n"+k+"="); n != 1 {
			t.Errorf("config.env has %d %s lines:\n%s", n, k, got)
		}
	}
}

// The closing ends on the agent's refusal, in its words, whatever else the journal holds.
func TestInstallWaitReadsTheRefusal(t *testing.T) {
	j := []byte("starting\nnetwork: refused: the guest was handed this host's own address\nprimary=true healthy=true\n")
	if got := refusedLine(j); got != "the guest was handed this host's own address" {
		t.Errorf("refusedLine = %q", got)
	}
	if got := refusedLine([]byte("primary=true healthy=true\n")); got != "" {
		t.Errorf("refusedLine on a healthy journal = %q", got)
	}
}
