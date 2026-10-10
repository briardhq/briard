package host

import (
	"context"
	"crypto/rand"
	"encoding/base32"
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
	"briard.io/shared/api"
	"briard.io/shared/atomicfile"
	"briard.io/shared/dashboard"
)

// THE NIGHTLY BACKUP, 02:00 local: before the guest-update window (03:00) and the recorder check
// (05:30), so it copies the night's quietest hour. The guest does the work (agent/guestagent's
// data.backup says what goes in and how); this schedules it, holds the key, and serves the
// repository the guest writes to (backupstore.go).
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
	// backupKeyName is the repository key's file in the state dir: the household's key of record.
	backupKeyName = "backup-key"
)

// backupHostFacts are the host's own files a fresh install needs to come back as this household
// and could not mint again the same: its address, its name, its claim to that name. Never the
// node id -- that belongs to the hardware, and a new machine is a new node.
var backupHostFacts = []string{flockIDName, flockNameName, casaStateName, casaKeyName}

// backupRunner is the slice of the guest the backup costs.
type backupRunner interface {
	DataBackup(ctx context.Context, url, password string, host map[string][]byte) (bool, error)
	DataBackupResult(ctx context.Context) (guestagent.BackupState, error)
}

// backupScheduler remembers the local date it last started a backup, in the household's zone,
// and since when it has been waiting for one's report.
type backupScheduler struct {
	loc     *time.Location
	done    string
	waiting time.Time // zero when no report is awaited
}

func newBackupScheduler() *backupScheduler { return &backupScheduler{loc: householdLocation()} }

// backup collects a run that is going, or starts tonight's if it is due: backup is on, this node
// serves, and the local time is inside tonight's window.
func (cfg Config) backup(ctx context.Context, g backupRunner, b *backupScheduler, serving bool, now time.Time, logf func(string, ...any)) {
	if cfg.stateDir() == "" || cfg.hostNodeIP() == "" || !serving {
		return
	}
	if !b.waiting.IsZero() {
		cfg.collectBackup(ctx, g, b, logf)
		return
	}
	if cfg.backupFolder() == "" {
		return
	}
	local := now.In(b.loc)
	day := local.Format(time.DateOnly)
	mins := local.Hour()*60 + local.Minute()
	if b.done == day || mins < backupAt || mins >= backupAt+backupWindow {
		return
	}
	b.done = day
	key, err := backupKey(cfg.stateDir())
	if err != nil {
		logf("backup: no repository key: %v", err)
		return
	}
	url := "http://" + net.JoinHostPort(cfg.hostNodeIP(), backupStorePort) + "/"
	sctx, cancel := context.WithTimeout(ctx, backupCallTimeout)
	started, err := g.DataBackup(sctx, url, key, cfg.backupHostFacts(logf))
	cancel()
	if err != nil {
		logf("backup: could not start tonight's run: %v", err)
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
// last night -- what the page says -- unless there was nothing to back up.
func (cfg Config) collectBackup(ctx context.Context, g backupRunner, b *backupScheduler, logf func(string, ...any)) {
	sctx, cancel := context.WithTimeout(ctx, backupCallTimeout)
	s, err := g.DataBackupResult(sctx)
	cancel()
	switch {
	case err != nil, s.Report == nil && s.Running:
		return // asked again next cycle
	case s.Report == nil:
		logf("backup: tonight's run left no report") // the guest agent restarted and lost it
		b.waiting = time.Time{}
		return
	}
	started := b.waiting
	b.waiting = time.Time{}
	rep := *s.Report
	switch {
	case rep.Error != "":
		logf("backup: FAILED (snapshot %q): %s", rep.Snapshot, rep.Error)
	case rep.Snapshot == "":
		logf("backup: nothing to back up (no service has a snapshot yet)")
	default:
		logf("backup: snapshot %s of %s: %d files, %d bytes added", rep.Snapshot, strings.Join(rep.Services, ", "), rep.Files, rep.BytesAdded)
	}
	if rep.Error == "" && rep.Snapshot == "" {
		return
	}
	last := &dashboard.BackupRun{At: started, Snapshot: rep.Snapshot, BytesAdded: rep.BytesAdded, Error: rep.Error}
	if err := cfg.updateBackupRecord(func(r *backupRecord) { r.Last = last }); err != nil {
		logf("backup: could not record tonight's run: %v", err)
	}
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

// backupKey is the repository key, minted the first time it is asked for -- the agent's start, when
// the backup is on, so the page can show it from the first open -- and kept 0600 in the state dir.
//
// ⚠️ MINTED ONLY WHEN THE FILE IS ABSENT, never when it cannot be read: a new key over an
// unreadable old one would orphan every snapshot the old one opens. The repository's own init
// refuses a second key the same way (a repository that exists is never re-created).
//
// Base32 in groups of four: a person copies it by hand from the dashboard, and 160 bits read
// aloud is eight short groups.
func backupKey(dir string) (string, error) {
	path := filepath.Join(dir, backupKeyName)
	b, err := os.ReadFile(path)
	if err == nil {
		if k := strings.TrimSpace(string(b)); k != "" {
			return k, nil
		}
		return "", errors.New(path + " is empty")
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	raw := make([]byte, 20)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	enc := strings.ToLower(base32.StdEncoding.EncodeToString(raw))
	var groups []string
	for i := 0; i < len(enc); i += 4 {
		groups = append(groups, enc[i:i+4])
	}
	k := strings.Join(groups, "-")
	if err := atomicfile.Write(path, []byte(k+"\n"), 0o600, 0o700); err != nil {
		return "", err
	}
	return k, nil
}

// backupRecordName is the backup's node-local record beside the key: what the household told the
// page, and what the nights did. Node-scoped, like the key it sits beside.
const backupRecordName = "backup.json"

// backupRecord is that file. Folder is the folder the backup last ran into, kept while it is off
// so turning it on again knows where; KeySaved is the household's word that the key has a copy
// off this machine; Last is the newest night.
type backupRecord struct {
	Folder   string               `json:"folder,omitempty"`
	KeySaved bool                 `json:"keySaved,omitempty"`
	Last     *dashboard.BackupRun `json:"last,omitempty"`
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
// key if one has been minted -- read, never minted here -- and the record.
func (cfg Config) backupView() dashboard.Backup {
	rec := cfg.readBackupRecord()
	v := dashboard.Backup{Folder: cfg.backupFolder(), On: cfg.backupFolder() != "", KeySaved: rec.KeySaved, Last: rec.Last}
	if !v.On {
		v.Folder = rec.Folder
		if v.Folder == "" {
			v.Folder = defaultBackupDir // where turning it on goes (applyBackupSetting)
		}
	}
	if b, err := os.ReadFile(filepath.Join(cfg.stateDir(), backupKeyName)); err == nil {
		v.Key = strings.TrimSpace(string(b))
	}
	return v
}

// backupViewGuest is the slice of the guest the page's copy costs.
type backupViewGuest interface {
	DashboardBackup(ctx context.Context, b dashboard.Backup) error
}

// pushBackupView hands the guest the page's view when it differs from what this connection last
// pushed. pushed starts empty with every connection, so a fresh guest is always told.
func (cfg Config) pushBackupView(ctx context.Context, r any, pushed *string, logf func(string, ...any)) {
	g, ok := r.(backupViewGuest)
	if !ok || cfg.stateDir() == "" {
		return
	}
	v := cfg.backupView()
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
//     again. Choosing a DIFFERENT folder is not built: that needs the folder checked (absolute,
//     a person's, writable) before the host writes into it as root.
//   - backup-key-saved: "yes" records that the household has the key somewhere else.
const (
	settingBackupDir      = "backup-dir"
	settingBackupKeySaved = "backup-key-saved"
)

// isBackupSetting says whether a config-set payload names one of the backup's settings -- the only
// config-set the guest's page may ask for (adminport.go).
func isBackupSetting(payload string) bool {
	var s api.ConfigSetting
	if json.Unmarshal([]byte(payload), &s) != nil {
		return false
	}
	return s.Key == settingBackupDir || s.Key == settingBackupKeySaved
}

// applyBackupSetting applies a config-set naming a backup setting; ok is false for any other key.
func (cfg Config) applyBackupSetting(d api.Directive, logf func(string, ...any)) (out api.DirectiveOutcome, ok bool) {
	var s api.ConfigSetting
	if err := json.Unmarshal([]byte(d.Payload), &s); err != nil || (s.Key != settingBackupDir && s.Key != settingBackupKeySaved) {
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
		if err := cfg.updateBackupRecord(func(r *backupRecord) { r.KeySaved = true }); err != nil {
			return failed("could not record it: %v", err)
		}
		logf("backup: the household has saved the recovery key")
		return done("noted: the recovery key is saved")
	}
	now := cfg.backupFolder()
	want := s.Value
	if want != "" {
		back := cfg.readBackupRecord().Folder
		if back == "" {
			back = defaultBackupDir
		}
		if want != back && want != now {
			return failed("the backup goes back to %s; choosing another folder is not possible yet", back)
		}
		if want == defaultBackupDir {
			if err := os.MkdirAll(want, 0o700); err != nil {
				return failed("%v", err)
			}
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
		if err := cfg.updateBackupRecord(func(r *backupRecord) { r.Folder = want }); err != nil {
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
