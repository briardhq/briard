package guestagent

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"briard.io/agent/quadlet"
)

// backupRig is a mounted volume holding two services, each with a ring whose newest member was
// taken by a different trigger, a VIP, manifests and the household's key but no certificate yet,
// and a restic that answers from the table it is given (keyed by subcommand).
func backupRig(restic map[string]func() ([]byte, error)) (*fakeExec, map[string]string) {
	now := time.Now()
	name := func(svc string, tr quadlet.Trigger, ago time.Duration) string {
		return strings.TrimPrefix(quadlet.SnapshotMember(svc, tr, now.Add(-ago)), quadlet.SnapshotsDir)
	}
	members := []string{
		name("home-assistant", quadlet.TriggerClock, 2*time.Hour),
		name("home-assistant", quadlet.TriggerStart, time.Hour), // the newest, and not a clock sample
		name("home-assistant", quadlet.TriggerClock, 3*time.Hour),
		name("mosquitto", quadlet.TriggerClock, 5*time.Hour),
	}
	newest := map[string]string{
		"home-assistant": quadlet.SnapshotsDir + members[1],
		"mosquitto":      quadlet.SnapshotsDir + members[3],
	}
	f := ringExec(members...)
	if f.files == nil {
		f.files = map[string]string{}
	}
	f.files[backupKeyFile] = "s3cret\n"
	inner := f.runFn
	f.runFn = func(n string, args []string) ([]byte, error) {
		if n == "ls" && len(args) > 1 && args[1] == manifestDir {
			return []byte("home-assistant.json\nmosquitto.json\n"), nil
		}
		if n == "stat" && len(args) == 3 && args[2] == dataMountRoot {
			return []byte(dataMountRoot + "\n"), nil // mounted here
		}
		if n == "touch" && len(args) == 1 {
			return nil, f.WriteFile(args[0], nil)
		}
		if n == "test" && len(args) == 2 && args[0] == "-e" {
			if _, ok := f.files[args[1]]; args[1] == tlsDir || (strings.HasPrefix(args[1], dataMountRoot+"/.backup-key") && !ok) {
				return nil, errors.New("exit status 1")
			}
			return nil, nil
		}
		if n == "restic" {
			for _, a := range args {
				if fn, ok := restic[a]; ok {
					return fn()
				}
			}
			return nil, nil
		}
		return inner(n, args)
	}
	return f, newest
}

const resticSummary = `{"message_type":"summary","files_new":3,"total_files_processed":42,"data_added":1234,"snapshot_id":"abc123"}`

func resticRuns(f *fakeExec, sub string) [][]string {
	var out [][]string
	for _, r := range f.runs {
		if r[0] == "restic" && slices.Contains(r, sub) {
			out = append(out, r)
		}
	}
	return out
}

// TestTheBackupVerbsAreAdvertisedBesideActs: a verb absent from the handshake is one no host
// calls, and a read that minutes long must not hold the act lane.
func TestTheBackupVerbsAreAdvertisedBesideActs(t *testing.T) {
	for _, v := range []string{verbDataBackup, verbDataBackupResult, verbDataBackupKey} {
		for _, act := range []string{verbDataBackupRestore, verbDataBackupAdopt} {
			if !slices.Contains(guestCapabilities, act) || !isAct(act) {
				t.Errorf("%s must be advertised and must be an act: it writes", act)
			}
		}
		if !slices.Contains(guestCapabilities, v) {
			t.Errorf("the guest does not advertise %s", v)
		}
		if isAct(v) {
			t.Errorf("%s runs in the act lane", v)
		}
	}
}

// TestTheBackupRunsInTheBackground: the verb answers at once over the channel, a second start is
// refused while one runs, and the report is handed over once.
func TestTheBackupRunsInTheBackground(t *testing.T) {
	ctx := context.Background()
	release := make(chan struct{})
	f, _ := backupRig(map[string]func() ([]byte, error){
		"backup": func() ([]byte, error) { <-release; return []byte(resticSummary), nil },
	})
	g := dial(t, f)
	if ok, err := g.DataBackup(ctx, "http://10.0.0.129:7791/", "f1d", nil); err != nil || !ok {
		t.Fatalf("the backup did not start: %v %v", ok, err)
	}
	if ok, err := g.DataBackup(ctx, "http://10.0.0.129:7791/", "f1d", nil); err != nil || ok {
		t.Fatalf("a second backup started beside the first: %v %v", ok, err)
	}
	if s, err := g.DataBackupResult(ctx); err != nil || !s.Running || s.Report != nil {
		t.Fatalf("state %+v %v, want running with no report", s, err)
	}
	close(release)
	deadline := time.Now().Add(5 * time.Second)
	var s BackupState
	for s, _ = g.DataBackupResult(ctx); s.Report == nil && time.Now().Before(deadline); s, _ = g.DataBackupResult(ctx) {
		time.Sleep(10 * time.Millisecond)
	}
	if s.Running || s.Report == nil {
		t.Fatalf("state %+v, want a finished report", s)
	}
	want := BackupReport{Snapshot: "abc123", Services: []string{"home-assistant", "mosquitto"}, Files: 42, BytesAdded: 1234}
	if !slices.Equal(s.Report.Services, want.Services) || s.Report.Snapshot != want.Snapshot ||
		s.Report.Files != want.Files || s.Report.BytesAdded != want.BytesAdded || s.Report.Error != "" {
		t.Fatalf("report %+v, want %+v", *s.Report, want)
	}
	if again, _ := g.DataBackupResult(ctx); again.Report != nil {
		t.Fatal("the report was handed over twice")
	}
}

// TestTheBackupRefusesWhatIsNotARESTRepository: the backend is the guest's choice, never the
// request's.
func TestTheBackupRefusesWhatIsNotARESTRepository(t *testing.T) {
	for _, req := range []backupRequest{
		{Repository: "/var/lib/briard"},
		{Repository: "sftp:host:/x"},
	} {
		if ok, err := startBackup(&fakeExec{}, req); ok || err == nil {
			t.Errorf("%+v: started=%v err=%v, want a refusal", req, ok, err)
		}
	}
}

// TestTheBackupCommandLines: the newest member of every service (whatever took it) at a stable
// path, read-only; the volume's key in a 0600 file, never an argument; HA's own tarballs
// excluded; the household's group on the backup and on the forget; the retention policy;
// everything unmounted and removed after.
func TestTheBackupCommandLines(t *testing.T) {
	f, newest := backupRig(map[string]func() ([]byte, error){
		"backup": func() ([]byte, error) { return []byte(resticSummary), nil },
	})
	rep := runBackup(context.Background(), f, backupRequest{Repository: "http://10.0.0.129:7791/", Group: "f1d"})
	if rep.Error != "" || rep.Snapshot != "abc123" {
		t.Fatalf("report %+v", rep)
	}

	backup := step(f, func(r []string) bool { return r[0] == "restic" && slices.Contains(r, "backup") })
	for svc, member := range newest {
		p := backupServices + "/" + svc
		at := runIndex(f, 0, "mount", "-o", "bind,ro", member, p)
		if at < 0 || at > backup {
			t.Errorf("%s: %s not mounted at %s before the backup: %v", svc, member, p, f.runs)
		}
		if runIndex(f, backup, "umount", p) < 0 {
			t.Errorf("%s: never unmounted after the backup", svc)
		}
	}
	if step(f, func(r []string) bool {
		return slices.Equal(r, []string{"install", "-D", "-m", "0600", "/dev/null", backupPasswordFile})
	}) < 0 {
		t.Error("the password file was not created 0600 before it was written")
	}
	if f.files[backupPasswordFile] != "s3cret" {
		t.Errorf("password file holds %q", f.files[backupPasswordFile])
	}
	if lastStep(f, func(r []string) bool { return slices.Equal(r, []string{"rm", "-f", backupPasswordFile}) }) < 0 {
		t.Error("the password file was left behind")
	}
	for _, r := range f.runs {
		if slices.Contains(r, "s3cret") {
			t.Errorf("the password is on a command line: %v", r)
		}
	}

	b := resticRuns(f, "backup")
	if len(b) != 1 {
		t.Fatalf("restic backup ran %d times", len(b))
	}
	args := strings.Join(b[0], " ")
	for _, want := range []string{
		"--repo rest:http://10.0.0.129:7791/", "--password-file " + backupPasswordFile,
		"--exclude **/app/backups", "--host f1d",
		"--exclude **/app/backups " + backupRoot,
	} {
		if !strings.Contains(args, want) {
			t.Errorf("restic backup %q lacks %q", args, want)
		}
	}
	if strings.Contains(args, quadlet.SnapshotsDir) {
		t.Errorf("restic backup reads a stamped member path: %q", args)
	}
	fg := resticRuns(f, "forget")
	if len(fg) != 1 || !strings.Contains(strings.Join(fg[0], " "), "--host f1d --keep-daily 2 --keep-weekly 1 --prune") {
		t.Errorf("forget %v, want the 2-daily 1-weekly policy with prune, on this household's group only", fg)
	}
	if unlock := step(f, func(r []string) bool { return r[0] == "restic" && slices.Contains(r, "unlock") }); unlock < 0 || unlock > backup {
		t.Error("a stale lock is not cleared before the backup")
	}
	if len(resticRuns(f, "init")) != 0 {
		t.Error("an existing repository was initialised")
	}
}

// TestTheBackupCreatesTheRepositoryOnlyWhenItCannotOpenOne, and a failed init is the run's error:
// init refuses an existing repository, so nothing is replaced whatever made the open fail.
func TestTheBackupCreatesTheRepositoryOnlyWhenItCannotOpenOne(t *testing.T) {
	f, _ := backupRig(map[string]func() ([]byte, error){
		"cat": func() ([]byte, error) {
			return []byte("Is there a repository at the following location?"), errors.New("exit status 10")
		},
		"backup": func() ([]byte, error) { return []byte(resticSummary), nil },
	})
	if rep := runBackup(context.Background(), f, backupRequest{Repository: "http://h/"}); rep.Error != "" {
		t.Fatalf("report %+v", rep)
	}
	if len(resticRuns(f, "init")) != 1 {
		t.Fatal("a missing repository was not created")
	}

	f, _ = backupRig(map[string]func() ([]byte, error){
		"cat":  func() ([]byte, error) { return nil, errors.New("wrong password") },
		"init": func() ([]byte, error) { return nil, errors.New("config file already exists") },
	})
	rep := runBackup(context.Background(), f, backupRequest{Repository: "http://h/"})
	if rep.Error == "" || len(resticRuns(f, "backup")) != 0 {
		t.Fatalf("report %+v after a refused init; backup must not run", rep)
	}
	initAt := step(f, func(r []string) bool { return r[0] == "restic" && slices.Contains(r, "init") })
	for _, svc := range []string{"home-assistant", "mosquitto"} {
		if runIndex(f, initAt, "umount", backupServices+"/"+svc) < 0 {
			t.Errorf("a failed run left %s mounted", svc)
		}
	}
}

// TestAFailedBackupIsTheReportNotASilence: restic's error reaches the report, and a run that
// saved nothing does not prune.
func TestAFailedBackupIsTheReportNotASilence(t *testing.T) {
	f, _ := backupRig(map[string]func() ([]byte, error){
		"backup": func() ([]byte, error) { return []byte("Fatal: unable to save snapshot"), errors.New("exit status 1") },
	})
	rep := runBackup(context.Background(), f, backupRequest{Repository: "http://h/"})
	if rep.Error == "" || rep.Snapshot != "" {
		t.Fatalf("report %+v, want an error and no snapshot", rep)
	}
	if len(resticRuns(f, "forget")) != 0 {
		t.Error("a run that saved nothing pruned the repository")
	}
}

// TestANodeWithNothingInstalledBacksUpNothing: no services is the shipped state, not a failure.
func TestANodeWithNothingInstalledBacksUpNothing(t *testing.T) {
	f := ringExec()
	f.runFn = func(n string, args []string) ([]byte, error) { return nil, errors.New("no such directory") }
	rep := runBackup(context.Background(), f, backupRequest{Repository: "http://h/"})
	if rep.Error != "" || rep.Snapshot != "" || len(f.runs) != 1 {
		t.Fatalf("report %+v after %v, want nothing done", rep, f.runs)
	}
}

// runIndex is the index of the first run equal to want at or after from, or -1.
func runIndex(f *fakeExec, from int, want ...string) int {
	for i := max(from, 0); i < len(f.runs); i++ {
		if slices.Equal(f.runs[i], want) {
			return i
		}
	}
	return -1
}

// TestTheBackupCarriesWhatARestoreNeeds: beside each service's data, the manifest it was written
// under; the volume's flock-scoped facts that exist; the host's facts as 0600 files whose content
// never reaches a command line -- all in place before restic reads, and the copies gone after.
func TestTheBackupCarriesWhatARestoreNeeds(t *testing.T) {
	f, newest := backupRig(map[string]func() ([]byte, error){
		"backup": func() ([]byte, error) { return []byte(resticSummary), nil },
	})
	host := map[string][]byte{"flock-id": []byte("f1d"), "casa.key": []byte("k3y")}
	if rep := runBackup(context.Background(), f, backupRequest{Repository: "http://h/", Host: host}); rep.Error != "" {
		t.Fatalf("report %+v", rep)
	}
	backup := step(f, func(r []string) bool { return r[0] == "restic" && slices.Contains(r, "backup") })
	before := func(what string, want ...string) {
		t.Helper()
		if at := runIndex(f, 0, want...); at < 0 || at > backup {
			t.Errorf("%s: no %v before the backup", what, want)
		}
	}
	for svc, member := range newest {
		before(svc+"'s manifest", "cp", "-a", quadlet.SnapshotSidecar(member), backupFacts+"/members/"+svc+".json")
	}
	before("the VIP", "cp", "-a", dataMountRoot+"/.vip-address", backupFacts+"/volume/")
	before("the installed services", "cp", "-a", manifestDir, backupFacts+"/volume/")
	if runIndex(f, 0, "cp", "-a", tlsDir, backupFacts+"/volume/") >= 0 {
		t.Error("copied a certificate directory the volume does not have")
	}
	for name, data := range host {
		p := backupFacts + "/host/" + name
		before(name+" created 0600", "install", "-D", "-m", "0600", "/dev/null", p)
		if f.files[p] != string(data) {
			t.Errorf("%s holds %q", p, f.files[p])
		}
		for _, r := range f.runs {
			if slices.Contains(r, string(data)) {
				t.Errorf("host fact %s is on a command line: %v", name, r)
			}
		}
	}
	if runIndex(f, backup, "rm", "-rf", backupFacts) < 0 {
		t.Error("the copies (the host's secrets among them) were left behind")
	}
	if b := resticRuns(f, "backup"); len(b) != 1 || b[0][len(b[0])-1] != backupRoot || slices.Contains(b[0], backupServices) {
		t.Errorf("restic backup %v, want the one root and nothing else", b)
	}
}

// TestTheBackupRefusesAHostFactThatIsAPath: a host fact is a name in one directory, never a path.
func TestTheBackupRefusesAHostFactThatIsAPath(t *testing.T) {
	for _, name := range []string{"", ".", "..", "../x", "a/b", `a\b`} {
		req := backupRequest{Repository: "http://h/", Host: map[string][]byte{name: nil}}
		if ok, err := startBackup(&fakeExec{}, req); ok || err == nil {
			t.Errorf("%q: started=%v err=%v, want a refusal", name, ok, err)
		}
	}
}

// TestTheKeyIsTheVolumes: minted once, 0600 and synced, only where the volume is mounted; the same
// key on every later ask; never over a key that is there but cannot be read (a new one would
// orphan every snapshot the old one opens); the household's word that it is saved recorded beside
// it; and a run with no key is a failed run, not a repository under a key the volume lacks.
func TestTheKeyIsTheVolumes(t *testing.T) {
	ctx := context.Background()
	f, _ := backupRig(nil)
	delete(f.files, backupKeyFile)
	k, err := backupKeyState(ctx, f, backupKeyRequest{})
	if err != nil || k.Saved {
		t.Fatalf("first ask %+v %v", k, err)
	}
	if !regexp.MustCompile(`^([a-z2-7]{4}-){7}[a-z2-7]{4}$`).MatchString(k.Key) {
		t.Errorf("key %q is not eight groups of base32", k.Key)
	}
	if f.files[backupKeyFile] != k.Key+"\n" {
		t.Errorf("the volume holds %q", f.files[backupKeyFile])
	}
	made := step(f, func(r []string) bool {
		return slices.Equal(r, []string{"install", "-m", "0600", "/dev/null", backupKeyFile})
	})
	synced := step(f, func(r []string) bool { return slices.Equal(r, []string{"sync", "-f", dataMountRoot}) })
	if made < 0 || synced < made {
		t.Errorf("the key was not created 0600 and then synced: %v", f.runs)
	}
	again, err := backupKeyState(ctx, f, backupKeyRequest{Saved: true})
	if err != nil || again.Key != k.Key || !again.Saved {
		t.Fatalf("second ask %+v %v, want the same key, saved", again, err)
	}
	if _, ok := f.files[backupKeySavedFile]; !ok {
		t.Error("the household's word was not recorded on the volume")
	}

	// There, but unreadable: no key, and no new one.
	f, _ = backupRig(nil)
	f.files[backupKeyFile] = ""
	if k, err := backupKeyState(ctx, f, backupKeyRequest{}); err == nil {
		t.Fatalf("minted %+v over an unreadable key", k)
	}
	if rep := runBackup(ctx, f, backupRequest{Repository: "http://h/"}); rep.Error == "" || len(resticRuns(f, "init")) != 0 {
		t.Fatalf("report %+v after %d inits; a run without the key must fail before it opens anything", rep, len(resticRuns(f, "init")))
	}

	// Not mounted here (a Secondary): the path is the empty mountpoint, and nothing is written to it.
	f, _ = backupRig(nil)
	delete(f.files, backupKeyFile)
	inner := f.runFn
	f.runFn = func(n string, args []string) ([]byte, error) {
		if n == "stat" {
			return []byte("/\n"), nil
		}
		return inner(n, args)
	}
	if k, err := backupKeyState(ctx, f, backupKeyRequest{}); err == nil {
		t.Fatalf("minted %+v where the volume is not mounted", k)
	}
	if _, ok := f.files[backupKeyFile]; ok {
		t.Error("a key was written onto the empty mountpoint")
	}
}

// TestASnapshotWithNoGroupIsTheGuestsOwn: a request that names no group (a rig with no host) is
// taken under the one fixed name, so plain restic finds it where the README says.
func TestASnapshotWithNoGroupIsTheGuestsOwn(t *testing.T) {
	f, _ := backupRig(map[string]func() ([]byte, error){
		"backup": func() ([]byte, error) { return []byte(resticSummary), nil },
	})
	if rep := runBackup(context.Background(), f, backupRequest{Repository: "http://h/"}); rep.Error != "" {
		t.Fatalf("report %+v", rep)
	}
	for _, sub := range []string{"backup", "forget"} {
		if r := resticRuns(f, sub); len(r) != 1 || !strings.Contains(strings.Join(r[0], " "), "--host "+backupHost) {
			t.Errorf("%s %v, want --host %s", sub, r, backupHost)
		}
	}
}

// TestANightIsMaterialisedAsAMember: the sidecar is read from the backup first and names the
// manifest; the member is created empty, restored into from the service's subtree of that night,
// made read-only, labelled from-backup with the night's own TakenAt, and synced -- with the key
// in a file, never on a command line; and a restore that fails leaves nothing in the ring.
func TestANightIsMaterialisedAsAMember(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	taken := now.Add(-3 * 24 * time.Hour)
	side, _ := json.Marshal(quadlet.SnapshotMeta{Service: "home-assistant", Trigger: quadlet.TriggerStart, TakenAt: taken,
		Consistency: quadlet.Quiesced, Manifest: `{"name":"home-assistant"}`, Pending: true})
	var restoreErr error
	f, _ := backupRig(map[string]func() ([]byte, error){
		"dump":    func() ([]byte, error) { return side, nil },
		"restore": func() ([]byte, error) { return nil, restoreErr },
	})
	inner := f.runFn
	f.runFn = func(n string, args []string) ([]byte, error) {
		if n == "btrfs" && args[1] == "show" {
			return nil, errors.New("not found") // no collision
		}
		return inner(n, args)
	}
	member, err := restoreBackupMember(ctx, f, backupRestoreRequest{Repository: "http://10.0.0.129:7791/", Snapshot: "abc12345", Service: "home-assistant"}, now)
	if err != nil {
		t.Fatal(err)
	}
	want := quadlet.SnapshotMember("home-assistant", quadlet.TriggerFromBackup, now)
	if member != want {
		t.Fatalf("member %q, want %q", member, want)
	}
	dump := step(f, func(r []string) bool { return r[0] == "restic" && slices.Contains(r, "dump") })
	create := runIndex(f, 0, "btrfs", "subvolume", "create", member)
	restore := step(f, func(r []string) bool {
		return r[0] == "restic" && slices.Contains(r, "restore") && slices.Contains(r, "abc12345:"+backupServices+"/home-assistant") && slices.Contains(r, member)
	})
	ro := runIndex(f, 0, "btrfs", "property", "set", "-ts", member, "ro", "true")
	synced := runIndex(f, 0, "sync", "-f", dataMountRoot)
	if dump < 0 || create < dump || restore < create || ro < restore || synced < ro {
		t.Fatalf("order: dump %d, create %d, restore %d, ro %d, sync %d: %v", dump, create, restore, ro, synced, f.runs)
	}
	var meta quadlet.SnapshotMeta
	if err := json.Unmarshal([]byte(f.files[quadlet.SnapshotSidecar(member)]), &meta); err != nil {
		t.Fatalf("sidecar: %v", err)
	}
	if meta.Trigger != quadlet.TriggerFromBackup || meta.Pending || !meta.TakenAt.Equal(taken) || meta.Manifest != `{"name":"home-assistant"}` || meta.Consistency != quadlet.Quiesced {
		t.Errorf("sidecar %+v, want from-backup, not pending, the night's TakenAt and the backed-up manifest", meta)
	}
	for _, r := range f.runs {
		if slices.Contains(r, "s3cret") {
			t.Errorf("the key is on a command line: %v", r)
		}
	}
	if lastStep(f, func(r []string) bool { return slices.Equal(r, []string{"rm", "-f", backupPasswordFile}) }) < 0 {
		t.Error("the password file was left behind")
	}

	// The restore fails: the half-made member goes, and the error says the ring is untouched.
	f, _ = backupRig(map[string]func() ([]byte, error){
		"dump":    func() ([]byte, error) { return side, nil },
		"restore": func() ([]byte, error) { return []byte("path not found"), errors.New("exit status 1") },
	})
	inner = f.runFn
	f.runFn = func(n string, args []string) ([]byte, error) {
		if n == "btrfs" && args[1] == "show" {
			return nil, errors.New("not found")
		}
		return inner(n, args)
	}
	if _, err := restoreBackupMember(ctx, f, backupRestoreRequest{Repository: "http://h/", Snapshot: "latest", Service: "home-assistant"}, now); err == nil || !strings.Contains(err.Error(), "nothing was left") {
		t.Fatalf("err %v, want the failure and the ring untouched", err)
	}
	if runIndex(f, 0, "btrfs", "subvolume", "delete", want) < 0 {
		t.Errorf("the half-made member was not removed: %v", f.runs)
	}
	if _, ok := f.files[quadlet.SnapshotSidecar(want)]; ok {
		t.Error("a sidecar was written for a member that does not exist")
	}
}

// TestANightIsRefusedBeforeAnythingIsRead: a snapshot that is not an id, a service that is a
// path, a sidecar that describes another service -- none of them creates a member.
func TestANightIsRefusedBeforeAnythingIsRead(t *testing.T) {
	ctx := context.Background()
	other, _ := json.Marshal(quadlet.SnapshotMeta{Service: "mosquitto", Manifest: "{}"})
	for name, req := range map[string]backupRestoreRequest{
		"not a REST url":  {Repository: "/x", Snapshot: "latest", Service: "home-assistant"},
		"not a snapshot":  {Repository: "http://h/", Snapshot: "latest; rm -rf /", Service: "home-assistant"},
		"a service path":  {Repository: "http://h/", Snapshot: "latest", Service: "../etc"},
		"another service": {Repository: "http://h/", Snapshot: "latest", Service: "home-assistant"},
		"no such night":   {Repository: "http://h/", Snapshot: "ffffffff", Service: "home-assistant"},
	} {
		f, _ := backupRig(map[string]func() ([]byte, error){
			"dump": func() ([]byte, error) {
				if name == "no such night" {
					return []byte("no matching ID found"), errors.New("exit status 1")
				}
				return other, nil
			},
		})
		if _, err := restoreBackupMember(ctx, f, req, time.Now()); err == nil {
			t.Errorf("%s: accepted", name)
		}
		for _, r := range f.runs {
			if r[0] == "btrfs" && len(r) > 2 && r[2] == "create" {
				t.Errorf("%s: a member was created: %v", name, r)
			}
		}
	}
}

// TestAWrongKeyIsNamedNotReplaced: a repository the volume's key does not open is reported as
// such -- never re-initialised, never a bare error the page cannot act on.
func TestAWrongKeyIsNamedNotReplaced(t *testing.T) {
	f, _ := backupRig(map[string]func() ([]byte, error){
		"cat": func() ([]byte, error) { return []byte("Fatal: " + resticWrongKey), errors.New("exit status 1") },
	})
	rep := runBackup(context.Background(), f, backupRequest{Repository: "http://h/"})
	if !rep.WrongKey || rep.Error == "" || rep.Snapshot != "" {
		t.Fatalf("report %+v, want a wrong-key failure", rep)
	}
	if len(resticRuns(f, "init")) != 0 || len(resticRuns(f, "backup")) != 0 {
		t.Error("a repository under another key was initialised or written")
	}
}

// TestAdoptionAddsTheVolumesKey: the typed key opens the repository for `key add` of the
// volume's key, both only ever in 0600 files, both files gone after; a repository the volume's
// key already opens needs nothing; and a typed key that does not open it is said so.
func TestAdoptionAddsTheVolumesKey(t *testing.T) {
	ctx := context.Background()
	opens := map[string]bool{backupAdoptFile: true} // which password file restic accepts
	rig := func() *fakeExec {
		f, _ := backupRig(nil)
		inner := f.runFn
		f.runFn = func(n string, args []string) ([]byte, error) {
			if n == "restic" {
				pw := args[slices.Index(args, "--password-file")+1]
				if !opens[pw] {
					return []byte("Fatal: " + resticWrongKey), errors.New("exit status 1")
				}
				if slices.Contains(args, "add") {
					opens[args[slices.Index(args, "--new-password-file")+1]] = true
				}
				return nil, nil
			}
			return inner(n, args)
		}
		return f
	}
	f := rig()
	if err := adoptBackup(ctx, f, backupAdoptRequest{Repository: "http://h/", Password: " old-key\n"}); err != nil {
		t.Fatal(err)
	}
	add := step(f, func(r []string) bool {
		return r[0] == "restic" && slices.Contains(r, "add") && slices.Contains(r, "--password-file") && r[slices.Index(r, "--password-file")+1] == backupAdoptFile &&
			slices.Contains(r, "--new-password-file") && r[slices.Index(r, "--new-password-file")+1] == backupPasswordFile
	})
	if add < 0 {
		t.Fatalf("no key add opened by the typed key adding the volume's: %v", f.runs)
	}
	if f.files[backupAdoptFile] != "old-key" || f.files[backupPasswordFile] != "s3cret" {
		t.Errorf("files %q / %q", f.files[backupAdoptFile], f.files[backupPasswordFile])
	}
	for _, r := range f.runs {
		if slices.Contains(r, "old-key") || slices.Contains(r, "s3cret") {
			t.Errorf("a key is on a command line: %v", r)
		}
	}
	if lastStep(f, func(r []string) bool {
		return slices.Equal(r, []string{"rm", "-f", backupPasswordFile, backupAdoptFile})
	}) < add {
		t.Error("the key files were not removed after")
	}

	// Already ours: no add.
	f = rig()
	if err := adoptBackup(ctx, f, backupAdoptRequest{Repository: "http://h/", Password: "whatever"}); err != nil || len(resticRuns(f, "add")) != 0 {
		t.Fatalf("a repository the volume's key opens: err %v, adds %d", err, len(resticRuns(f, "add")))
	}

	opens = map[string]bool{}
	f = rig()
	if err := adoptBackup(ctx, f, backupAdoptRequest{Repository: "http://h/", Password: "wrong"}); err == nil || !strings.Contains(err.Error(), "does not open") {
		t.Fatalf("a wrong typed key: %v", err)
	}
	if err := adoptBackup(ctx, f, backupAdoptRequest{Repository: "http://h/", Password: "  "}); err == nil {
		t.Fatal("an empty key was accepted")
	}
}

// sidecarTar is one backup's members directory as `restic dump --archive tar` writes it.
func sidecarTar(t *testing.T, metas ...quadlet.SnapshotMeta) []byte {
	t.Helper()
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	_ = tw.WriteHeader(&tar.Header{Name: "/briard-backup/briard/members/", Typeflag: tar.TypeDir, Mode: 0o755})
	for _, m := range metas {
		raw, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		if err := tw.WriteHeader(&tar.Header{Name: "/briard-backup/briard/members/" + m.Service + ".json", Typeflag: tar.TypeReg, Mode: 0o600, Size: int64(len(raw))}); err != nil {
			t.Fatal(err)
		}
		tw.Write(raw)
	}
	tw.Close()
	return b.Bytes()
}

// The Backup page's list: every backup the repository holds, newest first, whichever household
// made it, each with the apps its sidecars describe -- a sidecar that does not, skipped as the
// restore would refuse it. A read that takes no lock, and its own password file, removed after:
// the one a running backup reads must outlive the list.
func TestTheListReadsEveryBackupWithItsApps(t *testing.T) {
	taken := time.Date(2026, 10, 30, 8, 34, 13, 0, time.UTC)
	members := sidecarTar(t,
		quadlet.SnapshotMeta{Service: "home-assistant", TakenAt: taken, Manifest: haManifest},
		quadlet.SnapshotMeta{Service: "mosquitto", TakenAt: taken}, // no manifest: not restorable
	)
	f, _ := backupRig(map[string]func() ([]byte, error){
		"snapshots": func() ([]byte, error) {
			return []byte(`[{"id":"aaaa1111","time":"2026-10-29T09:00:00Z","hostname":"f1d"},` +
				`{"id":"bbbb2222","time":"2026-10-30T09:00:00Z","hostname":"an-earlier-machine"}]`), nil
		},
		"dump": func() ([]byte, error) { return members, nil },
	})
	list, err := listBackups(context.Background(), f, backupListRequest{Repository: "http://h/", Group: "f1d"})
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].ID != "bbbb2222" || list[1].ID != "aaaa1111" {
		t.Fatalf("list = %+v; want both backups, newest first", list)
	}
	if list[0].Own || !list[1].Own {
		t.Errorf("own = %v, %v; want the earlier machine's not this household's", list[0].Own, list[1].Own)
	}
	for _, b := range list {
		if len(b.Apps) != 1 || b.Apps[0].Name != "home-assistant" || b.Apps[0].Version != "2026.7.1" || !b.Apps[0].TakenAt.Equal(taken) {
			t.Errorf("%s's apps = %+v; want home-assistant 2026.7.1 taken at %s alone", b.ID, b.Apps, taken)
		}
	}
	removed := false
	for _, r := range f.runs {
		if r[0] == "restic" && (!slices.Contains(r, "--no-lock") || !slices.Contains(r, backupListPasswordFile)) {
			t.Errorf("restic ran without --no-lock and the list's own password file: %v", r)
		}
		if slices.Contains(r, backupPasswordFile) {
			t.Errorf("the list touched the backup's password file: %v", r)
		}
		removed = removed || (r[0] == "rm" && slices.Contains(r, backupListPasswordFile))
	}
	if !removed {
		t.Error("the list's password file was left behind")
	}
}

// A backup whose members cannot be read is still listed, with no apps: the household may be
// looking for exactly that one, and the page says what it can.
func TestTheListKeepsABackupItCannotReadInto(t *testing.T) {
	f, _ := backupRig(map[string]func() ([]byte, error){
		"snapshots": func() ([]byte, error) {
			return []byte(`[{"id":"cccc3333","time":"2026-10-30T09:00:00Z","hostname":"f1d"}]`), nil
		},
		"dump": func() ([]byte, error) { return nil, errors.New("path not found") },
	})
	list, err := listBackups(context.Background(), f, backupListRequest{Repository: "http://h/", Group: "f1d"})
	if err != nil || len(list) != 1 || list[0].ID != "cccc3333" || len(list[0].Apps) != 0 {
		t.Fatalf("list = %+v, err %v; want the one backup, with no apps", list, err)
	}
}
