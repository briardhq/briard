package nic

import (
	"context"
	"os/exec"
)

// ip is the iproute2 runner for this package -- the one per-OS thing in it. Everything above it
// (which device, what the message says, what the spec is) is shared and testable; a Windows host
// owes its own runner and its own Spec builder, because nothing about `ip link add ... type
// macvtap` translates to tap-windows6 and a bridge ([V5.5]).
//
// It is this package's own rather than agent/platform's: platform's runIP is unexported and
// exporting it would invite arbitrary `ip` calls from anywhere, which is the thing keeping it
// private buys. Two three-line runners behind two narrow APIs is the smaller cost.
func ip(ctx context.Context, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, "ip", args...).CombinedOutput()
}

// loadTun makes /dev/net/tun exist before anything tries to open it. Every device this package
// builds -- a plain tap, and the macvtap children qemu is handed as file descriptors -- is a
// character device under it, and a stock host has the driver as a module that nothing has asked
// for yet.
//
// It is the AGENT's to do rather than the installer's, for the same reason the rest of the
// substrate is ([B.150](d)): the installer runs once, and this has to be true at every boot on a
// host whose module never loaded. Cheap and idempotent -- modprobe on a loaded module returns
// immediately -- so it costs a fork on the passes that CREATE devices and nothing on the rest.
//
// Best-effort: a kernel with tun built in has no module to load and says so, and the report card
// has already refused a kernel that genuinely lacks CONFIG_TUN. The real error, if there is one,
// belongs to the device creation that follows.
func loadTun(ctx context.Context) {
	if exists("/dev/net/tun") {
		return
	}
	_, _ = exec.CommandContext(ctx, "modprobe", "tun").CombinedOutput()
}
