package nic

import (
	"bytes"
	"encoding/binary"
	"net"
	"net/netip"
	"testing"
	"time"
)

var flockMAC = net.HardwareAddr{0x02, 0x42, 0xb1, 0xa2, 0xd3, 0x04}

// opts reads a message's options back into a map, the parser's own way.
func opts(t *testing.T, p []byte) map[byte][]byte {
	t.Helper()
	o := map[byte][]byte{}
	for i := 240; i < len(p) && p[i] != 255; {
		if p[i] == 0 {
			i++
			continue
		}
		o[p[i]] = p[i+2 : i+2+int(p[i+1])]
		i += 2 + int(p[i+1])
	}
	return o
}

func TestDHCPMessages(t *testing.T) {
	c := DHCPClient{Chaddr: flockMAC, Hostname: "briard-d30401"}
	addr, server := netip.MustParseAddr("192.168.7.120"), netip.MustParseAddr("192.168.7.1")
	for _, tc := range []struct {
		name           string
		kind           byte
		req, server    netip.Addr
		want50, want54 bool
	}{
		{"discover", dhcpDiscover, netip.Addr{}, netip.Addr{}, false, false},
		{"request (selecting)", dhcpRequest, addr, server, true, true},
		{"init-reboot", dhcpRequest, addr, netip.Addr{}, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := c.message(0xdeadbeef, tc.kind, tc.req, tc.server)
			if len(p) < 300 {
				t.Fatalf("message is %d bytes, under BOOTP's 300", len(p))
			}
			if p[0] != 1 || p[2] != 6 || binary.BigEndian.Uint32(p[4:8]) != 0xdeadbeef {
				t.Fatalf("header: op=%d hlen=%d xid=%x", p[0], p[2], p[4:8])
			}
			// THE RULES: broadcast reply, no ciaddr, chaddr and client-id both the flock's.
			if p[10] != 0x80 {
				t.Errorf("broadcast flag not set: %x", p[10:12])
			}
			if !bytes.Equal(p[12:16], make([]byte, 4)) {
				t.Errorf("ciaddr is set (%v): the reply would go to whoever holds it", p[12:16])
			}
			if !bytes.Equal(p[28:34], flockMAC) {
				t.Errorf("chaddr = %x, want the flock's", p[28:34])
			}
			o := opts(t, p)
			if !bytes.Equal(o[53], []byte{tc.kind}) {
				t.Errorf("type = %v", o[53])
			}
			if !bytes.Equal(o[61], append([]byte{1}, flockMAC...)) {
				t.Errorf("client-id = %x, want 01:<flock>", o[61])
			}
			if _, ok := o[50]; ok != tc.want50 {
				t.Errorf("option 50 present = %v, want %v", ok, tc.want50)
			}
			if _, ok := o[54]; ok != tc.want54 {
				t.Errorf("option 54 present = %v, want %v", ok, tc.want54)
			}
			if string(o[12]) != c.Hostname {
				t.Errorf("hostname = %q", o[12])
			}
		})
	}
}

// reply renders a server's answer the way a router would.
func reply(xid uint32, chaddr net.HardwareAddr, kind byte, yi string, extra ...[]byte) []byte {
	p := make([]byte, 240)
	p[0], p[1], p[2] = 2, 1, 6
	binary.BigEndian.PutUint32(p[4:8], xid)
	a := netip.MustParseAddr(yi).As4()
	copy(p[16:20], a[:])
	copy(p[28:34], chaddr)
	copy(p[236:240], dhcpCookie)
	p = append(p, 53, 1, kind)
	for _, e := range extra {
		p = append(p, e...)
	}
	return append(p, 255)
}

func TestParseDHCP(t *testing.T) {
	ok := reply(7, flockMAC, dhcpACK, "192.168.7.120",
		[]byte{1, 4, 255, 255, 255, 0}, []byte{54, 4, 192, 168, 7, 1}, []byte{51, 4, 0, 0, 0x0e, 0x10})
	kind, l, good := parseDHCP(ok, 7, flockMAC)
	if !good || kind != dhcpACK {
		t.Fatalf("an ACK did not parse: kind=%d ok=%v", kind, good)
	}
	if l.Addr != netip.MustParsePrefix("192.168.7.120/24") || l.Server != netip.MustParseAddr("192.168.7.1") ||
		l.Time != time.Hour || l.T1 != 30*time.Minute {
		t.Errorf("lease = %+v", l)
	}
	_, l, _ = parseDHCP(reply(7, flockMAC, dhcpOffer, "10.0.0.9", []byte{1, 4, 255, 255, 0, 0}, []byte{58, 4, 0, 0, 0, 60}), 7, flockMAC)
	if l.Addr.Bits() != 16 || l.T1 != time.Minute {
		t.Errorf("mask or T1 not read: %+v", l)
	}
	station := net.HardwareAddr{0x02, 0, 0, 0, 0, 0x99}
	for name, b := range map[string][]byte{
		"another xid":            reply(8, flockMAC, dhcpACK, "192.168.7.120"),
		"another client's reply": reply(7, station, dhcpACK, "192.168.7.120"),
		"our own request":        func() []byte { b := reply(7, flockMAC, dhcpACK, "192.168.7.120"); b[0] = 1; return b }(),
		"short":                  ok[:100],
	} {
		if _, _, got := parseDHCP(b, 7, flockMAC); got {
			t.Errorf("%s parsed as ours", name)
		}
	}
	// An option running past the end stops the read; it never reads out of bounds.
	parseDHCP(append(reply(7, flockMAC, dhcpACK, "1.2.3.4")[:243], 51, 4, 0), 7, flockMAC)
}
