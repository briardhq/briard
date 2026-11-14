// Package guestfirmware is the firmware half of the host<->guest control protocol: the part the
// guest image bakes, and the only part whose change moves the guest image's inputs hash.
//
// It is a deliberately small graph: the framing, the handshake, the three push verbs and their
// trial/aftermath rules, `os.poweroff`, and the Executor those shell out through. Everything else
// the guest does (DRBD bring-up, apps, telemetry, converge, the deadman) belongs to
// agent/guestagent, which the host pushes as `briard-guest-agent` at every bring-up. The split
// exists so that an edit to any of those must not rebuild the image: the image's Go inputs are
// `go list -deps ./agent/cmd/briard-guest-firmware` and nothing more (flake.nix
// guestInputPackages, asserted by internal/arch).
//
// Editing this package follows the protocol's own rule: additive only. A pushed binary can change
// everything about itself except the picker it is started by (guest-image/pivot.nix),
// `--test-launch`, and the verbs it is reached through.
package guestfirmware

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// ControlPort is the virtio-serial port name for the host<->guest channel;
// the guest device is ControlPortDev. The host launches QEMU with a virtserialport
// of this name; the guest agent opens ControlPortDev.
const (
	ControlPort    = "briard.control"
	ControlPortDev = "/dev/virtio-ports/" + ControlPort
)

// The handshake the channel opens with. It lives here, not in shared/api, because the firmware
// answers it before anything is pushed, and hashing all of shared/ into the image's inputs would
// republish a 400 MB guest chain for every unrelated shared/ edit; nor does it belong in
// shared/api's audited allowlist: it is not north-bound telemetry (nothing here leaves the house).
//
// ⚠️ There is no protocol version number, and adding one back is a decision, not a tidy-up.
// Negotiation is by verb name: the guest advertises the set it serves, and Client.Supports refuses
// exactly the one path a guest cannot serve while every other path keeps working
// (SupportsSnapshotMember is one such caller). A number can only refuse the whole channel, and the
// channel is what fixes a node: applying a vm release runs through the guest's os.* verbs
// (agent/host/guestupdate.go, osupgrade.go), so a host refusing the handshake can never reach the
// image that would satisfy it, a deadlock reachable from the call graph however the floor is
// chosen. Nor would a number measure anything: the guest agent is pushed at every bring-up from
// the host's own tree, so comparing versions with a guest the host dressed compares a constant
// with itself, and the only binary that can differ is this firmware, frozen and additive, whose
// push verbs SupportsBinPush already checks. For a guest it did not push, the host has facts
// richer than an integer: Bundle names the release the guest runs, BootID the boot answering.
//
// Cross-release floors that do exist ride the signed manifests: Manifest.MinBriard (a vm release
// naming the oldest briard that may install it) and Manifest.MinUpgradeFrom (a release naming its
// own upgrade floor); agent/install/fetch.go states which directions a floor may point and why a
// floor from briard at the VM is not one of them. What a name cannot absorb is a rename, which
// breaks a whole family of verbs at once (agent/guestagent); that is affordable only under the
// alpha reinstall-only policy (every node re-runs the installer, so there is no fleet to strand),
// and it is the policy, not a number on the wire, that makes it safe.

// The two verbs the firmware serves besides the three push ones: the handshake, and the clean
// shutdown. Poweroff is here because the host may have to stop a guest it has never dressed --
// an aborted bring-up, a refused bundle -- and the ACPI button is the fallback for a guest with
// no agent at all, not for one whose agent is answering.
const (
	VerbHello      = "hello"       // handshake: the verb set, the boot, the bundle
	VerbOSPowerOff = "os.poweroff" // ask the guest OS to shut itself down cleanly
	// VerbCancel stops a request in flight, by id. A protocol verb: the serve loop handles it,
	// no dispatch ever sees it, and it is not in Capabilities -- the host sends it when a
	// deadline abandons a call on a guest that is still answering (Conn.Call) and drops the
	// answer, so an older firmware's "unknown verb" costs nothing.
	VerbCancel = "cancel"
)

// Capabilities is what the firmware advertises: the handshake, the three push verbs, and the
// clean shutdown; everything else a host might ask for belongs to the pushed agent. What makes the
// host dress the guest before any real verb is Hello.Bundle ("" here) differing from the bundle
// it holds; this list tells it the guest can be dressed (agent/host/dress.go).
var Capabilities = []string{VerbHello, VerbBinStage, VerbBinTest, VerbBinActivate, VerbOSPowerOff}

// Hello is the guest's handshake reply: the verbs it serves (the whole negotiation; there is no
// version number above it, see the note at VerbHello), which boot of the guest is answering, and
// which bundle it runs.
//
// BootID is the guest kernel's boot_id, the host's only way to tell "the in-guest agent bounced"
// from "the guest rebooted underneath me": the two look identical on the channel and need opposite
// responses. The agent serves one connection then exits, so a re-run handshake proves nothing;
// boot_id is stable across that and changes only across an actual boot. Empty when the guest could
// not read it (HelloReply), which reads as "no evidence", not "a new boot": the host must not
// re-converge on silence.
type Hello struct {
	Capabilities []string `json:"capabilities,omitempty"`
	BootID       string   `json:"boot_id,omitempty"`
	// Bundle is the guest bundle this agent runs: the host release id whose
	// pushed binaries it was started from, or "" when it runs the firmware baked into the
	// image. The host compares it with the bundle it holds and dresses the guest when they
	// differ -- at bring-up, after a host commit, after any guest restart (the scratch is
	// emptied at boot, so every boot starts as firmware).
	Bundle string `json:"bundle,omitempty"`
}

// bootIDPath is the kernel's per-boot identifier, reported in the handshake so the host can
// recognise a guest that rebooted underneath it. Minted once per boot, it survives every in-guest
// agent restart, exactly the line the host needs drawn; and unlike a hostname or an address,
// bring-up does not set it, so it cannot be confused with the convergence it is used to trigger.
const bootIDPath = "/proc/sys/kernel/random/boot_id"

// HelloReply answers the handshake with the given capability list: Capabilities from the
// firmware, the full verb set from the pushed agent. No side effects; safe to call before
// anything else. The boot_id is best-effort: a failed hello would be a guest the host refuses to
// drive, and the host does not need this field to drive one, so an unreadable boot_id is reported
// as absent, not as an error.
func HelloReply(x Executor, caps []string) Hello {
	h := Hello{Capabilities: caps, Bundle: runningBundle()}
	if b, err := x.ReadFile(bootIDPath); err == nil {
		h.BootID = strings.TrimSpace(string(b))
	}
	return h
}

// Executor runs commands and writes files inside the guest. The real impl shells
// out (NewOSExecutor); tests supply a fake.
type Executor interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
	WriteFile(path string, data []byte) error
	// ReadFile returns a file's contents. A missing file must come back as an error the caller
	// can recognise with os.IsNotExist rather than as empty content: "no name is published" and
	// "the name is the empty string" are different answers, and only one of them is normal.
	ReadFile(path string) ([]byte, error)
	Sethostname(name string) error
}

// osExecutor is the real guest Executor: shell out + write files.
type osExecutor struct{}

// NewOSExecutor returns the production Executor: what both guest mains run with.
func NewOSExecutor() Executor { return osExecutor{} }

func (osExecutor) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

func (osExecutor) WriteFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func (osExecutor) ReadFile(path string) ([]byte, error) { return os.ReadFile(path) }

func (osExecutor) Sethostname(name string) error {
	return setHostname(name)
}

// PowerOffGrace bounds the one handler that runs detached from the serve context. Every other verb
// is rightly cancelled when the agent is asked to stop (an app pull has nothing worth finishing
// once the machine is going down); `os.poweroff` is the exception, because the shutdown it asks
// for is what SIGTERMs this process, so inheriting that cancellation would kill the verb by its
// own success. Detached, it needs a ceiling of its own: nothing else can stop it, and the handler
// holds the control port open (ServeFrames). Sized far above the command's actual cost (~100ms:
// `--no-block` returns once systemd has enqueued the job, not run it) and below the caller's exit
// backstop (guestStopGrace in both mains, which asserts the gap at compile time), so the reply is
// always written before the process ends.
const PowerOffGrace = 2 * time.Second

// powerOffPollEvery paces the state probe below. Short relative to PowerOffGrace, because the gap
// it covers -- systemd accepting the shutdown job, then the manager entering it -- is milliseconds
// on a healthy machine and worth several attempts on a loaded one.
const powerOffPollEvery = 100 * time.Millisecond

// systemStopping answers the one question that settles `os.poweroff`: has the manager entered
// shutdown? The command that asks for one is routinely killed by the shutdown it asks for, so its
// exit status reports failure for a request that worked (PowerOff); the verb observes the state it
// claims to cause instead of inferring it from how a process died.
//
// ⚠️ The probe's exit code is not the answer either: `systemctl is-system-running` exits 0 only
// for `running`, and `stopping` (the state being looked for) exits non-zero, as do `degraded` and
// `starting`, so an ordinary success check would call a working shutdown a failure. The stdout is
// the answer; the status is discarded on purpose.
//
// Polled rather than read once, for two independent reasons: `--no-block` returns when the job is
// enqueued, not the instant the manager flips to `stopping`, so a single immediate read can land
// in that gap; and this child shares the poweroff command's cgroup, so the probe can be signalled
// too. The cost: a genuine refusal takes the full grace to report instead of returning at once,
// because "not stopping yet" and "never going to stop" look the same until the window closes --
// bounded by the caller's context, two seconds, against a host-side shutdown grace of tens of
// them, and the escalation it delays is the one this path exists to avoid firing spuriously.
func systemStopping(ctx context.Context, x Executor) bool {
	// Strict on purpose about everything but the word `stopping`. A shutdown far enough along that
	// the manager no longer answers reports `offline` or nothing at all, and a machine genuinely
	// going down can reach that inside this window; it is not read as success, because "too far
	// gone to answer" and "never started" are indistinguishable from in here. Being wrong costs a
	// bounded, already-budgeted escalation: the host presses the ACPI button, then asks the
	// question that does settle it, whether the VM is still there (host.stopCleanly).
	for {
		// Error deliberately dropped: a probe that was killed, or a manager that answers
		// non-zero because it is not `running`, are both just "no answer yet, ask again".
		out, _ := x.Run(ctx, "systemctl", "is-system-running")
		if string(bytes.TrimSpace(out)) == "stopping" {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(powerOffPollEvery):
		}
	}
}

// PowerOff serves `os.poweroff`: the first-choice clean shutdown, asking the guest OS directly
// over the channel the host already has instead of rattling its virtual power button. QMP's ACPI
// path (platform.Guest.Shutdown) stays as the fallback for a guest whose agent is gone; the two
// fail independently, which is the whole reason to have both. The host confirms the outcome by
// watching the VM disappear (platform.Guest.WaitStopped), not by this return value.
//
// --no-block because the reply must be written before systemd starts tearing the machine down:
// otherwise the shutdown races the response and the host sees a dead channel, indistinguishable
// from a guest that crashed. ServeFrames holds the control port open until the reply is written.
//
// ⚠️ The command is killed by its own success in two ways, so its exit status is not the answer;
// either way the host would read a request that worked as a refusal and reach for the ACPI power
// button, the clean-shutdown regression nixosTest/guest-rescue.nix asserts against:
//   - PID 1 begins stopping units as soon as it holds the job, and nothing orders this agent's
//     unit late, so its SIGTERM can cancel the dispatch context while `systemctl` is still
//     exiting: `context canceled` for a request that worked. Hence the detached context.
//   - The unit's default KillMode is control-group, so the shutdown SIGTERMs every process in the
//     agent's cgroup, `systemctl` included: `signal: terminated` (one sample in four on an idle
//     runner). Detaching stops us killing the child; it does not stop systemd killing it.
//
// So the outcome is asked of the manager (systemStopping). Being signalled is not evidence the job
// was enqueued (a SIGTERM landing before systemd accepted it would report a shutdown that never
// started); `is-system-running` reporting `stopping` is. The exit status is consulted first and
// only as a fast path: a command that returned cleanly needs no second question. Every
// non-`stopping` outcome keeps its original error, so a genuine refusal still reaches the host
// with the text that says why, and the host still escalates: this removes the false negative, not
// the alarm.
func PowerOff(ctx context.Context, x Executor) error {
	pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), PowerOffGrace)
	defer cancel()
	out, perr := x.Run(pctx, "systemctl", "poweroff", "--no-block")
	if perr == nil || systemStopping(pctx, x) {
		return nil
	}
	if o := bytes.TrimSpace(out); len(o) > 0 {
		return fmt.Errorf("systemctl poweroff --no-block: %w: %s", perr, o)
	}
	return fmt.Errorf("systemctl poweroff --no-block: %w", perr)
}

// Serve is the firmware's dispatch loop: the handshake, the three push verbs and the clean
// shutdown, and a refusal for everything else. A guest running this has not been dressed yet,
// and the only thing worth doing to it is dressing it -- so a verb that belongs to the pushed
// agent is answered with what is actually wrong rather than with a bare "unknown verb".
func Serve(ctx context.Context, rw io.ReadWriteCloser, x Executor) error {
	return ServeFrames(ctx, rw, func(ctx context.Context, verb string, payload json.RawMessage) (any, error) {
		switch verb {
		case VerbHello:
			return HelloReply(x, Capabilities), nil
		case VerbBinStage, VerbBinTest, VerbBinActivate:
			return HandleBin(ctx, x, verb, payload)
		case VerbOSPowerOff:
			return nil, PowerOff(ctx, x)
		}
		return nil, fmt.Errorf("guestfirmware: %s is not a firmware verb -- this guest runs the image's firmware and has not been dressed with a bundle yet", verb)
	}, func(verb string) bool { return verb != VerbHello }) // the handshake answers beside a dress; the rest is one at a time
}
