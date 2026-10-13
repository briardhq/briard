package host

import "time"

// lastSleep is unknown on Linux: the agent holds no sleep off there, so it reports none.
func lastSleep() (slept, woke time.Time, ok bool) { return time.Time{}, time.Time{}, false }

// lidAction is unknown on Linux, for the same reason.
func lidAction() string { return "" }
