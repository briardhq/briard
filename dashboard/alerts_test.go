package main

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"briard.io/shared/api"
	"briard.io/shared/notify"
)

// hostPushes is the host handing over its alert store (dashboard.alerts); contact is the guest
// agent's stamp of the host's last request.
func (r *rig) hostPushes(t *testing.T, recs []notify.Record) {
	t.Helper()
	raw, _ := json.Marshal(recs)
	must(t, os.WriteFile(r.app.alertsPath, raw, 0o644))
}

func (r *rig) contact(t *testing.T, at time.Time) {
	t.Helper()
	must(t, os.WriteFile(r.app.contactPath, nil, 0o644))
	must(t, os.Chtimes(r.app.contactPath, at, at))
}

func alertsRig(t *testing.T) *rig {
	r := newRig(t)
	r.app.alertsPath = filepath.Join(r.dir, "alerts.json")
	r.app.contactPath = filepath.Join(r.dir, ".host-contact")
	return r
}

// What is open is listed first, anchored by key; what closed is history only. Before the host
// has handed anything over there is no card at all -- not a claim that nothing is wrong.
func TestAlertsOpenFirstThenHistory(t *testing.T) {
	r := alertsRig(t)
	c := r.trust()
	r.contact(t, time.Now())
	if body := r.page(c); strings.Contains(body, "Needs attention") || !strings.Contains(body, "Nothing yet") {
		t.Fatalf("a page the host never told shows an alert card: %s", body)
	}

	t0 := time.Date(2026, 10, 7, 9, 0, 0, 0, time.UTC)
	rec := func(min int, key string, kind notify.Kind, sev notify.Severity, title string) notify.Record {
		return notify.Record{Alert: notify.Alert{Key: key, Kind: kind, Severity: sev, Title: title, Body: "body of " + title}, At: t0.Add(time.Duration(min) * time.Minute)}
	}
	r.hostPushes(t, []notify.Record{
		rec(1, "disk", notify.Open, notify.Warning, "Disk space low"),
		rec(2, "clock", notify.Open, notify.Warning, "Clock not synchronised"),
		rec(3, "clock", notify.Resolved, "", "Clock synchronised"),
		rec(4, "upgrade-rolled-back:os", notify.Event, notify.Info, "Update rolled back"),
	})
	body := r.page(c)
	open := body[strings.Index(body, "Needs attention"):strings.Index(body, "<h1>Activity</h1>")]
	if !strings.Contains(open, `id="alert-disk"`) || !strings.Contains(open, "Disk space low") || strings.Contains(open, "Clock not synchronised") || strings.Contains(open, "rolled back") {
		t.Errorf("the open list is not exactly the disk alert: %s", open)
	}
	recent := body[strings.Index(body, "<h1>Activity</h1>"):]
	recent = recent[strings.Index(recent, `<div class="card wide"><section>`):]
	if !strings.Contains(recent, "Clock synchronised") || !strings.Contains(recent, "Update rolled back") || strings.Index(recent, "Update rolled back") > strings.Index(recent, "Disk space low") {
		t.Errorf("the history is not every record, newest first: %s", recent)
	}

	r.hostPushes(t, []notify.Record{rec(1, "disk", notify.Open, notify.Warning, "Disk space low"), rec(5, "disk", notify.Resolved, "", "Disk space recovered")})
	if body := r.page(c); strings.Contains(body, "Needs attention") || !strings.Contains(body, "Everything is running") || !strings.Contains(body, "Disk space recovered") {
		t.Errorf("a closed alert still reads as open, or the history went: %s", body)
	}
}

// The host agent away: one banner from the contact stamp, and the stale copy is not listed as if
// it were current. A stamp inside the minute is an agent that is merely between requests.
func TestAlertsBannerWhenTheHostAgentIsAway(t *testing.T) {
	r := alertsRig(t)
	c := r.trust()
	r.hostPushes(t, []notify.Record{{Alert: notify.Alert{Key: "disk", Kind: notify.Open, Severity: notify.Warning, Title: "Disk space low"}, At: time.Now()}})

	r.contact(t, time.Now().Add(-30*time.Second))
	if body := r.page(c); strings.Contains(body, "not answering") || !strings.Contains(body, "Disk space low") {
		t.Errorf("an agent between requests reads as away, or the list is hidden: %s", body)
	}
	r.contact(t, time.Now().Add(-5*time.Minute))
	body := r.page(c)
	if !strings.Contains(body, "Briard is not answering") || strings.Contains(body, "Disk space low") {
		t.Errorf("an agent silent for five minutes: want the banner alone, got %s", body)
	}
}

// Needs attention is notify.Attention: a warning condition and a warning event are there, each
// with its Dismiss naming the record by key and exact time; an info condition and a dismissed one
// are not, and stay in the history. Dismiss relays exactly one alert-dismiss and goes back.
func TestAlertsAttentionAndDismiss(t *testing.T) {
	r := alertsRig(t)
	c := r.trust()
	r.contact(t, time.Now())
	port := newFakePort()
	r.app.port = port
	t0 := time.Date(2026, 11, 13, 9, 0, 0, 123456789, time.UTC)
	at := func(min int) time.Time { return t0.Add(time.Duration(min) * time.Minute) }
	recs := []notify.Record{
		{Alert: notify.Alert{Key: "disk", Kind: notify.Open, Severity: notify.Warning, Title: "Disk space low"}, At: at(1)},
		{Alert: notify.Alert{Key: "fast-nic", Kind: notify.Open, Severity: notify.Info, Title: "Fast network card"}, At: at(2)},
		{Alert: notify.Alert{Key: "reparent", Kind: notify.Event, Severity: notify.Warning, Title: "Moved to another device"}, At: at(3)},
		{Alert: notify.Alert{Key: "backup", Kind: notify.Open, Severity: notify.Warning, Title: "Backup not working"}, At: at(4), Dismissed: true},
	}
	r.hostPushes(t, recs)

	body := r.page(c)
	open := body[strings.Index(body, "Needs attention"):strings.Index(body, "<h1>Activity</h1>")]
	for _, want := range []string{"Disk space low", `value="` + at(1).Format(time.RFC3339Nano) + `"`, "Moved to another device", "since ", ">Dismiss<"} {
		if !strings.Contains(open, want) {
			t.Errorf("needs attention lacks %q: %s", want, open)
		}
	}
	if strings.Contains(open, "Fast network card") || strings.Contains(open, "Backup not working") {
		t.Errorf("an info or a dismissed alert asks for attention: %s", open)
	}
	if ev := open[strings.Index(open, "Moved to another device"):]; strings.Contains(ev[:strings.Index(ev, "Dismiss")], "since") {
		t.Errorf("an event reads as holding since: %s", ev)
	}
	if !strings.Contains(body, "2 things need your attention.") {
		t.Errorf("the status line does not count what needs attention")
	}
	history := body[strings.Index(body, "<h1>Activity</h1>"):]
	if !strings.Contains(history, "Fast network card") || !strings.Contains(history, "Backup not working") {
		t.Errorf("the history lost the info or the dismissed alert: %s", history)
	}

	form := url.Values{"key": {"disk"}, "at": {at(1).Format(time.RFC3339Nano)}}
	if resp := r.form("/alerts/dismiss", nil, form); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("dismiss with no session = %d, want 401", resp.StatusCode)
	}
	if resp := r.form("/alerts/dismiss", c, url.Values{"key": {"disk"}, "at": {"yesterday"}}); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("dismiss with no time = %d, want 400", resp.StatusCode)
	}
	recs[0].Dismissed = true // the host's next copy
	r.hostPushes(t, recs)
	port.answer <- api.DirectiveOutcome{State: api.OutcomeDone}
	if resp := r.form("/alerts/dismiss", c, form); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("dismiss = %d, want 303", resp.StatusCode)
	}
	port.mu.Lock()
	asked := append([]api.Directive(nil), port.asked...)
	port.mu.Unlock()
	var got api.AlertDismiss
	if len(asked) == 1 {
		_ = json.Unmarshal([]byte(asked[0].Payload), &got)
	}
	if len(asked) != 1 || asked[0].Kind != api.DirectiveAlertDismiss || got.Key != "disk" || !got.At.Equal(at(1)) {
		t.Fatalf("dismiss asked the host %+v, want one alert-dismiss of disk at %s", asked, at(1))
	}
	if body := r.page(c); strings.Contains(body[:strings.Index(body, "<h1>Activity</h1>")], "Disk space low") {
		t.Errorf("the dismissed alert still asks for attention")
	}
}
