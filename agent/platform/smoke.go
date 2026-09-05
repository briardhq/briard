package platform

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// smokeTimeout bounds the whole smoke test -- its own hard limit, deliberately well under the
// agent unit's TimeoutStartSec (60s). If a staged qemu hangs and the test merely waited,
// systemd would kill the start: the revert would still happen, which is the right outcome, but
// the agent would be killed before it could write down WHY, and nothing would ever report.
var smokeTimeout = 20 * time.Second

// smokeDevices are the device models the guest launch depends on (qemuArgs); a bundle built
// without one of them execs fine and fails only at the next guest launch, which is exactly the
// diagnosis surface the smoke test exists to remove.
var smokeDevices = []string{"virtio-blk-pci", "virtio-net-pci", "virtio-serial-pci", "virtserialport"}

// SmokeTest proves a STAGED qemu tree runs on this host, before the release that brought it
// commits ([B.86b]). Two tiers. First, without a VM and in milliseconds: `-version` proves the
// binary execs and its libraries resolve; `-device help` proves it carries the device models we
// depend on; `-M help` proves the default machine type exists. Then a scratch machine -- the
// requested accelerator and CPU model, a throwaway raw disk on virtio-blk, a virtio NIC on a
// hub port (no tap: a second guest cannot share the live one's macvtap fd, and manufacturing
// scratch taps is more machinery than this earns), virtio-serial, the tree's own firmware dir --
// polled over QMP until it reports `running`, then killed. Reaching `running` is machine init
// done: accelerator up, firmware found under -L, every device model instantiated. That is the
// first sign of life, and all this needs.
//
// It answers "does release N work on THIS host" (a missing lib, an old glibc, an absent device
// model), not "is this qemu good" -- so an error here is a reason to refuse the release, and the
// caller does exactly that.
func SmokeTest(ctx context.Context, tree, accel, cpu string, logf func(string, ...any)) error {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	ctx, cancel := context.WithTimeout(ctx, smokeTimeout)
	defer cancel()
	bin := filepath.Join(tree, "bin", "qemu-system-x86_64")
	run := smokeRunner(tree, bin)

	out, err := run(ctx, "-version")
	if err != nil {
		return fmt.Errorf("qemu smoke test: -version: %w", err)
	}
	if !strings.Contains(out, "QEMU emulator") {
		return fmt.Errorf("qemu smoke test: -version printed %q", firstLine(out))
	}
	logf("qemu smoke test: %s", firstLine(out))
	out, err = run(ctx, "-device", "help")
	if err != nil {
		return fmt.Errorf("qemu smoke test: -device help: %w", err)
	}
	for _, dev := range smokeDevices {
		if !strings.Contains(out, `name "`+dev+`"`) {
			return fmt.Errorf("qemu smoke test: this qemu has no %s device model", dev)
		}
	}
	out, err = run(ctx, "-M", "help")
	if err != nil {
		return fmt.Errorf("qemu smoke test: -M help: %w", err)
	}
	if !strings.Contains("\n"+out, "\npc ") {
		return fmt.Errorf("qemu smoke test: this qemu has no `pc` machine type")
	}

	dir, err := os.MkdirTemp("", "briard-qemu-smoke-")
	if err != nil {
		return fmt.Errorf("qemu smoke test: scratch dir: %w", err)
	}
	defer os.RemoveAll(dir)
	scratch := filepath.Join(dir, "scratch.img")
	f, err := os.Create(scratch)
	if err != nil {
		return fmt.Errorf("qemu smoke test: scratch disk: %w", err)
	}
	if err := f.Truncate(8 << 20); err != nil {
		f.Close()
		return fmt.Errorf("qemu smoke test: scratch disk: %w", err)
	}
	f.Close()
	qmp := filepath.Join(dir, "qmp.sock")
	args := []string{"-machine", "accel=" + accel}
	if cpu != "" {
		args = append(args, "-cpu", cpu)
	}
	args = append(args,
		"-m", "128", "-smp", "1", "-no-reboot", "-display", "none", "-serial", "none", "-monitor", "none",
		"-L", filepath.Join(tree, "share", "qemu"),
		"-drive", "file="+scratch+",format=raw,if=virtio",
		"-netdev", "hubport,id=smoke0,hubid=0", "-device", "virtio-net-pci,netdev=smoke0",
		"-device", "virtio-serial-pci",
		"-qmp", "unix:"+qmp+",server=on,wait=off",
	)
	cmd := smokeCommand(ctx, tree, bin, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	// A killed machine must not keep Wait hanging on the stderr pipe (a child it forked could
	// still hold it); the loader runs qemu in-process, so this is defence, not the design.
	cmd.WaitDelay = time.Second
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("qemu smoke test: start scratch machine: %w", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	exited := false
	defer func() {
		if !exited {
			_ = cmd.Process.Kill()
			<-done
		}
	}()
	for {
		select {
		case err := <-done:
			exited = true
			return fmt.Errorf("qemu smoke test: scratch machine exited before it ran (%v): %s", err, firstLine(stderr.String()))
		case <-ctx.Done():
			return fmt.Errorf("qemu smoke test: scratch machine did not reach running within %s: %s", smokeTimeout, firstLine(stderr.String()))
		case <-time.After(50 * time.Millisecond):
		}
		if _, err := os.Stat(qmp); err != nil {
			continue
		}
		raw, err := qmpExecute(ctx, qmp, "query-status", nil)
		if err != nil {
			continue // the socket exists before the monitor answers; keep polling
		}
		var st struct {
			Status  string `json:"status"`
			Running bool   `json:"running"`
		}
		if err := json.Unmarshal(raw, &st); err != nil {
			return fmt.Errorf("qemu smoke test: query-status returned %s", raw)
		}
		if st.Running {
			logf("qemu smoke test: scratch machine running (accel=%q cpu=%q); killing it", accel, cpu)
			return nil
		}
		switch st.Status {
		case "internal-error", "guest-panicked", "shutdown", "io-error":
			return fmt.Errorf("qemu smoke test: scratch machine is %s: %s", st.Status, firstLine(stderr.String()))
		}
	}
}

// smokeRunner returns a bounded run-and-capture of the tree's qemu (tier 1).
func smokeRunner(tree, bin string) func(ctx context.Context, args ...string) (string, error) {
	return func(ctx context.Context, args ...string) (string, error) {
		out, err := smokeCommand(ctx, tree, bin, args...).CombinedOutput()
		if err != nil {
			return string(out), fmt.Errorf("%w: %s", err, firstLine(string(out)))
		}
		return string(out), nil
	}
}

// smokeCommand execs the tree's qemu THROUGH THE TREE'S OWN DYNAMIC LOADER. The bundle bakes
// its ELF interpreter to the COMMITTED prefix (/opt/briard/qemu/lib/ld-linux..., see
// qemu-bundle.nix), so exec'ing the staged binary directly would pair the new libc with the
// old loader -- a glibc bump between releases fails that pairing, and the test would refuse a
// good release for a reason that vanishes the moment the link moves. Invoking ld.so by path
// bypasses PT_INTERP and keeps every byte the test runs inside the tree under test; the binary's
// own $ORIGIN RPATH resolves within it too. A tree without a loader (nothing we ship, but a
// test's stand-in) runs direct.
func smokeCommand(ctx context.Context, tree, bin string, args ...string) *exec.Cmd {
	lib := filepath.Join(tree, "lib")
	if loaders, _ := filepath.Glob(filepath.Join(lib, "ld-linux*.so*")); len(loaders) > 0 {
		return exec.CommandContext(ctx, loaders[0], append([]string{"--library-path", lib, bin}, args...)...)
	}
	return exec.CommandContext(ctx, bin, args...)
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return s
}
