package host

import (
	"bufio"
	"context"
	"net"
	"strconv"
	"strings"
	"time"

	"briard.io/agent/guestagent/deadman"
)

// READING THE GUEST'S REBOOT GATE, from outside the VM.
//
// The guest deadman publishes RebootAllowed on every tick and serves it over the private
// host<->guest link as one line of key=value text (deadman/gate.go). This is the client: dial,
// read a line, parse, hang up. It is the only question this host asks the guest over anything
// other than the virtio-serial control channel, and it exists precisely because that channel
// being dead is what makes anyone ask.
//
// EVERY failure here -- no route, refused, timeout, garbage, an unknown protocol tag, a verdict
// too old to mean anything -- lands on the same answer: not reached, or reached but not fresh.
// The ladder reads both as "no opinion" and proceeds with the reboot it was already going to do.
// That is deliberate and it is what keeps this file from becoming load-bearing: a bug in here can
// cost the guard, never cause a power cycle that would not otherwise have happened.

// gateVerdict is what the host managed to learn about the guest's own view of rebooting itself.
//
// It carries no uptime, though an early version did, so the ladder could avoid power-cycling a
// guest that had just rebooted itself. That is not a question worth asking the guest: `-no-reboot`
// means every guest restart ends the VM's unit, so the host is the one that starts it and knows
// exactly when. The ladder reads the unit's state directly instead, which is also true while the
// deadman is dead.
type gateVerdict struct {
	reached bool // a well-formed reply arrived
	fresh   bool // it carried a verdict, recent enough to mean anything
	allowed bool // that verdict (meaningless unless fresh)
}

// gateTimeout bounds the whole probe. The guest answers in microseconds when it answers at all --
// it is one write on a point-to-point link -- so this is not a budget, it is the point at which
// waiting longer stops telling us anything new. The ladder has already waited ten minutes.
const gateTimeout = 5 * time.Second

// readGate probes the guest's gate. It never returns an error: the zero verdict already says
// "learned nothing", and every caller treats a failure and an absence identically, so an error
// return would only be a second way to spell the same thing.
func readGate(ctx context.Context, addr string, logf func(string, ...any)) gateVerdict {
	ctx, cancel := context.WithTimeout(ctx, gateTimeout)
	defer cancel()

	var d net.Dialer
	c, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		logf("guest-recovery: gate %s unreachable (%v) -- proceeding without it", addr, err)
		return gateVerdict{}
	}
	defer c.Close()
	if dl, ok := ctx.Deadline(); ok {
		_ = c.SetReadDeadline(dl)
	}

	sc := bufio.NewScanner(c)
	// Bound the read: a peer that never sends a newline must not grow this buffer, and the reply
	// is one short line by construction.
	sc.Buffer(make([]byte, 0, 512), 4096)
	if !sc.Scan() {
		logf("guest-recovery: gate %s gave no answer (%v) -- proceeding without it", addr, sc.Err())
		return gateVerdict{}
	}
	v, ok := parseGate(sc.Text())
	if !ok {
		logf("guest-recovery: gate %s answered %q, which is not a gate reply -- proceeding without it", addr, sc.Text())
		return gateVerdict{}
	}
	return v
}

// parseGate reads one reply line. Unknown keys are ignored so the guest can grow the reply
// without a flag day; a missing or unparsable key simply leaves its field unset, which the
// verdict already models as "not known".
func parseGate(line string) (gateVerdict, bool) {
	fields := strings.Fields(strings.TrimSpace(line))
	if len(fields) == 0 || fields[0] != deadman.GateProto {
		return gateVerdict{}, false // not ours, or a version we do not speak: refuse to interpret
	}
	v := gateVerdict{reached: true}
	var age time.Duration
	haveAge := false
	for _, f := range fields[1:] {
		k, val, ok := strings.Cut(f, "=")
		if !ok {
			continue
		}
		switch k {
		case "allowed":
			switch val {
			case "1":
				v.fresh, v.allowed = true, true
			case "0":
				v.fresh, v.allowed = true, false
			} // "?" -> no verdict; leave fresh false
		case "age":
			if n, err := strconv.Atoi(val); err == nil {
				age, haveAge = time.Duration(n)*time.Second, true
			}
		}
	}
	// A verdict with no age, or one older than the staleness bound, is a verdict from a deadman
	// that has stopped evaluating -- its accept loop still answers, but what it answers describes
	// a cluster from some other time. Downgrade it to "no verdict" rather than acting on it.
	if v.fresh && (!haveAge || age > guestGateStale) {
		v.fresh = false
	}
	return v, true
}
