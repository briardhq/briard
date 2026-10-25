//go:build !windows

package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"briard.io/agent/host"
	"briard.io/agent/platform"
	"briard.io/agent/selfupdate"
)

// runService is false off Windows: systemd runs the agent as an ordinary process.
func runService() bool { return false }

// elevateHint is the refusal when it is not.
const elevateHint = "run as root (curl ... | sudo sh)"

// elevated is root, which an install or an uninstall needs.
func elevated() bool { return os.Geteuid() == 0 }

// What `briard uninstall` names: the install tree, the data it keeps, and how to run it as needed.
const (
	installTree      = "/opt/briard"
	dataTree         = "/var/lib/briard"
	uninstallElevate = "run it as root (sudo briard uninstall ...)"
)

// windowed is false off Windows: a terminal is where `briard uninstall` runs, and its text stays
// there. confirm and tell are never reached.
func windowed() bool      { return false }
func confirm(string) bool { return false }
func tell(string, bool)   {}

// runWarden refuses off Windows: systemd's own watchdog is the agent's outside watcher there.
func runWarden() int {
	fmt.Fprintln(os.Stderr, "briard: the warden is the Windows service's; systemd watches the agent here")
	return 2
}

// updateStop and updateStart are the self-update's two service verbs (runUpdate): the agent
// unit's ordinary stop, with patience, and its start to READY. systemd's watchdog needs nothing
// of its own.
func updateStop(ctx context.Context, patience time.Duration) error {
	return platform.StopService(ctx, selfupdate.DefaultAgentUnit, patience, log.Printf)
}

func updateStart(ctx context.Context) error {
	return host.StartAgent(ctx, log.Printf)
}
