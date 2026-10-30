package main

// The household's backup on its page, one card per question: when it runs (Schedule), what the
// folder holds (Backups, each with its Details and the apps to restore), the recovery key, and
// where the folder is (Folder, with whether it has left this machine).
//
// The page decides nothing, as with the name. What it shows is the host's view, pushed into
// dashboard.BackupPath through the guest (the host holds the key and runs the backup; the guest is
// disposable), and what it asks for goes through the admin port -- the backup off or on and "I
// have saved the key" as config-sets, a run now, a folder read and then moved -- which the host
// applies and answers.
//
// THE KEY IS THE POINT. A backup whose key died with the machine is worse than none: the household
// believed it was covered. So the key is on the page, and until the household says it has a copy
// somewhere else, the page says plainly that the backup is only as safe as this machine. That is
// the emergency kit Home Assistant makes a gate, made a nudge here: the backup runs from the first
// night either way.

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"briard.io/shared/api"
	"briard.io/shared/dashboard"
	"briard.io/shared/routes"
)

// backupView is the page's data.
type backupView struct {
	Folder   string
	On       bool
	Previous string
	Key      string
	KeySaved bool
	// Last is the newest run, in the household's words; LastFailed says whether it went wrong.
	// The home tile says them; the Schedule says LastAgo.
	LastWhen, LastSize, LastError, LastAgo string
	LastFailed                             bool
	// WrongKey: the newest run found a backup in the folder that this household's key does not
	// open; the Folder card offers to adopt it with its key, or to choose another folder.
	WrongKey bool
	// Held is what the folder held after the newest run.
	Held string
	// OffsiteIn is the sync client holding the folder, "" for none; OffsiteSynced whether it has
	// uploaded the newest good backup; OffsiteUnknown that this host cannot tell. The host decides
	// all three; the card only says them.
	OffsiteIn                     string
	OffsiteSynced, OffsiteUnknown bool
	// Running is since when a run goes ("15:04"), the night's or one asked for; RunEnded how the
	// one Back up now asked for ended, said once. Moving: the Schedule card polls.
	Running, RunEnded string
	RunFailed, Moving bool
	// Asked is the host's refusal of the last thing asked from this page, until the next ask.
	Asked string
	// Backups is the Backups card rendered in the page, for a browser without scripts
	// (/backup?list); nil otherwise, and the card loads itself.
	Backups *backupsCardView
	// nights are the host's record of this machine's recent runs, newest first.
	nights []dashboard.BackupRun
}

// backupNowOp is a run Back up now asked for: when, and how the newest run had ended before it --
// the host's own stamp, so a later ending is this run's whatever either clock says.
type backupNowOp struct {
	Asked  time.Time
	Before time.Time
}

// backupState reads the host's view. Nil until the host has said anything -- a guest the host
// never told (an older host) shows no card rather than an empty one.
func (a *app) backupState() *backupView {
	raw, err := os.ReadFile(a.backupPath)
	if err != nil {
		return nil
	}
	var h dashboard.Backup
	if err := json.Unmarshal(raw, &h); err != nil {
		return nil
	}
	v := &backupView{Folder: h.Folder, On: h.On, Key: h.Key, KeySaved: h.KeySaved, OffsiteIn: h.OffsiteIn,
		OffsiteSynced: h.OffsiteSynced, OffsiteUnknown: h.OffsiteUnknown, nights: h.Nights}
	if h.Previous != h.Folder {
		v.Previous = h.Previous
	}
	if len(h.Nights) > 0 {
		last := h.Nights[0]
		v.LastWhen, v.LastSize, v.LastAgo = backupWhen(last.At), sizeWord(last.BytesAdded), ago(a.now().Sub(last.At))
		v.LastError, v.LastFailed, v.WrongKey = last.Error, last.Error != "", last.WrongKey
		if last.Size > 0 {
			v.Held = sizeWord(last.Size)
		}
	}
	if !h.Running.IsZero() {
		v.Running = h.Running.In(zone()).Format("15:04")
	}
	a.mu.Lock()
	v.Asked = a.backupAsked
	if op := a.backupNow; op != nil {
		switch e := h.Ended; {
		case e != nil && !e.Started.Equal(op.Before):
			v.RunEnded, v.RunFailed = runEnded(*e, h.Nights), e.Failed
		case v.Running == "":
			// Asked, and the host has not taken it up yet: it starts within a few seconds.
			v.Running = op.Asked.In(zone()).Format("15:04")
		}
	}
	a.mu.Unlock()
	v.Moving = v.Running != ""
	return v
}

// runEnded is how a run the page asked for ended, in the household's words: what it added when
// it made a backup, why not when it did not -- the run's record has both, in the words the host
// chose for the household -- and otherwise the host's own words (nothing to back up).
func runEnded(e dashboard.BackupEnd, nights []dashboard.BackupRun) string {
	if len(nights) > 0 && nights[0].At.Equal(e.Started) {
		if n := nights[0]; n.Error != "" {
			return "Did not finish: " + n.Error
		}
		return "Done: " + sizeWord(nights[0].BytesAdded) + " added."
	}
	if e.Failed {
		return "Did not finish: " + e.Detail
	}
	return "Done: " + e.Detail
}

// sayRunEnded forgets the run Back up now asked for once the page has said how it ended: once.
func (a *app) sayRunEnded(v *backupView) {
	if v == nil || v.RunEnded == "" {
		return
	}
	a.mu.Lock()
	a.backupNow = nil
	a.mu.Unlock()
}

// ago is a duration as a person reads it.
func ago(d time.Duration) string {
	plural := func(n int, unit string) string {
		if n == 1 {
			return "1 " + unit + " ago"
		}
		return fmt.Sprintf("%d %ss ago", n, unit)
	}
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return plural(int(d/time.Minute), "minute")
	case d < 48*time.Hour:
		return plural(int(d/time.Hour), "hour")
	}
	return plural(int(d/(24*time.Hour)), "day")
}

// sizeWord is a byte count as a person reads it.
func sizeWord(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.0f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f kB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d bytes", n)
}

// requestBackup is the page's buttons. The settings set through the admin port; the host checks a
// new folder, and the page only carries what was typed.
func (a *app) requestBackup(w http.ResponseWriter, r *http.Request, what string) {
	switch what {
	case "restore":
		a.requestBackupRestore(w, r)
		return
	case "folder":
		a.requestBackupFolder(w, r)
		return
	case "now":
		a.requestBackupNow(w, r)
		return
	}
	var s api.ConfigSetting
	var shown func(*backupView) bool // the view once the host has pushed what it did; nil: nothing to wait for
	switch what {
	case "saved":
		s = api.ConfigSetting{Key: "backup-key-saved", Value: "yes"}
		shown = func(v *backupView) bool { return v.KeySaved }
	case "adopt":
		key := strings.TrimSpace(r.FormValue("key"))
		if key == "" {
			http.Error(w, "the backup's key is needed\n", http.StatusBadRequest)
			return
		}
		s = api.ConfigSetting{Key: "backup-adopt", Value: key}
	case "off":
		s = api.ConfigSetting{Key: "backup-dir", Value: ""}
		shown = func(v *backupView) bool { return !v.On }
	case "on":
		shown = func(v *backupView) bool { return v.On }
		v := a.backupState()
		if v == nil || v.Folder == "" {
			http.Error(w, "this machine has not said where its backups go\n", http.StatusConflict)
			return
		}
		s = api.ConfigSetting{Key: "backup-dir", Value: v.Folder}
	default:
		http.NotFound(w, r)
		return
	}
	payload, err := json.Marshal(s)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	o, err := a.port.Submit(ctx, api.Directive{Kind: api.DirectiveConfigSet, Payload: string(payload)})
	log.Printf("dashboard: backup %s: outcome=%+v err=%v", what, o, err) // the typed key is not in the outcome
	a.asked(o, err)
	if err == nil && o.State == api.OutcomeDone && shown != nil {
		a.awaitView(r.Context(), shown)
	}
	http.Redirect(w, r, back(r), http.StatusSeeOther)
}

// awaitView waits, briefly, for the host's pushed view to show what the host just said it did.
// The host pushes the view on its next tick, so a page that came back first would show the folder
// or the switch as they were, and the household would take it for not done. Bounded: a host that
// has not pushed by then leaves the page one tick behind, as it was.
func (a *app) awaitView(ctx context.Context, shown func(*backupView) bool) {
	deadline := time.Now().Add(a.viewWait) // wall time: the page's clock may be a fixture
	for {
		if v := a.backupState(); v != nil && shown(v) {
			return
		}
		if !time.Now().Before(deadline) {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// asked keeps the host's refusal of what the page asked, or clears the last one.
func (a *app) asked(o api.DirectiveOutcome, err error) {
	asked := ""
	switch {
	case err != nil:
		asked = err.Error()
	case o.State != api.OutcomeDone:
		asked = o.Detail
	}
	a.mu.Lock()
	a.backupAsked = asked
	a.mu.Unlock()
}

// requestBackupNow is Back up now: the run 02:00 starts, asked for now. The host answers at once
// (api.BackupNowStart) and the Schedule card follows the run in the host's view, because the
// admin port answers in order and a first run may take an hour.
func (a *app) requestBackupNow(w http.ResponseWriter, r *http.Request) {
	var before time.Time
	if raw, err := os.ReadFile(a.backupPath); err == nil {
		var h dashboard.Backup
		if json.Unmarshal(raw, &h) == nil && h.Ended != nil {
			before = h.Ended.Started
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	o, err := a.port.Submit(ctx, api.Directive{Kind: api.DirectiveBackupNow, Payload: api.BackupNowStart})
	log.Printf("dashboard: back up now: outcome=%+v err=%v", o, err)
	a.asked(o, err)
	if err == nil && o.State == api.OutcomeDone {
		a.mu.Lock()
		a.backupNow = &backupNowOp{Asked: a.now(), Before: before}
		a.mu.Unlock()
	}
	http.Redirect(w, r, "/backup", http.StatusSeeOther)
}

// THE FOLDER MOVE, READ FIRST: Change asks the host what the folder holds (backup-folder, no
// Confirm) and shows it -- or the host's refusal in its place -- and only the confirmation moves
// the backup. A folder another key made takes that key in the confirmation, so the move and the
// adoption are one act on the host and the backup is never left in a folder this household cannot
// open.

// folderConfirmView is the page between Change and the move.
type folderConfirmView struct {
	Folder, Refused, Previous string
	Empty, OtherKey           bool
	// Own and Earlier say what the folder holds, in words; empty when it holds none of either.
	Own, Earlier string
}

// count is n of something as a person says it.
func count(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

// requestBackupFolder is both steps: GET /backup/folder?folder= reads the folder and asks (a read,
// as the undo's confirmation is), POST moves the backup there.
func (a *app) requestBackupFolder(w http.ResponseWriter, r *http.Request) {
	folder := strings.TrimSpace(r.FormValue("folder"))
	if folder == "" {
		http.Error(w, "a folder is needed\n", http.StatusBadRequest)
		return
	}
	confirm := r.Method == http.MethodPost
	payload, err := json.Marshal(api.BackupFolder{Folder: folder, Key: strings.TrimSpace(r.FormValue("key")), Confirm: confirm})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Minutes at most: restic lists the folder's backups, once for the check and again for the move.
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
	defer cancel()
	o, err := a.port.Submit(ctx, api.Directive{Kind: api.DirectiveBackupFolder, Payload: string(payload)})
	log.Printf("dashboard: backup folder %q (confirm %v): state=%s err=%v", folder, confirm, o.State, err) // never the key
	if confirm {
		a.asked(o, err)
		if err == nil && o.State == api.OutcomeDone {
			a.awaitView(r.Context(), func(v *backupView) bool { return v.On && v.Folder == folder })
		}
		http.Redirect(w, r, "/backup", http.StatusSeeOther)
		return
	}
	v := folderConfirmView{Folder: folder}
	var c dashboard.BackupFolderCheck
	switch {
	case err != nil:
		v.Refused = err.Error()
	case o.State != api.OutcomeDone:
		v.Refused = o.Detail
	case json.Unmarshal([]byte(o.Detail), &c) != nil:
		v.Refused = "the machine's answer did not parse"
	default:
		v.Previous, v.Empty, v.OtherKey = c.Previous, c.Empty, c.OtherKey
		if c.Own > 0 {
			v.Own = count(c.Own, "of this household's backups", "of this household's backups") + ", the newest from " + backupWhen(c.Newest)
		}
		if c.Earlier > 0 {
			v.Earlier = count(c.Earlier, "from an earlier install", "from an earlier install")
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := page.ExecuteTemplate(w, "backup-folder", v); err != nil {
		log.Printf("dashboard: backup folder page: %v", err)
	}
}

// THE BACKUPS: what the folder holds, newest first -- an earlier install's said so -- and the runs
// that failed since the newest backup, which the household would otherwise not see. Each backup
// has a Details page with its apps, each with a Restore. The restore relays exactly as the
// history's undo does -- `backup-list` to list, `backup-restore` to act, both on the host's
// guest-may-ask allowlist -- and shares the undo's in-flight record, so one app has one restore at
// a time whichever page asked for it.
//
// THE LIST IS READ WHEN ASKED, never pushed: it is restic reading the repository, seconds, so the
// card loads itself (/f/backups) and the page around it renders at once; without a script, the
// card's link renders it in the page (/backup?list).
//
// NOTHING IS "LOST": the restore takes an undo point first, so the words say the app goes back,
// and that what it has now is kept in its History.

// backupsCardView is the card: the backups and the failed runs, and a restore in flight or just
// ended.
type backupsCardView struct {
	Rows  []backupRowView
	Error string
	Ops   []backupOpView
	// Moving: a restore is running; the card polls, and the list waits until it ends.
	Moving bool
}

// backupRowView is one row: a backup (ID set), or a run that did not make one.
type backupRowView struct {
	ID, When, Added, Error string
	Earlier                bool // an earlier install's, in an adopted folder
}

// backupAppView is one app in a backup.
type backupAppView struct {
	Name, Title, Version, Taken string
	Installed                   bool
}

// backupOpView is one restore from a backup, running or just ended.
type backupOpView struct {
	Service, Title, From, Since, Detail string
	Running, Failed                     bool
}

// backupWhen is how the page says a backup's or an app's moment.
func backupWhen(t time.Time) string { return t.In(zone()).Format("Mon 2 Jan, 15:04") }

// backupsCard is the card's view. A restore that ended well is said once and forgotten, as the
// history's undo is; one that failed stays until the next restore of that app.
func (a *app) backupsCard(ctx context.Context) backupsCardView {
	var v backupsCardView
	a.mu.Lock()
	for service, op := range a.restores {
		if !op.Backup {
			continue
		}
		v.Ops = append(v.Ops, backupOpView{Service: service, Title: appTitle(service), From: op.What,
			Since: op.Started.In(zone()).Format("15:04"), Detail: op.Detail, Running: !op.Done, Failed: op.Failed})
		v.Moving = v.Moving || !op.Done
		if op.Done && !op.Failed {
			delete(a.restores, service)
		}
	}
	a.mu.Unlock()
	slices.SortFunc(v.Ops, func(x, y backupOpView) int { return strings.Compare(x.Title, y.Title) })
	if v.Moving {
		return v
	}
	list, err := a.backups(ctx)
	if err != nil {
		v.Error = err.Error()
		return v
	}
	var nights []dashboard.BackupRun
	if s := a.backupState(); s != nil {
		nights = s.nights
	}
	// The runs that made no backup since the newest one: this machine's record alone has them.
	for _, n := range nights {
		if n.Error != "" && (len(list) == 0 || n.At.After(list[0].At)) {
			v.Rows = append(v.Rows, backupRowView{When: backupWhen(n.At), Error: n.Error})
		}
	}
	for _, b := range list {
		row := backupRowView{ID: b.ID, When: backupWhen(b.At), Earlier: !b.Own}
		// Added is this machine's record of its own run: an earlier install's has none here.
		for _, n := range nights {
			if b.Own && n.Snapshot != "" && (strings.HasPrefix(n.Snapshot, b.ID) || strings.HasPrefix(b.ID, n.Snapshot)) {
				row.Added = sizeWord(n.BytesAdded)
			}
		}
		v.Rows = append(v.Rows, row)
	}
	return v
}

// backups asks the host what the folder holds.
func (a *app) backups(ctx context.Context) ([]dashboard.BackupListing, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	o, err := a.port.Submit(ctx, api.Directive{Kind: api.DirectiveBackupList})
	if err != nil {
		return nil, err
	}
	if o.State != api.OutcomeDone {
		return nil, fmt.Errorf("%s", o.Detail)
	}
	var list []dashboard.BackupListing
	if err := json.Unmarshal([]byte(o.Detail), &list); err != nil {
		return nil, fmt.Errorf("the machine's answer did not parse: %w", err)
	}
	return list, nil
}

// backupDetailsView is one backup's page: when, whose, what it added and holds, and its apps.
type backupDetailsView struct {
	ID, When, Added, Size string
	Earlier               bool
	Apps                  []backupAppView
}

// backupID is a restic snapshot id as the page links one: hex, short or full.
var backupID = regexp.MustCompile(`^[0-9a-f]{8,64}$`)

// showBackup is a backup's Details page, read from the folder as the list is: the backup can be
// pruned between the list and the click, and then the page says so.
func (a *app) showBackup(w http.ResponseWriter, r *http.Request, id string) {
	if !backupID.MatchString(id) {
		http.NotFound(w, r)
		return
	}
	list, err := a.backups(r.Context())
	if err != nil {
		http.Error(w, "could not read the backups: "+err.Error()+"\n", http.StatusBadGateway)
		return
	}
	installed := a.installedApps()
	for _, b := range list {
		if b.ID != id {
			continue
		}
		v := backupDetailsView{ID: b.ID, When: backupWhen(b.At), Earlier: !b.Own}
		if b.Added > 0 || b.Size > 0 {
			v.Added, v.Size = sizeWord(b.Added), sizeWord(b.Size)
		}
		for _, ap := range b.Apps {
			v.Apps = append(v.Apps, backupAppView{Name: ap.Name, Title: appTitle(ap.Name), Version: ap.Version,
				Taken: backupWhen(ap.TakenAt), Installed: installed[ap.Name]})
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := page.ExecuteTemplate(w, "backup-details", v); err != nil {
			log.Printf("dashboard: backup details page: %v", err)
		}
		return
	}
	http.Error(w, "that backup is no longer in the folder\n", http.StatusNotFound)
}

// backupConfirmView is the page between the button and the restore.
type backupConfirmView struct {
	Backup, When, Service, Title, Version, Taken string
	Installed                                    bool
	// From is the version the app runs now, when the backup's is another: the restore moves it.
	From string
}

// requestBackupRestore is the Details page's button and the page between it and the act -- two
// steps, as the undo's are, and the confirmation re-reads the list rather than trusting the form:
// a backup can be pruned between the listing and the click.
func (a *app) requestBackupRestore(w http.ResponseWriter, r *http.Request) {
	id, service := r.FormValue("backup"), r.FormValue("app")
	if id == "" || service == "" {
		http.Error(w, "a backup and an app are needed\n", http.StatusBadRequest)
		return
	}
	if r.FormValue("confirm") != "yes" {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
		defer cancel()
		list, err := a.backups(ctx)
		if err != nil {
			http.Error(w, "could not read the backups: "+err.Error()+"\n", http.StatusBadGateway)
			return
		}
		var v *backupConfirmView
		for _, b := range list {
			for _, ap := range b.Apps {
				if b.ID == id && ap.Name == service {
					v = &backupConfirmView{Backup: id, When: backupWhen(b.At), Service: service, Title: appTitle(service),
						Version: ap.Version, Taken: backupWhen(ap.TakenAt), Installed: a.installedApps()[service]}
				}
			}
		}
		if v == nil {
			http.Error(w, "that backup no longer holds that app\n", http.StatusNotFound)
			return
		}
		if v.Installed {
			if _, running, err := a.rows(ctx, service); err == nil && running != nil && running.Version != v.Version {
				v.From = running.Version
			}
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := page.ExecuteTemplate(w, "backup-confirm", v); err != nil {
			log.Printf("dashboard: backup confirm page: %v", err)
		}
		return
	}
	payload, err := json.Marshal(api.BackupRestore{Service: service, Snapshot: id})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	a.mu.Lock()
	if op, running := a.restores[service]; running && !op.Done {
		a.mu.Unlock()
		http.Redirect(w, r, "/backup", http.StatusSeeOther)
		return
	}
	a.restores[service] = &restoreOp{Started: a.now(), What: r.FormValue("when"), Backup: true}
	a.mu.Unlock()
	// IN THE BACKGROUND, as the undo: the host reads the backup, pulls the app's image if this
	// machine lacks it, and swaps the data in -- minutes, which no browser should hold a request
	// open across.
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), restoreBudget)
		defer cancel()
		o, err := a.port.Submit(ctx, api.Directive{Kind: api.DirectiveBackupRestore, Payload: string(payload)})
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
		log.Printf("dashboard: restore %s from backup %s: outcome=%+v err=%v", service, id, o, err)
	}()
	http.Redirect(w, r, "/backup", http.StatusSeeOther)
}

// installedApps is what this machine serves, by the routes table the reverse proxy reads.
func (a *app) installedApps() map[string]bool {
	installed := map[string]bool{}
	if raw, err := os.ReadFile(a.routesPath); err == nil {
		if t, err := routes.Parse(raw); err == nil {
			for _, s := range t.Services {
				installed[s.Name] = true
			}
		}
	}
	return installed
}
