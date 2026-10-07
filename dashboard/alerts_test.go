package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	if body := r.page(c); strings.Contains(body, "needs your attention") || strings.Contains(body, "Recent alerts") {
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
	open := body[strings.Index(body, "Needs your attention"):strings.Index(body, "Recent alerts")]
	if !strings.Contains(open, `id="alert-disk"`) || !strings.Contains(open, "Disk space low") || strings.Contains(open, "Clock not synchronised") || strings.Contains(open, "rolled back") {
		t.Errorf("the open list is not exactly the disk alert: %s", open)
	}
	recent := body[strings.Index(body, "Recent alerts"):]
	if !strings.Contains(recent, "Clock synchronised") || !strings.Contains(recent, "Update rolled back") || strings.Index(recent, "Update rolled back") > strings.Index(recent, "Disk space low") {
		t.Errorf("the history is not every record, newest first: %s", recent)
	}

	r.hostPushes(t, []notify.Record{rec(1, "disk", notify.Open, notify.Warning, "Disk space low"), rec(5, "disk", notify.Resolved, "", "Disk space recovered")})
	if body := r.page(c); strings.Contains(body, "Needs your attention") || !strings.Contains(body, "Nothing on this machine needs your attention") || !strings.Contains(body, "Disk space recovered") {
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
