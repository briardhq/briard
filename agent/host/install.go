// `briard-agent install`: lay this node down from a verified artifact set, and -- as a second
// call once the service runs -- wait for it to answer and hand the household its link.
//
// THE INSTALLER'S BODY, IN THE BINARY THE CHANNEL CAN FIX. install.sh is fetched from the channel
// root and run once, so whatever it does in shell is frozen where no release can reach it. It now
// does only what supervises the agent's own arrival -- root, the bootstrap fetch, this call,
// enabling the service, the hand-over -- and everything that lays files down is here. A Windows
// installer calls the same verb rather than re-implementing it.
//
// Two tests decide what belongs in the layout at all, the second wider than the first:
//
//  1. would a RELEASE ever need to change it?  Which device the guest's L2 hangs off, which
//     substrate that implies, which addresses this node numbers itself from, whether the tun
//     driver is loaded -- all revisited at every start (the agent's tick), none of them laid here.
//  2. is it a DECISION about this machine?  The disks, the identifiers, the console's rotation:
//     the agent's first start makes them, so no installer of any OS writes them.
//
// It installs NO SERVICE. A node is a node first: ready, replicating, able to fail over -- then
// you choose what runs on it. So the VIP answers with Briard's own page, and the health probe
// watches that front door, the one address that answers whether or not anything is installed.
//
// Cattle/pet FHS:
//
//	/opt/briard     = cattle: signed self-updating binaries + qemu bundle + guest image, plus
//	                  config.env. `rm -rf /opt/briard` + reinstall = a fresh host.
//	/var/lib/briard = pet: the data volume, the state disk, this node's identifiers and the
//	                  subnets it drew -- all of them the AGENT's to create and keep.
//	/run/briard     = tmpfs flags.
//
// BRIARD_ARTIFACTS=<dir> installs from a local, already-verified staging dir (the hermetic install
// tests, and a future offline install). Unset = the signed network fetch over the channel.
package host

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"briard.io/agent/install"
	"briard.io/agent/selfupdate"
)

// installRoots are the three directories an install lays down, as one value so a test can point
// them at a temp dir. In a real install they are the constants (config.go's note on why the
// prefix cannot move).
type installRoots struct {
	prefix, state, run string
	unitDir            string // where the shipped units go (BRIARD_UNIT_DIR)
	cliLink            string // `briard` on $PATH
}

// Install lays this node down: the verified artifact set (fetched over the channel, or the
// caller's BRIARD_ARTIFACTS dir), the qemu and guest trees, the image, the manifests, the
// agent's three frozen scripts, config.env and the units -- everything a node's first start
// reads. It enables nothing and starts nothing; that is the installer's hand-over.
//
// The report card is NOT here: the caller runs it first, so a refusal means nothing was written.
func Install(ctx context.Context, say, logf func(string, ...any)) error {
	r := installRoots{prefix: prefixDir, state: stateDir, run: runDir,
		// NixOS's /etc/systemd/system is a read-only store path, so the install rigs point this elsewhere.
		unitDir: env("BRIARD_UNIT_DIR", "/etc/systemd/system"), cliLink: cliLink}
	if err := os.MkdirAll(filepath.Join(r.prefix, "agent"), 0o755); err != nil {
		return err
	}
	for _, d := range []string{r.state, r.run} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}

	// ---- the artifacts (cattle) ----
	hostSrc, guestSrc := os.Getenv("BRIARD_ARTIFACTS"), os.Getenv("BRIARD_ARTIFACTS")
	staging := "" // the staging dir THIS call created, if any -- see the removal at the end
	if hostSrc == "" {
		// The signed release channel root, and WHICH release off it: `stable` (what strangers get,
		// a tested pair by construction), `latest` (what was published most recently -- how a
		// release is proven before promotion), `dev` (whatever was last cut from HEAD, ungated,
		// for a test node), or an exact host id. One selector, both chains.
		channel := env("BRIARD_CHANNEL_URL", "https://get.briard.io")
		release := env("BRIARD_RELEASE", install.TargetStable)
		// The release public key: the installer writes it before this runs (it travels inside
		// install.sh, the one thing fetched before anything is on disk to trust).
		keyPath := env("BRIARD_UPDATE_KEYRING", prefixDir+"/keyring.pem")
		pemBytes, err := os.ReadFile(keyPath)
		if err != nil {
			return fmt.Errorf("read the release keyring %s: %w", keyPath, err)
		}
		kr, err := selfupdate.NewKeyring(pemBytes)
		if err != nil {
			return err
		}
		// OURS TO DELETE, and the only staging dir that is. Recorded in the branch that CREATES
		// it rather than re-derived from BRIARD_ARTIFACTS at the far end: the caller's artifact
		// dir on the other branch is read-only and not ours, and deleting it would be a
		// considerably worse bug than the leak this closes.
		staging = filepath.Join(r.prefix, "staging")
		if err := os.RemoveAll(staging); err != nil {
			return err
		}
		say("fetching + verifying the signed artifact set (briard/%s + vm) ...", release)
		// Both chains, all-or-nothing: staging appears only once both have verified, so a
		// briard bundle never lands without the VM image it was published beside.
		if err := install.FetchRelease(ctx, channel, release, kr, staging, logf); err != nil {
			return fmt.Errorf("artifact verification failed (%w); nothing installed", err)
		}
		hostSrc, guestSrc = filepath.Join(staging, install.ChainBriard), filepath.Join(staging, install.ChainVM)
		// The verified qemu bundle arrives as a tarball (FetchRelease expanded the .zst). The tar
		// is rooted at the bundle itself (bin/ lib/ share/ PROVENANCE), NOT at a qemu/ dir, so it
		// is extracted INTO one -- the shape the local staging dir has.
		if err := os.MkdirAll(filepath.Join(hostSrc, "qemu"), 0o755); err != nil {
			return err
		}
		if err := run(ctx, "tar", "-xf", filepath.Join(hostSrc, "qemu-bundle.tar"), "-C", filepath.Join(hostSrc, "qemu")); err != nil {
			return err
		}
		os.Remove(filepath.Join(hostSrc, "qemu-bundle.tar"))
	}
	if err := r.layDown(ctx, hostSrc, guestSrc, say); err != nil {
		return err
	}

	// ---- the staging dir goes ----
	// Every artifact has been copied out, so what is left there is a second copy of the largest
	// thing we ship: the guest image byte for byte, a second qemu tree, a second agent -- 1.2 GB,
	// measured on briard-test, 13% of a 9.5 GiB disk, on the same filesystem as the state disk
	// that must grow to hold every service image the household installs.
	//
	// Never fatal: this is space reclaimed from a finished install, not a step whose failure means
	// the install did not happen.
	if staging != "" {
		say("removing the staging directory")
		os.RemoveAll(staging)
	}

	// ---- everything reaches the disk before anything runs it ----
	// A copy returns with the bytes in the page cache, and a rename or a new file's name can reach
	// the disk before its contents do. A power cut in the next few seconds would leave a truncated
	// qemu or image under a verified name, surfacing at the next BOOT where nobody would connect it
	// to the install. One sync covers every file laid down; it costs seconds, once.
	say("flushing the installed files to disk")
	return run(ctx, "sync")
}

// layDown places a staged artifact set -- hostSrc the briard chain, guestSrc the vm chain (the
// same flat dir on the BRIARD_ARTIFACTS path) -- under the roots.
func (r installRoots) layDown(ctx context.Context, hostSrc, guestSrc string, say func(string, ...any)) error {
	agentDir := filepath.Join(r.prefix, "agent")
	if err := installFile(filepath.Join(hostSrc, "briard-agent"), filepath.Join(agentDir, "briard-agent"), 0o755); err != nil {
		return fmt.Errorf("staging dir %s has no usable briard-agent: %w", hostSrc, err)
	}
	// The macvtap launch wrapper -- the fd-passing shim the agent runs as the guest unit's
	// ExecStart. Cattle that rides with the agent: an update stages briard-net-wrap.next beside it
	// for briard-commit to move.
	netWrap := ""
	if _, err := os.Stat(filepath.Join(hostSrc, "briard-net-wrap")); err == nil {
		netWrap = filepath.Join(agentDir, "briard-net-wrap")
		if err := installFile(filepath.Join(hostSrc, "briard-net-wrap"), netWrap, 0o755); err != nil {
			return err
		}
	}

	// THE QEMU TREE, REACHED THROUGH A LINK. One extracted bundle per release lives at
	// agent/qemu-<release>/ -- inside the directory the frozen pivot commits in -- and agent/qemu
	// is a symlink to the current one. An update extracts the next release's tree beside it and
	// stages qemu.next as a link; briard-commit commits qemu with ONE rename of that link, so no
	// recursive delete ever runs near a frozen script.
	//
	// <prefix>/qemu stays as the PUBLIC path, a fixed link onto that moving one: the bundle bakes
	// /opt/briard/qemu/lib/ld-linux... into its ELF interpreter, so that path must always resolve
	// to the running tree, and the agent's QEMU / QEMU_DATADIR defaults point through it. Named by
	// the installed release when a manifest is present, so a later pin back to it reuses the tree;
	// the local staging path has no manifest and gets a fixed name.
	rel := "install"
	if m, err := install.ReadManifest(filepath.Join(hostSrc, install.ManifestName)); err == nil && m.Version != "" {
		rel = m.Version
	}
	qemuTree := filepath.Join(agentDir, "qemu-"+rel)
	if err := freshDir(qemuTree); err != nil {
		return err
	}
	if err := run(ctx, "cp", "-a", filepath.Join(hostSrc, "qemu")+"/.", qemuTree+"/"); err != nil {
		return err
	}
	if err := run(ctx, "chmod", "-R", "u+w", qemuTree); err != nil {
		return err
	}
	if err := relink(filepath.Base(qemuTree), filepath.Join(agentDir, "qemu")); err != nil {
		return err
	}
	// An install that predates the link laid a real directory here; a link cannot replace one.
	if fi, err := os.Lstat(filepath.Join(r.prefix, "qemu")); err == nil && fi.IsDir() {
		if err := os.RemoveAll(filepath.Join(r.prefix, "qemu")); err != nil {
			return err
		}
	}
	if err := relink("agent/qemu", filepath.Join(r.prefix, "qemu")); err != nil {
		return err
	}

	// THE GUEST BUNDLE: the briard binaries the guest runs, shipped in the host chain and pushed
	// into the guest by the agent at every bring-up. Same tree-and-link shape as qemu, same commit
	// (briard-commit moves guest.next with -T). Existence-guarded: a channel without one leaves the
	// guest on the image's firmware -- which REFUSES every non-firmware verb, so an install that
	// finds a bundle must land it.
	if bundle := filepath.Join(hostSrc, "guest-bundle.tar"); exists(bundle) {
		guestTree := filepath.Join(agentDir, "guest-"+rel)
		if err := freshDir(guestTree); err != nil {
			return err
		}
		if err := run(ctx, "tar", "-xf", bundle, "-C", guestTree); err != nil {
			return err
		}
		// Freeing the tarball is an OPTIMISATION for the network path, where hostSrc is our own
		// temp dir. BRIARD_ARTIFACTS points at a read-only staging dir (a Nix store path, in the
		// install rigs), so a failure here is not one the install may stop on.
		os.Remove(bundle)
		if err := run(ctx, "chmod", "-R", "u+w", guestTree); err != nil {
			return err
		}
		if err := relink(filepath.Base(guestTree), filepath.Join(agentDir, "guest")); err != nil {
			return err
		}
	}
	image := filepath.Join(r.prefix, "guest-image", "nixos.qcow2")
	if err := os.MkdirAll(filepath.Dir(image), 0o755); err != nil {
		return err
	}
	if err := installFile(filepath.Join(guestSrc, "nixos.qcow2"), image, 0); err != nil {
		return err
	}
	// THE INSTALLED MANIFESTS, one per chain, each beside what it describes: the exact signed bytes
	// that verified, so the node can say which release it is on -- and so the update path can diff
	// the target's manifest against it and fetch only what changed. Absent on the local staging
	// path, which has no manifest to keep.
	//
	// The guest chain's third copy is the node-local RECORD of the release this node runs: pet
	// state, beside the identity, because the guest itself knows only a closure path and the stable
	// path orders on a release id. The copy under guest-image/ describes the IMAGE on disk; this one
	// follows the running OS as updates land.
	for _, m := range []struct{ src, dst string }{
		{filepath.Join(hostSrc, install.ManifestName), filepath.Join(agentDir, install.ManifestName)},
		{filepath.Join(guestSrc, install.ManifestName), filepath.Join(r.prefix, "guest-image", install.ManifestName)},
		{filepath.Join(guestSrc, install.ManifestName), filepath.Join(r.state, "guest-release.json")},
	} {
		if !exists(m.src) {
			continue
		}
		if err := installFile(m.src, m.dst, 0o644); err != nil {
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

	// THE AGENT'S THREE FROZEN SCRIPTS, copied out of the verified set. briard-exec and
	// briard-commit are self-update's on-disk pivot; briard-update is the fetch, which must not be
	// shipped by the thing it updates. Frozen means dumb, agent-INDEPENDENT shell, so a bug in the
	// volatile agent cannot wedge the mechanism that replaces it; shipped means they are signed
	// artifacts of the host chain, covered by the same manifest as the binary they start.
	say("installing the agent's scripts")
	for _, s := range []string{"briard-exec", "briard-commit", "briard-update"} {
		if !exists(filepath.Join(hostSrc, s)) {
			return fmt.Errorf("%s is absent from staging; this release cannot be installed", s)
		}
		if err := installFile(filepath.Join(hostSrc, s), filepath.Join(agentDir, s), 0o755); err != nil {
			return err
		}
	}
	if err := r.writeConfig(ctx, netWrap, say); err != nil {
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

// installEnvKey is an operator setting in the installer's environment: BRIARD_ + a config.env key.
var installEnvKey = regexp.MustCompile(`^BRIARD_[A-Z0-9_]*$`)

// installOnlyKeys configure THE INSTALL rather than the node: where to fetch from, which release,
// where the units go -- plus BRIARD_CONFIG, which names config.env itself, so copying it in would
// be a node telling itself where it already is.
var installOnlyKeys = map[string]bool{
	"BRIARD_ARTIFACTS": true, "BRIARD_RELEASE": true, "BRIARD_UNIT_DIR": true, "BRIARD_CONFIG": true,
}

// writeConfig writes config.env: A FILE, NOT THE UNIT, because some of these are decisions the
// agent revisits, and a decision it can revisit has to live somewhere it can rewrite (configset.go).
// The agent reads it with the environment layered ON TOP (loadConfigFile), so a rig that exports a
// variable still wins. Plain and greppable at 2am; 0600 because it is root's business alone.
//
// ⚠️ TWO KINDS OF LINE, AND ONLY TWO. What this install LAID DOWN -- the files it created or
// staged -- and what the OPERATOR said, copied verbatim from the environment. There is
// deliberately no third kind: NO DEFAULTS. A key that is absent is answered from config.go's
// shipped default, which a release can change and a line in this file could not.
//
// ⚠️ AND NO IDENTIFIERS. The node id, the flock id and the flock name are minted once into pet
// state at the agent's first start (identity.go), never written here.
func (r installRoots) writeConfig(ctx context.Context, netWrap string, say func(string, ...any)) error {
	var b strings.Builder
	fmt.Fprintf(&b, `# briard node configuration, written by install.sh. KEY=value, one per line; blank lines and
# '#' comments are ignored, whitespace either side of the '=' is trimmed, and NOTHING else is
# parsed -- no quoting, no expansion, no export. Every value is a path, a device name, an
# address or a duration. What each key MEANS, and what it defaults to when absent, is
# agent/host/config.go -- not this file, and not the installer that wrote it.
# The bundle and the disks THIS INSTALL laid down. Paths to files it created or staged, so they
# are facts about this host rather than defaults -- and their absence is a decision too, which is
# why the agent defaults none of them: an empty one is how a harness says it has no such disk.
QEMU=%[1]s/qemu/bin/qemu-system-x86_64
QEMU_DATADIR=%[1]s/qemu/share/qemu
NET_WRAP_BIN=%[2]s
GUEST_IMAGE=%[1]s/guest-image/nixos.qcow2
DATA_DISK=%[3]s/data.img
STATE_DISK=%[3]s/state.img
`, r.prefix, netWrap, r.state)
	if dir := backupFolder(ctx); dir != "" {
		fmt.Fprintf(&b, "BACKUP_DIR=%s\n", dir)
		say("nightly backups go to %s", dir)
	}
	// The operator's own settings: EVERY BRIARD_* in the environment, prefix stripped, so
	// `BRIARD_CPU=qemu64` becomes `CPU=qemu64` and the rule is one sentence rather than a table to
	// keep in step with config.go. Sorted, so two installs given the same environment produce
	// byte-identical files; single-line by the format's own rule.
	var lines []string
	for _, kv := range os.Environ() {
		k, v, _ := strings.Cut(kv, "=")
		if !installEnvKey.MatchString(k) || installOnlyKeys[k] || strings.Contains(v, "\n") {
			continue
		}
		lines = append(lines, strings.TrimPrefix(kv, "BRIARD_"))
	}
	sort.Strings(lines)
	for _, l := range lines {
		b.WriteString(l + "\n")
	}
	path := filepath.Join(r.prefix, "config.env")
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
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

// installFile copies src to dst whole: into a temp name beside it, then renamed over, so a binary
// a running process holds is replaced rather than rewritten under it (install(1)'s unlink, without
// the moment where neither exists). mode 0 keeps the source's permissions under the umask, the
// way cp(1) creates a file; anything else is set exactly, the way install -m does.
func installFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	fi, err := in.Stat()
	if err != nil {
		return err
	}
	tmp := dst + ".install-tmp"
	os.Remove(tmp)
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, fi.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return fmt.Errorf("copy %s: %w", src, err)
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if mode != 0 {
		if err := os.Chmod(tmp, mode); err != nil {
			os.Remove(tmp)
			return err
		}
	}
	return os.Rename(tmp, dst)
}

// freshDir makes dir empty: whatever an earlier install left under this release's name goes.
func freshDir(dir string) error {
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	return os.MkdirAll(dir, 0o755)
}

// relink points link at target, replacing whatever link was there (ln -sfnT).
func relink(target, link string) error {
	if err := os.Remove(link); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.Symlink(target, link)
}

// run executes one tool of the host's own -- tar, cp, chmod, sync: the same ones the update path
// and every host we install on already reach -- and returns its output as the error.
func run(ctx context.Context, name string, args ...string) error {
	if out, err := exec.CommandContext(ctx, name, args...).CombinedOutput(); err != nil {
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// installWaitBound is how long the closing waits for the node to answer. Past it the closing says
// how to get a link instead: one printed before the door is up would 503 in the household's face.
const installWaitBound = 180 * time.Second

// installLink picks the coded dashboard link out of what `briard open` printed; installHealthy is
// the agent's status line once the node is primary and its chain passed the health gate.
var (
	installLink    = regexp.MustCompile(`https?://[^ ]+/\?code=[0-9a-f]+`)
	installHealthy = regexp.MustCompile(`primary=true.*healthy=true`)
)

// InstallWait is `briard-agent install -wait`, the installer's hand-over once the service runs:
// wait, bounded, for the agent's OWN word that the node is primary and its chain (the front door,
// the dashboard) passed its health gate -- the status line it logs, the same signal the tier-4 rig
// waits on, and local (no name to resolve, no address to know under DHCP) -- then print what
// `briard open` says, its one-time link included, and hand that link to a desktop browser.
//
// ⚠️ THE AGENT'S OWN WORDS ARE PRINTED, not re-rendered here: `briard open` says what is on the
// node, which is the same answer a household gets running it a month from now.
//
// NO ADDRESS IS PRINTED before then. Nothing answers yet, so a name or an address is a promise
// about a node still booting -- and under DHCP there is no address to print at all.
func InstallWait(ctx context.Context, say func(string, ...any)) error {
	// The clock mark the journal is read from: a reinstall's journal still holds the previous
	// life's "healthy=true", and a link minted on a stale line is minted before the guest exists.
	since := time.Now().Format("2006-01-02 15:04:05")
	say("installed. briard is starting (up to 3 minutes)...")
	report := ""
	if _, err := exec.LookPath("journalctl"); err == nil {
		for deadline := time.Now().Add(installWaitBound); time.Now().Before(deadline); {
			journal, _ := exec.CommandContext(ctx, "journalctl", "-u", "briard-agent", "--since", since, "--no-pager", "-o", "cat").Output()
			// THE AGENT REFUSING ends the wait, in its own words: a node that cannot serve must not
			// leave the household waiting out the bound for a sentence that says "still starting".
			// Today that is a Wi-Fi guest handed the host's own address (DHCP client IDs), and the
			// remedy is a re-run of the installer, so it ends here as a failure.
			//
			// ⚠️ ASKED BEFORE HEALTHY, and the order is the point: the guest judges its health from
			// inside, so it reads healthy on the address it was refused, and the agent logs that
			// status line AFTER the refusal in the same pass.
			if refused := refusedLine(journal); refused != "" {
				return fmt.Errorf("briard is installed but cannot serve your home: %s", refused)
			}
			if installHealthy.Match(journal) {
				out, err := exec.CommandContext(ctx, filepath.Join(prefixDir, "agent", "briard-agent"), "open").Output()
				if report = string(out); err != nil {
					report = ""
				}
				if report != "" {
					break
				}
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(5 * time.Second):
			}
		}
	}
	if report == "" {
		say("briard is still starting. once it answers, get a one-time link to your Briard page with: sudo briard open")
		return nil
	}
	say("your Briard page is ready:")
	say("")
	fmt.Print(strings.TrimRight(report, "\n") + "\n")
	say("another link, any time: sudo briard open")
	// The desktop hand-off needs the bare URL, so it is picked back out of what was printed rather
	// than minted a second time -- a second mint would burn the link just shown.
	openForUser(installLink.FindString(report), say)
	return nil
}

// refusedLine is the first "network: refused: <why>" the agent logged, as <why>; "" if none.
func refusedLine(journal []byte) string {
	for _, l := range bytes.Split(journal, []byte("\n")) {
		if _, why, ok := bytes.Cut(l, []byte("network: refused: ")); ok {
			return string(why)
		}
	}
	return ""
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
