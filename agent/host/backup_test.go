package host

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"briard.io/agent/guestagent"
	"briard.io/shared/api"
	"briard.io/shared/dashboard"
	"briard.io/shared/notify"
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
	cfg := Config{BackupDir: t.TempDir(), FlockID: "f1d", AssignmentCache: filepath.Join(state, "assignment.json"), SystemHostCIDR: "10.7.0.129/32"}
	return cfg, fakeStatus{backup: &fakeBackup{}}, utcScheduler()
}

func tonightAt(hhmm string) time.Time {
	t, _ := time.Parse("2006-01-02 15:04", "2026-10-10 "+hhmm)
	return t
}

// TestTheBackupStartsOnceANightOnTheServingNode: tonight's window, once, on the node that holds the
// volume, with the store's URL, the flock's id as the group and the host's facts -- the node id never
// among them.
func TestTheBackupStartsOnceANightOnTheServingNode(t *testing.T) {
	ctx := context.Background()
	cfg, g, b := backupRig(t)
	cfg.backup(ctx, g, b, false, tonightAt("02:05"), nil, quiet)
	cfg.backup(ctx, g, b, true, tonightAt("01:59"), nil, quiet)
	cfg.backup(ctx, g, b, true, tonightAt("03:00"), nil, quiet)
	if g.backup.starts != 0 {
		t.Fatalf("started %d times outside the window or off the serving node", g.backup.starts)
	}
	cfg.backup(ctx, g, b, true, tonightAt("02:05"), nil, quiet)
	if g.backup.starts != 1 {
		t.Fatalf("started %d times in the window, want 1", g.backup.starts)
	}
	if g.backup.url != "http://10.7.0.129:7791/" {
		t.Errorf("url %q", g.backup.url)
	}
	if g.backup.group != "f1d" {
		t.Errorf("group %q, want the flock's id", g.backup.group)
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
	cfg.backup(ctx, g, b, true, tonightAt("02:06"), nil, quiet)
	g.backup.running, g.backup.report = false, &guestagent.BackupReport{Snapshot: "abc"}
	cfg.backup(ctx, g, b, true, tonightAt("02:07"), nil, quiet)
	if !b.waiting.IsZero() {
		t.Fatal("a finished report was not collected")
	}
	cfg.backup(ctx, g, b, true, tonightAt("02:30"), nil, quiet)
	if g.backup.starts != 1 {
		t.Fatalf("started %d times in one night, want 1", g.backup.starts)
	}
}

// TestTheBackupIsOffWhenTheFolderIsEmpty: BACKUP_DIR="" is the household turning it off.
func TestTheBackupIsOffWhenTheFolderIsEmpty(t *testing.T) {
	cfg, g, b := backupRig(t)
	cfg.BackupDir = ""
	cfg.backup(context.Background(), g, b, true, tonightAt("02:05"), nil, quiet)
	if g.backup.starts != 0 {
		t.Fatal("a disabled backup ran")
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
func liveBackup(t *testing.T, folder string) (Config, *fakeBackup) {
	t.Helper()
	cfg, fg, _ := backupRig(t)
	cfg.BackupDir = folder
	cfg.backupLive = &atomic.Pointer[string]{}
	cfg.backupLive.Store(&folder)
	conf := filepath.Join(t.TempDir(), "config.env")
	if err := os.WriteFile(conf, []byte("DATA_DISK=/d\nBACKUP_DIR="+folder+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BRIARD_CONFIG", conf)
	return cfg, fg.backup
}

func setBackup(cfg Config, g *fakeBackup, key, value string) api.DirectiveOutcome {
	raw, _ := json.Marshal(api.ConfigSetting{Key: key, Value: value})
	out, ok := cfg.applyBackupSetting(context.Background(), api.Directive{ID: "d", Kind: api.DirectiveConfigSet, Payload: string(raw)}, fakeStatus{backup: g}, quiet)
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
	cfg, g := liveBackup(t, folder)
	ctx := context.Background()
	if err := cfg.updateBackupRecord(func(r *backupRecord) { r.Folder = folder }); err != nil {
		t.Fatal(err)
	}
	conf := os.Getenv("BRIARD_CONFIG")

	if o := setBackup(cfg, g, settingBackupDir, ""); o.State != api.OutcomeDone {
		t.Fatalf("off: %+v", o)
	}
	b, _ := os.ReadFile(conf)
	if !strings.Contains(string(b), "\nBACKUP_DIR=\n") || !strings.Contains(string(b), "DATA_DISK=/d") {
		t.Fatalf("config after off:\n%s", b)
	}
	if cfg.backupFolder() != "" {
		t.Fatal("off did not take effect")
	}
	if v := cfg.backupView(ctx, &viewGuest{fakeStatus: fakeStatus{backup: g}}); v.On || v.Folder != folder {
		t.Fatalf("off view %+v, want off and the folder it returns to", v)
	}

	if o := setBackup(cfg, g, settingBackupDir, "/etc"); o.State != api.OutcomeFailed {
		t.Fatalf("another folder was accepted: %+v", o)
	}
	if o := setBackup(cfg, g, settingBackupDir, folder); o.State != api.OutcomeDone || cfg.backupFolder() != folder {
		t.Fatalf("on: %+v, folder %q", o, cfg.backupFolder())
	}
	if b, _ := os.ReadFile(conf); !strings.Contains(string(b), "BACKUP_DIR="+folder+"\n") {
		t.Fatalf("config after on:\n%s", b)
	}

	if v := cfg.backupView(ctx, &viewGuest{fakeStatus: fakeStatus{backup: g}}); v.KeySaved {
		t.Fatal("saved before anyone said so")
	}
	if o := setBackup(cfg, g, settingBackupKeySaved, "yes"); o.State != api.OutcomeDone || !cfg.backupView(ctx, &viewGuest{fakeStatus: fakeStatus{backup: g}}).KeySaved {
		t.Fatalf("saved: %+v", o)
	}
	if o := setBackup(cfg, g, "vip", "dhcp"); o.State != "not-a-backup-setting" {
		t.Fatal("the address was taken for a backup setting")
	}
}

// With no folder ever recorded, on goes to the agent's own -- and the view says so while off.
func TestTheBackupWithNoFolderGoesToTheAgentsOwn(t *testing.T) {
	cfg, g := liveBackup(t, "")
	ctx := context.Background()
	if v := cfg.backupView(ctx, &viewGuest{fakeStatus: fakeStatus{backup: g}}); v.On || v.Folder != defaultBackupDir {
		t.Fatalf("view %+v, want off, returning to %s", v, defaultBackupDir)
	}
	if o := setBackup(cfg, g, settingBackupDir, "/somewhere/else"); o.State != api.OutcomeFailed {
		t.Fatalf("a folder never used was accepted: %+v", o)
	}
}

// The guest's page may ask for the backup's settings, and for no other config-set.
func TestTheGuestMayAskForTheBackupSettingsOnly(t *testing.T) {
	ask := func(key string) bool {
		raw, _ := json.Marshal(api.ConfigSetting{Key: key, Value: ""})
		return guestMayAsk(api.Directive{Kind: api.DirectiveConfigSet, Payload: string(raw)})
	}
	if !ask(settingBackupDir) || !ask(settingBackupKeySaved) || !ask(settingBackupAdopt) {
		t.Error("the page cannot turn the backup off, say the key is saved, or adopt a backup")
	}
	if ask("vip") || ask("") || guestMayAsk(api.Directive{Kind: api.DirectiveConfigSet, Payload: "not json"}) {
		t.Error("the guest may change a setting outside the backup")
	}
}

// The page is told the key the volume holds -- the guest's, minted there the first time it is
// asked -- and told again only when the view moved; a guest with no volume mounted gives the page
// a view without the key, not no view.
func TestThePageIsToldTheViewWhenItMoves(t *testing.T) {
	cfg, g := liveBackup(t, "/f")
	ctx := context.Background()
	vg := &viewGuest{fakeStatus: fakeStatus{backup: g}}
	pushed := ""
	cfg.pushBackupView(ctx, vg, nil, &pushed, quiet)
	if len(vg.views) != 1 || vg.views[0].Key != g.key.Key || vg.views[0].Key == "" || !vg.views[0].On || vg.views[0].KeySaved {
		t.Fatalf("first push %+v", vg.views)
	}
	cfg.pushBackupView(ctx, vg, nil, &pushed, quiet)
	if len(vg.views) != 1 {
		t.Fatal("an unchanged view was pushed again")
	}
	if o := setBackup(cfg, g, settingBackupKeySaved, "yes"); o.State != api.OutcomeDone {
		t.Fatalf("saved: %+v", o)
	}
	cfg.pushBackupView(ctx, vg, nil, &pushed, quiet)
	if len(vg.views) != 2 || !vg.views[1].KeySaved {
		t.Fatalf("the acknowledgement did not reach the page: %+v", vg.views)
	}

	none := &viewGuest{fakeStatus: fakeStatus{}} // no volume here: the verb fails
	pushed = ""
	cfg.pushBackupView(ctx, none, nil, &pushed, quiet)
	if len(none.views) != 1 || none.views[0].Key != "" || !none.views[0].On {
		t.Fatalf("push with no key %+v", none.views)
	}
}

type viewGuest struct {
	fakeStatus
	views []dashboard.Backup
}

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
		*b = *utcScheduler()
		start := tonightAt("02:04").Add(time.Duration(night) * 24 * time.Hour)
		cfg.backup(context.Background(), g, b, true, start, nil, quiet)
		g.backup.running, g.backup.report = false, &guestagent.BackupReport{Snapshot: "s", BytesAdded: int64(night)}
		cfg.backup(context.Background(), g, b, true, start.Add(time.Minute), nil, quiet)
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
	// A root-owned folder is a Linux refusal: Windows has no owner check (OwnerUID).
	if err := checkBackupFolder("/"); runtime.GOOS == "linux" && (err == nil || !strings.Contains(err.Error(), "root")) {
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
	cfg, g := liveBackup(t, old)
	ctx := context.Background()
	if err := cfg.updateBackupRecord(func(r *backupRecord) { r.Folder = old }); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(old, "README.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	moved := t.TempDir()
	if o := setBackup(cfg, g, settingBackupDir, moved); o.State != api.OutcomeDone || cfg.backupFolder() != moved {
		t.Fatalf("move: %+v, folder %q", o, cfg.backupFolder())
	}
	if v := cfg.backupView(ctx, &viewGuest{fakeStatus: fakeStatus{backup: g}}); v.Folder != moved || v.Previous != old {
		t.Fatalf("view %+v, want the new folder and the old one named", v)
	}
	if b, _ := os.ReadFile(os.Getenv("BRIARD_CONFIG")); !strings.Contains(string(b), "BACKUP_DIR="+moved+"\n") {
		t.Fatalf("config:\n%s", b)
	}
	if _, err := os.Stat(filepath.Join(old, "README.txt")); err != nil {
		t.Fatal("the old folder was touched")
	}
	// Off and on again comes back to the new folder, not the old.
	setBackup(cfg, g, settingBackupDir, "")
	if o := setBackup(cfg, g, settingBackupDir, moved); o.State != api.OutcomeDone || cfg.readBackupRecord().Previous != old {
		t.Fatalf("on again: %+v, record %+v", o, cfg.readBackupRecord())
	}
	if o := setBackup(cfg, g, settingBackupDir, "/etc"); o.State != api.OutcomeFailed {
		t.Fatalf("a root folder was accepted: %+v", o)
	}
}

// backupNight runs one night through the scheduler: started at 02:04 on day d, and finished with rep (nil
// leaves the guest's report lost).
func backupNight(t *testing.T, cfg Config, g fakeStatus, n *fakeNotifier, d int, rep *guestagent.BackupReport) {
	t.Helper()
	b := utcScheduler()
	start := tonightAt("02:04").Add(time.Duration(d) * 24 * time.Hour)
	cfg.backup(context.Background(), g, b, true, start, n, quiet)
	if g.backup != nil {
		g.backup.running, g.backup.report = false, rep
	}
	cfg.backup(context.Background(), g, b, true, start.Add(time.Minute), n, quiet)
}

func backupAlerts(n *fakeNotifier) []notify.Alert {
	var out []notify.Alert
	for _, a := range n.alerts {
		if a.Key == "backup" {
			out = append(out, a)
		}
	}
	return out
}

var failedRun = &guestagent.BackupReport{Error: "Fatal: unable to save snapshot"}

// One failed night is a laptop lid; the second in a row reaches the household, as a Warning that
// names what went wrong; the next good night ends it.
func TestTheSecondFailedNightAlerts(t *testing.T) {
	cfg, g, _ := backupRig(t)
	n := &fakeNotifier{}
	backupNight(t, cfg, g, n, 0, failedRun)
	if a := backupAlerts(n); len(a) != 0 {
		t.Fatalf("one failed night alerted: %+v", a)
	}
	backupNight(t, cfg, g, n, 1, failedRun)
	a := backupAlerts(n)
	if len(a) != 1 || a[0].Kind != notify.Open || a[0].Severity != notify.Warning || !strings.Contains(a[0].Body, "unable to save snapshot") {
		t.Fatalf("after two failed nights: %+v", a)
	}
	backupNight(t, cfg, g, n, 2, &guestagent.BackupReport{Snapshot: "s"})
	if a := backupAlerts(n); len(a) != 2 || a[1].Kind != notify.Resolved {
		t.Fatalf("a good night did not end it: %+v", a)
	}
}

// A good night between two failed ones starts the count again.
func TestAGoodNightBreaksTheStreak(t *testing.T) {
	cfg, g, _ := backupRig(t)
	n := &fakeNotifier{}
	backupNight(t, cfg, g, n, 0, failedRun)
	backupNight(t, cfg, g, n, 1, &guestagent.BackupReport{Snapshot: "s"})
	backupNight(t, cfg, g, n, 2, failedRun)
	for _, a := range backupAlerts(n) {
		if a.Kind == notify.Open {
			t.Fatalf("failures either side of a good night alerted: %+v", a)
		}
	}
}

// Nights that never ran are failed nights: the guest could not be asked, or lost the run.
func TestNightsThatNeverRanCount(t *testing.T) {
	cfg, g, _ := backupRig(t)
	n := &fakeNotifier{}
	backupNight(t, cfg, g, n, 0, nil) // the report was lost
	none := fakeStatus{}              // a guest that cannot be asked
	backupNight(t, cfg, none, n, 1, nil)
	if a := backupAlerts(n); len(a) != 1 || a[0].Kind != notify.Open {
		t.Fatalf("two nights that never ran: %+v", a)
	}
	if got := len(cfg.readBackupRecord().Nights); got != 2 {
		t.Fatalf("%d nights recorded, want 2", got)
	}
}

// What the household is told to do, from the folder itself: connect it, fix it, or the run's own
// words. Each is also what the card shows for that night.
func TestTheFailureSaysWhatToDo(t *testing.T) {
	for name, tc := range map[string]struct {
		folder func(t *testing.T) string
		want   string
	}{
		"missing":  {func(t *testing.T) string { return filepath.Join(t.TempDir(), "unplugged") }, "is not there. If it is on a disk or a network drive, connect it"},
		"unusable": {func(t *testing.T) string { p := filepath.Join(t.TempDir(), "f"); os.WriteFile(p, nil, 0o600); return p }, "cannot write to the backup folder"},
		"run":      {func(t *testing.T) string { return t.TempDir() }, "the backup did not finish: Fatal: unable to save snapshot"},
	} {
		cfg, g, _ := backupRig(t)
		cfg.BackupDir = tc.folder(t)
		n := &fakeNotifier{}
		backupNight(t, cfg, g, n, 0, failedRun)
		backupNight(t, cfg, g, n, 1, failedRun)
		a := backupAlerts(n)
		if len(a) != 1 || !strings.Contains(a[0].Body, tc.want) {
			t.Errorf("%s: %+v, want the body to say %q", name, a, tc.want)
		}
		if nights := cfg.readBackupRecord().Nights; len(nights) == 0 || !strings.Contains(nights[0].Error, tc.want) {
			t.Errorf("%s: the card's night says %+v", name, nights)
		}
	}
}

// Turned off, the backup is no longer asked of the folder: an open alert ends at the next window.
func TestTurningTheBackupOffEndsTheAlert(t *testing.T) {
	cfg, g, _ := backupRig(t)
	n := &fakeNotifier{}
	backupNight(t, cfg, g, n, 0, failedRun)
	backupNight(t, cfg, g, n, 1, failedRun)
	cfg.BackupDir = ""
	backupNight(t, cfg, g, n, 2, nil)
	a := backupAlerts(n)
	if len(a) != 2 || a[1].Kind != notify.Resolved || !strings.Contains(a[1].Body, "turned off") {
		t.Fatalf("off: %+v", a)
	}
}

// TestAdoptingABackup: the typed key reaches the guest once and is kept nowhere; a run is kicked
// so the page sees the folder working; and a folder with no repository, a backup that is off, or
// a guest without the verb refuse before the guest is asked.
func TestAdoptingABackup(t *testing.T) {
	folder := filepath.Join(t.TempDir(), "Briard Backup")
	for _, p := range []string{"config", "keys/k1"} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(folder, p)), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(folder, p), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cfg, g := liveBackup(t, folder)
	cfg.backupKick = &atomic.Bool{}
	if o := setBackup(cfg, g, settingBackupAdopt, "old-key"); o.State != api.OutcomeDone {
		t.Fatalf("adopt: %+v", o)
	}
	if g.adopted != "old-key" || !cfg.backupKick.Load() {
		t.Fatalf("adopted with %q, kicked %v", g.adopted, cfg.backupKick.Load())
	}
	if b, _ := os.ReadFile(filepath.Join(cfg.stateDir(), backupRecordName)); strings.Contains(string(b), "old-key") {
		t.Fatal("the typed key was recorded")
	}
	// The kicked run starts outside the window, once.
	bs := utcScheduler()
	cfg.backup(context.Background(), fakeStatus{backup: g}, bs, true, tonightAt("14:00"), nil, quiet)
	cfg.backup(context.Background(), fakeStatus{backup: g}, bs, true, tonightAt("14:01"), nil, quiet)
	if g.starts != 1 {
		t.Fatalf("the kicked run started %d times, want 1", g.starts)
	}

	g.adopted, g.adoptErr = "", errors.New("that key does not open the backup in this folder")
	if o := setBackup(cfg, g, settingBackupAdopt, "wrong"); o.State != api.OutcomeFailed || !strings.Contains(o.Detail, "does not open") {
		t.Fatalf("a wrong key: %+v", o)
	}
	g.adopted, g.adoptErr = "", nil
	if err := os.Remove(filepath.Join(folder, "config")); err != nil {
		t.Fatal(err)
	}
	if o := setBackup(cfg, g, settingBackupAdopt, "k"); o.State != api.OutcomeFailed || g.adopted != "" {
		t.Fatalf("no repository, yet asked the guest (%q): %+v", g.adopted, o)
	}
	g.noAdopt = true
	if o := setBackup(cfg, g, settingBackupAdopt, "k"); o.State != api.OutcomeFailed || !strings.Contains(o.Detail, "update the guest") {
		t.Fatalf("an old guest: %+v", o)
	}
	g.noAdopt = false
	setBackup(cfg, g, settingBackupDir, "")
	if o := setBackup(cfg, g, settingBackupAdopt, "k"); o.State != api.OutcomeFailed || g.adopted != "" {
		t.Fatalf("off, yet asked the guest: %+v", o)
	}
}

// TestAWrongKeyNightIsNamedForThePage: the night's record carries the flag the card acts on, and
// its words say what to do.
func TestAWrongKeyNightIsNamedForThePage(t *testing.T) {
	cfg, g, b := backupRig(t)
	cfg.backup(context.Background(), g, b, true, tonightAt("02:04"), nil, quiet)
	g.backup.running, g.backup.report = false, &guestagent.BackupReport{Error: "the folder holds a backup made with another key", WrongKey: true}
	cfg.backup(context.Background(), g, b, true, tonightAt("02:05"), nil, quiet)
	nights := cfg.readBackupRecord().Nights
	if len(nights) != 1 || !nights[0].WrongKey || !strings.Contains(nights[0].Error, "Enter that key") {
		t.Fatalf("nights %+v, want one wrong-key night that says what to do", nights)
	}
}

// utcScheduler is the agent's scheduler, scheduled in UTC.
func utcScheduler() *backupScheduler {
	b := newBackupScheduler()
	b.loc = time.UTC
	return b
}

// syncFake is a sync client: the folders it holds, and the files it has uploaded.
type syncFake struct {
	name     string
	uploaded map[string]bool
	asked    []string
	fail     error // every file's state unreadable
}

func (f *syncFake) check() offsiteCheck {
	return offsiteCheck{
		provider: func(string) (string, error) {
			if f.name == "" {
				return "", errors.New("not under a sync root")
			}
			return f.name, nil
		},
		inSync: func(p string) error {
			f.asked = append(f.asked, filepath.Base(p))
			switch {
			case f.fail != nil:
				return f.fail
			case !f.uploaded[filepath.Base(p)]:
				return errors.New("not uploaded yet")
			}
			return nil
		},
	}
}

// settle ticks until the check in flight has answered.
func settle(t *testing.T, o *offsiteCheck, folder string, nights []dashboard.BackupRun, now time.Time) {
	t.Helper()
	for i := 0; o.running != nil; i++ {
		if i > 400 {
			t.Fatal("the off-site check never answered")
		}
		time.Sleep(5 * time.Millisecond)
		o.tick(folder, nights, now, quiet)
	}
}

// writeAt is a file the store wrote at a moment.
func writeAt(t *testing.T, dir, name string, at time.Time) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(name), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, at, at); err != nil {
		t.Fatal(err)
	}
}

// TestTheOffsiteCheckAsksAboutTheNewestGoodNight: the files the newest good night wrote and no
// others, asked again every offsiteRetry while one is waiting, and not again once all are
// uploaded -- until a new night.
func TestTheOffsiteCheckAsksAboutTheNewestGoodNight(t *testing.T) {
	dir := t.TempDir()
	good, failed := tonightAt("02:00"), tonightAt("02:00").Add(24*time.Hour)
	writeAt(t, dir, "config", good.Add(-72*time.Hour))
	writeAt(t, dir, "data/ab/old", good.Add(-24*time.Hour))
	writeAt(t, dir, "data/cd/pack", good.Add(10*time.Minute))
	writeAt(t, dir, "snapshots/snap", good.Add(20*time.Minute))
	// A failed night after it: not the one asked about, though its stray pack counts as written since.
	nights := []dashboard.BackupRun{{At: failed, Error: "the folder is not there"}, {At: good, Snapshot: "s1"}}
	f := &syncFake{name: "OneDrive", uploaded: map[string]bool{"pack": true}}
	o := f.check()
	now := good.Add(time.Hour)

	o.tick(dir, nights, now, quiet)
	settle(t, &o, dir, nights, now)
	if o.in != "OneDrive" || o.synced {
		t.Fatalf("one file of the night not uploaded: in=%q synced=%v", o.in, o.synced)
	}
	if strings.Join(f.asked, ",") != "pack,snap" {
		t.Errorf("asked about %v, want the night's two files only", f.asked)
	}

	f.asked = nil
	o.tick(dir, nights, now.Add(offsiteRetry-time.Second), quiet)
	if o.running != nil {
		t.Fatal("asked again before offsiteRetry")
	}
	f.uploaded["snap"] = true
	o.tick(dir, nights, now.Add(offsiteRetry), quiet)
	settle(t, &o, dir, nights, now.Add(offsiteRetry))
	if !o.synced || o.in != "OneDrive" {
		t.Fatalf("every file uploaded: in=%q synced=%v", o.in, o.synced)
	}
	f.asked = nil
	o.tick(dir, nights, now.Add(24*time.Hour), quiet)
	if o.running != nil || len(f.asked) != 0 {
		t.Fatal("a settled night was asked about again")
	}

	// The next good night: asked about from the start, and not synced until it says so.
	next := failed.Add(24 * time.Hour)
	writeAt(t, dir, "snapshots/snap2", next.Add(time.Minute))
	nights = append([]dashboard.BackupRun{{At: next, Snapshot: "s3"}}, nights...)
	o.tick(dir, nights, next.Add(time.Hour), quiet)
	if o.synced || o.in != "" {
		t.Fatalf("a new night kept the last one's answer: in=%q synced=%v", o.in, o.synced)
	}
	settle(t, &o, dir, nights, next.Add(time.Hour))
	if o.synced || strings.Join(f.asked, ",") != "snap2" {
		t.Fatalf("the new night: synced=%v asked=%v", o.synced, f.asked)
	}
}

// TestTheOffsiteCheckCountsCouldNotTellAsWaiting: green only on a positive answer for every file.
// A night with no files found, a state that could not be read, or a folder that cannot be listed
// is waiting; a folder under no sync client is no answer, asked once; and no good night, or the
// backup off, asks nothing.
func TestTheOffsiteCheckCountsCouldNotTellAsWaiting(t *testing.T) {
	night := tonightAt("02:00")
	nights := []dashboard.BackupRun{{At: night, Snapshot: "s1"}}
	now := night.Add(time.Hour)
	ask := func(f *syncFake, dir string, nights []dashboard.BackupRun) offsiteCheck {
		o := f.check()
		o.tick(dir, nights, now, quiet)
		settle(t, &o, dir, nights, now)
		return o
	}

	empty := t.TempDir()
	writeAt(t, empty, "config", night.Add(-time.Hour))
	if o := ask(&syncFake{name: "OneDrive"}, empty, nights); o.synced || o.in != "OneDrive" || o.done {
		t.Errorf("no file of the night: in=%q synced=%v done=%v, want waiting", o.in, o.synced, o.done)
	}

	dir := t.TempDir()
	writeAt(t, dir, "snapshots/snap", night.Add(time.Minute))
	if o := ask(&syncFake{name: "OneDrive", fail: errors.New("access denied")}, dir, nights); o.synced || o.in != "OneDrive" {
		t.Errorf("unreadable state: in=%q synced=%v, want waiting", o.in, o.synced)
	}
	if runtime.GOOS != "windows" && os.Geteuid() != 0 {
		// Every file it could read uploaded, and a directory it could not list.
		part := t.TempDir()
		writeAt(t, part, "snapshots/snap", night.Add(time.Minute))
		writeAt(t, part, "data/ab/pack", night.Add(time.Minute))
		if err := os.Chmod(filepath.Join(part, "data", "ab"), 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(filepath.Join(part, "data", "ab"), 0o755) })
		if o := ask(&syncFake{name: "OneDrive", uploaded: map[string]bool{"snap": true, "pack": true}}, part, nights); o.synced {
			t.Error("a folder with a directory it could not list read as synced")
		}
	}

	f := &syncFake{uploaded: map[string]bool{"snap": true}}
	o := ask(f, dir, nights)
	if o.in != "" || o.synced || !o.done || len(f.asked) != 0 {
		t.Errorf("no sync client: in=%q synced=%v done=%v asked=%v", o.in, o.synced, o.done, f.asked)
	}
	o.tick(dir, nights, now.Add(offsiteRetry), quiet)
	if o.running != nil {
		t.Error("a folder under no sync client was asked about again the same night")
	}

	for name, c := range map[string]struct {
		dir    string
		nights []dashboard.BackupRun
	}{
		"no good night": {dir, []dashboard.BackupRun{{At: night, Error: "x"}}},
		"backup off":    {"", nights},
	} {
		o := (&syncFake{name: "OneDrive"}).check()
		o.tick(c.dir, c.nights, now, quiet)
		if o.running != nil {
			t.Errorf("%s: a check started", name)
		}
	}
}

// The page is told the check's answer with the rest of the view.
func TestThePageIsToldTheOffsiteAnswer(t *testing.T) {
	cfg, g := liveBackup(t, "/f")
	vg := &viewGuest{fakeStatus: fakeStatus{backup: g}}
	pushed := ""
	b := &backupScheduler{offsite: offsiteCheck{in: "OneDrive"}}
	cfg.pushBackupView(context.Background(), vg, b, &pushed, quiet)
	b.offsite.synced = true
	cfg.pushBackupView(context.Background(), vg, b, &pushed, quiet)
	if len(vg.views) != 2 || vg.views[0].OffsiteIn != "OneDrive" || vg.views[0].OffsiteSynced || !vg.views[1].OffsiteSynced {
		t.Fatalf("pushed %+v", vg.views)
	}
}

// fakeLister is the guest's data.backup.list.
type fakeLister struct {
	list       []dashboard.BackupListing
	err        error
	old        bool
	url, group string
}

func (f *fakeLister) DataBackupList(_ context.Context, url, group string) ([]dashboard.BackupListing, error) {
	f.url, f.group = url, group
	return f.list, f.err
}
func (f *fakeLister) SupportsBackupList() bool { return !f.old }

// TestTheBackupListIsTheGuestsReadOfTheRepository: the page's list is what the guest reads from the
// store this host serves it, under this household's group so it can say which backups are its
// own, relayed as it came. Off, or a guest without the verb, refuses before the guest is asked.
func TestTheBackupListIsTheGuestsReadOfTheRepository(t *testing.T) {
	folder := filepath.Join(t.TempDir(), "Briard Backup")
	cfg, _ := liveBackup(t, folder)
	cfg.FlockID = "f1d"
	want := []dashboard.BackupListing{{ID: "bbbb2222", At: time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC), Own: true,
		Apps: []dashboard.BackupApp{{Name: "home-assistant", Version: "2026.9.4"}}}}
	l := &fakeLister{list: want}
	// A new install's folder: empty until the first run makes the repository. No backup, and
	// nothing to ask the guest.
	if err := os.MkdirAll(folder, 0o700); err != nil {
		t.Fatal(err)
	}
	if o := cfg.applyBackupList(context.Background(), l, api.Directive{ID: "d"}); o.State != api.OutcomeDone || o.Detail != "[]" || l.url != "" {
		t.Fatalf("an empty folder: %+v, asked %q; want no backups, and the guest not asked", o, l.url)
	}
	if err := os.MkdirAll(filepath.Join(folder, "keys"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(folder, "config"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	o := cfg.applyBackupList(context.Background(), l, api.Directive{ID: "d", Kind: api.DirectiveBackupList})
	if o.State != api.OutcomeDone {
		t.Fatalf("list: %+v", o)
	}
	var got []dashboard.BackupListing
	if err := json.Unmarshal([]byte(o.Detail), &got); err != nil || len(got) != 1 || got[0].ID != want[0].ID || got[0].Apps[0].Version != "2026.9.4" {
		t.Fatalf("detail %q (%v); want the guest's list as it came", o.Detail, err)
	}
	if l.group != "f1d" || !strings.Contains(l.url, ":"+backupStorePort+"/") {
		t.Errorf("asked the guest with %q, %q; want this household's group and the store", l.url, l.group)
	}
	l.err = errors.New("repository does not exist")
	if o := cfg.applyBackupList(context.Background(), l, api.Directive{ID: "d"}); o.State != api.OutcomeFailed || !strings.Contains(o.Detail, "does not exist") {
		t.Errorf("a failed read: %+v", o)
	}
	l.err, l.old, l.url = nil, true, ""
	if o := cfg.applyBackupList(context.Background(), l, api.Directive{ID: "d"}); o.State != api.OutcomeFailed || l.url != "" || !strings.Contains(o.Detail, "update the guest") {
		t.Errorf("an old guest: %+v, asked %q", o, l.url)
	}
	l.old = false
	cfg.BackupDir = ""
	cfg.backupLive.Store(new(string))
	if o := cfg.applyBackupList(context.Background(), l, api.Directive{ID: "d"}); o.State != api.OutcomeFailed || l.url != "" {
		t.Errorf("off, yet asked the guest: %+v", o)
	}
}

// TestThePageMayRestoreFromTheBackup: the Backup page's restore and its list are the page's to
// ask, as an undo and an install are; the backup's other local-only acts and the node's own are not.
func TestThePageMayRestoreFromTheBackup(t *testing.T) {
	for _, kind := range []string{api.DirectiveBackupRestore, api.DirectiveBackupList, api.DirectiveBackupNow, api.DirectiveBackupFolder} {
		if !guestMayAsk(api.Directive{Kind: kind}) {
			t.Errorf("the page may not ask %s", kind)
		}
	}
	for _, kind := range []string{api.DirectiveStop, api.DirectiveDebugArm, api.DirectiveDoctor, api.DirectiveStatus} {
		if guestMayAsk(api.Directive{Kind: kind}) {
			t.Errorf("the page may ask %s", kind)
		}
	}
}

// `briard backup` waits for the run IT asked for: an ending older than the ask (last night's) is
// not its answer; the kicked run's is, success or failure, said as the scheduler said it.
func TestBackupNowWaitsForTheRunItAskedFor(t *testing.T) {
	cfg, _ := liveBackup(t, filepath.Join(t.TempDir(), "Briard Backup"))
	cfg.backupKick, cfg.backupEnded = &atomic.Bool{}, &atomic.Pointer[dashboard.BackupEnd]{}
	cfg.ended(time.Now().Add(-8*time.Hour), false, "last night's") // before the ask
	for _, tc := range []struct {
		failed bool
		detail string
		want   string
	}{
		{false, "backed up home-assistant: 39 files, 418 kB added (snapshot 40796b60)", api.OutcomeDone},
		{true, "Fatal: unable to save snapshot", api.OutcomeFailed},
	} {
		got := make(chan api.DirectiveOutcome, 1)
		go func() {
			got <- cfg.applyBackupNow(context.Background(), api.Directive{ID: "d", Kind: api.DirectiveBackupNow})
		}()
		for !cfg.backupKick.Load() {
			time.Sleep(time.Millisecond)
		}
		select {
		case o := <-got:
			t.Fatalf("answered %+v before its run ended -- the older ending was taken for it", o)
		case <-time.After(1500 * time.Millisecond):
		}
		cfg.backupKick.Store(false)                 // the scheduler took it up...
		cfg.ended(time.Now(), tc.failed, tc.detail) // ...and its run ended
		if o := <-got; o.State != tc.want || o.Detail != tc.detail {
			t.Errorf("outcome %+v; want %s, %q", o, tc.want, tc.detail)
		}
	}
	cfg.BackupDir = ""
	cfg.backupLive.Store(new(string))
	if o := cfg.applyBackupNow(context.Background(), api.Directive{ID: "d"}); o.State != api.OutcomeFailed || !strings.Contains(o.Detail, "turned off") || cfg.backupKick.Load() {
		t.Errorf("off: %+v, kicked %v", o, cfg.backupKick.Load())
	}
}

// The scheduler says how each run ended, for `briard backup`: a kicked run outside the window
// starts, and its report is the ending -- what it added, or nothing to back up.
func TestTheSchedulerSaysHowAKickedRunEnded(t *testing.T) {
	cfg, g := liveBackup(t, filepath.Join(t.TempDir(), "Briard Backup"))
	cfg.backupKick, cfg.backupEnded = &atomic.Bool{}, &atomic.Pointer[dashboard.BackupEnd]{}
	for _, tc := range []struct {
		rep    *guestagent.BackupReport
		failed bool
		want   string
	}{
		{&guestagent.BackupReport{Snapshot: "40796b608ceb", Services: []string{"home-assistant"}, Files: 39, BytesAdded: 427712}, false, "backed up home-assistant: 39 files, 418 kB added (snapshot 40796b60)"},
		{&guestagent.BackupReport{}, false, "nothing to back up"},
		{failedRun, true, "unable to save snapshot"},
	} {
		asked := time.Now()
		cfg.backupKick.Store(true)
		bs := utcScheduler()
		// A tick that began well before the ask: the run it takes up is stamped when taken up.
		cfg.backup(context.Background(), fakeStatus{backup: g}, bs, true, tonightAt("14:00").Add(-48*time.Hour), nil, quiet)
		g.running, g.report = false, tc.rep
		cfg.backup(context.Background(), fakeStatus{backup: g}, bs, true, tonightAt("14:01").Add(-48*time.Hour), nil, quiet)
		e := cfg.backupEnded.Load()
		if e == nil || e.Started.Before(asked) || e.Failed != tc.failed || !strings.Contains(e.Detail, tc.want) {
			t.Errorf("ended %+v; want started after the ask, failed %v, %q", e, tc.failed, tc.want)
		}
	}
}

// folderGuest is the guest a folder move reads through: the list it answers, and the candidate
// root the store was serving while it read.
type folderGuest struct {
	fakeStatus
	*fakeLister
	cfg    Config
	during string
}

func (g *folderGuest) DataBackupList(ctx context.Context, url, group string) ([]dashboard.BackupListing, error) {
	g.during = g.cfg.backupCandidateDir()
	return g.fakeLister.DataBackupList(ctx, url, group)
}

// fakeRepo makes dir look like a restic repository, as isResticRepository reads one.
func fakeRepo(t *testing.T, dir string) {
	t.Helper()
	for _, p := range []string{"config", "keys/k1"} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, p)), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, p), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func askFolder(cfg Config, g any, req api.BackupFolder) (api.DirectiveOutcome, dashboard.BackupFolderCheck) {
	raw, _ := json.Marshal(req)
	o := cfg.applyBackupFolder(context.Background(), api.Directive{ID: "d", Kind: api.DirectiveBackupFolder, Payload: string(raw)}, g, quiet)
	var c dashboard.BackupFolderCheck
	if o.State == api.OutcomeDone && !req.Confirm {
		_ = json.Unmarshal([]byte(o.Detail), &c)
	}
	return o, c
}

// A FOLDER IS READ BEFORE THE BACKUP MOVES: empty says so with no read; one holding backups is
// read with this household's key through the candidate root -- set for the read, cleared after --
// and counted, this household's and an earlier install's; one another key made says so; anything
// checkBackupFolder refuses is refused before the guest is asked. Nothing moves without Confirm.
func TestAFolderIsReadBeforeTheBackupMoves(t *testing.T) {
	old := filepath.Join(t.TempDir(), "Briard Backup")
	fakeRepo(t, old)
	cfg, _ := liveBackup(t, old)
	cfg.backupCandidate = &atomic.Pointer[string]{}
	at := time.Date(2026, 10, 30, 2, 0, 0, 0, time.UTC)
	g := &folderGuest{fakeStatus: fakeStatus{backup: &fakeBackup{}}, fakeLister: &fakeLister{}, cfg: cfg}

	empty := t.TempDir()
	o, c := askFolder(cfg, g, api.BackupFolder{Folder: empty})
	if o.State != api.OutcomeDone || !c.Empty || c.Previous != old || g.url != "" {
		t.Fatalf("an empty folder: %+v %+v, asked %q; want empty, the old folder named, no read", o, c, g.url)
	}

	moved := filepath.Join(t.TempDir(), "OneDrive")
	fakeRepo(t, moved)
	g.list = []dashboard.BackupListing{{ID: "c", At: at, Own: true}, {ID: "b", At: at.Add(-24 * time.Hour), Own: true},
		{ID: "a", At: at.Add(-7 * 24 * time.Hour), Own: true}, {ID: "e1", At: at.Add(-30 * 24 * time.Hour)}, {ID: "e2", At: at.Add(-31 * 24 * time.Hour)}}
	o, c = askFolder(cfg, g, api.BackupFolder{Folder: moved})
	if o.State != api.OutcomeDone || c.Empty || c.OtherKey || c.Own != 3 || c.Earlier != 2 || !c.Newest.Equal(at) {
		t.Fatalf("a folder of backups: %+v %+v; want 3 of this household's, newest %s, and 2 earlier", o, c, at)
	}
	if g.during != moved || !strings.HasSuffix(g.url, ":"+backupStorePort+"/"+backupCandidateRoot+"/") || g.group != "f1d" {
		t.Errorf("read %q (%q) while the candidate was %q; want the candidate root on %s, this household's group", g.url, g.group, g.during, moved)
	}
	if cfg.backupCandidateDir() != "" {
		t.Error("the candidate root was left set after the read")
	}
	if cfg.backupFolder() != old {
		t.Errorf("a check moved the backup to %s", cfg.backupFolder())
	}

	g.list, g.fakeLister.err = nil, errors.New("restic --repo rest:http://h/candidate/ snapshots: exit status 12: Fatal: "+guestagent.ResticWrongKey)
	if o, c = askFolder(cfg, g, api.BackupFolder{Folder: moved}); o.State != api.OutcomeDone || !c.OtherKey {
		t.Errorf("another key's: %+v %+v", o, c)
	}
	g.fakeLister.err = errors.New("connection refused")
	if o, _ = askFolder(cfg, g, api.BackupFolder{Folder: moved}); o.State != api.OutcomeFailed || !strings.Contains(o.Detail, "could not be read") {
		t.Errorf("an unreadable folder: %+v", o)
	}
	if cfg.backupCandidateDir() != "" {
		t.Error("a failed read left the candidate root set")
	}
	g.fakeLister.err = nil

	kube := t.TempDir()
	if err := os.WriteFile(filepath.Join(kube, "config"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	g.url = ""
	for _, dir := range []string{kube, filepath.Join(empty, "not-mounted"), old} {
		if o, _ := askFolder(cfg, g, api.BackupFolder{Folder: dir}); o.State != api.OutcomeFailed || g.url != "" {
			t.Errorf("%s: %+v, asked %q; want refused unread", dir, o, g.url)
		}
	}
	busy := "/elsewhere"
	cfg.backupCandidate.Store(&busy)
	if o, _ := askFolder(cfg, g, api.BackupFolder{Folder: moved}); o.State != api.OutcomeFailed || g.url != "" || cfg.backupCandidateDir() != busy {
		t.Errorf("a second read while one goes: %+v; want refused, the first one's root kept", o)
	}
}

// THE CONFIRMED MOVE: into an empty folder or one of this household's, the folder changes and the
// old one is remembered; into another key's, only with that key, adopted in the same act -- and a
// key that does not open it leaves the backup where it was, the record as it was.
func TestTheConfirmedMoveAdoptsOrStaysWhereItWas(t *testing.T) {
	old := filepath.Join(t.TempDir(), "Briard Backup")
	fakeRepo(t, old)
	cfg, fb := liveBackup(t, old)
	cfg.backupCandidate, cfg.backupKick = &atomic.Pointer[string]{}, &atomic.Bool{}
	g := &folderGuest{fakeStatus: fakeStatus{backup: fb}, fakeLister: &fakeLister{}, cfg: cfg}

	other := filepath.Join(t.TempDir(), "Their Backup")
	fakeRepo(t, other)
	g.fakeLister.err = errors.New("Fatal: " + guestagent.ResticWrongKey)
	if o, _ := askFolder(cfg, g, api.BackupFolder{Folder: other, Confirm: true}); o.State != api.OutcomeFailed || !strings.Contains(o.Detail, "enter that key") || cfg.backupFolder() != old {
		t.Fatalf("another key's with no key: %+v, folder %s", o, cfg.backupFolder())
	}
	fb.adoptErr = errors.New("that key does not open the backup in this folder")
	o, _ := askFolder(cfg, g, api.BackupFolder{Folder: other, Key: " wrong ", Confirm: true})
	if o.State != api.OutcomeFailed || !strings.Contains(o.Detail, "was not changed") || cfg.backupFolder() != old || fb.adopted != "wrong" {
		t.Fatalf("a key that does not open it: %+v, folder %s, adopted with %q", o, cfg.backupFolder(), fb.adopted)
	}
	if rec := cfg.readBackupRecord(); rec.Previous != "" {
		t.Errorf("the failed move left the record %+v", rec)
	}
	if b, _ := os.ReadFile(os.Getenv("BRIARD_CONFIG")); !strings.Contains(string(b), "BACKUP_DIR="+old) {
		t.Errorf("config.env after the failed move:\n%s", b)
	}
	fb.adoptErr = nil
	if o, _ := askFolder(cfg, g, api.BackupFolder{Folder: other, Key: "theirs", Confirm: true}); o.State != api.OutcomeDone || cfg.backupFolder() != other || fb.adopted != "theirs" {
		t.Fatalf("with its key: %+v, folder %s, adopted with %q", o, cfg.backupFolder(), fb.adopted)
	}
	if rec := cfg.readBackupRecord(); rec.Folder != other || rec.Previous != old {
		t.Errorf("record %+v; want the new folder, the old one previous", rec)
	}

	g.fakeLister.err, fb.adopted = nil, ""
	empty := t.TempDir()
	if o, _ := askFolder(cfg, g, api.BackupFolder{Folder: empty, Confirm: true}); o.State != api.OutcomeDone || cfg.backupFolder() != empty || fb.adopted != "" {
		t.Fatalf("an empty folder: %+v, folder %s, adopted %q", o, cfg.backupFolder(), fb.adopted)
	}
	if entries, _ := os.ReadDir(empty); len(entries) != 0 {
		t.Errorf("the move wrote %v into the folder", entries)
	}
}

// The page asks for a run and does not wait for it: the scheduler is kicked, the answer is at once.
func TestThePagesBackupNowDoesNotWait(t *testing.T) {
	cfg, _ := liveBackup(t, filepath.Join(t.TempDir(), "Briard Backup"))
	cfg.backupKick, cfg.backupEnded = &atomic.Bool{}, &atomic.Pointer[dashboard.BackupEnd]{}
	done := make(chan api.DirectiveOutcome, 1)
	go func() {
		done <- cfg.applyBackupNow(context.Background(), api.Directive{ID: "d", Kind: api.DirectiveBackupNow, Payload: api.BackupNowStart})
	}()
	select {
	case o := <-done:
		if o.State != api.OutcomeDone || !cfg.backupKick.Load() {
			t.Errorf("%+v, kicked %v", o, cfg.backupKick.Load())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the page's ask waited for the run")
	}
}

// The page is told the run going and how the newest one ended, so it can follow one it asked for.
func TestThePageIsToldTheRunGoingAndHowItEnded(t *testing.T) {
	cfg, g := liveBackup(t, "/f")
	cfg.backupEnded = &atomic.Pointer[dashboard.BackupEnd]{}
	vg := &viewGuest{fakeStatus: fakeStatus{backup: g}}
	pushed := ""
	b := utcScheduler()
	b.waiting = time.Date(2026, 10, 30, 14, 5, 0, 0, time.UTC)
	cfg.pushBackupView(context.Background(), vg, b, &pushed, quiet)
	b.waiting = time.Time{}
	cfg.ended(time.Date(2026, 10, 30, 14, 5, 0, 0, time.UTC), false, "backed up home-assistant: 36 files, 301 kB added (snapshot 5aa539af)")
	cfg.pushBackupView(context.Background(), vg, b, &pushed, quiet)
	if len(vg.views) != 2 || vg.views[0].Running.IsZero() || !vg.views[1].Running.IsZero() || vg.views[1].Ended == nil || !strings.Contains(vg.views[1].Ended.Detail, "301 kB") {
		t.Fatalf("pushed %+v", vg.views)
	}
}

// followRig is a node whose backup folder was in its installing account's Documents, and a
// Documents that moved: the old folder gone, the new Documents read as docs says.
func followRig(t *testing.T) (Config, *folderGuest, *backupScheduler, string, string, *fakeNotifier) {
	t.Helper()
	home := t.TempDir()
	old := filepath.Join(home, "Documents", "Briard Backup") // never created: OneDrive moved it
	cfg, fb := liveBackup(t, old)
	cfg.InstallUser = "S-1-5-21-1-2-3-1001"
	cfg.backupCandidate, cfg.backupKick = &atomic.Pointer[string]{}, &atomic.Bool{}
	cfg.acts = newActLane()
	docs := filepath.Join(home, "OneDrive", "Documents")
	g := &folderGuest{fakeStatus: fakeStatus{backup: fb}, fakeLister: &fakeLister{}, cfg: cfg}
	b := utcScheduler()
	b.documents = func(sid string) (string, error) {
		if sid != cfg.InstallUser {
			t.Errorf("read the Documents of %q, want the installing account's", sid)
		}
		return docs, nil
	}
	return cfg, g, b, old, filepath.Join(docs, "Briard Backup"), &fakeNotifier{}
}

// follow runs one tick's check and waits for the act it may have started.
func follow(t *testing.T, cfg Config, g *folderGuest, b *backupScheduler, n *fakeNotifier) {
	t.Helper()
	cfg.followDocuments(context.Background(), g, b, n, quiet)
	for i := 0; i < 500 && cfg.acts.kind() != ""; i++ {
		time.Sleep(2 * time.Millisecond)
	}
}

// THE FOLDER FOLLOWS DOCUMENTS: gone from where the install put it, and this household's backups
// in the account's Documents now -- the move is followed through the backup-folder act, read with
// the household's key, with an event saying so and no "earlier backups" in a folder that is gone.
func TestTheBackupFolderFollowsAMovedDocuments(t *testing.T) {
	cfg, g, b, old, to, n := followRig(t)
	fakeRepo(t, to)
	g.list = []dashboard.BackupListing{{ID: "a", At: time.Date(2026, 10, 30, 2, 0, 0, 0, time.UTC), Own: true}}
	follow(t, cfg, g, b, n)
	if cfg.backupFolder() != to {
		t.Fatalf("folder %q, want it followed to %q", cfg.backupFolder(), to)
	}
	if b, _ := os.ReadFile(os.Getenv("BRIARD_CONFIG")); !strings.Contains(string(b), "BACKUP_DIR="+to) {
		t.Errorf("config.env:\n%s", b)
	}
	if rec := cfg.readBackupRecord(); rec.Folder != to || rec.Previous != "" {
		t.Errorf("record %+v; want the new folder and no previous one -- %s is gone, its backups moved", rec, old)
	}
	if len(n.alerts) != 1 || n.alerts[0].Kind != notify.Event || !strings.Contains(n.alerts[0].Body, to) {
		t.Errorf("alerts %+v; want one event naming where the folder is now", n.alerts)
	}
	if g.during != to {
		t.Errorf("read %q through the candidate root; want the new folder, read before the move", g.during)
	}
}

// NOTHING ELSE IS FOLLOWED: a folder that is there (the household's choice), no folder of that name
// in Documents, one that is not a repository, another key's, an earlier install's alone. And a
// disappearance is tried once -- "not yet" (the account's Documents unreadable, the act lane busy)
// is not a try, and the folder coming back starts over.
func TestTheBackupFolderFollowsNothingElse(t *testing.T) {
	t.Run("there", func(t *testing.T) {
		cfg, g, b, old, _, n := followRig(t)
		must(t, os.MkdirAll(old, 0o700))
		b.documents = func(string) (string, error) { t.Error("asked Documents about a folder that is there"); return "", nil }
		follow(t, cfg, g, b, n)
	})
	for name, set := range map[string]func(t *testing.T, g *folderGuest, to string){
		"no folder there":  func(*testing.T, *folderGuest, string) {},
		"not a repository": func(t *testing.T, _ *folderGuest, to string) { must(t, os.MkdirAll(to, 0o700)) },
		"another key's": func(t *testing.T, g *folderGuest, to string) {
			fakeRepo(t, to)
			g.fakeLister.err = errors.New("Fatal: " + guestagent.ResticWrongKey)
		},
		"an earlier install": func(t *testing.T, g *folderGuest, to string) {
			fakeRepo(t, to)
			g.list = []dashboard.BackupListing{{ID: "e", Own: false}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			cfg, g, b, old, to, n := followRig(t)
			set(t, g, to)
			follow(t, cfg, g, b, n)
			reads := g.url
			follow(t, cfg, g, b, n) // the next tick: tried once already
			if cfg.backupFolder() != old || len(n.alerts) != 0 || b.followed != old {
				t.Errorf("folder %q, alerts %v, followed %q; want nothing moved, the disappearance marked tried", cfg.backupFolder(), n.alerts, b.followed)
			}
			if g.url != reads {
				t.Error("the folder was read again on the next tick")
			}
		})
	}
	t.Run("not yet, then once", func(t *testing.T) {
		cfg, g, b, old, to, n := followRig(t)
		fakeRepo(t, to)
		g.list = []dashboard.BackupListing{{ID: "a", Own: true}}
		docs := b.documents
		b.documents = func(string) (string, error) { return "", errors.New("the account's settings are not loaded") }
		follow(t, cfg, g, b, n)
		if b.followed != "" || cfg.backupFolder() != old {
			t.Fatal("an unreadable Documents counted as a try")
		}
		b.documents = docs
		held, _ := cfg.acts.claim(api.DirectiveServiceInstall)
		_ = held
		cfg.followDocuments(context.Background(), g, b, n, quiet)
		cfg.acts.release()
		if b.followed != "" || cfg.backupFolder() != old {
			t.Fatal("a busy act lane counted as a try")
		}
		follow(t, cfg, g, b, n)
		if cfg.backupFolder() != to {
			t.Fatalf("folder %q; want it followed once nothing stood in the way", cfg.backupFolder())
		}
	})
	t.Run("back again", func(t *testing.T) {
		cfg, g, b, old, _, n := followRig(t)
		follow(t, cfg, g, b, n)
		must(t, os.MkdirAll(old, 0o700))
		follow(t, cfg, g, b, n)
		if b.followed != "" {
			t.Error("the folder came back and the try was not forgotten")
		}
	})
}

// The shell's own form of a path in an account's hive, %USERPROFILE% in any case.
func TestExpandUserProfile(t *testing.T) {
	for raw, want := range map[string]string{
		`%USERPROFILE%\Documents`:           `C:\Users\kosta\Documents`,
		`%userprofile%\OneDrive\Documents`:  `C:\Users\kosta\OneDrive\Documents`,
		`C:\Users\kosta\OneDrive\Documents`: `C:\Users\kosta\OneDrive\Documents`,
		`D:\Docs`:                           `D:\Docs`,
	} {
		if got := expandUserProfile(raw, `C:\Users\kosta`); got != want {
			t.Errorf("%q -> %q, want %q", raw, got, want)
		}
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
