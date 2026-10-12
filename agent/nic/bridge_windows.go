package nic

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"golang.org/x/sys/windows"
)

// THE WINDOWS BRIDGE IS BUILT, ONCE, AT INSTALL -- never by convergence.
//
// On Linux the agent never creates a bridge: a bridge parent is the user's, and the guest gets a
// port on it. On Windows there is no bridge to find, and the guest's one tap has to be a member of
// one, so the agent builds it -- but as an ACT the installer sequences, not as drift the tick
// restores: a Windows bridge carries a TAP member only if the bridge object was CREATED with it,
// and only after the machine restarts (measured on the rig: six other levers left the member
// inert). So a bridge somebody deleted is not rebuilt by the tick; it is a degraded node that says
// so, exactly as a missing user bridge is on Linux.

// The names a person sees in Network Connections. An unexplained adapter on a stranger's desktop
// is exactly what a helpful relative deletes, so ours say whose they are.
const (
	BridgeName = "Briard bridge"
	TapName    = "Briard guest link"
)

// Offload is what building the bridge changed on the household's own NIC, recorded BEFORE the
// change so that removing briard can put back what was there: keyed by the adapter's GUID (a
// rename, ours or the user's, moves the name; the GUID stays) and stored as the registry value,
// never the display string, which is localized.
type Offload struct {
	GUID    string `json:"guid"`
	Keyword string `json:"keyword"`
	Value   string `json:"value"`
}

// checksumOffload is the setting the bridge needs off on the member: bridge.sys emits IPv4 frames
// with a zero header checksum when the member offloads it, and a Linux receiver drops them.
const checksumOffload = "*IPChecksumOffloadIPv4"

// BuildBridge bridges member (the household's NIC, by name) with the TAP adapter, and returns what
// it changed on member. Idempotent: a bridge that already holds both is left as it is and reported with a
// zero Offload. It refuses a bridge that exists without them -- Windows allows one bridge, and a
// bridge the user made is theirs.
//
// The caller owes the machine a RESTART before the guest's frames will cross the bridge, and
// persists the returned record before anything else can lose it.
func BuildBridge(ctx context.Context, member string) (Offload, error) {
	as := adapters()
	m, ok := byName(as, member)
	if !ok {
		return Offload{}, fmt.Errorf("no adapter named %q to bridge", member)
	}
	// An existing bridge is judged first: ours (named for us, holding the member) is done -- its TAP
	// shows as a member only after the restart, so the TAP cannot be the test -- and any other is
	// not ours to change.
	for _, a := range as {
		if a.name != "" && !a.filter && IsBridge(a.name) {
			if a.name == BridgeName && master(m.name) == a.name {
				return Offload{}, nil
			}
			return Offload{}, fmt.Errorf("this machine already has a bridge (%q) that is not ours; Windows allows one", a.name)
		}
	}
	t, ok := tapAdapter(as)
	if !ok {
		return Offload{}, errors.New("no TAP adapter, or more than one: the tap-windows6 driver installs ours, at install")
	}
	// 1. THE RECORD, FIRST: after the change there is nothing left to read.
	val, err := powershell(ctx, fmt.Sprintf("(Get-NetAdapterAdvancedProperty -Name %s -RegistryKeyword '%s' -EA Stop).RegistryValue -join ','",
		psQuote(m.name), checksumOffload))
	if err != nil {
		return Offload{}, fmt.Errorf("reading %s on %s: %w", checksumOffload, m.name, err)
	}
	rec := Offload{GUID: m.guid, Keyword: checksumOffload, Value: val}
	if val != "0" {
		if _, err := powershell(ctx, fmt.Sprintf("Set-NetAdapterAdvancedProperty -Name %s -RegistryKeyword '%s' -RegistryValue 0 -EA Stop",
			psQuote(m.name), checksumOffload)); err != nil {
			return rec, fmt.Errorf("turning %s off on %s: %w", checksumOffload, m.name, err)
		}
	}
	// 2. Our TAP named for us, before the bridge exists (so the bridge is created with its final
	// member name).
	if t.name != TapName {
		if _, err := powershell(ctx, fmt.Sprintf("Rename-NetAdapter -Name %s -NewName %s -EA Stop", psQuote(t.name), psQuote(TapName))); err != nil {
			return rec, fmt.Errorf("naming the TAP adapter: %w", err)
		}
	}
	// 3. The bridge, created WITH the TAP, by GUID -- the one adapter id netsh takes that no name
	// or index change can make ambiguous.
	if out, err := exec.CommandContext(ctx, "netsh", "bridge", "create", m.guid, t.guid).CombinedOutput(); err != nil {
		return rec, fmt.Errorf("netsh bridge create: %s", firstLine(out, err))
	}
	br, err := waitBridge(ctx, m.name)
	if err != nil {
		return rec, err
	}
	// 4. The bridge keeps the household's own MAC, so the desktop keeps its identity on the LAN
	// (its DHCP reservation, its router's view of it) through being bridged.
	if _, err := powershell(ctx, fmt.Sprintf("Set-NetAdapterAdvancedProperty -Name %s -RegistryKeyword NetworkAddress -RegistryValue '%s' -EA Stop",
		psQuote(br), m.mac)); err != nil {
		return rec, fmt.Errorf("pinning the bridge's MAC: %w", err)
	}
	if br != BridgeName {
		if _, err := powershell(ctx, fmt.Sprintf("Rename-NetAdapter -Name %s -NewName %s -EA Stop", psQuote(br), psQuote(BridgeName))); err != nil {
			return rec, fmt.Errorf("naming the bridge: %w", err)
		}
	}
	return rec, nil
}

// waitBridge waits for the household NIC's new master to appear, and names it. Only the NIC: until
// the restart the TAP does not appear under the bridge in the stack table at all (measured -- it is
// the same fact as the bridge not carrying it yet), so waiting for it waits out the deadline.
func waitBridge(ctx context.Context, member string) (string, error) {
	deadline := time.Now().Add(60 * time.Second)
	for {
		if br := master(member); br != "" {
			return br, nil
		}
		if time.Now().After(deadline) {
			return "", errors.New("netsh created the bridge, but no bridge holds the member a minute later")
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// powershell runs one command and returns its trimmed output. The cmdlets are the only interface
// Windows offers to an adapter's advanced properties and names that also restarts the driver to
// apply them.
func powershell(ctx context.Context, cmd string) (string, error) {
	out, err := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-Command", cmd).CombinedOutput()
	if err != nil {
		return "", errors.New(firstLine(out, err))
	}
	return strings.TrimSpace(string(out)), nil
}

// psQuote single-quotes a PowerShell string literal, where the one escape is a doubled quote.
func psQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// tapDescription is how the tap-windows6 driver describes its adapters ("TAP-Windows Adapter V9",
// "... #2"), which is the one thing about a fresh one that is not up to Windows' naming.
const tapDescription = "TAP-Windows Adapter V9"

// tapAdapter is our TAP: the one already named for us, else the one tap-windows6 adapter that is
// not a bridge member. A filter layer carries its adapter's description as a prefix of its own
// ("TAP-Windows Adapter V9-WFP ..."), and is told apart by IP Helper's own flag. More than one
// candidate is ambiguous and answers nothing: another program's VPN adapter is a TAP too, and
// bridging the wrong one would put its traffic on the LAN.
func tapAdapter(as []adapter) (adapter, bool) {
	if a, ok := byName(as, TapName); ok {
		return a, true
	}
	var found []adapter
	for _, a := range as {
		if a.name != "" && !a.filter && strings.HasPrefix(a.desc, tapDescription) && master(a.name) == "" {
			found = append(found, a)
		}
	}
	if len(found) != 1 {
		return adapter{}, false
	}
	return found[0], true
}

// MemberSpeed names the household's NIC in bridge -- the member that is not our TAP -- and its link
// speed in bits/s. The bridge's own speed is the SUM of its members (measured: 2 Gbps on a gigabit
// NIC plus the TAP), so it is never the question. ("", 0) when there is no such member.
func MemberSpeed(bridge string) (string, uint64) {
	for _, a := range adapters() {
		if a.name == "" || a.filter || a.name == TapName || master(a.name) != bridge {
			continue
		}
		row := windows.MibIfRow2{InterfaceIndex: a.index}
		if windows.GetIfEntry2Ex(windows.MibIfEntryNormalWithoutStatistics, &row) != nil {
			return a.name, 0
		}
		return a.name, max(row.TransmitLinkSpeed, row.ReceiveLinkSpeed)
	}
	return "", 0
}
