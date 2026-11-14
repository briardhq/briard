package main

import (
	"context"
	"log"
	"net/http"
	"sync"
	"time"

	"briard.io/shared/routes"
)

// forwardTrust is what the reverse proxy knows about each routed app's willingness to be told who
// the client is: whether a request carrying X-Forwarded-For is accepted from this proxy. It starts
// empty, meaning no header for anyone: an app not expecting a proxy must keep working behind one,
// and Home Assistant answers 400 to the header from a proxy it has not been told to trust.
//
// It is learned, never configured: we do not own the app's configuration, so the reverse proxy
// asks the app. A household that trusts 127.0.0.1 in Home Assistant gets real client addresses
// (login tracking, ip_ban, trusted_networks), and one that does not loses nothing.
//
// It is asked only when an app starts, since an app reads its proxy trust at start: the guest
// agent sends SIGHUP on every start it sees (the container's own and Home Assistant's restarts
// inside it), and the reverse proxy asks once at its own start. No timer: each refused probe is
// an error line in the app's log, and one per start is all a household that never opted in pays.
type forwardTrust struct {
	mu      sync.RWMutex
	trusted map[string]bool
	cancel  context.CancelFunc // the recheck in flight, superseded by the next
}

// Probe cadence. The wait is for an app that is starting -- the agent signals before the
// app has opened a socket -- and is bounded by how long a cold Home Assistant can take.
var (
	probeEvery   = 2 * time.Second
	probeWaitFor = 10 * time.Minute
	probeClient  = &http.Client{Timeout: 5 * time.Second}
)

// probeClientAddr is the client the probe claims to be forwarding for: a documentation address
// (RFC 5737), so an app that logs it logs something nobody can mistake for a household device.
const probeClientAddr = "192.0.2.1"

func (t *forwardTrust) allows(app string) bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.trusted[app]
}

func (t *forwardTrust) set(app string, ok bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.trusted == nil {
		t.trusted = map[string]bool{}
	}
	if t.trusted[app] != ok {
		log.Printf("reverse-proxy: %s %s X-Forwarded-For from this proxy", app, map[bool]string{true: "accepts", false: "refuses"}[ok])
	}
	t.trusted[app] = ok
}

// recheck asks every routed app again, cancelling a recheck still waiting on an app that
// has not come up. It returns at once; the answers land as each app does.
func (t *forwardTrust) recheck(r *routing) {
	ctx, cancel := context.WithCancel(context.Background())
	t.mu.Lock()
	if t.cancel != nil {
		t.cancel()
	}
	t.cancel = cancel
	t.mu.Unlock()
	for _, s := range r.table.Apps {
		if _, routed := s.Route(routes.ListenName); !routed || s.Health == "" {
			continue
		}
		u, err := s.Resolve(s.Health)
		if err != nil {
			continue
		}
		go func(name, url string) {
			if ok, known := probeForwarded(ctx, url); known {
				t.set(name, ok)
			}
		}(s.Name, u.String())
	}
}

// probeForwarded waits for the app to answer its health endpoint plainly, then asks the same
// endpoint again with the header. known is false when no answer was reached -- the app never
// came up, or the recheck was superseded -- and the caller then keeps what it had.
//
// The plain answer is what makes a refusal an answer: without it, an app still starting (a 503,
// a refused connection) would read as "does not trust us", and so would every transient error.
func probeForwarded(ctx context.Context, url string) (ok, known bool) {
	deadline := time.Now().Add(probeWaitFor)
	for {
		if code, err := probe(ctx, url, false); err == nil && code >= 200 && code < 300 {
			break
		}
		if time.Now().After(deadline) {
			return false, false
		}
		select {
		case <-ctx.Done():
			return false, false
		case <-time.After(probeEvery):
		}
	}
	code, err := probe(ctx, url, true)
	if err != nil {
		return false, false
	}
	return code != http.StatusBadRequest, true
}

func probe(ctx context.Context, url string, forwarded bool) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	if forwarded {
		req.Header.Set("X-Forwarded-For", probeClientAddr)
	}
	resp, err := probeClient.Do(req)
	if err != nil {
		return 0, err
	}
	resp.Body.Close()
	return resp.StatusCode, nil
}
