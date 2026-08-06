// Package sdnotify sends systemd service-manager notifications (sd_notify(3)) over the
// $NOTIFY_SOCKET datagram socket — no cgo, no dependency, just the documented protocol.
//
// The host agent runs under `Type=notify`: systemd considers the unit
// "started" only once it receives READY=1, and self-update hangs the whole commit decision
// off that — ExecStartPost (the commit) runs only after READY, and a trial that never gets
// healthy never sends it, so TimeoutStartSec trips and the update reverts. So Ready() is the
// one piece of agent code in the update loop: the new binary proving itself.
package sdnotify

import (
	"net"
	"os"
)

// Ready tells the service manager the agent is up and healthy (READY=1). It is a no-op when
// $NOTIFY_SOCKET is unset (not run under Type=notify — e.g. tests, the lab fleet, a dev run),
// so callers can invoke it unconditionally on healthy convergence. Errors are returned but are
// non-fatal to the agent: readiness is a signal, not a dependency.
func Ready() error { return notify("READY=1") }

// Notify sends a raw sd_notify state string (e.g. "STATUS=...", "WATCHDOG=1", "RELOADING=1").
// No-op when $NOTIFY_SOCKET is unset.
func Notify(state string) error { return notify(state) }

func notify(state string) error {
	sock := os.Getenv("NOTIFY_SOCKET")
	if sock == "" {
		return nil // not under Type=notify — nothing to signal
	}
	// systemd uses an abstract socket ("@...") or a filesystem path; net.DialUnixgram
	// handles the path form, and the abstract form is expressed with a leading NUL.
	addr := sock
	if len(addr) > 0 && addr[0] == '@' {
		addr = "\x00" + addr[1:]
	}
	conn, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: addr, Net: "unixgram"})
	if err != nil {
		return err
	}
	defer conn.Close()
	_, err = conn.Write([]byte(state))
	return err
}
