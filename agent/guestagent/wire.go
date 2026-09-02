package guestagent

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

// Wire is guestagent's private framing for the host<->guest control channel
// Wire is the framing: length-prefixed JSON request/response over any io.ReadWriteCloser
// -- a net.Pipe in tests, a virtio-serial port in the guest. It is the guest
// link's only transport, so it stays unexported; the public surface is
// Client / Serve / Executor.

const maxFrame = 8 << 20 // 8 MiB cap so a corrupt length prefix can't allocate wildly

// maxResyncSkips bounds how many stale frames Handshake skips to resync a reconnected
// channel. The protocol is synchronous (<=1 reply ever in flight), so 1 would do;
// the slack tolerates buffering without letting a genuinely broken stream loop forever.
const maxResyncSkips = 8

type request struct {
	ID      uint64          `json:"id"`
	Verb    string          `json:"verb"`
	Payload json.RawMessage `json:"payload,omitempty"`
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
		return fmt.Errorf("guestagent: frame too large (%d bytes)", len(b))
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
		return fmt.Errorf("guestagent: frame too large (%d bytes)", n)
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// conn is the host end: synchronous, serialized calls over the single stream
// .
type conn struct {
	mu     sync.Mutex
	rw     io.ReadWriteCloser
	nextID uint64
}

// newConn wraps a stream in a host-side conn. Request ids start from the WALL CLOCK, not
// from 1, because the stream OUTLIVES the process at both ends: QEMU keeps the guest port
// open across a host re-dial, so an agent killed mid-call leaves its reply sitting in the
// channel for its successor to read. Handshake resyncs past such a frame BY ID, which
// separates the two sessions only while their ids differ -- and ids that restart at 1
// collide on the one frame every session has, the reply to its hello [V3b.17]. A clock
// base makes a later session's ids strictly greater than an earlier session's, so a
// leftover frame is decidably stale rather than coincidentally distinguishable.
func newConn(rw io.ReadWriteCloser) *conn {
	return &conn{rw: rw, nextID: uint64(time.Now().UnixNano())}
}

func (c *conn) call(ctx context.Context, verb string, arg, reply any) error {
	return c.callResync(ctx, verb, arg, reply, false)
}

// CallResync is call with an optional resync: when resync is set, it skips stale
// reply frames -- ones whose id != our request id, left in the stream by a *previous*,
// dropped session -- up to maxResyncSkips, until the matching reply arrives. Over
// virtio-serial QEMU keeps the guest port open across a host reconnect, so a re-dial can
// find the previous session's in-flight reply ahead of ours; every frame readFrame yields
// is complete + well-framed (it ReadFulls the declared length), so the stale one is
// skippable by id -- and newConn's per-session id base is what keeps those ids apart.
// Only Handshake -- the first call after a (re)connect -- sets resync; a *mid-session*
// id mismatch stays a hard desync error (ErrChannelDown -> re-dial).
func (c *conn) callResync(ctx context.Context, verb string, arg, reply any, resync bool) error {
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
	c.mu.Lock()
	defer c.mu.Unlock()
	c.nextID++
	id := c.nextID

	// A stuck guest verb must not hang the host forever (the guest serves verbs
	// synchronously, so a slow one blocks our read indefinitely). Watch ctx and, on
	// cancel/deadline, close the channel to unblock the pending write/read. The channel
	// is single-shot after that -- its framing is desynced -- so the caller re-dials.
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			_ = c.rw.Close()
		case <-stop:
		}
	}()

	if err := writeFrame(c.rw, request{ID: id, Verb: verb, Payload: payload}); err != nil {
		return channelDown(ctx, err)
	}
	for skips := 0; ; skips++ {
		var resp response
		if err := readFrame(c.rw, &resp); err != nil {
			return channelDown(ctx, err)
		}
		if resp.ID == id {
			if resp.Error != "" {
				return errors.New(resp.Error)
			}
			if reply != nil && len(resp.Payload) > 0 {
				return json.Unmarshal(resp.Payload, reply)
			}
			return nil
		}
		// Reply id doesn't match our request. Without resync (a mid-session call) the
		// channel is desynced -- single-shot, re-dial. With resync (Handshake),
		// skip a bounded number of stale frames from the dropped session.
		if !resync || skips >= maxResyncSkips {
			return fmt.Errorf("%w: reply id %d != request id %d", ErrChannelDown, resp.ID, id)
		}
	}
}

func (c *conn) close() error { return c.rw.Close() }

// ctxOr prefers the context's error (cancel/deadline) over the I/O error it caused
// -- e.g. the "closed pipe" from the watcher closing the channel on timeout.
func ctxOr(ctx context.Context, err error) error {
	if cerr := ctx.Err(); cerr != nil {
		return cerr
	}
	return err
}

// ErrChannelDown marks a transport-level failure of the control channel: a write/read
// failed, or a per-call deadline closed it mid-frame. The channel is single-shot after
// any of these (its framing is desynced), so the host re-dials (host.Run's reconnect
// loop). A *verb* error -- the guest ran the request and returned an error string
// -- is NOT this: the round-trip completed, the channel is fine, so callers see the plain
// error and keep using the connection. A mid-call deadline wraps *both* ErrChannelDown and
// context.DeadlineExceeded, so a bounded op still detects its timeout while the observe
// loop still detects the dead channel.
var ErrChannelDown = errors.New("guestagent: control channel down")

func channelDown(ctx context.Context, err error) error {
	return fmt.Errorf("%w: %w", ErrChannelDown, ctxOr(ctx, err))
}

// serve is the guest end: read requests, dispatch to d, write responses, until
// the connection closes (returns nil on EOF) or ctx is done.
//
// ⚠️ CANCELLATION CLOSES THE CONNECTION HERE, AND ONLY BETWEEN REPLIES. The blocking read on a
// virtio-serial port cannot be interrupted by a context, so something has to close the port out
// from under it; that used to be the caller, the instant ctx was done. It cost "at most one
// in-flight reply" -- except for `os.poweroff`, where the reply lost is always the one the host is
// waiting on: the shutdown that verb starts is what SIGTERMs this process, so the race was not a
// rare interleaving but the guaranteed outcome of the one verb whose answer decides what the host
// does next. A lost reply reads as EOF, EOF is indistinguishable from a crashed agent, and the
// host escalated to the ACPI power button on a guest that had done exactly as it was asked
// ([B.127]).
//
// So the close waits for the reply being written, and only for that: after ctx is done the loop
// above returns before reading another request, so this can hold up the close by one handler and
// never by two. The caller's own exit deadline remains the backstop for a handler that never
// returns.
func serve(ctx context.Context, rw io.ReadWriteCloser, d dispatchFunc) error {
	var replying sync.Mutex
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
		case <-done:
			return
		}
		replying.Lock() // let an answer already being written reach the host
		replying.Unlock()
		rw.Close()
	}()

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
		if err := func() error {
			replying.Lock()
			defer replying.Unlock()
			resp := response{ID: req.ID}
			if result, herr := d(ctx, req.Verb, req.Payload); herr != nil {
				resp.Error = herr.Error()
			} else if result != nil {
				if b, merr := json.Marshal(result); merr != nil {
					resp.Error = merr.Error()
				} else {
					resp.Payload = b
				}
			}
			return writeFrame(rw, resp)
		}(); err != nil {
			return err
		}
	}
}

// dispatchFunc handles one request verb and returns a result to marshal back.
type dispatchFunc func(ctx context.Context, verb string, payload json.RawMessage) (any, error)
