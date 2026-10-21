package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
	"golang.org/x/sys/windows/svc/mgr"

	"briard.io/agent/nic"
	"briard.io/shared/atomicfile"
)

// ServiceName is the agent's name in the service manager: what the install registers and what the
// service handler runs under.
const ServiceName = "briard-agent"

// The file names the Windows arm ships under.
const (
	agentExe      = "briard-agent.exe"
	qemuBundleTar = "qemu-bundle-windows.tar"
)

// bridgeRecord is where the bridge build's offload record is kept: pet state, because it is the
// only copy of what this machine's NIC was set to before briard changed it.
const bridgeRecord = "bridge.json"

// layDownOS is the Windows tail of the layout. Everything here is something a systemd unit file or
// the Linux installer does there, and a Windows install has no other place to do it: config.env,
// the tree's ACL, the service and its recovery, the one firewall hole, `briard` on PATH -- and the
// bridge, which only an install may build (nic.BuildBridge says why), and which carries the guest
// only after the restart the installer then asks for.
func (r installRoots) layDownOS(ctx context.Context, hostSrc string, say func(string, ...any)) error {
	agentDir := filepath.Join(r.prefix, "agent")
	exe := filepath.Join(agentDir, agentExe)
	if err := r.writeConfig(ctx, r.bundleLines(""), say); err != nil {
		return err
	}
	// THE TREE IS SYSTEM'S AND THE ADMINISTRATORS'. %ProgramData% lets Users create and read
	// beneath it, and run\ holds the admin socket and the monitor socket -- unrestricted control
	// of the agent and the VM. The Linux 0700/0600 modes say this there; on Windows only an ACL
	// can: the ROOT stops inheriting and grants the two by SID (names are localized), and
	// everything beneath INHERITS that. Never `/inheritance:r /T`: on a file it strips the inherited
	// entries and the (OI)(CI) grants cannot attach to a file, which left every file with an empty
	// ACL -- measured, an agent even SYSTEM could not start. So the root alone is restricted, and
	// then everything BENEATH it is reset to inheriting from it. Root first, and the reset never
	// names the root: a reset of the root re-inherits %ProgramData%'s Users entries, and a reset
	// that fails partway (a reinstall's walk meeting an entry it cannot open) then left the whole
	// tree readable by Users. In this order a failure leaves the tree restricted, only some files
	// not yet inheriting.
	say("restricting %s to SYSTEM and Administrators", r.prefix)
	if err := run(ctx, "icacls", r.prefix, "/inheritance:r", "/grant:r", "*S-1-5-18:(OI)(CI)F", "*S-1-5-32-544:(OI)(CI)F", "/Q"); err != nil {
		return err
	}
	if err := run(ctx, "icacls", filepath.Join(r.prefix, "*"), "/reset", "/T", "/Q"); err != nil {
		return err
	}
	// THE SERVICE BEFORE THE BRIDGE, knowingly. A bridge build that fails leaves an auto-start
	// service behind, so the agent runs degraded at the next boot and says so -- the better
	// leftover than the other order's, a bridge nobody runs on, and every step here is
	// idempotent, so the re-run the failure asks for finds its own work done.
	if err := registerService(exe, say); err != nil {
		return err
	}
	// THE ONE INBOUND HOLE: the guest dials the host's backup store. Scoped to this program and
	// port rather than to a network profile, so the household's network category stays theirs.
	say("allowing the guest to reach the backup store (TCP %s, %s only)", backupStorePort, agentExe)
	_ = run(ctx, "netsh", "advfirewall", "firewall", "delete", "rule", "name=Briard backup store")
	if err := run(ctx, "netsh", "advfirewall", "firewall", "add", "rule", "name=Briard backup store", "dir=in",
		"action=allow", "program="+exe, "protocol=TCP", "localport="+backupStorePort, "profile=any"); err != nil {
		return err
	}
	if err := r.onPath(exe, say); err != nil {
		say("note: could not put briard on PATH (%v); run %s directly", err, exe)
	}
	return r.buildBridge(ctx, say)
}

// registerService registers the agent with the service manager, or brings an existing
// registration to this shape: automatic start, recovery that never gives up, and long enough at
// shutdown to power the guest off cleanly.
func registerService(exe string, say func(string, ...any)) error {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("the service manager: %w", err)
	}
	defer m.Disconnect()
	c := mgr.Config{
		StartType:   mgr.StartAutomatic,
		DisplayName: "Briard agent",
		Description: "Runs this machine's Briard virtual machine and keeps its network",
	}
	s, err := m.OpenService(ServiceName)
	if err == nil {
		say("updating the %s service", ServiceName)
		cur, cerr := s.Config()
		if cerr == nil {
			cur.StartType, cur.DisplayName, cur.Description = c.StartType, c.DisplayName, c.Description
			cur.BinaryPathName = windows.EscapeArg(exe) + " run"
			err = s.UpdateConfig(cur)
		} else {
			err = cerr
		}
	} else {
		say("registering the %s service", ServiceName)
		s, err = m.CreateService(ServiceName, exe, c, "run")
	}
	if err != nil {
		return fmt.Errorf("registering %s: %w", ServiceName, err)
	}
	defer s.Close()
	// RECOVERY, WHICH NEVER LATCHES DEAD: the last action repeats for every failure after the
	// third, so "restart" three times is restart forever -- systemd's StartLimitIntervalSec=0.
	restart := mgr.RecoveryAction{Type: mgr.ServiceRestart, Delay: 5 * time.Second}
	if err := s.SetRecoveryActions([]mgr.RecoveryAction{restart, restart, restart}, 0); err != nil {
		return fmt.Errorf("recovery actions: %w", err)
	}
	// And a FAILURE is any non-zero exit, not only a crash: without this flag an agent that reports
	// STOPPED with an error is, to the service manager, a service that stopped as asked.
	if err := s.SetRecoveryActionsOnNonCrashFailures(true); err != nil {
		return fmt.Errorf("recovery on failure exits: %w", err)
	}
	// The preshutdown budget: the guest's clean powerdown runs inside it, and the default is too
	// short to be sure of one.
	info := struct{ Timeout uint32 }{uint32(preshutdownBudget / time.Millisecond)}
	if err := windows.ChangeServiceConfig2(s.Handle, windows.SERVICE_CONFIG_PRESHUTDOWN_INFO, (*byte)(unsafe.Pointer(&info))); err != nil {
		return fmt.Errorf("preshutdown timeout: %w", err)
	}
	return nil
}

// preshutdownBudget is how long Windows waits for the agent at shutdown: the guest's own powerdown
// grace, its handover margin, and the agent's own stop, with room to spare.
const preshutdownBudget = 2 * time.Minute

// onPath puts `briard` on the machine's PATH: <prefix>\bin\briard.exe, a link to the agent (a link
// rather than a copy, so a self-update never leaves a stale CLI), and <prefix>\bin on the system
// Path. New shells see it; open ones keep the PATH they started with.
func (r installRoots) onPath(exe string, say func(string, ...any)) error {
	bin := filepath.Join(r.prefix, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		return err
	}
	if err := relink(exe, filepath.Join(bin, "briard.exe")); err != nil {
		return err
	}
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
	for _, p := range strings.Split(path, ";") {
		if strings.EqualFold(strings.TrimRight(p, `\`), bin) {
			return nil
		}
	}
	say("adding %s to the system PATH", bin)
	return k.SetExpandStringValue("Path", strings.TrimRight(path, ";")+";"+bin)
}

// buildBridge builds this machine's bridge over the household NIC -- the one holding the default
// route, or BRIARD_NIC's -- and keeps what the build changed on it. On a reinstall the bridge is
// already ours and the NIC is its member, and nothing changes.
func (r installRoots) buildBridge(ctx context.Context, say func(string, ...any)) error {
	sel := nic.Select(os.Getenv("BRIARD_NIC"), "")
	member := sel.Dev
	if member == nic.BridgeName {
		member, _ = nic.MemberSpeed(nic.BridgeName)
	}
	if member == "" {
		return fmt.Errorf("no network adapter to bridge: %s", sel.Fix())
	}
	say("building the network bridge over %s (the network drops for a moment)", member)
	rec, err := nic.BuildBridge(ctx, member)
	if rec != (nic.Offload{}) {
		// Kept even when the build failed part-way: the offload may already have been changed.
		b, merr := json.Marshal(rec)
		if merr == nil {
			merr = atomicfile.Write(filepath.Join(r.state, bridgeRecord), b, 0o600, 0o700)
		}
		if merr != nil {
			say("WARNING: could not keep the record of %s's checksum offload (%v); it was %s", member, merr, rec.Value)
		}
	}
	if err != nil {
		return fmt.Errorf("building the bridge over %s: %w", member, err)
	}
	// SPANNING TREE OFF. The bridge runs 802.1D: on every member carrier-up -- a cable in, a Wi-Fi
	// association -- it holds the port dark ~11 s in listening/learning, host and guest both, and
	// sends BPDUs onto the household LAN every 2 s. With one physical member beside the TAP there
	// is no loop to break. The value lives on the service key itself (its Parameters subkey is
	// ignored), the machine has one bridge and it is ours, and the restart below applies it.
	if err := disableBridgeSTA(); err != nil {
		return fmt.Errorf("turning the bridge's spanning tree off: %w", err)
	}
	say("the bridge is built; Windows must restart before briard can use it")
	return nil
}

func disableBridgeSTA() error {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SYSTEM\CurrentControlSet\Services\MsBridge`, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	return k.SetDWordValue("DisableSTA", 1)
}

// swapMember moves briard's bridge onto the standalone adapter to (nic.SwapMember) and keeps the
// arriving member's offload record where buildBridge keeps the first one, so the record always
// describes the member the bridge stands on, and the leaving member gets its own value back.
//
// AND WAITS FOR THE BRIDGE'S LEASE: it re-leases under the arriving member's MAC, and until it
// has, everything read off it -- the LAN fingerprint the bring-up compares and records, the
// address the guest is reached by -- is a reading of an adapter mid-DHCP. Both callers boot the
// guest right after (the cold start's swap, the live re-parent's), so both wait here. Bounded by
// the caller's budget; a lease that never comes is the guest's to wait out, as at any boot. Asked
// of the fingerprint, not the route table: Windows keeps the bridge's default route through the
// re-lease, while its address is the link-local placeholder and its gateway does not answer.
func (cfg Config) swapMember(ctx context.Context, bridge, to string, logf func(string, ...any)) error {
	if err := cfg.swapMemberRecorded(ctx, bridge, to, logf); err != nil {
		return err
	}
	for ctx.Err() == nil {
		if fp := nic.Read(ctx, bridge); fp.HostCIDR != "" && fp.GatewayMAC != "" {
			break
		}
		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
		}
	}
	return nil
}

func (cfg Config) swapMemberRecorded(ctx context.Context, bridge, to string, logf func(string, ...any)) error {
	path := filepath.Join(filepath.Dir(cfg.AssignmentCache), bridgeRecord)
	var prev nic.Offload
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &prev)
	}
	rec, err := nic.SwapMember(ctx, bridge, to, prev)
	if rec != (nic.Offload{}) {
		// Kept even when the swap failed part-way: the offload may already have been changed.
		b, merr := json.Marshal(rec)
		if merr == nil {
			merr = atomicfile.Write(path, b, 0o600, 0o700)
		}
		if merr != nil {
			logf("network: could not keep the record of %s's checksum offload (%v); it was %s", to, merr, rec.Value)
		}
	}
	return err
}

// copyTree copies a staged tree's contents into dst. The bundles hold plain files and directories.
func copyTree(_ context.Context, src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		to := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(to, 0o755)
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("%s: not a regular file", p)
		}
		in, err := os.Open(p)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(to, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, in); err != nil {
			out.Close()
			return err
		}
		return out.Close()
	})
}

// makeWritable has nothing to do: a tree extracted on Windows is writable by its owner already.
func makeWritable(context.Context, string) error { return nil }

// flushInstall is Linux's `sync`, spelled file by file: Windows has no call that flushes a
// whole volume from user space, so every regular file under the prefix is opened for writing
// and flushed. The restart the install ends in would write them out too, but the person is
// asked before it and may decline, and the rule (a verified name must never sit over truncated
// bytes) does not get to depend on their answer.
//
// The running executable is skipped: it is the installer's bootstrap, written before it ran rather
// than by the install, and Windows refuses a running image to a writer.
func flushInstall(_ context.Context, say func(string, ...any)) error {
	say("flushing the installed files to disk")
	self, _ := os.Executable()
	return filepath.WalkDir(prefixDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() || strings.EqualFold(p, self) {
			return err
		}
		f, err := os.OpenFile(p, os.O_WRONLY, 0)
		if err != nil {
			return err
		}
		defer f.Close()
		return f.Sync()
	})
}

// backupFolder makes the household's backup folder: `Briard Backup` in the Documents of the account
// that passed UAC -- the installer runs elevated AS that account, so the known folder is theirs.
// Resolved through the shell, never composed: with OneDrive's folder backup on (Windows 11's default
// with a Microsoft account) Documents IS OneDrive's, and the off-site copy comes with it. No owner
// step: the profile's inherited ACL gives its owner every file under it, whoever wrote it. SYSTEM
// (a remote or scripted install) has no household Documents: "", and the agent's own folder under
// the state dir is used. An operator who named one (BRIARD_BACKUP_DIR, "" for off) is copied like
// any other setting instead.
func backupFolder(context.Context) string {
	if _, set := os.LookupEnv("BRIARD_BACKUP_DIR"); set || isLocalSystem() {
		return ""
	}
	docs, err := windows.KnownFolderPath(windows.FOLDERID_Documents, windows.KF_FLAG_DEFAULT)
	if err != nil {
		return ""
	}
	dir := filepath.Join(docs, "Briard Backup")
	if os.MkdirAll(dir, 0o755) != nil {
		return ""
	}
	return dir
}

// isLocalSystem reports whether this process runs as SYSTEM rather than as a person; a token it
// cannot read counts as SYSTEM, so the backup lands in the agent's own folder rather than a guess.
func isLocalSystem() bool {
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	return err != nil || u.User.Sid.IsWellKnown(windows.WinLocalSystemSid)
}

// openForUser does nothing on Windows: the installer runs elevated, and a browser started from it
// would be too. The link is printed, and the tray opens it as the person.
func openForUser(string, func(string, ...any)) {}

// The agent's log, as the closing reads it: the agent's own file, from a byte offset.
type logMark int64

func agentLogMark() logMark {
	if fi, err := os.Stat(AgentLog()); err == nil {
		return logMark(fi.Size())
	}
	return 0
}

func agentLogReadable() bool { return true }

// agentLogSince reads the log from the mark -- or whole, when it is shorter than the mark (it rolled).
func agentLogSince(_ context.Context, m logMark) []byte {
	f, err := os.Open(AgentLog())
	if err != nil {
		return nil
	}
	defer f.Close()
	if fi, err := f.Stat(); err == nil && fi.Size() >= int64(m) {
		if _, err := f.Seek(int64(m), io.SeekStart); err != nil {
			return nil
		}
	}
	b, err := io.ReadAll(f)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil
	}
	return b
}

// bundleLines is config.env's bundle block on Windows: the qemu through agent\qemu. There is no
// launch wrapper: the guest's one tap is opened by name.
func (r installRoots) bundleLines(string) string {
	q := filepath.Join(r.prefix, "agent", "qemu")
	return fmt.Sprintf("QEMU=%s\nQEMU_DATADIR=%s\n", filepath.Join(q, "qemu-system-x86_64.exe"), filepath.Join(q, "share", "qemu"))
}

// osRoots has nothing to add: the service is registered, not a unit file, and `briard` goes on
// PATH under the prefix (onPath).
func (r installRoots) osRoots() installRoots { return r }

// stopRunning stops a node already installed here before the lay-down replaces its files: Windows
// refuses to replace an executable or a library that is running (measured on a reinstall: the
// agent's own .exe, then a DLL of the qemu still running from the same release's tree). The guest
// is powered off cleanly, never killed. The restart the installer ends in starts it all again.
func stopRunning(ctx context.Context, say func(string, ...any)) error {
	return stopNode(ctx, ConfigFromEnv().QMPSock, say)
}
