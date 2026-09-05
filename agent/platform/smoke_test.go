package platform

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A stand-in qemu tree: bin/qemu-system-x86_64 is a shell script whose tier-1 answers are
// given, and whose "boot" runs the given shell fragment. lib/ and share/qemu exist and are
// empty unless a test fills them.
func fakeTree(t *testing.T, version, devices, machines, boot string) string {
	t.Helper()
	tree := filepath.Join(t.TempDir(), "qemu-v1")
	for _, d := range []string{"bin", "lib", "share/qemu"} {
		if err := os.MkdirAll(filepath.Join(tree, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	script := "#!/bin/sh\n" +
		"case \"$1\" in\n" +
		"-version) printf '%s\\n' '" + version + "'; exit 0;;\n" +
		"-device) printf '%s\\n' '" + devices + "'; exit 0;;\n" +
		"-M) printf '%s\\n' '" + machines + "'; exit 0;;\n" +
		"esac\n" + boot + "\n"
	if err := os.WriteFile(filepath.Join(tree, "bin", "qemu-system-x86_64"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return tree
}

const (
	goodVersion  = "QEMU emulator version 10.2.4"
	goodDevices  = `name "virtio-blk-pci", bus PCI\nname "virtio-net-pci", bus PCI\nname "virtio-serial-pci", bus PCI\nname "virtserialport", bus virtio-serial-bus`
	goodMachines = "Supported machines are:\nmicrovm  microvm\npc  Standard PC (i440FX + PIIX, 1996) (alias of pc-i440fx-10.2) (default)\nq35  Standard PC (Q35 + ICH9, 2009)"
)

func shortSmoke(t *testing.T) {
	t.Helper()
	old := smokeTimeout
	smokeTimeout = 2 * time.Second
	t.Cleanup(func() { smokeTimeout = old })
}

// Tier 1 refusals, each a way a release can fail to run on a host without a VM ever starting:
// a binary that will not exec, one that carries no virtio device model, one with no `pc`
// machine. [[verification-assertions-must-fail]]: the test refuses for the stated reason.
func TestSmokeTestRefusesABrokenTier1(t *testing.T) {
	shortSmoke(t)
	for _, tc := range []struct{ name, version, devices, machines, want string }{
		{"exits non-zero", "", "", "", "-version"},
		{"not qemu", "something else 1.0", goodDevices, goodMachines, "-version printed"},
		{"missing device model", goodVersion, `name "virtio-blk-pci"`, goodMachines, "no virtio-net-pci device model"},
		{"missing machine type", goodVersion, goodDevices, "Supported machines are:\nq35  Standard PC", "no `pc` machine type"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tree := fakeTree(t, tc.version, tc.devices, tc.machines, "exit 0")
			if tc.version == "" {
				os.WriteFile(filepath.Join(tree, "bin", "qemu-system-x86_64"), []byte("#!/bin/sh\nexit 7\n"), 0o755)
			}
			err := SmokeTest(context.Background(), tree, "tcg", "", t.Logf)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want one mentioning %q", err, tc.want)
			}
		})
	}
}

// Tier 2 refusals: a machine that dies before it runs, and one that hangs without ever
// serving its monitor -- the hang is caught by the test's OWN timeout (so the agent can still
// write the failure down), not by systemd's.
func TestSmokeTestRefusesAMachineThatDiesOrHangs(t *testing.T) {
	shortSmoke(t)
	dies := fakeTree(t, goodVersion, goodDevices, goodMachines, "echo 'qemu-system-x86_64: -L: firmware missing' >&2; exit 1")
	err := SmokeTest(context.Background(), dies, "tcg", "max", t.Logf)
	if err == nil || !strings.Contains(err.Error(), "exited before it ran") || !strings.Contains(err.Error(), "firmware missing") {
		t.Fatalf("dying machine: err = %v", err)
	}
	hangs := fakeTree(t, goodVersion, goodDevices, goodMachines, "exec sleep 30")
	start := time.Now()
	err = SmokeTest(context.Background(), hangs, "tcg", "", t.Logf)
	if err == nil || !strings.Contains(err.Error(), "did not reach running") {
		t.Fatalf("hanging machine: err = %v", err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("the hang was not cut off by the smoke timeout (took %s)", took)
	}
	// The hung process was killed, not left behind holding the scratch dir.
	if out, _ := exec.Command("pgrep", "-f", "sleep 30").Output(); strings.TrimSpace(string(out)) != "" {
		t.Errorf("the hung scratch machine is still running: pids %s", out)
	}
}

// The staged tree's OWN loader is what runs its qemu: the bundle bakes its interpreter to the
// committed prefix, so a direct exec would pair the new libc with the old loader. Prove the
// bypass by giving the tree a "loader" that records how it was invoked.
func TestSmokeTestRunsThroughTheTreesOwnLoader(t *testing.T) {
	shortSmoke(t)
	tree := fakeTree(t, goodVersion, goodDevices, goodMachines, "exit 1")
	record := filepath.Join(t.TempDir(), "loader.argv")
	loader := "#!/bin/sh\nprintf '%s\\n' \"$@\" >> " + record + "\n[ \"$1\" = --library-path ] && shift 2\nexec \"$@\"\n"
	if err := os.WriteFile(filepath.Join(tree, "lib", "ld-linux-x86-64.so.2"), []byte(loader), 0o755); err != nil {
		t.Fatal(err)
	}
	_ = SmokeTest(context.Background(), tree, "tcg", "", t.Logf) // tier 2 fails on purpose; tier 1 must have run
	b, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("the loader was never invoked: %v", err)
	}
	got := string(b)
	for _, want := range []string{"--library-path\n" + filepath.Join(tree, "lib"), filepath.Join(tree, "bin", "qemu-system-x86_64") + "\n-version"} {
		if !strings.Contains(got, want) {
			t.Errorf("loader argv missing %q:\n%s", want, got)
		}
	}
}

// The positive: a REAL qemu on PATH boots the scratch machine to `running` and is killed.
// Skipped where no qemu is installed; install-macvtap.nix proves this on the shipped bundle.
func TestSmokeTestPassesARealQEMU(t *testing.T) {
	real, err := exec.LookPath("qemu-system-x86_64")
	if err != nil {
		t.Skip("no qemu-system-x86_64 on PATH")
	}
	real, _ = filepath.EvalSymlinks(real)
	// qemu's compiled-in datadir: `-L help` lists the search path; the first existing dir wins.
	out, _ := exec.Command(real, "-L", "help").Output()
	datadir := ""
	for l := range strings.SplitSeq(string(out), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			if _, err := os.Stat(filepath.Join(l, "bios-256k.bin")); err == nil {
				datadir = l
				break
			}
		}
	}
	if datadir == "" {
		t.Skip("could not locate qemu's firmware dir")
	}
	tree := filepath.Join(t.TempDir(), "qemu-real")
	os.MkdirAll(filepath.Join(tree, "bin"), 0o755)
	os.MkdirAll(filepath.Join(tree, "share"), 0o755)
	os.Symlink(real, filepath.Join(tree, "bin", "qemu-system-x86_64"))
	os.Symlink(datadir, filepath.Join(tree, "share", "qemu"))
	if err := SmokeTest(context.Background(), tree, "kvm:tcg", "max", t.Logf); err != nil {
		t.Fatalf("a real qemu failed the smoke test: %v", err)
	}
	if out, _ := exec.Command("pgrep", "-f", "briard-qemu-smoke-").Output(); strings.TrimSpace(string(out)) != "" {
		t.Errorf("the scratch machine outlived the test: pids %s", out)
	}
}
