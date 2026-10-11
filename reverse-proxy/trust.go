package main

import (
	"context"
	"log"
	"net/http"
	"sync"
	"time"

	"briard.io/shared/routes"
)

// forwardTrust is what the door knows about each routed service's willingness to be told who the
// client is: whether a request carrying X-Forwarded-For is accepted from this door. It starts
// empty, which means NO header for anyone -- a service that is not expecting a proxy must keep
// working when put behind one, and Home Assistant answers 400 to the header from a proxy it has
// not been told to trust.
//
// IT IS LEARNED, NEVER CONFIGURED. We do not own the service's configuration, so the door asks the
// service instead: a household that trusts 127.0.0.1 in Home Assistant gets real client addresses
// (login tracking, ip_ban, trusted_networks), and one that does not loses nothing.
//
// IT IS ASKED ONLY WHEN A SERVICE STARTS. A service reads its proxy trust at start, so a start is
// the only moment the answer can change; the guest agent sends SIGHUP on every start it sees (the
// container's own and Home Assistant's restarts inside it), and the door asks once at its own
// start. No timer: each refused probe is an error line in the service's log, one per start is
// what a household that never opted in pays, and nothing more.
type forwardTrust struct {
	mu      sync.RWMutex
	trusted map[string]bool
	cancel  context.CancelFunc // the recheck in flight, superseded by the next
}

// Probe cadence. The wait is for a service that is starting -- the agent signals before the
// service has opened a socket -- and is bounded by how long a cold Home Assistant can take.
var (
	probeEvery   = 2 * time.Second
	probeWaitFor = 10 * time.Minute
	probeClient  = &http.Client{Timeout: 5 * time.Second}
)

// probeClientAddr is the client the probe claims to be forwarding for: a documentation address
// (RFC 5737), so a service that logs it logs something nobody can mistake for a household device.
const probeClientAddr = "192.0.2.1"

func (t *forwardTrust) allows(service string) bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.trusted[service]
}

func (t *forwardTrust) set(service string, ok bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.trusted == nil {
		t.trusted = map[string]bool{}
	}
	if t.trusted[service] != ok {
		log.Printf("reverse-proxy: %s %s X-Forwarded-For from this door", service, map[bool]string{true: "accepts", false: "refuses"}[ok])
	}
	t.trusted[service] = ok
}

// recheck asks every routed service again, cancelling a recheck still waiting on a service that
// has not come up. It returns at once; the answers land as each service does.
func (t *forwardTrust) recheck(r *routing) {
	ctx, cancel := context.WithCancel(context.Background())
	t.mu.Lock()
	if t.cancel != nil {
		t.cancel()
	}
	t.cancel = cancel
	t.mu.Unlock()
	for _, s := range r.table.Services {
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

// probeForwarded waits for the service to answer its health endpoint plainly, then asks the same
// endpoint again with the header. known is false when no answer was reached -- the service never
// came up, or the recheck was superseded -- and the caller then keeps what it had.
//
// THE PLAIN ANSWER IS WHAT MAKES A REFUSAL AN ANSWER. Without it, a service still starting (a 503,
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
