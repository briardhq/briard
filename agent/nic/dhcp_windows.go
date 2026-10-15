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

// THE WINDOWS TRANSPORT: two UDP sockets on port 68, beside Windows' own DHCP client.
//
// The sender is bound to the bridge's own address, which is what picks the interface a limited
// broadcast leaves by; the bridge rewrites its MEMBERS' frames on a Wi-Fi member, never the host's
// own, so the chaddr reaches the router intact (measured on the rig). Windows hands a unicast
// reply to the socket bound to the specific address and a broadcast to the wildcard one, so both
// are read. SO_REUSEADDR on both is what lets them share 68 with the system's client.

type udpConn struct {
	tx, rx net.PacketConn
	got    chan []byte
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
	tx, err := lc.ListenPacket(ctx, "udp4", net.JoinHostPort(addrs[0], "68"))
	if err != nil {
		return nil, fmt.Errorf("nic: dhcp socket on %s: %w", addrs[0], err)
	}
	rx, err := lc.ListenPacket(ctx, "udp4", "0.0.0.0:68")
	if err != nil {
		tx.Close()
		return nil, fmt.Errorf("nic: dhcp listener: %w", err)
	}
	c := &udpConn{tx: tx, rx: rx, got: make(chan []byte, 16)}
	for _, s := range []net.PacketConn{tx, rx} {
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

func (c *udpConn) send(payload []byte) error {
	_, err := c.tx.WriteTo(payload, &net.UDPAddr{IP: net.IPv4bcast, Port: 67})
	return err
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

func (c *udpConn) Close() error { return errors.Join(c.tx.Close(), c.rx.Close()) }
