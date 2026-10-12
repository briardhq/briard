package reportcard

import "fmt"

// platformChecks are the Windows host's hard requirements: an administrator, the hypervisor WHPX
// runs on, and the TAP driver the guest's network rides.
func platformChecks(f HostFacts) []Check {
	var cs []Check
	if f.Elevated {
		cs = append(cs, Check{"admin", Pass, "running as an administrator", ""})
	} else {
		cs = append(cs, Check{"admin", Refuse, "not running as an administrator",
			"run the installer as an administrator: it registers a service and builds a network bridge"})
	}
	// THE HYPERVISOR, asked of the hypervisor rather than of the feature list: the feature reads
	// "Enabled" from the moment it is turned on, while the hypervisor only runs after a restart.
	// So "enabled, not running" is the normal state of a first install -- the installer turns it
	// on and restarts once for it and the bridge together -- and is said, not refused.
	switch {
	case f.Hypervisor:
		cs = append(cs, Check{"hypervisor", Pass, "the Windows hypervisor is running (WHPX can accelerate the guest)", ""})
	case f.HypervisorPending:
		cs = append(cs, Check{"hypervisor", Warn, "Windows Hypervisor Platform is on, and starts at the next restart",
			"the install ends by asking for that restart; if this machine has restarted since the feature was turned on, virtualization (VT-x / AMD-V) is off in its firmware settings"})
	default:
		cs = append(cs, Check{"hypervisor", Refuse, "Windows Hypervisor Platform is not enabled",
			"the installer turns it on; by hand: Settings > Optional features > More Windows features > Windows Hypervisor Platform, then restart"})
	}
	if f.TAP {
		cs = append(cs, Check{"tap", Pass, "the TAP driver is installed", ""})
	} else {
		cs = append(cs, Check{"tap", Refuse, "the TAP driver (tap-windows6) is not installed",
			"the installer installs it, before this check runs"})
	}
	return cs
}

// platformAdvisories warns about a NIC faster than gigabit: the bridge turns its IPv4 checksum
// offload off, which holds such a link below its speed. Read from the NIC itself before the
// bridge exists -- a bridge reports the sum of its members.
func platformAdvisories(f HostFacts) []Check {
	if f.NICSpeedBps <= FastNICBps {
		return nil
	}
	return []Check{{"nic-speed", Warn, fmt.Sprintf("%s runs at %.1f Gbit/s", f.NIC.Dev, float64(f.NICSpeedBps)/1e9),
		"Briard's network bridge turns this card's IPv4 checksum offload off, which can hold transfers below that speed; Briard will say how to try it back on"}}
}
