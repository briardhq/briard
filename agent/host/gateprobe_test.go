package host

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"briard.io/agent/guestagent/deadman"
)

// The parser is what stands between a line of text on a socket and a decision to power-cycle a
// VM, so every way it can fail must land on "learned nothing" rather than on a verdict.
func TestParseGate(t *testing.T) {
	proto := deadman.GateProto
	cases := []struct {
		name string
		line string
		ok   bool
		want gateVerdict
	}{
		{
			name: "a permissive verdict",
			line: proto + " allowed=1 age=3",
			ok:   true,
			want: gateVerdict{reached: true, fresh: true, allowed: true},
		},
		{
			name: "a denial",
			line: proto + " allowed=0 age=0",
			ok:   true,
			want: gateVerdict{reached: true, fresh: true, allowed: false},
		},
		{
			// The guest is up and answering but has not evaluated yet (or its last fabric read
			// failed). Reached, but with no opinion -- which must not read as a denial.
			name: "no verdict published",
			line: proto + " allowed=? age=?",
			ok:   true,
			want: gateVerdict{reached: true},
		},
		{
			// A deadman whose evaluation loop wedged while its accept loop lives. The answer keeps
			// arriving and describes a cluster from another time; only the age gives it away.
			name: "a verdict too old to mean anything",
			line: proto + " allowed=0 age=99999",
			ok:   true,
			want: gateVerdict{reached: true},
		},
		{
			// Unknown keys are ignored so the guest can grow the reply without a flag day.
			name: "unknown keys are tolerated",
			line: proto + " allowed=1 age=1 peers=2 something=else",
			ok:   true,
			want: gateVerdict{reached: true, fresh: true, allowed: true},
		},
		{name: "empty line", line: "", ok: false},
		{name: "not our protocol", line: "SSH-2.0-OpenSSH_9.6", ok: false},
		{name: "a future protocol version we do not speak", line: "briard-gate/2 allowed=1 age=0", ok: false},
		{name: "the tag alone", line: proto, ok: true, want: gateVerdict{reached: true}},
		{
			name: "junk values leave their fields unset",
			line: proto + " allowed=maybe age=soon",
			ok:   true,
			want: gateVerdict{reached: true},
		},
	}
	for _, c := range cases {
		got, ok := parseGate(c.line)
		if ok != c.ok {
			t.Errorf("%s: parseGate(%q) ok = %v, want %v", c.name, c.line, ok, c.ok)
			continue
		}
		if ok && got != c.want {
			t.Errorf("%s: parseGate(%q) = %+v, want %+v", c.name, c.line, got, c.want)
		}
	}
}

// A refused connection is the everyday case on a dead guest, and it must be cheap and silent-ish
// rather than an error path anyone has to handle.
func TestReadGateOnADeadAddressLearnsNothing(t *testing.T) {
	// Bind and immediately release, so the address is one nothing listens on.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()

	if got := readGate(context.Background(), addr, func(string, ...any) {}); got != (gateVerdict{}) {
		t.Errorf("readGate on a dead address = %+v, want the zero verdict", got)
	}
}

// End to end against the real server, so the two sides' wire format is proven together rather
// than each against a fixture of the other. A change to Render that the parser cannot read would
// otherwise pass both packages' tests and disable the guard in production.
func TestReadGateAgainstTheRealDeadmanGate(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	g := &deadman.Gate{
		Now: func() time.Time { return now },
	}
	g.Publish(false, nil) // a denial: the verdict that must survive the round trip intact

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = g.Serve(ctx, addr) }()

	var v gateVerdict
	for range 100 {
		if v = readGate(ctx, addr, func(string, ...any) {}); v.reached {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !v.reached || !v.fresh || v.allowed {
		t.Fatalf("round trip = %+v, want reached+fresh with allowed=false", v)
	}

	// And the decision it feeds: a denial holds.
	r := &guestRecovery{}
	if got := r.next(v, never, false); got != stepHold {
		t.Errorf("a real denied gate produced %v, want stepHold", got)
	}
}

// The staleness bound has to sit well above the deadman's evaluation cadence, or a merely slow
// tick reads as a wedged evaluator and the guard silently switches itself off.
func TestGateStalenessBoundIsAboveTheDeadmanTick(t *testing.T) {
	if guestGateStale < time.Minute {
		t.Errorf("guestGateStale = %v: the deadman re-evaluates every 15s, so this would call a "+
			"healthy gate stale and discard verdicts that are fine", guestGateStale)
	}
}

// The proto tag is a prefix check, and a sloppy one (strings.HasPrefix on the bare name) would
// accept "briard-gate/11" as version 1. Guard the exactness, since accepting a future version's
// reply means interpreting fields that may have changed meaning.
func TestParseGateRejectsAVersionPrefixCollision(t *testing.T) {
	line := deadman.GateProto + "1 allowed=1 age=0"
	if _, ok := parseGate(line); ok {
		t.Errorf("parseGate accepted %q; %q is a different protocol version", line, strings.Fields(line)[0])
	}
}
