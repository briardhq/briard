package main

// The household's backup on its page: where the nightly copy goes, how last night went, the
// recovery key to copy, and the one switch.
//
// The page decides nothing, as with the name. What it shows is the host's view, pushed into
// dashboard.BackupPath through the guest (the host holds the key and runs the backup; the guest is
// disposable), and what it asks for is a config-set through the admin port -- the backup's folder
// off, or back on, and "I have saved the key" -- which the host applies and answers.
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
	"slices"
	"strings"
	"time"

	"briard.io/shared/api"
	"briard.io/shared/dashboard"
	"briard.io/shared/routes"
)

// backupView is the card's data.
type backupView struct {
	Folder   string
	On       bool
	Previous string
	Key      string
	KeySaved bool
	// Last is the newest night, in the household's words; LastFailed says whether it went wrong.
	LastWhen, LastSize, LastError string
	LastFailed                    bool
	// WrongKey: the newest night found a backup in the folder that this household's key does not
	// open; the card offers to adopt it with its key, or to choose another folder.
	WrongKey bool
	// Held is what the folder held after the newest night; Nights the recent nights, newest first.
	Held   string
	Nights []nightView
	// OffsiteIn is the sync client holding the folder, "" for none; OffsiteSynced whether it has
	// uploaded the newest good night. The host decides both; the card only says them.
	OffsiteIn     string
	OffsiteSynced bool
	// Asked is the host's refusal of the last thing asked from this card, until the next ask.
	Asked string
	// Restore is the restore card rendered in the page, for a browser without scripts (/backup?list);
	// nil otherwise, and the card loads itself.
	Restore *restoreCardView
}

// nightView is one row of the recent nights.
type nightView struct {
	When, Added, Error string
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
	v := &backupView{Folder: h.Folder, On: h.On, Key: h.Key, KeySaved: h.KeySaved, OffsiteIn: h.OffsiteIn, OffsiteSynced: h.OffsiteSynced}
	if h.Previous != h.Folder {
		v.Previous = h.Previous
	}
	for _, n := range h.Nights {
		v.Nights = append(v.Nights, nightView{When: n.At.In(zone()).Format("Mon 2 Jan, 15:04"), Added: sizeWord(n.BytesAdded), Error: n.Error})
	}
	if len(h.Nights) > 0 {
		last := h.Nights[0]
		v.LastWhen, v.LastSize = v.Nights[0].When, v.Nights[0].Added
		v.LastError, v.LastFailed, v.WrongKey = last.Error, last.Error != "", last.WrongKey
		if last.Size > 0 {
			v.Held = sizeWord(last.Size)
		}
	}
	a.mu.Lock()
	v.Asked = a.backupAsked
	a.mu.Unlock()
	return v
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

// requestBackup is the card's buttons, each one setting through the admin port. The host checks a
// new folder; the page only carries what was typed.
func (a *app) requestBackup(w http.ResponseWriter, r *http.Request, what string) {
	if what == "restore" {
		a.requestBackupRestore(w, r)
		return
	}
	var s api.ConfigSetting
	switch what {
	case "folder":
		folder := strings.TrimSpace(r.FormValue("folder"))
		if folder == "" {
			http.Error(w, "a folder is needed\n", http.StatusBadRequest)
			return
		}
		s = api.ConfigSetting{Key: "backup-dir", Value: folder}
	case "saved":
		s = api.ConfigSetting{Key: "backup-key-saved", Value: "yes"}
	case "adopt":
		key := strings.TrimSpace(r.FormValue("key"))
		if key == "" {
			http.Error(w, "the backup's key is needed\n", http.StatusBadRequest)
			return
		}
		s = api.ConfigSetting{Key: "backup-adopt", Value: key}
	case "off":
		s = api.ConfigSetting{Key: "backup-dir", Value: ""}
	case "on":
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
	asked := ""
	switch {
	case err != nil:
		asked = err.Error()
	case o.State != api.OutcomeDone:
		asked = o.Detail
	}
	log.Printf("dashboard: backup %s: outcome=%+v err=%v", what, o, err) // the typed key is not in the outcome
	a.mu.Lock()
	a.backupAsked = asked
	a.mu.Unlock()
	http.Redirect(w, r, back(r), http.StatusSeeOther)
}

// THE RESTORE: an app put back from one of the backups in the folder, installed here or not --
// which is why it is on this page and not the app's: an app this machine does not have has no
// page. It relays exactly as the history's undo does -- `backup-list` to list, `backup-restore`
// to act, both on the host's guest-may-ask allowlist -- and shares the undo's in-flight record, so
// one app has one restore at a time whichever page asked for it.
//
// THE LIST IS READ WHEN ASKED, never pushed: it is restic reading the repository, seconds, so the
// card loads itself (/f/restore) and the page around it renders at once; without a script, the
// card's link renders it in the page (/backup?list).
//
// NOTHING IS "LOST": the restore takes an undo point first, so the words say the app goes back,
// and that what it has now is kept in its History.

// restoreCardView is the card: the backups and their apps, and a restore in flight or just ended.
type restoreCardView struct {
	Backups []backupItemView
	Error   string
	Ops     []backupOpView
	// Moving: a restore is running; the card polls, and the list waits until it ends.
	Moving bool
}

// backupItemView is one backup, in the household's words.
type backupItemView struct {
	ID, When string
	Earlier  bool // an earlier machine's, in an adopted folder
	Apps     []backupAppView
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

// backupWhen is how the card says a backup's or an app's moment.
func backupWhen(t time.Time) string { return t.In(zone()).Format("Mon 2 Jan, 15:04") }

// restoreCard is the card's view. A restore that ended well is said once and forgotten, as the
// history's undo is; one that failed stays until the next restore of that app.
func (a *app) restoreCard(ctx context.Context) restoreCardView {
	var v restoreCardView
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
	installed := a.installedApps()
	for _, b := range list {
		item := backupItemView{ID: b.ID, When: backupWhen(b.At), Earlier: !b.Own}
		for _, ap := range b.Apps {
			item.Apps = append(item.Apps, backupAppView{Name: ap.Name, Title: appTitle(ap.Name), Version: ap.Version,
				Taken: backupWhen(ap.TakenAt), Installed: installed[ap.Name]})
		}
		v.Backups = append(v.Backups, item)
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

// backupConfirmView is the page between the button and the restore.
type backupConfirmView struct {
	Backup, When, Service, Title, Version, Taken string
	Installed                                    bool
	// From is the version the app runs now, when the backup's is another: the restore moves it.
	From string
}

// requestBackupRestore is the card's button and the page between it and the act -- two steps, as
// the undo's are, and the confirmation re-reads the list rather than trusting the form: a backup
// can be pruned between the listing and the click.
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
