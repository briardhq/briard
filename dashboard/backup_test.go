package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"briard.io/shared/api"
	"briard.io/shared/dashboard"
)

const testKey = "abcd-efgh-ijkl-mnop-qrst-uvwx-yz23-4567"

// hostBacksUp is the host pushing its backup view into the guest (dashboard.backup).
func (r *rig) hostBacksUp(t *testing.T, v dashboard.Backup) {
	t.Helper()
	raw, _ := json.Marshal(v)
	must(t, os.WriteFile(r.app.backupPath, raw, 0o600))
}

func backupRig(t *testing.T) (*rig, *http.Cookie, *fakePort) {
	r := newRig(t)
	r.app.backupPath = filepath.Join(r.dir, "backup.json")
	port := newFakePort()
	r.app.port = port
	return r, r.trust(), port
}

// The card's three states, each from the host's view alone: the key not yet saved (the banner and
// the button), saved (neither), and the backup off (the folder kept, the switch back on).
func TestTheBackupCardRendersTheHostsView(t *testing.T) {
	r, c, _ := backupRig(t)
	if body := r.page(c); strings.Contains(body, `id="backup"`) {
		t.Fatal("a card with no view from the host")
	}
	last := &dashboard.BackupRun{At: time.Date(2026, 10, 10, 2, 4, 0, 0, time.UTC), Snapshot: "abc", BytesAdded: 312 << 20}
	r.hostBacksUp(t, dashboard.Backup{Folder: "/home/ana/Briard Backup", On: true, Key: testKey, Nights: []dashboard.BackupRun{*last}})
	body := r.page(c)
	for _, want := range []string{"/home/ana/Briard Backup", testKey, "only on this machine", `action="/backup/saved"`, "312 MB new", `action="/backup/off"`} {
		if !strings.Contains(body, want) {
			t.Errorf("unsaved key: the card lacks %q", want)
		}
	}

	r.hostBacksUp(t, dashboard.Backup{Folder: "/home/ana/Briard Backup", On: true, Key: testKey, KeySaved: true, Nights: []dashboard.BackupRun{*last}})
	body = r.page(c)
	if !strings.Contains(body, testKey) || strings.Contains(body, "only on this machine") || strings.Contains(body, `action="/backup/saved"`) {
		t.Errorf("saved key: the card still nudges, or lost the key: %s", body)
	}

	r.hostBacksUp(t, dashboard.Backup{Folder: "/home/ana/Briard Backup", Key: testKey, KeySaved: true})
	body = r.page(c)
	if !strings.Contains(body, "nightly backup is off") || !strings.Contains(body, `action="/backup/on"`) || strings.Contains(body, `action="/backup/off"`) {
		t.Errorf("off: %s", body)
	}

	r.hostBacksUp(t, dashboard.Backup{Folder: "/f", On: true, Key: testKey, Nights: []dashboard.BackupRun{{At: last.At, Error: "the backup folder is not there"}}})
	if body := r.page(c); !strings.Contains(body, "did not finish: the backup folder is not there") {
		t.Errorf("a failed night is not said: %s", body)
	}
}

// Each button is exactly one config-set through the admin port, and no session asks for nothing.
func TestTheBackupButtonsAskTheHost(t *testing.T) {
	r, c, port := backupRig(t)
	r.hostBacksUp(t, dashboard.Backup{Folder: "/home/ana/Briard Backup", On: true, Key: testKey})
	if resp := r.form("/backup/off", nil, nil); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("off with no session = %d, want 401", resp.StatusCode)
	}
	for path, want := range map[string]api.ConfigSetting{
		"/backup/saved": {Key: "backup-key-saved", Value: "yes"},
		"/backup/off":   {Key: "backup-dir", Value: ""},
		"/backup/on":    {Key: "backup-dir", Value: "/home/ana/Briard Backup"},
	} {
		port.mu.Lock()
		port.asked = nil
		port.mu.Unlock()
		port.answer <- api.DirectiveOutcome{State: api.OutcomeDone}
		if resp := r.form(path, c, nil); resp.StatusCode != http.StatusSeeOther {
			t.Fatalf("%s = %d, want 303", path, resp.StatusCode)
		}
		port.mu.Lock()
		asked := append([]api.Directive(nil), port.asked...)
		port.mu.Unlock()
		var got api.ConfigSetting
		if len(asked) == 1 {
			_ = json.Unmarshal([]byte(asked[0].Payload), &got)
		}
		if len(asked) != 1 || asked[0].Kind != api.DirectiveConfigSet || got != want {
			t.Errorf("%s asked the host %+v, want one config-set %+v", path, asked, want)
		}
	}

	port.answer <- api.DirectiveOutcome{State: api.OutcomeFailed, Detail: "could not record it"}
	r.form("/backup/off", c, nil)
	if body := r.page(c); !strings.Contains(body, "could not record it") {
		t.Errorf("the host's refusal is not shown: %s", body)
	}
}

// The key is served to a trusted device only: a browser with no session gets the refusal, which
// says nothing about this home.
func TestTheKeyIsShownToATrustedDeviceOnly(t *testing.T) {
	r, _, _ := backupRig(t)
	r.hostBacksUp(t, dashboard.Backup{Folder: "/f", On: true, Key: testKey})
	resp := r.do("GET", "/", nil, nil)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusUnauthorized || strings.Contains(string(body), testKey) {
		t.Fatalf("an untrusted browser got %d and the key: %v", resp.StatusCode, strings.Contains(string(body), testKey))
	}
}

// Changing the folder carries what was typed to the host as one config-set; the host judges it,
// and its refusal is what the card shows.
func TestTheFolderFormAsksTheHost(t *testing.T) {
	r, c, port := backupRig(t)
	r.hostBacksUp(t, dashboard.Backup{Folder: "/home/ana/Briard Backup", On: true, Key: testKey})
	if body := r.page(c); !strings.Contains(body, `action="/backup/folder"`) {
		t.Fatalf("no way to change the folder: %s", body)
	}
	if resp := r.form("/backup/folder", c, url.Values{"folder": {"  "}}); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("an empty folder = %d, want 400", resp.StatusCode)
	}
	port.answer <- api.DirectiveOutcome{State: api.OutcomeFailed, Detail: "/etc belongs to root: choose a folder a person owns"}
	r.form("/backup/folder", c, url.Values{"folder": {" /home/ana/Dropbox/Briard Backup "}})
	port.mu.Lock()
	asked := append([]api.Directive(nil), port.asked...)
	port.mu.Unlock()
	var got api.ConfigSetting
	if len(asked) == 1 {
		_ = json.Unmarshal([]byte(asked[0].Payload), &got)
	}
	if want := (api.ConfigSetting{Key: "backup-dir", Value: "/home/ana/Dropbox/Briard Backup"}); len(asked) != 1 || got != want {
		t.Fatalf("asked %+v, want one config-set %+v", asked, want)
	}
	if body := r.page(c); !strings.Contains(body, "belongs to root") {
		t.Errorf("the host's refusal is not shown: %s", body)
	}
}

// After a move: the earlier backups are named as still there and the household's; the nights list
// and what the folder holds come from the host's record.
func TestTheCardSaysWhereTheEarlierBackupsAre(t *testing.T) {
	r, c, _ := backupRig(t)
	at := time.Date(2026, 10, 10, 2, 4, 0, 0, time.UTC)
	r.hostBacksUp(t, dashboard.Backup{Folder: "/new", Previous: "/home/ana/Briard Backup", On: true, Key: testKey, KeySaved: true,
		Nights: []dashboard.BackupRun{
			{At: at, BytesAdded: 5 << 20, Size: 3 << 30},
			{At: at.Add(-24 * time.Hour), Error: "the backup folder is not there"},
		}})
	body := r.page(c)
	for _, want := range []string{"earlier backups are still in <code>/home/ana/Briard Backup</code>", "the folder holds 3.0 GB", "Recent backups", "5 MB new", "did not finish: the backup folder is not there"} {
		if !strings.Contains(body, want) {
			t.Errorf("the card lacks %q", want)
		}
	}
	r.hostBacksUp(t, dashboard.Backup{Folder: "/new", Previous: "/new", On: true})
	if body := r.page(c); strings.Contains(body, "earlier backups are still in") {
		t.Error("a previous folder that is the current one is named as somewhere else")
	}
}

// The off-site line's three states, each the host's: uploaded (the green line, and no advice to
// move the folder), waiting (the plain line and when the client uploads), and no sync client
// (neither, and the advice).
func TestTheCardSaysWhetherTheBackupIsOffsite(t *testing.T) {
	r, c, _ := backupRig(t)
	nights := []dashboard.BackupRun{{At: time.Date(2026, 10, 10, 2, 4, 0, 0, time.UTC), Snapshot: "abc", BytesAdded: 5 << 20}}
	const folder = `C:\Users\ana\OneDrive\Documents\Briard Backup`
	const advice = "keep the folder in a synced drive"

	r.hostBacksUp(t, dashboard.Backup{Folder: folder, On: true, Key: testKey, KeySaved: true, Nights: nights, OffsiteIn: "OneDrive", OffsiteSynced: true})
	body := r.page(c)
	if !strings.Contains(body, `<p class="ok">Off-site: in OneDrive.`) || strings.Contains(body, "Waiting for") || strings.Contains(body, advice) {
		t.Errorf("uploaded: %s", body)
	}

	r.hostBacksUp(t, dashboard.Backup{Folder: folder, On: true, Key: testKey, KeySaved: true, Nights: nights, OffsiteIn: "OneDrive"})
	body = r.page(c)
	if !strings.Contains(body, "Waiting for OneDrive to upload the newest backup.") || !strings.Contains(body, "while someone is signed in") ||
		strings.Contains(body, "Off-site:") || strings.Contains(body, advice) {
		t.Errorf("waiting: %s", body)
	}

	r.hostBacksUp(t, dashboard.Backup{Folder: folder, On: true, Key: testKey, KeySaved: true, Nights: nights})
	body = r.page(c)
	if strings.Contains(body, "Off-site:") || strings.Contains(body, "Waiting for") || !strings.Contains(body, advice) {
		t.Errorf("no sync client: %s", body)
	}
}

// An act returns to the page it was pressed on -- the section's page, read from the Referer --
// and to the home when there is none or it is not one of ours.
func TestActsReturnToThePageTheyWerePressedOn(t *testing.T) {
	r, c, port := backupRig(t)
	answerHistory(port, "") // the host says yes to each; the answer is not the question here
	r.hostBacksUp(t, dashboard.Backup{Folder: "/f", On: true, Key: testKey, KeySaved: true})
	post := func(referer string) string {
		t.Helper()
		req, _ := http.NewRequest("POST", r.srv.URL+"/backup/off", strings.NewReader(""))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if referer != "" {
			req.Header.Set("Referer", referer)
		}
		req.AddCookie(c)
		resp, err := noRedirect.Do(req)
		must(t, err)
		resp.Body.Close()
		if resp.StatusCode != http.StatusSeeOther {
			t.Fatalf("POST /backup/off = %d", resp.StatusCode)
		}
		return resp.Header.Get("Location")
	}
	for referer, want := range map[string]string{r.srv.URL + "/backup": "/backup", r.srv.URL + "/": "/", "": "/", "https://evil.example/backup": "/backup", r.srv.URL + "/apps/home-assistant": "/"} {
		if got := post(referer); got != want {
			t.Errorf("after an act pressed on %q the page goes to %q, want %q", referer, got, want)
		}
	}
}

// theBackups is the host's answer to backup-list: one backup of this household's with Home
// Assistant (installed in the rig) and mosquitto (not), and an earlier machine's.
func theBackups() string {
	at := time.Date(2026, 10, 30, 2, 0, 0, 0, time.Local)
	raw, _ := json.Marshal([]dashboard.BackupListing{
		{ID: "bbbb2222", At: at, Own: true, Apps: []dashboard.BackupApp{
			{Name: "home-assistant", Version: "2026.9.4", TakenAt: at.Add(-26 * time.Minute)},
			{Name: "mosquitto", Version: "2.0.22", TakenAt: at.Add(-40 * time.Minute)},
		}},
		{ID: "aaaa1111", At: at.Add(-7 * 24 * time.Hour), Own: false, Apps: []dashboard.BackupApp{
			{Name: "home-assistant", Version: "2026.8.0", TakenAt: at.Add(-7*24*time.Hour - time.Hour)},
		}},
	})
	return string(raw)
}

// answerByKind answers whatever the page asks: the backups, an app's ring, and a restore with
// restored (or a failure, when restored is "").
func answerByKind(port *fakePort, restored string) {
	go func() {
		for range port.waiting {
			port.mu.Lock()
			d := port.asked[len(port.asked)-1]
			port.mu.Unlock()
			switch d.Kind {
			case api.DirectiveBackupList:
				port.answer <- api.DirectiveOutcome{State: api.OutcomeDone, Detail: theBackups()}
			case api.DirectiveServiceMembers:
				port.answer <- api.DirectiveOutcome{State: api.OutcomeDone, Detail: ring()}
			case api.DirectiveBackupRestore:
				if restored == "" {
					port.answer <- api.DirectiveOutcome{State: api.OutcomeFailed, Detail: "the image could not be fetched"}
				} else {
					port.answer <- api.DirectiveOutcome{State: api.OutcomeDone, Detail: restored}
				}
			default:
				port.answer <- api.DirectiveOutcome{State: api.OutcomeFailed, Detail: "not this test's"}
			}
		}
	}()
}

func (r *rig) get(t *testing.T, path string, c *http.Cookie) string {
	t.Helper()
	req, _ := http.NewRequest("GET", r.srv.URL+path, nil)
	req.AddCookie(c)
	resp, err := noRedirect.Do(req)
	must(t, err)
	return bodyOf(t, resp)
}

// THE RESTORE CARD lists the folder's backups and the apps in each -- installed here or not, and
// an earlier machine's said so -- with a restore per app. The page does not wait for it: the card
// loads itself, or, with no script, renders in the page behind its link.
func TestTheRestoreCardListsTheBackups(t *testing.T) {
	r, c, port := backupRig(t)
	r.hostBacksUp(t, dashboard.Backup{Folder: "/home/ana/Briard Backup", On: true, Key: testKey, KeySaved: true})
	answerByKind(port, "")
	page := r.get(t, "/backup", c)
	if !strings.Contains(page, `hx-get="/f/restore" hx-trigger="load"`) || !strings.Contains(page, `href="/backup?list"`) {
		t.Fatalf("the page does not load its restore card, or offers no way without a script:\n%s", page)
	}
	port.mu.Lock()
	asked := len(port.asked)
	port.mu.Unlock()
	if asked != 0 {
		t.Errorf("rendering the page asked the host %d times; the card asks for itself", asked)
	}
	for _, body := range []string{r.get(t, "/f/restore", c), r.get(t, "/backup?list", c)} {
		for _, want := range []string{"Restore an app", "2026.9.4", "Fri 30 Oct, 02:00", "Fri 30 Oct, 01:34", "from an earlier machine",
			`name="backup" value="bbbb2222"`, `name="app" value="mosquitto"`} {
			if !strings.Contains(body, want) {
				t.Errorf("the card lacks %q:\n%s", want, body)
			}
		}
		if strings.Count(body, "not installed") != 1 {
			t.Errorf("want mosquitto alone marked not installed:\n%s", body)
		}
		if strings.Contains(body, "every 5s") {
			t.Errorf("an idle card polls:\n%s", body)
		}
	}
	if card := r.get(t, "/f/restore", c); strings.Contains(strings.ToLower(card), "night") {
		t.Errorf("the card says night:\n%s", card)
	}
}

// A RESTORE ASKS FIRST, in words that say the app goes back and what it has now is kept -- or,
// for an app this machine does not have, that it is installed. Then it relays one backup-restore,
// the card follows it while it runs, says how it ended once, and is a list again.
func TestARestoreFromTheBackupConfirmsThenRuns(t *testing.T) {
	r, c, port := backupRig(t)
	r.hostBacksUp(t, dashboard.Backup{Folder: "/home/ana/Briard Backup", On: true, Key: testKey, KeySaved: true})
	answerByKind(port, "restored from the backup, as it was at Fri 30 Oct 2026, 01:34:00, code too")

	body := bodyOf(t, postForm(t, r, "/backup/restore", c, url.Values{"backup": {"bbbb2222"}, "app": {"home-assistant"}}))
	for _, want := range []string{"Restore Home Assistant?", "goes back to how it was at Fri 30 Oct, 01:34", "from 2026.8.0 to 2026.9.4",
		"kept in its History", `name="confirm" value="yes"`} {
		if !strings.Contains(body, want) {
			t.Errorf("the confirmation lacks %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "lost") {
		t.Errorf("the confirmation says lost:\n%s", body)
	}
	body = bodyOf(t, postForm(t, r, "/backup/restore", c, url.Values{"backup": {"bbbb2222"}, "app": {"mosquitto"}}))
	if !strings.Contains(body, "is not installed on this machine") || !strings.Contains(body, "installs Mosquitto 2.0.22") {
		t.Errorf("the not-installed confirmation:\n%s", body)
	}
	port.mu.Lock()
	for _, d := range port.asked {
		if d.Kind == api.DirectiveBackupRestore {
			t.Errorf("a confirmation already asked the host to restore: %+v", d)
		}
	}
	port.mu.Unlock()

	// The restore itself: one directive, the app and the backup, and the card follows it.
	resp := postForm(t, r, "/backup/restore", c, url.Values{"backup": {"bbbb2222"}, "app": {"home-assistant"}, "when": {"Fri 30 Oct, 02:00"}, "confirm": {"yes"}})
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/backup" {
		t.Fatalf("the act = %d to %q, want back to the Backup page", resp.StatusCode, resp.Header.Get("Location"))
	}
	var restore api.Directive
	for i := 0; i < 200 && restore.Kind == ""; i++ {
		port.mu.Lock()
		for _, d := range port.asked {
			if d.Kind == api.DirectiveBackupRestore {
				restore = d
			}
		}
		port.mu.Unlock()
		time.Sleep(5 * time.Millisecond)
	}
	var p api.BackupRestore
	if err := json.Unmarshal([]byte(restore.Payload), &p); err != nil || p.Service != "home-assistant" || p.Snapshot != "bbbb2222" {
		t.Fatalf("relayed %+v (%v); want home-assistant from bbbb2222", restore, err)
	}
	var card string
	for i := 0; i < 200; i++ {
		if card = r.get(t, "/f/restore", c); strings.Contains(card, "is back as it was in the backup of Fri 30 Oct, 02:00") {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !strings.Contains(card, "is back as it was") || !strings.Contains(card, `href="/apps/home-assistant"`) {
		t.Fatalf("the card never said the restore ended:\n%s", card)
	}
	if again := r.get(t, "/f/restore", c); strings.Contains(again, "is back as it was") || !strings.Contains(again, "bbbb2222") {
		t.Errorf("the ending was said twice, or the list did not come back:\n%s", again)
	}
}

// A restore that fails says why, and stays said until the next restore of that app.
func TestAFailedRestoreFromTheBackupSaysWhy(t *testing.T) {
	r, c, port := backupRig(t)
	r.hostBacksUp(t, dashboard.Backup{Folder: "/home/ana/Briard Backup", On: true, Key: testKey, KeySaved: true})
	answerByKind(port, "")
	postForm(t, r, "/backup/restore", c, url.Values{"backup": {"bbbb2222"}, "app": {"mosquitto"}, "when": {"Fri 30 Oct, 02:00"}, "confirm": {"yes"}})
	var card string
	for i := 0; i < 200; i++ {
		if card = r.get(t, "/f/restore", c); strings.Contains(card, "was not restored") {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !strings.Contains(card, "Mosquitto was not restored: the image could not be fetched") {
		t.Fatalf("the card:\n%s", card)
	}
	if again := r.get(t, "/f/restore", c); !strings.Contains(again, "was not restored") {
		t.Errorf("the failure was forgotten after one look:\n%s", again)
	}
}

// A new install's folder holds no backup until the first run: the card says so, and offers none.
func TestTheRestoreCardOfAFolderWithNoBackupYet(t *testing.T) {
	r, c, port := backupRig(t)
	r.hostBacksUp(t, dashboard.Backup{Folder: "/var/lib/briard/backup", On: true, Key: testKey, KeySaved: true})
	go func() {
		for range port.waiting {
			port.answer <- api.DirectiveOutcome{State: api.OutcomeDone, Detail: "[]"}
		}
	}()
	card := r.get(t, "/f/restore", c)
	if !strings.Contains(card, "The folder holds no backup yet.") || strings.Contains(card, `action="/backup/restore"`) || strings.Contains(card, "could not be read") {
		t.Errorf("an empty folder's card:\n%s", card)
	}
}
