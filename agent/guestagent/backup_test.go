package guestagent

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"briard.io/agent/quadlet"
)

// backupRig is a volume holding two services, each with a ring whose newest member was taken by a
// different trigger, a VIP and manifests but no certificate yet, and a restic that answers from
// the table it is given (keyed by subcommand).
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
	inner := f.runFn
	f.runFn = func(n string, args []string) ([]byte, error) {
		if n == "ls" && len(args) > 1 && args[1] == manifestDir {
			return []byte("home-assistant.json\nmosquitto.json\n"), nil
		}
		if n == "test" && len(args) == 2 && args[0] == "-e" {
			if args[1] == tlsDir {
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
	for _, v := range []string{verbDataBackup, verbDataBackupResult} {
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
	if ok, err := g.DataBackup(ctx, "http://10.0.0.129:7791/", "pw", nil); err != nil || !ok {
		t.Fatalf("the backup did not start: %v %v", ok, err)
	}
	if ok, err := g.DataBackup(ctx, "http://10.0.0.129:7791/", "pw", nil); err != nil || ok {
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
// request's, and a run with no password has nothing to open the repository with.
func TestTheBackupRefusesWhatIsNotARESTRepository(t *testing.T) {
	for _, req := range []backupRequest{
		{Repository: "/var/lib/briard", Password: "pw"},
		{Repository: "sftp:host:/x", Password: "pw"},
		{Repository: "http://10.0.0.129:7791/"},
	} {
		if ok, err := startBackup(&fakeExec{}, req); ok || err == nil {
			t.Errorf("%+v: started=%v err=%v, want a refusal", req, ok, err)
		}
	}
}

// TestTheBackupCommandLines: the newest member of every service (whatever took it) at a stable
// path, read-only; the password in a 0600 file, never an argument; HA's own tarballs excluded;
// one host name across nodes; the retention policy; everything unmounted and removed after.
func TestTheBackupCommandLines(t *testing.T) {
	f, newest := backupRig(map[string]func() ([]byte, error){
		"backup": func() ([]byte, error) { return []byte(resticSummary), nil },
	})
	rep := runBackup(context.Background(), f, backupRequest{Repository: "http://10.0.0.129:7791/", Password: "s3cret"})
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
		"--exclude **/app/backups", "--host " + backupHost,
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
	if len(fg) != 1 || !strings.Contains(strings.Join(fg[0], " "), "--keep-daily 2 --keep-weekly 1 --prune") {
		t.Errorf("forget %v, want the 2-daily 1-weekly policy with prune", fg)
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
	if rep := runBackup(context.Background(), f, backupRequest{Repository: "http://h/", Password: "pw"}); rep.Error != "" {
		t.Fatalf("report %+v", rep)
	}
	if len(resticRuns(f, "init")) != 1 {
		t.Fatal("a missing repository was not created")
	}

	f, _ = backupRig(map[string]func() ([]byte, error){
		"cat":  func() ([]byte, error) { return nil, errors.New("wrong password") },
		"init": func() ([]byte, error) { return nil, errors.New("config file already exists") },
	})
	rep := runBackup(context.Background(), f, backupRequest{Repository: "http://h/", Password: "pw"})
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
	rep := runBackup(context.Background(), f, backupRequest{Repository: "http://h/", Password: "pw"})
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
	rep := runBackup(context.Background(), f, backupRequest{Repository: "http://h/", Password: "pw"})
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
	if rep := runBackup(context.Background(), f, backupRequest{Repository: "http://h/", Password: "pw", Host: host}); rep.Error != "" {
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
		req := backupRequest{Repository: "http://h/", Password: "pw", Host: map[string][]byte{name: nil}}
		if ok, err := startBackup(&fakeExec{}, req); ok || err == nil {
			t.Errorf("%q: started=%v err=%v, want a refusal", name, ok, err)
		}
	}
}
