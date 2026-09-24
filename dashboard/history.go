package main

// THE HISTORY ([B.143], [B.167]): what happened to an app, and undoing it.
//
// It is the dashboard's half of the ring, and it relays exactly like the install button does —
// `service-members` to list, `service-restore` to act, both already on the host's guest-may-ask
// allowlist because their blast radius is one service's data and code on this node. The CLI's
// `briard app history` / `app undo` are the same two directives typed, and neither front end
// decides anything the other does not: the rows are quadlet.History's.
//
// UNDO LANDS BEFORE THE EVENT, and everything above it goes too. Hovering a row (or tapping it on a
// touch screen) greys it and every row above it — exactly what "Undo these changes" discards — so
// the page shows the consequence before the button is pressed. This is deliberately the opposite of
// Photoshop's history panel, whose states are the moment AFTER an action: ours have gaps, "after A"
// can be hours older than "before B", and a household wants the most recent moment that excludes
// the change.

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"briard.io/agent/quadlet"
	"briard.io/shared/api"
	"briard.io/shared/manifest"
)

// restoreBudget bounds the wait for the host's answer. A restore stops the service, puts a
// subvolume back and converges: seconds, unless an image has to be fetched first.
const restoreBudget = 20 * time.Minute

// restoreOp is one undo this page asked for, from the click until the host answers. In memory
// like the installs beside it: it outlives neither the operation nor this process.
type restoreOp struct {
	Started      time.Time
	What         string
	Done, Failed bool
	Detail       string
}

// historyView is the history page.
type historyView struct {
	Service string
	Rows    []rowView // newest FIRST here, unlike the CLI: a page is read from the top
	Op      *restoreOpView
	Refresh bool
	Error   string
}

// rowView is one event as a household reads it.
type rowView struct {
	Point string // the member that undoing this row puts back
	When  string // the EVENT's time, which is what the row shows
	What  string
	Note  string // the point's consistency phrase, empty for an ordinary clean point
	// Back is the exact moment undoing this row goes back to — the confirm step says it, since it
	// differs from When for a detected change.
	Back    string
	Version string
	// MovesCode says the point runs a different version of the app than the one running now,
	// which is the difference between undoing an edit and the DESIGN §8 rollback.
	MovesCode bool
}

type restoreOpView struct {
	Running bool
	Failed  bool
	What    string
	Detail  string
	Since   string
}

// confirmView is the page between the button and the act. An undo discards everything since the
// point, which is not a thing to do on one click.
type confirmView struct {
	Service, Point, What, Back string
	Note                       string
	MovesCode                  bool
	From, To                   string
}

// showHistory lists one app's events.
func (a *app) showHistory(w http.ResponseWriter, r *http.Request, service string) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	v := historyView{Service: service}
	rows, _, err := a.rows(ctx, service)
	if err != nil {
		v.Error = err.Error()
	} else {
		v.Rows = rows
	}
	a.mu.Lock()
	if op, ok := a.restores[service]; ok {
		v.Op = &restoreOpView{Running: !op.Done, Failed: op.Failed, What: op.What, Detail: op.Detail,
			Since: op.Started.Format("15:04")}
		if op.Done && !op.Failed {
			delete(a.restores, service) // the list below already shows the undo as its newest row
		}
	}
	a.mu.Unlock()
	v.Refresh = v.Op != nil && v.Op.Running
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := page.ExecuteTemplate(w, "history", v); err != nil {
		log.Printf("dashboard: history page: %v", err)
	}
}

// rows asks the host for the ring and renders its history for the page, with the version the app
// runs now.
func (a *app) rows(ctx context.Context, service string) ([]rowView, string, error) {
	o, err := a.port.Submit(ctx, api.Directive{Kind: api.DirectiveServiceMembers, Payload: service})
	if err != nil {
		return nil, "", err
	}
	if o.State != api.OutcomeDone {
		return nil, "", fmt.Errorf("%s", o.Detail)
	}
	var members []quadlet.SnapshotEntry
	if err := json.Unmarshal([]byte(o.Detail), &members); err != nil {
		return nil, "", fmt.Errorf("the machine's answer did not parse: %w", err)
	}
	running := runningVersion(members)
	var out []rowView
	for _, h := range quadlet.History(members, time.Local) {
		v := versionOf(h.Point.Meta.Manifest)
		back, _ := quadlet.SnapshotMemberTime(h.Point.Member)
		out = append(out, rowView{
			Point:     h.Point.Member,
			When:      h.At.Local().Format("Mon 2 Jan, 15:04"),
			What:      h.What,
			Note:      h.Point.Meta.Consistency.Note(),
			Back:      back.Local().Format("Mon 2 Jan 2006, 15:04:05"),
			Version:   v,
			MovesCode: v != "" && running != "" && v != running,
		})
	}
	return out, running, nil
}

// runningVersion is what the app is on now, read off the NEWEST member.
//
// It is a derivation rather than a question asked, and it holds because of how members are taken:
// every start takes one and the newest is never pruned, and an update always restarts the service
// — so the newest member is pinned to the identity running now. An empty ring has no answer and
// marks nothing, which is the right failure: an unmarked row says less, never something false.
func runningVersion(members []quadlet.SnapshotEntry) string {
	var newest quadlet.SnapshotEntry
	var at time.Time
	for _, m := range members {
		if t, ok := quadlet.SnapshotMemberTime(m.Member); ok && t.After(at) {
			newest, at = m, t
		}
	}
	return versionOf(newest.Meta.Manifest)
}

func versionOf(raw string) string {
	m, _, err := manifest.Parse([]byte(raw))
	if err != nil {
		return ""
	}
	return m.Version
}

// requestUndo is the button, and the page between it and the act.
//
// TWO STEPS, ALWAYS. An undo discards everything since the point it puts back — including, on a
// mis-click, the afternoon the household actually wanted. The confirmation names the exact moment
// it goes back to and whether the app's version moves with it. (The undo is itself in the history
// and can be undone; the confirmation says so rather than relying on it.)
func (a *app) requestUndo(w http.ResponseWriter, r *http.Request) {
	point := r.FormValue("point")
	service, _, _, ok := quadlet.ParseSnapshotMember(point)
	if !ok {
		http.Error(w, "that is not a point this machine took\n", http.StatusBadRequest)
		return
	}
	if r.FormValue("confirm") != "yes" {
		a.confirmUndo(w, r, service, point)
		return
	}
	a.mu.Lock()
	if op, running := a.restores[service]; running && !op.Done {
		a.mu.Unlock()
		http.Redirect(w, r, "/history/"+service, http.StatusSeeOther)
		return
	}
	a.restores[service] = &restoreOp{Started: a.now(), What: r.FormValue("what")}
	a.mu.Unlock()
	// IN THE BACKGROUND, like an install: the host stops the service, puts the subvolume back and
	// converges, and a browser should not hold a request open across it. The page refreshes and
	// says where it got to.
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), restoreBudget)
		defer cancel()
		o, err := a.port.Submit(ctx, api.Directive{Kind: api.DirectiveServiceRestore, Payload: point})
		a.mu.Lock()
		defer a.mu.Unlock()
		op := a.restores[service]
		op.Done = true
		switch {
		case err != nil:
			op.Failed, op.Detail = true, err.Error()
		case o.State != api.OutcomeDone:
			op.Failed, op.Detail = true, o.Detail
		default:
			op.Detail = o.Detail
		}
		log.Printf("dashboard: undo %s to %s: outcome=%+v err=%v", service, point, o, err)
	}()
	http.Redirect(w, r, "/history/"+service, http.StatusSeeOther)
}

// confirmUndo renders the step between. It re-reads the history rather than trusting the form,
// because a point can be pruned between the listing and the click and "the point is gone" must be
// said here rather than discovered by the restore.
func (a *app) confirmUndo(w http.ResponseWriter, r *http.Request, service, point string) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	rows, running, err := a.rows(ctx, service)
	if err != nil {
		http.Error(w, "could not read this app's history: "+err.Error()+"\n", http.StatusBadGateway)
		return
	}
	var chosen *rowView
	for i := range rows {
		if rows[i].Point == point {
			chosen = &rows[i]
			break
		}
	}
	if chosen == nil {
		http.Error(w, "that point is no longer on this machine\n", http.StatusNotFound)
		return
	}
	v := confirmView{Service: service, Point: point, What: chosen.What, Back: chosen.Back,
		Note: chosen.Note, MovesCode: chosen.MovesCode, To: chosen.Version}
	if chosen.MovesCode {
		v.From = running
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := page.ExecuteTemplate(w, "confirm", v); err != nil {
		log.Printf("dashboard: confirm page: %v", err)
	}
}

// historyService reads the app name out of /history/<service>, or "" for a path that names none.
func historyService(path string) string {
	name := strings.TrimPrefix(path, "/history/")
	if name == path || name == "" || strings.Contains(name, "/") {
		return ""
	}
	return name
}
