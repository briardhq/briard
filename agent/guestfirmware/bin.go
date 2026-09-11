package guestfirmware

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// THE PUSH PROTOCOL ([B.86j], re-cut by [B.138] and [B.139]): every briard binary the guest runs
// rides the HOST bundle, and the host dresses the guest with them over this channel -- at
// bring-up, after a host commit, after any guest restart. The image bakes ONE binary,
// briard-guest-firmware, whose whole content is this protocol -- these three verbs, the
// handshake, os.poweroff -- so the guest chain moves when the protocol moves and not when the
// agent does ([B.139]). The guest AGENT, the front door and the dashboard exist in the guest
// only as pushed files. A pushed set lives in a disposable directory and is what the units run
// until the next boot.
//
// The set commits as ONE, or not at all ([B.138], owner's design):
//
//	bin.stage     every binary lands as <name>.next, verified (sha256 over the whole file)
//	bin.test      every staged file is run with --test-launch; any failure discards the whole
//	              staged set and the dress is refused with nothing armed -- the cheap gate, and on
//	              a secondary the only one
//	bin.activate  arms every name's single-use trial flag and restarts ONLY the agent's unit
//
// The trial agent's start is the verdict (BinStartup): it try-restarts the door units, which
// restarts them only where they are running -- a secondary does nothing and the verdict is
// immediate -- and their own pickers, flags consumed, exec the staged files. Both units are
// Type=notify with a short start timeout, so `try-restart` returning is the outcome: READY, or a
// failure. A failed door has ALREADY reverted itself by then: its auto-restart (2 s, direct mode)
// finds the flag consumed and execs the committed file; one failed start out of the unit's
// budget, which is why a failed upgrade never demotes (the promotion hold fires on start-limit
// exhaustion only -- configuration.nix, chainMemberFailure). A passing verdict opens the control
// port and then COMMITS, in this process and before a single verb is served (BinCommit,
// [B.148]): every staged name plus the release id together. A failing verdict exits 1 without
// opening the port; the agent's own picker brings the committed agent back -- or the FIRMWARE,
// when the first dress is what failed and nothing is committed yet -- and that start runs
// the AFTERMATH rule: staged files present on a non-trial start mean the set failed -- discard
// them and try-restart both doors, cheap and safe, because this agent cannot tell which of the
// three failed (a failed door reverted itself; a failed dashboard or agent left the door on the
// staged file). When it was the door, that is three door restarts a few seconds apart. Accepted.
//
// The host reads the outcome in the next handshake's Bundle: the pushed release means the set
// took; anything else means it was refused, recorded for good ([B.86j]).
//
// Frozen contract: the verbs are versioned additively, like briard-exec.
const (
	VerbBinStage    = "bin.stage"    // one chunk of a binary into <bin>/<name>.part; the last chunk verifies + renames to <name>.next
	VerbBinTest     = "bin.test"     // run every named staged file with --test-launch; a failure discards the staged set
	VerbBinActivate = "bin.activate" // arm every named binary's trial flag and restart the agent's own unit (detached)
)

// BinChunk is the wire unit of bin.stage. Data rides as JSON base64 (encoding/json does that
// for []byte), sized by the client under the channel's frame cap.
type BinChunk struct {
	Name   string `json:"name"`             // the binary: one of BinNames
	Seq    int    `json:"seq"`              // 0 starts the file (truncates any earlier attempt)
	Data   []byte `json:"data"`             // this chunk's bytes
	Last   bool   `json:"last"`             // after this chunk the file is complete
	SHA256 string `json:"sha256,omitempty"` // with Last: the whole file's digest, checked before it becomes .next
}

// BinTest names the staged binaries to prove.
type BinTest struct {
	Names []string `json:"names"`
}

// BinActivation names the bundle and the binaries to arm.
type BinActivation struct {
	Release string   `json:"release"` // the host release id these binaries came from -- what Bundle reports after the commit
	Names   []string `json:"names"`   // every name of the set; must include the guest agent's own, whose restart is the trial
}

// BinChunkSize is what the client sends per frame: comfortably under wire.go's 8 MiB cap once
// base64-encoded, and small enough that the guest never holds more than one chunk in memory.
const BinChunkSize = 4 << 20

// The binaries the guest can be dressed with, and the unit each one runs under. Closed on
// purpose: a name outside this table is refused before a byte is written, so the verb cannot
// be turned into "write any file".
var binUnits = map[string]string{
	"briard-dashboard":     "briard-dashboard.service",
	"briard-reverse-proxy": "briard-reverse-proxy.service",
	"briard-guest-agent":   "briard-guest-agent.service",
}

// BinNames is the set, in the order the trial exercises it: the doors first, the guest agent
// LAST -- its own unit is the one bin.activate restarts, and its start is the verdict on the
// others. guest-image/pivot.nix commits the same names.
var BinNames = []string{"briard-dashboard", "briard-reverse-proxy", selfBin}

// selfBin is this agent's own binary: activating the set restarts the unit serving the activation.
const selfBin = "briard-guest-agent"

// doorNames are the set minus the agent: the chain members the trial agent try-restarts, in
// PROMOTER CHAIN ORDER (agent/host/config.go promoterUnits: ... vip -> reverse-proxy ->
// dashboard -> the publishers), and the order is load-bearing. reactor writes each member
// `Requires=`/`After=` the previous one, so a restart of the door tears down and rebuilds
// everything after it. Restarting the dashboard FIRST and the door second therefore cancels the
// dashboard's in-flight start job -- and a member whose start job fails that way fires
// `OnFailure=` exactly like one that failed on its own (the same trap configuration.nix's
// chainMemberFailure note records for a lost promotion race), so the node demoted in the middle
// of an upgrade. Measured on install-macvtap, [B.138]. Earliest first, and a door the earlier
// one's restart already carried is verified rather than restarted again.
var doorNames = []string{"briard-reverse-proxy", "briard-dashboard"}

// TestLaunchFlag is the argv every briard guest binary accepts as its cheap self-test: start,
// check what can be checked without touching the real ports or the data volume (on a primary
// they are in use, on a secondary the volume is absent), exit 0.
const TestLaunchFlag = "--test-launch"

// testLaunchTimeout bounds one binary's --test-launch; doorTrialTimeout bounds one door's real
// launch under the trial (its unit's start timeout is shorter -- configuration.nix -- so this is
// the ceiling on systemctl itself, never the thing that decides).
const (
	testLaunchTimeout = 15 * time.Second
	doorTrialTimeout  = 60 * time.Second
)

// The two directories, overridable for tests and for a rig that dresses a stub. BinDir is on
// the guest's DISPOSABLE overlay root: a pushed binary does not survive a boot, which is the
// design -- every boot starts as firmware and the host re-dresses it. ⚠️ NOT under
// /var/lib/briard: in the guest that path is the REPLICATED DATA VOLUME, mounted only while the
// node holds the house -- binaries put there before promotion vanish under the mount, and
// binaries running from it hold the volume open so the node cannot demote (measured on the first
// install-macvtap run of [B.86j]: "Device is held open by someone"). BinRunDir is tmpfs: the
// trial flags, single-use, read by the picker.
const (
	defaultBinDir    = "/var/lib/briard-bin"
	defaultBinRunDir = "/run/briard-bin"
	releaseFile      = "RELEASE"
	nextSuffix       = ".next"
	partSuffix       = ".part"
	updateSuffix     = ".update" // armed for a trial; the picker DELETES it as it execs (single-use)
	ranSuffix        = ".ran"    // what the picker exec'd on the last start: trial | pushed | baked
)

func binDir() string {
	if d := os.Getenv("BRIARD_BIN_DIR"); d != "" {
		return d
	}
	return defaultBinDir
}

func binRunDir() string {
	if d := os.Getenv("BRIARD_BIN_RUN"); d != "" {
		return d
	}
	return defaultBinRunDir
}

// stageChunk appends one chunk; on the last one it checks the digest and renames the part
// into place as <name>.next, mode 0755. Any failure leaves no .next behind.
func stageChunk(c BinChunk) error {
	if _, ok := binUnits[c.Name]; !ok {
		return fmt.Errorf("bin.stage: %q is not a binary this guest can be dressed with", c.Name)
	}
	dir := binDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	part := filepath.Join(dir, c.Name+partSuffix)
	flags := os.O_WRONLY | os.O_CREATE | os.O_APPEND
	if c.Seq == 0 {
		flags = os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	}
	f, err := os.OpenFile(part, flags, 0o755)
	if err != nil {
		return err
	}
	if _, err := f.Write(c.Data); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if !c.Last {
		return nil
	}
	if c.SHA256 == "" {
		os.Remove(part)
		return errors.New("bin.stage: the last chunk names no sha256")
	}
	got, err := fileSHA256(part)
	if err != nil {
		os.Remove(part)
		return err
	}
	if got != c.SHA256 {
		os.Remove(part)
		return fmt.Errorf("bin.stage: %s: sha256 %s, want %s", c.Name, got, c.SHA256)
	}
	if err := os.Chmod(part, 0o755); err != nil {
		os.Remove(part)
		return err
	}
	return os.Rename(part, filepath.Join(dir, c.Name+nextSuffix))
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// testLaunch runs every named staged file with --test-launch, each under its own timeout, in
// order. The first failure discards the WHOLE staged set (the invariant BinStartup's aftermath
// rule relies on: staged files exist only between a stage and its verdict) and names the binary
// in the error, so the host's log says which one.
func testLaunch(ctx context.Context, x Executor, t BinTest) error {
	if len(t.Names) == 0 {
		return errors.New("bin.test: nothing to test")
	}
	dir := binDir()
	for _, n := range t.Names {
		if _, ok := binUnits[n]; !ok {
			return fmt.Errorf("bin.test: %q is not a binary this guest runs", n)
		}
		if _, err := os.Stat(filepath.Join(dir, n+nextSuffix)); err != nil {
			return fmt.Errorf("bin.test: %s is not staged: %w", n, err)
		}
	}
	for _, n := range t.Names {
		tctx, cancel := context.WithTimeout(ctx, testLaunchTimeout)
		out, err := x.Run(tctx, filepath.Join(dir, n+nextSuffix), TestLaunchFlag)
		cancel()
		if err != nil {
			discardStaged()
			return fmt.Errorf("bin.test: %s failed its test launch: %w: %s", n, err, strings.TrimSpace(string(out)))
		}
	}
	return nil
}

// discardStaged removes every staged file, the staged release id and every flag: the set is
// gone as a whole, and the committed binaries are what every unit runs from here.
func discardStaged() {
	dir, run := binDir(), binRunDir()
	for n := range binUnits {
		os.Remove(filepath.Join(dir, n+nextSuffix))
		os.Remove(filepath.Join(dir, n+partSuffix))
		os.Remove(filepath.Join(run, n+updateSuffix))
		// NOT the `.ran` markers: they describe what each unit is RUNNING, which discarding a
		// staged set does not change. The aftermath reads them right after this to decide which
		// doors still need putting back, and the picker rewrites each as its unit restarts.
	}
	os.Remove(filepath.Join(dir, releaseFile+nextSuffix))
}

// stagedSet reports the names whose staged file exists, in BinNames order.
func stagedSet() []string {
	var s []string
	for _, n := range BinNames {
		if _, err := os.Stat(filepath.Join(binDir(), n+nextSuffix)); err == nil {
			s = append(s, n)
		}
	}
	return s
}

// activate arms the trial flags of every named binary and restarts the agent's own unit --
// only that one: the doors are restarted by the TRIAL agent, whose start is the verdict on them
// (BinStartup). It refuses to arm a name whose .next is not staged (the picker would exec
// nothing), refuses a set without the agent (nothing would trial it), and refuses the release
// id unless it is a plain segment (it becomes a file's contents and a handshake field).
//
// The restart is on a context detached from the dispatch one, for the reason os.poweroff
// spells out: it is this agent's own unit, and the SIGTERM it earns would otherwise cancel the
// command that asked for it. The reply is written before it takes effect only because the serve
// loop holds the port until it is ([B.127]).
func activate(ctx context.Context, x Executor, a BinActivation) error {
	if a.Release == "" || strings.ContainsAny(a.Release, "/ \n") {
		return fmt.Errorf("bin.activate: bad release id %q", a.Release)
	}
	if len(a.Names) == 0 {
		return errors.New("bin.activate: nothing to activate")
	}
	dir, run := binDir(), binRunDir()
	self := false
	for _, n := range a.Names {
		if _, ok := binUnits[n]; !ok {
			return fmt.Errorf("bin.activate: %q is not a binary this guest runs", n)
		}
		if _, err := os.Stat(filepath.Join(dir, n+nextSuffix)); err != nil {
			return fmt.Errorf("bin.activate: %s is not staged: %w", n, err)
		}
		self = self || n == selfBin
	}
	if !self {
		return fmt.Errorf("bin.activate: the set does not include %s, whose restart is the trial", selfBin)
	}
	if err := os.MkdirAll(run, 0o755); err != nil {
		return err
	}
	// The release commits with the set (the picker's commit moves RELEASE.next when the trial
	// agent says READY), so a handshake after a refused set reports the old bundle.
	if err := os.WriteFile(filepath.Join(dir, releaseFile+nextSuffix), []byte(a.Release+"\n"), 0o644); err != nil {
		return err
	}
	for _, n := range a.Names {
		if err := os.WriteFile(filepath.Join(run, n+updateSuffix), nil, 0o644); err != nil {
			return err
		}
	}
	// OUR OWN UNIT, and the reason this is not a plain restart: the restart SIGTERMs this
	// process's whole cgroup -- `systemctl` included -- before it returns, so the command
	// dies with `signal: terminated` and the host reads a push that WORKED as a failure
	// (measured on install-macvtap, the first run of [B.86j]; os.poweroff hit the same
	// trap, [B.132]). A transient timer runs OUTSIDE this cgroup and fires after the reply
	// has left; the host then meets the restart as a dropped channel and reconnects.
	rctx := context.WithoutCancel(ctx)
	out, err := x.Run(rctx, "systemd-run", "--quiet", "--collect", "--on-active=1", "--timer-property=AccuracySec=100ms",
		"systemctl", "restart", binUnits[selfBin])
	if err != nil {
		return fmt.Errorf("bin.activate: restart %s: %w: %s", binUnits[selfBin], err, strings.TrimSpace(string(out)))
	}
	return nil
}

// BinStartup is the push protocol's start-time duty, run by `run --guest` BEFORE the control
// port is opened -- the host reconnects the moment the port opens and reads the release this
// process runs, so the port must not open until that answer is decided.
//
// A TRIAL start -- the picker's own marker says it ran the staged copy -- is the verdict on the
// whole set: each door unit that is running is try-restarted (blocking, so systemctl's exit IS
// the outcome of a Type=notify unit's start) and must be active afterwards AND still on its
// staged copy. Any failure returns an error and the caller exits 1 without opening the port; the
// door that failed has already reverted itself, its arm flag consumed and its marker no longer
// saying trial. A passing verdict returns nil, and the caller opens the port, commits the set
// (BinCommit) and only then says READY and serves.
//
// A NON-TRIAL start with staged files present is the aftermath of a failed trial (the invariant:
// staged files exist only between a stage and its verdict): the set is discarded and both doors
// are try-restarted onto the committed files, whichever of the three actually failed.
func BinStartup(ctx context.Context, x Executor, logf func(string, ...any)) error {
	if pickerRan(selfBin) == "trial" {
		return trialVerdict(ctx, x, logf)
	}
	if staged := stagedSet(); len(staged) > 0 {
		logf("bin: a staged set (%s) was left behind by a trial that did not commit: discarding it and putting both doors back on the committed binaries", strings.Join(staged, ", "))
		discardStaged()
		// ONE restart, at the EARLIEST door in chain order, blocking -- the same rule the trial
		// follows, and for the same reason: everything ordered after a member comes back with it
		// in one transaction, so one call suffices, and a second overlapping one would cancel the
		// start jobs the first had queued (a cancelled start job fires `OnFailure=`; measured on
		// install-macvtap, [B.138], when this restarted the dashboard first and the door second).
		// The staged files are already gone, so every picker in that transaction lands on the
		// committed binary. A door the transaction did not reach still reads `trial` and is
		// restarted by the next turn of this loop.
		for _, n := range doorNames {
			if !unitActive(ctx, x, binUnits[n]) || pickerRan(n) != "trial" {
				continue // not running here, or already carried back by an earlier member
			}
			_, _ = x.Run(ctx, "systemctl", "reset-failed", binUnits[n])
			if out, err := x.Run(ctx, "systemctl", "try-restart", binUnits[n]); err != nil {
				logf("bin: try-restart %s after the failed trial: %v: %s", binUnits[n], err, strings.TrimSpace(string(out)))
			}
		}
	}
	return nil
}

// trialVerdict is the trial agent's half of BinStartup.
//
// A FAILED UPGRADE MUST NEVER DEMOTE (owner, 2026-09-08), and what that takes is ONE RESTART
// TRANSACTION AT A TIME, walked in chain order. Three separate things have to be true, and the
// loop below is arranged around them:
//
//  1. A door whose staged copy exits 1 fires NO hook. systemd's rule: an auto-restart under
//     `RestartMode=direct` skips failed/inactive and skips `OnFailure=`, for a start that FAILED
//     exactly as for a crash while running (`Restart=` covers both). It just comes back on the
//     committed binary. So the failure this trial deliberately provokes is free.
//  2. What DOES fire the hook, besides a member with no restart left, is a member whose START JOB
//     IS CANCELLED. Restarting a chain member stops everything ordered after it and enqueues
//     their restarts in the same transaction; a SECOND restart issued while that is in flight
//     tears down what the first just started, and the cancelled start job is a job-level failure
//     -- `RestartMode=direct` does not cover it and no start limit is involved. configuration.nix
//     records the same trap for a lost promotion race: a member whose start job fails with result
//     `dependency` fires `OnFailure=` exactly like one that failed on its own. This is what
//     demoted a node mid-upgrade when the loop restarted the dashboard first and the door second
//     (install-macvtap, [B.138]). Hence: earliest member first, blocking, and a door the earlier
//     restart already carried is verified rather than restarted again.
//  3. The start budget, the ordinary way the hook fires: `StartLimitBurst` inside
//     `StartLimitIntervalSec`, 5 in 300 s here. A trial spends up to three, so it clears the
//     counter first (`reset-failed` zeroes the start rate limit and the restart counter --
//     systemctl(1)) and cannot land a door that had already burned starts over the limit.
//
// (The very first demote this item measured was none of these: the blind verdict had COMMITTED a
// door binary that exits 1, so every later start failed, the budget went in ~10 s, and
// briard-promotion-hold fired CORRECTLY on a member that genuinely could not start.)
func trialVerdict(ctx context.Context, x Executor, logf func(string, ...any)) error {
	release := "?"
	if b, err := os.ReadFile(filepath.Join(binDir(), releaseFile+nextSuffix)); err == nil {
		release = strings.TrimSpace(string(b))
	}
	for _, n := range doorNames {
		unit := binUnits[n]
		if !unitActive(ctx, x, unit) {
			logf("bin: trial of %s: %s is not running here (not the primary); its staged file commits on the cheap gate alone", release, n)
			continue
		}
		// RESTART ONLY WHAT IS STILL ARMED, which in the ordinary case is the FIRST door alone.
		// The arm flag is single-use and consumed by the picker, so a door still holding one has
		// not started since the activation; a door whose flag is gone was carried by the restart
		// of the member before it (chain propagation, above) and is already on its staged copy.
		// Restarting such a door again would find no flag, run the COMMITTED copy, and refuse a
		// trial that is passing -- besides being the second transaction that cancels start jobs.
		if !armed(n) {
			logf("bin: trial of %s: %s came up with the member before it; verifying where it stands", release, n)
		} else {
			// A FULL START BUDGET FIRST. Best-effort: a reset that fails leaves the budget as it
			// was, and the trial is worth attempting on a door that has not been failing anyway.
			if out, err := x.Run(ctx, "systemctl", "reset-failed", unit); err != nil {
				logf("bin: trial of %s: could not clear %s's start budget first (%v: %s); trialling anyway", release, n, err, strings.TrimSpace(string(out)))
			}
			// BLOCKING: the next member is not touched until this transaction has finished.
			tctx, cancel := context.WithTimeout(ctx, doorTrialTimeout)
			out, err := x.Run(tctx, "systemctl", "try-restart", unit)
			cancel()
			if err != nil {
				return fmt.Errorf("bin: trial of %s REFUSED: %s failed its real launch (%v: %s); it has reverted to the committed binary by its own restart, and this agent exits so the committed agent comes back", release, n, err, strings.TrimSpace(string(out)))
			}
		}
		if !unitActive(ctx, x, unit) {
			return fmt.Errorf("bin: trial of %s REFUSED: %s is not active after its restart", release, n)
		}
		// WHAT IT ACTUALLY RAN, and the reason the two checks above are not enough: a staged copy
		// that exits 1 fails its start, systemd's own auto-restart brings the unit back on the
		// COMMITTED binary 2 s later, and the restart job then reports success -- active, exit 0,
		// nothing to see. Measured on the first install-macvtap run of [B.138]: the verdict passed
		// and committed a door that had already reverted. The picker leaves the answer behind.
		if ran := pickerRan(n); ran != "trial" {
			return fmt.Errorf("bin: trial of %s REFUSED: %s is active but running the %s binary, not the staged one -- its start failed and systemd restarted it onto what it ran before", release, n, ran)
		}
		logf("bin: trial of %s: %s restarted on the staged binary and says READY", release, n)
	}
	logf("bin: trial of %s: verdict PASS; opening the port, then committing before anything is served", release)
	return nil
}

// BinCommit is the ONE commit of the pushed set -- every staged name plus RELEASE, moved
// together, and every flag cleared. It runs IN THE AGENT, after the control port is open and
// BEFORE the port is served or READY is sent ([B.148]).
//
// WHY IT IS HERE AND NOT AN ExecStartPost. It used to be guest-image/pivot.nix's
// briard-bin-commit, run by systemd only after READY=1, so that a start systemd had not seen
// succeed could not commit. That ordering bought less than it looked: the host does not watch
// READY -- it cannot, it is outside the guest -- it watches the PORT, which this process opens
// two statements earlier. So the real window was [port open, commit], and in it the host
// reconnects, handshakes, judges the guest dressed and starts sending bring-up verbs, while
// briard-node-storage.service names <binDir>/briard-guest-agent DIRECTLY (configuration.nix:
// through the picker it would arm a trial every time storage came up). Measured 2026-09-11 on
// the fleet tier (os-reboot.sh, run 34576181211): storage bring-up reached the guest 12 ms
// before the commit did, exec'd a path that did not exist yet, 203/EXEC -- and because
// `systemctl start` on a oneshot returns the failure, a healthy OS upgrade rolled back. The
// deadman lands in the same window on every dress and survives it only because it retries every
// 2 s; the units bring-up starts get one attempt each.
//
// Committing here closes it by construction: the set is final before anything can ask for it.
// What is given up is the guarantee that a binary which trialled, opened the port and then died
// before READY cannot commit itself -- a datagram send's worth of stretch, and a binary that
// dies immediately AFTER READY commits today with the same result. The verdict was always the
// pushed binary's own (BinStartup); the guarantee that survives a wrong one is not this hook but
// the update timer below the agent, which fetches, verifies and restarts again regardless.
//
// Only a TRIAL start commits. The flags are cleared on EVERY start: a `.ran` marker left behind
// by an earlier start would otherwise be read as this one's.
func BinCommit(ctx context.Context, x Executor, logf func(string, ...any)) {
	dir, run := binDir(), binRunDir()
	if pickerRan(selfBin) == "trial" {
		var set []string
		for _, n := range BinNames {
			err := os.Rename(filepath.Join(dir, n+nextSuffix), filepath.Join(dir, n)) // atomic same-fs commit
			if err == nil {
				set = append(set, n)
			} else if !os.IsNotExist(err) {
				logf("bin: committing %s failed (%v): the set is PARTIAL, and the next non-trial start discards what is left of it", n, err)
			}
		}
		if err := os.Rename(filepath.Join(dir, releaseFile+nextSuffix), filepath.Join(dir, releaseFile)); err != nil && !os.IsNotExist(err) {
			logf("bin: committing %s failed (%v): the handshake will report the release this set replaced", releaseFile, err)
		}
		logf("bin: committed %s: %s", committedRelease(), strings.Join(set, " "))
		// The doors get their start budget back: the trial spent one of it where a door was running.
		args := []string{"reset-failed"}
		for _, n := range doorNames {
			args = append(args, binUnits[n])
		}
		if out, err := x.Run(ctx, "systemctl", args...); err != nil {
			logf("bin: could not clear the doors' start budget after the commit (%v: %s)", err, strings.TrimSpace(string(out)))
		}
	}
	for _, n := range BinNames {
		os.Remove(filepath.Join(run, n+updateSuffix))
		os.Remove(filepath.Join(run, n+ranSuffix))
	}
}

// committedRelease is the id in <binDir>/RELEASE, "?" when there is none to read -- the same
// fallback the shell commit printed, and only ever used in that log line.
func committedRelease() string {
	b, err := os.ReadFile(filepath.Join(binDir(), releaseFile))
	if err != nil {
		return "?"
	}
	return strings.TrimSpace(string(b))
}

// armed reports whether n still holds its single-use arm flag: it has not started since the
// activation. The picker consumes the flag as it execs, so this is also "has NOT been carried by
// an earlier chain member's restart".
func armed(name string) bool {
	_, err := os.Stat(filepath.Join(binRunDir(), name+updateSuffix))
	return err == nil
}

// pickerRan is what the picker exec'd on a unit's last start -- "trial", "pushed", "baked", or
// "unknown" when it left nothing (a unit that has not started this boot). guest-image/pivot.nix
// writes it; it is cleared with the trial flags at the commit.
func pickerRan(name string) string {
	b, err := os.ReadFile(filepath.Join(binRunDir(), name+ranSuffix))
	if err != nil {
		return "unknown"
	}
	if s := strings.TrimSpace(string(b)); s != "" {
		return s
	}
	return "unknown"
}

func unitActive(ctx context.Context, x Executor, unit string) bool {
	out, _ := x.Run(ctx, "systemctl", "is-active", unit)
	return strings.TrimSpace(string(out)) == "active"
}

// runningBundle is what the handshake reports: the bundle id when this process runs from the
// pushed directory, "" when it runs the firmware.
//
// It reads the COMMITTED file and nothing else. Between [B.138] and [B.148] it had to prefer
// RELEASE.next on a trial start, because a handshake could be answered in the window between
// READY and the ExecStartPost commit, where only the staged id existed -- reading RELEASE there
// handed the host the OLD id and had a good dress recorded as a permanent revert (measured on
// the first install-macvtap run of [B.138]). BinCommit now runs before this process serves
// anything, so that window is gone: by the time any handshake is answered RELEASE holds the id
// just committed, and the `.ran` marker the inversion keyed off has been cleared. A trial that
// FAILS never opens the port at all, so this can only ever report a release the doors earned.
func runningBundle() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	dir := binDir()
	if filepath.Dir(exe) != dir {
		return ""
	}
	b, err := os.ReadFile(filepath.Join(dir, releaseFile))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// HandleBin is the dispatch arm for the three verbs.
func HandleBin(ctx context.Context, x Executor, verb string, payload json.RawMessage) (any, error) {
	switch verb {
	case VerbBinStage:
		var c BinChunk
		if err := json.Unmarshal(payload, &c); err != nil {
			return nil, err
		}
		return nil, stageChunk(c)
	case VerbBinTest:
		var t BinTest
		if err := json.Unmarshal(payload, &t); err != nil {
			return nil, err
		}
		return nil, testLaunch(ctx, x, t)
	case VerbBinActivate:
		var a BinActivation
		if err := json.Unmarshal(payload, &a); err != nil {
			return nil, err
		}
		return nil, activate(ctx, x, a)
	}
	return nil, fmt.Errorf("guestfirmware: %s is not a bin verb", verb)
}
