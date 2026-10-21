//go:build !windows

package main

import "os"

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
