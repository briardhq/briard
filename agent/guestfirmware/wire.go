package guestfirmware

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
)

// The framing of the host<->guest control channel: length-prefixed JSON request/response over
// any io.ReadWriteCloser -- a net.Pipe in tests, a virtio-serial port in the guest. It is the
// guest link's only transport, and it lives in the FIRMWARE because the firmware is the half of
// the protocol the image bakes: the pushed agent layers its own verbs on this framing
// and never redefines it.

const maxFrame = 8 << 20 // 8 MiB cap so a corrupt length prefix can't allocate wildly

type request struct {
	ID      uint64          `json:"id"`
	Verb    string          `json:"verb"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// cancelRequest is the payload of VerbCancel: the id of the request to stop.
type cancelRequest struct {
	ID uint64 `json:"id"`
}

type response struct {
	ID      uint64          `json:"id"`
	Error   string          `json:"error,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

func writeFrame(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if len(b) > maxFrame {
		return fmt.Errorf("guestfirmware: frame too large (%d bytes)", len(b))
	}
	// One Write for header+payload: two Writes could tear a frame if the ctx watcher
	// closes the channel between them, byte-desyncing the peer (which frame-level resync
	// can't recover). A single small write lands whole, so a dropped session leaves
	// at most a *complete* stale frame -- skippable by id.
	frame := make([]byte, 4+len(b))
	binary.BigEndian.PutUint32(frame[:4], uint32(len(b)))
	copy(frame[4:], b)
	_, err = w.Write(frame)
	return err
}

func readFrame(r io.Reader, v any) error {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n > maxFrame {
		return fmt.Errorf("guestfirmware: frame too large (%d bytes)", n)
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// Conn is the host end: calls multiplexed over the single stream, matched to their replies
// by id. Any number may be in flight; the guest answers them in whatever order it serves them.
//
// A call's deadline abandons THAT call -- its reply, if it ever comes, is dropped, and the
// guest is told to stop it (VerbCancel), so a hung act does not hold the guest's act lane --
// and leaves the channel up for everything else, with one exception that is the liveness rule:
// if nothing at all has arrived from the guest since the call was sent, the guest is not
// answering anything, and the deadline closes the channel so the host re-dials and, failing
// that, climbs the recovery ladder. A guest that answered something meanwhile is alive and
// merely slow on this verb. Against a guest that serves one verb at a time the two cases are
// the same case, and this is exactly the old behaviour; against one that serves reads beside a
// long act, a timed-out act costs nothing but itself.
type Conn struct {
	rw io.ReadWriteCloser
	// wmu serialises frames onto the stream: writeFrame is one Write, but two of them must
	// not interleave.
	wmu sync.Mutex
	// mu guards everything below.
	mu      sync.Mutex
	nextID  uint64
	pending map[uint64]chan response
	// lastReply is when the reader last read a frame -- any frame, any id. The liveness rule
	// above compares a timed-out call's send time against it; alive says whether any frame has
	// ever arrived, because the rule applies only to a connection that has proven itself once.
	lastReply time.Time
	alive     bool
	// readErr is why the reader stopped; set once, after which every call fails with it.
	readErr  error
	readDone chan struct{}
	readOnce sync.Once
}

// NewConn wraps a stream in a host-side Conn. Request ids start from the WALL CLOCK, not
// from 1, because the stream OUTLIVES the process at both ends: QEMU keeps the guest port
// open across a host re-dial, so an agent killed mid-call leaves its reply sitting in the
// channel for its successor to read. A reply whose id nothing is waiting on is dropped, which
// separates the two sessions only while their ids differ -- and ids that restart at 1
// collide on the one frame every session has, the reply to its hello. A clock base makes a
// later session's ids strictly greater than an earlier session's, so a leftover frame is
// decidably stale rather than coincidentally distinguishable.
func NewConn(rw io.ReadWriteCloser) *Conn {
	return &Conn{rw: rw, nextID: uint64(time.Now().UnixNano()), pending: map[uint64]chan response{}, readDone: make(chan struct{})}
}

// reader is the one goroutine that reads the stream, started by the first call. Every frame
// goes to the call waiting on its id, or nowhere: a reply to an abandoned call, or one left by
// a previous session, is dropped. It ends on the first read error, failing every pending call.
func (c *Conn) reader() {
	for {
		var resp response
		err := readFrame(c.rw, &resp)
		c.mu.Lock()
		if err != nil {
			c.readErr = err
			for id, ch := range c.pending {
				delete(c.pending, id)
				close(ch)
			}
			c.mu.Unlock()
			close(c.readDone)
			return
		}
		c.lastReply, c.alive = time.Now(), true
		if ch, ok := c.pending[resp.ID]; ok {
			delete(c.pending, resp.ID)
			ch <- resp
		}
		c.mu.Unlock()
	}
}

func (c *Conn) Call(ctx context.Context, verb string, arg, reply any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var payload json.RawMessage
	if arg != nil {
		b, err := json.Marshal(arg)
		if err != nil {
			return err
		}
		payload = b
	}
	c.readOnce.Do(func() { go c.reader() })

	c.mu.Lock()
	if c.readErr != nil {
		c.mu.Unlock()
		return channelDown(ctx, c.readErr)
	}
	c.nextID++
	id := c.nextID
	ch := make(chan response, 1)
	c.pending[id] = ch
	c.mu.Unlock()
	sent := time.Now()

	c.wmu.Lock()
	err := writeFrame(c.rw, request{ID: id, Verb: verb, Payload: payload})
	c.wmu.Unlock()
	if err != nil {
		c.abandon(id)
		_ = c.rw.Close() // a stream that will not take a frame is not a channel
		return channelDown(ctx, err)
	}

	select {
	case resp, ok := <-ch:
		if !ok {
			c.mu.Lock()
			rerr := c.readErr
			c.mu.Unlock()
			return channelDown(ctx, rerr)
		}
		if resp.Error != "" {
			return errors.New(resp.Error)
		}
		if reply != nil && len(resp.Payload) > 0 {
			return json.Unmarshal(resp.Payload, reply)
		}
		return nil
	case <-ctx.Done():
		c.abandon(id)
		c.mu.Lock()
		silent, alive := c.lastReply.Before(sent), c.alive
		c.mu.Unlock()
		switch {
		case silent && alive:
			// The liveness rule: nothing answered since this was sent, on a connection that
			// used to answer. Close, so the reader ends and the host re-dials; the error
			// carries both facts, as it always has.
			_ = c.rw.Close()
			return channelDown(ctx, ctx.Err())
		case silent:
			// Nothing has EVER arrived here: not a guest that stopped answering but one that
			// has not answered yet -- the handshake's case, where the guest agent may be
			// between an EOF and its restart and the request written meanwhile is lost with
			// the port it held. The caller decides; sending again on this same connection is
			// what reaches the agent that opens the port next.
			return ctx.Err()
		default:
			c.cancel(id)
			return ctx.Err()
		}
	}
}

// cancel tells the guest to stop the request with this id: fire-and-forget, its own id, its
// reply dropped by the reader like any other reply nothing waits on. An older firmware answers
// "unknown verb", which costs the same nothing. A write error here is the stream going away,
// which the reader reports on its own.
func (c *Conn) cancel(id uint64) {
	payload, _ := json.Marshal(cancelRequest{ID: id})
	c.mu.Lock()
	c.nextID++
	cid := c.nextID
	c.mu.Unlock()
	c.wmu.Lock()
	_ = writeFrame(c.rw, request{ID: cid, Verb: VerbCancel, Payload: payload})
	c.wmu.Unlock()
}

// abandon forgets a call: its reply, if one comes, is dropped by the reader.
func (c *Conn) abandon(id uint64) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}

func (c *Conn) Close() error { return c.rw.Close() }

// ctxOr prefers the context's error (cancel/deadline) over the I/O error it caused
// -- e.g. the "closed pipe" from the watcher closing the channel on timeout.
func ctxOr(ctx context.Context, err error) error {
	if cerr := ctx.Err(); cerr != nil {
		return cerr
	}
	return err
}

// ErrChannelDown marks a transport-level failure of the control channel: a write or read
// failed, or a call's deadline passed with the guest answering nothing at all (Conn's liveness
// rule). The host re-dials on it (host.Run's reconnect loop). A *verb* error -- the guest ran
// the request and returned an error string -- is NOT this: the round-trip completed, the
// channel is fine, so callers see the plain error and keep using the connection. Nor is a
// deadline on one call while the guest answers others: that is the call's own
// context.DeadlineExceeded, and the channel stays up. When the liveness rule does close it,
// the error wraps *both*, so a bounded op still sees its timeout while the observe loop sees
// the dead channel.
var ErrChannelDown = errors.New("guestfirmware: control channel down")

func channelDown(ctx context.Context, err error) error {
	return fmt.Errorf("%w: %w", ErrChannelDown, ctxOr(ctx, err))
}

// ServeFrames is the guest end: read requests, dispatch each to d in its own goroutine, write
// responses as they complete, until the connection closes (returns nil on EOF) or ctx is done.
//
// TWO LANES. A verb for which act reports true takes the one act mutex, so acts run one at a
// time in the order they arrived -- a pull, a converge, a format, a ring write never interleave.
// Every other verb runs at once, beside whatever act is in flight: the handshake, a status
// read, a health probe, the host's push of an alert copy all answer while an install pulls its
// image, which is what lets the host tell a slow guest from a dead one. The host matches
// replies by id, so answering out of order is the protocol working, not a fault.
//
// ⚠️ CANCELLATION CLOSES THE CONNECTION HERE, AND ONLY BETWEEN REPLIES. The blocking read on a
// virtio-serial port cannot be interrupted by a context, so something has to close the port out
// from under it; that used to be the caller, the instant ctx was done. It cost "at most one
// in-flight reply" -- except for `os.poweroff`, where the reply lost is always the one the host is
// waiting on: the shutdown that verb starts is what SIGTERMs this process, so the race was not a
// rare interleaving but the guaranteed outcome of the one verb whose answer decides what the host
// does next. A lost reply reads as EOF, EOF is indistinguishable from a crashed agent, and the
// host escalated to the ACPI power button on a guest that had done exactly as it was asked.
//
// So the close waits for every reply owed -- each handler holds the connection open for its
// run, and the close takes it from all of them at once -- and nothing after it: the loop reads
// no further request once ctx is done, so what the close waits for is bounded by what was in
// flight. An act keeps running to its end whether the host is listening or not, exactly as it
// did when the loop could not read the next frame until it finished, and the caller's own exit
// deadline remains the backstop for a handler that never returns.
//
// ...UNLESS THE HOST CANCELS IT. VerbCancel names a request in flight; the loop handles it here,
// never d: the request's context is cancelled, exec.CommandContext kills its child, the handler
// returns, and whatever it held -- the act mutex above all -- is free again. This is what the
// host sends when a deadline abandons a call, and it is what keeps a hung act from holding the
// guest's act lane until the next restart. A cancel for an id nothing is running is a no-op,
// answered like any other request so the host's Conn can drop the reply.
func ServeFrames(ctx context.Context, rw io.ReadWriteCloser, d DispatchFunc, act func(verb string) bool) error {
	var (
		open    sync.RWMutex   // held (read) by every handler for its run; taken (write) to close
		writing sync.Mutex     // one reply on the stream at a time
		acting  sync.Mutex     // one act at a time
		wg      sync.WaitGroup // every handler, so the return waits for them
		// inflight is the cancel for every request still being handled, by id, for VerbCancel.
		inflightMu sync.Mutex
		inflight   = map[uint64]context.CancelFunc{}
	)
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
		case <-done:
			return
		}
		open.Lock() // let every answer owed reach the host
		rw.Close()
		open.Unlock()
	}()
	defer wg.Wait()

	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		var req request
		if err := readFrame(rw, &req); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrClosedPipe) {
				return nil
			}
			return err
		}
		hctx, hcancel := context.WithCancel(ctx)
		inflightMu.Lock()
		inflight[req.ID] = hcancel
		inflightMu.Unlock()
		wg.Add(1)
		go func(req request) {
			defer wg.Done()
			defer func() {
				inflightMu.Lock()
				delete(inflight, req.ID)
				inflightMu.Unlock()
				hcancel()
			}()
			open.RLock()
			defer open.RUnlock()
			if req.Verb == VerbCancel {
				var c cancelRequest
				if err := json.Unmarshal(req.Payload, &c); err == nil {
					inflightMu.Lock()
					if cancel, ok := inflight[c.ID]; ok {
						cancel()
					}
					inflightMu.Unlock()
				}
				writing.Lock()
				defer writing.Unlock()
				_ = writeFrame(rw, response{ID: req.ID})
				return
			}
			if act(req.Verb) {
				acting.Lock()
				defer acting.Unlock()
			}
			resp := response{ID: req.ID}
			if result, herr := d(hctx, req.Verb, req.Payload); herr != nil {
				resp.Error = herr.Error()
			} else if result != nil {
				if b, merr := json.Marshal(result); merr != nil {
					resp.Error = merr.Error()
				} else {
					resp.Payload = b
				}
			}
			writing.Lock()
			defer writing.Unlock()
			// A write that fails is a stream that is gone; the read loop sees the same and ends.
			_ = writeFrame(rw, resp)
		}(req)
	}
}

// DispatchFunc handles one request verb and returns a result to marshal back.
type DispatchFunc func(ctx context.Context, verb string, payload json.RawMessage) (any, error)
