package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"briard.io/shared/routes"
)

// backend stands in for a service behind the door, with Home Assistant's forwarded-header rule:
// X-Forwarded-For from a proxy it does not trust is a 400. unready is how many plain requests it
// answers 503 before it is up.
type backend struct {
	srv     *httptest.Server
	trusts  bool
	mu      sync.Mutex
	unready int
	early   int    // header-carrying requests seen while not yet up: a probe that did not wait
	gotXFF  string // the header on the last forwarded request
}

func newBackend(t *testing.T, trusts bool, unready int) *backend {
	b := &backend{trusts: trusts, unready: unready}
	b.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b.mu.Lock()
		defer b.mu.Unlock()
		xff := r.Header.Get("X-Forwarded-For")
		if b.unready > 0 {
			if xff != "" {
				b.early++
			} else {
				b.unready--
			}
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		if xff != "" && !b.trusts {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		b.gotXFF = xff
		fmt.Fprint(w, "payload-ok")
	}))
	t.Cleanup(b.srv.Close)
	return b
}

func (b *backend) xff() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.gotXFF
}

func fastProbes(t *testing.T) {
	every, wait := probeEvery, probeWaitFor
	probeEvery, probeWaitFor = 5*time.Millisecond, time.Second
	t.Cleanup(func() { probeEvery, probeWaitFor = every, wait })
}

// settled waits for the door to have an answer for service, which is the probe having finished.
func settled(t *testing.T, f *frontDoor, service string) {
	t.Helper()
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		f.trust.mu.RLock()
		_, ok := f.trust.trusted[service]
		f.trust.mu.RUnlock()
		if ok {
			return
		}
	}
	t.Fatalf("no trust answer for %s", service)
}

func getXFF(t *testing.T, base, host, xff string) int {
	t.Helper()
	req, _ := http.NewRequest("GET", base+"/", nil)
	req.Host = host
	req.Header.Set("X-Forwarded-For", xff)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// The default household: Home Assistant trusts no proxy, so the door must keep sending nothing
// and keep working. The household that added 127.0.0.1: the door tells it who the client is --
// the peer the door saw, never what the client claimed.
func TestForwardedForOnlyToAServiceThatAcceptsIt(t *testing.T) {
	fastProbes(t)
	refuser, accepter := newBackend(t, false, 0), newBackend(t, true, 0)
	f := door(fixed(routes.Table{Services: []routes.Service{
		fronted("refuser", "refuser.test", refuser.srv.URL),
		fronted("accepter", "accepter.test", accepter.srv.URL),
	}}))
	srv := httptest.NewServer(f)
	defer srv.Close()

	f.trust.recheck(f.routes())
	settled(t, f, "refuser")
	settled(t, f, "accepter")

	if code := getXFF(t, srv.URL, "refuser.test", "6.6.6.6"); code != http.StatusOK {
		t.Errorf("a service that refuses the header = %d through the door, want 200", code)
	}
	if got := refuser.xff(); got != "" {
		t.Errorf("refuser saw X-Forwarded-For %q, want none", got)
	}
	if code := getXFF(t, srv.URL, "accepter.test", "6.6.6.6"); code != http.StatusOK {
		t.Fatalf("a service that accepts the header = %d through the door, want 200", code)
	}
	if got := accepter.xff(); got != "127.0.0.1" {
		t.Errorf("accepter saw X-Forwarded-For %q, want the peer the door saw (127.0.0.1), never the client's claim", got)
	}
}

// The agent signals BEFORE the service has opened a socket, so the probe must wait for a plain
// answer first: a header sent to a service that is still starting would read its 503 as a verdict.
func TestTheProbeWaitsForTheServiceToComeUp(t *testing.T) {
	fastProbes(t)
	b := newBackend(t, true, 5)
	f := door(fixed(routes.Table{Services: []routes.Service{fronted("svc", "svc.test", b.srv.URL)}}))
	f.trust.recheck(f.routes())
	settled(t, f, "svc")
	if !f.trust.allows("svc") {
		t.Error("a service that accepts the header once up was marked as refusing it")
	}
	if b.early != 0 {
		t.Errorf("%d header probes reached the service before it answered plainly", b.early)
	}
}

// No answer is not "no": a service that never comes up within the wait keeps what the door
// already knew, so a slow start never takes a household's client addresses away.
func TestNoAnswerKeepsTheLastOne(t *testing.T) {
	fastProbes(t)
	probeWaitFor = 50 * time.Millisecond
	b := newBackend(t, false, 1<<30)
	f := door(fixed(routes.Table{Services: []routes.Service{fronted("svc", "svc.test", b.srv.URL)}}))
	f.trust.set("svc", true)
	f.trust.recheck(f.routes())
	time.Sleep(200 * time.Millisecond)
	if !f.trust.allows("svc") {
		t.Error("a service that never answered lost the trust it had")
	}
}
