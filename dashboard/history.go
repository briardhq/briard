package main

// THE PICKER ([B.143]): the points an app can be put back to, and putting one back.
//
// It is the dashboard's half of the ring, and it relays exactly like the install button does —
// `service-members` to list, `service-restore` to act, both already on the host's guest-may-ask
// allowlist because their blast radius is one service's data and code on this node. The CLI's
// `briard app history` / `app revert` are the same two directives typed, and neither front end
// decides anything the other does not.
//
// WHAT THE PAGE HAS TO SAY, beyond a list of times: which points are trustworthy, and which ones
// move the app's VERSION as well as its data. The first is the member's own consistency class,
// rendered in the same words the CLI uses. The second is the difference the household is really
// choosing — going back a version is a different act from undoing this afternoon's edit — so it
// is on the row and again on the confirmation.

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

// restoreOp is one restore this page asked for, from the click until the host answers. In memory
// like the installs beside it: it outlives neither the operation nor this process.
type restoreOp struct {
	Started      time.Time
	Title        string
	Done, Failed bool
	Detail       string
}

// historyView is the picker's page.
type historyView struct {
	Service string
	Points  []pointView // newest FIRST here, unlike the CLI: a page is read from the top
	Op      *restoreOpView
	Refresh bool
	Error   string
}

// pointView is one point as a household reads it.
type pointView struct {
	Member  string
	When    string
	Title   string
	Note    string // the consistency phrase, empty for an ordinary clean point
	Version string
	// MovesCode says this point runs a different version of the app than the one running now,
	// which is the difference between a data revert and the DESIGN §8 rollback.
	MovesCode bool
}

type restoreOpView struct {
	Running bool
	Failed  bool
	Title   string
	Detail  string
	Since   string
}

// confirmView is the page between the button and the act. A restore discards everything since the
// point, which is not a thing to do on one click.
type confirmView struct {
	Service, Member, Title, When string
	Note                         string
	MovesCode                    bool
	From, To                     string
}

// showHistory lists one app's points.
func (a *app) showHistory(w http.ResponseWriter, r *http.Request, service string) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	v := historyView{Service: service}
	points, err := a.points(ctx, service)
	if err != nil {
		v.Error = err.Error()
	} else {
		v.Points = points
	}
	a.mu.Lock()
	if op, ok := a.restores[service]; ok {
		v.Op = &restoreOpView{Running: !op.Done, Failed: op.Failed, Title: op.Title, Detail: op.Detail,
			Since: op.Started.Format("15:04")}
		if op.Done && !op.Failed {
			delete(a.restores, service) // the list below already shows where it landed
		}
	}
	a.mu.Unlock()
	v.Refresh = v.Op != nil && v.Op.Running
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := page.ExecuteTemplate(w, "history", v); err != nil {
		log.Printf("dashboard: history page: %v", err)
	}
}

// points asks the host for the ring and renders each member for the page.
//
// NEWEST FIRST, which is the one place this deliberately differs from the CLI: a terminal reads
// upward from the prompt, so the CLI puts the newest last; a page is read from the top.
func (a *app) points(ctx context.Context, service string) ([]pointView, error) {
	o, err := a.port.Submit(ctx, api.Directive{Kind: api.DirectiveServiceMembers, Payload: service})
	if err != nil {
		return nil, err
	}
	if o.State != api.OutcomeDone {
		return nil, fmt.Errorf("%s", o.Detail)
	}
	var members []quadlet.SnapshotEntry
	if err := json.Unmarshal([]byte(o.Detail), &members); err != nil {
		return nil, fmt.Errorf("the machine's answer did not parse: %w", err)
	}
	running := runningVersion(members)
	out := make([]pointView, 0, len(members))
	for i := len(members) - 1; i >= 0; i-- {
		m := members[i]
		v := versionOf(m.Meta.Manifest)
		out = append(out, pointView{
			Member:    m.Member,
			When:      m.Meta.TakenAt.Local().Format("Mon 2 Jan, 15:04"),
			Title:     m.Meta.Title,
			Note:      m.Meta.Consistency.Note(),
			Version:   v,
			MovesCode: v != "" && running != "" && v != running,
		})
	}
	return out, nil
}

// runningVersion is what the app is on now, read off the NEWEST point.
//
// It is a derivation rather than a question asked, and it holds because of how members are taken:
// every start takes one, and an upgrade always restarts the service — so the newest point is
// pinned to the identity running now. An empty ring has no answer and marks nothing, which is the
// right failure: an unmarked row is a row that says less, never one that says something false.
func runningVersion(members []quadlet.SnapshotEntry) string {
	if len(members) == 0 {
		return ""
	}
	return versionOf(members[len(members)-1].Meta.Manifest)
}

func versionOf(raw string) string {
	m, _, err := manifest.Parse([]byte(raw))
	if err != nil {
		return ""
	}
	return m.Version
}

// requestRevert is the button, and the page between it and the act.
//
// TWO STEPS, ALWAYS. A restore discards everything since the point it puts back — including, on a
// mis-click, the afternoon the household actually wanted. The confirmation names the point, what
// it was taken around, and whether the app's version moves with it. (The undo exists too: the
// restore takes a point of its own first. The confirmation says so rather than relying on it.)
func (a *app) requestRevert(w http.ResponseWriter, r *http.Request) {
	member := r.FormValue("member")
	service, _, _, ok := quadlet.ParseSnapshotMember(member)
	if !ok {
		http.Error(w, "that is not a point this machine took\n", http.StatusBadRequest)
		return
	}
	if r.FormValue("confirm") != "yes" {
		a.confirmRevert(w, r, service, member)
		return
	}
	a.mu.Lock()
	if op, running := a.restores[service]; running && !op.Done {
		a.mu.Unlock()
		http.Redirect(w, r, "/history/"+service, http.StatusSeeOther)
		return
	}
	title := r.FormValue("title")
	a.restores[service] = &restoreOp{Started: a.now(), Title: title}
	a.mu.Unlock()
	// IN THE BACKGROUND, like an install: the host stops the service, puts the subvolume back and
	// converges, and a browser should not hold a request open across it. The page refreshes and
	// says where it got to.
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), restoreBudget)
		defer cancel()
		o, err := a.port.Submit(ctx, api.Directive{Kind: api.DirectiveServiceRestore, Payload: member})
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
		log.Printf("dashboard: revert %s to %s: outcome=%+v err=%v", service, member, o, err)
	}()
	http.Redirect(w, r, "/history/"+service, http.StatusSeeOther)
}

// confirmRevert renders the step between. It re-reads the ring rather than trusting the form,
// because a point can be pruned between the listing and the click and "the point is gone" must be
// said here rather than discovered by the restore.
func (a *app) confirmRevert(w http.ResponseWriter, r *http.Request, service, member string) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	points, err := a.points(ctx, service)
	if err != nil {
		http.Error(w, "could not read this app's points: "+err.Error()+"\n", http.StatusBadGateway)
		return
	}
	var chosen *pointView
	for i := range points {
		if points[i].Member == member {
			chosen = &points[i]
			break
		}
	}
	if chosen == nil {
		http.Error(w, "that point is no longer on this machine\n", http.StatusNotFound)
		return
	}
	v := confirmView{Service: service, Member: member, Title: chosen.Title, When: chosen.When,
		Note: chosen.Note, MovesCode: chosen.MovesCode, To: chosen.Version}
	if len(points) > 0 {
		v.From = points[0].Version
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
