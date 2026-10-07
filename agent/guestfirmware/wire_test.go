package guestfirmware

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
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

// wirePair wires a host conn to a ServeFrames(echoDispatch) over an in-memory pipe.
func wirePair(t *testing.T) *Conn {
	t.Helper()
	cc, sc := net.Pipe()
	go ServeFrames(context.Background(), sc, echoDispatch)
	c := NewConn(cc)
	t.Cleanup(func() { c.Close() })
	return c
}

func TestWireRoundTripAndSequentialIDs(t *testing.T) {
	c := wirePair(t)
	for _, w := range []string{"ab", "c", "def"} {
		var out string
		if err := c.Call(context.Background(), "echo", w, &out); err != nil {
			t.Fatal(err)
		}
		if out != w+w {
			t.Errorf("echo(%q) = %q, want %q", w, out, w+w)
		}
	}
}

func TestWireHandlerErrorPropagates(t *testing.T) {
	c := wirePair(t)
	err := c.Call(context.Background(), "boom", nil, nil)
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
	if err := c.Call(context.Background(), "echo", "z", &out); err != nil || out != "zz" {
		t.Errorf("post-error call: out=%q err=%v", out, err)
	}
}

// A dead transport surfaces as ErrChannelDown so the host re-dials, unlike a verb
// error which leaves the channel usable.
func TestWireChannelDownOnDeadConn(t *testing.T) {
	cc, _ := net.Pipe()
	c := NewConn(cc)
	cc.Close()
	if err := c.Call(context.Background(), "echo", "a", nil); !errors.Is(err, ErrChannelDown) {
		t.Errorf("call on a closed conn = %v, want ErrChannelDown", err)
	}
}

func TestWireUnknownVerb(t *testing.T) {
	c := wirePair(t)
	err := c.Call(context.Background(), "nope", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "unknown verb") {
		t.Errorf("err = %v, want unknown verb", err)
	}
}

func TestWireContextCancelled(t *testing.T) {
	c := wirePair(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := c.Call(ctx, "echo", "a", nil); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

func TestServeEndsOnClose(t *testing.T) {
	cc, sc := net.Pipe()
	done := make(chan error, 1)
	go func() { done <- ServeFrames(context.Background(), sc, echoDispatch) }()
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
	g := NewConn(cconn)
	defer g.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- g.Call(ctx, VerbHello, nil, nil) }()

	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("err = %v, want DeadlineExceeded", err)
		}
		// Nothing answered since the call was sent -- the liveness rule -- so the deadline also
		// closed the channel and it is ErrChannelDown too: a
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
// had shut itself down as asked. Here the handler blocks until the context is cancelled
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
	go ServeFrames(ctx, sconn, d)

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

// On a reconnect the still-open virtio-serial stream can carry a stale in-flight reply from the
// *dropped* session ahead of the handshake reply (QEMU keeps the guest port open across a host
// re-dial). A reply whose id nothing waits on is dropped, never a failure -- otherwise a
// reconnect never recovers (the observed agent-bringup freeze/thaw failure). The handshake is
// where it shows, being the first call after a (re)connect.
func TestCallDropsAStaleFrame(t *testing.T) {
	cc, sc := net.Pipe()
	c := NewConn(cc)
	t.Cleanup(func() { c.Close() })
	go func() {
		var req request
		if err := readFrame(sc, &req); err != nil { // read the hello request
			return
		}
		// A leftover reply from the previous session (an unrelated id), THEN the real
		// handshake reply. The caller must skip the first and match the second.
		hello, _ := json.Marshal(Hello{Capabilities: []string{VerbHello}, BootID: "the-real-reply"})
		_ = writeFrame(sc, response{ID: req.ID + 42, Payload: json.RawMessage(`"stale reply from a dropped session"`)})
		_ = writeFrame(sc, response{ID: req.ID, Payload: hello})
	}()
	var h Hello
	if err := c.Call(context.Background(), VerbHello, nil, &h); err != nil {
		t.Fatalf("a call must drop a stale frame nothing waits on, got: %v", err)
	}
	if h.BootID != "the-real-reply" {
		t.Errorf("boot_id = %q, want the real reply's: the call matched a frame, but not that one", h.BootID)
	}
}

// slowDispatch: "slow" blocks until released; "echo" answers at once. The guest end of a channel
// that serves reads beside a long act.
func slowDispatch(release <-chan struct{}) DispatchFunc {
	return func(ctx context.Context, verb string, payload json.RawMessage) (any, error) {
		if verb == "slow" {
			select {
			case <-release:
			case <-ctx.Done():
			}
			return "done", nil
		}
		return echoDispatch(ctx, verb, payload)
	}
}

// serveConcurrently is a guest that dispatches every request in its own goroutine -- the shape
// the guest's serve loop takes for the read lane -- so a slow verb does not hold an echo.
func serveConcurrently(ctx context.Context, rw io.ReadWriteCloser, d DispatchFunc) {
	var wmu sync.Mutex
	for {
		var req request
		if err := readFrame(rw, &req); err != nil {
			return
		}
		go func(req request) {
			resp := response{ID: req.ID}
			if result, err := d(ctx, req.Verb, req.Payload); err != nil {
				resp.Error = err.Error()
			} else if b, err := json.Marshal(result); err == nil {
				resp.Payload = b
			}
			wmu.Lock()
			defer wmu.Unlock()
			_ = writeFrame(rw, resp)
		}(req)
	}
}

// A SLOW VERB HOLDS NOTHING BUT ITSELF. While one call waits on a long act, another call on the
// same channel is answered, and the act's own deadline abandons the act alone: the channel stays
// up, the next call still works, and the act's late reply is dropped when it comes.
func TestCallsMultiplexAndADeadlineAbandonsOnlyItsCall(t *testing.T) {
	cc, sc := net.Pipe()
	release := make(chan struct{})
	go serveConcurrently(context.Background(), sc, slowDispatch(release))
	c := NewConn(cc)
	t.Cleanup(func() { c.Close() })

	slow := make(chan error, 1)
	sctx, scancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer scancel()
	go func() { slow <- c.Call(sctx, "slow", nil, nil) }()

	// Beside it, an echo answers at once.
	var out string
	ectx, ecancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer ecancel()
	if err := c.Call(ectx, "echo", "x", &out); err != nil || out != "xx" {
		t.Fatalf("echo beside a slow verb = (%q, %v), want it answered", out, err)
	}

	// The slow call's deadline: its own error, NOT a dead channel -- the echo proved the guest alive.
	err := <-slow
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("slow call = %v, want its own DeadlineExceeded", err)
	}
	if errors.Is(err, ErrChannelDown) {
		t.Fatalf("a deadline on one call beside an answering guest must not be ErrChannelDown: %v", err)
	}
	// The channel is still up: the late reply arrives and is dropped, the next call works.
	close(release)
	if err := c.Call(ectx, "echo", "y", &out); err != nil || out != "yy" {
		t.Fatalf("echo after the abandoned call = (%q, %v), want the channel still up", out, err)
	}
}

// THE LIVENESS RULE, the other way round from TestCallHonorsContextOnStuckGuest: a guest that
// answered something -- anything -- since the call was sent is alive, so the deadline is the
// call's own; a guest that answered nothing is not, so the deadline closes the channel. Here
// the guest serves one verb at a time (ServeFrames, the serial shape): the echo queued behind
// the slow verb is never answered, so from the echo's point of view nothing arrived since it was
// sent, and it reports the channel down -- which is what re-dials, and what the recovery ladder
// hangs off.
func TestADeadlineWithNoAnswerAtAllIsChannelDown(t *testing.T) {
	cc, sc := net.Pipe()
	release := make(chan struct{})
	defer close(release)
	go ServeFrames(context.Background(), sc, slowDispatch(release))
	c := NewConn(cc)
	t.Cleanup(func() { c.Close() })

	sctx, scancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer scancel()
	go func() { _ = c.Call(sctx, "slow", nil, nil) }()
	time.Sleep(20 * time.Millisecond) // let the slow request reach the serial server first

	ectx, ecancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer ecancel()
	err := c.Call(ectx, "echo", "x", nil)
	if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, ErrChannelDown) {
		t.Fatalf("echo behind a serial guest's slow verb = %v, want DeadlineExceeded AND ErrChannelDown", err)
	}
}
