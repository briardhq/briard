// Package nic answers one question: which of this host's devices does the guest's L2 hang off?
//
// It is a leaf — no briard imports — because both ends of the install need it. The report card
// asks before anything is written, so a host that cannot carry a guest is refused rather than
// half-installed ([B.150](b)); the agent asks again at bring-up, because the answer is a property
// of the machine on the day it is asked and machines change ([B.150](d)).
//
// ONE SELECTOR, ONE PROBE, AND A MESSAGE. The selector is the default route and nothing else. The
// probe does not select — it validates the selection by creating the very thing the install will
// create. And the message is the whole safety margin between the two, because every way this can
// be wrong ends with a guest that boots and is unreachable, which is the failure a user cannot
// diagnose from inside the house.
package nic

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
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

// probeDev is the throwaway macvtap. A fixed name rather than a random one so a leaked device
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
	// Wireless is whether Dev is an 802.11 station. Kept separate from the probe on purpose —
	// the probe SUCCEEDS on wireless (the kernel makes the macvtap happily), and the frames die
	// later at the AP, which is exactly the class of failure the probe cannot see.
	Wireless bool
	// Probed is whether the macvtap probe actually ran. False on an unprivileged card run, where
	// `ip link add` fails for a reason that says nothing about the host.
	Probed bool
	// Err is why Dev is unusable: one of the sentinels above, or a wrapped probe failure.
	Err error
	// Candidates are this host's non-loopback devices, for the message. The user needs the list
	// to act on the fix, and a machine we refused is the worst place to make them go find it.
	Candidates []string
}

// Choose makes the selection and validates it. override is BRIARD_NIC — kept under that name
// (widened: since [B.150](c) it may name a bridge) because renaming it would cost a sweep of
// every rig that sets it and buy nothing.
func Choose(ctx context.Context, override string) Selection {
	s := Selection{Candidates: Candidates()}
	switch {
	case override != "":
		s.Dev, s.Override = override, true
		if !exists("/sys/class/net/" + override) {
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
		if s.Dev = DefaultRoute(); s.Dev == "" {
			if len(s.Candidates) == 0 {
				s.Err = ErrNoInterfaces
			} else {
				s.Err = ErrNoDefaultRoute
			}
			return s
		}
	}
	s.Wireless = Wireless(s.Dev)
	// THE PROBE VALIDATES, IT DOES NOT SELECT. There is no second heuristic above it: no
	// gateway-ARP confirmation, no scope-global fallback, no virtual/non-virtual filter — a bond
	// and a VLAN are both "virtual" and both fine, and no sysfs attribute separates a tun device
	// from them the way creating a macvtap on it does. docker0 and tailscale0 fall out of the
	// selection because they hold no default route, never by a name blacklist.
	if os.Geteuid() == 0 {
		s.Probed = true
		s.Err = Probe(ctx, s.Dev)
	}
	return s
}

// Usable reports whether the install may proceed on this selection. Wireless is not a fault here:
// its severity is the report card's call, not the selector's.
func (s Selection) Usable() bool { return s.Dev != "" && s.Err == nil }

// Fix is the remedy line, and it is the whole safety margin: what was picked, why, what failed,
// the override with a copy-pasteable example, and the devices to choose from. It is rendered here
// rather than at each caller so the refusal a user meets before the install and the degraded
// status they meet after it say the same thing.
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
	case s.Wireless:
		fmt.Fprintf(&b, "%s is wireless: the guest gets its own MAC on your network, and no household access point carries a second MAC behind one wireless station", s.Dev)
	default:
		return ""
	}
	b.WriteString(". Name the device yourself with BRIARD_NIC, e.g. `BRIARD_NIC=")
	b.WriteString(exampleDev(s.Candidates))
	b.WriteString(" curl -fsSL https://get.briard.io/install.sh | sudo sh`")
	if len(s.Candidates) > 0 {
		fmt.Fprintf(&b, " -- this machine has: %s", strings.Join(s.Candidates, ", "))
	}
	return b.String()
}

// exampleDev makes the example copy-pasteable rather than notional: a name off THIS machine when
// there is one, and a placeholder the user can see is a placeholder when there is not.
func exampleDev(candidates []string) string {
	if len(candidates) > 0 {
		return candidates[0]
	}
	return "eth0"
}

// Probe validates dev by creating the very thing the install creates — a macvtap child — and
// deleting it. It turns a wrong selection into a clean refusal instead of a guest that boots and
// is unreachable, which is the one failure mode a household cannot diagnose.
//
// It is NOT a wireless test: a macvtap on wlan0 is created without complaint, and the frames die
// at the access point. What it catches is the device that cannot parent one at all — a full-tunnel
// tun0 above all, which a laptop on a corporate VPN hands us as its default route.
func Probe(ctx context.Context, dev string) error {
	// A leaked probe device from a run killed between create and delete would make every later
	// probe fail with EEXIST -- i.e. would condemn a perfectly good NIC. Clear it first.
	_ = exec.CommandContext(ctx, "ip", "link", "del", probeDev).Run()
	out, err := exec.CommandContext(ctx, "ip", "link", "add", "link", dev, "name", probeDev, "type", "macvtap", "mode", "bridge").CombinedOutput()
	if err != nil {
		return fmt.Errorf("a macvtap could not be created on it (%s)", firstLine(out, err))
	}
	_ = exec.CommandContext(ctx, "ip", "link", "del", probeDev).Run()
	return nil
}

// firstLine is the one line of `ip`'s complaint worth showing a user ("Error: argument
// \"tun0\" is wrong: Device does not support macvlan"), falling back to the exec error when the
// command said nothing at all.
func firstLine(out []byte, err error) string {
	for _, l := range strings.Split(string(out), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			return strings.TrimPrefix(l, "Error: ")
		}
	}
	return err.Error()
}

// DefaultRoute reads the interface owning the default route (destination 00000000) from
// /proc/net/route -- the MAIN table, which is the point (see Choose). "" if none.
func DefaultRoute() string {
	f, err := os.Open("/proc/net/route")
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Scan() // header
	for sc.Scan() {
		fields := strings.Fields(sc.Text()) // Iface Destination Gateway Flags ...
		if len(fields) >= 2 && fields[1] == "00000000" {
			return fields[0]
		}
	}
	_ = sc.Err()
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

// Wireless reports whether dev is an 802.11 station. Both spellings: `wireless` is the old
// extension, `phy80211` the cfg80211 one, and drivers differ on which they publish.
func Wireless(dev string) bool {
	base := "/sys/class/net/" + dev
	return exists(base+"/wireless") || exists(base+"/phy80211")
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
