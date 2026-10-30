package main

// THE HISTORY: what happened to an app, and undoing it.
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
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
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
	// Backup says it is a restore from the backup (backup.go), which the Backup page shows.
	Backup bool
}

// appPageView is an app's page: its card, with the one button, and its history.
type appPageView struct {
	App     *appView
	History historyView
}

// historyView is the history card, with what the newest member says about the app as it runs now.
type historyView struct {
	Service string
	Title   string // the app as a person names it
	// Version is what the app runs now; OnDisk what its image holds on this machine (the
	// manifest's installed size). Both "" when the ring is empty or says nothing.
	Version, OnDisk string
	Rows            []rowView // newest FIRST here, unlike the CLI: a page is read from the top
	// Banner is the row whose undo returns an UNHEALTHY app to its last healthy state, or nil while
	// the app is healthy.
	Banner  *rowView
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
	// which is the difference between undoing an edit and the designed {code identity + data} rollback.
	MovesCode bool
	// Caption is the red-marked line under a row whose app did not start cleanly after it.
	Caption string
	// Offer says this row is the banner's undo (quadlet.Unhealthy).
	Offer bool
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
	Service, Title, Point, What, Back string
	Note                              string
	MovesCode                         bool
	From, To                          string
}

// showApp is one app's page: its card as the home shows it, and its history.
func (a *app) showApp(w http.ResponseWriter, r *http.Request, self device, service string) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	v := appPageView{App: a.view(ctx, r, self).app(service), History: a.history(ctx, service)}
	v.App.Here = true
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := page.ExecuteTemplate(w, "app-page", v); err != nil {
		log.Printf("dashboard: app page: %v", err)
	}
}

// history is the card's view: the rows, the banner, and the undo in flight if there is one.
func (a *app) history(ctx context.Context, service string) historyView {
	v := historyView{Service: service, Title: appTitle(service)}
	rows, running, err := a.rows(ctx, service)
	if err != nil {
		v.Error = err.Error()
	} else {
		v.Rows = rows
		if running != nil {
			v.Version = running.Version
			if running.InstalledSize > 0 {
				v.OnDisk = gb(running.InstalledSize)
			}
		}
		for i := range rows {
			if rows[i].Offer {
				v.Banner = &rows[i]
			}
		}
	}
	a.mu.Lock()
	if op, ok := a.restores[service]; ok {
		v.Op = &restoreOpView{Running: !op.Done, Failed: op.Failed, What: op.What, Detail: op.Detail,
			Since: op.Started.In(zone()).Format("15:04")}
		if op.Done && !op.Failed {
			delete(a.restores, service) // the list below already shows the undo as its newest row
		}
	}
	a.mu.Unlock()
	v.Refresh = v.Op != nil && v.Op.Running
	return v
}

// rows asks the host for the ring and renders its history for the page, with the manifest the
// app runs now (nil when the ring is empty or its newest member does not say).
func (a *app) rows(ctx context.Context, service string) ([]rowView, *manifest.Manifest, error) {
	o, err := a.port.Submit(ctx, api.Directive{Kind: api.DirectiveServiceMembers, Payload: service})
	if err != nil {
		return nil, nil, err
	}
	if o.State != api.OutcomeDone {
		return nil, nil, fmt.Errorf("%s", o.Detail)
	}
	var members []quadlet.SnapshotEntry
	if err := json.Unmarshal([]byte(o.Detail), &members); err != nil {
		return nil, nil, fmt.Errorf("the machine's answer did not parse: %w", err)
	}
	newest := runningManifest(members)
	var running string
	if newest != nil {
		running = newest.Version
	}
	offer, unhealthy := quadlet.Unhealthy(members)
	var out []rowView
	loc := zone()
	for _, h := range quadlet.History(members, loc) {
		v := versionOf(h.Point.Meta.Manifest)
		back, _ := quadlet.SnapshotMemberTime(h.Point.Member)
		out = append(out, rowView{
			Point:     h.Point.Member,
			When:      h.At.In(loc).Format("Mon 2 Jan, 15:04"),
			What:      h.What,
			Note:      h.Point.Meta.Consistency.Note(),
			Back:      back.In(loc).Format("Mon 2 Jan 2006, 15:04:05"),
			Version:   v,
			MovesCode: v != "" && running != "" && v != running,
			Caption:   h.Caption,
			Offer:     unhealthy && h.Point.Member == offer.Member,
		})
	}
	return out, newest, nil
}

// runningManifest is what the app is on now, read off the NEWEST member.
//
// It is a derivation rather than a question asked, and it holds because of how members are taken:
// every start takes one and the newest is never pruned, and an update always restarts the service
// — so the newest member is pinned to the identity running now. An empty ring has no answer and
// marks nothing, which is the right failure: an unmarked row says less, never something false.
func runningManifest(members []quadlet.SnapshotEntry) *manifest.Manifest {
	var newest quadlet.SnapshotEntry
	var at time.Time
	for _, m := range members {
		if t, ok := quadlet.SnapshotMemberTime(m.Member); ok && t.After(at) {
			newest, at = m, t
		}
	}
	m, _, err := manifest.Parse([]byte(newest.Meta.Manifest))
	if err != nil {
		return nil
	}
	return &m
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
		http.Redirect(w, r, "/apps/"+service, http.StatusSeeOther)
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
	http.Redirect(w, r, "/apps/"+service, http.StatusSeeOther)
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
	v := confirmView{Service: service, Title: appTitle(service), Point: point, What: cmp.Or(chosen.What, chosen.Caption), Back: chosen.Back,
		Note: chosen.Note, MovesCode: chosen.MovesCode, To: chosen.Version}
	if chosen.MovesCode && running != nil {
		v.From = running.Version
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := page.ExecuteTemplate(w, "confirm", v); err != nil {
		log.Printf("dashboard: confirm page: %v", err)
	}
}

// appService reads the app name out of /apps/<service>, or "" for a path that names none.
func appService(path string) string {
	name := strings.TrimPrefix(path, "/apps/")
	if name == path || name == "" || strings.Contains(name, "/") {
		return ""
	}
	return name
}

// localtime is the guest's zone link, which the host sets at every bring-up (sys.timezone).
var localtime = "/etc/localtime"

// zone is the household's timezone as the guest holds it NOW, read at each render. Not time.Local:
// Go fixes that at its first use, and this process starts before the host has pushed the zone, so
// the page would show the History in UTC for the rest of the boot. No link, or one that does not
// parse, is UTC, which is what the guest is until it is told.
func zone() *time.Location {
	b, err := os.ReadFile(localtime)
	if err != nil {
		return time.UTC
	}
	loc, err := time.LoadLocationFromTZData("Local", b)
	if err != nil {
		return time.UTC
	}
	return loc
}

// appTitle is an app's name for a person, the slug for one the dashboard does not offer.
func appTitle(name string) string {
	if a := offeredApp(name); a != nil {
		return a.Title
	}
	return name
}
