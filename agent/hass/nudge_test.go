package hass

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

// eventStub is Home Assistant's inbound half: the exchange the whole channel starts with, and the
// event view Nudge fires at. It records what actually arrived, because every one of those fields
// is part of the contract with a real Home Assistant — the path names the event, the view is
// admin-gated so the Bearer has to be the exchanged token, and the body has to be a JSON object.
type eventStub struct {
	code     int // non-200 to refuse the event
	path     string
	method   string
	bearer   string
	ctype    string
	body     string
	fired    int
	tokenFor *haStub
}

func (e *eventStub) start(t *testing.T) int {
	t.Helper()
	e.tokenFor = &haStub{token: "tok"}
	mux := http.NewServeMux()
	mux.HandleFunc("/auth/token", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil || r.Form.Get("refresh_token") != e.tokenFor.token {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Write([]byte(`{"access_token":"acc","token_type":"Bearer","expires_in":1800}`))
	})
	mux.HandleFunc("/api/events/", func(w http.ResponseWriter, r *http.Request) {
		e.fired++
		e.path, e.method = r.URL.Path, r.Method
		e.bearer = r.Header.Get("Authorization")
		e.ctype = r.Header.Get("Content-Type")
		buf := make([]byte, r.ContentLength)
		r.Body.Read(buf)
		e.body = string(buf)
		if e.code != 0 {
			w.WriteHeader(e.code)
			return
		}
		w.Write([]byte(`{"message":"Event ` + EventReconsider + ` fired."}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatal(err)
	}
	return port
}

// TestNudgeFiresTheEventThroughTheSameToken is the push direction end to end, minus Home
// Assistant: read the token off tmpfs, exchange it, and POST briard's one event with the access
// token it got back.
//
// EVERY FIELD IS ASSERTED because every one of them is load-bearing against the real view, and
// three of them fail SILENTLY if they drift — a wrong path fires an event nothing listens for, a
// missing Bearer is a 401 on an admin-gated view, and a body that is not a JSON object is a 400.
func TestNudgeFiresTheEventThroughTheSameToken(t *testing.T) {
	e := &eventStub{}
	port := e.start(t)
	if err := Nudge(context.Background(), tokenFile("tok\n"), port); err != nil {
		t.Fatalf("Nudge: %v", err)
	}
	if e.fired != 1 {
		t.Fatalf("the event was fired %d times, want once", e.fired)
	}
	if want := "/api/events/" + EventReconsider; e.path != want {
		t.Fatalf("fired at %q, want %q", e.path, want)
	}
	if e.method != http.MethodPost {
		t.Fatalf("method = %q, want POST", e.method)
	}
	if e.bearer != "Bearer acc" {
		t.Fatalf("Authorization = %q, want the exchanged access token", e.bearer)
	}
	if e.ctype != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", e.ctype)
	}
	// THE SIGNAL CARRIES NOTHING, which is the design and not an omission: the integration
	// re-derives its world when it wakes, so there is no state here that could be stale or wrong.
	if e.body != "{}" {
		t.Fatalf("the event carried %q, want the empty object", e.body)
	}
}

// TestNudgeReportsARefusedEvent. The caller treats a failure as "Home Assistant will pick this up
// at its next start" and carries on, so the ONE thing this owes is not to report success — a
// silent 403 (our system user no longer admin) would look exactly like a delivered nudge.
func TestNudgeReportsARefusedEvent(t *testing.T) {
	e := &eventStub{code: http.StatusForbidden}
	if err := Nudge(context.Background(), tokenFile("tok"), e.start(t)); err == nil {
		t.Fatal("a refused event was reported as delivered")
	}
}

// TestNudgeWithoutAControlTokenSaysSo: no token means the channel was never materialised on this
// node (Prepare did not run, or /run was cleared under a running guest) — a different fact from
// "Home Assistant refused", and the log line is the only place the difference shows.
func TestNudgeWithoutAControlTokenSaysSo(t *testing.T) {
	e := &eventStub{}
	err := Nudge(context.Background(), &fake{}, e.start(t))
	if err == nil {
		t.Fatal("a guest with no control token reported a delivered nudge")
	}
	if e.fired != 0 {
		t.Fatal("an unauthenticated nudge was sent anyway")
	}
}

// TestBothEndsSpellTheEventTheSameWay. The name is the whole contract between a Go package and a
// Python module that never see each other, and a rename on either side is SILENT: the event is
// fired, Home Assistant accepts it, nothing listens, and the only symptom is a broker that
// quietly stops being offered. The port next door is substituted for exactly this reason; the
// event name is ours on both sides, so a test is the cheaper guard than a placeholder.
func TestBothEndsSpellTheEventTheSameWay(t *testing.T) {
	if !strings.Contains(implSource, `EVENT_RECONSIDER = "`+EventReconsider+`"`) {
		t.Fatalf("the integration does not define %s as %q", "EVENT_RECONSIDER", EventReconsider)
	}
	if !strings.Contains(implSource, "async_listen(EVENT_RECONSIDER") {
		t.Fatal("the integration defines the event but never listens for it")
	}
}
