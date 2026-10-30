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
		{ID: "bbbb2222", At: at, Own: true, Added: 301 << 10, Size: 3 << 30, Apps: []dashboard.BackupApp{
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

// card is the part of a page from the element with id to the next card: what one card says.
func card(page, id string) string {
	i := strings.Index(page, `id="`+id+`"`)
	if i < 0 {
		return ""
	}
	rest := page[i:]
	if j := strings.Index(rest[1:], `<div class="card`); j >= 0 {
		return rest[:j+1]
	}
	return rest
}

// THE SCHEDULE says when it runs and how long ago the last run was -- red when it did not finish,
// and no sizes -- with Back up now and the switch; off, the folder kept and the switch back on.
func TestTheScheduleCard(t *testing.T) {
	r, c, _ := backupRig(t)
	if body := r.page(c); strings.Contains(body, `id="schedule"`) {
		t.Fatal("a card with no view from the host")
	}
	now := time.Date(2026, 10, 30, 11, 4, 0, 0, time.UTC)
	r.app.now = func() time.Time { return now }
	last := dashboard.BackupRun{At: now.Add(-9 * time.Hour), Snapshot: "abc", BytesAdded: 312 << 20, Size: 3 << 30}

	r.hostBacksUp(t, dashboard.Backup{Folder: "/home/ana/Briard Backup", On: true, Key: testKey})
	if s := card(r.get(t, "/backup", c), "schedule"); !strings.Contains(s, "Runs daily at 02:00. The first runs tonight.") {
		t.Errorf("before the first run:\n%s", s)
	}
	r.hostBacksUp(t, dashboard.Backup{Folder: "/home/ana/Briard Backup", On: true, Key: testKey, Nights: []dashboard.BackupRun{last}})
	body := r.get(t, "/backup", c)
	s := card(body, "schedule")
	for _, want := range []string{"Runs daily at 02:00.", "Last run: 9 hours ago.", `action="/backup/now"`, `action="/backup/off"`, "Turn off"} {
		if !strings.Contains(s, want) {
			t.Errorf("the schedule lacks %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "MB") || strings.Contains(s, "GB") || strings.Contains(s, `class="bad"`) || strings.Contains(s, "hx-get") {
		t.Errorf("a good run's schedule says sizes, is red, or polls:\n%s", s)
	}
	for _, want := range []string{testKey, "only on this machine", `action="/backup/saved"`} {
		if !strings.Contains(body, want) {
			t.Errorf("unsaved key: the page lacks %q", want)
		}
	}

	last.Error = "the backup folder is not there"
	r.hostBacksUp(t, dashboard.Backup{Folder: "/f", On: true, Key: testKey, KeySaved: true, Nights: []dashboard.BackupRun{last}})
	if s := card(r.get(t, "/backup", c), "schedule"); !strings.Contains(s, `<span class="bad">Last run: 9 hours ago, did not finish.</span>`) {
		t.Errorf("a failed run is not red:\n%s", s)
	}

	r.hostBacksUp(t, dashboard.Backup{Folder: "/home/ana/Briard Backup", Key: testKey, KeySaved: true})
	s = card(r.get(t, "/backup", c), "schedule")
	if !strings.Contains(s, "Off. The backups already in <code>/home/ana/Briard Backup</code> are kept.") || !strings.Contains(s, `action="/backup/on"`) ||
		!strings.Contains(s, "Turn back on") || strings.Contains(s, `action="/backup/now"`) {
		t.Errorf("off:\n%s", s)
	}
}

// BACK UP NOW asks the host to start the run and does not wait for it; the card says the run is
// going and polls until the host says how it ended, then the page says so once: what it added,
// or why not. A refusal is shown, and nothing polls.
func TestBackUpNowFollowsTheRunToItsEnd(t *testing.T) {
	r, c, port := backupRig(t)
	folder := "/home/ana/Briard Backup"
	before := dashboard.BackupEnd{Started: time.Date(2026, 10, 30, 2, 0, 0, 0, time.UTC), Detail: "last night's"}
	r.hostBacksUp(t, dashboard.Backup{Folder: folder, On: true, Key: testKey, KeySaved: true, Ended: &before})

	port.answer <- api.DirectiveOutcome{State: api.OutcomeDone, Detail: "a backup starts now"}
	if resp := r.form("/backup/now", c, nil); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("Back up now = %d", resp.StatusCode)
	}
	port.mu.Lock()
	asked := append([]api.Directive(nil), port.asked...)
	port.mu.Unlock()
	if len(asked) != 1 || asked[0].Kind != api.DirectiveBackupNow || asked[0].Payload != api.BackupNowStart {
		t.Fatalf("asked %+v; want one backup-now that does not wait", asked)
	}
	// Not taken up yet, then running: the card says so and polls, with no Back up now.
	for _, running := range []time.Time{{}, time.Date(2026, 10, 30, 14, 5, 0, 0, time.UTC)} {
		r.hostBacksUp(t, dashboard.Backup{Folder: folder, On: true, Key: testKey, KeySaved: true, Ended: &before, Running: running})
		s := r.get(t, "/f/schedule", c)
		if !strings.Contains(s, "Backing up…") || !strings.Contains(s, `hx-get="/f/schedule"`) || strings.Contains(s, `action="/backup/now"`) {
			t.Fatalf("running (%v):\n%s", running, s)
		}
	}
	// Ended: the fragment hands over to the page, which says it once.
	ended := dashboard.BackupEnd{Started: time.Date(2026, 10, 30, 14, 5, 0, 0, time.UTC), Detail: "backed up home-assistant: 36 files, 301 kB added (snapshot 5aa539af)"}
	r.hostBacksUp(t, dashboard.Backup{Folder: folder, On: true, Key: testKey, KeySaved: true, Ended: &ended,
		Nights: []dashboard.BackupRun{{At: ended.Started, Snapshot: "5aa539af", BytesAdded: 301 << 10}}})
	resp := r.do("GET", "/f/schedule", c, nil)
	if resp.StatusCode != http.StatusNoContent || resp.Header.Get("HX-Refresh") != "true" {
		t.Fatalf("an ended run's fragment = %d, HX-Refresh %q", resp.StatusCode, resp.Header.Get("HX-Refresh"))
	}
	if s := card(r.get(t, "/backup", c), "schedule"); !strings.Contains(s, `<p class="ok">Done: 301 kB added.</p>`) {
		t.Errorf("the ending is not said:\n%s", s)
	}
	if s := card(r.get(t, "/backup", c), "schedule"); strings.Contains(s, "Done:") {
		t.Errorf("the ending was said twice:\n%s", s)
	}

	// A run that did not finish says why, in the run's record's words.
	port.answer <- api.DirectiveOutcome{State: api.OutcomeDone}
	r.form("/backup/now", c, nil)
	failed := dashboard.BackupEnd{Started: ended.Started.Add(time.Hour), Failed: true, Detail: "Fatal: unable to save snapshot"}
	r.hostBacksUp(t, dashboard.Backup{Folder: folder, On: true, Key: testKey, KeySaved: true, Ended: &failed,
		Nights: []dashboard.BackupRun{{At: failed.Started, Error: "the backup folder is not there"}}})
	if s := card(r.get(t, "/backup", c), "schedule"); !strings.Contains(s, `<p class="bad">Did not finish: the backup folder is not there</p>`) {
		t.Errorf("the failure is not said:\n%s", s)
	}

	port.answer <- api.DirectiveOutcome{State: api.OutcomeFailed, Detail: "the nightly backup is turned off on this node"}
	r.form("/backup/now", c, nil)
	body := r.get(t, "/backup", c)
	if !strings.Contains(body, "turned off on this node") || strings.Contains(card(body, "schedule"), "hx-get") {
		t.Errorf("a refused Back up now:\n%s", card(body, "schedule"))
	}
}

// THE FOLDER card: the current folder in the field, Change, no rules; one off-site line --
// uploaded, waiting, none found, or (Linux) Briard can't tell; the adopt prompt for a folder
// another key made; and where the earlier backups are.
func TestTheFolderCard(t *testing.T) {
	r, c, _ := backupRig(t)
	nights := []dashboard.BackupRun{{At: time.Date(2026, 10, 10, 2, 4, 0, 0, time.UTC), Snapshot: "abc", BytesAdded: 5 << 20}}
	const folder = `C:\Users\ana\OneDrive\Documents\Briard Backup`
	const advice = "keep the folder in a synced drive"
	folderCard := func(b dashboard.Backup) string {
		t.Helper()
		r.hostBacksUp(t, b)
		page := r.get(t, "/backup", c)
		return page[strings.Index(page, "<h2>Folder</h2>"):]
	}
	base := dashboard.Backup{Folder: folder, On: true, Key: testKey, KeySaved: true, Nights: nights}

	s := folderCard(base)
	if !strings.Contains(s, `value="`+folder+`"`) || !strings.Contains(s, `method="get" action="/backup/folder"`) || strings.Contains(s, "It must already exist") {
		t.Errorf("the field does not hold the folder, or the rules are still there:\n%s", s)
	}
	b := base
	b.OffsiteIn, b.OffsiteSynced = "OneDrive", true
	if s := folderCard(b); !strings.Contains(s, `<p class="ok">In OneDrive. The newest backup has left this machine.</p>`) || strings.Contains(s, advice) {
		t.Errorf("uploaded:\n%s", s)
	}
	b.OffsiteSynced = false
	if s := folderCard(b); !strings.Contains(s, "Waiting for OneDrive to upload the newest backup.") || !strings.Contains(s, "while someone is signed in") || strings.Contains(s, advice) {
		t.Errorf("waiting:\n%s", s)
	}
	if s := folderCard(base); !strings.Contains(s, advice) || !strings.Contains(s, "OneDrive, Dropbox") || strings.Contains(s, "can't tell") {
		t.Errorf("none found:\n%s", s)
	}
	b = base
	b.OffsiteUnknown = true
	if s := folderCard(b); !strings.Contains(s, "Briard can't tell whether this folder is copied off this machine.") || strings.Contains(s, "(OneDrive") {
		t.Errorf("Linux:\n%s", s)
	}
	b = base
	b.Nights = []dashboard.BackupRun{{At: nights[0].At, Error: "the folder holds a backup made with another key", WrongKey: true}}
	if s := folderCard(b); !strings.Contains(s, `action="/backup/adopt"`) || !strings.Contains(s, "Enter its key") {
		t.Errorf("another key's backup in the folder: no adopt prompt:\n%s", s)
	}
	b = base
	b.Previous = "/home/ana/Old"
	if s := folderCard(b); !strings.Contains(s, "earlier backups are still in <code>/home/ana/Old</code>") {
		t.Errorf("the earlier folder:\n%s", s)
	}
}

// answerFolder answers the page's backup-folder with check, or refuses with why.
func answerFolder(port *fakePort, check *dashboard.BackupFolderCheck, why string) {
	if check == nil {
		port.answer <- api.DirectiveOutcome{State: api.OutcomeFailed, Detail: why}
		return
	}
	raw, _ := json.Marshal(check)
	port.answer <- api.DirectiveOutcome{State: api.OutcomeDone, Detail: string(raw)}
}

// CHANGE READS THE FOLDER FIRST and says what is there -- or why it cannot be used, in place of
// the question -- and only the confirmation moves the backup: with the key, for a folder another
// key made.
func TestChangeReadsTheFolderFirst(t *testing.T) {
	r, c, port := backupRig(t)
	r.hostBacksUp(t, dashboard.Backup{Folder: "/home/ana/Briard Backup", On: true, Key: testKey, KeySaved: true})
	const to = "/home/ana/Dropbox/Briard Backup"
	check := func(answer *dashboard.BackupFolderCheck, why string) string {
		t.Helper()
		port.mu.Lock()
		port.asked = nil
		port.mu.Unlock()
		answerFolder(port, answer, why)
		page := r.get(t, "/backup/folder?"+url.Values{"folder": {" " + to + " "}}.Encode(), c)
		port.mu.Lock()
		defer port.mu.Unlock()
		var p api.BackupFolder
		if len(port.asked) == 1 {
			_ = json.Unmarshal([]byte(port.asked[0].Payload), &p)
		}
		if len(port.asked) != 1 || port.asked[0].Kind != api.DirectiveBackupFolder || p.Folder != to || p.Confirm {
			t.Fatalf("asked %+v; want one unconfirmed backup-folder of %q", port.asked, to)
		}
		return page
	}

	page := check(nil, "/home/ana/Dropbox/Briard Backup already holds other files: choose an empty folder")
	if !strings.Contains(page, "That folder cannot be used") || !strings.Contains(page, "already holds other files") || strings.Contains(page, "Change folder") {
		t.Errorf("a refusal:\n%s", page)
	}
	page = check(&dashboard.BackupFolderCheck{Folder: to, Empty: true, Previous: "/home/ana/Briard Backup"}, "")
	for _, want := range []string{"Future backups will be stored in <code>" + to + "</code>. This folder is empty.",
		"Backups already in <code>/home/ana/Briard Backup</code> stay where they are.", `action="/backup/folder"`, "Change folder", `href="/backup">Cancel`} {
		if !strings.Contains(page, want) {
			t.Errorf("empty: lacks %q:\n%s", want, page)
		}
	}
	page = check(&dashboard.BackupFolderCheck{Folder: to, Own: 3, Earlier: 2, Newest: time.Date(2026, 10, 30, 2, 0, 0, 0, time.Local)}, "")
	for _, want := range []string{"holds 3 of this household&#39;s backups, the newest from Fri 30 Oct, 02:00; Briard continues them.", "It also holds 2 from an earlier install, kept as they are."} {
		if !strings.Contains(page, want) {
			t.Errorf("this household's: lacks %q:\n%s", want, page)
		}
	}
	if strings.Contains(page, "stay where they are") {
		t.Error("an old folder holding no backup is named")
	}
	page = check(&dashboard.BackupFolderCheck{Folder: to, OtherKey: true}, "")
	if !strings.Contains(page, "holds backups made with another key") || !strings.Contains(page, `name="key" form="backup-folder-confirm" required`) {
		t.Errorf("another key's: no key field:\n%s", page)
	}

	// The confirmation: one confirmed backup-folder, the key with it; the host's refusal on the page.
	port.mu.Lock()
	port.asked = nil
	port.mu.Unlock()
	port.answer <- api.DirectiveOutcome{State: api.OutcomeFailed, Detail: "the folder was not changed: that key does not open the backup in this folder"}
	if resp := r.form("/backup/folder", c, url.Values{"folder": {to}, "key": {" theirs "}}); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("confirm = %d", resp.StatusCode)
	}
	port.mu.Lock()
	var p api.BackupFolder
	if len(port.asked) == 1 {
		_ = json.Unmarshal([]byte(port.asked[0].Payload), &p)
	}
	port.mu.Unlock()
	if want := (api.BackupFolder{Folder: to, Key: "theirs", Confirm: true}); p != want {
		t.Errorf("confirmed %+v, want %+v", p, want)
	}
	if body := r.get(t, "/backup", c); !strings.Contains(body, "that key does not open the backup") {
		t.Error("the host's refusal of the move is not shown")
	}
	if resp := r.form("/backup/folder", c, url.Values{"folder": {"  "}}); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("an empty folder = %d, want 400", resp.StatusCode)
	}
}

// THE BACKUPS card: the folder's backups newest first and the run that failed since the newest,
// each with when, what this machine's run added (none for an earlier install's), its status and
// its Details; the retention under it. The page does not wait for it: the card loads itself, or,
// with no script, renders in the page behind its link.
func TestTheBackupsCard(t *testing.T) {
	r, c, port := backupRig(t)
	at := time.Date(2026, 10, 30, 2, 0, 0, 0, time.Local)
	r.hostBacksUp(t, dashboard.Backup{Folder: "/home/ana/Briard Backup", On: true, Key: testKey, KeySaved: true, Nights: []dashboard.BackupRun{
		{At: at.Add(12 * time.Hour), Error: "the backup folder is not there"},
		{At: at, Snapshot: "bbbb2222ffffffff", BytesAdded: 301 << 10},
		{At: at.Add(-48 * time.Hour), Error: "older, and not shown"},
	}})
	answerByKind(port, "")
	page := r.get(t, "/backup", c)
	if !strings.Contains(page, `hx-get="/f/backups" hx-trigger="load"`) || !strings.Contains(page, `href="/backup?list"`) {
		t.Fatalf("the page does not load its backups card, or offers no way without a script:\n%s", page)
	}
	port.mu.Lock()
	asked := len(port.asked)
	port.mu.Unlock()
	if asked != 0 {
		t.Errorf("rendering the page asked the host %d times; the card asks for itself", asked)
	}
	for _, body := range []string{r.get(t, "/f/backups", c), card(r.get(t, "/backup?list", c), "backups")} {
		rows := strings.Split(body, "<tr>")
		if len(rows) != 5 { // the head and three rows
			t.Fatalf("want three rows, the failed run first:\n%s", body)
		}
		for i, want := range []string{"Fri 30 Oct, 14:00</td><td></td><td><span class=\"bad\">did not finish: the backup folder is not there",
			"Fri 30 Oct, 02:00</td><td>301 kB</td><td><span class=\"badge\" data-variant=\"secondary\">ok</span></td><td><a class=\"btn\" data-variant=\"outline\" href=\"/backup/bbbb2222\">Details</a>",
			"</td><td></td><td><span class=\"badge\" data-variant=\"outline\">from an earlier install</span></td><td><a class=\"btn\" data-variant=\"outline\" href=\"/backup/aaaa1111\">Details</a>"} {
			if !strings.Contains(rows[i+2], want) {
				t.Errorf("row %d lacks %q:\n%s", i, want, rows[i+2])
			}
		}
		if strings.Contains(rows[2], "Details") || strings.Contains(body, "older, and not shown") {
			t.Errorf("a failed run has Details, or one older than the newest backup is listed:\n%s", body)
		}
		if !strings.Contains(body, "Briard keeps the backups of the last 2 days and 1 weekly.") || strings.Contains(body, "every 5s") {
			t.Errorf("no retention line, or an idle card polls:\n%s", body)
		}
		if strings.Contains(strings.ToLower(body), "night") {
			t.Errorf("the card says night:\n%s", body)
		}
	}
}

// A BACKUP'S DETAILS: when, by whom, what it added and holds, and its apps -- installed here or
// not -- each with a Restore into the confirmation. One no longer in the folder is not found.
func TestABackupsDetails(t *testing.T) {
	r, c, port := backupRig(t)
	r.hostBacksUp(t, dashboard.Backup{Folder: "/home/ana/Briard Backup", On: true, Key: testKey, KeySaved: true})
	answerByKind(port, "")
	page := r.get(t, "/backup/bbbb2222", c)
	for _, want := range []string{"Backup of Fri 30 Oct, 02:00", "Made by this machine.", "It added 301 kB, and holds 3.0 GB of data.",
		"Home Assistant 2026.9.4", "Mosquitto 2.0.22", "Fri 30 Oct, 01:34", `action="/backup/restore"`, `name="backup" value="bbbb2222"`, `name="app" value="mosquitto"`} {
		if !strings.Contains(page, want) {
			t.Errorf("the details lack %q:\n%s", want, page)
		}
	}
	if strings.Count(page, "not installed") != 1 {
		t.Errorf("want mosquitto alone marked not installed:\n%s", page)
	}
	if page := r.get(t, "/backup/aaaa1111", c); !strings.Contains(page, "Made by an earlier install.") || strings.Contains(page, "It added") {
		t.Errorf("an earlier install's, with no summary:\n%s", page)
	}
	for _, id := range []string{"cccc3333", "not-an-id"} {
		if resp := r.do("GET", "/backup/"+id, c, nil); resp.StatusCode != http.StatusNotFound {
			t.Errorf("/backup/%s = %d, want 404", id, resp.StatusCode)
		}
	}
	if resp := r.do("GET", "/backup/bbbb2222", nil, nil); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("no session = %d, want 401", resp.StatusCode)
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
	var cardBody string
	for i := 0; i < 200; i++ {
		if cardBody = r.get(t, "/f/backups", c); strings.Contains(cardBody, "is back as it was in the backup of Fri 30 Oct, 02:00") {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !strings.Contains(cardBody, "is back as it was") || !strings.Contains(cardBody, `href="/apps/home-assistant"`) {
		t.Fatalf("the card never said the restore ended:\n%s", cardBody)
	}
	if again := r.get(t, "/f/backups", c); strings.Contains(again, "is back as it was") || !strings.Contains(again, "bbbb2222") {
		t.Errorf("the ending was said twice, or the list did not come back:\n%s", again)
	}
}

// A restore that fails says why, and stays said until the next restore of that app.
func TestAFailedRestoreFromTheBackupSaysWhy(t *testing.T) {
	r, c, port := backupRig(t)
	r.hostBacksUp(t, dashboard.Backup{Folder: "/home/ana/Briard Backup", On: true, Key: testKey, KeySaved: true})
	answerByKind(port, "")
	postForm(t, r, "/backup/restore", c, url.Values{"backup": {"bbbb2222"}, "app": {"mosquitto"}, "when": {"Fri 30 Oct, 02:00"}, "confirm": {"yes"}})
	var cardBody string
	for i := 0; i < 200; i++ {
		if cardBody = r.get(t, "/f/backups", c); strings.Contains(cardBody, "was not restored") {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !strings.Contains(cardBody, "Mosquitto was not restored: the image could not be fetched") {
		t.Fatalf("the card:\n%s", cardBody)
	}
	if again := r.get(t, "/f/backups", c); !strings.Contains(again, "was not restored") {
		t.Errorf("the failure was forgotten after one look:\n%s", again)
	}
}

// A new install's folder holds no backup until the first run: the card says so, and offers none.
func TestTheBackupsCardOfAFolderWithNoBackupYet(t *testing.T) {
	r, c, port := backupRig(t)
	r.hostBacksUp(t, dashboard.Backup{Folder: "/var/lib/briard/backup", On: true, Key: testKey, KeySaved: true})
	go func() {
		for range port.waiting {
			port.answer <- api.DirectiveOutcome{State: api.OutcomeDone, Detail: "[]"}
		}
	}()
	body := r.get(t, "/f/backups", c)
	if !strings.Contains(body, "The folder holds no backup yet.") || strings.Contains(body, "Details") || strings.Contains(body, "could not be read") {
		t.Errorf("an empty folder's card:\n%s", body)
	}
}

// A SETTING'S PAGE SHOWS IT DONE: the host pushes its view on its next tick, so the answer to a
// move waits for the view that has the new folder before sending the browser back -- and a host
// that never pushes leaves the page as it was once the wait is up, rather than holding it.
func TestAMoveComesBackToThePageShowingIt(t *testing.T) {
	r, c, port := backupRig(t)
	r.app.viewWait = 5 * time.Second
	const old, to = "/home/ana/Briard Backup", "/home/ana/Dropbox/Briard Backup"
	r.hostBacksUp(t, dashboard.Backup{Folder: old, On: true, Key: testKey, KeySaved: true})
	port.answer <- api.DirectiveOutcome{State: api.OutcomeDone, Detail: "backups are now stored in " + to}
	go func() {
		time.Sleep(500 * time.Millisecond) // the host's next tick
		raw, _ := json.Marshal(dashboard.Backup{Folder: to, On: true, Key: testKey, KeySaved: true, Previous: old})
		_ = os.WriteFile(r.app.backupPath, raw, 0o600)
	}()
	if resp := r.form("/backup/folder", c, url.Values{"folder": {to}}); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("confirm = %d", resp.StatusCode)
	}
	if got := r.get(t, "/backup", c); !strings.Contains(got, `value="`+to+`"`) {
		t.Errorf("the page after the move still shows the old folder:\n%s", card(got, "backup-folder-form"))
	}

	r.app.viewWait = 300 * time.Millisecond
	port.answer <- api.DirectiveOutcome{State: api.OutcomeDone}
	start := time.Now()
	r.form("/backup/off", c, nil) // the host never pushes "off"
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("a host that does not push held the page %s", d)
	}
}
