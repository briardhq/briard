package platform

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os/exec"
	"strings"
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
	if deadline, ok := ctx.Deadline(); ok {
		_ = c.SetDeadline(deadline)
	} else {
		_ = c.SetDeadline(time.Now().Add(qmpTimeout))
	}
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
func (g *Guest) WaitStopped(ctx context.Context, grace time.Duration) error {
	if g == nil || g.unit == "" {
		return nil
	}
	deadline := time.Now().Add(grace)
	for {
		if !unitActive(g.unit) {
			return nil
		}
		if time.Now().After(deadline) {
			// Report what the unit still looks like: "the guest ignored us" and "it powered
			// off but the unit lingered" are different faults with the same symptom.
			state, _ := exec.Command("systemctl", "is-active", g.unit).Output()
			return fmt.Errorf("platform: guest %s still running %s after being asked to stop (unit is %q)",
				g.unit, grace, strings.TrimSpace(string(state)))
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
