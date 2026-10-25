package host

import (
	"errors"
	"time"
)

// lastSleep is unknown on Linux: the agent holds no sleep off there, so it reports none.
func lastSleep() (slept, woke time.Time, ok bool) { return time.Time{}, time.Time{}, false }

// lidAction is unknown on Linux, for the same reason.
func lidAction() string { return "" }

// fastStartup is unknown on Linux, for the same reason.
func fastStartup(time.Time) bool { return false }

// coldBootSince is never asked on Linux: no bridge record exists there (restart.go).
func coldBootSince(time.Time) (bool, error) { return false, errors.New("not counted on Linux") }
