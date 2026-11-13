package host

import (
	"fmt"

	"briard.io/agent/reportcard"
	"briard.io/shared/notify"
)

// fastNICAlert is the notice a bridged NIC faster than gigabit earns, and whether there is
// anything to say. Building the bridge turned the NIC's IPv4 checksum offload off (bridge.sys
// emits frames a Linux receiver drops otherwise), which is right at gigabit and a ceiling above
// it. Only a person can test whether their NIC and their network tolerate it back on, so the
// notice hands them the measured procedure rather than the agent guessing. It does not read the
// setting back: Info is history only, so a notice outliving the fix asks nothing of anyone.
//
// It resolves when the condition is gone: no NIC in the bridge (member "", the bridge removed or
// the NIC with it) or a link at gigabit or below. A speed that cannot be read (0) is an unknown,
// and says nothing either way.
func fastNICAlert(member string, bps uint64) (notify.Alert, bool) {
	resolved := notify.Alert{Key: "fast-nic", Kind: notify.Resolved,
		Title: "Briard: the network card is no longer faster than Briard's network setting allows for"}
	switch {
	case member == "":
		return resolved, true
	case bps == 0:
		return notify.Alert{}, false
	case bps <= reportcard.FastNICBps:
		return resolved, true
	}
	return notify.Alert{
		Key:      "fast-nic",
		Kind:     notify.Open,
		Severity: notify.Info,
		Title:    "Briard: this computer's network card is faster than Briard's network setting allows for",
		Body: fmt.Sprintf("%s runs at %.1f Gbit/s, and setting up Briard turned its IPv4 checksum offload off, "+
			"which can hold transfers below that speed. To try it back on: in PowerShell as administrator, "+
			"Set-NetAdapterAdvancedProperty -Name '%s' -RegistryKeyword '*IPChecksumOffloadIPv4' -RegistryValue 3 -- "+
			"then check Get-NetIPAddress -InterfaceAlias 'Briard bridge'. If its address shows PrefixOrigin "+
			"WellKnown, the network did not take it: set the value back to 0.",
			member, float64(bps)/1e9, member),
	}, true
}
