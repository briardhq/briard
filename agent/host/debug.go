package host

// The debug console's two directives ([B.142a]). Arming swaps the guest's second serial port
// (ttyS1) onto a unix socket so the getty that has been sitting there since boot becomes
// reachable; disarming puts it back on a null backend and QEMU unlinks the socket.
//
// THE AGENT OWNS THE ACT SO THAT IT OWNS THE RECORD. `briard debug shell` used to drive the
// monitor itself and write its own audit line to syslog, because an interactive CLI's stderr is
// the operator's terminal -- the one place the record will not be found afterwards. Here logf
// reaches the journal the way every other action on this node does, which is also what took
// log/syslog (and with it the GOOS seam it broke) out of the tree.
//
// WHAT THIS COSTS, ACCEPTED KNOWINGLY: routed through the observe loop, the console cannot be
// opened while that loop is wedged -- and a wedged agent is one reason somebody reaches for a
// shell. The owner's call was that this is closer to a feature than a defect: the user Briard is
// built for should be deterred from working on an already-degraded node, and anyone who can
// drive QMP by hand is not that user. A wedged host agent has its own answers (`briard logs`,
// restarting the unit, `rescue`).

import (
	"context"
	"time"

	"briard.io/agent/platform"
	"briard.io/shared/api"
)

// debugConsoleTimeout bounds one monitor round-trip. The same 15s the CLI used to apply from
// outside: a live QEMU answers chardev-change in milliseconds, so this is the "the monitor is
// not going to answer" deadline, not a budget anything is expected to spend.
const debugConsoleTimeout = 15 * time.Second

// applyDebugConsole handles both kinds. They share everything but the monitor call and the
// wording, and splitting them would mean two copies of the guard below.
func (cfg Config) applyDebugConsole(ctx context.Context, d api.Directive, logf func(string, ...any)) api.DirectiveOutcome {
	if cfg.QMPSock == "" {
		// A node with no monitor configured has no guest to open. Failed rather than done:
		// the caller asked for a console and is not getting one.
		return api.DirectiveOutcome{ID: d.ID, State: api.OutcomeFailed, Detail: "no QMP monitor on this node"}
	}
	console := platform.DebugConsolePath(cfg.QMPSock)
	cctx, cancel := context.WithTimeout(ctx, debugConsoleTimeout)
	defer cancel()

	if d.Kind == api.DirectiveDebugDisarm {
		if err := platform.DebugDisarm(cctx, cfg.QMPSock); err != nil {
			// LOUD, because a console that failed to close is a node left open and the journal
			// is the only place that fact now lives -- the operator is about to walk away.
			logf("debug console DISARM FAILED on %s: %v", console, err)
			return api.DirectiveOutcome{ID: d.ID, State: api.OutcomeFailed, Detail: err.Error()}
		}
		logf("debug console disarmed on %s", console)
		return api.DirectiveOutcome{ID: d.ID, State: api.OutcomeDone, Detail: console}
	}

	if err := platform.DebugArm(cctx, cfg.QMPSock, console); err != nil {
		logf("debug console arm failed on %s: %v", console, err)
		return api.DirectiveOutcome{ID: d.ID, State: api.OutcomeFailed, Detail: err.Error()}
	}
	// No uid here, unlike the syslog line this replaces. The admin socket is chmod 0600 owned by
	// the agent, so the peer is always root and the field was a constant wearing the look of
	// evidence. What is worth recording is that the local door was used at all.
	logf("debug console ARMED on %s (via the local admin socket)", console)
	return api.DirectiveOutcome{ID: d.ID, State: api.OutcomeDone, Detail: console}
}
