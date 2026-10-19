package nic

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os/exec"
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
	ipBound bool   // IPv4 or IPv6 is bound to it -- false on a bridge member, true on the bridge
	desc    string // the driver's description, e.g. "TAP-Windows Adapter V9 #2"
	filter  bool   // a filter layer over some adapter (WFP, QoS), never an adapter of its own
	leased  bool   // a DHCPv4 server is recorded for it: its address came from a lease
	guid    string // "{...}": the key that survives a rename, ours or the user's
	mac     string // "aabbccddeeff"
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
				desc:    windows.UTF16PtrToString(a.Description),
				ifType:  a.IfType,
				up:      a.OperStatus == windows.IfOperStatusUp,
				ipBound: a.Flags&(adapterIPv4Enabled|adapterIPv6Enabled) != 0,
				leased:  a.Dhcpv4Server.Sockaddr != nil,
				guid:    windows.BytePtrToString(a.AdapterName),
				filter:  isFilter(a.IfIndex),
				mac:     fmt.Sprintf("%x", a.PhysicalAddress[:a.PhysicalAddressLength]),
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

// defaultRoute is one IPv4 default route: the adapter it leaves by, its next hop, and the metric
// Windows actually orders it by.
type defaultRoute struct {
	index  uint32
	hop    netip.Addr
	metric uint32
}

var procGetIpInterfaceEntry = windows.NewLazySystemDLL("iphlpapi.dll").NewProc("GetIpInterfaceEntry")

// bestDefault is the default route Windows would use -- or, with on != 0, the one it would use
// on that adapter. ok is false when there is none.
//
// ⚠️ THE EFFECTIVE METRIC IS ROUTE + INTERFACE. A route row's own Metric is only half of what
// `route print` orders by: Windows adds the adapter's interface metric (automatic from link
// speed, or set by hand), and on a machine with two live uplinks -- a laptop on both its wire
// and its Wi-Fi -- the route metrics tie and the interface metric is the whole decision. Ranking
// by the row alone picks whichever came first.
func bestDefault(on uint32) (defaultRoute, bool) {
	var t *windows.MibIpForwardTable2
	if err := windows.GetIpForwardTable2(windows.AF_INET, &t); err != nil {
		return defaultRoute{}, false
	}
	defer windows.FreeMibTable(unsafe.Pointer(t))
	var best defaultRoute
	found := false
	for _, r := range t.Rows() {
		if r.DestinationPrefix.PrefixLength != 0 || (on != 0 && r.InterfaceIndex != on) {
			continue
		}
		m := r.Metric + interfaceMetric(r.InterfaceIndex)
		if !found || m < best.metric {
			hop := (*windows.RawSockaddrInet4)(unsafe.Pointer(&r.NextHop)).Addr
			best, found = defaultRoute{index: r.InterfaceIndex, hop: netip.AddrFrom4(hop), metric: m}, true
		}
	}
	return best, found
}

// interfaceMetric is the adapter's IPv4 interface metric; 0 when it cannot be read, which leaves
// the route's own metric to rank alone.
func interfaceMetric(index uint32) uint32 {
	row := windows.MibIpInterfaceRow{Family: windows.AF_INET, InterfaceIndex: index}
	if r, _, _ := procGetIpInterfaceEntry.Call(uintptr(unsafe.Pointer(&row))); r != 0 {
		return 0
	}
	return row.Metric
}

// DefaultRoute names the adapter the guest should stand on, from the IPv4 default routes Windows
// holds, ranked by effective metric (route + interface) and carrier (rankDefaults). "" if there is
// none. Windows has one routing table, so the main-table question Select argues for is the only
// one there is to ask.
//
// ⚠️ THE BRIDGE'S CARRIER IS ITS MEMBER'S. Its own media state is the OR with the TAP: connected
// whenever qemu holds it, disconnected before the guest's first start, and evidence of nothing.
// And Windows keeps a disconnected adapter's default route, so a standalone adapter with its
// cable out still holds one; its own media state says so.
//
// THE BRIDGE WINS A TIE, whatever the caller prefers: it is the only parent there ever is here,
// and before the guest holds the TAP a same-tier standalone adapter can tie it exactly.
func DefaultRoute(string) string {
	var t *windows.MibIpForwardTable2
	if err := windows.GetIpForwardTable2(windows.AF_INET, &t); err != nil {
		return ""
	}
	defer windows.FreeMibTable(unsafe.Pointer(t))
	as := adapters()
	var cs []defaultCandidate
	bridged := IsBridge(BridgeName)
	for _, r := range t.Rows() {
		a, ok := byIndex(as, r.InterfaceIndex)
		if r.DestinationPrefix.PrefixLength != 0 || !ok {
			continue
		}
		// A STANDALONE RADIO IS NO CANDIDATE BESIDE THE BRIDGE: the agent does not swap one in.
		// Before the bridge exists (the install's choice) it is one like any other, and so is a
		// bridge over one.
		if bridged && a.name != BridgeName && a.ifType == windows.IF_TYPE_IEEE80211 {
			continue
		}
		carrier := a.up
		if a.name == BridgeName {
			m, _ := MemberSpeed(BridgeName)
			carrier = m != "" && up(m)
		}
		cs = append(cs, defaultCandidate{dev: a.name, metric: uint64(r.Metric + interfaceMetric(r.InterfaceIndex)), carrier: carrier})
	}
	return rankDefaults(cs, BridgeName)
}

// asMember answers a device as the bridge the guest stands on. A standalone adapter is a
// candidate only as the member briard's bridge would swap to -- nothing is built on it at
// runtime -- and a bridge member is its bridge.
func asMember(dev string) (string, string) {
	switch {
	case dev == "" || IsBridge(dev):
		return dev, ""
	case master(dev) != "":
		return master(dev), ""
	case IsBridge(BridgeName):
		return BridgeName, dev
	}
	return dev, ""
}

// Uplink names the device whose presence and carrier stand for parent's: for briard's bridge its
// physical member ("" when only the TAP is left), and true, because the guest moves off it by a
// member swap. Any other parent stands for itself.
func Uplink(parent string) (string, bool) {
	if parent != BridgeName {
		return parent, false
	}
	m, _ := MemberSpeed(parent)
	return m, true
}

// Carrier reports whether dev's media is connected (OperStatus up). A bridge's own status is the
// OR of its members' and the TAP's, so it says nothing about the uplink; the live rule asks it
// of no bridge.
func Carrier(dev string) bool { return up(dev) }

// lowerOf is "" here: nothing on this OS stacks a child on a parent the way a macvtap does --
// the guest's one port is a TAP, a member of the bridge (master).
func lowerOf(string) string { return "" }

// Settling names an adapter the selection should wait for: one with carrier and no default route
// yet -- DHCP still in flight at boot. briard's bridge counts with its member's carrier, and is
// the case this exists for: at a boot the bridge leases seconds after a standalone adapter does
// (measured), and choosing in between swaps away a member that was about to win. Otherwise only a
// hardware adapter outside the bridge counts. "" when there is none.
func Settling() string {
	var t *windows.MibIpForwardTable2
	if err := windows.GetIpForwardTable2(windows.AF_INET, &t); err != nil {
		return ""
	}
	routed := map[uint32]bool{}
	for _, r := range t.Rows() {
		if r.DestinationPrefix.PrefixLength == 0 {
			routed[r.InterfaceIndex] = true
		}
	}
	windows.FreeMibTable(unsafe.Pointer(t))
	for _, a := range adapters() {
		if a.name == "" || a.filter || routed[a.index] {
			continue
		}
		if a.name == BridgeName {
			if m, _ := MemberSpeed(BridgeName); m != "" && up(m) {
				return a.name
			}
			continue
		}
		if a.up && a.ipBound && isHardware(a.index) {
			return a.name
		}
	}
	return ""
}

// ifHardwareInterface is MIB_IF_ROW2's HardwareInterface bit (InterfaceAndOperStatusFlags).
const ifHardwareInterface = 0x01

// isHardware asks IP Helper whether an interface is a physical adapter, not a virtual one (a TAP,
// a bridge, a hypervisor's switch port).
func isHardware(index uint32) bool {
	row := windows.MibIfRow2{InterfaceIndex: index}
	if windows.GetIfEntry2Ex(windows.MibIfEntryNormalWithoutStatistics, &row) != nil {
		return false
	}
	return row.InterfaceAndOperStatusFlags&ifHardwareInterface != 0
}

// defaultGateway is dev's default next hop, from the same table DefaultRoute reads.
func defaultGateway(dev string) string {
	a, ok := byName(adapters(), dev)
	if !ok {
		return ""
	}
	d, ok := bestDefault(a.index)
	if !ok || d.hop.IsUnspecified() {
		return ""
	}
	return d.hop.String()
}

var procSendARP = windows.NewLazySystemDLL("iphlpapi.dll").NewProc("SendARP")

// neighbourMAC resolves ip's link address: SendARP asks the wire and answers with the address,
// so there is no table to provoke and read. The adapter is Windows' to choose (the route to ip),
// which for a gateway is dev. Its wait is the stack's own ARP retry budget, a few seconds at
// most; the context cannot shorten it.
func neighbourMAC(_ context.Context, _ string, ip string) string {
	a, err := netip.ParseAddr(ip)
	if err != nil || !a.Is4() {
		return ""
	}
	b := a.As4()
	var mac [8]byte
	n := uint32(len(mac))
	if r, _, _ := procSendARP.Call(uintptr(binary.LittleEndian.Uint32(b[:])), 0,
		uintptr(unsafe.Pointer(&mac[0])), uintptr(unsafe.Pointer(&n))); r != 0 || n == 0 {
		return ""
	}
	return net.HardwareAddr(mac[:n]).String()
}

// canProbe: nothing the install builds is created at runtime here (substrateBuildable), so there
// is nothing to validate by creating it.
func canProbe() bool { return false }

// The remedy line's copy-pasteable example, in this OS's installer. The elevation relaunch starts
// from a fresh environment, so the setting reaches the agent only from an administrator's prompt.
func overrideExample(dev string) string {
	return "$env:BRIARD_NIC='" + dev + "'; irm https://get.briard.io/install.ps1 | iex   (in a PowerShell opened as administrator)"
}

// placeholderDev is the example device when this machine offers none to name.
const placeholderDev = "Ethernet"

// The macvtap child's flag readers have nothing to read: the substrate they describe is one
// substrateBuildable refuses here, so Converge never reaches them and Converged's check is
// behind the same fork. They answer "satisfied" so a spec that does name one is reported for
// what it is -- unbuildable -- rather than as a device to repair.
func flags(string) int64            { return ifAllmulti }
func ipv6Disabled(string) bool      { return true }
func disableIPv6Path(string) string { return "" }

// The writers. netsh rather than IP Helper's Create* calls: they are rare -- a first convergence,
// or one after something flushed the address -- and netsh is the documented spelling of the one
// setting IP Helper does not expose (dhcpstaticipcoexistence). The reads above stay in IP Helper,
// because the tick asks them.

func netsh(ctx context.Context, args ...string) error {
	out, err := exec.CommandContext(ctx, "netsh", args...).CombinedOutput()
	if err != nil {
		return errors.New(firstLine(out, err))
	}
	return nil
}

// createTap cannot: a TAP adapter on Windows is a device instance of the tap-windows6 driver, laid
// down by the installer with the driver, not something the agent makes at runtime.
func createTap(_ context.Context, dev string) error {
	return fmt.Errorf("no adapter named %q: the TAP adapter comes with the tap-windows6 driver, at install", dev)
}

// joinBridge cannot either: a Windows bridge carries a TAP member only if the bridge was BUILT with
// it, and only after a restart (measured on the rig: a live add joins the bridge and carries
// nothing). The TAP's membership is an install act, never a repair the tick can make; a physical
// member's is not bound that way, and a cold start or the live rule moves it with SwapMember.
func joinBridge(_ context.Context, dev, bridge string) error {
	return fmt.Errorf("%s is not a member of %s: the bridge is built with its TAP at install and "+
		"carries it only after a restart", dev, bridge)
}

func linkUp(ctx context.Context, dev string) error {
	return netsh(ctx, "interface", "set", "interface", "name="+dev, "admin=enabled")
}

// addAddr puts a host address on a DHCP-configured adapter WITHOUT turning its DHCP off, which is
// what a manual address does there unless dhcpstaticipcoexistence is set first (measured: the
// host's own lease was released within a second). The address is active-only -- the agent puts it
// back at every start, as on Linux -- and never a source for the host's other traffic: Windows
// has no per-route source, so a 10.x address an internet-bound connection could pick would be a
// fault on the household's own desktop. What must come from it (a dial to the guest's node IP)
// binds it explicitly.
func addAddr(ctx context.Context, a Addr) error {
	p, err := netip.ParsePrefix(a.CIDR)
	if err != nil {
		return err
	}
	if err := netsh(ctx, "interface", "ipv4", "set", "interface", "interface="+a.Dev, "dhcpstaticipcoexistence=enabled"); err != nil {
		return fmt.Errorf("dhcpstaticipcoexistence on %s: %w", a.Dev, err)
	}
	return netsh(ctx, "interface", "ipv4", "add", "address", "name="+a.Dev, "address="+p.Addr().String(),
		"mask="+maskOf(p.Bits()), "store=active", "skipassource=true")
}

func delAddr(ctx context.Context, a Addr) error {
	p, err := netip.ParsePrefix(a.CIDR)
	if err != nil {
		return err
	}
	return netsh(ctx, "interface", "ipv4", "delete", "address", "name="+a.Dev, "address="+p.Addr().String(), "store=active")
}

// addRoute adds an on-link /32 (next hop 0.0.0.0). r.Src has no Windows spelling (see Route).
func addRoute(ctx context.Context, r Route) error {
	return netsh(ctx, "interface", "ipv4", "add", "route", "prefix="+r.Dst+"/32", "interface="+r.Dev,
		"nexthop=0.0.0.0", "store=active")
}

func delRoute(ctx context.Context, r Route) error {
	return netsh(ctx, "interface", "ipv4", "delete", "route", "prefix="+r.Dst+"/32", "interface="+r.Dev, "store=active")
}

// hasRoute asks the forward table for a /32 to r.Dst on r.Dev.
func hasRoute(r Route) bool {
	dst, err := netip.ParseAddr(r.Dst)
	if err != nil {
		return false
	}
	a, ok := byName(adapters(), r.Dev)
	if !ok {
		return false
	}
	var t *windows.MibIpForwardTable2
	if err := windows.GetIpForwardTable2(windows.AF_INET, &t); err != nil {
		return false
	}
	defer windows.FreeMibTable(unsafe.Pointer(t))
	for _, row := range t.Rows() {
		if row.InterfaceIndex == a.index && row.DestinationPrefix.PrefixLength == 32 &&
			netip.AddrFrom4((*windows.RawSockaddrInet4)(unsafe.Pointer(&row.DestinationPrefix.Prefix)).Addr) == dst {
			return true
		}
	}
	return false
}

// maskOf renders a prefix length as the dotted mask netsh takes.
func maskOf(bits int) string {
	m := net.CIDRMask(bits, 32)
	return net.IP(m).String()
}

// ifFilterInterface is MIB_IF_ROW2's FilterInterface bit (InterfaceAndOperStatusFlags).
const ifFilterInterface = 0x02

// isFilter asks IP Helper whether an interface is a filter layer. Structure cannot answer it: a
// filter sits ON its adapter exactly as a bridge sits on its members, and its description begins
// with its adapter's.
func isFilter(index uint32) bool {
	row := windows.MibIfRow2{InterfaceIndex: index}
	if windows.GetIfEntry2Ex(windows.MibIfEntryNormalWithoutStatistics, &row) != nil {
		return false
	}
	return row.InterfaceAndOperStatusFlags&ifFilterInterface != 0
}

// substrateBuildable refuses everything but a bridge: there are no macvtap or ipvtap children on
// Windows, and the bridge is built at install, so a parent that is not one is a node whose bridge
// is missing -- said plainly, rather than as a macvtap that failed.
func substrateBuildable(s Spec) error {
	if !s.Bridge {
		return fmt.Errorf("%s is not briard's bridge: on Windows the guest's network is %q, built at install and "+
			"in use after a restart -- reinstall to build it again", s.Parent, BridgeName)
	}
	return nil
}

// Leased reports whether dev's address came from a DHCP server.
func Leased(dev string) bool {
	a, ok := byName(adapters(), dev)
	return ok && a.leased
}
