package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"briard.io/agent/quadlet"
	"briard.io/shared/api"
)

// ring is what the host answers `service-members` with: two points, an older one pinned to an
// older version of the app and taken while it was running, and a clean newer one.
func ring() string {
	manifestOf := func(v string) string {
		return `{"name":"home-assistant","version":"` + v + `","containers":[{"name":"app",` +
			`"image":"ghcr.io/x/ha@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",` +
			`"mount":"/config","primary":true,"port":8123,"healthPath":"/"}]}`
	}
	older := time.Date(2026, 9, 20, 3, 0, 0, 0, time.Local)
	entries := []quadlet.SnapshotEntry{
		{
			Member: quadlet.SnapshotMember("home-assistant", quadlet.TriggerDaily, older),
			Meta: quadlet.SnapshotMeta{Service: "home-assistant", Trigger: quadlet.TriggerDaily,
				Title: "home-assistant nightly", TakenAt: older, Consistency: quadlet.Crash,
				Manifest: manifestOf("2026.6.0")},
		},
		{
			Member: quadlet.SnapshotMember("home-assistant", quadlet.TriggerUpgrade, older.Add(24*time.Hour)),
			Meta: quadlet.SnapshotMeta{Service: "home-assistant", Trigger: quadlet.TriggerUpgrade,
				Title: "2026.7.1, before upgrading to 2026.8.0", TakenAt: older.Add(24 * time.Hour),
				Consistency: quadlet.Quiesced, Manifest: manifestOf("2026.7.1")},
		},
	}
	raw, _ := json.Marshal(entries)
	return string(raw)
}

// postForm submits one of the picker's forms.
func postForm(t *testing.T, r *rig, path string, c *http.Cookie, form url.Values) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("POST", r.srv.URL+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if c != nil {
		req.AddCookie(c)
	}
	resp, err := noRedirect.Do(req)
	must(t, err)
	return resp
}

func bodyOf(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

// answerMembers lets the fake port answer the next service-members with the ring above.
func answerMembers(port *fakePort) {
	go func() {
		for range port.waiting {
			port.answer <- api.DirectiveOutcome{State: api.OutcomeDone, Detail: ring()}
		}
	}()
}

// THE PICKER ([B.143]): the page lists what the HOST says the ring holds, and says the two things
// a list of times cannot -- which points the app has to recover from, and which ones move its
// version as well as its data.
func TestPickerListsWhatTheHostReports(t *testing.T) {
	r := newRig(t)
	c := r.trust()
	port := newFakePort()
	r.app.port = port
	answerMembers(port)

	resp := r.do("GET", "/history/home-assistant", c, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("history = %d, want 200", resp.StatusCode)
	}
	req, _ := http.NewRequest("GET", r.srv.URL+"/history/home-assistant", nil)
	req.AddCookie(c)
	got, err := http.DefaultClient.Do(req)
	must(t, err)
	body := bodyOf(t, got)

	for _, want := range []string{
		"2026.7.1, before upgrading to 2026.8.0", // the titles the host gave, not re-derived here
		"home-assistant nightly",
		"taken while the app was running", // the nightly's class, in the CLI's words
		"also changes the version",        // the older point runs an older app
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the page does not say %q:\n%s", want, body)
		}
	}
	// NEWEST FIRST on a page, which is the one place this differs from the CLI's listing.
	if strings.Index(body, "before upgrading") > strings.Index(body, "nightly") {
		t.Errorf("the points are oldest-first on a page that is read from the top:\n%s", body)
	}
	// The clean point carries no note at all: a note on every line is a note nobody reads.
	if strings.Count(body, "taken while the app was running") != 1 {
		t.Errorf("the clean point was annotated too:\n%s", body)
	}
	port.mu.Lock()
	asked := append([]api.Directive(nil), port.asked...)
	port.mu.Unlock()
	for _, d := range asked {
		if d.Kind != api.DirectiveServiceMembers {
			t.Errorf("listing the points asked the host for %q", d.Kind)
		}
	}
}

// TestPickerConfirmsBeforePuttingAPointBack: a restore discards everything since the point,
// including -- on a mis-click -- the afternoon the household actually wanted. The first press
// must ask nothing of the host.
func TestPickerConfirmsBeforePuttingAPointBack(t *testing.T) {
	r := newRig(t)
	c := r.trust()
	port := newFakePort()
	r.app.port = port
	answerMembers(port)
	member := quadlet.SnapshotMember("home-assistant", quadlet.TriggerUpgrade,
		time.Date(2026, 9, 21, 3, 0, 0, 0, time.Local))

	resp := postForm(t, r, "/revert", c, url.Values{"member": {member}})
	body := bodyOf(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("first press = %d, want the confirmation page", resp.StatusCode)
	}
	if !strings.Contains(body, "before upgrading") || !strings.Contains(body, "discarded") {
		t.Errorf("the confirmation does not name the point and what it costs:\n%s", body)
	}
	if !strings.Contains(body, `name="confirm" value="yes"`) {
		t.Errorf("the confirmation has no way to go through with it:\n%s", body)
	}
	port.mu.Lock()
	for _, d := range port.asked {
		if d.Kind == api.DirectiveServiceRestore {
			t.Errorf("the first press already asked the host to restore: %+v", d)
		}
	}
	port.mu.Unlock()

	// CONFIRMED: exactly one service-restore, naming the point VERBATIM -- never an index into a
	// listing that anything taking a member makes stale.
	resp = postForm(t, r, "/revert", c, url.Values{"member": {member}, "confirm": {"yes"}, "title": {"x"}})
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("confirmed press = %d, want 303", resp.StatusCode)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		port.mu.Lock()
		asked := append([]api.Directive(nil), port.asked...)
		port.mu.Unlock()
		for _, d := range asked {
			if d.Kind == api.DirectiveServiceRestore {
				if d.Payload != member {
					t.Errorf("payload = %q, want the point verbatim %q", d.Payload, member)
				}
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the confirmed press never reached the host")
}

// TestPickerRefusesAPointThatIsGone: pruning runs on every take, so a listing a browser has been
// looking at for a while can name a point that no longer exists. The confirmation re-reads the
// ring rather than trusting the form.
func TestPickerRefusesAPointThatIsGone(t *testing.T) {
	r := newRig(t)
	c := r.trust()
	port := newFakePort()
	r.app.port = port
	answerMembers(port)
	gone := quadlet.SnapshotMember("home-assistant", quadlet.TriggerStart, time.Date(2020, 1, 1, 0, 0, 0, 0, time.Local))
	resp := postForm(t, r, "/revert", c, url.Values{"member": {gone}})
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("a pruned point = %d, want 404", resp.StatusCode)
	}
	port.mu.Lock()
	defer port.mu.Unlock()
	for _, d := range port.asked {
		if d.Kind == api.DirectiveServiceRestore {
			t.Errorf("a pruned point was relayed to the host anyway: %+v", d)
		}
	}
}

// TestPickerNeedsATrustedBrowser: reaching this port is not authentication ([V3b.31a](a)), and
// these two routes read a household's history and can discard part of it.
func TestPickerNeedsATrustedBrowser(t *testing.T) {
	r := newRig(t)
	port := newFakePort()
	r.app.port = port
	if resp := r.do("GET", "/history/home-assistant", nil, nil); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("history with no session = %d, want 401", resp.StatusCode)
	}
	resp := postForm(t, r, "/revert", nil, url.Values{"member": {"x"}})
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("revert with no session = %d, want 401", resp.StatusCode)
	}
	port.mu.Lock()
	defer port.mu.Unlock()
	if len(port.asked) != 0 {
		t.Errorf("an untrusted browser reached the host: %+v", port.asked)
	}
}

// TestPickerRefusesSomethingThatIsNotAPoint: the member arrives in a form field, and it becomes a
// path the host acts on. A name this machine did not take is refused before anything is relayed.
func TestPickerRefusesSomethingThatIsNotAPoint(t *testing.T) {
	r := newRig(t)
	c := r.trust()
	port := newFakePort()
	r.app.port = port
	for _, bad := range []string{"", "/etc/passwd", "../../x", "home-assistant"} {
		resp := postForm(t, r, "/revert", c, url.Values{"member": {bad}})
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("member %q = %d, want 400", bad, resp.StatusCode)
		}
	}
	port.mu.Lock()
	defer port.mu.Unlock()
	if len(port.asked) != 0 {
		t.Errorf("a name that is not a point reached the host: %+v", port.asked)
	}
}
