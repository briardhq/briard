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

// haStub is a stand-in for Home Assistant's two relevant endpoints. It listens on loopback, which
// is what Readiness talks to, so the test exercises the real URLs, the real form encoding and the
// real Bearer header rather than a mock of them.
type haStub struct {
	token     string // the refresh token it will accept
	entries   string // the body /api/config/config_entries/entry returns
	tokenCode int    // non-200 to refuse the exchange
	entryCode int    // non-200 to refuse the listing
	sawBearer string
}

func (h *haStub) start(t *testing.T) int {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/auth/token", func(w http.ResponseWriter, r *http.Request) {
		if h.tokenCode != 0 {
			w.WriteHeader(h.tokenCode)
			return
		}
		if err := r.ParseForm(); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		// The documented exchange: grant_type=refresh_token, client_id omitted (a system token
		// has none, and HA refuses one that carries it).
		if r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("refresh_token") != h.token {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if r.Form.Has("client_id") {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Write([]byte(`{"access_token":"acc","token_type":"Bearer","expires_in":1800}`))
	})
	mux.HandleFunc("/api/config/config_entries/entry", func(w http.ResponseWriter, r *http.Request) {
		h.sawBearer = r.Header.Get("Authorization")
		if h.entryCode != 0 {
			w.WriteHeader(h.entryCode)
			return
		}
		w.Write([]byte(h.entries))
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

func tokenFile(v string) *fake { return &fake{files: map[string]string{TokenPath: v}} }

// TestReadinessReadsTheStatesThroughTheToken is the channel end to end, minus Home Assistant:
// read the token off tmpfs, exchange it, present the Bearer, and come back with the triple the
// gate reasons over — nothing else from HA's much larger document.
func TestReadinessReadsTheStatesThroughTheToken(t *testing.T) {
	h := &haStub{token: "tok", entries: `[
	  {"entry_id":"1","domain":"hue","state":"loaded","title":"Hue","supports_options":true},
	  {"entry_id":"2","domain":"mqtt","state":"setup_error","title":"MQTT"}]`}
	port := h.start(t)
	got, err := Readiness(context.Background(), tokenFile("tok\n"), port)
	if err != nil {
		t.Fatalf("Readiness: %v", err)
	}
	want := []Entry{{ID: "1", Domain: "hue", State: "loaded"}, {ID: "2", Domain: "mqtt", State: "setup_error"}}
	if len(got) != len(want) {
		t.Fatalf("entries = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("entries[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
	if h.sawBearer != "Bearer acc" {
		t.Fatalf("Authorization = %q, want the exchanged access token", h.sawBearer)
	}
}

// TestReadinessTrimsTheTokenFile: the file is written with a trailing newline (it is read by a
// shell wrapper too), and a token sent with one is a token HA does not know.
func TestReadinessTrimsTheTokenFile(t *testing.T) {
	h := &haStub{token: "tok", entries: `[]`}
	if _, err := Readiness(context.Background(), tokenFile("  tok\n"), h.start(t)); err != nil {
		t.Fatalf("a whitespace-wrapped token was not trimmed: %v", err)
	}
}

// TestReadinessEmptyIsNotAnError: Home Assistant answers its HTTP stack before default_config has
// finished setting entries up, so an early sample is legitimately empty — measured, and it is the
// first thing this gate saw in CI. An empty PRE sample simply excludes everything, which is the
// safe direction; treating it as a failure would have made the gate flap on every fast node.
func TestReadinessEmptyIsNotAnError(t *testing.T) {
	h := &haStub{token: "tok", entries: `[]`}
	got, err := Readiness(context.Background(), tokenFile("tok"), h.start(t))
	if err != nil {
		t.Fatalf("an empty entry list was reported as a failure: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("entries = %+v, want none", got)
	}
}

// TestReadinessSaysWhenThereIsNoToken: the caller's only other reading of a failure here is
// "Home Assistant is broken", and it is not — it is that the control channel was never
// materialised on this node. The log line is the only place that difference is visible.
func TestReadinessSaysWhenThereIsNoToken(t *testing.T) {
	h := &haStub{token: "tok", entries: `[]`}
	_, err := Readiness(context.Background(), &fake{}, h.start(t))
	if err == nil {
		t.Fatal("a missing token was not reported")
	}
	if !strings.Contains(err.Error(), TokenPath) {
		t.Fatalf("the error does not name the missing token: %v", err)
	}
	if _, err := Readiness(context.Background(), tokenFile("   \n"), h.start(t)); err == nil {
		t.Fatal("an empty token file was accepted")
	}
}

// TestReadinessReportsARefusedToken: a 400 on the exchange means HA does not know our token, i.e.
// the mint did not stick. Distinct from "HA is down", and the gate degrades to floor-only on
// both, so the message is what tells them apart afterwards.
func TestReadinessReportsARefusedToken(t *testing.T) {
	h := &haStub{token: "other", entries: `[]`}
	_, err := Readiness(context.Background(), tokenFile("tok"), h.start(t))
	if err == nil {
		t.Fatal("a refused token was not reported")
	}
	if !strings.Contains(err.Error(), "exchange token") {
		t.Fatalf("unhelpful error for a refused token: %v", err)
	}
}

// TestReadinessRefusesABadPort: the port comes from the manifest, and a manifest that named
// something impossible would otherwise turn into a confusing dial error.
func TestReadinessRefusesABadPort(t *testing.T) {
	if _, err := Readiness(context.Background(), tokenFile("tok"), 0); err == nil {
		t.Fatal("port 0 was accepted")
	}
}
