package platform

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"time"
)

// QMP -- QEMU's machine protocol -- is the host's control channel to the VM itself, as
// distinct from the virtio-serial channel, which reaches the guest OS *inside* it.
// The distinction is the point: everything this file does must keep working when the guest
// OS is wedged, mid-reboot, or not yet booted.
//
// It is line-oriented JSON over a unix socket QEMU serves -- the same shape the agent
// already speaks to the guest agent -- so it needs no new process, no daemon, and no
// coordination. A connection is dialled per call rather than held open: these are rare,
// deliberate operations, and a long-lived handle to total-control-of-the-VM is a liability
// with no benefit.
//
// SECURITY. QMP is unrestricted control of the VM: it can reset it, stop it, and dump guest
// RAM to a file. There is no capability subsetting to hide behind, so the containment is the
// filesystem -- the socket lives in a directory the agent creates 0700 root (see Launch).
// Connecting to a unix socket requires write permission on it, and QEMU creates it 0755, so
// the directory is the second lock rather than the only one; it is deliberate either way.

// qmpTimeout bounds a single request/response exchange. QMP replies are effectively
// immediate (QEMU answers from its main loop); this exists so a wedged QEMU surfaces as an
// error instead of a hang, since the whole point of this channel is to act when things are
// already going wrong.
const qmpTimeout = 10 * time.Second

// qmpConn is one dialled, capability-negotiated QMP session.
type qmpConn struct {
	c   net.Conn
	dec *json.Decoder
}

// qmpMessage is the union QEMU sends: a greeting, a command reply, an error, or an
// asynchronous event. Exactly one field is populated per message.
type qmpMessage struct {
	QMP    json.RawMessage `json:"QMP"`
	Return json.RawMessage `json:"return"`
	Error  *struct {
		Class string `json:"class"`
		Desc  string `json:"desc"`
	} `json:"error"`
	Event string `json:"event"`
}

// dialQMP connects and completes the capabilities handshake, after which QEMU accepts
// commands. QMP opens in "capabilities negotiation mode" and rejects everything else until
// qmp_capabilities has been executed, so this is not optional politeness.
func dialQMP(ctx context.Context, path string) (*qmpConn, error) {
	if path == "" {
		return nil, fmt.Errorf("platform: no QMP socket configured for this guest")
	}
	var d net.Dialer
	c, err := d.DialContext(ctx, "unix", path)
	if err != nil {
		return nil, fmt.Errorf("platform: dial QMP %s: %w", path, err)
	}
	q := &qmpConn{c: c, dec: json.NewDecoder(c)}
	// THE HANDSHAKE IS BOUNDED BY qmpTimeout, THE COMMAND BY THE CALLER'S CONTEXT, and the two
	// must not share a deadline. This used to hand the caller's deadline to both, which read as
	// "the caller decides how long it is willing to wait" and behaved as a hang: a frozen QEMU
	// never sends the greeting, and a caller with a long budget then blocks on a Decode for the
	// whole of it. The recovery ladder (host.rebootGuest) budgets ~6.5 minutes for stop +
	// relaunch, so its ACPI attempt sat in this Decode for that whole budget before it could fall
	// back to the forced stop -- found by nixosTest/agent-recover, which SIGSTOPs QEMU and is
	// the first caller whose guest is frozen rather than merely unhealthy. The rollback leg
	// (osUpgrade.restore) carries the identical budget and had the identical latent hang; it had
	// simply never met a QEMU that was not answering, because a guest that fails a health gate
	// still has a live monitor.
	//
	// The split is not a compromise between the two numbers, it is the honest bound for each.
	// Connection setup on a local unix socket is sub-second on any QEMU that is alive at all, so
	// no useful caller is served by waiting longer -- silence here means the process is gone or
	// stopped, which more waiting cannot change. A COMMAND is different: SnapshotCreateLive can
	// legitimately run for minutes, so capping the whole conversation at qmpTimeout would have
	// traded this hang for a broken snapshot. Hence the deadline is tightened for the handshake
	// and handed back to the caller's context before the command runs.
	setupDeadline := time.Now().Add(qmpTimeout)
	if deadline, ok := ctx.Deadline(); ok && deadline.Before(setupDeadline) {
		setupDeadline = deadline // a caller in more of a hurry than we are still wins
	}
	_ = c.SetDeadline(setupDeadline)
	// The greeting arrives unprompted; read it before speaking, or the handshake races it.
	var greeting qmpMessage
	if err := q.dec.Decode(&greeting); err != nil {
		q.close()
		return nil, fmt.Errorf("platform: QMP greeting: %w", err)
	}
	if greeting.QMP == nil {
		q.close()
		return nil, fmt.Errorf("platform: QMP: first message was not a greeting")
	}
	if _, err := q.execute("qmp_capabilities", nil); err != nil {
		q.close()
		return nil, err
	}
	// Handshake done: restore exactly what the command would have got before -- the caller's
	// deadline, or qmpTimeout when it set none. Only the setup phase above changes, so a
	// long-running command keeps the budget its caller chose and a deadline-less caller keeps
	// the bound it has always had.
	cmdDeadline := time.Now().Add(qmpTimeout)
	if deadline, ok := ctx.Deadline(); ok {
		cmdDeadline = deadline
	}
	_ = c.SetDeadline(cmdDeadline)
	return q, nil
}

// Execute sends one command and returns its result. Asynchronous events (SHUTDOWN,
// POWERDOWN, RESET, …) can arrive at any time, including between the command and its reply,
// so they are skipped rather than mistaken for one -- the single subtlety of the protocol.
func (q *qmpConn) execute(cmd string, args any) (json.RawMessage, error) {
	req := map[string]any{"execute": cmd}
	if args != nil {
		req["arguments"] = args
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	if _, err := q.c.Write(append(body, '\n')); err != nil {
		return nil, fmt.Errorf("platform: QMP %s: write: %w", cmd, err)
	}
	for {
		var msg qmpMessage
		if err := q.dec.Decode(&msg); err != nil {
			return nil, fmt.Errorf("platform: QMP %s: read: %w", cmd, err)
		}
		switch {
		case msg.Event != "":
			continue // asynchronous, not our reply
		case msg.Error != nil:
			return nil, fmt.Errorf("platform: QMP %s: %s: %s", cmd, msg.Error.Class, msg.Error.Desc)
		case msg.Return != nil:
			return msg.Return, nil
		default:
			return nil, fmt.Errorf("platform: QMP %s: unrecognised reply", cmd)
		}
	}
}

func (q *qmpConn) close() error { return q.c.Close() }

// qmpExecute dials, runs one command, and hangs up.
func qmpExecute(ctx context.Context, path, cmd string, args any) (json.RawMessage, error) {
	q, err := dialQMP(ctx, path)
	if err != nil {
		return nil, err
	}
	defer q.close()
	return q.execute(cmd, args)
}

// Shutdown asks the guest OS to power off cleanly (an ACPI power-button event) and waits
// until the VM is actually gone.
//
// This is the missing counterpart to Stop, which stops the transient unit and so *kills*
// QEMU -- a power cut, from the guest's point of view. That was the only host-side stop
// there was, which made the reboot leg of an OS upgrade begin by inflicting the exact
// failure the upgrade's snapshot exists to insure against. Measured cost of getting this
// wrong: power-cutting a guest moments after it rewrote its own bootloader hung the next
// boot about half the time.
//
// It does NOT escalate on its own. A guest that ignores ACPI is a decision, not a detail --
// the caller may want to snapshot, alert, or wait longer before resorting to Stop -- so this
// reports the failure and leaves the choice upstream.
func (g *Guest) Shutdown(ctx context.Context, grace time.Duration) error {
	if g == nil || g.unit == "" {
		return nil
	}
	if _, err := qmpExecute(ctx, g.QMPSock, "system_powerdown", nil); err != nil {
		return err
	}
	return g.WaitStopped(ctx, grace)
}

// GuestShutdownGrace bounds the clean powerdown ShutdownVM waits for. A NixOS guest's own
// shutdown was measured at ~25s, so this is generous rather than tight; the unit's
// TimeoutStopSec is set above it so systemd's SIGKILL arrives after we have given up, never
// through the middle of a shutdown that was working.
const GuestShutdownGrace = 60 * time.Second

// ShutdownVM is Guest.Shutdown for a caller that has a socket path and nothing else: the guest
// unit's own ExecStop, which runs as a separate process with no Guest handle to hold.
//
// It exists as a sibling rather than a reuse because of one detail that would otherwise make it
// useless here, in either direction the underlying check could go. Guest.Shutdown confirms the
// stop with WaitStopped, which judges the unit by its ActiveState -- and inside its own unit's
// ExecStop the unit reads "deactivating" for as long as this function runs. Reading that as
// stopped (`systemctl is-active`, which is what WaitStopped did before [B.103]) returns SUCCESS
// immediately, reporting a clean shutdown while the guest is still flushing, and systemd goes on
// to kill QEMU underneath it; reading it as still-running, which is what WaitStopped does now,
// waits on a state only this function's own return can clear. Vacuous one way, self-deadlocked
// the other.
//
// So this judges the VM by the MONITOR SOCKET instead, which belongs to QEMU rather than to
// systemd's opinion of QEMU: a dial that no longer connects means the process is gone (QEMU
// unlinks the socket at exit; a leftover file refuses the connection instead -- both are a
// failed dial, so neither needs special-casing).
//
// A socket that is already unreachable on entry is success, not failure. The commonest caller is
// a stop that follows something else having killed QEMU already (Stop's SIGKILL, a crash), and
// "there is no VM to power down" is that request satisfied.
func ShutdownVM(ctx context.Context, qmpSock string, grace time.Duration) error {
	if !qmpReachable(ctx, qmpSock) {
		return nil // nothing there to power down
	}
	if _, err := qmpExecute(ctx, qmpSock, "system_powerdown", nil); err != nil {
		if !qmpReachable(ctx, qmpSock) {
			return nil // it went away while we were asking -- the request is moot, not failed
		}
		return err
	}
	deadline := time.Now().Add(grace)
	for {
		if !qmpReachable(ctx, qmpSock) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("platform: guest at %s still running %s after the power button", qmpSock, grace)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// qmpReachable reports whether anything is still listening on the monitor socket -- the probe
// ShutdownVM uses for "is QEMU gone". It dials and hangs up without speaking QMP: the handshake
// would tell us nothing more and costs a round trip on a process that is busy dying.
func qmpReachable(ctx context.Context, path string) bool {
	if path == "" {
		return false
	}
	dial, cancel := context.WithTimeout(ctx, qmpTimeout)
	defer cancel()
	var d net.Dialer
	c, err := d.DialContext(dial, "unix", path)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

// Accelerated reports whether the running VM is actually being accelerated by KVM, and
// whether the host has KVM at all -- QEMU's own answer (`query-kvm`), not an inference from
// the argv or from /dev/kvm.
//
// It has to be asked rather than assumed, because QEMUSpec.Accel is a fallback LIST
// ("kvm:tcg"): the command line says what the host REQUESTED and only the VM knows what it
// got. The fallback is deliberate and supported (the no-virt host), but it is also SILENT and
// costs roughly an order of magnitude of guest speed, so the one place it must not stay silent
// is the log a future performance question starts from (host.bringUp logs it).
//
// `present` distinguishes the two ways to land in emulation, which have different fixes:
// present=false is a host with no virtualisation extensions available at all (usually
// firmware, or a VM without nested virt), while present=true with enabled=false means KVM is
// there and something stopped us using it (permissions on /dev/kvm, a module not loaded).
func (g *Guest) Accelerated(ctx context.Context) (enabled, present bool, err error) {
	if g == nil {
		return false, false, fmt.Errorf("platform: no guest to ask about acceleration")
	}
	raw, err := qmpExecute(ctx, g.QMPSock, "query-kvm", nil)
	if err != nil {
		return false, false, err
	}
	var out struct {
		Enabled bool `json:"enabled"`
		Present bool `json:"present"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return false, false, fmt.Errorf("platform: query-kvm: %w", err)
	}
	return out.Enabled, out.Present, nil
}

// WaitStopped blocks until the VM is actually gone, which is the only honest confirmation
// that a shutdown happened: every way of ASKING for one (the guest agent's os.poweroff, the
// ACPI button) returns as soon as the request is accepted, and the caller's next act is
// usually to touch the disk QEMU still has open.
//
// "Gone" means the unit is at rest, not merely non-"active" -- see unitAtRest and [B.103].
func (g *Guest) WaitStopped(ctx context.Context, grace time.Duration) error {
	if g == nil || g.unit == "" {
		return nil
	}
	deadline := time.Now().Add(grace)
	for {
		if unitStopped(g.unit) {
			return nil
		}
		if time.Now().After(deadline) {
			// Report what the unit still looks like: "the guest ignored us" and "it powered
			// off but the unit lingered" are different faults with the same symptom.
			state, _ := unitState(g.unit)
			return fmt.Errorf("platform: guest %s still running %s after being asked to stop (unit is %q)",
				g.unit, grace, state)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// SnapshotCreateLive takes the OS-upgrade rollback point on a RUNNING guest's root disk --
// the switch path's half of what QEMUSpec.SnapshotCreate does for the reboot path.
//
// Going through QEMU is not a preference here: an in-band activation keeps the VM up, that
// being the whole definition of switch-only, and qemu-img refuses an image QEMU holds open.
//
// The snapshot it takes is therefore CRASH-CONSISTENT, and there is no alternative on this
// path. That is an accepted residual rather than an oversight: (c1) only routes a target here
// when it touches no kernel, initrd, module or boot parameter, so what can go wrong is
// userland, and a restore is the fallback for the rarer case where it goes wrong in a way that
// also disturbed state outside the closure.
//
// Unlike its siblings below, a guest that is not there is an ERROR, not a no-op. The two read
// alike and are opposites: "nothing to stop" and "nothing to drop" are true statements about a
// missing VM, while returning nil here would tell the caller it holds a rollback point it does
// not have -- and the caller's next act is to replace the running system.
func (g *Guest) SnapshotCreateLive(ctx context.Context) error {
	if g == nil || g.unit == "" {
		return fmt.Errorf("platform: no running guest to snapshot")
	}
	_, err := qmpExecute(ctx, g.QMPSock, "blockdev-snapshot-internal-sync",
		map[string]any{"device": RootDriveID, "name": UpgradeSnapshot})
	return err
}

// SnapshotDropLive deletes the OS-upgrade rollback point from a RUNNING guest's root disk.
//
// It exists because of where the reboot path ends up: the upgrade is committed only once the
// guest has booted the new generation and passed its health-gate, so at the moment the
// snapshot becomes garbage the VM is up and must stay up. qemu-img cannot touch an image QEMU
// holds open, so the delete has to go through QEMU itself. This and QEMUSpec.SnapshotDelete
// are two ways to reach one snapshot rather than two mechanisms, and which applies is settled
// entirely by whether the VM is running: the rollback leg has already stopped it, the commit
// leg has not.
//
// Leaving the snapshot behind is the cost of not having this: its fixed tag's presence IS the
// answer to "was an upgrade in flight?" (snapshot.go), so an undeleted one tells every later
// restart it interrupted an upgrade.
func (g *Guest) SnapshotDropLive(ctx context.Context) error {
	if g == nil || g.unit == "" {
		return nil
	}
	_, err := qmpExecute(ctx, g.QMPSock, "blockdev-snapshot-delete-internal-sync",
		map[string]any{"device": RootDriveID, "name": UpgradeSnapshot})
	return err
}

// Reset forces an immediate VM reset -- the hard reboot for a guest that is wedged past
// talking to (its agent unreachable, ACPI ignored). It is the fallback path, not the reboot
// mechanism: a planned reboot shuts down cleanly and relaunches, because only a relaunch can
// change what the VM boots (the selector is a launch property).
func (g *Guest) Reset(ctx context.Context) error {
	if g == nil || g.unit == "" {
		return nil
	}
	_, err := qmpExecute(ctx, g.QMPSock, "system_reset", nil)
	return err
}
