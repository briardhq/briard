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
	// build gives it.
	defaultSystemTap = nic.TapName

	// openHint is how a person here asks for another one-time link.
	openHint = "briard open, in a terminal opened as administrator"

	// defaultAccel is WHPX, with TCG as the fallback qemu takes when the hypervisor platform is not
	// enabled.
	defaultAccel = "whpx:tcg"
)

// AgentLog is the agent's own log file. Under the SCM a service's stderr goes nowhere, so the
// agent's main sends log.Printf here.
func AgentLog() string { return logDir + `\briard-agent.log` }
