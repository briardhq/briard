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

	"briard.io/agent/quadlet"
	"briard.io/shared/api"
	"briard.io/shared/dashboard"
)

// uninstalled puts the copy an uninstall kept of app, taken at `at`, in the rig's History directory.
func (r *rig) uninstalled(t *testing.T, app string, at time.Time) {
	t.Helper()
	must(t, os.MkdirAll(filepath.Join(r.app.snapshotsPath, filepath.Base(quadlet.SnapshotMember(app, quadlet.TriggerUninstall, at))), 0o755))
}

// UNINSTALL IS TWO STEPS: the first press shows what is kept and until when, asking the host only
// what the app holds; the confirmation sends one app-uninstall -- "now" only from its own button --
// and lands on the Apps page; a refusal is said on the page between.
func TestUninstallIsTwoSteps(t *testing.T) {
	r := newRig(t)
	c := r.trust()
	port := newFakePort()
	r.app.port = port
	r.app.now = func() time.Time { return time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC) }
	usage, _ := json.Marshal(dashboard.AppUsage{Data: &dashboard.TierUsage{Measured: true, Current: 1.21e9}})
	port.answer <- api.DirectiveOutcome{State: api.OutcomeDone, Detail: string(usage)}
	page := bodyOf(t, postForm(t, r, "/uninstall", c, url.Values{"app": {"home-assistant"}}))
	for _, want := range []string{"Uninstall Home Assistant?", "kept until " + keptUntil(r.app.now()), "at most 1.21 GB",
		`name="confirm" value="yes"`, "Uninstall and delete its data now"} {
		if !strings.Contains(page, want) {
			t.Errorf("the page between lacks %q:\n%s", want, page)
		}
	}
	if len(port.asked) != 1 || port.asked[0].Kind != api.DirectiveAppUsage {
		t.Fatalf("the first press asked %+v, want only the app's usage", port.asked)
	}

	for _, tc := range []struct {
		form    url.Values
		payload string
	}{
		{url.Values{"app": {"home-assistant"}, "confirm": {"yes"}}, "home-assistant"},
		{url.Values{"app": {"home-assistant"}, "now": {"yes"}, "confirm": {"yes"}}, "home-assistant now"},
	} {
		port.answer <- api.DirectiveOutcome{State: api.OutcomeDone}
		resp := postForm(t, r, "/uninstall", c, tc.form)
		resp.Body.Close()
		last := port.asked[len(port.asked)-1]
		if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/apps" ||
			last.Kind != api.DirectiveAppUninstall || last.Payload != tc.payload {
			t.Errorf("%v: %d to %q after %+v, want one app-uninstall %q and the Apps page", tc.form, resp.StatusCode, resp.Header.Get("Location"), last, tc.payload)
		}
	}

	port.answer = make(chan api.DirectiveOutcome, 2)
	port.answer <- api.DirectiveOutcome{State: api.OutcomeFailed, Detail: "an install of home-assistant has not finished"}
	port.answer <- api.DirectiveOutcome{State: api.OutcomeDone, Detail: string(usage)}
	page = bodyOf(t, postForm(t, r, "/uninstall", c, url.Values{"app": {"home-assistant"}, "confirm": {"yes"}}))
	if !strings.Contains(page, "Could not uninstall it: an install of home-assistant has not finished") {
		t.Errorf("a refusal is not said:\n%s", page)
	}
}

// THE KEPT COPY IS ON ITS CARD: an uninstalled app's Available card says until when, with Delete
// now; the app installed has none, and Delete now asks before it deletes.
func TestAKeptCopyIsOnItsCard(t *testing.T) {
	r := newRig(t)
	c := r.trust()
	at := time.Now().Add(-24 * time.Hour)
	r.uninstalled(t, "mosquitto", at)
	page := r.get(t, "/apps", c)
	if !strings.Contains(page, "its data is kept until "+keptUntil(at)) || strings.Count(page, `name="now" value="yes"`) != 1 {
		t.Errorf("the Apps page does not show the one kept copy with its Delete now:\n%s", page)
	}

	port := newFakePort()
	r.app.port = port
	page = bodyOf(t, postForm(t, r, "/uninstall", c, url.Values{"app": {"mosquitto"}, "now": {"yes"}}))
	if !strings.Contains(page, "Delete what Mosquitto kept?") || !strings.Contains(page, "kept until "+keptUntil(at)) || len(port.asked) != 0 {
		t.Errorf("Delete now's page between (asked %+v):\n%s", port.asked, page)
	}
}
