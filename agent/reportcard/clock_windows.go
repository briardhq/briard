package reportcard

import (
	"context"

	"golang.org/x/sys/windows/registry"
)

// ClockRemedy is how a household turns time synchronisation on, said by the doctor and the
// agent's clock alert.
const ClockRemedy = "turn on Settings > Time & language > Date & time > Set the time automatically"

// NTPSynced is whether Windows keeps the clock by a time server: "yes" when its time service is
// set to synchronise, "no" when "Set the time automatically" is off, "" when the setting could not
// be read. Windows has no NTPSynchronized; the setting is the fact a household can change, and
// the service keeps the clock itself once it is on. The doctor and the agent's clock alert both
// read it here.
func NTPSynced(context.Context) string {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SYSTEM\CurrentControlSet\Services\W32Time\Parameters`, registry.QUERY_VALUE)
	if err != nil {
		return ""
	}
	defer k.Close()
	typ, _, err := k.GetStringValue("Type")
	if err != nil {
		return ""
	}
	return w32timeSynced(typ)
}
