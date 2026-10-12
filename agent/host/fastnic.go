package host

import (
	"fmt"

	"briard.io/shared/notify"
)

// fastNICBps is the link speed above which the bridge's disabled checksum offload costs throughput
// a household could notice. Gigabit is the audience the unconditional disable was decided for.
const fastNICBps = 1_000_000_000

// fastNICAlert is the warning a bridged NIC faster than gigabit earns, and whether it earns one.
// Building the bridge turned the NIC's IPv4 checksum offload off (bridge.sys emits frames a Linux
// receiver drops otherwise), which is right at gigabit and a ceiling above it. Only a person can
// test whether their NIC and their network tolerate it back on, so the alert hands them the
// measured procedure rather than the agent guessing.
func fastNICAlert(member string, bps uint64) (notify.Alert, bool) {
	if member == "" || bps <= fastNICBps {
		return notify.Alert{}, false
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
