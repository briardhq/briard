package notify

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Ntfy posts the body as the message and the title/priority/tags as headers (ntfy's HTTP
// contract), with warning-level mapping to high priority.
func TestNtfyPostsHeaders(t *testing.T) {
	var title, priority, tags, body, method string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method = r.Method
		title, priority, tags = r.Header.Get("Title"), r.Header.Get("Priority"), r.Header.Get("Tags")
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	err := Ntfy(srv.URL).Notify(context.Background(), Alert{Level: Warning, Title: "reduced redundancy", Body: "node n1 lost a replica"})
	if err != nil {
		t.Fatal(err)
	}
	if method != http.MethodPost {
		t.Errorf("method = %q, want POST", method)
	}
	if title != "reduced redundancy" || body != "node n1 lost a replica" {
		t.Errorf("title/body = %q / %q", title, body)
	}
	if priority != "high" || tags != "warning" {
		t.Errorf("warning must map to priority=high tags=warning, got %q / %q", priority, tags)
	}
}

// A non-2xx from ntfy surfaces as an error (the caller logs it; alerting is best-effort).
func TestNtfyErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	if err := Ntfy(srv.URL).Notify(context.Background(), Alert{Level: Recovered, Title: "x"}); err == nil {
		t.Error("expected an error on a 500 from ntfy")
	}
}

// Nop delivers nowhere and never errors (the default when no endpoint is configured).
func TestNopNotifier(t *testing.T) {
	if err := Nop().Notify(context.Background(), Alert{Level: Warning}); err != nil {
		t.Errorf("Nop must not error: %v", err)
	}
}

// The Log notifier records the alert (an alternative log-based sink).
func TestLogNotifier(t *testing.T) {
	var logged string
	err := Log(func(f string, a ...any) { logged = fmt.Sprintf(f, a...) }).
		Notify(context.Background(), Alert{Level: Warning, Title: "T", Body: "B"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logged, "T") || !strings.Contains(logged, "B") || !strings.Contains(logged, "warning") {
		t.Errorf("log line missing content: %q", logged)
	}
}

// captureNotifier records the alerts it receives (and optionally fails) -- a test double for Tee.
type captureNotifier struct {
	got  []Alert
	fail error
}

func (c *captureNotifier) Notify(_ context.Context, a Alert) error {
	c.got = append(c.got, a)
	return c.fail
}

// Tee fans one alert out to every notifier and joins their errors -- one dead channel must not
// suppress delivery to the others (the operator copy still goes even if the owner email fails).
func TestTeeFansOutAndJoinsErrors(t *testing.T) {
	ok1, ok2 := &captureNotifier{}, &captureNotifier{}
	bad := &captureNotifier{fail: fmt.Errorf("boom")}
	al := Alert{Level: Warning, Title: "T", Body: "B"}

	err := Tee(ok1, bad, ok2).Notify(context.Background(), al)
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("Tee error = %v, want it to carry the failed channel's error", err)
	}
	// Every channel still received the alert -- the failure didn't short-circuit the others.
	for i, c := range []*captureNotifier{ok1, bad, ok2} {
		if len(c.got) != 1 || c.got[0] != al {
			t.Errorf("channel %d got %+v, want the one alert", i, c.got)
		}
	}
	// All-success Tee returns nil.
	if err := Tee(ok1, ok2).Notify(context.Background(), al); err != nil {
		t.Errorf("all-success Tee err = %v, want nil", err)
	}
}
