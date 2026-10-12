package subnet

import (
	"context"
	"net/netip"
	"unsafe"

	"briard.io/agent/reportcard"
	"golang.org/x/sys/windows"
)

// hostRoutes reads the ranges this host routes. Windows has one routing table, so the forward
// table is every route there is -- a VPN client's ranges included.
func hostRoutes(context.Context) []seenPrefix {
	var t *windows.MibIpForwardTable2
	if err := windows.GetIpForwardTable2(windows.AF_INET, &t); err != nil {
		return nil
	}
	defer windows.FreeMibTable(unsafe.Pointer(t))
	var out []seenPrefix
	for _, r := range t.Rows() {
		a := (*windows.RawSockaddrInet4)(unsafe.Pointer(&r.DestinationPrefix.Prefix)).Addr
		p := netip.PrefixFrom(netip.AddrFrom4(a), int(r.DestinationPrefix.PrefixLength)).Masked()
		if worthHonouring(p) {
			out = append(out, seenPrefix{p, "a route on this host"})
		}
	}
	return out
}

// LANProbe asks the household's L2 by ARP, as on Linux, through the report card's probe -- which
// on Windows needs no temporary route (reportcard.AddressAnswers says why). The adapter it asks on
// is the default-route one, and on Windows that IS the selected device, so nic is consulted only
// for "is there one".
//
// A false is "nothing answered", INCLUDING "we could not ask", for Linux's reason.
func LANProbe(_ context.Context, nic string) func(netip.Addr) bool {
	if nic == "" {
		return func(netip.Addr) bool { return false }
	}
	return func(ip netip.Addr) bool { return ip.Is4() && reportcard.AddressAnswers(ip.String()+"/32") }
}
