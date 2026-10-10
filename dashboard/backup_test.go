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
	for _, want := range []string{"earlier backups are still in <code>/home/ana/Briard Backup</code>", "the folder holds 3.0 GB", "Recent nights", "5 MB new", "did not finish: the backup folder is not there"} {
		if !strings.Contains(body, want) {
			t.Errorf("the card lacks %q", want)
		}
	}
	r.hostBacksUp(t, dashboard.Backup{Folder: "/new", Previous: "/new", On: true})
	if body := r.page(c); strings.Contains(body, "earlier backups are still in") {
		t.Error("a previous folder that is the current one is named as somewhere else")
	}
}
