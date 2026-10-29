package nic

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
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
		return Offload{}, tapAmbiguity(tapCandidates(as))
	}
	// 1. THE RECORD, FIRST: after the change there is nothing left to read.
	rec, err := offloadOff(ctx, m)
	if err != nil {
		return rec, err
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

// offloadOff records a's IPv4 checksum offload and receive segment coalescing and turns them off,
// the settings a member needs (checksumOffload, rscOffloadIPv4, rscOffloadIPv6). The record is
// returned even when turning one off fails part-way. An adapter whose driver offers none of them
// has nothing to record or turn off (measured: Realtek's RTL8922AE package has no checksum
// keyword): the property list is read whole, so a failed read stays an error and only a list
// without the keyword reads as absent.
func offloadOff(ctx context.Context, a adapter) (Offload, error) {
	rec := Offload{GUID: a.guid, Keyword: checksumOffload}
	for i, v := range []*string{&rec.Value, &rec.RSC4, &rec.RSC6} {
		val, err := readSetting(ctx, a.name, rec.settings()[i][0])
		if err != nil {
			return Offload{}, err
		}
		*v = val
	}
	if rec.Value == "" && rec.RSC4 == "" && rec.RSC6 == "" {
		return Offload{}, nil
	}
	for _, s := range rec.settings() {
		if kw, val := s[0], s[1]; val != "" && val != "0" {
			if _, err := powershell(ctx, fmt.Sprintf("Set-NetAdapterAdvancedProperty -Name %s -RegistryKeyword '%s' -RegistryValue 0 -EA Stop",
				psQuote(a.name), kw)); err != nil {
				return rec, fmt.Errorf("turning %s off on %s: %w", kw, a.name, err)
			}
		}
	}
	return rec, nil
}

// readSetting is adapter name's registry value for keyword kw: "" when its driver has no such
// setting (the property list is read whole, so a failed read is an error, never an absence).
func readSetting(ctx context.Context, name, kw string) (string, error) {
	val, err := powershell(ctx, fmt.Sprintf("@(Get-NetAdapterAdvancedProperty -Name %s -AllProperties -EA Stop | Where-Object RegistryKeyword -eq '%s').RegistryValue -join ','",
		psQuote(name), kw))
	if err != nil {
		return "", fmt.Errorf("reading %s on %s: %w", kw, name, err)
	}
	return val, nil
}

// putBack gives adapter name the values rec holds, keyword by keyword; a "" value is skipped.
func putBack(ctx context.Context, name string, rec Offload) error {
	for _, s := range rec.settings() {
		if kw, val := s[0], s[1]; val != "" {
			if _, err := powershell(ctx, fmt.Sprintf("Set-NetAdapterAdvancedProperty -Name %s -RegistryKeyword '%s' -RegistryValue '%s' -EA Stop",
				psQuote(name), kw, val)); err != nil {
				return fmt.Errorf("putting %s back to %s on %s: %w", kw, val, name, err)
			}
		}
	}
	return nil
}

// SwapMember moves bridge onto the standalone adapter to: the physical member leaves, the
// bridge's MAC is re-pinned to to's, and to joins. The TAP stays where it is: its membership is
// the part bound to the bridge's creation and a restart, and a physical member's is not (measured:
// a live swap carried the guest, both directions). It returns to's offload record; prev is the
// leaving member's, whose value is put back once it is standalone again.
//
// ⚠️ OUT BEFORE IN. Both uplinks in the bridge at once on one LAN is a loop, so to is added only
// once the leaving member is seen gone, and not at all if it is not.
//
// ⚠️ THE RE-PIN, because the bridge keeps the household's MAC (BuildBridge): unmoved, the leaving
// member, standalone again, transmits the MAC the bridge still carries.
func SwapMember(ctx context.Context, bridge, to string, prev Offload) (Offload, error) {
	as := adapters()
	b, ok := byName(as, bridge)
	if !ok || !IsBridge(bridge) {
		return Offload{}, fmt.Errorf("no bridge named %q", bridge)
	}
	// Hardware, as the selector's candidates are (DefaultRoute): the leaving member is removed
	// before the arriving one is added, so a refusal the add would give must be given here.
	t, ok := byName(as, to)
	if !ok || t.filter || IsBridge(to) || master(to) != "" || t.name == TapName || strings.HasPrefix(t.desc, tapDescription) || !isHardware(t.index) {
		return Offload{}, fmt.Errorf("%q is not a standalone physical adapter to swap into %s", to, bridge)
	}
	from, _ := MemberSpeed(bridge)
	rec, err := offloadOff(ctx, t)
	if err != nil {
		return rec, err
	}
	if from != "" {
		if err := netsh(ctx, "bridge", "remove", from, "from", b.guid); err != nil {
			return rec, fmt.Errorf("netsh bridge remove %s: %w", from, err)
		}
		if err := waitMaster(ctx, from, ""); err != nil {
			return rec, fmt.Errorf("%s did not leave %s, so %s was not added: %w", from, bridge, to, err)
		}
		if f, ok := byName(adapters(), from); ok && prev.GUID == f.guid {
			// Best-effort: the leaving member works either way; it is the bridge that needs them off.
			_ = putBack(ctx, from, prev)
		}
	}
	if _, err := powershell(ctx, fmt.Sprintf("Set-NetAdapterAdvancedProperty -Name %s -RegistryKeyword NetworkAddress -RegistryValue '%s' -EA Stop",
		psQuote(bridge), t.mac)); err != nil {
		return rec, fmt.Errorf("re-pinning the bridge's MAC to %s's: %w", to, err)
	}
	if err := netsh(ctx, "bridge", "add", to, "to", b.guid); err != nil {
		return rec, fmt.Errorf("netsh bridge add %s: %w", to, err)
	}
	if err := waitMaster(ctx, to, bridge); err != nil {
		return rec, fmt.Errorf("%s did not join %s: %w", to, bridge, err)
	}
	return rec, nil
}

// waitMaster waits up to 30 s for dev's bridge to read want ("" for none).
func waitMaster(ctx context.Context, dev, want string) error {
	deadline := time.Now().Add(30 * time.Second)
	for master(dev) != want {
		if time.Now().After(deadline) {
			return errors.New("not within 30 s")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return nil
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
	found := tapCandidates(as)
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
		return a.name, Speed(a.name)
	}
	return "", 0
}

// Speed is dev's link speed in bits/s, 0 when it cannot be read.
func Speed(dev string) uint64 {
	a, ok := byName(adapters(), dev)
	if !ok {
		return 0
	}
	row := windows.MibIfRow2{InterfaceIndex: a.index}
	if windows.GetIfEntry2Ex(windows.MibIfEntryNormalWithoutStatistics, &row) != nil {
		return 0
	}
	return max(row.TransmitLinkSpeed, row.ReceiveLinkSpeed)
}

// TapInstalled reports whether a tap-windows6 adapter exists, bridged or not.
func TapInstalled() bool {
	for _, a := range adapters() {
		if a.name != "" && !a.filter && (a.name == TapName || strings.HasPrefix(a.desc, tapDescription)) {
			return true
		}
	}
	return false
}

// LinkChanges is a channel that receives when Windows reports a change to an IP interface. A
// bridge member has no IP binding, so its carrier is never reported itself: what is reported is
// the bridge's, whose link speed is the sum of its members and so moves on every edge of a
// member's carrier (measured on the rig: four notifications per edge, within milliseconds of the
// member's own state). A receive therefore says only "look again"; the reader reads the member.
//
// Registered once per process and never cancelled: a callback made by windows.NewCallback is
// never freed, so one per observe loop would leak one per channel re-dial. Notifications that
// arrive while nobody reads coalesce into one.
func LinkChanges() (<-chan struct{}, error) {
	linkOnce.Do(func() {
		cb := windows.NewCallback(func(_ uintptr, _ *windows.MibIpInterfaceRow, _ uint32) uintptr {
			select {
			case linkChanged <- struct{}{}:
			default:
			}
			return 0
		})
		var h windows.Handle
		if err := windows.NotifyIpInterfaceChange(windows.AF_UNSPEC, cb, nil, false, &h); err != nil {
			linkErr = fmt.Errorf("NotifyIpInterfaceChange: %w", err)
		}
	})
	if linkErr != nil {
		return nil, linkErr
	}
	return linkChanged, nil
}

var (
	linkOnce    sync.Once
	linkChanged = make(chan struct{}, 1)
	linkErr     error
)

// DestroyBridge undoes BuildBridge (`briard uninstall`): our bridge is destroyed, its member gets
// the values rec recorded back, and our TAP is removed. A bridge that is not ours is never
// touched, and an absent one is already destroyed.
//
// THE MEMBER BY GUID, the value by registry keyword and value: a name is the user's to change and
// a display string is localized. Never the driver's default either -- a user who had tuned the
// setting gets their own value back, not the factory one. An empty Value is a driver with no such
// setting, and restores nothing.
//
// THE TAP BY INSTANCE, only once the bridge is gone. The driver's own `tapinstall remove <hwid>`
// removes EVERY instance of the hardware id (measured: "2 device(s) were removed" when one was
// asked for, taking a live bridge member with it), and another program's VPN adapter is a TAP too.
// The driver package itself stays: other programs may use it, and a reinstall's TAP comes from it.
func DestroyBridge(ctx context.Context, rec Offload) error {
	as := adapters()
	if b, ok := byName(as, BridgeName); ok && IsBridge(BridgeName) {
		member, _ := MemberSpeed(BridgeName)
		if err := netsh(ctx, "bridge", "destroy", b.guid); err != nil {
			return fmt.Errorf("netsh bridge destroy: %w", err)
		}
		if member != "" {
			if err := waitMaster(ctx, member, ""); err != nil {
				return fmt.Errorf("%s did not leave %s: %w", member, BridgeName, err)
			}
		}
	}
	if rec.Value != "" || rec.RSC4 != "" || rec.RSC6 != "" {
		m, ok := byGUID(adapters(), rec.GUID)
		if !ok {
			return fmt.Errorf("no adapter %s to give its settings back to (%+v)", rec.GUID, rec)
		}
		if err := putBack(ctx, m.name, rec); err != nil {
			return err
		}
	}
	if _, ok := byName(adapters(), TapName); !ok {
		return nil
	}
	id, err := powershell(ctx, fmt.Sprintf("(Get-NetAdapter -Name %s -EA Stop).PnPDeviceID", psQuote(TapName)))
	if err != nil || id == "" {
		return fmt.Errorf("finding %s's device instance: %v", TapName, err)
	}
	if out, err := exec.CommandContext(ctx, "pnputil", "/remove-device", id).CombinedOutput(); err != nil {
		return fmt.Errorf("pnputil /remove-device %s: %s", id, firstLine(out, err))
	}
	return nil
}

// tapCandidates are the tap-windows6 adapters that could be ours: not a filter layer, not a bridge
// member already.
func tapCandidates(as []adapter) []adapter {
	var found []adapter
	for _, a := range as {
		if a.name != "" && !a.filter && strings.HasPrefix(a.desc, tapDescription) && master(a.name) == "" {
			found = append(found, a)
		}
	}
	return found
}

// tapAmbiguity is the refusal when there is not exactly one candidate, with the way out: none is
// an install that did not finish its driver step; several is never guessed between, since one may
// be another program's VPN -- named, so the person can tell which.
func tapAmbiguity(found []adapter) error {
	if len(found) == 0 {
		return errors.New("no TAP adapter: the tap-windows6 driver installs ours, at install; run the installer again")
	}
	names := make([]string, len(found))
	for i, a := range found {
		names[i] = fmt.Sprintf("%q (%s)", a.name, a.desc)
	}
	return fmt.Errorf("%d TAP adapters (%s), and briard will not pick one: one may be another program's VPN. "+
		"Remove the ones nothing uses (Device Manager > Network adapters), or uninstall TAP-Windows from "+
		"Installed apps if no VPN needs it, then install again", len(found), strings.Join(names, ", "))
}
