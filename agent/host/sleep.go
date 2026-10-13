package host

import (
	"context"
	"fmt"
	"time"

	"briard.io/shared/notify"
)

// sleepAlerter tells the owner when this computer was asleep, after it wakes: an event, because
// by the time it can be said the sleep is over and there is nothing left to resolve. The agent
// holds idle sleep off, so a sleep that happens anyway was asked for -- a closed lid, the power
// button, Sleep from the Start menu, a Fast Startup shut down -- and every one of them took the
// household's services down for its length. When the machine has a lid that the power plan sleeps
// on (plugged in), the alert says how to change that, since the lid is the one a person closes
// without meaning to stop a server.
//
// ONLY A SLEEP THIS AGENT SAW START TO END IS NEWS. The first reading primes: a sleep that ended
// before the agent started was not one it was serving through. "No sleep since boot" is an answer
// (ok, zero times) and primes like any other -- read as unknown, the first sleep after a boot would
// be the reading that primes, and go untold. AN UNKNOWN IS NOT AN ALARM: a machine that does not
// say (Linux) or a read that failed is not-ok and changes nothing.
type sleepAlerter struct {
	read   func() (slept, woke time.Time, ok bool) // lastSleep in production
	lid    func() string                           // lidAction in production: "sleeps", "stays", or ""
	woke   time.Time                               // the wake already accounted for
	primed bool
}

// sleepSameWake is how far two readings of one wake may disagree: the times are derived from a
// tick count against the wall clock at each reading, so the same wake moves by milliseconds.
const sleepSameWake = 5 * time.Second

func (s *sleepAlerter) observe(ctx context.Context, n notify.Notifier, node string, logf func(string, ...any)) {
	if s == nil {
		return
	}
	slept, woke, ok := s.read()
	if !ok {
		return
	}
	if !s.primed {
		s.woke, s.primed = woke, true
		return
	}
	if woke.IsZero() || woke.Sub(s.woke) < sleepSameWake {
		return
	}
	s.woke = woke
	// To the second under a minute: a short sleep rounded to the minute reads "0s".
	asleep := woke.Sub(slept).Round(time.Minute)
	if asleep < time.Minute {
		asleep = woke.Sub(slept).Round(time.Second)
	}
	body := fmt.Sprintf("node %s was asleep from %s to %s (%s), and Briard's services, Home Assistant included, "+
		"were unreachable for that time. Briard keeps this computer awake by itself, so the sleep was asked for: "+
		"a closed lid, the power button, or Sleep from the Start menu.",
		node, slept.Local().Format("Mon 2 Jan 15:04"), woke.Local().Format("15:04"), asleep)
	if s.lid() == "sleeps" {
		body += " This computer sleeps when its lid is closed while plugged in. To keep it serving with the lid shut, " +
			"open Control Panel > Power Options > Choose what closing the lid does, and set \"When I close the lid\" " +
			"(Plugged in) to Do nothing. Or, in PowerShell as administrator: " +
			"powercfg /setacvalueindex SCHEME_CURRENT SUB_BUTTONS LIDACTION 0; powercfg /setactive SCHEME_CURRENT"
	}
	fireAlert(ctx, n, logf, notify.Alert{
		Key:      "host-asleep",
		Kind:     notify.Event,
		Severity: notify.Warning,
		Title:    "Briard: this computer was asleep",
		Body:     body,
	})
}
