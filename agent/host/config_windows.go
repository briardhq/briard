package host

import "briard.io/agent/nic"

// The install roots on Windows: one tree under %ProgramData%, which is where a machine-wide
// service keeps its files. Constants rather than read from the environment, for the reason the
// Linux roots are -- every default below derives from them, and an agent that relocated itself on
// a variable would be looking for its own state somewhere the installer never put it.
//
// There is no tmpfs, so runDir survives a reboot. What lives there today is sockets, which every
// listener removes before it binds. Anything that relies on the Linux runDir being emptied by a
// boot (self-update's trial flags) has no Windows arm yet, and gets its clear when it gets one.
const (
	prefixDir = `C:\ProgramData\briard`
	stateDir  = prefixDir + `\state`
	runDir    = prefixDir + `\run`
	logDir    = prefixDir + `\log`

	adminPortSock = runDir + `\admin-port.sock`
	guestConsole  = logDir + `\guest-console.log`
	// guestUnitLog is qemu's own output. The SCM keeps no journal, so it is a file or it is lost.
	guestUnitLog = logDir + `\briard-guest.log`

	// bridgeWhose says, in the log, whose bridge the guest hangs off: on Windows, the one briard built.
	bridgeWhose = "briard's bridge, built at install"

	// defaultSystemTap is the tap-windows6 adapter the bridge is built with, under the name the bridge
	// build gives it. It is the guest's ONE NIC here: the bridge shape has no service tap and no
	// private link (applySubstrate), and a config that named them would name devices this host
	// never has -- which the hot path reads as "not built" and sends every restart down the cold
	// start.
	defaultSystemTap  = nic.TapName
	defaultServiceTap = ""
	defaultWitnessTap = ""

	// openHint is how a person here asks for another one-time link.
	openHint = "Open Briard, on the Briard icon in the taskbar -- or briard open, in a terminal"

	// vipFixHint is the remedy a lease refusal names, with the prefix to fill in (lease.go).
	vipFixHint = "run `briard config set vip <address>/%s` in a terminal opened as administrator"

	// hostLeasesBridge: the Windows bridge is the one substrate here, and on a Wi-Fi member it
	// rewrites the guest's frames to the station's MAC, so the host leases the flock's address
	// on every bridge -- wired too, so a cable swap never changes who asks (lease.go).
	hostLeasesBridge = true

	// defaultAccel is WHPX, with TCG as the fallback qemu takes when the hypervisor platform is not
	// enabled.
	defaultAccel = "whpx:tcg"
)

// AgentLog is the agent's own log file. Under the SCM a service's stderr goes nowhere, so the
// agent's main sends log.Printf here.
func AgentLog() string { return logDir + `\briard-agent.log` }

// WardenLog is the warden's own log file, beside the agent's: what it watched, and every agent it
// ended.
func WardenLog() string { return logDir + `\briard-warden.log` }
