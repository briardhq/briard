package host

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"errors"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"briard.io/agent/guestagent"
	"briard.io/shared/atomicfile"
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
	if cfg.BackupDir == "" || cfg.stateDir() == "" || cfg.hostNodeIP() == "" || !serving {
		return
	}
	if !b.waiting.IsZero() {
		collectBackup(ctx, g, b, logf)
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

// collectBackup asks for a started run's report, and logs it once it has come.
func collectBackup(ctx context.Context, g backupRunner, b *backupScheduler, logf func(string, ...any)) {
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

// backupKey is the repository key, minted on the first run and kept 0600 in the state dir.
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
