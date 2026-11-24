package reportcard

// platformRefusals are the Windows host's hard requirements, each refused with its fix.
var platformRefusals = []refusal{
	{"not an administrator", func(f *HostFacts) { f.Elevated = false }, "admin", "administrator"},
	{"no hypervisor platform", func(f *HostFacts) { f.Hypervisor = false }, "hypervisor", "Windows Hypervisor Platform"},
	{"no TAP driver", func(f *HostFacts) { f.TAP = false }, "tap", "installer"},
}
