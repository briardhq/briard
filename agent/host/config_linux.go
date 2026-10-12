package host

// The install roots on Linux. Constants because the prefix is: /opt/briard is baked into the qemu
// bundle's own ELF interpreter, so an install anywhere else produces a qemu that cannot execute.
const (
	prefixDir = "/opt/briard"
	stateDir  = "/var/lib/briard"
	runDir    = "/run/briard"

	// adminPortSock is the guest admin port's host end, beside the run directory rather than in it.
	adminPortSock = "/run/briard-admin.sock"
	// guestConsole is the guest's serial capture.
	guestConsole = "/var/log/briard-guest-console.log"
	// guestUnitLog is where the guest unit's own output goes: "" is the journal, which is there.
	guestUnitLog = ""

	// bridgeWhose says, in the log, whose bridge the guest hangs off: on Linux, always the user's.
	bridgeWhose = "a bridge this host did not create"

	// defaultSystemTap is the guest's eth1 on the host: a tap the agent creates.
	defaultSystemTap = "briard-drbd0"

	// defaultAccel is KVM, with TCG as the fallback qemu takes when /dev/kvm is not there.
	defaultAccel = "kvm:tcg"
)
