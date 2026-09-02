package guestagent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

// EchoDispatch: "echo" doubles its string arg; "boom" errors; else unknown-verb.
func echoDispatch(_ context.Context, verb string, payload json.RawMessage) (any, error) {
	switch verb {
	case "echo":
		var s string
		if err := json.Unmarshal(payload, &s); err != nil {
			return nil, err
		}
		return s + s, nil
	case "boom":
		return nil, errors.New("kaboom")
	default:
		return nil, errors.New("unknown verb " + verb)
	}
}

// wirePair wires a host conn to a serve(echoDispatch) over an in-memory pipe.
func wirePair(t *testing.T) *conn {
	t.Helper()
	cc, sc := net.Pipe()
	go serve(context.Background(), sc, echoDispatch)
	c := newConn(cc)
	t.Cleanup(func() { c.close() })
	return c
}

func TestWireRoundTripAndSequentialIDs(t *testing.T) {
	c := wirePair(t)
	for _, w := range []string{"ab", "c", "def"} {
		var out string
		if err := c.call(context.Background(), "echo", w, &out); err != nil {
			t.Fatal(err)
		}
		if out != w+w {
			t.Errorf("echo(%q) = %q, want %q", w, out, w+w)
		}
	}
}

func TestWireHandlerErrorPropagates(t *testing.T) {
	c := wirePair(t)
	err := c.call(context.Background(), "boom", nil, nil)
	if err == nil || err.Error() != "kaboom" {
		t.Errorf("err = %v, want kaboom", err)
	}
	// A verb error is NOT a dead channel — the round-trip completed, so the host must
	// keep the connection (not reconnect).
	if errors.Is(err, ErrChannelDown) {
		t.Error("a verb error must not be ErrChannelDown")
	}
	// The stream survives a handler error — the next call still works.
	var out string
	if err := c.call(context.Background(), "echo", "z", &out); err != nil || out != "zz" {
		t.Errorf("post-error call: out=%q err=%v", out, err)
	}
}

// A dead transport surfaces as ErrChannelDown so the host re-dials, unlike a verb
// error which leaves the channel usable.
func TestWireChannelDownOnDeadConn(t *testing.T) {
	cc, _ := net.Pipe()
	c := newConn(cc)
	cc.Close()
	if err := c.call(context.Background(), "echo", "a", nil); !errors.Is(err, ErrChannelDown) {
		t.Errorf("call on a closed conn = %v, want ErrChannelDown", err)
	}
}

func TestWireUnknownVerb(t *testing.T) {
	c := wirePair(t)
	err := c.call(context.Background(), "nope", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "unknown verb") {
		t.Errorf("err = %v, want unknown verb", err)
	}
}

func TestWireContextCancelled(t *testing.T) {
	c := wirePair(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := c.call(ctx, "echo", "a", nil); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

func TestServeEndsOnClose(t *testing.T) {
	cc, sc := net.Pipe()
	done := make(chan error, 1)
	go func() { done <- serve(context.Background(), sc, echoDispatch) }()
	cc.Close()
	if err := <-done; err != nil {
		t.Errorf("serve after close = %v, want nil", err)
	}
}

// A guest that reads the request but never replies must not hang the host: the
// call returns when its context deadline fires (turning a wedged verb into a
// timeout the caller can act on), not block forever.
func TestCallHonorsContextOnStuckGuest(t *testing.T) {
	cconn, sconn := net.Pipe()
	go io.Copy(io.Discard, sconn) // drain requests, never respond
	g := NewClient(cconn)
	defer g.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- g.Up(ctx, "r0") }()

	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("err = %v, want DeadlineExceeded", err)
		}
		// The deadline closed the channel mid-frame, so it's also ErrChannelDown: a
		// bounded op sees its timeout AND the observe loop reconnects.
		if !errors.Is(err, ErrChannelDown) {
			t.Errorf("a mid-call deadline must also be ErrChannelDown, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("call hung despite a ctx deadline")
	}
}

// A REPLY ALREADY BEING WRITTEN SURVIVES CANCELLATION, which is what makes EOF mean "the agent
// died" rather than "the agent answered and we threw the answer away".
//
// The verb that forced this is `os.poweroff`: the shutdown it starts is what SIGTERMs the guest
// agent, so its reply is ALWAYS the one in flight when the context is cancelled. Losing it looked
// exactly like a crashed agent, and the host escalated to the ACPI power button on a guest that
// had shut itself down as asked ([B.127]). Here the handler blocks until the context is cancelled
// and only then returns, so the close and the reply are in the order that used to lose.
func TestServeFinishesInFlightReplyOnCancel(t *testing.T) {
	cconn, sconn := net.Pipe()
	defer cconn.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	handling := make(chan struct{})
	d := func(ctx context.Context, verb string, payload json.RawMessage) (any, error) {
		close(handling)
		<-ctx.Done() // the cancellation lands while this reply is owed
		return "pong", nil
	}
	go serve(ctx, sconn, d)

	go func() {
		<-handling
		cancel()
	}()

	if err := writeFrame(cconn, request{ID: 1, Verb: "ping"}); err != nil {
		t.Fatalf("writeFrame: %v", err)
	}
	cconn.SetReadDeadline(time.Now().Add(2 * time.Second))
	var resp response
	if err := readFrame(cconn, &resp); err != nil {
		t.Fatalf("the reply owed at cancellation never arrived: %v", err)
	}
	if resp.ID != 1 || string(resp.Payload) != `"pong"` {
		t.Errorf("resp = %+v, want ID 1 payload \"pong\"", resp)
	}
}
