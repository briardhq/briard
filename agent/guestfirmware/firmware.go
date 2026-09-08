// Package guestfirmware is the FIRMWARE half of the host<->guest control protocol: the part the
// guest image bakes, and the only part whose change moves the guest image's inputs hash
// ([B.86i], [B.139]).
//
// It is a deliberately small graph -- the framing, the handshake, the three push verbs and their
// trial/aftermath rules, `os.poweroff`, and the Executor those shell out through. Everything
// else the guest does (DRBD bring-up, services, telemetry, converge, the deadman) belongs to
// agent/guestagent, which the host PUSHES as `briard-guest-agent` at every bring-up. The whole
// point of the split is that an edit to any of those must NOT rebuild the image: the image's
// inputs list is `go list -deps ./agent/cmd/briard-guest-firmware` and nothing more
// (flake.nix guestInputPackages, asserted by internal/arch).
//
// So the rule for editing this package is the rule for the protocol itself: additive only. A
// pushed binary can change everything about itself except the picker it is started by
// (guest-image/pivot.nix), `--test-launch`, and the verbs it is reached through.
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

// The host<->guest control protocol's own version, and the handshake that carries it. It lives
// HERE, not in shared/api, because the firmware is what answers a handshake before anything has
// been pushed, and hashing the whole of shared/ into the image's inputs would republish a 400 MB
// guest chain for every unrelated shared/ edit ([B.139]). It is not north-bound telemetry --
// nothing here leaves the house -- so shared/api's audited allowlist is not the register it
// belongs in.
//
// The host and guest agents version this wire protocol so they can evolve on independent
// cadences and so the host detects skew during a rolling update/failover -- a survivor's guest
// may run a newer OS generation (thus a newer guest agent) than the host was built against. The
// host handshakes on connect and refuses a guest whose protocol it can't speak: a safe deferral
// (bring-up/upgrade fails -> rollback / no promotion) beats silent misbehaviour.
//
// VERSION 2 (2026-08-29) renamed five verbs from `payload.*` to `service.*` (agent/guestagent,
// and see the note at that const block). A rename is the one change a capability handshake
// cannot absorb -- the guest advertises names, so a rolled host meeting a v1 guest finds none of
// them -- which is precisely what MinGuestProtocol is for: refuse at the handshake rather than
// fail five verbs in a row. Raising the FLOOR, not just the ceiling, is deliberate and is
// affordable only under the alpha reinstall-only policy ([[alpha-reinstall-only-policy]]): every
// node re-runs the installer, so there is no fleet to strand. Note what it costs when that
// policy ends -- the host agent self-updates independently of the guest OS closure ([V3.4]), so
// a floor raise makes every host refuse every not-yet-rolled guest fleet-wide and its own health
// gate then reverts the self-update.
const (
	GuestProtocol    = 2 // the current host<->guest wire protocol version
	MinGuestProtocol = 2 // the oldest guest protocol this host can still drive
)

// The two verbs the firmware serves besides the three push ones: the handshake, and the clean
// shutdown. Poweroff is here because the host may have to stop a guest it has never dressed --
// an aborted bring-up, a refused bundle -- and the ACPI button is the fallback for a guest with
// no agent at all, not for one whose agent is answering.
const (
	VerbHello      = "hello"       // protocol handshake: version + capabilities
	VerbOSPowerOff = "os.poweroff" // ask the guest OS to shut itself down cleanly
)

// Capabilities is what the FIRMWARE advertises: the handshake, the three push verbs, and the
// clean shutdown. Everything else a host might ask for belongs to the pushed agent, and a host
// meeting this list learns from it that this guest has not been dressed yet -- which is exactly
// what makes it dress the guest before any real verb (agent/host/dress.go).
var Capabilities = []string{VerbHello, VerbBinStage, VerbBinTest, VerbBinActivate, VerbOSPowerOff}

// Hello is the guest's handshake reply: its protocol version plus the verbs it
// supports (fine-grained capability negotiation on top of the coarse version gate), and
// which BOOT of the guest is answering.
//
// BootID is the guest kernel's boot_id, and it is the host's only way to tell "the in-guest
// agent bounced" from "the guest rebooted underneath me" -- two events that look identical
// on the channel and need opposite responses ([B.102]). The agent serves one connection then
// exits, so a handshake re-running proves nothing; the boot_id is stable across that and
// changes only across an actual boot. Empty from a guest too old to send it, which reads as
// "no evidence" rather than "a new boot" -- the host must not re-converge on silence.
type Hello struct {
	Version      int      `json:"version"`
	Capabilities []string `json:"capabilities,omitempty"`
	BootID       string   `json:"boot_id,omitempty"`
	// Bundle is the guest bundle this agent RUNS ([B.86j]): the host release id whose
	// pushed binaries it was started from, or "" when it runs the firmware baked into the
	// image. The host compares it with the bundle it holds and dresses the guest when they
	// differ -- at bring-up, after a host commit, after any guest restart (the overlay is
	// disposable, so every boot starts as firmware).
	Bundle string `json:"bundle,omitempty"`
}

// bootIDPath is the kernel's per-boot identifier, which the handshake reports so the host can
// recognise a guest that rebooted underneath it ([B.102]). The kernel mints it once per boot and
// it survives every in-guest agent restart, which is exactly the line the host needs drawn --
// and unlike a hostname or an address it is not something bring-up sets, so it cannot be
// confused with the convergence it is used to trigger.
const bootIDPath = "/proc/sys/kernel/random/boot_id"

// HelloReply answers the handshake with the given capability list -- Capabilities from the
// firmware, the full verb set from the pushed agent. No side effects; safe to call before
// anything else.
//
// The boot_id rides along because the host cannot otherwise tell a bounced agent from a
// rebooted guest ([B.102]). Read best-effort: a hello that FAILED would be a guest the host
// refuses to drive, and no host has ever needed this field to drive one -- so an unreadable
// boot_id is reported as absent, not as an error.
func HelloReply(x Executor, caps []string) Hello {
	h := Hello{Version: GuestProtocol, Capabilities: caps, Bundle: runningBundle()}
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
	// ReadFile returns a file's contents. A MISSING file must come back as an error the caller
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

// PowerOffGrace bounds the one handler that runs DETACHED from the serve context. Every other
// verb is cancelled when the agent is asked to stop, and that is correct -- a service pull fetching a
// closure has nothing worth finishing once the machine is going down. `os.poweroff` is the
// exception, and the reason is circular: the shutdown it asks for is what SIGTERMs this process,
// so inheriting that cancellation means the verb is killed by its own success ([B.132]).
//
// Detached, it needs a ceiling of its own -- nothing else can stop it, and the handler holds the
// serve loop's reply lock, which holds the control port open. Sized far above what the command
// actually costs (~100ms: `--no-block` returns once systemd has ENQUEUED the job, not run it) and
// far below the caller's exit backstop, so the reply is always written before the process ends.
// The gap to that backstop is asserted at compile time where the backstop is declared.
const PowerOffGrace = 2 * time.Second

// powerOffPollEvery paces the state probe below. Short relative to PowerOffGrace, because the gap
// it covers -- systemd ACCEPTING the shutdown job, then the manager entering it -- is milliseconds
// on a healthy machine and worth several attempts on a loaded one.
const powerOffPollEvery = 100 * time.Millisecond

// systemStopping answers the one question that settles `os.poweroff`: has the manager entered
// shutdown? It exists because the command that asks for one is routinely killed BY the shutdown it
// asks for, so its exit status reports a failure for a request that worked. Two mechanisms did
// that, in sequence -- the dispatch context ([B.132]) and then systemd's own cgroup SIGTERM -- and
// the second is what makes this the right shape rather than a third patch on the first: the verb
// stops inferring its outcome from how a process died and observes the state it claims to cause.
//
// ⚠️ THE PROBE'S EXIT CODE IS NOT THE ANSWER EITHER, and it is the trap this function exists to
// contain: `systemctl is-system-running` exits 0 ONLY for `running`. `stopping` -- the state being
// looked for -- exits NON-ZERO, as do `degraded` and `starting`. Written as an ordinary success
// check it would invert its own result and call a working shutdown a failure, which is the bug one
// level up wearing a different hat. THE STDOUT IS THE ANSWER; the status is discarded on purpose.
//
// POLLED RATHER THAN READ ONCE, for two independent reasons. `--no-block` returns when the job is
// ENQUEUED, which is not the instant the manager flips to `stopping`, so a single immediate read
// can land in that gap. And this child sits in the same cgroup as the one whose death started all
// this, so the probe can be signalled too -- a fix that loses to the race it settles is no fix.
//
// COST, since it is not free: a genuine refusal now takes the full grace to report instead of
// returning at once, because "not stopping yet" and "never going to stop" look the same until the
// window closes. Bounded by the caller's context, two seconds, against a host-side shutdown grace
// measured in tens of them -- and the escalation it delays is the one this whole path exists to
// avoid firing spuriously.
func systemStopping(ctx context.Context, x Executor) bool {
	// STRICT ON PURPOSE about everything that is not the word `stopping`. A shutdown far enough
	// along that the manager no longer answers reports `offline`, or nothing at all, and a machine
	// genuinely going down can reach that inside this window. Not read as success: "too far gone to
	// answer" and "never started" are indistinguishable from in here, and guessing between them is
	// the habit this function exists to break. Being wrong costs a bounded, already-budgeted
	// escalation -- the host presses the ACPI button and then asks the question that does settle
	// it, whether the VM is still there (host.stopCleanly, [B.98]).
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

// PowerOff serves `os.poweroff`.
//
// The FIRST-CHOICE clean shutdown: ask the guest OS directly, over the channel
// the host already has, instead of rattling its virtual power button and hoping
// something inside is listening. QMP's ACPI path (platform.Guest.Shutdown) stays
// as the fallback for a guest whose agent is gone -- the two fail independently,
// which is the whole reason to have both.
//
// --no-block because the reply must be written BEFORE systemd starts tearing the
// machine down: without it the shutdown races the response and the host sees a
// dead channel, which is indistinguishable from a guest that crashed. The host
// confirms the outcome by watching the VM disappear, not by this return value
// (platform.Guest.WaitStopped).
//
// ⚠️ DETACHED FROM THE DISPATCH CONTEXT, because this verb is cancelled by its own
// success. `--no-block` returns once systemd has enqueued the shutdown, but PID 1
// begins stopping units as soon as it holds the job -- and nothing orders this
// agent's unit late, so its SIGTERM can land while `systemctl` is still exiting.
// That cancels the dispatch context, exec.CommandContext kills the child, and Wait
// reports `context canceled` for a request that WORKED. The host reads that as "the
// agent route failed" and reaches for the ACPI power button -- the [B.85] regression
// guest-rescue greps for, seen live on the nightly 2026-09-03 ([B.132]).
//
// [B.127] fixed the neighbouring half: the serve loop now holds the control port
// open until this reply is written, so the answer does reach the host. That is why
// the failure changed shape from a lost reply into a delivered error -- and why the
// fix belongs here rather than in the channel. The reply was never the problem; the
// command underneath it was.
// ⚠️ AND THE COMMAND'S EXIT STATUS IS STILL NOT THE ANSWER. Detaching the context
// stopped US killing the child; it does not stop SYSTEMD killing it. The unit's
// default KillMode is control-group, so the shutdown this verb starts SIGTERMs
// every process in the agent's cgroup -- `systemctl` included -- and the error
// changes shape again, from `context canceled` to `signal: terminated`. Measured on
// run 33978948462, one sample in four on an idle L0: the host read a request that
// had WORKED as a refusal and reached for the ACPI button, which is [B.85] going
// red for the third distinct reason.
//
// So stop inferring the outcome from how the command died and ASK THE MANAGER. Being
// signalled is not evidence the job was enqueued -- `--no-block` returns once systemd
// has accepted it, and a SIGTERM landing before that would have us report a shutdown
// that never started. `is-system-running` reporting `stopping` IS that evidence, and
// it is the same fact the verb claims to have caused, observed rather than guessed.
//
// The exit status is consulted first and only as a fast path: a command that returned
// cleanly needs no second question. Every non-`stopping` outcome keeps its ORIGINAL
// error, so a genuine refusal still reaches the host with the text that says why, and
// the host still escalates. What this removes is the false negative, not the alarm.
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

// Serve is the FIRMWARE's dispatch loop: the handshake, the three push verbs and the clean
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
	})
}
