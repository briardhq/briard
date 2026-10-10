package host

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"briard.io/agent/guestagent"
	"briard.io/shared/api"
	"briard.io/shared/dashboard"
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

// liveBackup is a node whose backup folder can be switched at runtime, with a config file.
func liveBackup(t *testing.T, folder string) Config {
	t.Helper()
	cfg, _, _ := backupRig(t)
	cfg.BackupDir = folder
	cfg.backupLive = &atomic.Pointer[string]{}
	cfg.backupLive.Store(&folder)
	conf := filepath.Join(t.TempDir(), "config.env")
	if err := os.WriteFile(conf, []byte("DATA_DISK=/d\nBACKUP_DIR="+folder+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BRIARD_CONFIG", conf)
	return cfg
}

func setBackup(cfg Config, key, value string) api.DirectiveOutcome {
	raw, _ := json.Marshal(api.ConfigSetting{Key: key, Value: value})
	out, ok := cfg.applyBackupSetting(api.Directive{ID: "d", Kind: api.DirectiveConfigSet, Payload: string(raw)}, quiet)
	if !ok {
		return api.DirectiveOutcome{State: "not-a-backup-setting"}
	}
	return out
}

// Off writes BACKUP_DIR= (the decision, not the default), takes effect at once and leaves the
// folder remembered; on returns there; any other folder is refused; and the key's
// acknowledgement is recorded.
func TestTheBackupTurnsOffAndBackOn(t *testing.T) {
	folder := filepath.Join(t.TempDir(), "Briard Backup")
	cfg := liveBackup(t, folder)
	if err := cfg.updateBackupRecord(func(r *backupRecord) { r.Folder = folder }); err != nil {
		t.Fatal(err)
	}
	conf := os.Getenv("BRIARD_CONFIG")

	if o := setBackup(cfg, settingBackupDir, ""); o.State != api.OutcomeDone {
		t.Fatalf("off: %+v", o)
	}
	b, _ := os.ReadFile(conf)
	if !strings.Contains(string(b), "\nBACKUP_DIR=\n") || !strings.Contains(string(b), "DATA_DISK=/d") {
		t.Fatalf("config after off:\n%s", b)
	}
	if cfg.backupFolder() != "" {
		t.Fatal("off did not take effect")
	}
	if v := cfg.backupView(); v.On || v.Folder != folder {
		t.Fatalf("off view %+v, want off and the folder it returns to", v)
	}

	if o := setBackup(cfg, settingBackupDir, "/etc"); o.State != api.OutcomeFailed {
		t.Fatalf("another folder was accepted: %+v", o)
	}
	if o := setBackup(cfg, settingBackupDir, folder); o.State != api.OutcomeDone || cfg.backupFolder() != folder {
		t.Fatalf("on: %+v, folder %q", o, cfg.backupFolder())
	}
	if b, _ := os.ReadFile(conf); !strings.Contains(string(b), "BACKUP_DIR="+folder+"\n") {
		t.Fatalf("config after on:\n%s", b)
	}

	if v := cfg.backupView(); v.KeySaved {
		t.Fatal("saved before anyone said so")
	}
	if o := setBackup(cfg, settingBackupKeySaved, "yes"); o.State != api.OutcomeDone || !cfg.backupView().KeySaved {
		t.Fatalf("saved: %+v", o)
	}
	if o := setBackup(cfg, "vip", "dhcp"); o.State != "not-a-backup-setting" {
		t.Fatal("the address was taken for a backup setting")
	}
}

// With no folder ever recorded, on goes to the agent's own -- and the view says so while off.
func TestTheBackupWithNoFolderGoesToTheAgentsOwn(t *testing.T) {
	cfg := liveBackup(t, "")
	if v := cfg.backupView(); v.On || v.Folder != defaultBackupDir {
		t.Fatalf("view %+v, want off, returning to %s", v, defaultBackupDir)
	}
	if o := setBackup(cfg, settingBackupDir, "/somewhere/else"); o.State != api.OutcomeFailed {
		t.Fatalf("a folder never used was accepted: %+v", o)
	}
}

// The guest's page may ask for the backup's settings, and for no other config-set.
func TestTheGuestMayAskForTheBackupSettingsOnly(t *testing.T) {
	ask := func(key string) bool {
		raw, _ := json.Marshal(api.ConfigSetting{Key: key, Value: ""})
		return guestMayAsk(api.Directive{Kind: api.DirectiveConfigSet, Payload: string(raw)})
	}
	if !ask(settingBackupDir) || !ask(settingBackupKeySaved) {
		t.Error("the page cannot turn the backup off, or say the key is saved")
	}
	if ask("vip") || ask("") || guestMayAsk(api.Directive{Kind: api.DirectiveConfigSet, Payload: "not json"}) {
		t.Error("the guest may change a setting outside the backup")
	}
}

// The page is told the key the host holds -- read, never minted -- and told again only when the
// view moved.
func TestThePageIsToldTheViewWhenItMoves(t *testing.T) {
	cfg := liveBackup(t, "/f")
	g := &viewGuest{}
	pushed := ""
	cfg.pushBackupView(context.Background(), g, &pushed, quiet)
	if len(g.views) != 1 || g.views[0].Key != "" || !g.views[0].On {
		t.Fatalf("first push %+v", g.views)
	}
	if _, err := os.Stat(filepath.Join(cfg.stateDir(), backupKeyName)); err == nil {
		t.Fatal("telling the page minted a key")
	}
	cfg.pushBackupView(context.Background(), g, &pushed, quiet)
	if len(g.views) != 1 {
		t.Fatal("an unchanged view was pushed again")
	}
	k, _ := backupKey(cfg.stateDir())
	cfg.pushBackupView(context.Background(), g, &pushed, quiet)
	if len(g.views) != 2 || g.views[1].Key != k {
		t.Fatalf("the key did not reach the page: %+v", g.views)
	}
}

type viewGuest struct{ views []dashboard.Backup }

func (g *viewGuest) DashboardBackup(_ context.Context, b dashboard.Backup) error {
	g.views = append(g.views, b)
	return nil
}

// A finished night is the page's "last backup", with the time it started and what the folder then
// holds; a week of them is kept, newest first.
func TestTheNightsAreRecorded(t *testing.T) {
	cfg, g, b := backupRig(t)
	if err := os.WriteFile(filepath.Join(cfg.BackupDir, "pack"), make([]byte, 1000), 0o600); err != nil {
		t.Fatal(err)
	}
	for night := 0; night < backupNightsKept+2; night++ {
		*b = backupScheduler{loc: time.UTC}
		start := tonightAt("02:04").Add(time.Duration(night) * 24 * time.Hour)
		cfg.backup(context.Background(), g, b, true, start, quiet)
		g.backup.running, g.backup.report = false, &guestagent.BackupReport{Snapshot: "s", BytesAdded: int64(night)}
		cfg.backup(context.Background(), g, b, true, start.Add(time.Minute), quiet)
	}
	nights := cfg.readBackupRecord().Nights
	if len(nights) != backupNightsKept {
		t.Fatalf("%d nights kept, want %d", len(nights), backupNightsKept)
	}
	newest := nights[0]
	if newest.BytesAdded != backupNightsKept+1 || !newest.At.Equal(tonightAt("02:04").Add(time.Duration(backupNightsKept+1)*24*time.Hour)) || newest.Size != 1000 {
		t.Fatalf("newest night %+v, want the last one, at its start, with the folder's 1000 bytes", newest)
	}
}

// What a new folder must be before root writes into it and serves it to the guest.
func TestANewFolderIsChecked(t *testing.T) {
	empty := t.TempDir()
	markers := t.TempDir()
	if err := os.WriteFile(filepath.Join(markers, ".stfolder"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "config"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(repo, "keys"), 0o700); err != nil {
		t.Fatal(err)
	}
	kube := t.TempDir()
	if err := os.WriteFile(filepath.Join(kube, "config"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	for dir, ok := range map[string]bool{empty: true, markers: true, repo: true} {
		if err := checkBackupFolder(dir); (err == nil) != ok {
			t.Errorf("%s: %v", dir, err)
		}
	}
	missing := filepath.Join(empty, "not-mounted")
	for _, dir := range []string{"relative/dir", empty + "/../" + filepath.Base(empty), missing, kube, "/"} {
		if err := checkBackupFolder(dir); err == nil {
			t.Errorf("%s was accepted", dir)
		}
	}
	if _, err := os.Stat(missing); err == nil {
		t.Error("checking a missing folder created it")
	}
	if err := checkBackupFolder("/"); err == nil || !strings.Contains(err.Error(), "root") {
		t.Errorf("a root-owned folder: %v", err)
	}
	if entries, _ := os.ReadDir(empty); len(entries) != 0 {
		t.Errorf("the check left %v behind", entries)
	}
}

// A move: the new folder from tonight, the old one remembered as holding the earlier backups --
// and nothing done to either folder.
func TestTheBackupMoves(t *testing.T) {
	old := filepath.Join(t.TempDir(), "Briard Backup")
	if err := os.Mkdir(old, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := liveBackup(t, old)
	if err := cfg.updateBackupRecord(func(r *backupRecord) { r.Folder = old }); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(old, "README.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	moved := t.TempDir()
	if o := setBackup(cfg, settingBackupDir, moved); o.State != api.OutcomeDone || cfg.backupFolder() != moved {
		t.Fatalf("move: %+v, folder %q", o, cfg.backupFolder())
	}
	if v := cfg.backupView(); v.Folder != moved || v.Previous != old {
		t.Fatalf("view %+v, want the new folder and the old one named", v)
	}
	if b, _ := os.ReadFile(os.Getenv("BRIARD_CONFIG")); !strings.Contains(string(b), "BACKUP_DIR="+moved+"\n") {
		t.Fatalf("config:\n%s", b)
	}
	if _, err := os.Stat(filepath.Join(old, "README.txt")); err != nil {
		t.Fatal("the old folder was touched")
	}
	// Off and on again comes back to the new folder, not the old.
	setBackup(cfg, settingBackupDir, "")
	if o := setBackup(cfg, settingBackupDir, moved); o.State != api.OutcomeDone || cfg.readBackupRecord().Previous != old {
		t.Fatalf("on again: %+v, record %+v", o, cfg.readBackupRecord())
	}
	if o := setBackup(cfg, settingBackupDir, "/etc"); o.State != api.OutcomeFailed {
		t.Fatalf("a root folder was accepted: %+v", o)
	}
}
