package sdnotify

import "sync"

// On Windows the service manager is the SCM, and READY=1 is SERVICE_RUNNING. The SCM is not told
// by a datagram but by status reports that belong to the process's service handler (the agent's
// main); this half only carries the moment READY is reached across to it, so the agent's one call
// site stays the same on both systems and still means "the agent started", never "the node is
// healthy".
var (
	ready     = make(chan struct{})
	readyOnce sync.Once
)

// Adopt has no socket to take.
func Adopt() {}

// Ready marks the agent started. Idempotent, like a second READY=1.
func Ready() error {
	readyOnce.Do(func() { close(ready) })
	return nil
}

// Readied is closed once Ready has been called -- what the service handler waits on before it
// reports SERVICE_RUNNING.
func Readied() <-chan struct{} { return ready }

// Watchdog has no SCM equivalent; the agent's beat is read in-process there.
func Watchdog() error { return nil }

// Notify has no SCM equivalent.
func Notify(string) error { return nil }
