package main

// THE ALERT LIST: what is wrong on this machine right now, then what happened lately.
//
// The page decides nothing. The alerts are the host's -- its store is the one record, and the
// host pushes a copy here (dashboard.AlertsPath) when the store moves and at the start of every
// connection. Nothing is acknowledged from the page: an alert closes when the host sees its
// condition end, and an event is history the moment it is written.
//
// WHILE THE HOST AGENT IS AWAY the copy stops moving, and listing it would show a stale picture
// as if it were current. So the page shows one banner instead, from the guest agent's contact
// stamp -- the agent is unreachable, and that is the thing to fix, whatever else was open. What
// the guest did meanwhile reaches the store as an alert once the agent is back.

import (
	"encoding/json"
	"os"
	"slices"
	"time"

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
	Open        []alertRow
	Recent      []alertRow // newest first
}

// alertRow is one alert as a household reads it.
type alertRow struct {
	Key      string
	When     string
	Severity string // info / warning / critical; "" on a resolved
	Resolved bool
	Title    string
	Body     string
}

// alertsState reads the contact stamp and the host's copy. Nil when the host never pushed one
// (an older host, or a guest not yet told) and is not known to be away: no card, rather than a
// card claiming nothing is wrong.
func (a *app) alertsState() *alertsView {
	if fi, err := os.Stat(a.contactPath); err == nil && a.now().Sub(fi.ModTime()) > hostSilent {
		return &alertsView{Unreachable: fi.ModTime().In(zone()).Format("2 Jan 15:04")}
	}
	raw, err := os.ReadFile(a.alertsPath)
	if err != nil {
		return nil
	}
	var recs []notify.Record
	if err := json.Unmarshal(raw, &recs); err != nil {
		return nil
	}
	row := func(r notify.Record) alertRow {
		return alertRow{Key: r.Key, When: r.At.In(zone()).Format("2 Jan 15:04"), Severity: string(r.Severity), Resolved: r.Kind == notify.Resolved, Title: r.Title, Body: r.Body}
	}
	v := &alertsView{}
	for _, r := range notify.OpenNow(recs) {
		v.Open = append(v.Open, row(r))
	}
	slices.Reverse(recs)
	for _, r := range recs[:min(len(recs), alertsRecent)] {
		v.Recent = append(v.Recent, row(r))
	}
	return v
}
