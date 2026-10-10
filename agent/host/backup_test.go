package host

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"briard.io/agent/guestagent"
)

// backupRig is a node whose state dir holds a flock id and name (no casa claim yet) and whose
// guest runs backups, scheduled in UTC.
func backupRig(t *testing.T) (Config, fakeStatus, *backupScheduler) {
	t.Helper()
	state := t.TempDir()
	for name, v := range map[string]string{flockIDName: "f1d\n", flockNameName: "maple\n", nodeIDName: "n0de\n"} {
		if err := os.WriteFile(filepath.Join(state, name), []byte(v), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cfg := Config{BackupDir: t.TempDir(), AssignmentCache: filepath.Join(state, "assignment.json"), SystemHostCIDR: "10.7.0.129/32"}
	return cfg, fakeStatus{backup: &fakeBackup{}}, &backupScheduler{loc: time.UTC}
}

func tonightAt(hhmm string) time.Time {
	t, _ := time.Parse("2006-01-02 15:04", "2026-10-10 "+hhmm)
	return t
}

// TestTheBackupStartsOnceANightOnTheServingNode: tonight's window, once, on the node that holds the
// volume, with the store's URL, the key and the host's facts -- the node id never among them.
func TestTheBackupStartsOnceANightOnTheServingNode(t *testing.T) {
	ctx := context.Background()
	cfg, g, b := backupRig(t)
	cfg.backup(ctx, g, b, false, tonightAt("02:05"), quiet)
	cfg.backup(ctx, g, b, true, tonightAt("01:59"), quiet)
	cfg.backup(ctx, g, b, true, tonightAt("03:00"), quiet)
	if g.backup.starts != 0 {
		t.Fatalf("started %d times outside the window or off the serving node", g.backup.starts)
	}
	cfg.backup(ctx, g, b, true, tonightAt("02:05"), quiet)
	if g.backup.starts != 1 {
		t.Fatalf("started %d times in the window, want 1", g.backup.starts)
	}
	if g.backup.url != "http://10.7.0.129:7791/" {
		t.Errorf("url %q", g.backup.url)
	}
	if !regexp.MustCompile(`^([a-z2-7]{4}-){7}[a-z2-7]{4}$`).MatchString(g.backup.key) {
		t.Errorf("key %q is not eight groups of base32", g.backup.key)
	}
	if string(g.backup.host[flockIDName]) != "f1d\n" || string(g.backup.host[flockNameName]) != "maple\n" {
		t.Errorf("host facts %v", g.backup.host)
	}
	if _, ok := g.backup.host[nodeIDName]; ok {
		t.Error("the node id went into the backup; it belongs to the hardware")
	}
	if _, ok := g.backup.host[casaKeyName]; ok {
		t.Error("an absent casa key was sent")
	}

	// Running: collected, never started again -- not this night, not while it runs.
	cfg.backup(ctx, g, b, true, tonightAt("02:06"), quiet)
	g.backup.running, g.backup.report = false, &guestagent.BackupReport{Snapshot: "abc"}
	cfg.backup(ctx, g, b, true, tonightAt("02:07"), quiet)
	if !b.waiting.IsZero() {
		t.Fatal("a finished report was not collected")
	}
	cfg.backup(ctx, g, b, true, tonightAt("02:30"), quiet)
	if g.backup.starts != 1 {
		t.Fatalf("started %d times in one night, want 1", g.backup.starts)
	}
}

// TestTheBackupIsOffWhenTheFolderIsEmpty: BACKUP_DIR="" is the household turning it off.
func TestTheBackupIsOffWhenTheFolderIsEmpty(t *testing.T) {
	cfg, g, b := backupRig(t)
	cfg.BackupDir = ""
	cfg.backup(context.Background(), g, b, true, tonightAt("02:05"), quiet)
	if g.backup.starts != 0 {
		t.Fatal("a disabled backup ran")
	}
	if _, err := os.Stat(filepath.Join(cfg.stateDir(), backupKeyName)); err == nil {
		t.Fatal("a disabled backup minted a key")
	}
}

// TestTheKeyIsMintedOnceAndNeverOverAnUnreadableOne: the same key every night, 0600; and a key
// file that cannot be read stops the run rather than being replaced -- a new key would orphan
// every snapshot the old one opens.
func TestTheKeyIsMintedOnceAndNeverOverAnUnreadableOne(t *testing.T) {
	dir := t.TempDir()
	k1, err := backupKey(dir)
	if err != nil {
		t.Fatal(err)
	}
	k2, err := backupKey(dir)
	if err != nil || k2 != k1 {
		t.Fatalf("second read %q %v, want %q", k2, err, k1)
	}
	if fi, err := os.Stat(filepath.Join(dir, backupKeyName)); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("key file %v %v, want 0600", fi.Mode(), err)
	}

	bad := t.TempDir()
	// A link to itself: the read fails (ELOOP, as root too) with something other than "absent",
	// while a rename over it would succeed -- so only the guard stands between it and a new key.
	p := filepath.Join(bad, backupKeyName)
	if err := os.Symlink(p, p); err != nil {
		t.Fatal(err)
	}
	if k, err := backupKey(bad); err == nil {
		t.Fatalf("minted %q over an unreadable key", k)
	}

	cfg, g, b := backupRig(t)
	if err := os.Mkdir(filepath.Join(cfg.stateDir(), backupKeyName), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg.backup(context.Background(), g, b, true, tonightAt("02:05"), quiet)
	if g.backup.starts != 0 {
		t.Fatal("a run started without the household's key")
	}
}

// TestBackupDirIsDeclared: absent is the shipped folder, empty is off -- the one key whose empty
// value is the household's decision.
func TestBackupDirIsDeclared(t *testing.T) {
	t.Setenv("BRIARD_CONFIG", filepath.Join(t.TempDir(), "none"))
	os.Unsetenv("BACKUP_DIR")
	if d := ConfigFromEnv().BackupDir; d != defaultBackupDir {
		t.Errorf("absent: %q, want %q", d, defaultBackupDir)
	}
	t.Setenv("BACKUP_DIR", "")
	if d := ConfigFromEnv().BackupDir; d != "" {
		t.Errorf("empty: %q, want off", d)
	}
	t.Setenv("BACKUP_DIR", "/home/ana/Briard Backup")
	if d := ConfigFromEnv().BackupDir; d != "/home/ana/Briard Backup" {
		t.Errorf("set: %q", d)
	}
}
