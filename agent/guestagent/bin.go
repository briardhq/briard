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
)

// THE PUSH PROTOCOL ([B.86j]): every briard binary the guest runs rides the HOST bundle, and
// the host dresses the guest with them over this channel -- at bring-up, after a host commit,
// after any guest restart. The image bakes a FIRMWARE copy of each binary whose stable core is
// exactly these two verbs plus the handshake; a pushed binary lives beside the firmware in a
// disposable directory and is what the unit runs until the next boot.
//
// The guest's side of the frozen pivot is the shell picker the image installs as each unit's
// ExecStart (guest-image/disk-image.nix `briard-bin-exec`): trial -> pushed -> baked, a
// single-use trial flag in /run, Type=notify with READY at listen, and an ExecStartPost that
// commits `<name>.next` only after READY -- the same shape as the host's briard-exec/commit
// (scripts/install.sh, [B.84]), so a pushed binary that will not start reverts on the next
// start with no channel and no timer. `bin.activate` only arms and restarts; the picker does
// the rest, and the host reads the outcome in the next handshake's Bundle.
//
// Frozen contract: bin.stage and bin.activate are versioned additively, like briard-exec.
const (
	verbBinStage    = "bin.stage"    // one chunk of a binary into <bin>/<name>.part; the last chunk verifies + renames to <name>.next
	verbBinActivate = "bin.activate" // arm every named binary's trial flag and restart its unit (detached; the agent's own comes last)
)

// BinChunk is the wire unit of bin.stage. Data rides as JSON base64 (encoding/json does that
// for []byte), sized by the client under the channel's frame cap.
type BinChunk struct {
	Name   string `json:"name"`             // the binary: briard-guest-agent | briard-reverse-proxy
	Seq    int    `json:"seq"`              // 0 starts the file (truncates any earlier attempt)
	Data   []byte `json:"data"`             // this chunk's bytes
	Last   bool   `json:"last"`             // after this chunk the file is complete
	SHA256 string `json:"sha256,omitempty"` // with Last: the whole file's digest, checked before it becomes .next
}

// BinActivation names the bundle and the binaries to arm.
type BinActivation struct {
	Release string   `json:"release"` // the host release id these binaries came from -- what Bundle reports after the commit
	Names   []string `json:"names"`   // in activation order; the guest agent's own binary must come LAST (its restart ends the channel)
}

// BinChunkSize is what the client sends per frame: comfortably under wire.go's 8 MiB cap once
// base64-encoded, and small enough that the guest never holds more than one chunk in memory.
const BinChunkSize = 4 << 20

// The binaries the guest can be dressed with, and the unit each one runs under. Closed on
// purpose: a name outside this table is refused before a byte is written, so the verb cannot
// be turned into "write any file"; and the ORDER is the activation order the client enforces.
var binUnits = map[string]string{
	"briard-reverse-proxy": "briard-reverse-proxy.service",
	"briard-guest-agent":   "briard-guest-agent.service",
}

// BinNames is the activation order: the front door first (a sub-second blip on a serving
// node, nothing about the workload), the guest agent LAST -- its restart drops the channel the
// activation itself came over, so nothing may follow it.
var BinNames = []string{"briard-reverse-proxy", selfBin}

// selfBin is this agent's own binary: activating it restarts the unit serving the activation.
const selfBin = "briard-guest-agent"

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

// activate arms the trial flags and restarts the units, in the order given. It refuses to arm
// a name whose .next is not staged (the picker would exec nothing) and refuses the release id
// unless it is a plain segment (it becomes a file's contents and a handshake field).
//
// The restarts are `--no-block` on a context detached from the dispatch one, for the reason
// os.poweroff spells out: the LAST restart is this agent's own unit, and the SIGTERM it earns
// would otherwise cancel the command that asked for it. The reply is written before any of them
// take effect only because the serve loop holds the port until it is ([B.127]).
func activate(ctx context.Context, x Executor, a BinActivation) error {
	if a.Release == "" || strings.ContainsAny(a.Release, "/ \n") {
		return fmt.Errorf("bin.activate: bad release id %q", a.Release)
	}
	if len(a.Names) == 0 {
		return errors.New("bin.activate: nothing to activate")
	}
	dir, run := binDir(), binRunDir()
	for _, n := range a.Names {
		if _, ok := binUnits[n]; !ok {
			return fmt.Errorf("bin.activate: %q is not a binary this guest runs", n)
		}
		if _, err := os.Stat(filepath.Join(dir, n+nextSuffix)); err != nil {
			return fmt.Errorf("bin.activate: %s is not staged: %w", n, err)
		}
	}
	if err := os.MkdirAll(run, 0o755); err != nil {
		return err
	}
	// `try-restart`, never `restart`: the front door is a member of drbd-reactor's promotion
	// chain and runs only while this node holds the house. A plain restart would START it on a
	// standby -- outside the chain, binding a VIP the node does not hold. An inactive unit keeps
	// its trial flag and picks the pushed binary up at its next (promoter-driven) start.
	// The release commits with the LAST binary (the picker's commit moves RELEASE.next when it
	// commits the guest agent), so a handshake after a half-applied set reports the old bundle.
	if err := os.WriteFile(filepath.Join(dir, releaseFile+nextSuffix), []byte(a.Release+"\n"), 0o644); err != nil {
		return err
	}
	for _, n := range a.Names {
		if err := os.WriteFile(filepath.Join(run, n+updateSuffix), nil, 0o644); err != nil {
			return err
		}
	}
	rctx := context.WithoutCancel(ctx)
	for _, n := range a.Names {
		var out []byte
		var err error
		if n == selfBin {
			// OUR OWN UNIT, and the reason this is not a plain restart: the restart SIGTERMs this
			// process's whole cgroup -- `systemctl` included -- before it returns, so the command
			// dies with `signal: terminated` and the host reads a push that WORKED as a failure
			// (measured on install-macvtap, the first run of [B.86j]; os.poweroff hit the same
			// trap, [B.132]). A transient timer runs OUTSIDE this cgroup and fires after the reply
			// has left; the host then meets the restart as a dropped channel and reconnects.
			out, err = x.Run(rctx, "systemd-run", "--quiet", "--collect", "--on-active=1", "--timer-property=AccuracySec=100ms",
				"systemctl", "restart", binUnits[n])
		} else {
			out, err = x.Run(rctx, "systemctl", "try-restart", "--no-block", binUnits[n])
		}
		if err != nil {
			return fmt.Errorf("bin.activate: restart %s: %w: %s", binUnits[n], err, strings.TrimSpace(string(out)))
		}
	}
	return nil
}

// runningBundle is what the handshake reports: the bundle id when this process runs from the
// pushed directory, "" when it runs the firmware. A trial that has not committed yet reports
// the release it is trialling (RELEASE.next), so a host reconnecting between READY and the
// commit still reads the right answer.
func runningBundle() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	dir := binDir()
	if filepath.Dir(exe) != dir {
		return ""
	}
	for _, f := range []string{releaseFile, releaseFile + nextSuffix} {
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

// BinActivate arms the staged binaries and restarts their units, the guest agent's LAST. The
// call returns once the guest has accepted the restarts; the channel then drops (the agent's own
// unit restarts), and the caller learns the outcome from the next handshake's Bundle.
func (g *Client) BinActivate(ctx context.Context, release string, names []string) error {
	return g.c.call(ctx, verbBinActivate, BinActivation{Release: release, Names: names}, nil)
}

func (g *Client) SupportsBinPush() bool {
	return g.Supports(verbBinStage) && g.Supports(verbBinActivate)
}

// handleBin is the dispatch arm for both verbs.
func handleBin(ctx context.Context, x Executor, verb string, payload json.RawMessage) (any, error) {
	switch verb {
	case verbBinStage:
		var c BinChunk
		if err := json.Unmarshal(payload, &c); err != nil {
			return nil, err
		}
		return nil, stageChunk(c)
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
