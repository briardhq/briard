// Package tlspipe is the generic byte-stream transport under the cloud witness's TLS auth proxy
// : a bidirectional relay (Pipe) and the anchor-side Forwarder (listen localhost ->
// mTLS dial-out -> Pipe). It is the public, on-node half -- the server-side auth / one-connection
// gate / cert->flock policy stays closed in the cloud witness, which reuses only Pipe from here.
//
// briard uses it to tunnel a firewalled localhost DRBD connection over mTLS so neither the anchor's
// nor the witness's DRBD kernel module faces the internet; the toolkit itself is content-agnostic.
package tlspipe

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"net"
)

// Pipe relays a<->b until either direction ends, then closes both (a torn stream reads as a peer
// drop -> reconnect). Returns only once both copy goroutines have finished, so a caller can release
// per-connection state without leaking. Shared by the witness proxy (server) and the Forwarder
// (anchor) so the byte-shovelling lives in exactly one place.
func Pipe(a, b net.Conn) {
	done := make(chan struct{}, 2)
	cp := func(dst, src net.Conn) { io.Copy(dst, src); done <- struct{}{} }
	go cp(a, b)
	go cp(b, a)
	<-done    // one direction ended...
	a.Close() // ...so tear both down, unblocking the other io.Copy
	b.Close()
	<-done
}

// Forwarder is the anchor client side of the witness link. It accepts plaintext connections on a
// localhost listener (the local DRBD dials it as if it were the witness peer) and, for each, dials
// Target over mTLS presenting the anchor's client cert, then Pipes the two. So the anchor's DRBD
// kernel speaks only plaintext-localhost; all WAN exposure is this userspace mTLS hop.
type Forwarder struct {
	Target string      // the cloud proxy address, e.g. "witness.example:443"
	TLS    *tls.Config // client config: the anchor's cert + CA roots + ServerName
	Dialer *net.Dialer // optional; nil -> a default dialer (set a Timeout to bound the outbound dial)
}

// Serve accepts on ln until ctx is cancelled (which closes ln to unblock Accept). Each local
// connection is tunnelled on its own goroutine.
func (f *Forwarder) Serve(ctx context.Context, ln net.Listener) error {
	go func() { <-ctx.Done(); ln.Close() }()
	for {
		local, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil // ctx cancelled: clean shutdown
			}
			return fmt.Errorf("tlspipe forwarder accept: %w", err)
		}
		go f.handle(local)
	}
}

func (f *Forwarder) handle(local net.Conn) {
	d := f.Dialer
	if d == nil {
		d = &net.Dialer{}
	}
	remote, err := tls.DialWithDialer(d, "tcp", f.Target, f.TLS)
	if err != nil {
		log.Printf("tlspipe forwarder: dial %s: %v", f.Target, err)
		local.Close()
		return
	}
	Pipe(local, remote)
}
