package nic

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"runtime"
	"time"

	"golang.org/x/sys/unix"
)

// THE LINUX TRANSPORT: a packet socket on an ipvtap child, opened inside HoldNS.
//
// The child carries the station's MAC, so a frame sent on it leaves the radio as the station's own
// -- the one L2 source an access point carries -- with the flock's chaddr inside. ipvlan hands the
// child every broadcast that reaches the parent, which is how the reply comes back (every message
// asks for a broadcast reply). The socket belongs to the namespace it was opened in, so only the
// open crosses into HoldNS; the exchange runs on any thread.
//
// ETH_P_ALL, not ETH_P_IP, and that is not a preference: the child's rx_handler hands every frame to
// the guest's tap and consumes it, and a protocol-specific packet socket is only offered a frame
// AFTER that handler. An ETH_P_ALL tap is offered it before, which is how tcpdump sees a tap too.
//
// SOCK_DGRAM: the kernel writes the ethernet header (the child's MAC to ff:ff:ff:ff:ff:ff), and
// this writes the IP and UDP ones -- from 0.0.0.0, to 255.255.255.255, 68 to 67 -- because the
// asking address is nobody's yet.

type packetConn struct {
	fd      int
	ifindex int
}

func openDHCP(_ context.Context, dev string) (dhcpConn, error) {
	type opened struct {
		c   *packetConn
		err error
	}
	ch := make(chan opened, 1)
	// A goroutine of its own, locked to its thread: if the way back out of HoldNS fails, the
	// thread stays locked and dies with the goroutine, rather than carrying a caller into the
	// wrong namespace.
	go func() {
		runtime.LockOSThread()
		self, err := os.Open("/proc/thread-self/ns/net")
		if err != nil {
			ch <- opened{err: err}
			return
		}
		defer self.Close()
		hold, err := os.Open("/run/netns/" + HoldNS)
		if err != nil {
			ch <- opened{err: err}
			return
		}
		defer hold.Close()
		if err := unix.Setns(int(hold.Fd()), unix.CLONE_NEWNET); err != nil {
			ch <- opened{err: fmt.Errorf("entering %s: %w", HoldNS, err)}
			return
		}
		c, err := openPacket(dev)
		if unix.Setns(int(self.Fd()), unix.CLONE_NEWNET) == nil {
			runtime.UnlockOSThread()
		}
		ch <- opened{c, err}
	}()
	o := <-ch
	if o.err != nil {
		return nil, fmt.Errorf("nic: dhcp socket on %s in %s: %w", dev, HoldNS, o.err)
	}
	return o.c, nil
}

// openPacket opens the socket on dev in the calling thread's namespace.
func openPacket(dev string) (*packetConn, error) {
	i, err := net.InterfaceByName(dev)
	if err != nil {
		return nil, err
	}
	fd, err := unix.Socket(unix.AF_PACKET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, int(htons(unix.ETH_P_ALL)))
	if err != nil {
		return nil, err
	}
	if err := unix.Bind(fd, &unix.SockaddrLinklayer{Protocol: htons(unix.ETH_P_ALL), Ifindex: i.Index}); err != nil {
		unix.Close(fd)
		return nil, err
	}
	return &packetConn{fd: fd, ifindex: i.Index}, nil
}

func (c *packetConn) send(payload []byte) error {
	to := &unix.SockaddrLinklayer{Protocol: htons(unix.ETH_P_IP), Ifindex: c.ifindex, Halen: 6}
	copy(to.Addr[:], []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff})
	return unix.Sendto(c.fd, ipUDP(payload), 0, to)
}

func (c *packetConn) recv(deadline time.Time) ([]byte, error) {
	buf := make([]byte, 1600)
	for {
		left := time.Until(deadline)
		if left <= 0 {
			return nil, os.ErrDeadlineExceeded
		}
		tv := unix.NsecToTimeval(left.Nanoseconds())
		if err := unix.SetsockoptTimeval(c.fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &tv); err != nil {
			return nil, err
		}
		n, from, err := unix.Recvfrom(c.fd, buf, 0)
		if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if ll, ok := from.(*unix.SockaddrLinklayer); !ok || ll.Protocol != htons(unix.ETH_P_IP) {
			continue
		}
		if p := udpTo68(buf[:n]); p != nil {
			return p, nil
		}
	}
}

func (c *packetConn) Close() error { return unix.Close(c.fd) }

// ipUDP wraps a BOOTP payload in IPv4 and UDP headers, 0.0.0.0:68 to 255.255.255.255:67. The UDP
// checksum is left 0 (none), which IPv4 allows and every server accepts.
func ipUDP(payload []byte) []byte {
	b := make([]byte, 28+len(payload))
	b[0], b[8], b[9] = 0x45, 64, unix.IPPROTO_UDP
	binary.BigEndian.PutUint16(b[2:4], uint16(len(b)))
	copy(b[16:20], []byte{255, 255, 255, 255})
	binary.BigEndian.PutUint16(b[10:12], checksum(b[:20]))
	binary.BigEndian.PutUint16(b[20:22], 68)
	binary.BigEndian.PutUint16(b[22:24], 67)
	binary.BigEndian.PutUint16(b[24:26], uint16(8+len(payload)))
	copy(b[28:], payload)
	return b
}

// udpTo68 is the UDP payload of an IPv4 packet to port 68, or nil.
func udpTo68(b []byte) []byte {
	if len(b) < 20 || b[0]>>4 != 4 || b[9] != unix.IPPROTO_UDP {
		return nil
	}
	ihl := int(b[0]&0xf) * 4
	if len(b) < ihl+8 || binary.BigEndian.Uint16(b[ihl+2:ihl+4]) != 68 {
		return nil
	}
	return b[ihl+8:]
}

func checksum(b []byte) uint16 {
	var s uint32
	for i := 0; i+1 < len(b); i += 2 {
		s += uint32(binary.BigEndian.Uint16(b[i:]))
	}
	for s>>16 != 0 {
		s = s&0xffff + s>>16
	}
	return ^uint16(s)
}

func htons(v uint16) uint16 { return v<<8 | v>>8 }
