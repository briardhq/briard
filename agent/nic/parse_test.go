package nic

import (
	"slices"
	"testing"
)

// THE FORMAT HANDLING, AGAINST REAL OUTPUT.
//
// Every fixture below is a verbatim capture from a running kernel (brie, 2026-09-21) rather than
// something written to match the parser -- which is the point of the split these tests exist to
// pay for. The readers above them do one os.ReadFile each and nothing else, so what is left to be
// wrong is here, where a test can reach it: the hex encodings, the completeness flag, and the
// depth of a symlink.

// procNetRoute as the kernel renders it: the machine's default is on eno1, and three other
// devices hold connected routes. Note the trailing whitespace the kernel pads each row with.
const procNetRoute = `Iface	Destination	Gateway 	Flags	RefCnt	Use	Metric	Mask		MTU	Window	IRTT
eno1	00000000	0154C558	0003	0	0	1024	00000000	0	0	0
incusbr0	0000990A	00000000	0001	0	0	0	00FFFFFF	0	0	0
eno1	0054C558	00000000	0001	0	0	1024	00FCFFFF	0	0	0
eno1	0154C558	00000000	0005	0	0	1024	FFFFFFFF	0	0	0
docker0	000011AC	00000000	0001	0	0	0	0000FFFF	0	0	0
eno2	0008A8C0	00000000	0001	0	0	0	00FFFFFF	0	0	0
`

// routeHeader and defaultRow build the two-uplink tables: a laptop on its wire and its radio, as
// NetworkManager numbers them (100 wired, 600 wireless), and the same with the metrics tied.
const routeHeader = "Iface\tDestination\tGateway \tFlags\tRefCnt\tUse\tMetric\tMask\t\tMTU\tWindow\tIRTT\n"

func defaultRow(dev, metric string) string {
	return dev + "\t00000000\t0108A8C0\t0003\t0\t0\t" + metric + "\t00000000\t0\t0\t0\n"
}

func TestParseDefaultRouteDev(t *testing.T) {
	all := func(string) bool { return true }
	wired := routeHeader + defaultRow("wlp2s0", "600") + defaultRow("enp3s0", "100") +
		"enp3s0\t0008A8C0\t00000000\t0001\t0\t0\t100\t00FFFFFF\t0\t0\t0\n"
	tied := routeHeader + defaultRow("wlp2s0", "100") + defaultRow("enp3s0", "100")
	for _, tc := range []struct {
		name, text string
		carrier    func(string) bool
		prefer     string
		want       string
	}{
		{"the default route's device", procNetRoute, all, "", "eno1"},
		// The selector's ErrNoDefaultRoute case: devices exist, none holds a default. This is the
		// reading that must NOT fall through to a second heuristic (see Select).
		{"no default route at all", "Iface\tDestination\tGateway\n" +
			"eno2\t0008A8C0\t00000000\t0001\t0\t0\t0\t00FFFFFF\t0\t0\t0\n", all, "", ""},
		{"header only", "Iface\tDestination\tGateway\n", all, "", ""},
		{"nothing at all", "", all, "", ""},
		// THE METRIC, NOT THE ROW ORDER: the radio's row is first and loses.
		{"two defaults, the lower metric wins", wired, all, "", "enp3s0"},
		{"two defaults, the standing parent does not outrank a lower metric", wired, all, "wlp2s0", "enp3s0"},
		// THE CARRIER IS READ: networkd keeps a dead wire's route, and a dead wire is no candidate.
		{"the lower metric has no carrier", wired, func(d string) bool { return d != "enp3s0" }, "enp3s0", "wlp2s0"},
		{"one default, without carrier", routeHeader + defaultRow("enp3s0", "100"),
			func(string) bool { return false }, "enp3s0", ""},
		// A TIE IS NOT A BETTER ANSWER: the parent last served on keeps it, whichever row is first.
		{"a tie, the standing parent second", tied, all, "enp3s0", "enp3s0"},
		{"a tie, the standing parent first", tied, all, "wlp2s0", "wlp2s0"},
		{"a tie, no parent standing: the first row", tied, all, "", "wlp2s0"},
		{"a tie, the standing parent without carrier", tied, func(d string) bool { return d != "enp3s0" }, "enp3s0", "wlp2s0"},
		// OpenVPN's def1 pair: 0.0.0.0/1 reads destination 00000000 too, and is no default route.
		{"a /1 is not a default", routeHeader +
			"tun0\t00000000\t0100080A\t0003\t0\t0\t0\t00000080\t0\t0\t0\n" + defaultRow("enp3s0", "100"),
			all, "", "enp3s0"},
	} {
		if got := parseDefaultRouteDev(tc.text, tc.carrier, tc.prefer); got != tc.want {
			t.Errorf("%s: parseDefaultRouteDev = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// THE SETTLE WAIT'S QUESTION: a device with a link and no default route yet holds the choice. A
// device holding a default does not, and neither does one without carrier.
func TestParseSettling(t *testing.T) {
	text := routeHeader + defaultRow("wlp2s0", "600")
	for _, tc := range []struct {
		name    string
		devs    []string
		carrier func(string) bool
		want    string
	}{
		{"the wire has a link and no route yet", []string{"enp3s0", "wlp2s0"}, func(string) bool { return true }, "enp3s0"},
		{"the wire has no link", []string{"enp3s0", "wlp2s0"}, func(d string) bool { return d != "enp3s0" }, ""},
		{"every device with a link is routed", []string{"wlp2s0"}, func(string) bool { return true }, ""},
		{"no devices", nil, func(string) bool { return true }, ""},
	} {
		if got := parseSettling(text, tc.devs, tc.carrier); got != tc.want {
			t.Errorf("%s: parseSettling = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// THE LITTLE-ENDIAN DECODE, which is the whole reason this parser is worth testing. 0154C558 is
// 88.197.84.1 read back-to-front, and `ip route` on the same box agrees. A big-endian read would
// say 1.84.197.88 -- a plausible-looking address, which is exactly why reading the code cannot
// settle it.
func TestParseDefaultGateway(t *testing.T) {
	for _, tc := range []struct{ name, dev, want string }{
		{"the gateway, little-endian", "eno1", "88.197.84.1"},
		// eno2 holds a connected route and no default: its next hop is 00000000, and a parser
		// that matched on the device alone would answer 0.0.0.0 -- a fingerprint that then ARPs
		// for nothing and records an empty gateway MAC as though it had looked.
		{"a device with no default route", "eno2", ""},
		{"a device that is not there", "enp3s0", ""},
	} {
		if got := parseDefaultGateway(procNetRoute, tc.dev); got != tc.want {
			t.Errorf("%s: parseDefaultGateway(%q) = %q, want %q", tc.name, tc.dev, got, tc.want)
		}
	}
}

// procNetARP as the kernel renders it, with one row edited to the shape the flag exists to catch:
// 192.168.8.9 is an INCOMPLETE entry (flags 0x0), which is what an unanswered probe leaves behind.
const procNetARP = `IP address       HW type     Flags       HW address            Mask     Device
88.197.86.127    0x1         0x2         34:17:eb:d1:15:60     *        eno1
192.168.8.3      0x1         0x2         74:19:f8:16:60:c5     *        eno2
192.168.8.9      0x1         0x0         00:00:00:00:00:00     *        eno2
88.197.84.1      0x1         0x2         e8:d3:22:5a:a4:bc     *        eno1
`

func TestParseARPMAC(t *testing.T) {
	for _, tc := range []struct{ name, dev, ip, want string }{
		{"a resolved neighbour", "eno1", "88.197.84.1", "e8:d3:22:5a:a4:bc"},
		{"another, on the other device", "eno2", "192.168.8.3", "74:19:f8:16:60:c5"},
		// ATF_COM IS THE EVIDENCE, NOT THE ROW. An unanswered probe leaves the row present, and a
		// parser that read the HW address column alone would hand back all-zeroes as a gateway
		// MAC -- which Compare would then match against another all-zeroes reading and call
		// SameWire, the one relation that says "rebuild now".
		{"an incomplete entry is not an answer", "eno2", "192.168.8.9", ""},
		// The device is part of the question: the same address reachable on two segments is two
		// different neighbours.
		{"right address, wrong device", "eno1", "192.168.8.3", ""},
		{"no such entry", "eno1", "10.0.0.1", ""},
	} {
		if got := parseARPMAC(procNetARP, tc.dev, tc.ip); got != tc.want {
			t.Errorf("%s: parseARPMAC(%q, %q) = %q, want %q", tc.name, tc.dev, tc.ip, got, tc.want)
		}
	}
}

// The flag values are captures from a real macvtap child on brie, taken at each step of the
// sequence ensureMacvtap performs.
func TestParseIfFlags(t *testing.T) {
	for _, tc := range []struct {
		name         string
		text         string
		want         int64
		wantAllmulti bool
	}{
		{"a macvtap as created", "0x1002\n", 0x1002, false},
		{"after `allmulticast on`", "0x1202\n", 0x1202, true},
		{"up, with allmulti", "0x1203\n", 0x1203, true},
		{"an ordinary NIC", "0x1003\n", 0x1003, false},
		// 0 is "no flags set", which sends Converged to false: the tick tries to fix the device
		// rather than assuming an unreadable one is fine.
		{"unreadable", "", 0, false},
		{"not a number", "banana\n", 0, false},
	} {
		got := parseIfFlags(tc.text)
		if got != tc.want {
			t.Errorf("%s: parseIfFlags(%q) = %#x, want %#x", tc.name, tc.text, got, tc.want)
		}
		if allmulti := got&ifAllmulti != 0; allmulti != tc.wantAllmulti {
			t.Errorf("%s: ALLMULTI = %v, want %v", tc.name, allmulti, tc.wantAllmulti)
		}
	}
}

// THE SYMLINK'S DEPTH VARIES, and that is the bug this test is here for: a port whose
// own sysfs node is virtual sits one level from its bridge, a physical NIC several. A prefix trim
// of a single `../` answers the first shape and silently mangles the second, so Converged would
// have answered false forever on a bridge parented by a real card -- and the tick would have
// re-converged on every pass.
func TestParseMaster(t *testing.T) {
	for _, tc := range []struct{ name, link, want string }{
		{"a virtual port, one level", "../b153br", "b153br"},
		{"a physical NIC's port, several", "../../../../virtual/net/br0", "br0"},
		{"an absolute target", "/sys/devices/virtual/net/br-lan", "br-lan"},
	} {
		if got := parseMaster(tc.link); got != tc.want {
			t.Errorf("%s: parseMaster(%q) = %q, want %q", tc.name, tc.link, got, tc.want)
		}
	}
}

// THE ARGV, PINNED. Each of these carries a detail that cannot be inferred by reading the call
// site, and until now only a 12-minute rig would have noticed one change.
func TestIPArgv(t *testing.T) {
	// `mode bridge` is the macvtap mode that lets the guest's two children talk to each other. It
	// is not `mode private` (which would isolate them from one another) and not a bridge device.
	want := []string{"link", "add", "link", "eno1", "name", "sys-n1", "type", "macvtap", "mode", "bridge"}
	if got := macvtapAddArgs("sys-n1", "eno1"); !slices.Equal(got, want) {
		t.Errorf("macvtapAddArgs = %q, want %q", got, want)
	}
	// The probe's whole claim is that it creates THE VERY THING the install creates, so it must
	// render the same argv but for the device name.
	probe := macvtapAddArgs(probeDev, "eno1")
	install := macvtapAddArgs("sys-n1", "eno1")
	for i := range probe {
		if probe[i] != install[i] && probe[i] != probeDev && install[i] != "sys-n1" {
			t.Errorf("the probe's argv differs from the install's at %d: %q vs %q", i, probe[i], install[i])
		}
	}
	if w := []string{"tuntap", "add", "priv-n1", "mode", "tap"}; !slices.Equal(tapAddArgs("priv-n1"), w) {
		t.Errorf("tapAddArgs = %q, want %q", tapAddArgs("priv-n1"), w)
	}
	// MULTICAST, deliberately not promiscuous: `allmulticast on`, never `promisc on`, which would
	// pull every unicast frame on the segment off the wire for nothing.
	if w := []string{"link", "set", "sys-n1", "allmulticast", "on"}; !slices.Equal(allmulticastOnArgs("sys-n1"), w) {
		t.Errorf("allmulticastOnArgs = %q, want %q", allmulticastOnArgs("sys-n1"), w)
	}
	// ipvtap: `mode l2`, or the guest is no L2 citizen (no ARP, no DHCP, no mDNS); `bridge`, or
	// its two children cannot reach each other. Created in the host's namespace, like the probe's,
	// so the probe still builds the very thing the install builds -- the move comes after.
	want = []string{"link", "add", "link", "wlan0", "name", "sys-n1", "type", "ipvtap", "mode", "l2", "bridge"}
	if got := ipvtapAddArgs("sys-n1", "wlan0"); !slices.Equal(got, want) {
		t.Errorf("ipvtapAddArgs = %q, want %q", got, want)
	}
}

// The holding namespace is read through `ip -n … -o link show`, whose lines name a child's
// parent by an index in ANOTHER namespace. The flags are where ALLMULTI is visible.
func TestParseLinks(t *testing.T) {
	text := "1: lo: <LOOPBACK> mtu 65536 qdisc noop state DOWN mode DEFAULT group default qlen 1000\\    link/loopback 00:00:00:00:00:00 brd 00:00:00:00:00:00\n" +
		"7: briard0@if3: <BROADCAST,MULTICAST,ALLMULTI,UP,LOWER_UP> mtu 1500 qdisc noqueue state UP mode DEFAULT\\    link/ether 02:00:00:00:00:01 brd ff:ff:ff:ff:ff:ff link-netnsid 0\n" +
		"8: briard-drbd0@if3: <BROADCAST,MULTICAST> mtu 1500 qdisc noop state DOWN\n"
	got := parseLinks(text)
	if !slices.Contains(got["briard0"], "ALLMULTI") || !slices.Contains(got["briard0"], "UP") {
		t.Errorf("briard0 flags = %q", got["briard0"])
	}
	if f, ok := got["briard-drbd0"]; !ok || slices.Contains(f, "ALLMULTI") {
		t.Errorf("briard-drbd0 = %q (present %v), want present without ALLMULTI", f, ok)
	}
	if got["briard0@if3"] != nil {
		t.Error("the @ifN suffix must be cut off the name")
	}
	if got["nope"] != nil {
		t.Error("an absent device must read as nil, which is how the callers ask")
	}
}

// Hold compares what a child holds against what it should, so parseInet must find every address
// and nothing else.
func TestParseInet(t *testing.T) {
	text := "7: briard0    inet 192.168.7.120/32 scope global briard0\\       valid_lft forever preferred_lft forever\n" +
		"7: briard0    inet 192.168.7.121/32 scope global briard0\\       valid_lft forever preferred_lft forever\n" +
		"8: briard-drbd0    inet 10.40.3.1/32 scope global briard-drbd0\\       valid_lft forever preferred_lft forever\n"
	got := parseInet(text)
	if want := []string{"192.168.7.120/32", "192.168.7.121/32"}; !slices.Equal(got["briard0"], want) {
		t.Errorf("briard0 = %q, want %q", got["briard0"], want)
	}
	if want := []string{"10.40.3.1/32"}; !slices.Equal(got["briard-drbd0"], want) {
		t.Errorf("briard-drbd0 = %q, want %q", got["briard-drbd0"], want)
	}
	if got := parseInet(""); len(got) != 0 {
		t.Errorf("parseInet of nothing = %q", got)
	}
}

// A /32 to the guest's node IP on the bridge, in /proc/net/route's own shape: little-endian hex, a
// full mask. The /24 beside it and the same /32 on another device are not the route.
func TestParseHostRoute(t *testing.T) {
	const text = "Iface\tDestination\tGateway \tFlags\tRefCnt\tUse\tMetric\tMask\t\tMTU\tWindow\tIRTT\n" +
		"br0\t00000000\t0101A8C0\t0003\t0\t0\t0\t00000000\t0\t0\t0\n" +
		"br0\t0001A8C0\t00000000\t0001\t0\t0\t0\t00FFFFFF\t0\t0\t0\n" +
		"br0\t0105000A\t00000000\t0005\t0\t0\t0\tFFFFFFFF\t0\t0\t0\n" +
		"eth9\t0205000A\t00000000\t0005\t0\t0\t0\tFFFFFFFF\t0\t0\t0\n"
	for _, tc := range []struct {
		dev, dst string
		want     bool
	}{
		{"br0", "10.0.5.1", true},
		{"br0", "10.0.5.2", false}, // on another device
		{"eth9", "10.0.5.2", true},
		{"br0", "192.168.1.0", false}, // a /24, not a host route
		{"br0", "not-an-ip", false},
	} {
		if got := parseHostRoute(text, tc.dev, tc.dst); got != tc.want {
			t.Errorf("parseHostRoute(%s, %s) = %v, want %v", tc.dev, tc.dst, got, tc.want)
		}
	}
}
