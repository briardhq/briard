package deadman

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"sync"
	"time"
)

// The gate, served outward. The host holds the same reflex from outside the VM
// (agent/host/guestrecover.go), and it fires on the death of the control channel, so every question
// that would gate its decision is one it can no longer ask: it cannot read DRBD (in the guest),
// ask the guest agent (the thing that stopped answering), or, under the default macvtap
// substrate, reach the VIP. So the deadman answers for it: this process already computes
// RebootAllowed every tick, already survives the per-connection guest agent's crash-loop as its
// own long-running unit, and already runs precisely when the host agent is gone. Giving it a
// socket costs one more listener; a second in-guest daemon would be one more lifecycle to go wrong.
//
// Raw TCP, not HTTP: one line of key=value text needs nothing more. It reads nothing from the
// connection, so there is no parser to get wrong: accept, write one line, close. It answers on
// this node's own address, which rides the LAN's L2, so the listener is not hidden by addressing
// -- reading nothing is what makes that safe.

// GatePort is where the gate answers. Adjacent to DRBD's 7789, and the only fixed number on this
// path: the address is the node's own, agent-assigned and flock-scoped, so it cannot be a
// constant here and must not be one anywhere else either -- the node IP is assigned once and
// travels over the channel. Paired with guest-image/disk-image.nix's BRIARD_GATE_ADDR (":7790"),
// which the deadman unit listens on; different languages, so no shared import.
const GatePort = 7790

// GateAddr is the address the host dials to read a guest's reboot gate: that node's node IP, the
// one address anything uses to reach it. "" in -- a node with no address yet -- gives "", which
// the caller reads the same as unreachable and therefore as allowed, exactly as it reads a node
// with no gate at all.
func GateAddr(nodeIP string) string {
	if nodeIP == "" {
		return ""
	}
	return net.JoinHostPort(nodeIP, strconv.Itoa(GatePort))
}

// GateProto is the first token of the reply. A version tag rather than bare values, so the host
// refuses to interpret something it does not recognise instead of parsing garbage into a verdict
// that power-cycles a VM.
const GateProto = "briard-gate/1"

// Gate holds the latest verdict and serves it. The zero value is usable but answers "unknown"
// until the first publish -- correct rather than merely safe, since the host reads "unknown" the
// same as unreachable and proceeds, which is what it would do with no gate at all.
//
// It deliberately does not report the guest's uptime, though "did the guest just reboot itself?"
// looks like a question for the guest: `-no-reboot` means every guest restart ends the VM's unit,
// so the host is the one that starts it and already knows when. A second, weaker source of the
// same fact -- one that exists only while the deadman is still answering -- would only be a way
// for the two to disagree.
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
// Age is seconds since the verdict was computed, and it is what makes a wedged deadman
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
// A failure to bind is returned, not logged and swallowed: addr is a bare port, so the only ways
// to fail are a port already taken or a misconfigured image — both mean the host rung is running
// blind, and blind is the state this exists to end. The caller decides whether that is fatal.
// It binds a port rather than an address because the address is the node's own and is assigned
// at bring-up, so the image cannot name it. Answering on every interface is tolerable only
// because this reads nothing from a connection, so a wider bind adds no attack surface beyond
// letting a reader learn whether a reboot is currently allowed.
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
		// time.Now, not g.now: the injectable clock is logical time, used to age a verdict, and
		// feeding it to an I/O deadline couples the socket to whatever a caller set it to. A test
		// clock in 1970 makes every write deadline already-expired, so the gate accepts
		// connections and answers none of them — silence being the one failure mode that reads
		// exactly like a healthy-but-quiet node.
		_ = c.SetWriteDeadline(time.Now().Add(5 * time.Second))
		_, _ = c.Write([]byte(g.Render()))
		_ = c.Close()
	}
}
