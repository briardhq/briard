package nic

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
)

// THE LAN FINGERPRINT, and what it is FOR.
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
	}
	f.GatewayIP = defaultGateway(dev)
	f.HostCIDR = hostCIDR(dev, f.GatewayIP)
	if f.GatewayIP != "" {
		// The gateway is by definition something this host has been talking to, so its neighbour
		// entry is normally already resolved. ARP it if not, exactly as the VIP check does -- a
		// fingerprint taken at bring-up with an empty gateway MAC is a fingerprint that cannot
		// pace anything later.
		f.GatewayMAC = neighbourMAC(ctx, dev, f.GatewayIP)
	}
	return f
}

// The two readers Read needs -- defaultGateway (dev's default next hop, from the same table
// DefaultRoute uses) and neighbourMAC (that hop's link address, provoked and read without a raw
// socket) -- are the per-OS half, in link_linux.go / link_windows.go. The parsers stay here.

// HostCIDR is dev's own LAN address with its prefix ("192.168.9.100/24"): the LAN this node is on,
// which is what the VIP has to be inside and what the fingerprint compares. "" when dev has none,
// which every reader takes as "unknown" rather than as a different network.
//
// ⚠️ THE ADDRESS ON THE GATEWAY'S SUBNET, never the first one listed. On a bridge the host's LAN
// address sits beside the agent's own system-subnet /32, the one it puts there for the guest, and
// which of the two the OS lists first is its choice, not ours. Without a default route, the first
// address that is not link-local: a bridge mid-DHCP on Windows holds an APIPA placeholder, and a
// placeholder is not a LAN.
func HostCIDR(dev string) string {
	if dev == "" {
		return ""
	}
	return hostCIDR(dev, defaultGateway(dev))
}

func hostCIDR(dev, gateway string) string { return pickHostCIDR(prefixes4(dev), gateway) }

// LANAddr is dev's address on its default gateway's subnet and nothing else ("" when dev has no
// default route, or no address inside it): HostCIDR's first rule without its fallback, for a
// reader that must not act from any other address -- a DHCP request sent on the flock's behalf,
// whose reply the stack hands to the socket bound to the address it arrived for.
func LANAddr(dev string) string {
	if dev == "" {
		return ""
	}
	p := onGatewaySubnet(prefixes4(dev), defaultGateway(dev))
	if p == "" {
		return ""
	}
	return netip.MustParsePrefix(p).Addr().String()
}

// prefixes4 is dev's IPv4 addresses with their prefixes, in the order the OS lists them.
func prefixes4(dev string) []netip.Prefix {
	i, err := net.InterfaceByName(dev)
	if err != nil {
		return nil
	}
	addrs, err := i.Addrs()
	if err != nil {
		return nil
	}
	var ps []netip.Prefix
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil {
			if p, err := netip.ParsePrefix(n.String()); err == nil {
				ps = append(ps, p)
			}
		}
	}
	return ps
}

// onGatewaySubnet is the prefix among ps that holds gateway, or "".
func onGatewaySubnet(ps []netip.Prefix, gateway string) string {
	if gw, err := netip.ParseAddr(gateway); err == nil {
		for _, p := range ps {
			if p.Contains(gw) {
				return p.String()
			}
		}
	}
	return ""
}

// pickHostCIDR is HostCIDR's choice among dev's IPv4 prefixes, pure so it is unit-tested.
func pickHostCIDR(ps []netip.Prefix, gateway string) string {
	if p := onGatewaySubnet(ps, gateway); p != "" {
		return p
	}
	for _, p := range ps {
		if !p.Addr().IsLinkLocalUnicast() {
			return p.String()
		}
	}
	return ""
}

// parseDefaultGateway pulls dev's default next hop out of /proc/net/route's text. Pure, so the
// one detail that cannot be inferred by reading it -- the LITTLE-ENDIAN hex the address column is
// rendered in -- is unit-tested rather than trusted.
func parseDefaultGateway(text, dev string) string {
	for _, f := range procTable(text) { // Iface Destination Gateway ...
		if len(f) < 3 || f[0] != dev || f[1] != "00000000" {
			continue
		}
		// Little-endian hex, which is how /proc/net/route has always rendered addresses.
		n, err := strconv.ParseUint(f[2], 16, 32)
		if err != nil {
			return ""
		}
		var b [4]byte
		binary.LittleEndian.PutUint32(b[:], uint32(n))
		return netip.AddrFrom4(b).String()
	}
	return ""
}

// parseARPMAC pulls a resolved link address for ip on dev out of /proc/net/arp's text. Pure, so
// the detail that cannot be inferred by reading it is unit-tested rather than trusted:
// the flags column carries ATF_COM (0x2) once an address actually replied, and an unanswered probe
// leaves the row PRESENT but incomplete -- so the flag is the evidence, never the row.
func parseARPMAC(text, dev, ip string) string {
	for _, f := range procTable(text) { // IP HWtype Flags HWaddress Mask Device
		if len(f) < 6 || f[0] != ip || f[5] != dev {
			continue
		}
		flags, err := strconv.ParseInt(strings.TrimPrefix(f[2], "0x"), 16, 64)
		if err != nil || flags&atfCom == 0 || f[3] == "00:00:00:00:00:00" {
			return ""
		}
		return f[3]
	}
	return ""
}

// atfCom is ATF_COM in /proc/net/arp's flags column: the entry is COMPLETE, i.e. something at
// that address answered.
const atfCom = 0x2

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
