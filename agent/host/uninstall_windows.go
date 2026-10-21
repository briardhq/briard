package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"

	"briard.io/agent/nic"
	"briard.io/agent/platform"
)

// What the Windows install made (install.ps1 and layDownOS), and so what goes:
//
//   - the service, and the resume task install.ps1 registers for the closing;
//   - the bridge, with its member's checksum offload put back from the record, the TAP adapter by
//     instance, the bridge's spanning-tree value, and the backup store's firewall rule;
//   - the tree under %ProgramData%\briard and its bin on the system PATH;
//   - state\ under it, the pet -- KEPT unless the operator asks for it to go.
//
// The tap-windows6 driver package and the Hypervisor Platform stay: install.ps1 put them in place,
// and either may be serving something else on this machine by now.

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
	if s, err := m.OpenService(ServiceName); err == nil {
		err = s.Delete()
		s.Close()
		if err != nil {
			return fmt.Errorf("deleting the %s service: %w", ServiceName, err)
		}
	}
	// Best-effort: absent is the common case, the closing having run and removed it.
	_ = run(ctx, "schtasks", "/delete", "/tn", resumeTask, "/f")
	return nil
}

// stopNode stops a running node for something that must not race it -- an uninstall, or a
// reinstall laying files over the ones in use: the agent's service first (a stop leaves the guest
// running, for the next agent to re-adopt; here nothing must re-adopt it), then the guest's clean
// powerdown. A job kill is not a stop on Windows (unitStop), and the data volume a reinstall or a
// kept uninstall leaves behind is only worth keeping if the guest was not power-cut off it.
func stopNode(ctx context.Context, qmpSock string, logf func(string, ...any)) error {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("the service manager: %w", err)
	}
	defer m.Disconnect()
	s, err := m.OpenService(ServiceName)
	if err == nil {
		defer s.Close()
		st, err := s.Query()
		if err != nil {
			return fmt.Errorf("asking the %s service its state: %w", ServiceName, err)
		}
		if st.State != svc.Stopped {
			logf("stopping the %s service", ServiceName)
			if st.State != svc.StopPending {
				if _, err := s.Control(svc.Stop); err != nil {
					return fmt.Errorf("stopping the %s service: %w", ServiceName, err)
				}
			}
			deadline := time.Now().Add(serviceStopBound)
			for st.State != svc.Stopped {
				if time.Now().After(deadline) {
					return fmt.Errorf("the %s service has not stopped %s after it was asked to", ServiceName, serviceStopBound)
				}
				time.Sleep(time.Second)
				if st, err = s.Query(); err != nil {
					return fmt.Errorf("asking the %s service its state: %w", ServiceName, err)
				}
			}
		}
	}
	if s != nil {
		logf("powering the guest off cleanly")
	}
	gctx, cancel := context.WithTimeout(ctx, platform.GuestShutdownGrace+15*time.Second)
	defer cancel()
	return platform.ShutdownVM(gctx, qmpSock, platform.GuestShutdownGrace)
}

func (cfg Config) removeNetwork(ctx context.Context, _ func(string, ...any)) error {
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
// -- this very executable, which `briard uninstall` runs as, and the directories holding it -- is
// handed to the restart, which deletes it before anything can run it again.
func (cfg Config) removeFiles(deleteData bool, logf func(string, ...any)) error {
	var errs []error
	if err := offPath(filepath.Join(prefixDir, "bin")); err != nil {
		errs = append(errs, fmt.Errorf("taking briard off the system PATH: %w", err))
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
	var left []string
	for _, e := range entries {
		p := filepath.Join(prefixDir, e.Name())
		if !deleteData && strings.EqualFold(p, keep) {
			continue
		}
		if os.RemoveAll(p) == nil {
			continue
		}
		_ = filepath.WalkDir(p, func(q string, _ fs.DirEntry, err error) error {
			if err == nil {
				left = append(left, q)
			}
			return nil
		})
	}
	if deleteData {
		if err := os.Remove(prefixDir); err != nil && !errors.Is(err, fs.ErrNotExist) {
			left = append([]string{prefixDir}, left...)
		}
	}
	if len(left) == 0 {
		return errors.Join(errs...)
	}
	// Deepest first: the restart replays these in the order they were asked, and a directory goes
	// only once it is empty.
	slices.Reverse(left)
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
