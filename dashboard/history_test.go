package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"briard.io/agent/quadlet"
	"briard.io/agent/services"
	"briard.io/shared/api"
)

// ring is what the host answers `service-members` with: quiet time on an older version
// whose point was taken while the app ran, the update to 2026.8.0 on the point before it, and the
// start after it -- a sample, which anchors nothing and is not a row.
func ring() string {
	manifestOf := func(v string) string {
		return `{"name":"home-assistant","version":"` + v + `","containers":[{"name":"app",` +
			`"image":"ghcr.io/x/ha@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",` +
			`"mount":"/config","primary":true,"port":8123,"healthPath":"/"}]}`
	}
	older := time.Date(2026, 9, 20, 3, 0, 0, 0, time.Local)
	update := older.Add(24 * time.Hour)
	entries := []quadlet.SnapshotEntry{
		{
			Member: quadlet.SnapshotMember("home-assistant", quadlet.TriggerClock, older),
			Meta: quadlet.SnapshotMeta{Service: "home-assistant", Trigger: quadlet.TriggerClock,
				TakenAt: older, Consistency: quadlet.Crash, Manifest: manifestOf("2026.6.0"),
				Event: &quadlet.Event{At: older.Add(20 * time.Hour), Reasons: []quadlet.Reason{{Kind: quadlet.ReasonQuiet}}}},
		},
		{
			Member: quadlet.SnapshotMember("home-assistant", quadlet.TriggerAppUpdateBefore, update),
			Meta: quadlet.SnapshotMeta{Service: "home-assistant", Trigger: quadlet.TriggerAppUpdateBefore,
				TakenAt: update, Consistency: quadlet.Quiesced, Manifest: manifestOf("2026.7.1"),
				Event: &quadlet.Event{At: update, Reasons: []quadlet.Reason{{Kind: quadlet.ReasonAppUpdate, What: "Updated to 2026.8.0"}}}},
		},
		{
			Member: quadlet.SnapshotMember("home-assistant", quadlet.TriggerStart, update.Add(5*time.Minute)),
			Meta: quadlet.SnapshotMeta{Service: "home-assistant", Trigger: quadlet.TriggerStart,
				TakenAt: update.Add(5 * time.Minute), Consistency: quadlet.Quiesced, Manifest: manifestOf("2026.8.0")},
		},
	}
	raw, _ := json.Marshal(entries)
	return string(raw)
}

// postForm submits one of the history page's forms.
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

// THE HISTORY: the page lists the EVENTS the host's ring holds, and says the two
// things a list of times cannot -- which points the app has to recover from, and which ones move
// its version as well as its data. A sample that anchors nothing is not a row.
func TestHistoryListsWhatTheHostReports(t *testing.T) {
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
		"Updated to 2026.8.0", // the events the host recorded, not re-derived here
		"Ran normally",
		"taken while the app was running", // the day's point's class, in the CLI's words
		"also goes back to 2026.7.1",      // undoing the update moves the version too
		"Undo these changes",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the page does not say %q:\n%s", want, body)
		}
	}
	// NEWEST FIRST on a page, which is the one place this differs from the CLI's listing.
	if strings.Index(body, "Updated to") > strings.Index(body, "Ran normally") {
		t.Errorf("the rows are oldest-first on a page that is read from the top:\n%s", body)
	}
	// The clean point carries no note at all: a note on every line is a note nobody reads.
	if strings.Count(body, "taken while the app was running") != 1 {
		t.Errorf("the clean point was annotated too:\n%s", body)
	}
	if strings.Contains(body, quadlet.SnapshotMember("home-assistant", quadlet.TriggerStart, time.Date(2026, 9, 21, 3, 5, 0, 0, time.Local))) {
		t.Errorf("the start after the update is listed as a row:\n%s", body)
	}
	port.mu.Lock()
	asked := append([]api.Directive(nil), port.asked...)
	port.mu.Unlock()
	for _, d := range asked {
		if d.Kind != api.DirectiveServiceMembers {
			t.Errorf("listing the history asked the host for %q", d.Kind)
		}
	}
}

// TestHistoryConfirmsBeforeUndoing: a restore discards everything since the point,
// including -- on a mis-click -- the afternoon the household actually wanted. The first press
// must ask nothing of the host.
func TestHistoryConfirmsBeforeUndoing(t *testing.T) {
	r := newRig(t)
	c := r.trust()
	port := newFakePort()
	r.app.port = port
	answerMembers(port)
	member := quadlet.SnapshotMember("home-assistant", quadlet.TriggerAppUpdateBefore,
		time.Date(2026, 9, 21, 3, 0, 0, 0, time.Local))

	resp := postForm(t, r, "/undo", c, url.Values{"point": {member}})
	body := bodyOf(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("first press = %d, want the confirmation page", resp.StatusCode)
	}
	if !strings.Contains(body, "Updated to 2026.8.0") || !strings.Contains(body, "is lost") ||
		!strings.Contains(body, time.Date(2026, 9, 21, 3, 0, 0, 0, time.Local).Format("Mon 2 Jan 2006, 15:04:05")) {
		t.Errorf("the confirmation does not name the change, the exact moment it goes back to, and what it costs:\n%s", body)
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
	resp = postForm(t, r, "/undo", c, url.Values{"point": {member}, "confirm": {"yes"}, "what": {"x"}})
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

// TestHistoryRefusesAPointThatIsGone: pruning runs on every take, so a listing a browser has been
// looking at for a while can name a point that no longer exists. The confirmation re-reads the
// ring rather than trusting the form.
func TestHistoryRefusesAPointThatIsGone(t *testing.T) {
	r := newRig(t)
	c := r.trust()
	port := newFakePort()
	r.app.port = port
	answerMembers(port)
	gone := quadlet.SnapshotMember("home-assistant", quadlet.TriggerStart, time.Date(2020, 1, 1, 0, 0, 0, 0, time.Local))
	resp := postForm(t, r, "/undo", c, url.Values{"point": {gone}})
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

// TestHistoryNeedsATrustedBrowser: reaching this port is not authentication, and
// these two routes read a household's history and can discard part of it.
func TestHistoryNeedsATrustedBrowser(t *testing.T) {
	r := newRig(t)
	port := newFakePort()
	r.app.port = port
	if resp := r.do("GET", "/history/home-assistant", nil, nil); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("history with no session = %d, want 401", resp.StatusCode)
	}
	resp := postForm(t, r, "/undo", nil, url.Values{"point": {"x"}})
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

// TestHistoryRefusesSomethingThatIsNotAPoint: the member arrives in a form field, and it becomes a
// path the host acts on. A name this machine did not take is refused before anything is relayed.
func TestHistoryRefusesSomethingThatIsNotAPoint(t *testing.T) {
	r := newRig(t)
	c := r.trust()
	port := newFakePort()
	r.app.port = port
	for _, bad := range []string{"", "/etc/passwd", "../../x", "home-assistant"} {
		resp := postForm(t, r, "/undo", c, url.Values{"point": {bad}})
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

// TestHistoryMarksAnAppThatDidNotStart: a row with an `unhealthy` reason carries the red
// mark, and while the app is unhealthy a banner offers the undo back to its last healthy state --
// and says nothing once it is healthy again.
func TestHistoryMarksAnAppThatDidNotStart(t *testing.T) {
	at := time.Date(2026, 9, 21, 3, 0, 0, 0, time.Local)
	point := quadlet.SnapshotMember("home-assistant", quadlet.TriggerAppUpdateBefore, at)
	start := quadlet.SnapshotMember("home-assistant", quadlet.TriggerStart, at.Add(time.Minute))
	members := func(h services.Health) string {
		raw, _ := json.Marshal([]quadlet.SnapshotEntry{
			{Member: point, Meta: quadlet.SnapshotMeta{Service: "home-assistant", Trigger: quadlet.TriggerAppUpdateBefore, TakenAt: at,
				Event: &quadlet.Event{At: at, Reasons: []quadlet.Reason{{Kind: quadlet.ReasonAppUpdate, What: "Updated to 2026.9.0"},
					{Kind: quadlet.ReasonUnhealthy}}}}},
			{Member: start, Meta: quadlet.SnapshotMeta{Service: "home-assistant", Trigger: quadlet.TriggerStart, TakenAt: at.Add(time.Minute), Health: h}},
		})
		return string(raw)
	}
	for _, c := range []struct {
		health services.Health
		banner bool
	}{{services.Unhealthy, true}, {services.Healthy, false}} {
		r := newRig(t)
		cookie := r.trust()
		port := newFakePort()
		r.app.port = port
		go func() {
			for range port.waiting {
				port.answer <- api.DirectiveOutcome{State: api.OutcomeDone, Detail: members(c.health)}
			}
		}()
		req, _ := http.NewRequest("GET", r.srv.URL+"/history/home-assistant", nil)
		req.AddCookie(cookie)
		got, err := http.DefaultClient.Do(req)
		must(t, err)
		body := bodyOf(t, got)
		if !strings.Contains(body, `class="what">Updated to 2026.9.0</span>`) || !strings.Contains(body, `class="bad">home-assistant failed to start cleanly after this change.</span>`) {
			t.Errorf("%s: the row that did not start carries no mark:\n%s", c.health, body)
		}
		if got := strings.Contains(body, "is not working"); got != c.banner {
			t.Errorf("%s: banner shown = %t, want %t:\n%s", c.health, got, c.banner, body)
		}
	}
}

// TestHistoryShowsTheHouseholdsHours: times on the page are in the zone the guest holds NOW, read
// from its link at the render, not Go's time.Local -- which this process fixed at its first use,
// before the host pushed the zone. Tokyo, because it is +9 with no DST, so the hour cannot match
// UTC or the test machine's zone by accident.
func TestHistoryShowsTheHouseholdsHours(t *testing.T) {
	var tokyo string
	for _, dir := range []string{os.Getenv("TZDIR"), "/etc/zoneinfo", "/usr/share/zoneinfo"} {
		if p := dir + "/Asia/Tokyo"; dir != "" {
			if _, err := os.Stat(p); err == nil {
				tokyo = p
				break
			}
		}
	}
	if tokyo == "" {
		t.Skip("no timezone database on this machine")
	}
	raw, err := os.ReadFile(tokyo)
	must(t, err)
	loc, err := time.LoadLocationFromTZData("Asia/Tokyo", raw)
	must(t, err)
	old := localtime
	localtime = tokyo
	defer func() { localtime = old }()

	r := newRig(t)
	c := r.trust()
	port := newFakePort()
	r.app.port = port
	answerMembers(port)
	req, _ := http.NewRequest("GET", r.srv.URL+"/history/home-assistant", nil)
	req.AddCookie(c)
	got, err := http.DefaultClient.Do(req)
	must(t, err)
	body := bodyOf(t, got)
	update := time.Date(2026, 9, 21, 3, 0, 0, 0, time.Local) // ring()'s update
	if want := update.In(loc).Format("Mon 2 Jan, 15:04"); !strings.Contains(body, want) {
		t.Errorf("the update's row does not read %q in Tokyo:\n%s", want, body)
	}

	// No link at all is UTC, which is what the guest is until the host tells it.
	localtime = "/nonexistent/localtime"
	if zone() != time.UTC {
		t.Errorf("a guest with no zone link renders in %v, want UTC", zone())
	}
}
