package nic

import (
	"bufio"
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"net/netip"
	"os"
	"strconv"
	"strings"
)

// THE LAN FINGERPRINT, and what it is FOR ([B.150](e)).
//
// When the device the guest's L2 hangs off disappears, the agent has to decide how eagerly to
// rebuild on another one -- and that is not a networking question, it is a question about where
// the machine now IS. A NIC replaced in the same house wants a prompt rebuild. The same box
// carried to a friend's house wants nothing automatic at all, because a paired node on a
// different LAN is a split flock.
//
// ⚠️ "SAME SUBNET" IS NOT AN ANSWER. 192.168.1.0/24 is the most common home subnet on earth, so
// "the new device is on the same subnet" is also exactly what moving the box somewhere else looks
// like. The GATEWAY MAC is the strong signal: it identifies a specific piece of hardware on a
// specific segment. A replaced router renumbers what is unquestionably the same LAN, which is why
// the subnet alone cannot be trusted in the other direction either.

// Fingerprint is what this host could see of its LAN at the moment it last brought the guest up.
// Recorded then rather than sampled now, because its whole job is to be compared against a later
// reading.
type Fingerprint struct {
	Parent     string `json:"parent"`      // the device the guest's L2 hung off
	ParentMAC  string `json:"parent_mac"`  // its link address -- a NIC swap changes this, a rename does not
	HostCIDR   string `json:"host_cidr"`   // this host's own address on it, with the prefix
	GatewayIP  string `json:"gateway_ip"`  // the default route's next hop
	GatewayMAC string `json:"gateway_mac"` // the STRONG signal: a specific box on a specific segment
}

// Usable reports whether this fingerprint can pace anything. HostCIDR is the criterion because it
// is exactly what Compare needs before it can say anything but Unknown -- a reading with no
// address is not a description of a network, it is the absence of one.
func (f Fingerprint) Usable() bool { return f.HostCIDR != "" }

// String renders the fingerprint for a human. It exists because the interesting failure is a
// COMPARISON that came out `unknown`, and a line saying only that is a dead end: the reader needs
// to see which side was missing what. Fields that could not be read say so rather than rendering
// as empty, since "absent" is exactly the thing being diagnosed.
func (f Fingerprint) String() string {
	return fmt.Sprintf("%s mac=%s addr=%s gw=%s/%s",
		orNone(f.Parent), orNone(f.ParentMAC), orNone(f.HostCIDR), orNone(f.GatewayIP), orNone(f.GatewayMAC))
}

func orNone(s string) string {
	if s == "" {
		return "?"
	}
	return s
}

// Read takes the fingerprint of dev as it is right now. Best-effort throughout: a field it cannot
// read is empty, and Compare treats an empty field as "no evidence" rather than as a difference.
// That asymmetry is the safe direction -- it can only make the agent MORE cautious, never less.
func Read(ctx context.Context, dev string) Fingerprint {
	f := Fingerprint{Parent: dev}
	if dev == "" {
		return f
	}
	if i, err := net.InterfaceByName(dev); err == nil {
		f.ParentMAC = i.HardwareAddr.String()
		if addrs, err := i.Addrs(); err == nil {
			for _, a := range addrs {
				if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil {
					f.HostCIDR = n.String()
					break
				}
			}
		}
	}
	f.GatewayIP = defaultGateway(dev)
	if f.GatewayIP != "" {
		// The gateway is by definition something this host has been talking to, so its neighbour
		// entry is normally already resolved. ARP it if not, exactly as the VIP check does -- a
		// fingerprint taken at bring-up with an empty gateway MAC is a fingerprint that cannot
		// pace anything later.
		f.GatewayMAC = neighbourMAC(ctx, dev, f.GatewayIP)
	}
	return f
}

// defaultGateway reads the next hop of the main table's default route on dev, from
// /proc/net/route -- the same reader and the same table DefaultRoute uses, for the same reason.
func defaultGateway(dev string) string {
	fh, err := os.Open("/proc/net/route")
	if err != nil {
		return ""
	}
	defer fh.Close()
	sc := bufio.NewScanner(fh)
	sc.Scan() // header
	for sc.Scan() {
		fields := strings.Fields(sc.Text()) // Iface Destination Gateway ...
		if len(fields) < 3 || fields[0] != dev || fields[1] != "00000000" {
			continue
		}
		// Little-endian hex, which is how /proc/net/route has always rendered addresses.
		n, err := strconv.ParseUint(fields[2], 16, 32)
		if err != nil {
			return ""
		}
		var b [4]byte
		binary.LittleEndian.PutUint32(b[:], uint32(n))
		return netip.AddrFrom4(b).String()
	}
	_ = sc.Err()
	return ""
}

// neighbourMAC resolves ip's link address on dev. It provokes the kernel the same way the
// report card's address probe does -- one datagram at the discard port, then read the kernel's
// own neighbour table -- so it needs no raw socket and no capability.
func neighbourMAC(ctx context.Context, dev, ip string) string {
	if mac := arpTableMAC(dev, ip); mac != "" {
		return mac
	}
	if c, err := net.DialTimeout("udp4", net.JoinHostPort(ip, "9"), arpWait); err == nil {
		_, _ = c.Write([]byte{0})
		_ = c.Close()
	}
	for range 10 {
		if mac := arpTableMAC(dev, ip); mac != "" {
			return mac
		}
		select {
		case <-ctx.Done():
			return ""
		case <-timeAfter(arpWait / 10):
		}
	}
	return ""
}

// arpTableMAC reads a RESOLVED entry for ip on dev out of /proc/net/arp. The flags column carries
// ATF_COM (0x2) once an address actually replied; an unanswered probe leaves the row present but
// incomplete, so the flag is the evidence rather than the row.
func arpTableMAC(dev, ip string) string {
	fh, err := os.Open("/proc/net/arp")
	if err != nil {
		return ""
	}
	defer fh.Close()
	sc := bufio.NewScanner(fh)
	sc.Scan() // header
	for sc.Scan() {
		f := strings.Fields(sc.Text()) // IP HWtype Flags HWaddress Mask Device
		if len(f) < 6 || f[0] != ip || f[5] != dev {
			continue
		}
		flags, err := strconv.ParseInt(strings.TrimPrefix(f[2], "0x"), 16, 64)
		if err != nil || flags&0x2 == 0 || f[3] == "00:00:00:00:00:00" {
			return ""
		}
		return f[3]
	}
	return ""
}

// Relation is how a candidate LAN relates to the one recorded -- the thing that paces a
// re-parent. Named for what it says about the WORLD rather than for how long to wait, so the
// durations stay a policy the caller owns.
type Relation string

const (
	// SameWire: the gateway MAC matches. The same router on the same segment, so whatever
	// happened is a change of wire -- a NIC replaced, a cable moved between ports on the same
	// switch. The only relation where a rebuild is unambiguously right.
	SameWire Relation = "same-wire"
	// SameSubnet: the addressing matches but the gateway MAC does not. AMBIGUOUS, in both
	// directions: a replaced router renumbering the same house looks like this, and so does
	// 192.168.1.0/24 at a different house.
	SameSubnet Relation = "same-subnet"
	// Elsewhere: a different subnet. The machine is somewhere else, and nothing about a rebuild
	// here is safe to decide automatically.
	Elsewhere Relation = "elsewhere"
	// Unknown: too little evidence to say. A fingerprint that could not be read -- which must
	// read as the MOST cautious answer, never as "nothing changed".
	Unknown Relation = "unknown"
)

// Compare says how have relates to the recorded want.
//
// ⚠️ MISSING EVIDENCE IS NEVER A MATCH. Every "can we tell?" question is asked before the
// comparison, and an unreadable field sends this to Unknown -- the direction that makes the agent
// wait longer or refuse, never the direction that makes it act.
func Compare(want, have Fingerprint) Relation {
	switch {
	case want.GatewayMAC != "" && have.GatewayMAC != "" && strings.EqualFold(want.GatewayMAC, have.GatewayMAC):
		return SameWire
	case want.HostCIDR == "" || have.HostCIDR == "":
		return Unknown
	case sameSubnet(want.HostCIDR, have.HostCIDR):
		// Reached only when the gateway MACs differ or one is unreadable. Both are the same
		// answer here: ambiguous.
		return SameSubnet
	default:
		return Elsewhere
	}
}

// sameSubnet reports whether two host addresses sit on the same network -- the same prefix AND
// the same masked base. Comparing the base alone would call 10.0.0.5/24 and 10.0.0.5/16 the same
// place, which is a different-sized claim about the same street.
func sameSubnet(a, b string) bool {
	pa, err := netip.ParsePrefix(a)
	if err != nil {
		return false
	}
	pb, err := netip.ParsePrefix(b)
	if err != nil {
		return false
	}
	return pa.Bits() == pb.Bits() && pa.Masked() == pb.Masked()
}
