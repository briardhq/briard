package host

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"briard.io/agent/guestagent"
	"briard.io/agent/platform"
	"briard.io/shared/api"
	"briard.io/shared/atomicfile"
	"briard.io/shared/dashboard"
	"briard.io/shared/notify"
)

// THE NIGHTLY BACKUP, 02:00 local: before the guest-update window (03:00) and the recorder check
// (05:30), so it copies the night's quietest hour. The guest does the work (agent/guestagent's
// data.backup says what goes in and how, and the key is the volume's -- the household's, not
// this node's); this schedules it and serves the repository the guest writes to (backupstore.go).
//
// IT NEVER BLOCKS THIS LOOP: the recorder check's shape. This starts the run in the guest's
// background and asks for the report once a cycle with an ordinary short call, for as long as the
// guest says it is running. A first run copies everything and may take an hour.
//
// ONCE A NIGHT WHATEVER HAPPENS, on the node that serves: a failure is logged and waits a day.
// An agent that is down at 02:00 still starts it within the window; one down for the whole window
// skips that night. A run a guest reboot cut short leaves a lock the next run's unlock clears.
const (
	backupAt          = 2 * 60 // minutes after local midnight
	backupWindow      = 60     // minutes after backupAt a late agent may still start it
	backupCallTimeout = 5 * time.Second
)

// backupHostFacts are the host's own files a fresh install needs to come back as this household
// and could not mint again the same: its address, its name, its claim to that name. Never the
// node id -- that belongs to the hardware, and a new machine is a new node.
var backupHostFacts = []string{flockIDName, flockNameName, casaStateName, casaKeyName}

// backupRunner is the slice of the guest the backup costs.
type backupRunner interface {
	DataBackup(ctx context.Context, url, group string, host map[string][]byte) (bool, error)
	DataBackupResult(ctx context.Context) (guestagent.BackupState, error)
}

// backupScheduler remembers the local date it last started a backup, in the household's zone,
// and since when it has been waiting for one's report.
type backupScheduler struct {
	loc     *time.Location
	done    string
	waiting time.Time // zero when no report is awaited
	offsite offsiteCheck
}

func newBackupScheduler() *backupScheduler {
	return &backupScheduler{loc: householdLocation(), offsite: offsiteCheck{provider: syncProvider, inSync: inSync}}
}

// backup collects a run that is going, or starts tonight's if it is due: this node serves, and the
// local time is inside tonight's window. Every night that should have run and did not is recorded
// as a failed one, so the alert counts what the household would count.
func (cfg Config) backup(ctx context.Context, g backupRunner, b *backupScheduler, serving bool, now time.Time, n notify.Notifier, logf func(string, ...any)) {
	if cfg.stateDir() == "" || cfg.hostNodeIP() == "" || !serving {
		return
	}
	if !b.waiting.IsZero() {
		cfg.collectBackup(ctx, g, b, n, logf)
		return
	}
	b.offsite.tick(cfg.backupFolder(), cfg.readBackupRecord().Nights, now, logf)
	// A kicked run (an adoption, `briard backup`) runs now and does not count as the night: the
	// night still runs, and finds nearly nothing to add.
	kicked := cfg.backupKick != nil && cfg.backupKick.Swap(false)
	if kicked {
		// Stamped when taken up, not when this tick began: `briard backup` waits for a run that
		// started after it asked, and it asks before it kicks.
		now = time.Now()
	}
	local := now.In(b.loc)
	day := local.Format(time.DateOnly)
	mins := local.Hour()*60 + local.Minute()
	if !kicked && (b.done == day || mins < backupAt || mins >= backupAt+backupWindow) {
		return
	}
	if !kicked {
		b.done = day
	}
	if cfg.backupFolder() == "" {
		// Off is the household's decision: whatever was failing is no longer being asked of it.
		fireAlert(ctx, n, logf, backupWorking(cfg.Node, "The nightly backup is turned off."))
		cfg.ended(now, true, "the nightly backup is turned off")
		return
	}
	failed := func(why string) {
		logf("backup: %s", why)
		cfg.ended(now, true, why)
		cfg.recordNight(ctx, dashboard.BackupRun{At: now, Error: cfg.backupFailure(why)}, n, logf)
	}
	url := "http://" + net.JoinHostPort(cfg.hostNodeIP(), backupStorePort) + "/"
	sctx, cancel := context.WithTimeout(ctx, backupCallTimeout)
	// THE GROUP IS THE FLOCK'S ID (guestagent.backupRequest.Group says why): shared by every node
	// of the pair, and nobody else's.
	started, err := g.DataBackup(sctx, url, cfg.FlockID, cfg.backupHostFacts(logf))
	cancel()
	if err != nil {
		failed("could not start tonight's run: " + err.Error())
		return
	}
	if !started {
		// The guest is still running one, or holds a finished one's report nobody collected (this
		// agent restarted): collecting that one is tonight's.
		logf("backup: the guest has a run going or a report waiting; collecting that one")
	}
	b.waiting = now
}

// collectBackup asks for a started run's report, logs it once it has come, and records it as the
// night -- unless there was nothing to back up.
func (cfg Config) collectBackup(ctx context.Context, g backupRunner, b *backupScheduler, n notify.Notifier, logf func(string, ...any)) {
	sctx, cancel := context.WithTimeout(ctx, backupCallTimeout)
	s, err := g.DataBackupResult(sctx)
	cancel()
	started := b.waiting
	switch {
	case err != nil, s.Report == nil && s.Running:
		return // asked again next cycle
	case s.Report == nil:
		// The guest agent restarted and lost it: a night that did not happen.
		b.waiting = time.Time{}
		why := "the run was lost when the guest restarted"
		logf("backup: %s", why)
		cfg.ended(started, true, why)
		cfg.recordNight(ctx, dashboard.BackupRun{At: started, Error: cfg.backupFailure(why)}, n, logf)
		return
	}
	b.waiting = time.Time{}
	rep := *s.Report
	switch {
	case rep.Error != "":
		logf("backup: FAILED (snapshot %q): %s", rep.Snapshot, rep.Error)
		cfg.ended(started, true, rep.Error)
	case rep.Snapshot == "":
		logf("backup: nothing to back up (no service has a snapshot yet)")
		cfg.ended(started, false, "nothing to back up: no app has a snapshot yet")
		return
	default:
		logf("backup: snapshot %s of %s: %d files, %d bytes added", rep.Snapshot, strings.Join(rep.Services, ", "), rep.Files, rep.BytesAdded)
		cfg.ended(started, false, fmt.Sprintf("backed up %s: %d files, %s added (snapshot %.8s)", strings.Join(rep.Services, ", "), rep.Files, sizeWords(rep.BytesAdded), rep.Snapshot))
	}
	night := dashboard.BackupRun{At: started, Snapshot: rep.Snapshot, BytesAdded: rep.BytesAdded, Size: folderSize(cfg.backupFolder()), WrongKey: rep.WrongKey}
	if rep.WrongKey {
		night.Error = "the folder holds a backup made with another key -- an earlier install's? Enter that key on the Briard page to continue it, or choose another folder"
	} else if rep.Error != "" {
		night.Error = cfg.backupFailure(rep.Error)
	}
	cfg.recordNight(ctx, night, n, logf)
}

// recordNight keeps the night, newest first, and says what it means for the household: a second
// failed night in a row opens the backup alert, a good one ends it.
func (cfg Config) recordNight(ctx context.Context, night dashboard.BackupRun, n notify.Notifier, logf func(string, ...any)) {
	var nights []dashboard.BackupRun
	if err := cfg.updateBackupRecord(func(r *backupRecord) {
		r.Nights = append([]dashboard.BackupRun{night}, r.Nights...)
		if len(r.Nights) > backupNightsKept {
			r.Nights = r.Nights[:backupNightsKept]
		}
		nights = r.Nights
	}); err != nil {
		logf("backup: could not record tonight's run: %v", err)
		nights = []dashboard.BackupRun{night}
	}
	if night.Error == "" {
		fireAlert(ctx, n, logf, backupWorking(cfg.Node, fmt.Sprintf("Last night's backup into %s finished.", cfg.backupFolder())))
		return
	}
	streak := 0
	for streak < len(nights) && nights[streak].Error != "" {
		streak++
	}
	if streak >= backupAlertAfter {
		fireAlert(ctx, n, logf, backupFailing(cfg.Node, streak, night.Error))
	}
}

// backupAlertAfter is how many failed nights in a row reach the household. One is a laptop lid, a
// NAS rebooting, a sync drive not mounted yet; two is something to fix.
const backupAlertAfter = 2

// backupFailure says what went wrong in the household's terms, from what the host can see of the
// folder itself rather than from the run's own words: a folder that is not there wants a disk
// plugged in, one that cannot be written wants its permissions, anything else is the run's.
func (cfg Config) backupFailure(runErr string) string {
	dir := cfg.backupFolder()
	if _, err := os.Stat(dir); err != nil {
		return fmt.Sprintf("the backup folder %s is not there. If it is on a disk or a network drive, connect it", dir)
	}
	f, err := os.CreateTemp(dir, ".briard-check-")
	if err != nil {
		return fmt.Sprintf("Briard cannot write to the backup folder %s (%v)", dir, err)
	}
	f.Close()
	_ = os.Remove(f.Name())
	return "the backup did not finish: " + runErr
}

// backupFailing is the open alert: the copy is getting old, the data is intact -- a Warning.
func backupFailing(node string, nights int, why string) notify.Alert {
	return notify.Alert{
		Key:      "backup",
		Kind:     notify.Open,
		Severity: notify.Warning,
		Title:    "Briard: the nightly backup is not working",
		Body: fmt.Sprintf("The last %d nights' backups on node %s did not happen: %s. Your apps and their data are fine; "+
			"the backup copy is what is getting old.", nights, node, why),
	}
}

// backupWorking ends it; the store drops it when nothing is open, which is every other night.
func backupWorking(node, why string) notify.Alert {
	return notify.Alert{
		Key:   "backup",
		Kind:  notify.Resolved,
		Title: "Briard: the nightly backup is working again",
		Body:  fmt.Sprintf("%s (node %s)", why, node),
	}
}

// backupNightsKept is how many nights the page lists: a week, which is also as far back as the
// repository keeps daily snapshots plus its one weekly.
const backupNightsKept = 7

// folderSize is what the folder holds, summed from what each file says it is -- never read, so a
// sync client's placeholder for a file it has moved to the cloud costs nothing. 0 when unreadable.
func folderSize(dir string) int64 {
	if dir == "" {
		return 0
	}
	var n int64
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if fi, err := d.Info(); err == nil {
				n += fi.Size()
			}
		}
		return nil
	})
	return n
}

// THE OFF-SITE CHECK: whether the folder's sync client -- OneDrive, where Documents is OneDrive's
// (backupFolder, on Windows) -- has uploaded the newest good night. A folder under a sync client
// says only that the client should upload it: OneDrive syncs nothing while nobody is signed in, a
// full plan stops it, sync can be paused. So the answer is per file, from the client's own record
// of each (inSync), over every file the night wrote -- the files modified since it started, which
// needs nothing recorded and survives an agent restart.
//
// A LONG READ, OFF THE LOOP, as every long read is: a walk of the repository and a handle per new
// file, in a goroutine the tick collects, between runs, on the node that serves. Asked once per
// night and folder, and again every offsiteRetry while some file is waiting; not again once the
// night is all uploaded or the folder is under no sync client. "Could not tell" -- a state that
// could not be read, a file the client has not taken up yet, a night with no files found -- is
// waiting, never synced.
const offsiteRetry = 10 * time.Minute

// offsiteCheck is the check's state, carried by the backup scheduler.
type offsiteCheck struct {
	provider func(dir string) (string, error) // the sync client holding dir (syncProvider)
	inSync   func(path string) error          // nil when that client has uploaded path (inSync)
	// folder and night are what the answer is about: a new one of either asks again.
	folder string
	night  time.Time
	// in and synced are the answer, offsiteAnswer's.
	in      string
	synced  bool
	next    time.Time // when to ask again while waiting
	done    bool      // asked, and settled for this night and folder
	running chan offsiteAnswer
}

// offsiteAnswer is what one check found.
type offsiteAnswer struct {
	folder          string
	night           time.Time
	in              string
	synced          bool
	files, uploaded int
	why             error // why not synced: the folder's sync client could not be read, or the first file that is not uploaded
}

// tick collects a check in flight, or starts one when one is due. nights are the record's, newest
// first; the newest without an error is the one asked about.
func (o *offsiteCheck) tick(folder string, nights []dashboard.BackupRun, now time.Time, logf func(string, ...any)) {
	var night time.Time
	for _, n := range nights {
		if n.Error == "" && n.Snapshot != "" {
			night = n.At
			break
		}
	}
	if folder != o.folder || !night.Equal(o.night) {
		*o = offsiteCheck{provider: o.provider, inSync: o.inSync, folder: folder, night: night, running: o.running}
	}
	if o.running != nil {
		select {
		case a := <-o.running:
			o.running = nil
			if a.folder != o.folder || !a.night.Equal(o.night) {
				return // asked about a folder or a night since replaced; the next tick asks about this one
			}
			if a.in != o.in || a.synced != o.synced {
				switch {
				case a.in == "":
					logf("backup: off-site: %s is in no sync client's folder (%v)", folder, a.why)
				case a.synced:
					logf("backup: off-site: %s has uploaded the night of %s (%d files)", a.in, night.Format(time.DateTime), a.files)
				default:
					logf("backup: off-site: waiting for %s: %d of %d files of the night of %s uploaded (%v)", a.in, a.uploaded, a.files, night.Format(time.DateTime), a.why)
				}
			}
			o.in, o.synced = a.in, a.synced
			o.done = a.in == "" || a.synced
			o.next = now.Add(offsiteRetry)
		default:
		}
		return
	}
	if folder == "" || night.IsZero() || o.done || now.Before(o.next) {
		return
	}
	o.running = make(chan offsiteAnswer, 1)
	go func(out chan<- offsiteAnswer, provider func(string) (string, error), inSync func(string) error) {
		out <- offsiteRead(folder, night, provider, inSync)
	}(o.running, o.provider, o.inSync)
}

// offsiteRead runs the check once: the folder's sync client, then every file modified since the
// night started. A file or directory it could not read counts as not uploaded.
func offsiteRead(folder string, night time.Time, provider func(string) (string, error), inSync func(string) error) offsiteAnswer {
	a := offsiteAnswer{folder: folder, night: night}
	if a.in, a.why = provider(folder); a.in == "" {
		return a
	}
	unread := false
	_ = filepath.WalkDir(folder, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			unread = true
			a.why = cmp.Or(a.why, err)
			return nil
		}
		if d.IsDir() {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			unread = true
			a.why = cmp.Or(a.why, err)
			return nil
		}
		if fi.ModTime().Before(night) {
			return nil
		}
		a.files++
		if err := inSync(p); err != nil {
			a.why = cmp.Or(a.why, fmt.Errorf("%s: %w", p, err))
			return nil
		}
		a.uploaded++
		return nil
	})
	a.synced = !unread && a.files > 0 && a.uploaded == a.files
	if a.files == 0 && a.why == nil {
		a.why = errors.New("no file of the night found")
	}
	return a
}

// backupHostFacts reads the host facts that exist. An absent one is a node that never had it (no
// casa claim); an unreadable one is logged and left out -- tonight's backup is worth more than
// one missing file.
func (cfg Config) backupHostFacts(logf func(string, ...any)) map[string][]byte {
	out := map[string][]byte{}
	for _, name := range backupHostFacts {
		b, err := os.ReadFile(filepath.Join(cfg.stateDir(), name))
		switch {
		case err == nil:
			out[name] = b
		case !errors.Is(err, fs.ErrNotExist):
			logf("backup: leaving out %s: %v", name, err)
		}
	}
	return out
}

// backupRecordName is the backup's node-local record: the folder, which is a path on this host,
// and what the nights did here. The key and the household's word that it is saved are the
// volume's (guestagent's data.backup.key) -- one household, one key.
const backupRecordName = "backup.json"

// backupRecord is that file. Folder is the folder the backup last ran into, kept while it is off
// so turning it on again knows where; Previous the one it ran into before the household moved it;
// Nights the recent runs, newest first.
type backupRecord struct {
	Folder   string                `json:"folder,omitempty"`
	Previous string                `json:"previous,omitempty"`
	Nights   []dashboard.BackupRun `json:"nights,omitempty"`
}

// backupFolder is the backup folder as it stands NOW: `config set backup-dir` turns it off and on
// at runtime, and the store and the nightly tick both read it. Atomic, because the store reads it
// from its own goroutine. nil (every unit test that does not set it) reads BackupDir.
func (cfg Config) backupFolder() string {
	if cfg.backupLive != nil {
		return *cfg.backupLive.Load()
	}
	return cfg.BackupDir
}

func (cfg Config) readBackupRecord() backupRecord {
	var rec backupRecord
	if b, err := os.ReadFile(filepath.Join(cfg.stateDir(), backupRecordName)); err == nil {
		_ = json.Unmarshal(b, &rec)
	}
	return rec
}

// updateBackupRecord changes the record and writes it whole (tmp + fsync + rename: the file is the
// only copy of what the household acknowledged).
func (cfg Config) updateBackupRecord(change func(*backupRecord)) error {
	if cfg.stateDir() == "" {
		return errors.New("no state directory")
	}
	rec := cfg.readBackupRecord()
	change(&rec)
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	return atomicfile.Write(filepath.Join(cfg.stateDir(), backupRecordName), b, 0o600, 0o700)
}

// backupView is what the page is told: the folder (the one in use, or the one it returns to), the
// record, and the key with the household's word on it -- read from the volume through the guest,
// which mints it the first time it is asked (so the page can ask the household to save it from
// its first open). Where the volume is not mounted there is no key to show and no page to show
// it on: the view goes without it.
func (cfg Config) backupView(ctx context.Context, g backupViewGuest) dashboard.Backup {
	rec := cfg.readBackupRecord()
	v := dashboard.Backup{Folder: cfg.backupFolder(), On: cfg.backupFolder() != "", Previous: rec.Previous, Nights: rec.Nights}
	if !v.On {
		v.Folder = rec.Folder
		if v.Folder == "" {
			v.Folder = defaultBackupDir // where turning it on goes (applyBackupSetting)
		}
	}
	kctx, cancel := context.WithTimeout(ctx, backupCallTimeout)
	defer cancel()
	if k, err := g.DataBackupKey(kctx, false); err == nil {
		v.Key, v.KeySaved = k.Key, k.Saved
	}
	return v
}

// backupViewGuest is the slice of the guest the page's copy costs.
type backupViewGuest interface {
	DataBackupKey(ctx context.Context, saved bool) (guestagent.BackupKey, error)
	DashboardBackup(ctx context.Context, b dashboard.Backup) error
}

// pushBackupView hands the guest the page's view when it differs from what this connection last
// pushed. pushed starts empty with every connection, so a fresh guest is always told. o is the
// off-site check's last answer; nil tells the page nothing about one.
func (cfg Config) pushBackupView(ctx context.Context, r any, o *offsiteCheck, pushed *string, logf func(string, ...any)) {
	g, ok := r.(backupViewGuest)
	if !ok || cfg.stateDir() == "" {
		return
	}
	v := cfg.backupView(ctx, g)
	if o != nil {
		v.OffsiteIn, v.OffsiteSynced = o.in, o.synced
	}
	b, err := json.Marshal(v)
	if err != nil || string(b) == *pushed {
		return
	}
	pctx, cancel := context.WithTimeout(ctx, backupCallTimeout)
	defer cancel()
	if err := g.DashboardBackup(pctx, v); err != nil {
		logf("backup: dashboard copy: %v", err)
		return
	}
	*pushed = string(b)
}

// THE BACKUP'S TWO SETTINGS, through `config set` -- the one runtime writer -- so the page and the
// CLI change them the same way. Applied in place: the store and the tick read backupFolder() at
// every use, so nothing restarts, and the guest is not involved.
//
//   - backup-dir: "" turns the nightly backup off (BACKUP_DIR="", the existing backups are left
//     where they are); the folder it last used -- or, with none, the agent's own -- turns it on
//     again; any other folder moves it there, once checkBackupFolder has passed it.
//   - backup-key-saved: "yes" records that the household has the key somewhere else -- on the
//     volume, beside the key, through the guest: the household's word is the household's.
//   - backup-adopt: the key that opens the backup already in the folder; the household's own key
//     is added to it (the guest's data.backup.adopt) and a run starts now. The typed key is the
//     payload of this one directive and is kept nowhere.
const (
	settingBackupDir      = "backup-dir"
	settingBackupKeySaved = "backup-key-saved"
	settingBackupAdopt    = "backup-adopt"
)

// backupSettings are the keys applyBackupSetting answers for.
var backupSettings = map[string]bool{settingBackupDir: true, settingBackupKeySaved: true, settingBackupAdopt: true}

// isBackupSetting says whether a config-set payload names one of the backup's settings -- the only
// config-set the guest's page may ask for (adminport.go).
func isBackupSetting(payload string) bool {
	var s api.ConfigSetting
	if json.Unmarshal([]byte(payload), &s) != nil {
		return false
	}
	return backupSettings[s.Key]
}

// applyBackupSetting applies a config-set naming a backup setting; ok is false for any other key.
func (cfg Config) applyBackupSetting(ctx context.Context, d api.Directive, r any, logf func(string, ...any)) (out api.DirectiveOutcome, ok bool) {
	var s api.ConfigSetting
	if err := json.Unmarshal([]byte(d.Payload), &s); err != nil || !backupSettings[s.Key] {
		return api.DirectiveOutcome{}, false
	}
	failed := func(format string, a ...any) (api.DirectiveOutcome, bool) {
		return api.DirectiveOutcome{ID: d.ID, State: api.OutcomeFailed, Detail: fmt.Sprintf(format, a...)}, true
	}
	done := func(format string, a ...any) (api.DirectiveOutcome, bool) {
		return api.DirectiveOutcome{ID: d.ID, State: api.OutcomeDone, Detail: fmt.Sprintf(format, a...)}, true
	}
	if cfg.backupLive == nil || cfg.stateDir() == "" {
		return failed("this node keeps no backup settings")
	}
	if s.Key == settingBackupKeySaved {
		if s.Value != "yes" {
			return failed(`%s takes "yes"`, settingBackupKeySaved)
		}
		g, ok := r.(interface {
			DataBackupKey(ctx context.Context, saved bool) (guestagent.BackupKey, error)
		})
		if !ok {
			return failed("no guest on this node to record it on")
		}
		kctx, cancel := context.WithTimeout(ctx, backupCallTimeout)
		defer cancel()
		if _, err := g.DataBackupKey(kctx, true); err != nil {
			return failed("could not record it: %v", err)
		}
		logf("backup: the household has saved the recovery key")
		return done("noted: the recovery key is saved")
	}
	if s.Key == settingBackupAdopt {
		g, ok := r.(backupAdopter)
		if !ok || !g.SupportsBackupAdopt() {
			return failed("this guest cannot adopt a backup; update the guest OS first")
		}
		folder := cfg.backupFolder()
		if folder == "" || cfg.hostNodeIP() == "" {
			return failed("the nightly backup is off: turn it on, into the folder that holds the backup, first")
		}
		if !isResticRepository(folder) {
			return failed("%s holds no backup to adopt", folder)
		}
		actx, cancel := context.WithTimeout(ctx, configSetBudget)
		defer cancel()
		url := "http://" + net.JoinHostPort(cfg.hostNodeIP(), backupStorePort) + "/"
		if err := g.DataBackupAdopt(actx, url, s.Value); err != nil {
			return failed("not adopted: %v", err)
		}
		if cfg.backupKick != nil {
			cfg.backupKick.Store(true)
		}
		logf("backup: adopted the backup in %s; a run starts now", folder)
		return done("the backup in %s is now this household's; tonight's copy is being made now", folder)
	}
	now := cfg.backupFolder()
	want := s.Value
	last := cfg.readBackupRecord().Folder
	switch {
	case want == "":
	case want == defaultBackupDir:
		if err := os.MkdirAll(want, 0o700); err != nil {
			return failed("%v", err)
		}
	case want == now || want == last:
		// A folder this backup already uses: nothing to check.
	default:
		if err := checkBackupFolder(want); err != nil {
			return failed("%v", err)
		}
	}
	if want == now {
		return done("the backup is already %s; nothing changed", backupWord(want))
	}
	path := cfg.configPathForMessage()
	if err := setConfigKey(path, "BACKUP_DIR", want, false); err != nil {
		return failed("could not record it in %s: %v", path, err)
	}
	if want != "" {
		// MOVED, NOT CARRIED: the earlier backups stay where they are, the household's to keep or
		// delete, and the next night opens the repository already in the new folder -- one the
		// household moved there itself -- or starts one, with the same key.
		if err := cfg.updateBackupRecord(func(r *backupRecord) {
			if r.Folder != "" && r.Folder != want {
				r.Previous = r.Folder
			}
			r.Folder = want
		}); err != nil {
			logf("backup: could not remember the folder: %v", err)
		}
	}
	cfg.backupLive.Store(&want)
	logf("backup: %s -> %s, recorded in %s", backupWord(now), backupWord(want), path)
	return done("the nightly backup is now %s", backupWord(want))
}

// backupWord names a BACKUP_DIR value for a person.
func backupWord(dir string) string {
	if dir == "" {
		return "off"
	}
	return "on, into " + dir
}

// checkBackupFolder is what a folder must be before the host, as root, writes the backup into it
// -- and before it serves what is in it to the guest, which may be the one asking:
//
//   - a full path to a folder that EXISTS. Never created here: a missing folder may be a disk not
//     mounted, and creating it would write onto whatever is underneath.
//   - owned by a person, not root: every file is given the folder's owner, so it stays theirs, and
//     no system directory qualifies. Linux only: on Windows the folder's inherited ACL is what
//     makes the files a person's, and OwnerUID says ErrUnsupported.
//   - EMPTY, or already a restic repository -- these backups, moved there by the household, which
//     the next night continues. Hidden files (a sync client's marker) do not count. Anything else
//     is refused: the store answers `GET /config` from the folder, so a folder holding some other
//     `config` (~/.kube, ~/.ssh) would hand that file to whoever asks for the backup's.
//   - writable: a read-only mount is found now, not at 02:00.
func checkBackupFolder(dir string) error {
	if !filepath.IsAbs(dir) || filepath.Clean(dir) != dir {
		return fmt.Errorf("%q is not a full path to a folder (like /home/you/Backups)", dir)
	}
	fi, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("there is no folder at %s: create it first", dir)
	}
	if !fi.IsDir() {
		return fmt.Errorf("%s is not a folder", dir)
	}
	uid, err := platform.OwnerUID(dir)
	if err != nil && !errors.Is(err, errors.ErrUnsupported) {
		return fmt.Errorf("%s: %v", dir, err)
	}
	if err == nil && uid == 0 {
		return fmt.Errorf("%s belongs to root: choose a folder a person owns, like one in their home", dir)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("%s cannot be read: %v", dir, err)
	}
	visible := 0
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), ".") {
			visible++
		}
	}
	if visible > 0 && !isResticRepository(dir) {
		return fmt.Errorf("%s already holds other files: choose an empty folder, or one that holds these backups", dir)
	}
	f, err := os.CreateTemp(dir, ".briard-check-")
	if err != nil {
		return fmt.Errorf("%s cannot be written: %v", dir, err)
	}
	f.Close()
	return os.Remove(f.Name())
}

// isResticRepository says whether dir is one: a config file and a keys directory, as every restic
// repository has.
func isResticRepository(dir string) bool {
	c, err := os.Stat(filepath.Join(dir, "config"))
	k, kerr := os.Stat(filepath.Join(dir, "keys"))
	return err == nil && c.Mode().IsRegular() && kerr == nil && k.IsDir()
}

// backupRestorer is the slice of the guest a restore from the backup costs beyond what a restore
// of a ring member already does.
type backupRestorer interface {
	DataBackupRestore(ctx context.Context, url, snapshot, service string) (string, error)
	SupportsBackupRestore() bool
}

// applyBackupRestore puts one service back to one night of the backup: the night's copy becomes a
// ring member (the guest's data.backup.restore), and the member is restored as any member is
// (applyServiceRestore) -- so the images, the undo point, the stop, the swap, the manifest and the
// converge are the one path History already has, and the restore is a row that can be undone.
//
// THE MATERIALISATION COSTS NOTHING THE HOUSEHOLD CAN SEE: the service keeps running while the
// night is read in, and a night that cannot be read leaves the ring as it was. Everything that can
// refuse, refuses before applyServiceRestore stops anything.
func (cfg Config) applyBackupRestore(ctx context.Context, g serviceInstaller, d api.Directive, logf func(string, ...any)) api.DirectiveOutcome {
	failed := func(detail string) api.DirectiveOutcome {
		return api.DirectiveOutcome{ID: d.ID, State: api.OutcomeFailed, Detail: detail}
	}
	var req api.BackupRestore
	if err := json.Unmarshal([]byte(d.Payload), &req); err != nil || req.Service == "" {
		return failed("the payload does not name a service and a night")
	}
	if req.Snapshot == "" {
		req.Snapshot = "latest"
	}
	br, ok := g.(backupRestorer)
	if !ok || !br.SupportsBackupRestore() {
		return failed("this guest is too old to restore from the backup; update the guest OS first")
	}
	if cfg.backupFolder() == "" || cfg.hostNodeIP() == "" {
		return failed("the backup is turned off on this node, so there is no repository to restore from")
	}
	url := "http://" + net.JoinHostPort(cfg.hostNodeIP(), backupStorePort) + "/"
	mctx, cancel := cfg.beat.budget(ctx, installBudget)
	member, err := br.DataBackupRestore(mctx, url, req.Snapshot, req.Service)
	cancel()
	if err != nil {
		return failed(fmt.Sprintf("read %s's copy from the backup (nothing was changed): %v", req.Service, err))
	}
	logf("backup restore %s: night %s is in the ring as %s; restoring it", req.Service, req.Snapshot, member)
	return cfg.applyServiceRestore(ctx, g, api.Directive{ID: d.ID, Kind: api.DirectiveServiceRestore, Payload: member}, logf)
}

// backupLister is the slice of the guest the Backup page's list costs.
type backupLister interface {
	DataBackupList(ctx context.Context, url, group string) ([]dashboard.BackupListing, error)
	SupportsBackupList() bool
}

// applyBackupList answers what the household's repository holds, for the Backup page's restore:
// every backup newest first, the apps in each. The guest reads it (data.backup.list), from the
// store this host serves it; this node's own record of its runs cannot answer it -- an adopted
// folder's backups, and the apps in any of them, are in the repository alone.
func (cfg Config) applyBackupList(ctx context.Context, g backupLister, d api.Directive) api.DirectiveOutcome {
	failed := func(detail string) api.DirectiveOutcome {
		return api.DirectiveOutcome{ID: d.ID, State: api.OutcomeFailed, Detail: detail}
	}
	if !g.SupportsBackupList() {
		return failed("this guest is too old to list the backup; update the guest OS first")
	}
	if cfg.backupFolder() == "" || cfg.hostNodeIP() == "" {
		return failed("the backup is turned off on this node, so there is no repository to list")
	}
	// A FOLDER WITH NO REPOSITORY YET holds no backup: the first run makes it, so a new install's
	// folder is empty until then, and restic would answer that with "repository does not exist".
	// Read off the folder this host serves, before the guest is asked.
	if !isResticRepository(cfg.backupFolder()) {
		return api.DirectiveOutcome{ID: d.ID, State: api.OutcomeDone, Detail: "[]"}
	}
	url := "http://" + net.JoinHostPort(cfg.hostNodeIP(), backupStorePort) + "/"
	lctx, cancel := context.WithTimeout(ctx, backupListTimeout)
	defer cancel()
	list, err := g.DataBackupList(lctx, url, cfg.FlockID)
	if err != nil {
		return failed(fmt.Sprintf("read the backup: %v", err))
	}
	raw, err := json.Marshal(list)
	if err != nil {
		return failed(err.Error())
	}
	return api.DirectiveOutcome{ID: d.ID, State: api.OutcomeDone, Detail: string(raw)}
}

// backupListTimeout bounds the list: one restic call for the snapshots and one per backup, a
// handful since the repository keeps two days and a week.
const backupListTimeout = 2 * time.Minute

// backupAdopter is the slice of the guest an adoption costs.
type backupAdopter interface {
	DataBackupAdopt(ctx context.Context, url, password string) error
	SupportsBackupAdopt() bool
}

// backupEnd is how one of the scheduler's runs ended: when it started, and what came of it.
type backupEnd struct {
	Started time.Time
	Failed  bool
	Detail  string
}

// ended records how the run started at started ended, for `briard backup` to read.
func (cfg Config) ended(started time.Time, failed bool, detail string) {
	if cfg.backupEnded != nil {
		cfg.backupEnded.Store(&backupEnd{Started: started, Failed: failed, Detail: detail})
	}
}

// sizeWords is a byte count as the CLI says it.
func sizeWords(n int64) string {
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

// backupNowBudget bounds `briard backup`'s wait: a first run copies everything, and may take an hour.
const backupNowBudget = 90 * time.Minute

// applyBackupNow is `briard backup`: the nightly run, now -- the newest History snapshot of every
// app, into the folder, as at 02:00 -- answered once it has ended. It KICKS THE SCHEDULER rather
// than starting a run itself: the run, its report and its record stay the scheduler's, so this and
// the night's are never two runs at once (asked during one, it waits for that one, then runs).
// Like an adoption's kick, it does not count as the night: the night still runs.
func (cfg Config) applyBackupNow(ctx context.Context, d api.Directive) api.DirectiveOutcome {
	failed := func(detail string) api.DirectiveOutcome {
		return api.DirectiveOutcome{ID: d.ID, State: api.OutcomeFailed, Detail: detail}
	}
	if cfg.backupKick == nil || cfg.backupEnded == nil {
		return failed("this agent has no backup scheduler")
	}
	if cfg.backupFolder() == "" {
		return failed("the nightly backup is turned off on this node; turn it on from the Briard page first")
	}
	asked := time.Now()
	cfg.backupKick.Store(true)
	ctx, cancel := context.WithTimeout(ctx, backupNowBudget)
	defer cancel()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return failed("the backup has not ended yet; the Briard page's Backup section, or `briard logs`, says how it ends")
		case <-tick.C:
			if e := cfg.backupEnded.Load(); e != nil && !e.Started.Before(asked) {
				if e.Failed {
					return failed(e.Detail)
				}
				return api.DirectiveOutcome{ID: d.ID, State: api.OutcomeDone, Detail: e.Detail}
			}
		}
	}
}
