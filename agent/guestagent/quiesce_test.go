package guestagent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"briard.io/agent/hass"
	"briard.io/agent/quadlet"
)

// haStub is Home Assistant as the quiesced take reaches it: the token exchange, and the view that
// holds the recorder still. `held` is what its release answers, `code` refuses outright.
type haStub struct {
	code  int
	held  bool
	holds []bool
}

func (h *haStub) start(t *testing.T) int {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/auth/token", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"access_token":"acc","token_type":"Bearer","expires_in":1800}`))
	})
	mux.HandleFunc("/api/briard/quiesce", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Hold bool `json:"hold"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		h.holds = append(h.holds, body.Hold)
		if h.code != 0 {
			w.WriteHeader(h.code)
			return
		}
		if body.Hold {
			w.Write([]byte(`{"held":true}`))
			return
		}
		json.NewEncoder(w).Encode(map[string]bool{"held": h.held})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatal(err)
	}
	return port
}

// quiesceRig is a guest whose Home Assistant answers at `port`, with the control token on tmpfs
// where connect() reads it.
func quiesceRig(t *testing.T, service string, port int) *fakeExec {
	t.Helper()
	f := ringExec()
	f.files[hass.TokenPath] = "tok\n"
	f.files[manifestPath(service)] = `{"name":"` + service + `","version":"2026.7.1","containers":[{"name":"app",` +
		`"image":"ghcr.io/x/ha@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",` +
		`"mount":"/config","primary":true,"port":` + strconv.Itoa(port) + `,"healthPath":"/"}]}`
	return f
}

func nightlySidecar(service string) string {
	raw, _ := json.Marshal(quadlet.SnapshotMeta{
		Service: service, Trigger: quadlet.TriggerDaily,
		TakenAt: time.Now(), Consistency: quadlet.Crash, Manifest: `{"name":"` + service + `"}`,
	})
	return string(raw)
}

func take(t *testing.T, f *fakeExec, service string) (quiescedResult, error) {
	t.Helper()
	run := func(name string, args ...string) error {
		_, err := f.Run(context.Background(), name, args...)
		return err
	}
	member := quadlet.SnapshotMember(service, quadlet.TriggerDaily, time.Now())
	return quiescedMember(context.Background(), f, run, snapshotRequest{
		Service: service, DataDir: quadlet.DataRoot(service), Path: member, Sidecar: nightlySidecar(service),
	})
}

func sidecarOf(t *testing.T, f *fakeExec) quadlet.SnapshotMeta {
	t.Helper()
	for path, body := range f.files {
		if !strings.HasSuffix(path, ".json") || !strings.HasPrefix(path, quadlet.SnapshotsDir) {
			continue
		}
		var meta quadlet.SnapshotMeta
		if err := json.Unmarshal([]byte(body), &meta); err != nil {
			t.Fatalf("the member's sidecar does not parse: %v", err)
		}
		return meta
	}
	t.Fatalf("no member sidecar was written: %v", f.files)
	return quadlet.SnapshotMeta{}
}

// THE NIGHTLY, QUIESCED ([B.143]): hold, snapshot, release — and the class is written from what
// the RELEASE said, because Home Assistant is the only party that knows whether its lock survived.
func TestQuiescedMemberUpgradesTheClassWhenTheServiceHeld(t *testing.T) {
	h := &haStub{held: true}
	f := quiesceRig(t, hass.Name, h.start(t))
	res, err := take(t, f, hass.Name)
	if err != nil {
		t.Fatalf("quiescedMember: %v", err)
	}
	if !res.Held {
		t.Errorf("result = %+v, want held", res)
	}
	if got := sidecarOf(t, f).Consistency; got != quadlet.Quiesced {
		t.Errorf("consistency = %q, want quiesced", got)
	}
	// THE ORDER IS THE PROPERTY: the lock is taken before the snapshot and released after it, or
	// the snapshot is of a database that was never held still.
	var holdAt, snapAt, releaseAt = -1, -1, -1
	for i, r := range f.runs {
		if len(r) > 3 && r[1] == "subvolume" && r[2] == "snapshot" {
			snapAt = i
		}
	}
	for i, hold := range h.holds {
		if hold && holdAt < 0 {
			holdAt = i
		}
		if !hold {
			releaseAt = i
		}
	}
	if holdAt != 0 || releaseAt != 1 || snapAt < 0 {
		t.Errorf("calls were holds=%v runs=%v, want hold -> snapshot -> release", h.holds, f.runs)
	}
}

// AND IT DOES NOT UPGRADE WHEN THE LOCK BROKE. Home Assistant resumes writing if the events it is
// buffering pile up; the member is still taken, and it says what it is.
func TestQuiescedMemberKeepsCrashWhenTheLockDidNotHold(t *testing.T) {
	h := &haStub{held: false}
	f := quiesceRig(t, hass.Name, h.start(t))
	res, err := take(t, f, hass.Name)
	if err != nil {
		t.Fatalf("quiescedMember: %v", err)
	}
	if res.Held {
		t.Error("the member claimed the service held still when it did not")
	}
	if got := sidecarOf(t, f).Consistency; got != quadlet.Crash {
		t.Errorf("consistency = %q, want crash", got)
	}
	if res.Why == "" {
		t.Error("nothing said why the member is crash-consistent")
	}
}

// A SERVICE THAT CANNOT BE ASKED STILL GETS ITS MEMBER. A Home Assistant that is down, an older
// integration with no such view, a service with no way to hold still at all — the nightly happens
// and the member says crash-consistent. A hole in the ring would be the worse answer.
func TestQuiescedMemberTakesOneEvenWhenNothingCanHoldStill(t *testing.T) {
	for name, f := range map[string]*fakeExec{
		"home assistant refuses": quiesceRig(t, hass.Name, (&haStub{code: http.StatusNotFound}).start(t)),
		"home assistant is down": quiesceRig(t, hass.Name, 1),
		"a service with no way":  quiesceRig(t, "mosquitto", (&haStub{held: true}).start(t)),
	} {
		service := hass.Name
		if strings.Contains(name, "no way") {
			service = "mosquitto"
		}
		res, err := take(t, f, service)
		if err != nil {
			t.Errorf("%s: quiescedMember: %v", name, err)
			continue
		}
		if res.Held {
			t.Errorf("%s: claimed the service held still", name)
		}
		if got := sidecarOf(t, f).Consistency; got != quadlet.Crash {
			t.Errorf("%s: consistency = %q, want crash", name, got)
		}
		if res.Why == "" {
			t.Errorf("%s: nothing said why", name)
		}
	}
}

// TestQuiescedMemberReleasesEvenWhenTheSnapshotFails: a lock nobody releases is released by Home
// Assistant itself, but only after its backlog check notices — up to ten seconds of a household's
// events buffered for nothing.
func TestQuiescedMemberReleasesEvenWhenTheSnapshotFails(t *testing.T) {
	h := &haStub{held: true}
	f := quiesceRig(t, hass.Name, h.start(t))
	inner := f.runFn
	f.runFn = func(name string, args []string) ([]byte, error) {
		if len(args) > 2 && args[0] == "subvolume" && args[1] == "snapshot" {
			return nil, errors.New("No space left on device")
		}
		return inner(name, args)
	}
	if _, err := take(t, f, hass.Name); err == nil {
		t.Fatal("a failed snapshot was reported as a member taken")
	}
	if len(h.holds) != 2 || !h.holds[0] || h.holds[1] {
		t.Errorf("calls were %v, want the hold released after the failure", h.holds)
	}
}
