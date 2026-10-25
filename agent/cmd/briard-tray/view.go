// briard-tray is the Windows notification-area icon: it runs as the person who installed briard,
// unelevated, at their logon, and everything it does goes through the agent's local admin door
// -- the socket `briard` itself uses, which admits that one account (host.restrictDoor) -- so its
// clicks never ask for elevation. It reads the node's state with the status directive, opens the
// Briard page with a one-time link from the dashboard directive, and stops and starts briard the
// way `briard stop` and `briard start` do.
//
// This file is what the icon SHOWS for what it read, kept apart from the Windows calls that show
// it, so the decision is testable anywhere.
package main

import (
	"errors"
	"fmt"
	"io/fs"

	"briard.io/agent/host"
)

// state is the icon: one of four.
type state int

const (
	serving  state = iota
	degraded       // something is wrong: an open alert, or the node not serving
	stopped        // briard is stopped, or this account cannot reach it
	working        // briard is updating, starting, or doing what was just asked of it
)

// reading is one poll: the status answer, or why there was none.
type reading struct {
	status  host.LocalStatus
	err     error  // the status directive's failure; nil when status is the answer
	down    bool   // with err: the agent's service is stopped by a clean exit
	update  bool   // with err: a self-update's files are on disk beside the agent (staged, or swapped in)
	refusal string // the agent answered, and said no (the reason): a node with no network refuses every directive
	pending string // a stop or start this tray asked for and has not seen land: "stop", "start"
}

// view is what the icon shows for a reading: its state, its tooltip, and the one action its
// menu offers beside Open Briard -- "stop", "start", or "" while one is in flight.
type view struct {
	state  state
	tip    string
	action string
	open   int // alerts open, for the menu's count; 0 hides it
}

func show(r reading) view {
	switch {
	case r.pending == "stop" && !(r.err != nil && r.down):
		return view{state: working, tip: "Briard is stopping"}
	case r.pending == "start" && r.err != nil:
		return view{state: working, tip: "Briard is starting"}
	case r.err != nil && errors.Is(r.err, fs.ErrPermission):
		return view{state: stopped, tip: "Briard was installed by another account on this computer"}
	case r.refusal != "":
		return view{state: degraded, tip: tip(r.refusal), action: "stop"}
	case r.err != nil && r.update:
		// The update stops the agent to swap it, and starts it again: not a stop to undo.
		return view{state: working, tip: "Briard is updating"}
	case r.err != nil && r.down:
		return view{state: stopped, tip: "Briard is stopped", action: "start"}
	case r.err != nil && errors.Is(r.err, host.ErrNoAgent):
		// Not answering, yet not cleanly stopped: starting, or restarting after a failure, which
		// the service manager is already doing.
		return view{state: working, tip: "Briard is starting"}
	case r.err != nil:
		// The door is there and nothing answered in time: the agent is bringing its VM up.
		return view{state: working, tip: "Briard is starting"}
	case r.status.Updating:
		return view{state: working, tip: "Briard is updating", action: "stop", open: r.status.Open}
	case r.status.Open > 0:
		return view{state: degraded, tip: alerts(r.status.Open), action: "stop", open: r.status.Open}
	case !r.status.Serving:
		return view{state: degraded, tip: "Briard is not serving right now", action: "stop"}
	}
	return view{state: serving, tip: "Briard is running", action: "stop"}
}

// tip fits a sentence in a notification-area tooltip, which Windows cuts at 127 characters.
func tip(s string) string {
	if r := []rune(s); len(r) > 120 {
		return string(r[:119]) + "…"
	}
	return s
}

// alerts is the count as a person reads it.
func alerts(n int) string {
	if n == 1 {
		return "1 alert needs your attention"
	}
	return fmt.Sprintf("%d alerts need your attention", n)
}

// toast reports whether a reading after prev is news worth a notification: more alerts open than
// the last answer said. The first answer is never news -- what was open at logon is on the icon --
// and a poll that got no answer neither raises nor clears one.
func toast(prev *host.LocalStatus, r reading) bool {
	return r.err == nil && prev != nil && r.status.Open > prev.Open
}
