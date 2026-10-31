package main

// THE STATE GALLERY: every page in every state it can be in, rendered to files a person clicks
// through and a script screenshots. The dashboard's look is judged from renders, never from
// prose, and this table is the record of which states exist -- a section with a state missing
// here has a state nobody has looked at. Skipped unless DASHBOARD_GALLERY names the directory to
// write into (a test tool's switch, not product configuration); it asserts only that every state
// renders, so a template error in a rarely reached branch fails here before a household finds it.
//
// Every render runs on a fixed clock and fixed fixtures, so two runs of the gallery differ only
// where the templates do -- which is what a screenshot diff is for.

import (
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"briard.io/agent/quadlet"
	"briard.io/agent/services"
	"briard.io/shared/api"
	"briard.io/shared/casa"
	"briard.io/shared/dashboard"
	"briard.io/shared/notify"
	"briard.io/shared/routes"
)

// galleryClock is when every gallery page is rendered; the fixtures are dated against it.
var galleryClock = time.Date(2026, 10, 28, 9, 30, 0, 0, time.UTC)

const (
	galleryPhone  = "Mozilla/5.0 (iPhone; CPU iPhone OS 19_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/19.0 Mobile/15E148 Safari/604.1"
	galleryLaptop = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/142.0.0.0 Safari/537.36 Edg/142.0.0.0"
	galleryName   = "brave-elf.briard.casa"
	galleryFolder = "/home/ana/Briard Backup"
)

// galleryState is one row of the gallery: a page in one state, and how to put a fresh rig there.
type galleryState struct {
	name, note string
	// render puts the rig in the state and returns the page to fetch: its path, and the cookie
	// to open it with (nil for a browser the home does not trust).
	render func(t *testing.T, r *rig, c *http.Cookie, port *fakePort) (string, *http.Cookie)
}

// galleryRig is a rig on the gallery's clock, trusted from a Windows laptop, with every file
// the host pushes pointed into the rig's directory and a fake admin port that answers nothing
// until a state says so.
func galleryRig(t *testing.T) (*rig, *http.Cookie, *fakePort) {
	t.Helper()
	r := newRig(t)
	raw, _ := json.Marshal(dashboard.Handoff{Code: r.code, Name: "Kostas", Username: "kostas", Language: "el", Issued: galleryClock})
	must(t, os.WriteFile(r.app.handoffPath, raw, 0o600))
	r.app.now = func() time.Time { return galleryClock.Add(time.Minute) }
	r.app.alertsPath = filepath.Join(r.dir, "alerts.json")
	r.app.contactPath = filepath.Join(r.dir, ".host-contact")
	r.app.casaPath = filepath.Join(r.dir, "casa.json")
	r.app.backupPath = filepath.Join(r.dir, "backup.json")
	port := newFakePort()
	r.app.port = port
	return r, r.trustAs(galleryLaptop), port
}

// everyday is the home as it stands most mornings: Home Assistant set up, the name registered
// and resolving, last night's backup fine and the key saved, nothing open, a phone trusted too.
func everyday(t *testing.T, r *rig) {
	t.Helper()
	r.ha.mu.Lock()
	for k := range r.ha.done {
		r.ha.done[k] = true
	}
	r.ha.mu.Unlock()
	r.hostSays(t, dashboard.Casa{State: casa.ClaimRegistered, Name: galleryName, Email: "ana@example.org",
		CertUntil: galleryClock.Add(80 * 24 * time.Hour), Resolves: dashboard.CasaResolves})
	r.hostBacksUp(t, dashboard.Backup{Folder: galleryFolder, On: true, Key: testKey, KeySaved: true, Nights: galleryNights(t, "")})
	r.contact(t, galleryClock)
	r.hostPushes(t, []notify.Record{
		galleryAlert(-3*24*time.Hour, "clock", notify.Open, notify.Warning, "Clock not synchronised", "The machine's clock is 4 minutes off and no time server answers."),
		galleryAlert(-3*24*time.Hour+10*time.Minute, "clock", notify.Resolved, "", "Clock synchronised", ""),
		galleryAlert(-24*time.Hour, "upgrade:os", notify.Event, notify.Info, "Updated", "Briard updated itself to v3.20261027.46bc529."),
	})
	r.reissue(galleryPhone, nil)
}

// galleryNights is a week of nightly backups, newest first; lastErr, when set, is why the newest
// did not finish.
func galleryNights(t *testing.T, lastErr string) []dashboard.BackupRun {
	t.Helper()
	var nights []dashboard.BackupRun
	for i := range 7 {
		at := time.Date(2026, 10, 28-i, 2, 4, 0, 0, time.UTC)
		n := dashboard.BackupRun{At: at, Snapshot: fmt.Sprintf("night-%d", i), BytesAdded: int64(40+i*37) << 20, Size: 3<<30 + int64(200-i*30)<<20}
		if i == 0 && lastErr != "" {
			n.Error, n.BytesAdded, n.Size = lastErr, 0, 0
		}
		nights = append(nights, n)
	}
	return nights
}

func galleryAlert(ago time.Duration, key string, kind notify.Kind, sev notify.Severity, title, body string) notify.Record {
	return notify.Record{Alert: notify.Alert{Key: key, Kind: kind, Severity: sev, Title: title, Body: body}, At: galleryClock.Add(ago)}
}

// notInstalled takes the routes table away: nothing is installed yet.
func notInstalled(t *testing.T, r *rig) {
	t.Helper()
	must(t, os.Remove(filepath.Join(r.dir, "routes.json")))
}

// startInstall presses "Install Home Assistant" and waits for the directive to reach the port,
// where it stays until the state answers it.
func startInstall(t *testing.T, r *rig, c *http.Cookie, port *fakePort) {
	t.Helper()
	r.do("POST", "/install/home-assistant", c, nil)
	select {
	case <-port.waiting:
	case <-time.After(5 * time.Second):
		t.Fatal("the install never reached the port")
	}
}

// galleryOffer is the host's view with Home Assistant installed at galleryRing's 2026.8.0 and
// the catalog's 2026.9.4 on offer.
func galleryOffer(t *testing.T, r *rig) {
	t.Helper()
	c := testCatalog()
	c.Apps[0].Installed, c.Apps[0].Running, c.Apps[0].Version, c.Apps[0].Update = true, "2026.8.0", "2026.9.4", true
	r.hostLists(t, c)
}

// startUpdate is the Update button, held at the port until the state answers it.
func startUpdate(t *testing.T, r *rig, c *http.Cookie, port *fakePort) {
	t.Helper()
	r.do("POST", "/update/home-assistant", c, nil)
	select {
	case <-port.waiting:
	case <-time.After(5 * time.Second):
		t.Fatal("the update never reached the port")
	}
}

// galleryRing is the history the host reports: a quiet day on 2026.7.1, the update to 2026.8.0
// taken quiesced, and the start after it; with broken set, that update did not start cleanly
// and the app is unhealthy now.
func galleryRing(broken bool) string {
	older := time.Date(2026, 10, 20, 3, 0, 0, 0, time.UTC)
	update := time.Date(2026, 10, 27, 3, 0, 0, 0, time.UTC)
	reasons := []quadlet.Reason{{Kind: quadlet.ReasonAppUpdate, What: "Updated to 2026.8.0"}}
	health := services.Healthy
	if broken {
		reasons = append(reasons, quadlet.Reason{Kind: quadlet.ReasonUnhealthy})
		health = services.Unhealthy
	}
	manifestOf := func(v string) string {
		return `{"name":"home-assistant","version":"` + v + `","size":622000000,"installedSize":2490000000,"containers":[{"name":"app",` +
			`"image":"ghcr.io/x/ha@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",` +
			`"mount":"/config","primary":true,"port":8123,"healthPath":"/"}]}`
	}
	entries := []quadlet.SnapshotEntry{
		{Member: quadlet.SnapshotMember("home-assistant", quadlet.TriggerClock, older),
			Meta: quadlet.SnapshotMeta{Service: "home-assistant", Trigger: quadlet.TriggerClock, TakenAt: older, Consistency: quadlet.Crash, Manifest: manifestOf("2026.7.1"),
				Event: &quadlet.Event{At: older.Add(20 * time.Hour), Reasons: []quadlet.Reason{{Kind: quadlet.ReasonQuiet}}}}},
		{Member: quadlet.SnapshotMember("home-assistant", quadlet.TriggerAppUpdateBefore, update),
			Meta: quadlet.SnapshotMeta{Service: "home-assistant", Trigger: quadlet.TriggerAppUpdateBefore, TakenAt: update, Consistency: quadlet.Quiesced, Manifest: manifestOf("2026.7.1"),
				Event: &quadlet.Event{At: update, Reasons: reasons}}},
		{Member: quadlet.SnapshotMember("home-assistant", quadlet.TriggerStart, update.Add(5*time.Minute)),
			Meta: quadlet.SnapshotMeta{Service: "home-assistant", Trigger: quadlet.TriggerStart, TakenAt: update.Add(5 * time.Minute), Consistency: quadlet.Quiesced, Manifest: manifestOf("2026.8.0"), Health: health}},
	}
	raw, _ := json.Marshal(entries)
	return string(raw)
}

// answerHistory lets the port answer every service-members with the given ring.
func answerHistory(port *fakePort, ring string) {
	go func() {
		for range port.waiting {
			port.answer <- api.DirectiveOutcome{State: api.OutcomeDone, Detail: ring}
		}
	}()
}

// home and section are the renders of the sidebar.s pages with the page.s own cookie: a state is
// listed under the page its section lives on.
func home(_ *testing.T, _ *rig, c *http.Cookie, _ *fakePort) (string, *http.Cookie) { return "/", c }
func section(path string) func(*testing.T, *rig, *http.Cookie, *fakePort) (string, *http.Cookie) {
	return func(_ *testing.T, _ *rig, c *http.Cookie, _ *fakePort) (string, *http.Cookie) { return path, c }
}

var (
	apps     = section("/apps")
	backup   = section("/backup")
	address  = section("/secure")
	devices  = section("/devices")
	activity = section("/activity")
)

var galleryStates = []galleryState{
	// THE HOME PAGE, through a household's life with it.
	{"home-first-open", "The first open after the install: Home Assistant ready for its first open, the name offered, the backup's key not yet saved, this laptop the only device.",
		func(t *testing.T, r *rig, c *http.Cookie, port *fakePort) (string, *http.Cookie) {
			r.hostSays(t, dashboard.Casa{Name: galleryName})
			r.hostBacksUp(t, dashboard.Backup{Folder: galleryFolder, On: true, Key: testKey})
			r.contact(t, galleryClock)
			r.hostPushes(t, nil)
			return home(t, r, c, port)
		}},
	{"home-everyday", "Most mornings: everything set up, nothing open, two recent alerts resolved.",
		func(t *testing.T, r *rig, c *http.Cookie, port *fakePort) (string, *http.Cookie) {
			everyday(t, r)
			return home(t, r, c, port)
		}},
	{"home-alert-open", "One alert open (disk space), on top of the everyday page.",
		func(t *testing.T, r *rig, c *http.Cookie, port *fakePort) (string, *http.Cookie) {
			everyday(t, r)
			r.hostPushes(t, []notify.Record{
				galleryAlert(-24*time.Hour, "upgrade:os", notify.Event, notify.Info, "Updated", "Briard updated itself to v3.20261027.46bc529."),
				galleryAlert(-40*time.Minute, "disk", notify.Open, notify.Warning, "Disk space low", "The data volume has 1.2 GB free; Home Assistant's recorder stops writing below 1 GB."),
			})
			return home(t, r, c, port)
		}},
	{"home-host-away", "The host agent has not been heard from for five minutes: the one banner, and no stale list.",
		func(t *testing.T, r *rig, c *http.Cookie, port *fakePort) (string, *http.Cookie) {
			everyday(t, r)
			r.contact(t, galleryClock.Add(-5*time.Minute))
			return home(t, r, c, port)
		}},
	{"home-older-host", "A host that pushed nothing (an older agent): no alerts, name or backup card at all.",
		func(t *testing.T, r *rig, c *http.Cookie, port *fakePort) (string, *http.Cookie) {
			r.ha.mu.Lock()
			for k := range r.ha.done {
				r.ha.done[k] = true
			}
			r.ha.mu.Unlock()
			return home(t, r, c, port)
		}},
	// HOME ASSISTANT's card, every state.
	{"home-first-run", "The first screen: nothing installed, the name and the key both still owed -- and only the chooser shows, one thing at a time.",
		func(t *testing.T, r *rig, c *http.Cookie, port *fakePort) (string, *http.Cookie) {
			notInstalled(t, r)
			r.hostSays(t, dashboard.Casa{Name: galleryName})
			r.hostBacksUp(t, dashboard.Backup{Folder: galleryFolder, On: true, Key: testKey})
			r.contact(t, galleryClock)
			r.hostPushes(t, nil)
			return home(t, r, c, port)
		}},
	{"home-ha-not-installed", "Nothing installed yet; the name skipped.",
		func(t *testing.T, r *rig, c *http.Cookie, port *fakePort) (string, *http.Cookie) {
			notInstalled(t, r)
			r.hostSays(t, dashboard.Casa{Name: galleryName})
			must(t, r.app.writeState(casaSkipFile, casaSkip{Skipped: true, At: galleryClock}))
			r.contact(t, galleryClock)
			r.hostPushes(t, nil)
			return home(t, r, c, port)
		}},
	{"home-ha-installing", "The install in flight, with the pull's progress, the address step beside it.",
		func(t *testing.T, r *rig, c *http.Cookie, port *fakePort) (string, *http.Cookie) {
			notInstalled(t, r)
			r.hostSays(t, dashboard.Casa{Name: galleryName})
			root := t.TempDir()
			must(t, os.MkdirAll(filepath.Join(root, "overlay-layers"), 0o755))
			must(t, os.MkdirAll(filepath.Join(root, "tmp"), 0o755))
			r.app.pulls = pullPaths{records: t.TempDir(), layers: filepath.Join(root, "overlay-layers", "layers.json"), tmp: filepath.Join(root, "tmp")}
			started := galleryClock.Add(-time.Minute)
			rec, _ := json.Marshal(dashboard.Pull{Service: "home-assistant", Size: 650e6, InstalledSize: 2500e6, Started: started})
			must(t, os.WriteFile(filepath.Join(r.app.pulls.records, "pull-home-assistant.json"), rec, 0o600))
			layers, _ := json.Marshal([]map[string]any{
				{"id": "c", "compressed-size": 150e6, "created": started.Add(10 * time.Second).Format(time.RFC3339Nano)},
				{"id": "d", "compressed-size": 110e6, "created": started.Add(20 * time.Second).Format(time.RFC3339Nano)},
			})
			must(t, os.WriteFile(r.app.pulls.layers, layers, 0o644))
			startInstall(t, r, c, port)
			return home(t, r, c, port)
		}},
	{"home-ha-installing-no-progress", "The install in flight on a host that left no pull record: no bar.",
		func(t *testing.T, r *rig, c *http.Cookie, port *fakePort) (string, *http.Cookie) {
			notInstalled(t, r)
			r.app.pulls = pullPaths{records: t.TempDir(), layers: filepath.Join(t.TempDir(), "none"), tmp: t.TempDir()}
			startInstall(t, r, c, port)
			return home(t, r, c, port)
		}},
	{"home-ha-install-failed", "The host refused the install: the reason, the CLI, and the button back.",
		func(t *testing.T, r *rig, c *http.Cookie, port *fakePort) (string, *http.Cookie) {
			notInstalled(t, r)
			startInstall(t, r, c, port)
			port.answer <- api.DirectiveOutcome{State: api.OutcomeFailed, Detail: "the data volume has 1.9 GB free; Home Assistant needs 3.5 GB"}
			deadline := time.Now().Add(5 * time.Second)
			for !strings.Contains(r.page(c), "Could not install") && time.Now().Before(deadline) {
				time.Sleep(20 * time.Millisecond)
			}
			return home(t, r, c, port)
		}},
	{"home-ha-starting", "Home Assistant answers but is not RUNNING yet: the button waits.",
		func(t *testing.T, r *rig, c *http.Cookie, port *fakePort) (string, *http.Cookie) {
			r.ha.mu.Lock()
			r.ha.state = "STARTING"
			r.ha.mu.Unlock()
			return home(t, r, c, port)
		}},
	{"home-ha-unreachable", "Home Assistant is routed but does not answer HTTP yet.",
		func(t *testing.T, r *rig, c *http.Cookie, port *fakePort) (string, *http.Cookie) {
			tbl := routes.Table{Services: []routes.Service{{
				Name: "home-assistant", Hosts: []string{"briard-brave-elf-home-assistant.local"},
				Address: "127.0.0.1", Health: "http://:1/manifest.json",
				Routes: []routes.Route{{Listen: routes.ListenName, To: "http://:1"}},
			}}}
			raw, _ := json.Marshal(tbl)
			must(t, os.WriteFile(filepath.Join(r.dir, "routes.json"), raw, 0o644))
			return home(t, r, c, port)
		}},
	// THE NAME's card, every state.
	{"home-casa-pending", "The claim sent: waiting for the email's link.",
		func(t *testing.T, r *rig, c *http.Cookie, port *fakePort) (string, *http.Cookie) {
			r.hostSays(t, dashboard.Casa{State: casa.ClaimPending, Name: galleryName, Email: "ana@example.org"})
			return home(t, r, c, port)
		}},
	{"home-casa-refused", "The host refused the claim: the reason above the form.",
		func(t *testing.T, r *rig, c *http.Cookie, port *fakePort) (string, *http.Cookie) {
			r.hostSays(t, dashboard.Casa{State: casa.ClaimRefused, Name: galleryName, Reason: "brave-elf.briard.casa is registered to another email; rename this home and claim again"})
			return home(t, r, c, port)
		}},
	{"home-casa-expired", "The link expired before it was opened.",
		func(t *testing.T, r *rig, c *http.Cookie, port *fakePort) (string, *http.Cookie) {
			r.hostSays(t, dashboard.Casa{State: casa.ClaimExpired, Name: galleryName})
			return home(t, r, c, port)
		}},
	{"home-casa-asked-failed", "The claim could not be relayed to the host (no admin port).",
		func(t *testing.T, r *rig, c *http.Cookie, port *fakePort) (string, *http.Cookie) {
			r.hostSays(t, dashboard.Casa{Name: galleryName})
			port.mu.Lock()
			port.noPort = true
			port.mu.Unlock()
			r.form("/casa/claim", c, url.Values{"email": {"ana@example.org"}})
			return home(t, r, c, port)
		}},
	{"address-checking", "Registered, and the resolve check has not answered yet.",
		func(t *testing.T, r *rig, c *http.Cookie, port *fakePort) (string, *http.Cookie) {
			r.hostSays(t, dashboard.Casa{State: casa.ClaimRegistered, Name: galleryName, Email: "ana@example.org"})
			return address(t, r, c, port)
		}},
	{"home-casa-blocked", "Registered, but the router's rebind protection drops the name.",
		func(t *testing.T, r *rig, c *http.Cookie, port *fakePort) (string, *http.Cookie) {
			r.hostSays(t, dashboard.Casa{State: casa.ClaimRegistered, Name: galleryName, Email: "ana@example.org", Resolves: dashboard.CasaBlocked})
			return home(t, r, c, port)
		}},
	{"address-cert-pending", "Registered and resolving; the certificate not yet issued.",
		func(t *testing.T, r *rig, c *http.Cookie, port *fakePort) (string, *http.Cookie) {
			r.hostSays(t, dashboard.Casa{State: casa.ClaimRegistered, Name: galleryName, Email: "ana@example.org", Resolves: dashboard.CasaResolves})
			return address(t, r, c, port)
		}},
	// THE BACKUP's card, every state.
	{"backup-off", "The nightly backup turned off; the folder kept.",
		func(t *testing.T, r *rig, c *http.Cookie, port *fakePort) (string, *http.Cookie) {
			everyday(t, r)
			r.hostBacksUp(t, dashboard.Backup{Folder: galleryFolder, Key: testKey, KeySaved: true, Nights: galleryNights(t, "")})
			return backup(t, r, c, port)
		}},
	{"backup-failed-night", "Last night did not finish; the earlier backups are in another folder.",
		func(t *testing.T, r *rig, c *http.Cookie, port *fakePort) (string, *http.Cookie) {
			everyday(t, r)
			r.hostBacksUp(t, dashboard.Backup{Folder: galleryFolder, Previous: "/home/ana/Old Briard Backup", On: true, Key: testKey, KeySaved: true,
				Nights: galleryNights(t, "the backup folder is not there")})
			return backup(t, r, c, port)
		}},
	{"backup-wrong-key", "The folder holds a backup this key does not open: adopt it, or move.",
		func(t *testing.T, r *rig, c *http.Cookie, port *fakePort) (string, *http.Cookie) {
			everyday(t, r)
			nights := galleryNights(t, "the folder holds a backup made with another key")
			nights[0].WrongKey = true
			r.hostBacksUp(t, dashboard.Backup{Folder: galleryFolder, On: true, Key: testKey, KeySaved: true, Nights: nights})
			return backup(t, r, c, port)
		}},
	{"backup-asked-refused", "The host refused the last thing asked of the card (a folder it will not use).",
		func(t *testing.T, r *rig, c *http.Cookie, port *fakePort) (string, *http.Cookie) {
			everyday(t, r)
			port.mu.Lock()
			port.fail = errors.New("the folder /root/x belongs to root; choose one of your own")
			port.mu.Unlock()
			r.form("/backup/folder", c, url.Values{"folder": {"/root/x"}})
			return backup(t, r, c, port)
		}},
	{"backup-running", "A backup running now: the Schedule follows it.",
		func(t *testing.T, r *rig, c *http.Cookie, port *fakePort) (string, *http.Cookie) {
			everyday(t, r)
			r.hostBacksUp(t, dashboard.Backup{Folder: galleryFolder, On: true, Key: testKey, KeySaved: true, Nights: galleryNights(t, ""), Running: galleryClock.Add(-2 * time.Minute)})
			return backup(t, r, c, port)
		}},
	{"backup-now-done", "Back up now has ended: said once, what it added.",
		func(t *testing.T, r *rig, c *http.Cookie, port *fakePort) (string, *http.Cookie) {
			everyday(t, r)
			port.answer <- api.DirectiveOutcome{State: api.OutcomeDone}
			r.form("/backup/now", c, nil)
			nights := append([]dashboard.BackupRun{{At: galleryClock, Snapshot: "now", BytesAdded: 301 << 10}}, galleryNights(t, "")...)
			r.hostBacksUp(t, dashboard.Backup{Folder: galleryFolder, On: true, Key: testKey, KeySaved: true, Nights: nights, Ended: &dashboard.BackupEnd{Started: galleryClock}})
			return backup(t, r, c, port)
		}},
	{"backup-linux", "On Linux: Briard cannot tell whether the folder is copied off the machine.",
		func(t *testing.T, r *rig, c *http.Cookie, port *fakePort) (string, *http.Cookie) {
			everyday(t, r)
			r.hostBacksUp(t, dashboard.Backup{Folder: galleryFolder, On: true, Key: testKey, KeySaved: true, Nights: galleryNights(t, ""), OffsiteUnknown: true})
			return backup(t, r, c, port)
		}},
	{"backup-list", "The Backups card rendered in the page (no script): a failed run, this household's backup, an earlier install's.",
		func(t *testing.T, r *rig, c *http.Cookie, port *fakePort) (string, *http.Cookie) {
			everyday(t, r)
			nights := galleryNights(t, "")
			nights[0].Snapshot = "bbbb2222"
			nights = append([]dashboard.BackupRun{{At: time.Date(2026, 10, 30, 14, 0, 0, 0, time.Local), Error: "the backup folder is not there"}}, nights...)
			r.hostBacksUp(t, dashboard.Backup{Folder: galleryFolder, On: true, Key: testKey, KeySaved: true, Nights: nights})
			answerByKind(port, "")
			return "/backup?list", c
		}},
	{"backup-details", "One backup: when, what it added and holds, its apps -- one not installed -- each with Restore.",
		func(t *testing.T, r *rig, c *http.Cookie, port *fakePort) (string, *http.Cookie) {
			everyday(t, r)
			answerByKind(port, "")
			return "/backup/bbbb2222", c
		}},
	{"backup-details-earlier", "An earlier install's backup, with no summary from restic.",
		func(t *testing.T, r *rig, c *http.Cookie, port *fakePort) (string, *http.Cookie) {
			everyday(t, r)
			answerByKind(port, "")
			return "/backup/aaaa1111", c
		}},
	{"backup-folder-empty", "Change into an empty folder: the confirmation.",
		func(t *testing.T, r *rig, c *http.Cookie, port *fakePort) (string, *http.Cookie) {
			everyday(t, r)
			answerFolder(port, &dashboard.BackupFolderCheck{Folder: "/home/ana/Dropbox/Briard Backup", Empty: true, Previous: galleryFolder}, "")
			return "/backup/folder?folder=" + url.QueryEscape("/home/ana/Dropbox/Briard Backup"), c
		}},
	{"backup-folder-ours", "Change into a folder holding this household's backups and an earlier install's.",
		func(t *testing.T, r *rig, c *http.Cookie, port *fakePort) (string, *http.Cookie) {
			everyday(t, r)
			answerFolder(port, &dashboard.BackupFolderCheck{Folder: "/home/ana/Dropbox/Briard Backup", Own: 3, Earlier: 2, Newest: galleryClock.Add(-7 * time.Hour), Previous: galleryFolder}, "")
			return "/backup/folder?folder=" + url.QueryEscape("/home/ana/Dropbox/Briard Backup"), c
		}},
	{"backup-folder-other-key", "Change into a folder another key made: its key, in the confirmation.",
		func(t *testing.T, r *rig, c *http.Cookie, port *fakePort) (string, *http.Cookie) {
			everyday(t, r)
			answerFolder(port, &dashboard.BackupFolderCheck{Folder: "/home/ana/Dropbox/Briard Backup", OtherKey: true, Previous: galleryFolder}, "")
			return "/backup/folder?folder=" + url.QueryEscape("/home/ana/Dropbox/Briard Backup"), c
		}},
	{"backup-folder-refused", "Change into a folder the host will not use: why, in place of the question.",
		func(t *testing.T, r *rig, c *http.Cookie, port *fakePort) (string, *http.Cookie) {
			everyday(t, r)
			answerFolder(port, nil, "/home/ana/Documents already holds other files: choose an empty folder, or one that holds these backups")
			return "/backup/folder?folder=" + url.QueryEscape("/home/ana/Documents"), c
		}},
	{"devices-many", "Four trusted devices, this laptop among them.",
		func(t *testing.T, r *rig, c *http.Cookie, port *fakePort) (string, *http.Cookie) {
			everyday(t, r)
			r.reissue("Mozilla/5.0 (Linux; Android 16; Pixel 9) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/142.0.0.0 Mobile Safari/537.36", nil)
			r.reissue("curl/8.9.1", nil)
			return devices(t, r, c, port)
		}},
	// THE SECTIONS, as they stand at the two ends of the home's life.
	{"address-first-open", "The web address page on the first open: the name offered, nothing registered.",
		func(t *testing.T, r *rig, c *http.Cookie, port *fakePort) (string, *http.Cookie) {
			r.hostSays(t, dashboard.Casa{Name: galleryName})
			r.hostBacksUp(t, dashboard.Backup{Folder: galleryFolder, On: true, Key: testKey})
			return address(t, r, c, port)
		}},
	{"address-everyday", "The web address page most mornings: registered, resolving, the certificate good.",
		func(t *testing.T, r *rig, c *http.Cookie, port *fakePort) (string, *http.Cookie) {
			everyday(t, r)
			return address(t, r, c, port)
		}},
	{"apps-everyday", "The apps page most mornings: Home Assistant installed, Mosquitto available.",
		func(t *testing.T, r *rig, c *http.Cookie, port *fakePort) (string, *http.Cookie) {
			everyday(t, r)
			return apps(t, r, c, port)
		}},
	{"apps-first-run", "The apps page before anything is installed: both available, two coming soon.",
		func(t *testing.T, r *rig, c *http.Cookie, port *fakePort) (string, *http.Cookie) {
			notInstalled(t, r)
			return apps(t, r, c, port)
		}},
	{"apps-wished", "A coming-soon app the household asked for: the wish recorded, the button now takes it back.",
		func(t *testing.T, r *rig, c *http.Cookie, port *fakePort) (string, *http.Cookie) {
			everyday(t, r)
			r.wished(t, "paperless-ngx", true)
			return apps(t, r, c, port)
		}},
	{"apps-wish-met", "The wished app gained a manifest: it moved to Available with the line that says so.",
		func(t *testing.T, r *rig, c *http.Cookie, port *fakePort) (string, *http.Cookie) {
			everyday(t, r)
			cat := testCatalog()
			cat.Apps[2].Available, cat.Apps[2].Wished = true, true
			r.hostLists(t, cat)
			return apps(t, r, c, port)
		}},
	{"apps-no-index", "The host has not read the catalog yet: what is installed, and the page says why that is all.",
		func(t *testing.T, r *rig, c *http.Cookie, port *fakePort) (string, *http.Cookie) {
			everyday(t, r)
			must(t, os.Remove(r.app.catalogPath))
			return apps(t, r, c, port)
		}},
	{"home-first-run-no-index", "The first screen on a machine whose host has not read the catalog: nothing to choose from yet.",
		func(t *testing.T, r *rig, c *http.Cookie, port *fakePort) (string, *http.Cookie) {
			notInstalled(t, r)
			must(t, os.Remove(r.app.catalogPath))
			r.hostSays(t, dashboard.Casa{Name: galleryName})
			r.contact(t, galleryClock)
			r.hostPushes(t, nil)
			return home(t, r, c, port)
		}},
	{"backup-everyday", "The backup page most mornings: last night, a week of nights, the key saved.",
		func(t *testing.T, r *rig, c *http.Cookie, port *fakePort) (string, *http.Cookie) {
			everyday(t, r)
			return backup(t, r, c, port)
		}},
	{"backup-first-open", "The backup page on the first open: no night yet, the key not yet saved.",
		func(t *testing.T, r *rig, c *http.Cookie, port *fakePort) (string, *http.Cookie) {
			r.hostBacksUp(t, dashboard.Backup{Folder: galleryFolder, On: true, Key: testKey})
			return backup(t, r, c, port)
		}},
	{"devices-everyday", "The trusted devices page most mornings: this laptop and a phone.",
		func(t *testing.T, r *rig, c *http.Cookie, port *fakePort) (string, *http.Cookie) {
			everyday(t, r)
			return devices(t, r, c, port)
		}},
	{"activity-everyday", "The activity page most mornings: an update, a resolved alert.",
		func(t *testing.T, r *rig, c *http.Cookie, port *fakePort) (string, *http.Cookie) {
			everyday(t, r)
			return activity(t, r, c, port)
		}},
	{"activity-alert-open", "The activity page with an alert open.",
		func(t *testing.T, r *rig, c *http.Cookie, port *fakePort) (string, *http.Cookie) {
			everyday(t, r)
			r.hostPushes(t, []notify.Record{
				galleryAlert(-24*time.Hour, "upgrade:os", notify.Event, notify.Info, "Updated", "Briard updated itself to v3.20261027.46bc529."),
				galleryAlert(-40*time.Minute, "disk", notify.Open, notify.Warning, "Disk space low", "The data volume has 1.2 GB free; Home Assistant's recorder stops writing below 1 GB."),
			})
			return activity(t, r, c, port)
		}},
	{"activity-empty", "The activity page on a host that pushed nothing.",
		func(t *testing.T, r *rig, c *http.Cookie, port *fakePort) (string, *http.Cookie) {
			return activity(t, r, c, port)
		}},
	{"apps-install-failed", "The apps page after the host refused an install: the reason, the CLI, and the button back.",
		func(t *testing.T, r *rig, c *http.Cookie, port *fakePort) (string, *http.Cookie) {
			notInstalled(t, r)
			startInstall(t, r, c, port)
			port.answer <- api.DirectiveOutcome{State: api.OutcomeFailed, Detail: "the data volume has 1.9 GB free; Home Assistant needs 3.5 GB"}
			deadline := time.Now().Add(5 * time.Second)
			for !strings.Contains(r.page(c), "Could not install") && time.Now().Before(deadline) {
				time.Sleep(20 * time.Millisecond)
			}
			return apps(t, r, c, port)
		}},
	// AN UPDATE ON OFFER, and the press: the home, the apps page and the app's own page.
	{"home-update-offered", "The catalog offers Home Assistant 2026.9.4 over the 2026.8.0 it runs: the badge, the line, the Update button.",
		func(t *testing.T, r *rig, c *http.Cookie, port *fakePort) (string, *http.Cookie) {
			everyday(t, r)
			galleryOffer(t, r)
			return home(t, r, c, port)
		}},
	{"apps-update-offered", "The apps page with an update on offer for an installed app.",
		func(t *testing.T, r *rig, c *http.Cookie, port *fakePort) (string, *http.Cookie) {
			everyday(t, r)
			galleryOffer(t, r)
			return apps(t, r, c, port)
		}},
	{"app-update-offered", "Home Assistant's page with an update on offer: the line that says what Briard does around it, and the version on offer.",
		func(t *testing.T, r *rig, c *http.Cookie, port *fakePort) (string, *http.Cookie) {
			galleryOffer(t, r)
			answerHistory(port, galleryRing(false))
			return "/apps/home-assistant", c
		}},
	{"home-updating", "The update in flight: the card says so and follows it, and the home is not setting up.",
		func(t *testing.T, r *rig, c *http.Cookie, port *fakePort) (string, *http.Cookie) {
			everyday(t, r)
			galleryOffer(t, r)
			startUpdate(t, r, c, port)
			return home(t, r, c, port)
		}},
	{"home-update-rolled-back", "The update did not come up and was put back: the reason, and the button to try again.",
		func(t *testing.T, r *rig, c *http.Cookie, port *fakePort) (string, *http.Cookie) {
			everyday(t, r)
			galleryOffer(t, r)
			startUpdate(t, r, c, port)
			port.answer <- api.DirectiveOutcome{State: api.OutcomeRolledBack, Detail: "home-assistant 2026.9.4 did not come up within 15m0s; the machine was put back"}
			deadline := time.Now().Add(5 * time.Second)
			for !strings.Contains(r.page(c), "The update did not finish") && time.Now().Before(deadline) {
				time.Sleep(20 * time.Millisecond)
			}
			return home(t, r, c, port)
		}},
	// THE OTHER PAGES.
	{"refused", "A browser the home does not trust.",
		func(t *testing.T, r *rig, c *http.Cookie, port *fakePort) (string, *http.Cookie) { return "/", nil }},
	{"join-waiting", "A new device showing its quick-connect code.",
		func(t *testing.T, r *rig, c *http.Cookie, port *fakePort) (string, *http.Cookie) {
			return "/join", r.ask(t, galleryPhone).join
		}},
	{"join-expired", "The quick-connect request expired or was already used.",
		func(t *testing.T, r *rig, c *http.Cookie, port *fakePort) (string, *http.Cookie) {
			j := r.ask(t, galleryPhone)
			r.app.now = func() time.Time { return galleryClock.Add(time.Minute + joinTTL + time.Second) }
			return "/join", j.join
		}},
	{"app", "Home Assistant's page: the card, what it is, and its history -- a quiet day, an update, each undoable.",
		func(t *testing.T, r *rig, c *http.Cookie, port *fakePort) (string, *http.Cookie) {
			answerHistory(port, galleryRing(false))
			return "/apps/home-assistant", c
		}},
	{"app-broken", "The app did not start cleanly after the update: the banner offers the way back.",
		func(t *testing.T, r *rig, c *http.Cookie, port *fakePort) (string, *http.Cookie) {
			answerHistory(port, galleryRing(true))
			return "/apps/home-assistant", c
		}},
	{"app-empty", "No history yet.",
		func(t *testing.T, r *rig, c *http.Cookie, port *fakePort) (string, *http.Cookie) {
			answerHistory(port, "[]")
			return "/apps/home-assistant", c
		}},
	{"app-unreadable", "The history could not be read (no admin port).",
		func(t *testing.T, r *rig, c *http.Cookie, port *fakePort) (string, *http.Cookie) {
			port.mu.Lock()
			port.noPort = true
			port.mu.Unlock()
			return "/apps/home-assistant", c
		}},
	{"app-undoing", "An undo in flight.",
		func(t *testing.T, r *rig, c *http.Cookie, port *fakePort) (string, *http.Cookie) {
			ring := galleryRing(false)
			// The listing's directives are answered; the restore's is left waiting.
			port.answer <- api.DirectiveOutcome{State: api.OutcomeDone, Detail: ring}
			point := quadlet.SnapshotMember("home-assistant", quadlet.TriggerAppUpdateBefore, time.Date(2026, 10, 27, 3, 0, 0, 0, time.UTC))
			postForm(t, r, "/undo", c, url.Values{"point": {point}, "what": {"Updated to 2026.8.0"}, "confirm": {"yes"}}).Body.Close()
			<-port.waiting
			port.answer <- api.DirectiveOutcome{State: api.OutcomeDone, Detail: ring}
			return "/apps/home-assistant", c
		}},
	{"confirm", "The step between pressing Undo and the undo itself.",
		func(t *testing.T, r *rig, c *http.Cookie, port *fakePort) (string, *http.Cookie) {
			answerHistory(port, galleryRing(false))
			point := quadlet.SnapshotMember("home-assistant", quadlet.TriggerAppUpdateBefore, time.Date(2026, 10, 27, 3, 0, 0, 0, time.UTC))
			return "/undo?" + url.Values{"point": {point}, "what": {"Updated to 2026.8.0"}}.Encode(), c
		}},
}

func TestGallery(t *testing.T) {
	dir := os.Getenv("DASHBOARD_GALLERY")
	if dir == "" {
		t.Skip("set DASHBOARD_GALLERY=<dir> to render every page in every state there")
	}
	must(t, os.MkdirAll(dir, 0o755))
	// The stylesheet beside the pages, so a file:// render finds it where the served page would.
	must(t, os.WriteFile(filepath.Join(dir, "app.css"), appCSS, 0o644))
	var index strings.Builder
	index.WriteString("<!doctype html><meta charset=utf-8><title>Briard dashboard gallery</title><style>body{font:16px/1.5 system-ui;max-width:48rem;margin:2rem auto;padding:0 1rem}dt{font-weight:600;margin-top:.8rem}dd{margin:0;color:#555}</style><h1>Briard dashboard — the state gallery</h1><p>Rendered " + galleryClock.Format("2 Jan 2006 15:04 MST") + " (the fixtures' clock). <a href=screens.html>Screenshots</a>, when the lab script has run.</p><dl>")
	for _, s := range galleryStates {
		t.Run(s.name, func(t *testing.T) {
			r, c, port := galleryRig(t)
			path, cookie := s.render(t, r, c, port)
			method := "GET"
			var body io.Reader
			if strings.HasPrefix(path, "/undo?") {
				// The confirm page answers a POST; the form is carried in the query here only
				// to keep the table's shape.
				method, body = "POST", strings.NewReader(strings.TrimPrefix(path, "/undo?"))
				path = "/undo"
			}
			req, _ := http.NewRequest(method, r.srv.URL+path, body)
			req.Host = routes.BareFlockHostName // the gallery is seen as a browser on the alias sees it
			if body != nil {
				req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			}
			if cookie != nil {
				req.AddCookie(cookie)
			}
			resp, err := noRedirect.Do(req)
			must(t, err)
			page := bodyOf(t, resp)
			if !strings.Contains(page, "<html") {
				t.Fatalf("%s %s = %d, not a page: %s", method, path, resp.StatusCode, page)
			}
			must(t, os.WriteFile(filepath.Join(dir, s.name+".html"), []byte(strings.ReplaceAll(page, `href="/app.css"`, `href="app.css"`)), 0o644))
			fmt.Fprintf(&index, "<dt><a href=%s.html>%s</a> <small>(%d)</small></dt><dd>%s</dd>", s.name, s.name, resp.StatusCode, html.EscapeString(s.note))
		})
	}
	index.WriteString("</dl>")
	must(t, os.WriteFile(filepath.Join(dir, "index.html"), []byte(index.String()), 0o644))
}
