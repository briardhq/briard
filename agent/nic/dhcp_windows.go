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
// bridge's LAN address (openDHCP says which) and an ephemeral port, which picks the interface a limited broadcast leaves
// by; a socket holding <bridge>:68, which sends the decoy and is where Windows hands a broadcast
// reply (to the most specific port-68 socket -- its own client's, were this one absent); and the
// wildcard 0.0.0.0:68. All three are read.

type udpConn struct {
	ctx          context.Context
	dev          string
	addr         string // what the sockets are bound to; "" while dev has no LAN address
	tx, hold, rx net.PacketConn
	got          chan []byte
}

// ⚠️ BOUND TO THE LAN ADDRESS, RE-READ BEFORE EVERY TRY. On briard's bridge the agent's own
// system-subnet /32 sits beside the LAN address, and the OS lists them in its own order; after a
// move to another network the bridge first holds that network's stale address, then only the
// /32, then the LAN's address back. A socket bound to either of the first two sends from an
// address the household's LAN has no business seeing, and the broadcast reply goes to whichever
// port-68 socket the stack finds more specific -- measured: a socket bound once to the /32 sent
// all five tries from it, the router answered, and the lease came only with a fresh socket on the
// LAN address 23 s after the radio was back. So the address is LANAddr's (the gateway's subnet,
// nothing else), read before each send, and a try with none sends nothing and waits its turn.
func openDHCP(ctx context.Context, dev string) (dhcpConn, error) {
	c := &udpConn{ctx: ctx, dev: dev, got: make(chan []byte, 16)}
	if err := c.bind(LANAddr(dev)); err != nil {
		return nil, err
	}
	return c, nil
}

// bind closes the sockets held and opens the three on addr; "" opens none.
func (c *udpConn) bind(addr string) error {
	c.closeSockets()
	c.addr = addr
	if addr == "" {
		return nil
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
	for _, a := range []string{net.JoinHostPort(addr, "0"), net.JoinHostPort(addr, "68"), "0.0.0.0:68"} {
		s, err := lc.ListenPacket(c.ctx, "udp4", a)
		if err != nil {
			for _, s := range socks {
				s.Close()
			}
			c.addr = ""
			return fmt.Errorf("nic: dhcp socket on %s: %w", a, err)
		}
		socks = append(socks, s)
	}
	c.tx, c.hold, c.rx = socks[0], socks[1], socks[2]
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
	return nil
}

func (c *udpConn) closeSockets() {
	for _, s := range []net.PacketConn{c.tx, c.hold, c.rx} {
		if s != nil {
			s.Close()
		}
	}
	c.tx, c.hold, c.rx = nil, nil, nil
}

// send broadcasts the decoy from port 68, then the message from the sender.
func (c *udpConn) send(payload []byte) error {
	if a := LANAddr(c.dev); a != c.addr {
		if err := c.bind(a); err != nil {
			return err
		}
	}
	if c.tx == nil {
		return nil // no LAN address yet: this try sends nothing
	}
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

func (c *udpConn) Close() error { c.closeSockets(); return nil }
