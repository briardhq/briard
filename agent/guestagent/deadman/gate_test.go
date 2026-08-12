package deadman

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

func testGate(now *time.Time) *Gate {
	return &Gate{
		Now: func() time.Time { return *now },
	}
}

// Before the first tick there is no verdict, and the gate must say so rather than guess. The host
// reads allowed=? the same as unreachable, so a wrong guess here is either a VM that gets
// power-cycled on no evidence or one that never does.
func TestGateRendersUnknownBeforeFirstPublish(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	got := testGate(&now).Render()
	if !strings.HasPrefix(got, GateProto+" ") {
		t.Errorf("reply %q does not start with the proto tag %q", got, GateProto)
	}
	if !strings.Contains(got, "allowed=?") || !strings.Contains(got, "age=?") {
		t.Errorf("pre-publish reply = %q, want allowed=? age=?", got)
	}
}

// A published verdict carries its age, and the age is what makes a WEDGED deadman detectable: if
// the evaluation loop stops while the accept loop lives, the answer keeps arriving and only its
// age gives it away. An implementation that stamped at Render time instead of Publish time would
// report age=0 forever and pass every other assertion here.
func TestGateReportsVerdictAndItsAge(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	g := testGate(&now)

	g.Publish(true, nil)
	now = now.Add(7 * time.Second)
	if got := g.Render(); !strings.Contains(got, "allowed=1") || !strings.Contains(got, "age=7") {
		t.Errorf("reply = %q, want allowed=1 age=7", got)
	}

	g.Publish(false, nil)
	if got := g.Render(); !strings.Contains(got, "allowed=0") || !strings.Contains(got, "age=0") {
		t.Errorf("reply after a fresh negative verdict = %q, want allowed=0 age=0", got)
	}
}

// A failed fabric read must ERASE the verdict, not leave the last one standing. Serving a stale
// "allowed=1" as though it described now is how the host ends up power-cycling a node whose
// cluster changed underneath it ten minutes ago.
func TestGateForgetsTheVerdictOnReadFailure(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	g := testGate(&now)
	g.Publish(true, nil)
	g.Publish(false, fmt.Errorf("drbdsetup exploded"))
	if got := g.Render(); !strings.Contains(got, "allowed=?") {
		t.Errorf("reply after a failed read = %q, want allowed=? (never a stale verdict)", got)
	}
}

// End to end over a real socket: one line, then the connection closes. Reading nothing from the
// client is the point — there is no request to parse, so a client that says nothing still gets
// an answer.
func TestGateServesOneLineAndCloses(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	g := testGate(&now)
	g.Publish(true, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close() // hand the port straight back to Serve

	errc := make(chan error, 1)
	go func() { errc <- g.Serve(ctx, addr) }()

	// Serve binds asynchronously; retry briefly rather than sleeping a fixed time.
	var c net.Conn
	for range 100 {
		if c, err = net.Dial("tcp", addr); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if c == nil {
		t.Fatalf("gate never came up on %s: %v", addr, err)
	}
	defer c.Close()

	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	sc := bufio.NewScanner(c)
	if !sc.Scan() {
		t.Fatalf("no reply line: %v", sc.Err())
	}
	line := sc.Text()
	if !strings.HasPrefix(line, GateProto+" ") || !strings.Contains(line, "allowed=1") {
		t.Errorf("served %q, want the proto tag and allowed=1", line)
	}
	// Second Scan returns false: the server closed after one line.
	if sc.Scan() {
		t.Errorf("gate sent a second line %q; it must answer once and close", sc.Text())
	}

	cancel()
	if err := <-errc; err == nil {
		t.Error("Serve returned nil on a cancelled context, want the ctx error")
	}
}

// A port that cannot be bound is returned, not swallowed. It means the host rung is running
// blind, and blind is the state the gate exists to end — the caller has to be able to say so.
func TestGateServeReportsABindFailure(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	now := time.Unix(1_000_000, 0)
	if err := testGate(&now).Serve(context.Background(), ln.Addr().String()); err == nil {
		t.Error("Serve on an occupied port returned nil, want the bind error")
	}
}
