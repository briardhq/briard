package host

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"briard.io/agent/platform"
	"briard.io/agent/selfupdate"
)

// layDownOS is the Linux tail of the layout: the fd-passing launch wrapper, `briard` on $PATH,
// the frozen update script, config.env and the systemd units.
func (r installRoots) layDownOS(ctx context.Context, hostSrc string, say func(string, ...any)) error {
	agentDir := filepath.Join(r.prefix, "agent")
	// <prefix>/qemu is the PUBLIC path, a fixed link onto the moving agent/qemu: the Linux bundle
	// bakes /opt/briard/qemu/lib/ld-linux... into its ELF interpreter, so that path must always
	// resolve to the running tree, and the agent's QEMU / QEMU_DATADIR point through it. An install
	// that predates the link laid a real directory here; a link cannot replace one.
	if fi, err := os.Lstat(filepath.Join(r.prefix, "qemu")); err == nil && fi.IsDir() {
		if err := os.RemoveAll(filepath.Join(r.prefix, "qemu")); err != nil {
			return err
		}
	}
	if err := relink("agent/qemu", filepath.Join(r.prefix, "qemu")); err != nil {
		return err
	}
	// The macvtap launch wrapper -- the fd-passing shim the agent runs as the guest unit's
	// ExecStart. Cattle that rides with the agent: an update swaps it with the agent.
	netWrap := ""
	if _, err := os.Stat(filepath.Join(hostSrc, "briard-net-wrap")); err == nil {
		netWrap = filepath.Join(agentDir, "briard-net-wrap")
		if err := installFile(filepath.Join(hostSrc, "briard-net-wrap"), netWrap, 0o755); err != nil {
			return err
		}
	}

	// `briard` -- the operator CLI, a MODE of the agent binary rather than a second one. Nothing
	// under the prefix is on $PATH, so this symlink IS the CLI's existence as far as a user is
	// concerned. A symlink rather than a copy: self-update replaces the binary in place, and a copy
	// would leave a stale CLI talking to a newer agent. Best-effort: an unwritable /usr/local/bin
	// costs the CLI, never the install.
	os.MkdirAll(filepath.Dir(r.cliLink), 0o755)
	if err := relink(filepath.Join(agentDir, "briard-agent"), r.cliLink); err != nil {
		say("note: could not link %s; run %s directly", r.cliLink, filepath.Join(agentDir, "briard-agent"))
	}

	// NETWORKING IS NOT CONFIGURED HERE: the agent owns the whole of it, converged on its status
	// tick. One thing still has to be true, and it is checked rather than assumed: without the
	// fd-passing launch wrapper the agent cannot attach a macvtap to qemu, and that failure belongs
	// at the install rather than at the first guest launch.
	if netWrap == "" {
		return errors.New("the briard-net-wrap wrapper is absent from staging; the guest cannot be given a NIC")
	}

	// THE AGENT'S FROZEN SCRIPT, copied out of the verified set: briard-update launches the
	// update, which must not be shipped by the thing it updates. Frozen means dumb,
	// agent-INDEPENDENT shell, so a bug in the volatile agent cannot wedge the mechanism that
	// replaces it; shipped means it is a signed artifact of the host chain, covered by the same
	// manifest as the binary it fetches.
	say("installing the agent's update script")
	if !exists(filepath.Join(hostSrc, "briard-update")) {
		return errors.New("briard-update is absent from staging; this release cannot be installed")
	}
	if err := installFile(filepath.Join(hostSrc, "briard-update"), filepath.Join(agentDir, "briard-update"), 0o755); err != nil {
		return err
	}
	if err := r.writeConfig(ctx, r.bundleLines(netWrap), say); err != nil {
		return err
	}

	// THE UNITS, copied, not written: shipped artifacts of the host chain, hashed by the same
	// signed manifest. Every path in them is /opt/briard, a constant on every install, so there is
	// nothing to interpolate. Refused rather than skipped when one is missing: a staging dir without
	// briard-agent.service is one this host cannot run briard from.
	if err := os.MkdirAll(r.unitDir, 0o755); err != nil {
		return err
	}
	say("installing the systemd units to %s", r.unitDir)
	for _, u := range []string{"briard-agent.service", "briard-update.service", "briard-update.timer"} {
		if !exists(filepath.Join(hostSrc, u)) {
			return fmt.Errorf("%s is absent from staging; this release cannot be installed", u)
		}
		if err := installFile(filepath.Join(hostSrc, u), filepath.Join(r.unitDir, u), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// The file names the Linux arm ships under.
const (
	agentExe      = "briard-agent"
	qemuBundleTar = "qemu-bundle.tar"
)

// copyTree copies a staged tree's contents into dst, modes and links kept.
func copyTree(ctx context.Context, src, dst string) error {
	return run(ctx, "cp", "-a", src+"/.", dst+"/")
}

// makeWritable gives the owner write on a tree extracted from a read-only store path, so a later
// update or prune can remove it.
func makeWritable(ctx context.Context, dir string) error { return run(ctx, "chmod", "-R", "u+w", dir) }

// stopRunning has nothing to stop: a lay-down replaces each file by rename, which Linux allows
// under a running process: the running agent keeps the binary it started from until it restarts.
func stopRunning(context.Context, func(string, ...any)) error { return nil }

// flushInstall is the one sync that covers every file laid down (see Install).
func flushInstall(ctx context.Context, say func(string, ...any)) error {
	say("flushing the installed files to disk")
	return run(ctx, "sync")
}

// backupFolder makes the household's backup folder: `Briard Backup` in the home of the person who
// ran the installer, made AS them so it is theirs from the start -- they can move, copy or delete
// it without us, it outlives an uninstall, and whatever already backs up or syncs their home takes
// it along. Laid down by this install, so it is a line in config.env rather than a default. Nobody
// human (root, a cloud-init) or no home to put it in: "", and the agent's own folder under the
// state dir is used. An operator who named one (BRIARD_BACKUP_DIR, "" for off) is copied like any
// other setting instead.
//
// getent and sudo rather than os/user and a chown: the static agent reads /etc/passwd alone, and
// a household's account may come from NSS; and a folder the user could not have made themselves
// is not one this should make for them.
func backupFolder(ctx context.Context) string {
	user := os.Getenv("SUDO_USER")
	if _, set := os.LookupEnv("BRIARD_BACKUP_DIR"); set || user == "" || user == "root" {
		return ""
	}
	out, err := exec.CommandContext(ctx, "getent", "passwd", user).Output()
	if err != nil {
		return ""
	}
	fields := strings.Split(strings.TrimSpace(string(out)), ":")
	if len(fields) < 6 || fields[5] == "" {
		return ""
	}
	if fi, err := os.Stat(fields[5]); err != nil || !fi.IsDir() {
		return ""
	}
	dir := filepath.Join(fields[5], "Briard Backup")
	if exec.CommandContext(ctx, "sudo", "-u", user, "mkdir", "-p", dir).Run() != nil {
		return ""
	}
	return dir
}

// openForUser hands the dashboard link to the invoking user's browser, on a desktop and only there:
// a user to run it AS (the browser must be theirs, never root's), a display they can see (sudo
// keeps DISPLAY and XAUTHORITY; a Wayland session carries DISPLAY through Xwayland), and xdg-open
// present. Best-effort and silent otherwise -- a headless install is the printed link opened on a
// phone, not a failure -- and never waited on: xdg-open may block until the browser exits.
func openForUser(link string, say func(string, ...any)) {
	user := os.Getenv("SUDO_USER")
	if link == "" || user == "" || user == "root" || os.Getenv("DISPLAY")+os.Getenv("WAYLAND_DISPLAY") == "" {
		return
	}
	if _, err := exec.LookPath("xdg-open"); err != nil {
		return
	}
	say("also asking your desktop to open it")
	cmd := exec.Command("sudo", "-u", user, "-H", "env",
		"DISPLAY="+os.Getenv("DISPLAY"), "WAYLAND_DISPLAY="+os.Getenv("WAYLAND_DISPLAY"), "XAUTHORITY="+os.Getenv("XAUTHORITY"),
		"xdg-open", link)
	_ = cmd.Start()
}

// The agent's log, as the closing reads it: systemd's journal, from a clock mark.
type logMark string

func agentLogMark() logMark { return logMark(time.Now().Format("2006-01-02 15:04:05")) }

func agentLogReadable() bool {
	_, err := exec.LookPath("journalctl")
	return err == nil
}

func agentLogSince(ctx context.Context, m logMark) []byte {
	out, _ := exec.CommandContext(ctx, "journalctl", "-u", "briard-agent", "--since", string(m), "--no-pager", "-o", "cat").Output()
	return out
}

// bundleLines is config.env's bundle block on Linux: the qemu through the public <prefix>/qemu
// link, and the fd-passing launch wrapper.
func (r installRoots) bundleLines(netWrap string) string {
	return fmt.Sprintf("QEMU=%[1]s/qemu/bin/qemu-system-x86_64\nQEMU_DATADIR=%[1]s/qemu/share/qemu\nNET_WRAP_BIN=%[2]s\n", r.prefix, netWrap)
}

// osRoots fills the two roots only Linux has: where the shipped units go -- NixOS's
// /etc/systemd/system is a read-only store path, so the install rigs point this elsewhere -- and
// where `briard` lands on $PATH.
func (r installRoots) osRoots() installRoots {
	r.unitDir, r.cliLink = env("BRIARD_UNIT_DIR", "/etc/systemd/system"), cliLink
	return r
}

// swapMember cannot: a Linux bridge is the user's, and no Linux substrate has members the agent
// swaps (nic.Uplink never asks for one here).
func (cfg Config) swapMember(context.Context, string, string, func(string, ...any)) error {
	return errors.New("no member swap on Linux")
}

// StartAgent starts the agent's unit and returns once it reported READY, or with why it did not:
// `briard start`, and the self-update's start of its candidate.
func StartAgent(ctx context.Context, _ func(string, ...any)) error {
	return platform.StartService(ctx, selfupdate.DefaultAgentUnit)
}

// installUser is nobody on Linux: the admin door is root's, and there is no tray.
func installUser() string { return "" }
