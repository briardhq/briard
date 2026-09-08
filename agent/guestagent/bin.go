package guestagent

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

// THE PUSH PROTOCOL ([B.86j], re-cut by [B.138]): every briard binary the guest runs rides the
// HOST bundle, and the host dresses the guest with them over this channel -- at bring-up, after
// a host commit, after any guest restart. The image bakes ONE firmware binary, the guest agent,
// whose stable core is exactly these three verbs plus the handshake; the front door and the
// dashboard exist in the guest only as pushed files. A pushed set lives in a disposable
// directory and is what the units run until the next boot.
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
// port and says READY; the unit's ExecStartPost (guest-image/pivot.nix briard-bin-commit) then
// commits every staged name plus the release id together. A failing verdict exits 1 without
// opening the port; the agent's own picker brings the committed agent back, and its start runs
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
	verbBinStage    = "bin.stage"    // one chunk of a binary into <bin>/<name>.part; the last chunk verifies + renames to <name>.next
	verbBinTest     = "bin.test"     // run every named staged file with --test-launch; a failure discards the staged set
	verbBinActivate = "bin.activate" // arm every named binary's trial flag and restart the agent's own unit (detached)
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

// doorNames are the set minus the agent: the chain members the trial agent try-restarts.
var doorNames = []string{"briard-dashboard", "briard-reverse-proxy"}

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
	updateSuffix     = ".update" // the picker consumes this into .trial
	trialSuffix      = ".trial"  // "this start IS a trial"
	ranSuffix        = ".ran"    // what the picker exec'd on the last start: trial | pushed | baked
	// trialInProgress holds the demote hook off while the trial restarts the doors ([B.138]).
	// Read by briard-promotion-hold's ExecCondition (guest-image/configuration.nix); the NAME is
	// part of that contract, like the flags above.
	trialInProgress = "trial-in-progress"
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
		os.Remove(filepath.Join(run, n+trialSuffix))
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
// A TRIAL start (the picker consumed this agent's flag) is the verdict on the whole set: each
// door unit that is running is try-restarted -- blocking, so systemctl's exit IS the outcome of
// a Type=notify unit's start -- and must be active afterwards. Any failure returns an error and
// the caller exits 1 without opening the port; the door that failed has already reverted itself
// (its auto-restart finds its flag consumed). A passing verdict returns nil, the caller opens
// the port and says READY, and the unit's ExecStartPost commits the set.
//
// A NON-TRIAL start with staged files present is the aftermath of a failed trial (the invariant:
// staged files exist only between a stage and its verdict): the set is discarded and both doors
// are try-restarted onto the committed files, whichever of the three actually failed.
func BinStartup(ctx context.Context, x Executor, logf func(string, ...any)) error {
	run := binRunDir()
	if _, err := os.Stat(filepath.Join(run, selfBin+trialSuffix)); err == nil {
		return trialVerdict(ctx, x, logf)
	}
	// Whatever else this start is, the demote hook is live again: only a trial in progress may
	// hold it off, and a trial that died without tidying up must not outlive its own process.
	if err := os.Remove(filepath.Join(run, trialInProgress)); err == nil {
		logf("bin: a previous trial left the demote hook held off; released")
	}
	if staged := stagedSet(); len(staged) > 0 {
		logf("bin: a staged set (%s) was left behind by a trial that did not commit: discarding it and putting both doors back on the committed binaries", strings.Join(staged, ", "))
		discardStaged()
		for _, n := range doorNames {
			_, _ = x.Run(ctx, "systemctl", "reset-failed", binUnits[n])
			if out, err := x.Run(ctx, "systemctl", "try-restart", "--no-block", binUnits[n]); err != nil {
				logf("bin: try-restart %s after the failed trial: %v: %s", binUnits[n], err, strings.TrimSpace(string(out)))
			}
		}
	}
	return nil
}

// trialVerdict is the trial agent's half of BinStartup.
//
// It holds the DEMOTE HOOK OFF while it is restarting doors (owner, 2026-09-08: a procedure
// designed to be controllable must never demote), and the reason is the START BUDGET, not the
// failure itself. systemd's rule: an auto-restart under `RestartMode=direct` skips failed and
// inactive and skips `OnFailure=` -- for a start that failed exactly as for a crash while running
// -- so ONE failed trial start demotes nothing. What fires the hook is a member with no restart
// left: the start limit (5 in 300 s for the doors) exhausted. A trial spends real budget on a
// chain member -- the trial start, its auto-restart, and the aftermath's restart, up to three of
// the five -- so a door that had already burned starts for unrelated reasons could cross the
// limit DURING an upgrade and hand the house on for it. This flag makes that impossible for the
// seconds it is up; the commit also gives the doors their budget back (pivot.nix reset-failed).
//
// (The demote measured on the first full install-macvtap run of [B.138] was NOT this: the blind
// verdict had committed a door binary that exits 1, so every later start of it failed, the budget
// went in ~10 s, and the hold then fired correctly on a member that genuinely could not start.
// Fixing the verdict removed that cause; this guard is the narrower one above.)
//
// The flag is what briard-promotion-hold's ExecCondition reads (guest-image/configuration.nix);
// it is on tmpfs and cleared at every agent start below, so the worst a crash here can cost is
// one restart's worth of a node that will not hand the house on.
func trialVerdict(ctx context.Context, x Executor, logf func(string, ...any)) error {
	release := "?"
	if b, err := os.ReadFile(filepath.Join(binDir(), releaseFile+nextSuffix)); err == nil {
		release = strings.TrimSpace(string(b))
	}
	if err := os.WriteFile(filepath.Join(binRunDir(), trialInProgress), []byte(release+"\n"), 0o644); err != nil {
		return fmt.Errorf("bin: trial of %s REFUSED: cannot hold the demote hook off (%w); a door that failed its trial would demote this node", release, err)
	}
	// NOT cleared here, on purpose. `OnFailure=` is a JOB systemd queues when the unit enters
	// failed, and our own try-restart does not return until the unit has finished being
	// auto-restarted -- so the hold can start AFTER the verdict is in. The flag is cleared by the
	// commit (guest-image/pivot.nix, the passing path) or by the next agent start (BinStartup,
	// the refused path), both of which are seconds away and neither of which races the hook.
	for _, n := range doorNames {
		unit := binUnits[n]
		if !unitActive(ctx, x, unit) {
			logf("bin: trial of %s: %s is not running here (not the primary); its staged file commits on the cheap gate alone", release, n)
			continue
		}
		tctx, cancel := context.WithTimeout(ctx, doorTrialTimeout)
		out, err := x.Run(tctx, "systemctl", "try-restart", unit)
		cancel()
		if err != nil {
			return fmt.Errorf("bin: trial of %s REFUSED: %s failed its real launch (%v: %s); it has reverted to the committed binary by its own restart, and this agent exits so the committed agent comes back", release, n, err, strings.TrimSpace(string(out)))
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
	logf("bin: trial of %s: verdict PASS; opening the port, and the commit follows READY", release)
	return nil
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
// A TRIAL THAT HAS PASSED reports the release it is trialling (RELEASE.next), and this order is
// load-bearing: the port opens only after the verdict, the commit is the unit's ExecStartPost
// AFTER that, and the host -- already retrying its reconnect -- gets in first almost every time.
// Reading RELEASE first meant handing it the OLD id and having a good dress recorded as a
// permanent revert (measured on the first install-macvtap run of [B.138]; the same window
// existed under [B.86j], hidden by a port that opened earlier). A trial that FAILS never opens
// the port at all, so this can only ever report a release the doors have already earned.
func runningBundle() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	dir := binDir()
	if filepath.Dir(exe) != dir {
		return ""
	}
	order := []string{releaseFile, releaseFile + nextSuffix}
	if _, err := os.Stat(filepath.Join(binRunDir(), selfBin+trialSuffix)); err == nil {
		order = []string{releaseFile + nextSuffix, releaseFile}
	}
	for _, f := range order {
		if b, err := os.ReadFile(filepath.Join(dir, f)); err == nil {
			return strings.TrimSpace(string(b))
		}
	}
	return ""
}

// ---- the host side -------------------------------------------------------------------------

// BinStage streams one binary to the guest in BinChunkSize chunks and has the guest verify the
// whole file's sha256 before it becomes <name>.next.
func (g *Client) BinStage(ctx context.Context, name string, r io.Reader) error {
	h := sha256.New()
	buf := make([]byte, BinChunkSize)
	seq := 0
	for {
		n, err := io.ReadFull(r, buf)
		last := errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
		if err != nil && !last {
			return err
		}
		h.Write(buf[:n])
		c := BinChunk{Name: name, Seq: seq, Data: buf[:n], Last: last}
		if last {
			c.SHA256 = hex.EncodeToString(h.Sum(nil))
		}
		if err := g.c.call(ctx, verbBinStage, c, nil); err != nil {
			return err
		}
		if last {
			return nil
		}
		seq++
	}
}

// BinTest has the guest run every staged binary's --test-launch. An error names the binary
// that failed; the guest has discarded the whole staged set by then.
func (g *Client) BinTest(ctx context.Context, names []string) error {
	return g.c.call(ctx, verbBinTest, BinTest{Names: names}, nil)
}

// BinActivate arms the staged set and restarts the guest agent's unit, whose start is the
// trial. The call returns once the guest has accepted the restart; the channel then drops, and
// the caller learns the outcome from the next handshake's Bundle.
func (g *Client) BinActivate(ctx context.Context, release string, names []string) error {
	return g.c.call(ctx, verbBinActivate, BinActivation{Release: release, Names: names}, nil)
}

func (g *Client) SupportsBinPush() bool {
	return g.Supports(verbBinStage) && g.Supports(verbBinTest) && g.Supports(verbBinActivate)
}

// handleBin is the dispatch arm for the three verbs.
func handleBin(ctx context.Context, x Executor, verb string, payload json.RawMessage) (any, error) {
	switch verb {
	case verbBinStage:
		var c BinChunk
		if err := json.Unmarshal(payload, &c); err != nil {
			return nil, err
		}
		return nil, stageChunk(c)
	case verbBinTest:
		var t BinTest
		if err := json.Unmarshal(payload, &t); err != nil {
			return nil, err
		}
		return nil, testLaunch(ctx, x, t)
	case verbBinActivate:
		var a BinActivation
		if err := json.Unmarshal(payload, &a); err != nil {
			return nil, err
		}
		return nil, activate(ctx, x, a)
	}
	return nil, fmt.Errorf("guestagent: %s is not a bin verb", verb)
}

// Bundle is the guest bundle the guest reported in its last handshake: the host release id its
// pushed binaries came from, "" while it runs the firmware baked into the image.
func (g *Client) Bundle() string { return g.bundle }
