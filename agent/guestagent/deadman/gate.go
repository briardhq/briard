package deadman

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"sync"
	"time"
)

// THE GATE, SERVED OUTWARD.
//
// The host holds the same reflex from outside the VM (host/guestrecover.go), and it fires on the
// death of the control channel — so every question that would gate its decision is one it can no
// longer ask. It cannot read DRBD (in the guest), it cannot ask the guest agent (that is the
// thing that stopped answering), and under the default macvtap substrate it cannot reach the VIP.
//
// So the deadman answers for it. This process already computes RebootAllowed every tick, already
// survives the per-connection guest agent's crash-loop as its own long-running unit, and already
// runs precisely when the host agent is gone. Giving it a socket costs one more listener; the
// alternative was a second in-guest daemon with its own lifecycle to go wrong, which is the
// problem rather than a solution to it.
//
// RAW TCP, NOT HTTP, and that is load-bearing rather than taste: the guest binary is built
// `-tags guest` so it never links net/http and the TLS stack behind it (the same trim that makes
// this package duplicate notify's level strings instead of importing them). `net` alone is a much
// smaller thing to pull in, and one line of key=value text needs nothing more.
//
// It READS NOTHING from the connection. There is no request to parse, so there is no parser to
// get wrong: accept, write one line, close. The link it binds is private and point-to-point
// (guest eth3 <-> host), but a surface that cannot be spoken to is cheaper than one that can.

// The private host<->guest link, in full. Fixed, not configured: both ends are ours, it is
// point-to-point, and there is nobody to collide with — so a knob would only add a way for the
// two ends to disagree (AGENTS §3). The port is adjacent to DRBD's 7789 on the same substrate.
//
// These same numbers are written in three other languages — the guest image bakes GuestIP on eth3
// and BRIARD_GATE_ADDR into the deadman unit, install.sh puts HostIP on the tap, and the cloud's
// mesh composer hands out both for the witness forwarder (cloud/server/pair.go). There is no
// shared constant across a Go/Nix/shell boundary, so gate_pairing_test.go reads the other files
// and fails on drift; a comment cannot catch a rename, and the failure mode is a host rung that
// silently cannot reach its guest.
const (
	GuestIP  = "10.9.9.2" // the guest's eth3 — where the gate answers
	HostIP   = "10.9.9.1" // the host's end of the tap — where the witness forwarder listens
	GatePort = 7790
)

// GateAddr is the address the host dials to read this guest's reboot gate.
func GateAddr() string { return net.JoinHostPort(GuestIP, strconv.Itoa(GatePort)) }

// GateProto is the first token of the reply. A version tag rather than bare values, so the host
// refuses to interpret something it does not recognise instead of parsing garbage into a verdict
// that power-cycles a VM.
const GateProto = "briard-gate/1"

// Gate holds the latest verdict and serves it. The zero value is usable but answers "unknown"
// until the first publish -- correct rather than merely safe, since the host reads "unknown" the
// same as unreachable and proceeds, which is what it would do with no gate at all.
//
// It deliberately does NOT report the guest's uptime, though an early version did, so the host
// could avoid power-cycling a guest that had just rebooted itself. That guard proved unnecessary:
// `-no-reboot` means every guest restart ends the VM's unit, so the HOST is the one that starts it
// and already knows when. A second, weaker source of the same fact -- one that exists only while
// the deadman is still answering -- would only be a way for the two to disagree.
type Gate struct {
	Now  func() time.Time     // injectable clock (tests)
	Logf func(string, ...any) // operational log

	mu      sync.Mutex
	known   bool
	allowed bool
	stamp   time.Time
}

func (g *Gate) now() time.Time {
	if g.Now != nil {
		return g.Now()
	}
	return time.Now()
}

func (g *Gate) logf(format string, a ...any) {
	if g.Logf != nil {
		g.Logf(format, a...)
	}
}

// Publish records the verdict from one evaluation tick. A failed fabric read publishes "unknown"
// rather than a stale answer: the host must not be handed a verdict from ten minutes ago as
// though it described now.
func (g *Gate) Publish(allowed bool, err error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if err != nil {
		g.known = false
		return
	}
	g.known, g.allowed, g.stamp = true, allowed, g.now()
}

// Render is the whole reply: one line, key=value, newline-terminated.
//
//	briard-gate/1 allowed=1 age=7
//
// Age is seconds since the verdict was computed, and it is what makes a WEDGED deadman
// detectable: if the evaluation loop stops while the accept loop lives, the answer keeps arriving
// and only its age gives it away. allowed=? means no verdict yet (or the last fabric read
// failed), which the host treats the same as unreachable.
func (g *Gate) Render() string {
	g.mu.Lock()
	known, allowed, stamp := g.known, g.allowed, g.stamp
	g.mu.Unlock()

	if !known {
		return GateProto + " allowed=? age=?\n"
	}
	age := max(int64(g.now().Sub(stamp)/time.Second), 0)
	return fmt.Sprintf("%s allowed=%s age=%d\n", GateProto, boolDigit(allowed), age)
}

func boolDigit(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

// Serve accepts on addr until ctx is done, answering every connection with Render().
//
// A failure to bind is returned, not logged and swallowed: the address is a baked constant on a
// link we own both ends of, so the only ways to fail are a misconfigured image or a port already
// taken — both of which mean the host rung is running blind, and blind is the state this exists
// to end. The caller decides whether that is fatal.
func (g *Gate) Serve(ctx context.Context, addr string) error {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("deadman: listen %s: %w", addr, err)
	}
	defer ln.Close()
	go func() { <-ctx.Done(); ln.Close() }() // unblock Accept on shutdown

	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			// A transient accept error must not kill the listener: this answers the reflex that
			// power-cycles the VM, so going quiet is the expensive failure. Keep accepting.
			g.logf("deadman: gate accept: %v", err)
			continue
		}
		// Serialised on purpose. The single caller polls at minutes' cadence, the reply is one
		// line, and a goroutine per connection would be a way for a chatty peer to grow this
		// process without bound for no gain.
		//
		// time.Now, NOT g.now: the injectable clock is LOGICAL time, used to age a verdict, and
		// feeding it to an I/O deadline couples the socket to whatever a caller set it to. A test
		// clock in 1970 makes every write deadline already-expired, so the gate accepts
		// connections and answers none of them — silence being the one failure mode that reads
		// exactly like a healthy-but-quiet node.
		_ = c.SetWriteDeadline(time.Now().Add(5 * time.Second))
		_, _ = c.Write([]byte(g.Render()))
		_ = c.Close()
	}
}
