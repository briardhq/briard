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
	"time"

	"briard.io/shared/api"
	"briard.io/shared/dashboard"
)

// backupView is the card's data.
type backupView struct {
	Folder   string
	On       bool
	Key      string
	KeySaved bool
	// Last is the newest night, in the household's words; LastFailed says whether it went wrong.
	LastWhen, LastSize, LastError string
	LastFailed                    bool
	// Asked is the host's refusal of the last thing asked from this card, until the next ask.
	Asked string
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
	v := &backupView{Folder: h.Folder, On: h.On, Key: h.Key, KeySaved: h.KeySaved}
	if h.Last != nil {
		v.LastWhen = h.Last.At.In(zone()).Format("Mon 2 Jan, 15:04")
		v.LastSize = sizeWord(h.Last.BytesAdded)
		v.LastError, v.LastFailed = h.Last.Error, h.Last.Error != ""
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

// requestBackup is the card's three buttons, each one setting through the admin port.
func (a *app) requestBackup(w http.ResponseWriter, r *http.Request, what string) {
	var s api.ConfigSetting
	switch what {
	case "saved":
		s = api.ConfigSetting{Key: "backup-key-saved", Value: "yes"}
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
	log.Printf("dashboard: backup %s: outcome=%+v err=%v", what, o, err)
	a.mu.Lock()
	a.backupAsked = asked
	a.mu.Unlock()
	http.Redirect(w, r, "/", http.StatusSeeOther)
}
