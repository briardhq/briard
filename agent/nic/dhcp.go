package nic

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"time"
)

// THE FLOCK'S LEASE, ASKED FOR BY THE HOST.
//
// On a single-anchor substrate -- an ipvtap child of a wireless station, the Windows bridge -- the
// guest's frames leave with the HOST's L2 source, so a router that keys leases on the source, or on
// a chaddr the substrate rewrote to it, hands the guest the host's own address. A router keys on
// the payload's chaddr, never the frame's source: that is what every relay and every bridged VM
// rests on. So the host asks, in a frame of its own whose chaddr is the flock's MAC, and the guest
// is handed the answer.
//
// ONE IDENTITY: chaddr = the flock MAC AND client-id 01:<flock MAC>, the same pair the guest's own
// client presents on a wired substrate. A chaddr-keyed router and a client-id-keyed one both
// resolve it to the flock, so there is no server the guest's client would satisfy that this does
// not, and the router sees one client whether the machine is on Wi-Fi or on its cable.
//
// THREE MESSAGES, and none of them has ciaddr set. A REQUEST carrying ciaddr (RENEWING,
// REBINDING) is answered by unicast TO ciaddr (RFC 2131 4.1), which the substrate hands to whoever
// holds the address -- the guest, never the host that asked. INIT-REBOOT asks for the address by
// option 50 with ciaddr 0 and the broadcast flag, so its answer comes back to the host; servers
// answer it with a full lease, which makes it the renewal too. Every message sets the broadcast
// flag, because the address being asked for is not one the asking machine holds.

// DHCPClient asks for leases under Chaddr, from Dev: on Linux the ipvtap child in HoldNS (the frame
// leaves with the station's MAC), on Windows the bridge (a socket bound to its address).
type DHCPClient struct {
	Dev      string
	Chaddr   net.HardwareAddr
	Hostname string // option 12: what the router's client list shows
}

// DHCPLease is a server's OFFER or ACK.
type DHCPLease struct {
	Addr   netip.Prefix // yiaddr, with the server's subnet mask (/24 when it gave none)
	Server netip.Addr   // option 54, the server identifier
	Time   time.Duration
	T1     time.Duration // option 58, or half the lease (RFC 2131 4.4.5)
}

var (
	// ErrNoAnswer is silence: nothing answered every try.
	ErrNoAnswer = errors.New("no DHCP server answered")
	// ErrNAK is a server refusing the address asked for.
	ErrNAK = errors.New("the DHCP server refused the address (NAK)")
)

// The DHCP message types this client sends or reads (RFC 2132 9.6).
const (
	dhcpDiscover = 1
	dhcpOffer    = 2
	dhcpRequest  = 3
	dhcpACK      = 5
	dhcpNAK      = 6
)

// dhcpTries and dhcpWait are one exchange's patience: five sends, four seconds after each. A
// DISCOVER that twenty seconds of a household's router leaves unanswered is silence. Five, not
// three, because the reply is a broadcast, and Wi-Fi sends broadcasts unacknowledged and never
// retried: a station on a weak link loses some, and two in a row has been seen on a real radio. A
// working router costs nothing extra -- its first answer that arrives ends the exchange.
const (
	dhcpTries = 5
	dhcpWait  = 4 * time.Second
)

// dhcpConn is one exchange's transport, per OS (dhcp_linux.go, dhcp_windows.go). send takes the
// BOOTP payload and broadcasts it; recv returns the next BOOTP payload addressed to port 68, or an
// error once the deadline passes.
type dhcpConn interface {
	send(payload []byte) error
	recv(deadline time.Time) ([]byte, error)
	Close() error
}

// Discover broadcasts a DISCOVER and returns the first OFFER. The caller judges the offer before
// asking for it (Request): an address it must not hold is never requested. hint, when valid, is
// option 50: the address last held, which a router honours when nobody answers for it -- the
// request every client sends after its lease expires (RFC 2131 4.4.5, 3.5).
func (c DHCPClient) Discover(ctx context.Context, hint netip.Addr) (DHCPLease, error) {
	return c.exchange(ctx, func(xid uint32) []byte { return c.message(xid, dhcpDiscover, hint, netip.Addr{}) }, dhcpOffer)
}

// Request asks for an offer (SELECTING: option 50 and the offering server's identifier).
func (c DHCPClient) Request(ctx context.Context, offer DHCPLease) (DHCPLease, error) {
	return c.exchange(ctx, func(xid uint32) []byte {
		return c.message(xid, dhcpRequest, offer.Addr.Addr(), offer.Server)
	}, dhcpACK)
}

// Reboot asks to keep addr (INIT-REBOOT: option 50, no server identifier, ciaddr 0). It is the
// renewal as well as the restart, for the reason above.
func (c DHCPClient) Reboot(ctx context.Context, addr netip.Addr) (DHCPLease, error) {
	return c.exchange(ctx, func(xid uint32) []byte { return c.message(xid, dhcpRequest, addr, netip.Addr{}) }, dhcpACK)
}

// exchange sends one message under a fresh xid and waits for the reply of kind want. A NAK ends it
// at once; anything else -- another client's reply, our own frame looped back -- is skipped.
func (c DHCPClient) exchange(ctx context.Context, msg func(xid uint32) []byte, want byte) (DHCPLease, error) {
	conn, err := openDHCP(ctx, c.Dev)
	if err != nil {
		return DHCPLease{}, err
	}
	defer conn.Close()
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return DHCPLease{}, err
	}
	xid := binary.BigEndian.Uint32(b[:])
	payload := msg(xid)
	for range dhcpTries {
		if err := conn.send(payload); err != nil {
			return DHCPLease{}, fmt.Errorf("nic: dhcp send on %s: %w", c.Dev, err)
		}
		deadline := time.Now().Add(dhcpWait)
		if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
			deadline = d
		}
		for time.Now().Before(deadline) {
			raw, err := conn.recv(deadline)
			if err != nil {
				break
			}
			kind, l, ok := parseDHCP(raw, xid, c.Chaddr)
			switch {
			case !ok:
			case kind == dhcpNAK:
				return DHCPLease{}, ErrNAK
			case kind == want:
				return l, nil
			}
		}
		if ctx.Err() != nil {
			return DHCPLease{}, ctx.Err()
		}
	}
	return DHCPLease{}, ErrNoAnswer
}

// message renders one client message. req is option 50 and server option 54, each only when set.
// Pure, so the bytes are unit-tested.
func (c DHCPClient) message(xid uint32, kind byte, req, server netip.Addr) []byte {
	p := make([]byte, 240, 300)
	p[0], p[1], p[2] = 1, 1, 6 // BOOTREQUEST, ethernet, hlen 6
	binary.BigEndian.PutUint32(p[4:8], xid)
	p[10] = 0x80 // the broadcast flag
	copy(p[28:34], c.Chaddr)
	copy(p[236:240], dhcpCookie)
	opt := func(code byte, data []byte) { p = append(append(p, code, byte(len(data))), data...) }
	opt(53, []byte{kind})
	opt(61, append([]byte{1}, c.Chaddr...)) // client-id: type 1 (ethernet) + the flock MAC
	if req.IsValid() {
		a := req.As4()
		opt(50, a[:])
	}
	if server.IsValid() {
		a := server.As4()
		opt(54, a[:])
	}
	if c.Hostname != "" {
		opt(12, []byte(c.Hostname))
	}
	opt(55, []byte{1, 3, 51, 54, 58}) // mask, router, lease, server id, T1
	p = append(p, 255)
	// BOOTP's 300-byte floor: udhcpd (busybox, inside many routers) drops anything shorter.
	for len(p) < 300 {
		p = append(p, 0)
	}
	return p
}

var dhcpCookie = []byte{99, 130, 83, 99}

// parseDHCP reads a server's reply to xid for chaddr: its message type and the lease it carries.
// ok is false for anything else. Pure, for the same reason as message.
func parseDHCP(b []byte, xid uint32, chaddr net.HardwareAddr) (kind byte, l DHCPLease, ok bool) {
	if len(b) < 240 || b[0] != 2 || binary.BigEndian.Uint32(b[4:8]) != xid ||
		string(b[236:240]) != string(dhcpCookie) || string(b[28:28+len(chaddr)]) != string(chaddr) {
		return 0, DHCPLease{}, false
	}
	opts := map[byte][]byte{}
	for i := 240; i < len(b); {
		code := b[i]
		if code == 255 {
			break
		}
		if code == 0 {
			i++
			continue
		}
		if i+1 >= len(b) || i+2+int(b[i+1]) > len(b) {
			break
		}
		opts[code] = b[i+2 : i+2+int(b[i+1])]
		i += 2 + int(b[i+1])
	}
	if t := opts[53]; len(t) == 1 {
		kind = t[0]
	} else {
		return 0, DHCPLease{}, false
	}
	bits := 24
	if m := opts[1]; len(m) == 4 {
		bits, _ = net.IPMask(m).Size()
	}
	yi := netip.AddrFrom4([4]byte(b[16:20]))
	l.Addr = netip.PrefixFrom(yi, bits)
	if s := opts[54]; len(s) == 4 {
		l.Server = netip.AddrFrom4([4]byte(s))
	}
	if t := opts[51]; len(t) == 4 {
		l.Time = time.Duration(binary.BigEndian.Uint32(t)) * time.Second
	}
	l.T1 = l.Time / 2
	if t := opts[58]; len(t) == 4 {
		l.T1 = time.Duration(binary.BigEndian.Uint32(t)) * time.Second
	}
	return kind, l, true
}
