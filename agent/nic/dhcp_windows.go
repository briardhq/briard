package nic

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"syscall"
	"time"
)

// THE WINDOWS TRANSPORT: two frames per message, past the bridge's DHCP rewriter.
//
// A Windows bridge in compatibility mode (what it switches on by itself for a Wi-Fi member, and
// may on a wired one) rewrites the chaddr of the DHCP requests it forwards to the member's MAC --
// the host's own included, so the router would lease the flock the host's address. It classifies
// as DHCP only UDP with BOTH ports in {67, 68}, and rewrites only what it classifies: a request
// from another source port leaves as plain IP, the flock's chaddr intact. Servers answer it at
// port 68 regardless (RFC 2131 fixes the destination ports only). But the bridge's inbound side
// classifies that reply and drops one whose xid it never saw leave -- in every member kind and
// state, measured -- so each message goes twice: first a DECOY from port 68, the same bytes with
// message type 255, which the bridge records under the xid and no server answers; then the real
// one from an ephemeral port. Nothing here reads the member kind or its mode; the one path holds
// in all of them, which is also why a cable swap changes nothing.
//
// Three sockets, all SO_REUSEADDR (Windows' own DHCP client holds 68): the sender on the
// bridge's address and an ephemeral port, which picks the interface a limited broadcast leaves
// by; a socket holding <bridge>:68, which sends the decoy and is where Windows hands a broadcast
// reply (to the most specific port-68 socket -- its own client's, were this one absent); and the
// wildcard 0.0.0.0:68. All three are read.

type udpConn struct {
	tx, hold, rx net.PacketConn
	got          chan []byte
}

func openDHCP(ctx context.Context, dev string) (dhcpConn, error) {
	addrs := IPv4s(dev)
	if len(addrs) == 0 {
		return nil, fmt.Errorf("nic: dhcp on %s: it has no IPv4 address yet", dev)
	}
	lc := net.ListenConfig{Control: func(_, _ string, rc syscall.RawConn) error {
		var serr error
		err := rc.Control(func(fd uintptr) {
			h := syscall.Handle(fd)
			if serr = syscall.SetsockoptInt(h, syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1); serr == nil {
				serr = syscall.SetsockoptInt(h, syscall.SOL_SOCKET, syscall.SO_BROADCAST, 1)
			}
		})
		return errors.Join(err, serr)
	}}
	var socks []net.PacketConn
	for _, a := range []string{net.JoinHostPort(addrs[0], "0"), net.JoinHostPort(addrs[0], "68"), "0.0.0.0:68"} {
		s, err := lc.ListenPacket(ctx, "udp4", a)
		if err != nil {
			for _, s := range socks {
				s.Close()
			}
			return nil, fmt.Errorf("nic: dhcp socket on %s: %w", a, err)
		}
		socks = append(socks, s)
	}
	c := &udpConn{tx: socks[0], hold: socks[1], rx: socks[2], got: make(chan []byte, 16)}
	for _, s := range socks {
		go func() {
			for {
				buf := make([]byte, 1600)
				n, _, err := s.ReadFrom(buf)
				if err != nil {
					return // closed
				}
				select {
				case c.got <- buf[:n]:
				default: // nobody is reading this fast; a reply worth having is resent
				}
			}
		}()
	}
	return c, nil
}

// send broadcasts the decoy from port 68, then the message from the sender.
func (c *udpConn) send(payload []byte) error {
	to := &net.UDPAddr{IP: net.IPv4bcast, Port: 67}
	if _, err := c.hold.WriteTo(decoy(payload), to); err != nil {
		return fmt.Errorf("decoy: %w", err)
	}
	_, err := c.tx.WriteTo(payload, to)
	return err
}

// decoy is payload with its message type (option 53, the first option message writes) set to
// 255, which is no DHCP message type.
func decoy(payload []byte) []byte {
	d := append([]byte(nil), payload...)
	d[242] = 255
	return d
}

func (c *udpConn) recv(deadline time.Time) ([]byte, error) {
	t := time.NewTimer(time.Until(deadline))
	defer t.Stop()
	select {
	case b := <-c.got:
		return b, nil
	case <-t.C:
		return nil, os.ErrDeadlineExceeded
	}
}

func (c *udpConn) Close() error { return errors.Join(c.tx.Close(), c.hold.Close(), c.rx.Close()) }
