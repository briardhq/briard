package reportcard

// platformRefusals are the Linux host's hard requirements, each refused with its fix.
var platformRefusals = []refusal{
	{"virt disabled in BIOS", func(f *HostFacts) { f.DevKVM = false; f.VirtFlags = false }, "kvm", "BIOS"},
	{"kvm module not loaded", func(f *HostFacts) { f.DevKVM = false; f.VirtFlags = true }, "kvm", "modprobe"},
	{"no tun driver", func(f *HostFacts) { f.DevNetTun = false; f.TunModule = false }, "tun", "CONFIG_TUN"},
	{"no iproute2", func(f *HostFacts) { f.HasIP = false }, "iproute2", "iproute2"},
	{"not booted with systemd", func(f *HostFacts) { f.SystemdBooted = false }, "systemd", "systemd units"},
}
