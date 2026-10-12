package nic

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

// The link facts on Windows, from IP Helper. Three things about it decide the shape of this file,
// each measured on the rig rather than assumed:
//
//   - A BRIDGE MEMBER HAS NO IP BINDING, so the default adapter list -- and therefore Go's
//     net.Interfaces and net.InterfaceByName -- does not contain it. Asking the stdlib whether the
//     TAP exists answers "no" about a TAP that is up and carrying the guest. Every lookup here goes
//     through the full list (GAA_FLAG_INCLUDE_ALL_INTERFACES).
//   - THE BRIDGE IS A STACK, NOT A FLAG. GetIfStackTable relates the bridge's interface to its
//     members through intermediate layers -- hidden filter interfaces, and a hidden "Network
//     Bridge" edge -- so membership is a walk through the table, never one row.
//   - "IsBridged" in `netsh bridge show adapter` is a localized string. The stack table is not.
//
// An adapter is found by its FriendlyName, which is what a person sees and what qemu's tap backend
// opens by. Filter layers carry composed names ("TAP0-WFP Native MAC Layer...") that no adapter
// is ever called, so a name lookup never lands on one.

type adapter struct {
	index   uint32
	name    string
	ifType  uint32
	up      bool
	ipBound bool // IPv4 or IPv6 is bound to it -- false on a bridge member, true on the bridge
}

// IP_ADAPTER_ADDRESSES.Flags bits (iptypes.h) that x/sys/windows does not name.
const (
	adapterIPv4Enabled = 0x80
	adapterIPv6Enabled = 0x100
)

// adapters is every interface IP Helper knows of, hidden layers included. nil on failure: every
// caller reads that as "no such device", which is the answer the Linux side gives a missing sysfs
// node.
func adapters() []adapter {
	size := uint32(16 << 10)
	for range 3 {
		buf := make([]byte, size)
		first := (*windows.IpAdapterAddresses)(unsafe.Pointer(&buf[0]))
		err := windows.GetAdaptersAddresses(windows.AF_UNSPEC,
			windows.GAA_FLAG_INCLUDE_ALL_INTERFACES|windows.GAA_FLAG_SKIP_ANYCAST|
				windows.GAA_FLAG_SKIP_MULTICAST|windows.GAA_FLAG_SKIP_DNS_SERVER, 0, first, &size)
		if err == windows.ERROR_BUFFER_OVERFLOW {
			continue
		}
		if err != nil {
			return nil
		}
		var out []adapter
		for a := first; a != nil; a = a.Next {
			out = append(out, adapter{
				index:   a.IfIndex,
				name:    windows.UTF16PtrToString(a.FriendlyName),
				ifType:  a.IfType,
				up:      a.OperStatus == windows.IfOperStatusUp,
				ipBound: a.Flags&(adapterIPv4Enabled|adapterIPv6Enabled) != 0,
			})
		}
		return out
	}
	return nil
}

func byName(as []adapter, dev string) (adapter, bool) {
	for _, a := range as {
		if dev != "" && a.name == dev {
			return a, true
		}
	}
	return adapter{}, false
}

func byIndex(as []adapter, i uint32) (adapter, bool) {
	for _, a := range as {
		if a.index == i {
			return a, true
		}
	}
	return adapter{}, false
}

// stackRow is MIB_IFSTACK_ROW: one higher-layer interface sitting on one lower-layer one.
type stackRow struct{ higher, lower uint32 }

var procGetIfStackTable = windows.NewLazySystemDLL("iphlpapi.dll").NewProc("GetIfStackTable")

// stack is the interface stack table, or nil when it cannot be read.
func stack() []stackRow {
	var tbl unsafe.Pointer
	if r, _, _ := procGetIfStackTable.Call(uintptr(unsafe.Pointer(&tbl))); r != 0 || tbl == nil {
		return nil
	}
	defer windows.FreeMibTable(tbl)
	n := *(*uint32)(tbl) // MIB_IFSTACK_TABLE: NumEntries, then the rows
	rows := unsafe.Slice((*stackRow)(unsafe.Add(tbl, 4)), n)
	return append([]stackRow(nil), rows...)
}

// walk visits every interface reachable from start through the stack table, upward or downward,
// in breadth-first order, until visit returns true. start itself is not visited.
func walk(rows []stackRow, start uint32, upward bool, visit func(uint32) bool) {
	seen := map[uint32]bool{start: true}
	queue := []uint32{start}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, r := range rows {
			from, to := r.lower, r.higher
			if !upward {
				from, to = r.higher, r.lower
			}
			if from != cur || seen[to] {
				continue
			}
			seen[to] = true
			if visit(to) {
				return
			}
			queue = append(queue, to)
		}
	}
}

// Exists reports whether this host has an adapter named dev, up or not, bridged or not.
func Exists(dev string) bool {
	_, ok := byName(adapters(), dev)
	return ok
}

func up(dev string) bool {
	a, ok := byName(adapters(), dev)
	return ok && a.up
}

// IsBridge reports whether dev is built on other interfaces -- anything at all below it in the
// stack. An adapter's own filters sit ABOVE it, so a plain NIC or TAP has nothing below; the bridge
// has its members (through their filters and its own hidden edge).
func IsBridge(dev string) bool {
	a, ok := byName(adapters(), dev)
	if !ok {
		return false
	}
	below := false
	walk(stack(), a.index, false, func(uint32) bool { below = true; return true })
	return below
}

// Wireless reports whether dev is an 802.11 adapter.
func Wireless(dev string) bool {
	a, ok := byName(adapters(), dev)
	return ok && a.ifType == windows.IF_TYPE_IEEE80211
}

// master names the bridge dev is a member of: walking UP its stack, the first adapter with an IP
// binding. A member's own filter layers have none, and neither does the bridge's hidden edge; the
// bridge itself is where IP is bound. "" when dev is a member of nothing.
func master(dev string) string {
	as := adapters()
	a, ok := byName(as, dev)
	if !ok || a.ipBound {
		return ""
	}
	name := ""
	walk(stack(), a.index, true, func(i uint32) bool {
		if m, ok := byIndex(as, i); ok && m.ipBound && m.name != "" {
			name = m.name
		}
		return name != ""
	})
	return name
}

// DefaultRoute names the adapter holding the IPv4 default route with the lowest route metric. ""
// if there is none. Windows has one routing table, so the main-table question Choose argues for
// is the only one there is to ask.
func DefaultRoute() string {
	var t *windows.MibIpForwardTable2
	if err := windows.GetIpForwardTable2(windows.AF_INET, &t); err != nil {
		return ""
	}
	defer windows.FreeMibTable(unsafe.Pointer(t))
	best, found := uint32(0), false
	var metric uint32
	for _, r := range t.Rows() {
		if r.DestinationPrefix.PrefixLength != 0 {
			continue
		}
		if !found || r.Metric < metric {
			best, metric, found = r.InterfaceIndex, r.Metric, true
		}
	}
	if !found {
		return ""
	}
	a, _ := byIndex(adapters(), best)
	return a.name
}
