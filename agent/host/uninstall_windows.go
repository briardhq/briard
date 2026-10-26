package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"

	"briard.io/agent/nic"
	"briard.io/agent/platform"
	"briard.io/agent/selfupdate"
)

// What the Windows install made (install.ps1 and layDownOS), and so what goes:
//
//   - the agent's service and the warden's, the update task, and the resume task install.ps1
//     registers for the closing;
//   - the bridge, with its member's checksum offload put back from the record, the TAP adapter by
//     instance, the bridge's spanning-tree value, and the backup store's firewall rule;
//   - the tree under %ProgramData%\briard, its bin on the system PATH, and its entry in Installed
//     apps;
//   - the tray, running in its owner's session, and its Run value in that account's hive;
//   - state\ under it, the pet -- KEPT unless the operator asks for it to go.
//
// The tap-windows6 driver package goes only when install.ps1 installed it (state\tap-installed)
// and no other TAP adapter is left using it (removeTapDriver). The Hypervisor Platform stays:
// turning it off needs a restart, and it may be serving something else on this machine by now.

// deleteHint finishes the kept-data line with how to delete it by hand.
const deleteHint = "`Remove-Item -Recurse -Force '%s'`, in a terminal opened as administrator, deletes them for good"

// resumeTask is the task install.ps1 registers to run the closing at the next logon ($ResumeTask
// there). It deletes itself when it runs; an uninstall before that logon finds it still waiting.
const resumeTask = "briard-install"

// firewallRule is the backup store's inbound rule, by the name layDownOS gives it.
const firewallRule = "Briard backup store"

// serviceStopBound bounds the wait for the agent to stop: its own stop, with the SCM's rolling
// checkpoints, is seconds. The guest is not in it -- a stop leaves the guest running.
const serviceStopBound = 2 * time.Minute

func (cfg Config) removeServices(ctx context.Context, logf func(string, ...any)) error {
	endTrays() // its program is in the tree that goes
	// The node is down by now (Stop); what stopNode adds is the warden, which the agent's exit
	// leaves running, and a re-check of the rest.
	if err := stopNode(ctx, cfg.QMPSock, logf); err != nil {
		return err
	}
	// The guest is powered off by now; what RemoveUnit adds is the job's kill, for any process
	// still in it.
	if err := platform.RemoveUnit(platform.GuestUnit); err != nil {
		return err
	}
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("the service manager: %w", err)
	}
	defer m.Disconnect()
	for _, name := range []string{WardenName, ServiceName} {
		if s, err := m.OpenService(name); err == nil {
			err = s.Delete()
			s.Close()
			if err != nil {
				return fmt.Errorf("deleting the %s service: %w", name, err)
			}
		}
	}
	if err := run(ctx, "schtasks", "/delete", "/tn", selfupdate.DefaultUpdateUnit, "/f"); err != nil {
		logf("the update task: %v", err)
	}
	// Best-effort: absent is the common case, the closing having run and removed it.
	_ = run(ctx, "schtasks", "/delete", "/tn", resumeTask, "/f")
	return nil
}

// stopNode stops a running node for something that must not race it -- an uninstall, or a
// reinstall laying files over the ones in use -- and is `briard stop` with no agent to ask (Stop):
// the warden (its program is one of those files),
// then the agent's service (a stop leaves the guest running, for the next agent to re-adopt; here
// nothing must re-adopt it), then the guest's clean powerdown. A job kill is not a stop on Windows
// (unitStop), and the data volume a reinstall or a kept uninstall leaves behind is only worth
// keeping if the guest was not power-cut off it.
func stopNode(ctx context.Context, qmpSock string, logf func(string, ...any)) error {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("the service manager: %w", err)
	}
	defer m.Disconnect()
	if _, err := stopService(m, WardenName, logf); err != nil {
		return err
	}
	agent, err := stopService(m, ServiceName, logf)
	if err != nil {
		return err
	}
	if agent {
		logf("powering the guest off cleanly")
	}
	// What follows deletes or replaces files qemu holds, and PowerOffGuest returns once its
	// process has exited.
	return platform.PowerOffGuest(ctx, qmpSock, logf)
}

// stopService stops the named service and waits, bounded, until it has; it reports whether the
// service is registered at all.
func stopService(m *mgr.Mgr, name string, logf func(string, ...any)) (bool, error) {
	s, err := m.OpenService(name)
	if err != nil {
		return false, nil
	}
	defer s.Close()
	st, err := s.Query()
	if err != nil {
		return true, fmt.Errorf("asking the %s service its state: %w", name, err)
	}
	if st.State == svc.Stopped {
		return true, nil
	}
	logf("stopping the %s service", name)
	if st.State != svc.StopPending {
		if _, err := s.Control(svc.Stop); err != nil {
			return true, fmt.Errorf("stopping the %s service: %w", name, err)
		}
	}
	deadline := time.Now().Add(serviceStopBound)
	for st.State != svc.Stopped {
		if time.Now().After(deadline) {
			return true, fmt.Errorf("the %s service has not stopped %s after it was asked to", name, serviceStopBound)
		}
		time.Sleep(time.Second)
		if st, err = s.Query(); err != nil {
			return true, fmt.Errorf("asking the %s service its state: %w", name, err)
		}
	}
	return true, nil
}

func (cfg Config) removeNetwork(ctx context.Context, logf func(string, ...any)) error {
	var errs []error
	record := filepath.Join(cfg.stateDir(), bridgeRecord)
	var rec nic.Offload
	if b, err := os.ReadFile(record); err == nil {
		if err := json.Unmarshal(b, &rec); err != nil {
			return fmt.Errorf("reading %s: %w", record, err)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("reading %s: %w", record, err)
	}
	if err := nic.DestroyBridge(ctx, rec); err != nil {
		errs = append(errs, err)
	} else if err := os.Remove(record); err != nil && !errors.Is(err, fs.ErrNotExist) {
		// Given back: kept, it would describe a change no longer made.
		errs = append(errs, err)
	}
	if err := cfg.removeTapDriver(ctx, logf); err != nil {
		errs = append(errs, err)
	}
	if k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SYSTEM\CurrentControlSet\Services\MsBridge`, registry.SET_VALUE); err == nil {
		if err := k.DeleteValue("DisableSTA"); err != nil && !errors.Is(err, registry.ErrNotExist) {
			errs = append(errs, fmt.Errorf("the bridge's DisableSTA value: %w", err))
		}
		k.Close()
	}
	// Best-effort, as layDownOS's own delete before its add: netsh fails on a rule that is not there.
	_ = run(ctx, "netsh", "advfirewall", "firewall", "delete", "rule", "name="+firewallRule)
	return errors.Join(errs...)
}

// removeFiles removes the tree, keeping state\ unless deleteData. What Windows will not delete now
// -- this very executable, which `briard uninstall` runs as, and the directories holding it -- a
// hidden helper deletes the moment this process exits (removeAfterExit). Only when the helper
// cannot start is it handed to the restart instead, which deletes it before anything can run it
// again.
func (cfg Config) removeFiles(deleteData bool, logf func(string, ...any)) error {
	var errs []error
	if err := offPath(filepath.Join(prefixDir, "bin")); err != nil {
		errs = append(errs, fmt.Errorf("taking briard off the system PATH: %w", err))
	}
	if err := registry.DeleteKey(registry.LOCAL_MACHINE, uninstallKey); err != nil && !errors.Is(err, registry.ErrNotExist) {
		errs = append(errs, fmt.Errorf("taking briard off Installed apps: %w", err))
	}
	if err := cfg.offLogon(); err != nil {
		errs = append(errs, fmt.Errorf("taking the icon off the install user's logon: %w", err))
	}
	entries, err := os.ReadDir(prefixDir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return errors.Join(append(errs, err)...)
	}
	keep := filepath.Clean(cfg.stateDir())
	if deleteData {
		if err := os.RemoveAll(keep); err != nil {
			errs = append(errs, err)
		}
	}
	// What could not go now, by its top-level entry, in the order it must go: the prefix itself
	// last, once it is empty.
	var left []string
	for _, e := range entries {
		p := filepath.Join(prefixDir, e.Name())
		if !deleteData && strings.EqualFold(p, keep) {
			continue
		}
		if os.RemoveAll(p) != nil {
			left = append(left, p)
		}
	}
	if deleteData {
		if err := os.Remove(prefixDir); err != nil && !errors.Is(err, fs.ErrNotExist) {
			left = append(left, prefixDir)
		}
	}
	if len(left) == 0 {
		return errors.Join(errs...)
	}
	// NEVER THE RESTART WHEN THE HELPER RUNS: a delete queued for the restart is by path, and a
	// reinstall before that restart lays its new agent down at the same path for the restart to
	// delete.
	err = removeAfterExit(left)
	if err == nil {
		logf("briard's own program files go the moment this command exits")
		return errors.Join(errs...)
	}
	logf("could not start the helper that removes briard's own program files (%v)", err)
	// Deepest first: the restart replays these in the order they were asked, and a directory goes
	// only once it is empty.
	var deep []string
	for _, p := range left {
		_ = filepath.WalkDir(p, func(q string, _ fs.DirEntry, err error) error {
			if err == nil && !slices.Contains(deep, q) {
				deep = append(deep, q)
			}
			return nil
		})
	}
	slices.Reverse(deep)
	left = deep
	for _, p := range left {
		name, err := windows.UTF16PtrFromString(p)
		if err == nil {
			err = windows.MoveFileEx(name, nil, windows.MOVEFILE_DELAY_UNTIL_REBOOT)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", p, err))
		}
	}
	logf("%d files and folders still in use go at the next restart of Windows; restart it to finish", len(left))
	return errors.Join(errs...)
}

// removeAfterExit starts a hidden PowerShell that waits for this process to exit and then deletes
// paths, in order. Windows will not delete an executable while it runs, and this process is one:
// once it exits nothing holds the files, and the retries cover a scanner opening the freed file a
// moment longer. Started outside any job this process is in where the job allows it, so a shell
// or a task that closes its job when this process exits does not take the helper with it.
func removeAfterExit(paths []string) error {
	quoted := make([]string, len(paths))
	for i, p := range paths {
		quoted[i] = "'" + strings.ReplaceAll(p, "'", "''") + "'"
	}
	script := fmt.Sprintf("Wait-Process -Id %d -ErrorAction SilentlyContinue; foreach ($p in %s) { "+
		"for ($i = 0; $i -lt 30 -and (Test-Path -LiteralPath $p); $i++) { "+
		"Remove-Item -LiteralPath $p -Recurse -Force -ErrorAction SilentlyContinue; "+
		"if (Test-Path -LiteralPath $p) { Start-Sleep -Seconds 1 } } }",
		os.Getpid(), strings.Join(quoted, ","))
	var err error
	for _, flags := range []uint32{windows.CREATE_BREAKAWAY_FROM_JOB, 0} {
		cmd := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command", script)
		// Not from inside the tree it deletes.
		cmd.Dir = os.Getenv("SystemRoot")
		cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW | windows.CREATE_NEW_PROCESS_GROUP | flags}
		if err = cmd.Start(); err == nil {
			return cmd.Process.Release()
		}
	}
	return err
}

// offPath takes bin off the system Path, the entry onPath added.
func offPath(bin string) error {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SYSTEM\CurrentControlSet\Control\Session Manager\Environment`,
		registry.QUERY_VALUE|registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	path, _, err := k.GetStringValue("Path")
	if err != nil {
		return err
	}
	var kept []string
	for _, p := range strings.Split(path, ";") {
		if !strings.EqualFold(strings.TrimRight(p, `\`), bin) {
			kept = append(kept, p)
		}
	}
	if len(kept) == len(strings.Split(path, ";")) {
		return nil
	}
	return k.SetExpandStringValue("Path", strings.Join(kept, ";"))
}

// offLogon deletes the tray's Run value from the install user's hive (runAtLogon). Loaded while
// that account is logged on -- the one uninstalling, as a rule; when it is not, the value stays,
// naming a program that is gone, which Windows skips.
func (cfg Config) offLogon() error {
	if cfg.InstallUser == "" {
		return nil
	}
	k, err := registry.OpenKey(registry.USERS, cfg.InstallUser+`\`+runKey, registry.SET_VALUE)
	if errors.Is(err, registry.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer k.Close()
	if err := k.DeleteValue(trayValue); err != nil && !errors.Is(err, registry.ErrNotExist) {
		return err
	}
	return nil
}

// tapMarker is install.ps1's record that it installed the TAP driver package itself.
const tapMarker = "tap-installed"

// tapUninstallKey is the driver package's own Installed apps entry, its NSIS installer's.
const tapUninstallKey = `SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall\TAP-Windows`

// removeTapDriver removes the tap-windows6 driver package by its own uninstaller -- the TAP
// adapter is gone by now (DestroyBridge) -- on a POSITIVE record that briard installed it: no
// record, or one that cannot be read, keeps the driver, which may be another program's. Kept too
// while any TAP adapter is left: something else uses the driver.
func (cfg Config) removeTapDriver(ctx context.Context, logf func(string, ...any)) error {
	marker := filepath.Join(cfg.stateDir(), tapMarker)
	if _, err := os.Stat(marker); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("reading %s: %w -- the TAP driver was kept", marker, err)
	}
	if nic.TapInstalled() {
		logf("kept the TAP driver: another TAP adapter on this machine still uses it")
		return nil
	}
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, tapUninstallKey, registry.QUERY_VALUE)
	if errors.Is(err, registry.ErrNotExist) {
		return os.Remove(marker) // removed already, by someone else
	}
	if err != nil {
		return fmt.Errorf("the TAP driver's Installed apps entry: %w", err)
	}
	cmd, _, err := k.GetStringValue("UninstallString")
	k.Close()
	if err != nil {
		return fmt.Errorf("the TAP driver's uninstall command: %w", err)
	}
	exe := strings.Trim(cmd, `"`)
	dir := filepath.Dir(exe)
	// An NSIS uninstaller copies itself out and returns at once unless told where it lives with
	// _?=, which must come last and unquoted even with spaces in it -- so the command line is
	// written by hand rather than escaped argument by argument. Run in place, it cannot delete
	// itself: the folder goes after.
	c := exec.CommandContext(ctx, exe)
	c.SysProcAttr = &syscall.SysProcAttr{CmdLine: windows.EscapeArg(exe) + " /S _?=" + dir}
	if out, err := c.CombinedOutput(); err != nil {
		return fmt.Errorf("the TAP driver's uninstaller: %w: %s", err, strings.TrimSpace(string(out)))
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("removing %s: %w", dir, err)
	}
	logf("removed the TAP driver briard installed")
	return os.Remove(marker)
}
