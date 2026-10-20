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

	// The guest's three NICs on the host, taps the agent creates: eth1 (the node IP and DRBD),
	// eth2 (where the VIP lives) and eth3 (the private host<->guest link). The macvtap shape every
	// Linux node ships in; a bridge parent narrows it (applySubstrate).
	defaultSystemTap  = "briard-drbd0"
	defaultServiceTap = "briard0"
	defaultWitnessTap = "briard-priv0"

	// openHint is how a person here asks for another one-time link.
	openHint = "sudo briard open"

	// vipFixHint is the remedy a lease refusal names, with the prefix to fill in (lease.go).
	vipFixHint = "run `sudo briard config set vip <address>/%s`"

	// hostLeasesBridge: a Linux bridge carries the guest's own MAC to the wire, so the guest leases
	// its own address there (lease.go).
	hostLeasesBridge = false

	// defaultAccel is KVM, with TCG as the fallback qemu takes when /dev/kvm is not there.
	defaultAccel = "kvm:tcg"
)
