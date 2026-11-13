package host

import (
	"context"
	"fmt"
	"time"

	"briard.io/shared/notify"
)

// sleepAlerter tells the owner when this computer was asleep, after it wakes: an event, because
// by the time it can be said the sleep is over and there is nothing left to resolve -- and asserts
// the condition that it sleeps at all, which is what asks for attention (sleepsKey). The agent
// holds idle sleep off, so a sleep that happens anyway was asked for -- a closed lid, the power
// button, Sleep from the Start menu, a Fast Startup shut down -- and every one of them took the
// household's apps down for its length. The last of them is a hibernate to the machine and a
// Shut down to the household, and is told as the one they chose. When the machine has a lid that the power plan sleeps
// on (plugged in), the alert says how to change that, since the lid is the one a person closes
// without meaning to stop a server.
//
// ONLY A SLEEP THIS AGENT SAW START TO END IS NEWS. The first reading primes: a sleep that ended
// before the agent started was not one it was serving through. "No sleep since boot" is an answer
// (ok, zero times) and primes like any other -- read as unknown, the first sleep after a boot would
// be the reading that primes, and go untold. AN UNKNOWN IS NOT AN ALARM: a machine that does not
// say (Linux) or a read that failed is not-ok and changes nothing.
type sleepAlerter struct {
	read func() (slept, woke time.Time, ok bool) // lastSleep in production
	lid  func() string                           // lidAction in production: "sleeps", "stays", or ""
	// shutDown reports whether the wake after a sleep that began at since was a power-on
	// (fastStartup in production); nil is never.
	shutDown func(since time.Time) bool
	woke     time.Time // the wake already accounted for
	primed   bool
}

// sleepSameWake is how far two readings of one wake may disagree: the times are derived from a
// tick count against the wall clock at each reading, so the same wake moves by milliseconds.
const sleepSameWake = 5 * time.Second

// sleepQuiet is how long without a sleep ends the host-sleeps condition. A week, because a laptop
// left shut over a weekend is the same habit as one shut every night: a day would resolve it on
// Saturday and ask again on Monday.
const sleepQuiet = 7 * 24 * time.Hour

// The two keys: every sleep is history (host-asleep, an Info event, the stretch and the apps down
// with it), and that this computer sleeps at all is what asks for attention (host-sleeps, a
// Warning condition). Each sleep asserts the condition again, which the store drops while it is
// open with the same title -- so one dismissal holds for as long as the habit does.
const (
	sleepEventKey = "host-asleep"
	sleepsKey     = "host-sleeps"
)

func (s *sleepAlerter) observe(ctx context.Context, n notify.Notifier, node string, now time.Time, logf func(string, ...any)) {
	if s == nil {
		return
	}
	s.tell(ctx, n, node, logf)
	s.settle(ctx, n, now, logf)
}

// tell raises a sleep this agent saw start to end: the event, and the condition asserted.
func (s *sleepAlerter) tell(ctx context.Context, n notify.Notifier, node string, logf func(string, ...any)) {
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
	from, to := slept.Local().Format("Mon 2 Jan 15:04"), woke.Local().Format("15:04")
	title := "Briard: this computer was asleep"
	body := fmt.Sprintf("machine %s was asleep from %s to %s (%s), and Briard's apps, Home Assistant included, "+
		"were unreachable for that time. Briard keeps this computer awake by itself, so the sleep was asked for: "+
		"a closed lid, the power button, or Sleep from the Start menu.",
		node, from, to, asleep)
	habit := fmt.Sprintf("machine %s has been asleep or shut down while it serves, and Briard's apps, Home Assistant "+
		"included, are unreachable for as long as it is; each time is listed under Activity. Briard keeps this "+
		"computer awake by itself, so each was asked for: a closed lid, the power button, Sleep or Shut down from "+
		"the Start menu. This notice closes after a week without one.", node)
	lidFix := ""
	if s.shutDown != nil && s.shutDown(slept) {
		title = "Briard: this computer was shut down"
		body = fmt.Sprintf("machine %s was shut down from %s to %s (%s), and Briard's apps, Home Assistant included, "+
			"were unreachable for that time. They came back by themselves when it was turned on.",
			node, from, to, asleep)
	} else if s.lid() == "sleeps" {
		lidFix = " This computer sleeps when its lid is closed while plugged in. To keep it serving with the lid shut, " +
			"open Control Panel > Power Options > Choose what closing the lid does, and set \"When I close the lid\" " +
			"(Plugged in) to Do nothing. Or, in PowerShell as administrator: " +
			"powercfg /setacvalueindex SCHEME_CURRENT SUB_BUTTONS LIDACTION 0; powercfg /setactive SCHEME_CURRENT"
	}
	fireAlert(ctx, n, logf, notify.Alert{Key: sleepEventKey, Kind: notify.Event, Severity: notify.Info,
		Title: title, Body: body + lidFix})
	fireAlert(ctx, n, logf, notify.Alert{Key: sleepsKey, Kind: notify.Open, Severity: notify.Warning,
		Title: "Briard: your apps stop while this computer sleeps or is off", Body: habit + lidFix})
}

// settle ends the condition sleepQuiet after the last sleep the STORE recorded: this alerter
// starts blank at every agent start, and the machine's own reading forgets a sleep at every boot,
// but the store kept the event. A notifier that is no store (a test's) has no record to ask.
func (s *sleepAlerter) settle(ctx context.Context, n notify.Notifier, now time.Time, logf func(string, ...any)) {
	st, ok := n.(*alertStore)
	if !ok {
		return
	}
	if last, has := st.latest(sleepEventKey); has && now.Sub(last.At) < sleepQuiet {
		return
	}
	fireAlert(ctx, n, logf, notify.Alert{Key: sleepsKey, Kind: notify.Resolved,
		Title: "Briard: this computer has not slept for a week"})
}
