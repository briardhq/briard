package subnet

import (
	"context"
	"encoding/binary"
	"net/netip"
	"unsafe"

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

var procSendARP = windows.NewLazySystemDLL("iphlpapi.dll").NewProc("SendARP")

// LANProbe asks the household's L2 by ARP, as on Linux. SendARP needs no temporary route: for an
// address with no route of its own it ARPs for the address itself on the interface holding the
// default route (measured on the rig: a candidate it reported unanswered was answered the moment
// a machine on the LAN took it) -- and on Windows that interface IS the selected device, the
// bridge, so nic is not consulted beyond "is there one".
//
// A false is "nothing answered", INCLUDING "we could not ask", for Linux's reason.
func LANProbe(_ context.Context, nic string) func(netip.Addr) bool {
	if nic == "" {
		return func(netip.Addr) bool { return false }
	}
	return func(ip netip.Addr) bool {
		if !ip.Is4() {
			return false
		}
		b := ip.As4()
		var mac [8]byte
		n := uint32(len(mac))
		r, _, _ := procSendARP.Call(uintptr(binary.LittleEndian.Uint32(b[:])), 0,
			uintptr(unsafe.Pointer(&mac[0])), uintptr(unsafe.Pointer(&n)))
		return r == 0 && n > 0
	}
}
