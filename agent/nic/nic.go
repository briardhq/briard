// Package nic answers one question: which of this host's devices does the guest's L2 hang off?
//
// It is a leaf — no briard imports — because both ends of the install need it. The report card
// asks before anything is written, so a host that cannot carry a guest is refused rather than
// half-installed; the agent asks again at bring-up, because the answer is a property
// of the machine on the day it is asked and machines change.
//
// ONE SELECTOR, ONE PROBE, AND A MESSAGE. The selector is the default route and nothing else. The
// probe does not select — it validates the selection by creating the very thing the install will
// create. And the message is the whole safety margin between the two, because every way this can
// be wrong ends with a guest that boots and is unreachable, which is the failure a user cannot
// diagnose from inside the house.
package nic

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"strconv"
	"strings"
)

// The two ways there is no answer, separated because they want opposite handling from a caller
// that can wait. A device with no default route YET is a timing question — the cable is in, NM has
// not finished, DHCP is mid-flight — and the answer is to wait. A host with no default route AT
// ALL is a configuration question, and waiting for it is waiting forever.
//
// ⚠️ NOTHING HERE TELLS THEM APART, deliberately: they are the same reading. Only elapsed time
// separates them, so the distinction belongs to the caller — the report card is a one-shot and
// refuses, the agent waits and then reports itself degraded. A selector that "helpfully" fell back
// to a second heuristic when the first came up empty would answer the timing case with a wrong
// device and never notice.
var (
	ErrNoDefaultRoute = errors.New("no default route")
	ErrNoInterfaces   = errors.New("no network interface")
	ErrNoSuchDevice   = errors.New("no such device")
)

// probeDev is the throwaway child. A fixed name rather than a random one so a leaked device
// (killed mid-probe) is recognisable and reclaimable, and one that cannot collide with the two
// the install really creates.
const probeDev = "briard-probe0"

// Selection is what this host answered. It is plain data: the report card's Assess is pure, and
// the agent's degraded status reports it, so both read the same fields rather than re-deriving.
type Selection struct {
	// Dev is the device the guest's L2 hangs off — "" when none could be chosen.
	Dev string
	// Override is true when Dev came from BRIARD_NIC rather than from the default route. It
	// changes the message and nothing else: a user who named a device does not need to be told
	// how we would have guessed.
	Override bool
	// Bridge and Wireless are the SUBSTRATE FORK, three ways: a bridge gets one port and the
	// guest makes its own service identity inside; a wireless station gets ipvtap children, on
	// its own MAC, the one an access point carries for it (ipvtap.go); anything else gets
	// macvtap children. Asked of the device rather than selected by a knob, because there is one
	// true answer and the machine holds it. A managed station cannot be a bridge member, so the
	// two never overlap.
	//
	// Wired stays macvtap even though ipvtap would carry it: failover moves a MAC, and an ipvtap
	// child cannot hold one of its own. That is also why a wireless node is yellow, not green.
	Bridge   bool
	Wireless bool
	// SwapIn is the adapter to swap into the bridge Dev names before the guest stands on it: the
	// standalone adapter that outranks the bridge's own member, or the one BRIARD_NIC names. ""
	// when the bridge's member is the answer, and always on Linux, whose bridges are the user's.
	SwapIn string
	// Err is why Dev is unusable: one of the sentinels above, or a wrapped probe failure.
	Err error
	// Candidates are this host's non-loopback devices, for the message. The user needs the list
	// to act on the fix, and a machine we refused is the worst place to make them go find it.
	Candidates []string
}

// Choose makes the selection and VALIDATES it — for a caller that is about to create devices on
// the answer. override is BRIARD_NIC — kept under that name (widened: it may now
// name a bridge) because renaming it would cost a sweep of every rig that sets it and buy nothing.
// prefer is the parent last served on, which wins a tie (DefaultRoute); "" for a caller with none.
func Choose(ctx context.Context, override, prefer string) Selection {
	s := Select(override, prefer)
	// THE PROBE VALIDATES, IT DOES NOT SELECT. There is no second heuristic above it: no
	// gateway-ARP confirmation, no scope-global fallback, no virtual/non-virtual filter — a bond
	// and a VLAN are both "virtual" and both fine, and no sysfs attribute separates a tun device
	// from them the way creating a macvtap on it does. docker0 and tailscale0 fall out of the
	// selection because they hold no default route, never by a name blacklist.
	//
	// Whether it can run is the OS's answer (canProbe): on Linux only as root -- `ip link add`
	// fails with EPERM for a reason that says nothing about the host, and a curious user running
	// `briard-agent --report-card` without sudo must not be told their machine is unfit; on
	// Windows never, since nothing the install builds is created at runtime there.
	if s.Err == nil && canProbe() {
		s.Err = Probe(ctx, s.Dev)
	}
	return s
}

// Select names the device without validating it — for a caller that is NOT about to create
// anything on the answer, and must not pay for a probe to find out where its existing devices
// hang. The agent's live rule is exactly that: it asks every tick whether a gone parent has a
// successor, and probing every ten seconds would be a netlink write to answer a question nobody
// asked -- the move that follows builds on the answer, and that build is the validation.
func Select(override, prefer string) Selection {
	s := Selection{Candidates: Candidates()}
	switch {
	case override != "":
		s.Dev, s.Override = override, true
		if !Exists(override) {
			s.Err = fmt.Errorf("%w: %s", ErrNoSuchDevice, override)
			return s
		}
	default:
		// THE DEFAULT ROUTE, read from the main table — and read that way rather than with
		// `ip route get`, which answers a different question. We want the household's L2, not the
		// path to the internet, and on a host running a VPN those differ: WireGuard/wg-quick and
		// Tailscale exit nodes put their default in a table of their own behind an ip rule, and
		// OpenVPN's `redirect-gateway def1` installs a 0.0.0.0/1 + 128.0.0.0/1 pair rather than a
		// default at all. `ip route get` follows every one of them and names a tunnel that cannot
		// carry a macvtap; the main table's default keeps naming the wire. ⚠️ The naive read is
		// the correct one here — say so, or the next reader "fixes" it.
		if s.Dev = DefaultRoute(prefer); s.Dev == "" {
			if len(s.Candidates) == 0 {
				s.Err = ErrNoInterfaces
			} else {
				s.Err = ErrNoDefaultRoute
			}
			return s
		}
	}
	// A device the guest moves to by joining it to a standing bridge (Windows) is answered as that
	// bridge, with the device as SwapIn.
	s.Dev, s.SwapIn = asMember(s.Dev)
	s.Wireless, s.Bridge = Wireless(s.Dev), IsBridge(s.Dev)
	return s
}

// Up reports whether dev exists and is up: IFF_UP on Linux, OperStatus on Windows.
func Up(dev string) bool { return Exists(dev) && up(dev) }

// Built reports whether dev stands as this node's substrate made it -- the question the agent's
// hot path asks of the devices its config names, before it asks anything about a parent. It is
// asked of the device's construction and never of its media: on Windows the guest's port carries
// the household NIC's carrier (the agent mirrors it there), so reading its OperStatus would send
// an agent restarting under its guest with the cable out down the cold path.
func Built(dev string) bool { return Exists(dev) && built(dev) }

// ParentOf names the device dev hangs off as it stands: the bridge it is a port of, or the device
// a macvtap child was created on. "" for a device that hangs off nothing. It is the agent's
// answer under a standing guest, which keeps the parent it has rather than re-selecting.
func ParentOf(dev string) string {
	if m := master(dev); m != "" {
		return m
	}
	return lowerOf(dev)
}

// Usable reports whether the agent may build on this selection. Wireless is not a fault: it is a
// substrate, and what it costs (no failover) is the report card's to grade.
func (s Selection) Usable() bool { return s.Dev != "" && s.Err == nil }

// Fix is the remedy line, and it is the whole safety margin: what was picked, why, what failed,
// the override, and the devices to choose from. It is rendered here rather than at each caller
// so the refusal a user meets before the install and the degraded status they meet after it say
// the same thing. No copy-pasteable example: a device picked for the example is a device this
// selection could not vouch for (a tunnel holding a default route was offered once), and who
// sets BRIARD_NIC knows how.
func (s Selection) Fix() string {
	var b strings.Builder
	switch {
	case errors.Is(s.Err, ErrNoInterfaces):
		return "connect this machine to your network (wired ethernet recommended); it has no network device but loopback"
	case errors.Is(s.Err, ErrNoDefaultRoute):
		b.WriteString("this machine has no default route, so there is no way to tell which device is on your home network")
	case errors.Is(s.Err, ErrNoSuchDevice):
		fmt.Fprintf(&b, "BRIARD_NIC names %s, which this machine does not have", s.Dev)
	case s.Err != nil && s.Override:
		fmt.Fprintf(&b, "%s cannot carry the guest's network: %v", s.Dev, s.Err)
	case s.Err != nil:
		fmt.Fprintf(&b, "%s was chosen because it holds this machine's default route, but it cannot carry the guest's network: %v", s.Dev, s.Err)
	default:
		return ""
	}
	b.WriteString(". Name the device yourself with BRIARD_NIC")
	if len(s.Candidates) > 0 {
		fmt.Fprintf(&b, " -- this machine has: %s", strings.Join(s.Candidates, ", "))
	}
	return b.String()
}

// Probe validates dev by creating the very thing the install creates — a macvtap child, or an
// ipvtap child on a wireless station — and deleting it. It turns a wrong selection into a clean
// refusal instead of a guest that boots and is unreachable, which is the one failure mode a
// household cannot diagnose.
//
// What it catches is the device that cannot parent one at all — a full-tunnel tun0 above all,
// which a laptop on a corporate VPN hands us as its default route. It cannot see whether an
// access point carries the result; the substrate is chosen so that it does.
func Probe(ctx context.Context, dev string) error {
	// A leaked probe device from a run killed between create and delete would make every later
	// probe fail with EEXIST -- i.e. would condemn a perfectly good NIC. Clear it first.
	_, _ = ip(ctx, "link", "del", probeDev)
	// THE SAME ARGV THE INSTALL USES, from the same builders -- that identity is the whole claim
	// this probe makes, so it is shared rather than restated.
	kind, args := "macvtap", macvtapAddArgs(probeDev, dev)
	if Wireless(dev) {
		kind, args = "ipvtap", ipvtapAddArgs(probeDev, dev)
	}
	out, err := ip(ctx, args...)
	if err != nil {
		return fmt.Errorf("a %s could not be created on it (%s)", kind, firstLine(out, err))
	}
	_, _ = ip(ctx, "link", "del", probeDev)
	return nil
}

// firstLine is the one line of `ip`'s complaint worth showing a user ("Error: argument
// \"tun0\" is wrong: Device does not support macvlan"), falling back to the exec error when the
// command said nothing at all.
func firstLine(out []byte, err error) string {
	for l := range strings.SplitSeq(string(out), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			return strings.TrimPrefix(l, "Error: ")
		}
	}
	return err.Error()
}

// THE PARSERS ARE SHARED, THE READERS ARE NOT. What follows here is pure text handling for the
// Linux arm's procfs tables (link_linux.go reads them), kept beside the other pure renderers so the
// unit tests reach it from any OS; nothing below touches the machine.

// procTable splits a /proc/net table into its rows' fields, dropping the header line every one of
// them carries. The header is the only thing these two files have in common, and skipping it is
// the one step each parser would otherwise repeat.
func procTable(text string) [][]string {
	lines := strings.Split(text, "\n")
	if len(lines) < 2 {
		return nil
	}
	var rows [][]string
	for _, l := range lines[1:] { // [0] is the header
		if f := strings.Fields(l); len(f) > 0 {
			rows = append(rows, f)
		}
	}
	return rows
}

// defaultCandidate is one IPv4 default route as the selector weighs it: the device it leaves by,
// its effective metric, and whether the link it stands for has carrier.
type defaultCandidate struct {
	dev     string
	metric  uint64
	carrier bool
}

// rankDefaults is the selection rule, one for both OSes; each OS's reader only builds the
// candidates. Among those with carrier the lowest metric wins, and prefer wins a tie.
//
// ⚠️ THE CARRIER IS READ, NOT INFERRED FROM THE ROUTE. systemd-networkd keeps a dead wire's
// default route and Windows keeps a disconnected adapter's, so a route alone does not say the
// cable is in. A dead wire is not a candidate, so it never outranks a live radio, and nobody
// guesses how long it stays dead.
//
// THE METRIC IS THE WHOLE RANKING. The OS already encodes wire-over-radio in it; a preference of
// our own by kind, name or speed would be a second opinion that can disagree with it.
//
// prefer is the parent last served on. A tie is not a better answer than the one standing, so it
// never moves the guest; with no prefer in the tie the first candidate wins, the OS's own order.
func rankDefaults(cs []defaultCandidate, prefer string) string {
	best, bestMetric := "", uint64(0)
	for _, c := range cs {
		if !c.carrier {
			continue
		}
		if best == "" || c.metric < bestMetric || (c.metric == bestMetric && c.dev == prefer && best != prefer) {
			best, bestMetric = c.dev, c.metric
		}
	}
	return best
}

// parseDefaultRouteDev picks the guest's parent out of /proc/net/route's text: the main table's
// default routes (destination AND mask 00000000), ranked by their Metric column and carrier
// (rankDefaults). Pure, so the format handling is unit-tested against real output rather than
// trusted. The mask is checked because OpenVPN's `redirect-gateway def1` adds a 0.0.0.0/1, whose
// destination column also reads 00000000.
func parseDefaultRouteDev(text string, carrier func(string) bool, prefer string) string {
	var cs []defaultCandidate
	for _, f := range procTable(text) { // Iface Destination Gateway Flags RefCnt Use Metric Mask ...
		if len(f) < 8 || f[1] != "00000000" || f[7] != "00000000" {
			continue
		}
		m, err := strconv.ParseUint(f[6], 10, 32)
		if err != nil {
			continue
		}
		cs = append(cs, defaultCandidate{dev: f[0], metric: m, carrier: carrier(f[0])})
	}
	return rankDefaults(cs, prefer)
}

// parseSettling names a device that has carrier and no default route yet, among devs, from
// /proc/net/route's text -- "" when there is none. Pure, beside the selector it holds back.
func parseSettling(text string, devs []string, carrier func(string) bool) string {
	routed := map[string]bool{}
	for _, f := range procTable(text) {
		if len(f) >= 8 && f[1] == "00000000" && f[7] == "00000000" {
			routed[f[0]] = true
		}
	}
	for _, d := range devs {
		if !routed[d] && carrier(d) {
			return d
		}
	}
	return ""
}

// Candidates lists this host's non-loopback devices, in kernel order. For the MESSAGE only --
// nothing selects from this list, because a list is not evidence about which device is on the
// household's LAN and the default route is.
func Candidates() []string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []string
	for _, i := range ifaces {
		if i.Flags&net.FlagLoopback == 0 {
			out = append(out, i.Name)
		}
	}
	return out
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// parseHostRoute reports whether /proc/net/route's text holds a /32 to dst on dev. The table
// prints addresses as the kernel stores them, little-endian hex, so 10.0.0.1 is 0100000A. Pure, so
// that detail is unit-tested rather than trusted.
func parseHostRoute(text, dev, dst string) bool {
	a, err := netip.ParseAddr(dst)
	if err != nil || !a.Is4() {
		return false
	}
	b := a.As4()
	want := fmt.Sprintf("%02X%02X%02X%02X", b[3], b[2], b[1], b[0])
	for _, f := range procTable(text) { // Iface Destination Gateway Flags RefCnt Use Metric Mask ...
		if len(f) >= 8 && f[0] == dev && strings.EqualFold(f[1], want) && strings.EqualFold(f[7], "FFFFFFFF") {
			return true
		}
	}
	return false
}

// Offload is what building the Windows bridge changed on the household's own NIC, recorded BEFORE
// the change so that removing briard can put back what was there: keyed by the adapter's GUID (a
// rename, ours or the user's, moves the name; the GUID stays) and stored as the registry value,
// never the display string, which is localized. Declared for every platform so the host's record
// of it reads and writes, and is tested, anywhere.
type Offload struct {
	GUID    string `json:"guid"`
	Keyword string `json:"keyword"`
	Value   string `json:"value"`
	// RSC4 and RSC6 are the member's receive segment coalescing values (*RscIPv4, *RscIPv6),
	// recorded and turned off beside the checksum offload; "" where the driver offers none.
	RSC4 string `json:"rsc4,omitempty"`
	RSC6 string `json:"rsc6,omitempty"`
}

// settings are what the record holds, by registry keyword, in the order they are read, turned
// off and put back; a "" value is a driver without the setting and is skipped.
func (o Offload) settings() [3][2]string {
	return [3][2]string{{o.Keyword, o.Value}, {rscOffloadIPv4, o.RSC4}, {rscOffloadIPv6, o.RSC6}}
}

// The receive segment coalescing settings the bridge needs off on the member too. A member that
// coalesces received segments hands the bridge frames larger than the MTU, and the bridge drops
// them -- measured on a virtio NIC, whose hypervisor coalesces on the member's behalf: every
// download crawled at tens of kilobytes a second until RSC was off. Windows already reports RSC as
// not operational on a bridge member, so turning it off costs nothing on any NIC; what it buys is a
// driver that stops asking its hardware, or its hypervisor, for coalesced frames.
const (
	rscOffloadIPv4 = "*RscIPv4"
	rscOffloadIPv6 = "*RscIPv6"
)
