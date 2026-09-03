// Package sdnotify sends systemd service-manager notifications (sd_notify(3)) over the
// $NOTIFY_SOCKET datagram socket — no cgo, no dependency, just the documented protocol.
//
// The host agent runs under `Type=notify`: systemd considers the unit
// "started" only once it receives READY=1, and self-update hangs the whole commit decision
// off that — ExecStartPost (the commit) runs only after READY, and a trial that never sends it
// trips TimeoutStartSec and reverts. So Ready() is the one piece of agent code in the update
// loop: the new binary proving itself.
//
// WHAT READY ASSERTS, AND WHAT IT DELIBERATELY DOES NOT (V3.32). It asserts that THE AGENT
// started — config read, loop entered. It does NOT assert that the node is healthy, and it used
// to: the agent sent READY on first healthy convergence, which coupled a supervisor's readiness
// to the health of the thing it supervises. Two costs, and the first is fatal. systemd arms the
// watchdog only once start-up COMPLETES, so an agent whose guest never converged never left
// `activating` and never armed its watchdog — leaving the recovery ladder, the one stretch of
// code that exists for a broken guest, the one stretch with no watchdog over it. And it made
// TimeoutStartSec answer to three incompatible requirements at once. A supervisor is ready when
// it can supervise; a broken guest is work for a healthy agent, not evidence of a sick one.
//
// The gate that rides on it is thinner as a result — "the binary runs" rather than "the node is
// healthy" — and that is a deliberate trade recorded in V3.32, not an oversight. It still catches
// a binary that will not exec, panics immediately, or cannot parse its config. What it stopped
// catching, it never really caught: an agent restart re-adopts a guest that was already running,
// so a healthy guest after an update mostly restates that it was healthy before one.
package sdnotify

import (
	"net"
	"os"
	"sync/atomic"
)

// adopted holds the socket path taken out of the environment by Adopt, so notify() can still
// reach systemd once $NOTIFY_SOCKET is gone. Atomic because the watchdog pings from its own
// goroutine.
var adopted atomic.Pointer[string]

// Adopt takes ownership of $NOTIFY_SOCKET: it remembers the path and REMOVES the variable from
// this process's environment, so no child this process execs can inherit the socket. Call it
// once, early, from a process that is itself the unit's main PID — the host agent does, before it
// starts anything.
//
// WHY, and it is not tidiness ([V3b.21e]). $NOTIFY_SOCKET is an ordinary environment variable, so
// every child inherits it — and `systemctl`, which the agent shells out to constantly, ends each
// run by reporting its own exit status to whatever notify socket it finds: `EXIT_STATUS=0`. Under
// NotifyAccess=main systemd refuses those, and on systemd 255 says so on every start of every
// node. The refusal breaks nothing; the point is that the socket which gates this unit's
// readiness and feeds its watchdog has no business being reachable from a `systemctl show`.
// libsystemd's counterpart is sd_notify's unset_environment argument, which exists for this.
//
// The seam rather than a sweep of cmd.Env at every exec site, per [V3b.15]: an opt-in at N call
// sites is forgotten at site N+1, and silently.
//
// SAFE ONLY BECAUSE NOTHING RE-EXECS. A successor started from this process would find no
// NOTIFY_SOCKET, never signal, and hang until TimeoutStartSec. The agent never re-execs itself
// (no syscall.Exec in the tree; briard-exec is systemd's ExecStart — a fresh process, handed the
// variable fresh). Check that still holds before calling this from anywhere new.
func Adopt() {
	sock := os.Getenv("NOTIFY_SOCKET")
	if sock == "" {
		return // not under Type=notify — nothing to take, and nothing to keep from children
	}
	adopted.Store(&sock)
	_ = os.Unsetenv("NOTIFY_SOCKET")
}

// socket is where to send: the adopted path if Adopt took one, else the environment. The
// fallback is what keeps Ready/Watchdog usable without Adopt — tests, a dev run, the lab fleet.
func socket() string {
	if p := adopted.Load(); p != nil {
		return *p
	}
	return os.Getenv("NOTIFY_SOCKET")
}

// Ready tells the service manager the agent has started (READY=1) — see the package doc on what
// that does and does not assert. It is a no-op when $NOTIFY_SOCKET is unset (not run under
// Type=notify — e.g. tests, the lab fleet, a dev run), so callers can invoke it unconditionally.
// Errors are returned but are non-fatal to the agent: readiness is a signal, not a dependency.
func Ready() error { return notify("READY=1") }

// Watchdog sends one watchdog keep-alive (WATCHDOG=1). systemd expects these at least every
// WATCHDOG_USEC while the service is running and kills the unit on a miss (WatchdogSignal=,
// SIGABRT by default). No-op when $NOTIFY_SOCKET is unset.
//
// Cheap by construction — one datagram to a socket systemd already holds open — which is what
// makes it reasonable to call from many sites rather than budgeting them. You do not pay per
// ping; you pay per GAP.
func Watchdog() error { return notify("WATCHDOG=1") }

// Notify sends a raw sd_notify state string (e.g. "STATUS=...", "RELOADING=1").
// No-op when $NOTIFY_SOCKET is unset.
func Notify(state string) error { return notify(state) }

func notify(state string) error {
	sock := socket()
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
