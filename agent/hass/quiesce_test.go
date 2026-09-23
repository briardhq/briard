package hass

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
)

// quiesceStub is Home Assistant's side of the hold: the token exchange the channel starts with,
// and the view the pair calls. It records what arrived, because each field is load-bearing against
// the real view — the path, the admin-gated Bearer, and a body whose `hold` is a BOOLEAN, which
// the view refuses if it is anything else.
type quiesceStub struct {
	code   int  // non-200 to refuse
	held   bool // what the release answers
	holds  []bool
	bearer string
	path   string
}

func (q *quiesceStub) start(t *testing.T) int {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/auth/token", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil || r.Form.Get("refresh_token") != "tok" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Write([]byte(`{"access_token":"acc","token_type":"Bearer","expires_in":1800}`))
	})
	mux.HandleFunc(quiescePath, func(w http.ResponseWriter, r *http.Request) {
		q.path, q.bearer = r.URL.Path, r.Header.Get("Authorization")
		var body struct {
			Hold *bool `json:"hold"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Hold == nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		q.holds = append(q.holds, *body.Hold)
		if q.code != 0 {
			w.WriteHeader(q.code)
			// ⚠️ A BODY THAT PARSES, deliberately. Home Assistant's own json_message answers an
			// error with JSON, and a stub that sent nothing would let the DECODER catch every
			// refusal — leaving the status check able to be deleted with every case still green.
			w.Write([]byte(`{"held":true,"message":"refused"}`))
			return
		}
		if *body.Hold {
			w.Write([]byte(`{"held":true}`))
			return
		}
		json.NewEncoder(w).Encode(map[string]bool{"held": q.held})
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

// TestQuiesceHoldsAndReleasesThroughTheSameToken is the pair end to end, minus Home Assistant.
// Every field is asserted for the same reason Nudge's are: the path names the view, the Bearer is
// what an admin-gated view requires, and `hold` has to be a boolean or the view answers 400.
func TestQuiesceHoldsAndReleasesThroughTheSameToken(t *testing.T) {
	q := &quiesceStub{held: true}
	port := q.start(t)
	x := tokenFile("tok\n")
	if err := Hold(context.Background(), x, port); err != nil {
		t.Fatalf("Hold: %v", err)
	}
	held, err := Release(context.Background(), x, port)
	if err != nil {
		t.Fatalf("Release: %v", err)
	}
	if !held {
		t.Error("Release reported the lock broken when Home Assistant said it held")
	}
	if q.path != quiescePath {
		t.Errorf("called %q, want %q", q.path, quiescePath)
	}
	if q.bearer != "Bearer acc" {
		t.Errorf("Authorization = %q, want the exchanged access token", q.bearer)
	}
	// THE ORDER AND THE SHAPE: a hold then a release, never two holds, and each carrying the
	// boolean the view reads.
	if len(q.holds) != 2 || !q.holds[0] || q.holds[1] {
		t.Errorf("the calls were %v, want [true false]", q.holds)
	}
}

// TestReleaseReportsALockThatDidNotHold is the whole reason this is a pair rather than a fire and
// forget ([B.143]). Home Assistant breaks its own lock if the events it is buffering pile up, and
// it says so on the release — a member taken under a broken lock is crash-consistent, and the
// caller may only learn that here.
func TestReleaseReportsALockThatDidNotHold(t *testing.T) {
	q := &quiesceStub{held: false}
	port := q.start(t)
	x := tokenFile("tok")
	if err := Hold(context.Background(), x, port); err != nil {
		t.Fatalf("Hold: %v", err)
	}
	held, err := Release(context.Background(), x, port)
	if err != nil {
		t.Fatalf("Release: %v", err)
	}
	if held {
		t.Error("Release claimed the lock held when Home Assistant said it broke it")
	}
}

// TestQuiesceReportsARefusal: a Home Assistant too old for the view (404), one whose recorder is
// absent (501), or a lock that timed out (503) must not read as a successful hold. The caller's
// fallback is the same either way — take the member unquiesced — but a silent success would label
// that member as something it is not.
func TestQuiesceReportsARefusal(t *testing.T) {
	for _, code := range []int{http.StatusNotFound, http.StatusNotImplemented, http.StatusServiceUnavailable, http.StatusConflict} {
		q := &quiesceStub{code: code}
		port := q.start(t)
		if err := Hold(context.Background(), tokenFile("tok"), port); err == nil {
			t.Errorf("HTTP %d was reported as a successful hold", code)
		}
		if held, err := Release(context.Background(), tokenFile("tok"), port); err == nil || held {
			t.Errorf("HTTP %d was reported as a lock that held", code)
		}
	}
}
