package main

// THE ALERT LIST: what needs the household's attention on this machine, then what happened lately.
//
// The page decides nothing. The alerts are the host's -- its store is the one record, and the
// host pushes a copy to the guest (dashboard.AlertsPath) when the store moves and at the start of every
// connection. What needs attention is notify.Attention over that copy: a condition or an event at
// Warning or above, not yet dismissed. The page's one act on it is Dismiss: the host marks that
// record seen, and it leaves the list -- a condition stays open, and closes only when the host
// sees it end; the history keeps every record either way.
//
// WHILE THE HOST AGENT IS AWAY the copy stops moving, and listing it would show a stale picture
// as if it were current. So the page shows one banner instead, from the guest agent's contact
// stamp -- the agent is unreachable, and that is the thing to fix, whatever else was open. What
// the guest did meanwhile reaches the store as an alert once the agent is back.

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"slices"
	"time"

	"briard.io/shared/api"
	"briard.io/shared/notify"
)

// hostSilent is how long the host agent may go without a request before the page says it is
// unreachable. The agent asks something every few seconds; a minute is a restart's worth of
// silence, well inside the deadman's minutes.
const hostSilent = time.Minute

// alertsRecent bounds the history under the open alerts.
const alertsRecent = 10

// alertsView is the card's data.
type alertsView struct {
	// Unreachable is when the host agent was last heard from, set only once it has been silent
	// past hostSilent; the card then shows that and nothing else.
	Unreachable string
	Attention   []alertRow
	Recent      []alertRow // newest first
}

// alertRow is one alert as a household reads it.
type alertRow struct {
	Key      string
	At       string // the record's time exactly, which Dismiss names it by
	When     string
	Severity string // info / warning / critical; "" on a resolved
	Resolved bool
	Event    bool // happened, rather than holds: no "since"
	Title    string
	Body     string
}

// readAlerts is the host's copy, or nil when it never pushed one or it cannot be read.
func (a *app) readAlerts() []notify.Record {
	raw, err := os.ReadFile(a.alertsPath)
	if err != nil {
		return nil
	}
	var recs []notify.Record
	if err := json.Unmarshal(raw, &recs); err != nil {
		return nil
	}
	return recs
}

// alertsState reads the contact stamp and the host's copy. Nil when the host never pushed one
// (an older host, or a guest not yet told) and is not known to be away: no card, rather than a
// card claiming nothing is wrong.
func (a *app) alertsState() *alertsView {
	if fi, err := os.Stat(a.contactPath); err == nil && a.now().Sub(fi.ModTime()) > hostSilent {
		return &alertsView{Unreachable: fi.ModTime().In(zone()).Format("2 Jan 15:04")}
	}
	recs := a.readAlerts()
	if recs == nil {
		return nil
	}
	row := func(r notify.Record) alertRow {
		return alertRow{Key: r.Key, At: r.At.Format(time.RFC3339Nano), When: r.At.In(zone()).Format("2 Jan 15:04"),
			Severity: string(r.Severity), Resolved: r.Kind == notify.Resolved, Event: r.Kind == notify.Event, Title: r.Title, Body: r.Body}
	}
	v := &alertsView{}
	for _, r := range notify.Attention(recs) {
		v.Attention = append(v.Attention, row(r))
	}
	slices.Reverse(recs)
	for _, r := range recs[:min(len(recs), alertsRecent)] {
		v.Recent = append(v.Recent, row(r))
	}
	return v
}

// dismissAlert is an alert's Dismiss: one alert-dismiss to the host, naming the record by its key
// and time, then the host's copy awaited until it no longer asks -- so the page the browser goes
// back to is already without it.
func (a *app) dismissAlert(w http.ResponseWriter, r *http.Request) {
	key := r.PostFormValue("key")
	at, err := time.Parse(time.RFC3339Nano, r.PostFormValue("at"))
	if key == "" || err != nil {
		http.Error(w, "an alert is named by its key and time", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	p, _ := json.Marshal(api.AlertDismiss{Key: key, At: at})
	o, err := a.port.Submit(ctx, api.Directive{Kind: api.DirectiveAlertDismiss, Payload: string(p)})
	switch {
	case err != nil:
		log.Printf("dashboard: dismiss %s: %v", key, err)
	case o.State != api.OutcomeDone:
		log.Printf("dashboard: dismiss %s: %s", key, o.Detail)
	default:
		a.awaitDismissed(ctx, key, at)
	}
	http.Redirect(w, r, back(r), http.StatusSeeOther)
}

// awaitDismissed waits, up to viewWait, for the host's copy to stop asking for the record: a few
// of the host's ticks, wall time, as awaitCatalog waits for a wish.
func (a *app) awaitDismissed(ctx context.Context, key string, at time.Time) {
	deadline := time.Now().Add(a.viewWait)
	for {
		asks := false
		for _, r := range notify.Attention(a.readAlerts()) {
			asks = asks || (r.Key == key && r.At.Equal(at))
		}
		if !asks || !time.Now().Before(deadline) {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(200 * time.Millisecond):
		}
	}
}
