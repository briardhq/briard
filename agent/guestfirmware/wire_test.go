package guestfirmware

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

// allActs is the serial shape: every verb takes the act mutex, one at a time in arrival order.
func allActs(string) bool { return true }

// wirePair wires a host conn to a ServeFrames(echoDispatch) over an in-memory pipe.
func wirePair(t *testing.T) *Conn {
	t.Helper()
	cc, sc := net.Pipe()
	go ServeFrames(context.Background(), sc, echoDispatch, allActs)
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
	// A verb error is not a dead channel — the round-trip completed, so the host must
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
	go func() { done <- ServeFrames(context.Background(), sc, echoDispatch, allActs) }()
	cc.Close()
	if err := <-done; err != nil {
		t.Errorf("serve after close = %v, want nil", err)
	}
}

// A guest that reads the request but never replies must not hang the host: the call returns at
// its context deadline, a timeout the caller can act on. A connection that answered and then
// stopped is merely late until it has left missLimit calls unanswered, then dead (the liveness
// rule closes it, ErrChannelDown too, so the observe loop re-dials). One that never answered is
// only unanswered however often it is asked -- the handshake's case, where the hello may have
// been lost in the guest agent's own restart and the right move is to resend on this same
// connection, not to close.
func TestCallHonorsContextOnStuckGuest(t *testing.T) {
	t.Run("never answered: the caller's own deadline, the connection kept", func(t *testing.T) {
		cconn, sconn := net.Pipe()
		go io.Copy(io.Discard, sconn) // drain requests, never respond
		g := NewConn(cconn)
		defer g.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		err := g.Call(ctx, VerbHello, nil, nil)
		if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrChannelDown) {
			t.Fatalf("err = %v, want a plain DeadlineExceeded: nothing has proven this connection alive", err)
		}
		// Still open: a second hello goes out on it (and times out the same way).
		ctx2, cancel2 := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel2()
		if err := g.Call(ctx2, VerbHello, nil, nil); !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrChannelDown) {
			t.Fatalf("second hello on the kept connection = %v, want a plain DeadlineExceeded", err)
		}
	})
	t.Run("answered once, then silent: late, late, then dead", func(t *testing.T) {
		cconn, sconn := net.Pipe()
		answered := false
		stuck := func(ctx context.Context, verb string, payload json.RawMessage) (any, error) {
			if !answered {
				answered = true
				return "first", nil
			}
			<-ctx.Done() // wedged from now on
			return nil, ctx.Err()
		}
		go ServeFrames(context.Background(), sconn, stuck, allActs)
		g := NewConn(cconn)
		defer g.Close()
		if err := g.Call(context.Background(), "echo", nil, nil); err != nil {
			t.Fatalf("the first call: %v", err)
		}
		for i := 1; i <= missLimit; i++ {
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			err := g.Call(ctx, fmt.Sprintf("verb%d", i), nil, nil)
			cancel()
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("miss %d = %v, want DeadlineExceeded", i, err)
			}
			if i < missLimit {
				if errors.Is(err, ErrChannelDown) {
					t.Fatalf("miss %d of %d = %v: a call short of the rule must leave the channel up", i, missLimit, err)
				}
				continue
			}
			if !errors.Is(err, ErrChannelDown) {
				t.Fatalf("miss %d = %v, want ErrChannelDown too: it answered once and stopped", i, err)
			}
			for j := 1; j <= missLimit; j++ {
				if v := fmt.Sprintf("verb%d", j); !strings.Contains(err.Error(), v) {
					t.Errorf("the closing error %q does not name %s, one of the calls that went unanswered", err, v)
				}
			}
		}
		// What everything else sees afterwards -- the observe loop's "control channel down"
		// line among it -- is the rule's reason too, not the socket the close shut.
		g.mu.Lock()
		done := g.readDone
		g.mu.Unlock()
		<-done
		err := g.Call(context.Background(), "echo", nil, nil)
		if !errors.Is(err, ErrChannelDown) || !strings.Contains(err.Error(), "unanswered") || !strings.Contains(err.Error(), "verb1") {
			t.Errorf("a call after the liveness close = %v, want ErrChannelDown naming the unanswered verbs", err)
		}
	})
}

// A slow handler is not a dead guest. Here a serial guest's one handler is stuck (the shape of an
// in-guest HTTP probe of a slow app), so nothing answers before the call behind it gives up. One
// such call leaves the channel up: the timeout is the call's own, and once the handler returns
// the next call is served on the same connection.
func TestOneUnansweredCallLeavesTheChannelUp(t *testing.T) {
	cc, sc := net.Pipe()
	release := make(chan struct{})
	go ServeFrames(context.Background(), sc, slowDispatch(release, nil), allActs)
	c := NewConn(cc)
	t.Cleanup(func() { c.Close() })
	var out string
	if err := c.Call(context.Background(), "echo", "a", &out); err != nil { // proven alive once
		t.Fatal(err)
	}

	sctx, scancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer scancel()
	slow := make(chan error, 1)
	go func() { slow <- c.Call(sctx, "slow", nil, nil) }()
	time.Sleep(20 * time.Millisecond) // let the slow request reach the serial server first

	ectx, ecancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer ecancel()
	err := c.Call(ectx, "echo", "x", nil)
	if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrChannelDown) {
		t.Fatalf("echo behind a slow handler = %v, want its own DeadlineExceeded and the channel up", err)
	}
	close(release)
	if err := <-slow; err != nil {
		t.Fatalf("the slow call, released = %v, want its answer", err)
	}
	if err := c.Call(context.Background(), "echo", "y", &out); err != nil || out != "yy" {
		t.Fatalf("echo after the slow handler returned = (%q, %v), want the channel still up", out, err)
	}
}

// What the rule counts. A caller's own cancel is not the guest failing to answer, and a call
// whose deadline had passed before it was sent never reached the guest: neither is a miss, so
// any number of them leave the channel up, and the misses that follow start from zero.
func TestCancelsAndUnsentCallsAreNotMisses(t *testing.T) {
	cc, sc := net.Pipe()
	stuck := func(ctx context.Context, verb string, payload json.RawMessage) (any, error) {
		if verb == "echo" {
			return echoDispatch(ctx, verb, payload)
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}
	go ServeFrames(context.Background(), sc, stuck, func(string) bool { return false })
	c := NewConn(cc)
	t.Cleanup(func() { c.Close() })
	var out string
	if err := c.Call(context.Background(), "echo", "a", &out); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2*missLimit; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		go func() { time.Sleep(20 * time.Millisecond); cancel() }()
		if err := c.Call(ctx, "hang", nil, nil); !errors.Is(err, context.Canceled) || errors.Is(err, ErrChannelDown) {
			t.Fatalf("a cancelled call = %v, want a plain Canceled", err)
		}
		expired, cancel2 := context.WithTimeout(context.Background(), -time.Second)
		if err := c.Call(expired, "hang", nil, nil); !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrChannelDown) {
			t.Fatalf("a call already past its deadline = %v, want a plain DeadlineExceeded", err)
		}
		cancel2()
	}
	for i := 1; i < missLimit; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		err := c.Call(ctx, "hang", nil, nil)
		cancel()
		if errors.Is(err, ErrChannelDown) {
			t.Fatalf("miss %d after only cancels and unsent calls = %v: they were counted", i, err)
		}
	}
	if err := c.Call(context.Background(), "echo", "b", &out); err != nil || out != "bb" {
		t.Fatalf("echo after = (%q, %v), want the channel up", out, err)
	}
}

// The window: misses spread wider than missWindow never add up to missLimit: only the ones
// inside the window count, and the call that brings the window to missLimit is the verdict.
func TestMissesCountOnlyInsideTheWindow(t *testing.T) {
	c := NewConn(nil)
	t0 := time.Now()
	step := missWindow + time.Second // each miss alone in its window
	for i := 0; i < 3*missLimit; i++ {
		if down := c.missed("spread", t0.Add(time.Duration(i)*step)); down != nil {
			t.Fatalf("miss %d, %s apart = %v: misses spread wider than %s were counted together", i, step, down, missWindow)
		}
	}
	c = NewConn(nil)
	for i := 0; i < missLimit; i++ {
		down := c.missed(fmt.Sprintf("v%d", i), t0.Add(time.Duration(i)*time.Second))
		if (down != nil) != (i == missLimit-1) {
			t.Fatalf("miss %d of %d within a second each: verdict %v", i+1, missLimit, down)
		}
	}
}

// A reply already being written survives cancellation, which is what makes EOF mean "the agent
// died" rather than "the agent answered and we threw the answer away". `os.poweroff` needs it:
// its shutdown SIGTERMs the guest agent, so its reply is always the one in flight at cancel, and
// lost it would look like a crashed agent -- the host would escalate to the ACPI power button on
// a guest shutting itself down as asked. The handler returns only after the cancel, the order
// that would lose it.
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
	go ServeFrames(ctx, sconn, d, allActs)

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
// dropped session ahead of the handshake reply (QEMU keeps the guest port open across a host
// re-dial). A reply whose id nothing waits on is dropped, never a failure, or a reconnect never
// recovers; the handshake, the first call after a (re)connect, is where it shows.
func TestCallDropsAStaleFrame(t *testing.T) {
	cc, sc := net.Pipe()
	c := NewConn(cc)
	t.Cleanup(func() { c.Close() })
	go func() {
		var req request
		if err := readFrame(sc, &req); err != nil { // read the hello request
			return
		}
		// A leftover reply from the previous session (an unrelated id), then the real
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

// slowDispatch: "slow" signals on started (if given) and blocks until released; "echo" answers
// at once. The guest end of a channel that serves reads beside a long act.
func slowDispatch(release <-chan struct{}, started chan<- struct{}) DispatchFunc {
	return func(ctx context.Context, verb string, payload json.RawMessage) (any, error) {
		if verb == "slow" {
			if started != nil {
				started <- struct{}{}
			}
			select {
			case <-release:
			case <-ctx.Done():
			}
			return "done", nil
		}
		return echoDispatch(ctx, verb, payload)
	}
}

// A slow verb holds nothing but itself. While one call waits on a long act, another call on the
// same channel is answered, and the act's deadline abandons only the act: the channel stays up,
// the next call works, and the late reply is dropped. The guest runs "slow" in the act lane and
// "echo" beside it -- the two lanes ServeFrames serves.
func TestCallsMultiplexAndADeadlineAbandonsOnlyItsCall(t *testing.T) {
	cc, sc := net.Pipe()
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	go ServeFrames(context.Background(), sc, slowDispatch(release, started), func(verb string) bool { return verb == "slow" })
	c := NewConn(cc)
	t.Cleanup(func() { c.Close() })

	slow := make(chan error, 1)
	sctx, scancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer scancel()
	go func() { slow <- c.Call(sctx, "slow", nil, nil) }()
	<-started // the slow request is in the guest's hands before the echo is sent

	// Beside it, an echo answers at once.
	var out string
	ectx, ecancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer ecancel()
	if err := c.Call(ectx, "echo", "x", &out); err != nil || out != "xx" {
		t.Fatalf("echo beside a slow verb = (%q, %v), want it answered", out, err)
	}

	// The slow call's deadline: its own error, not a dead channel -- the echo proved the guest alive.
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

// An abandoned act is stopped in the guest: the host's deadline abandons a hung "slow" act and
// sends cancel, the handler sees its context cancelled and returns, and the freed act mutex
// serves the next act instead of queueing it behind a hung one forever. The echo beside it proves
// the guest alive throughout (the abandon path, not the liveness close); the cancelled handler is
// observed directly.
func TestAnAbandonedActIsCancelledInTheGuest(t *testing.T) {
	cc, sc := net.Pipe()
	cancelled := make(chan struct{}, 1)
	started := make(chan struct{}, 1)
	hang := func(ctx context.Context, verb string, payload json.RawMessage) (any, error) {
		if verb == "slow" {
			started <- struct{}{}
			<-ctx.Done() // never released: only a cancel from the host ends this
			cancelled <- struct{}{}
			return nil, ctx.Err()
		}
		return echoDispatch(ctx, verb, payload)
	}
	go ServeFrames(context.Background(), sc, hang, func(verb string) bool { return verb != "echo" })
	c := NewConn(cc)
	t.Cleanup(func() { c.Close() })

	slow := make(chan error, 1)
	sctx, scancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer scancel()
	go func() { slow <- c.Call(sctx, "slow", nil, nil) }()
	<-started // in the guest's hands before the echo, so the echo's reply counts for it
	var out string
	ectx, ecancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer ecancel()
	if err := c.Call(ectx, "echo", "x", &out); err != nil {
		t.Fatalf("echo beside the hung act: %v", err)
	}
	if err := <-slow; !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrChannelDown) {
		t.Fatalf("the hung act = %v, want its own deadline with the channel up", err)
	}
	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("the guest never cancelled the abandoned act's handler")
	}
	// The act lane is free: another act is served, not queued behind the hung one.
	actx, acancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer acancel()
	if err := c.Call(actx, "boom", nil, nil); err == nil || err.Error() != "kaboom" {
		t.Fatalf("the next act after a cancelled one = %v, want it served (kaboom)", err)
	}
}
