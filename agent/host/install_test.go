package host

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// stagingDir builds a flat local staging dir, the BRIARD_ARTIFACTS shape the install rigs hand
// the installer: loose binaries and units, the qemu bundle as a read-only directory (a Nix store
// path is), the guest bundle as a plain tarball, the image. Every name in skip is left out.
func stagingDir(t *testing.T, skip ...string) string {
	t.Helper()
	src := t.TempDir()
	write := func(rel, body string, mode os.FileMode) {
		p := filepath.Join(src, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), mode); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{"briard-agent", "briard-net-wrap", "briard-update",
		"briard-agent.service", "briard-update.service", "briard-update.timer", "nixos.qcow2"} {
		if !slices.Contains(skip, f) {
			write(f, f+" bytes", 0o555)
		}
	}
	write("qemu/bin/qemu-system-x86_64", "qemu", 0o555)
	if err := os.Chmod(filepath.Join(src, "qemu", "bin"), 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(filepath.Join(src, "qemu", "bin"), 0o755) })
	bundle := t.TempDir()
	if err := os.MkdirAll(filepath.Join(bundle, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundle, "bin", "briard-guest-agent"), []byte("guest"), 0o555); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("tar", "-cf", filepath.Join(src, "guest-bundle.tar"), "-C", bundle, ".").CombinedOutput(); err != nil {
		t.Fatalf("tar: %v: %s", err, out)
	}
	return src
}

func testRoots(t *testing.T) installRoots {
	d := t.TempDir()
	return installRoots{prefix: filepath.Join(d, "opt"), state: filepath.Join(d, "var"), run: filepath.Join(d, "run"),
		unitDir: filepath.Join(d, "units"), cliLink: filepath.Join(d, "bin", "briard")}
}

// THE TREE THE LOCAL STAGING PATH LAYS DOWN, file by file: what a node's first start reads, where
// install.sh used to put it. The proof against the shipped installer is the install rigs; this
// pins the shape so a regression is a unit-test failure first.
func TestInstallLaysTheStagingDirDown(t *testing.T) {
	t.Setenv("SUDO_USER", "")
	src, r := stagingDir(t), testRoots(t)
	for _, d := range []string{filepath.Join(r.prefix, "agent"), r.state} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.layDown(context.Background(), src, src, quiet); err != nil {
		t.Fatalf("layDown: %v", err)
	}
	files := map[string]os.FileMode{
		"opt/agent/briard-agent":                 0o755,
		"opt/agent/briard-net-wrap":              0o755,
		"opt/agent/briard-update":                0o755,
		"units/briard-agent.service":             0o644,
		"units/briard-update.service":            0o644,
		"units/briard-update.timer":              0o644,
		"opt/config.env":                         0o600,
		"opt/qemu/bin/qemu-system-x86_64":        0, // through both links
		"opt/agent/guest/bin/briard-guest-agent": 0,
		"opt/guest-image/nixos.qcow2":            0,
	}
	base := filepath.Dir(r.prefix)
	for rel, mode := range files {
		fi, err := os.Stat(filepath.Join(base, rel))
		if err != nil {
			t.Errorf("%s: %v", rel, err)
			continue
		}
		if mode != 0 && fi.Mode().Perm() != mode {
			t.Errorf("%s is %v, want %v", rel, fi.Mode().Perm(), mode)
		}
	}
	links := map[string]string{
		"opt/agent/qemu":  "qemu-install",
		"opt/qemu":        "agent/qemu",
		"opt/agent/guest": "guest-install",
		"bin/briard":      filepath.Join(r.prefix, "agent", "briard-agent"),
	}
	for rel, want := range links {
		if got, err := os.Readlink(filepath.Join(base, rel)); err != nil || got != want {
			t.Errorf("%s -> %q (%v), want -> %q", rel, got, err, want)
		}
	}
	// The trees are made writable (the staging dir's are a read-only store path), so the next
	// install's fresh copy -- and the agent's prune -- can delete them.
	if fi, err := os.Stat(filepath.Join(r.prefix, "agent", "qemu-install", "bin")); err != nil || fi.Mode().Perm()&0o200 == 0 {
		t.Errorf("the qemu tree is not owner-writable: %v %v", fi, err)
	}
	// No manifest on the local path, so none is installed -- and no node-local guest record.
	for _, rel := range []string{"opt/agent/manifest.json", "opt/guest-image/manifest.json", "var/guest-release.json"} {
		if _, err := os.Stat(filepath.Join(base, rel)); err == nil {
			t.Errorf("%s exists, but the staging dir carried no manifest", rel)
		}
	}
	// A second install over the first replaces it whole: the reinstall path.
	if err := r.layDown(context.Background(), stagingDir(t), src, quiet); err != nil {
		t.Fatalf("layDown over an install: %v", err)
	}
}

// Refused, not skipped: a staging dir short of a unit, a frozen script or the launch wrapper is
// one this host cannot run briard from, and the error names what is missing.
func TestInstallRefusesAnIncompleteStagingDir(t *testing.T) {
	t.Setenv("SUDO_USER", "")
	for _, missing := range []string{"briard-net-wrap", "briard-update", "briard-update.timer"} {
		src, r := stagingDir(t, missing), testRoots(t)
		if err := os.MkdirAll(filepath.Join(r.prefix, "agent"), 0o755); err != nil {
			t.Fatal(err)
		}
		err := r.layDown(context.Background(), src, src, quiet)
		if err == nil || !strings.Contains(err.Error(), missing) {
			t.Errorf("without %s: %v, want a refusal naming it", missing, err)
		}
	}
}

// config.env: the paths the install laid down, then every operator BRIARD_* with the prefix
// stripped, sorted -- and none of the names that configure the install itself.
func TestInstallConfigCopiesTheOperatorsSettings(t *testing.T) {
	t.Setenv("SUDO_USER", "")
	for k, v := range map[string]string{
		"BRIARD_CPU": "qemu64", "BRIARD_VIP_ADDR": "192.168.1.100/24", "BRIARD_CHANNEL_URL": "http://ch",
		"BRIARD_RELEASE": "latest", "BRIARD_UNIT_DIR": "/run/x", "BRIARD_ARTIFACTS": "/nix/store/x", "BRIARD_CONFIG": "/x",
		"BRIARD_lower": "no",
	} {
		t.Setenv(k, v)
	}
	r := testRoots(t)
	if err := os.MkdirAll(r.prefix, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := r.writeConfig(context.Background(), r.bundleLines("/w"), quiet); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(r.prefix, "config.env"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	for _, want := range []string{
		"\nQEMU=" + r.prefix + "/qemu/bin/qemu-system-x86_64\n",
		"\nNET_WRAP_BIN=/w\nGUEST_IMAGE=" + r.prefix + "/guest-image/nixos.qcow2\n",
		"\nDATA_DISK=" + r.state + "/data.img\nSTATE_DISK=" + r.state + "/state.img\n",
		"\nCHANNEL_URL=http://ch\nCPU=qemu64\n",
		"\nVIP_ADDR=192.168.1.100/24\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("config.env lacks %q:\n%s", want, got)
		}
	}
	for _, not := range []string{"RELEASE=", "UNIT_DIR=", "ARTIFACTS=", "CONFIG=", "lower", "BACKUP_DIR="} {
		if strings.Contains(got, "\n"+not) {
			t.Errorf("config.env carries %q:\n%s", not, got)
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
