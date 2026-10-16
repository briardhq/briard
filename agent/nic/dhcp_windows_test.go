package nic

import (
	"bytes"
	"net"
	"net/netip"
	"testing"
)

// fakeConn records what is written to it.
type fakeConn struct {
	net.PacketConn
	name string
	log  *[]string
	sent *[][]byte
}

func (f fakeConn) WriteTo(b []byte, addr net.Addr) (int, error) {
	if u := addr.(*net.UDPAddr); !u.IP.Equal(net.IPv4bcast) || u.Port != 67 {
		*f.log = append(*f.log, f.name+" to "+addr.String())
	} else {
		*f.log = append(*f.log, f.name)
	}
	*f.sent = append(*f.sent, append([]byte(nil), b...))
	return len(b), nil
}

// THE TWO FRAMES: the decoy from the port-68 socket first -- the message with type 255, nothing
// else changed -- then the message itself from the sender, both broadcast to 67.
func TestSendTwoFrames(t *testing.T) {
	c := DHCPClient{Chaddr: flockMAC, Hostname: "briard-d30401"}
	for _, kind := range []byte{dhcpDiscover, dhcpRequest} {
		msg := c.message(0xdeadbeef, kind, netip.MustParseAddr("192.168.7.120"), netip.Addr{})
		var log []string
		var sent [][]byte
		u := &udpConn{tx: fakeConn{name: "tx", log: &log, sent: &sent}, hold: fakeConn{name: "hold", log: &log, sent: &sent}}
		if err := u.send(msg); err != nil {
			t.Fatal(err)
		}
		if len(log) != 2 || log[0] != "hold" || log[1] != "tx" {
			t.Fatalf("sends = %v, want [hold tx]", log)
		}
		if !bytes.Equal(sent[1], msg) {
			t.Errorf("the message was altered on the way out")
		}
		want := append([]byte(nil), msg...)
		want[242] = 255
		if !bytes.Equal(sent[0], want) {
			t.Errorf("decoy differs from the message beyond its type: %x", sent[0][240:250])
		}
		if kind, _, ok := parseDHCP(append([]byte{2}, sent[0][1:]...), 0xdeadbeef, flockMAC); !ok || kind != 255 {
			t.Errorf("decoy type = %d, want 255", kind)
		}
	}
}
