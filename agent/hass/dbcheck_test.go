package hass

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

// dbFake is a guest holding files, whose `podman run` answers the check from a table keyed by
// the directory mounted at /db. A directory with no answer fails to run, which is the
// could-not-run case.
type dbFake struct {
	files   map[string]string
	answers map[string]string // dir -> the check's JSON line
	runs    [][]string
}

func newDBFake() *dbFake {
	return &dbFake{files: map[string]string{}, answers: map[string]string{}}
}

func (f *dbFake) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	f.runs = append(f.runs, append([]string{name}, args...))
	switch name {
	case "podman":
		for i, a := range args {
			if a == "-v" {
				dir := strings.TrimSuffix(args[i+1], ":/db:ro")
				if ans, ok := f.answers[dir]; ok {
					return []byte("some podman chatter\n" + ans + "\n"), nil
				}
			}
		}
		return []byte("Error: no such directory"), errors.New("exit status 125")
	case "mv":
		src, dst := args[len(args)-2], args[len(args)-1]
		v, ok := f.files[src]
		if !ok {
			return nil, errors.New("no such file")
		}
		f.files[dst] = v
		delete(f.files, src)
	case "cp":
		src, dst := args[len(args)-2], args[len(args)-1]
		v, ok := f.files[src]
		if !ok {
			return nil, errors.New("no such file")
		}
		f.files[dst] = v
	case "rm":
		for _, p := range args[1:] {
			delete(f.files, p)
		}
	case "test":
		if f.files[args[1]] == "" {
			return nil, errors.New("exit status 1")
		}
	case "ls":
		dir := args[len(args)-1] + "/"
		var out []string
		for p := range f.files {
			if rest, ok := strings.CutPrefix(p, dir); ok && !strings.Contains(rest, "/") {
				out = append(out, rest)
			}
		}
		return []byte(strings.Join(out, "\n")), nil
	}
	return nil, nil
}

func (f *dbFake) WriteFile(p string, data []byte) error { f.files[p] = string(data); return nil }
func (f *dbFake) ReadFile(p string) ([]byte, error) {
	v, ok := f.files[p]
	if !ok {
		return nil, os.ErrNotExist
	}
	return []byte(v), nil
}

func (f *dbFake) state(m Member) string {
	r, ok := Record(f, m.App)
	if !ok {
		return ""
	}
	return r.State
}

const (
	answerClean   = `{"check": "ok", "detail": "ok", "schema": 48}`
	answerCorrupt = `{"check": "corrupt", "detail": "database disk image is malformed"}`
	liveDir       = "/var/lib/briard/home-assistant/config"
	testImage     = "ghcr.io/home-assistant/home-assistant@sha256:00"
)

var dbNow = time.Date(2026, 10, 1, 5, 30, 0, 0, time.UTC)

// ringAt is a quiesced member taken `ago` before dbNow.
func ringAt(name string, ago time.Duration) Member {
	p := "/var/lib/briard/.snapshots/home-assistant-clock-" + name
	return Member{Path: p, Dir: p + "/config", App: p + ".app.json", At: dbNow.Add(-ago), Quiesced: true}
}

func hold(t *testing.T, f *dbFake, m Member, state string) {
	t.Helper()
	if err := SetRecord(context.Background(), f, m.App, &DBRecord{State: state}); err != nil {
		t.Fatal(err)
	}
}

// TestSampledRetainsAQuiescedSampleEveryEightHours: the retained samples are what bound the
// history a restore loses, so one is kept every RetainEvery and no more often.
func TestSampledRetainsAQuiescedSampleEveryEightHours(t *testing.T) {
	ctx := context.Background()
	f := newDBFake()
	old := ringAt("a", 9*time.Hour)
	hold(t, f, old, DBRetained)
	soon, later := ringAt("b", 2*time.Hour), ringAt("c", 0)
	if err := Sampled(ctx, f, []Member{old, soon}, soon); err != nil {
		t.Fatal(err)
	}
	if f.state(soon) != "" {
		t.Fatalf("a sample 7 h after the last hold was retained")
	}
	if err := Sampled(ctx, f, []Member{old, soon, later}, later); err != nil {
		t.Fatal(err)
	}
	if f.state(later) != DBRetained {
		t.Fatalf("a sample 9 h after the last hold was not retained")
	}
	crash := ringAt("d", 0)
	crash.Quiesced = false
	_ = Sampled(ctx, f, []Member{crash}, crash)
	if f.state(crash) != "" {
		t.Fatalf("a crash-consistent sample was retained; the check may trust only quiesced bytes")
	}
}

// TestSetRecordKeepsOtherKeysAndRemovesAnEmptySidecar: the sidecar's existence is the hold, so
// releasing the last key must remove it, and another app's key must survive ours.
func TestSetRecordKeepsOtherKeysAndRemovesAnEmptySidecar(t *testing.T) {
	ctx := context.Background()
	f := newDBFake()
	m := ringAt("a", time.Hour)
	f.files[m.App] = `{"other":{"x":1}}`
	hold(t, f, m, DBClean)
	if err := SetRecord(ctx, f, m.App, nil); err != nil {
		t.Fatal(err)
	}
	if f.files[m.App] != `{"other":{"x":1}}` {
		t.Fatalf("sidecar = %q, want the other key alone", f.files[m.App])
	}
	delete(f.files, m.App)
	hold(t, f, m, DBRetained)
	_ = SetRecord(ctx, f, m.App, nil)
	if _, ok := f.files[m.App]; ok {
		t.Fatalf("an empty sidecar was left holding its member")
	}
}

// TestACleanNightReleasesEveryOlderHold: a clean newest copy is the floor, and nothing older is
// worth keeping for the recorder.
func TestACleanNightReleasesEveryOlderHold(t *testing.T) {
	f := newDBFake()
	a, b, c := ringAt("a", 30*time.Hour), ringAt("b", 20*time.Hour), ringAt("c", time.Hour)
	hold(t, f, a, DBClean)
	hold(t, f, b, DBRetained)
	f.answers[c.Dir] = answerClean
	rep := Nightly(context.Background(), f, testImage, liveDir, []Member{a, b, c})
	if rep.Verdict != VerdictClean || f.state(c) != DBClean || f.state(a) != "" || f.state(b) != "" {
		t.Fatalf("report %+v, states %q %q %q", rep, f.state(a), f.state(b), f.state(c))
	}
}

// TestTheNewestQuiescedMemberIsTheOneChecked: a crash-consistent member is never what a verdict
// is about.
func TestTheNewestQuiescedMemberIsTheOneChecked(t *testing.T) {
	f := newDBFake()
	q, crash := ringAt("a", 2*time.Hour), ringAt("b", time.Hour)
	crash.Quiesced = false
	f.answers[q.Dir] = answerClean
	rep := Nightly(context.Background(), f, testImage, liveDir, []Member{q, crash})
	if rep.Checked != q.Path {
		t.Fatalf("checked %q, want the newest quiesced %q", rep.Checked, q.Dir)
	}
}

// TestACorruptNightRestoresTheNewestCleanHeldCopy: the search goes newest first, steps over a
// damaged copy, and hands the restore the first that checks clean.
func TestACorruptNightRestoresTheNewestCleanHeldCopy(t *testing.T) {
	f := newDBFake()
	floor, mid, bad, newest := ringAt("a", 40*time.Hour), ringAt("b", 24*time.Hour), ringAt("c", 10*time.Hour), ringAt("d", time.Hour)
	hold(t, f, floor, DBClean)
	hold(t, f, mid, DBRetained)
	hold(t, f, bad, DBRetained)
	f.answers[newest.Dir] = answerCorrupt
	f.answers[bad.Dir] = answerCorrupt
	f.answers[mid.Dir] = answerClean
	f.answers[floor.Dir] = answerClean
	f.answers[liveDir] = `{"schema": 48}`
	rep := Nightly(context.Background(), f, testImage, liveDir, []Member{floor, mid, bad, newest})
	if rep.Candidate != mid.Path || !rep.CandidateAt.Equal(mid.At) {
		t.Fatalf("candidate %q at %v, want the newest clean hold %s", rep.Candidate, rep.CandidateAt, mid.Path)
	}
	if rep.Verdict != VerdictCorrupt {
		t.Fatalf("report %+v", rep)
	}
	if f.state(newest) != "" || f.state(bad) != "" {
		t.Errorf("a damaged copy still holds its member: %q %q", f.state(newest), f.state(bad))
	}
	if f.state(mid) != DBClean || f.state(floor) != "" {
		t.Errorf("the restored copy is the new floor: mid %q floor %q", f.state(mid), f.state(floor))
	}
}

// TestNoRestoreWithoutBothPositiveReads: an unreadable candidate, an unreadable live schema, or
// a candidate whose schema is newer than the live database's all end in no restore -- and the
// newer schema steps over to an older copy rather than stopping.
func TestNoRestoreWithoutBothPositiveReads(t *testing.T) {
	for _, tc := range []struct {
		name     string
		live     string // "" = the live read fails
		mid      string // "" = the candidate's read fails
		restored bool   // a candidate is named
	}{
		{"the live schema cannot be read", "", answerClean, false},
		{"the candidate cannot be read", `{"schema": 48}`, "", false},
		{"the candidate has no schema version", `{"schema": 48}`, `{"check": "ok", "schema_error": "no such table"}`, false},
		{"the live schema is older than the candidate's", `{"schema": 47}`, answerClean, true}, // falls back to the floor
		{"both reads are positive", `{"schema": 48}`, answerClean, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newDBFake()
			floor, mid, newest := ringAt("a", 40*time.Hour), ringAt("b", 20*time.Hour), ringAt("c", time.Hour)
			hold(t, f, floor, DBClean)
			hold(t, f, mid, DBRetained)
			f.answers[newest.Dir] = answerCorrupt
			f.answers[floor.Dir] = `{"check": "ok", "schema": 47}`
			if tc.live != "" {
				f.answers[liveDir] = tc.live
			}
			if tc.mid != "" {
				f.answers[mid.Dir] = tc.mid
			}
			rep := Nightly(context.Background(), f, testImage, liveDir, []Member{floor, mid, newest})
			if (rep.Candidate != "") != tc.restored {
				t.Fatalf("report %+v, want a candidate=%t", rep, tc.restored)
			}
			if !tc.restored && rep.Why == "" {
				t.Errorf("no candidate and no reason: %+v", rep)
			}
			if tc.name == "the live schema is older than the candidate's" && rep.Candidate != floor.Path {
				t.Errorf("candidate %s, want the floor whose schema is not newer", rep.Candidate)
			}
		})
	}
}

// TestACheckThatCannotRunIsNoVerdict: no answer is never damage, and the hold it took for the
// check is released.
func TestACheckThatCannotRunIsNoVerdict(t *testing.T) {
	f := newDBFake()
	m := ringAt("a", time.Hour)
	rep := Nightly(context.Background(), f, testImage, liveDir, []Member{m})
	if rep.Verdict != "" || rep.Why == "" {
		t.Fatalf("report %+v, want no verdict with a reason", rep)
	}
	if f.state(m) != "" {
		t.Fatalf("the check's hold was left: %q", f.state(m))
	}
}

// TestACheckingLeftByACrashIsCleared: the next night clears it, so a crash cannot hold a member.
func TestACheckingLeftByACrashIsCleared(t *testing.T) {
	f := newDBFake()
	stale, newest := ringAt("a", 25*time.Hour), ringAt("b", time.Hour)
	hold(t, f, stale, DBChecking)
	stale.Quiesced = false // so the night checks the other one
	f.answers[newest.Dir] = answerClean
	Nightly(context.Background(), f, testImage, liveDir, []Member{stale, newest})
	if f.state(stale) != "" {
		t.Fatalf("a stale checking survived the night: %q", f.state(stale))
	}
}

// TestRestoreDBReplacesTheWalBeforeTheDatabase: a stale -wal beside a different database is
// replayed into it, so the live one goes before the copy lands, and the copy's own comes with it.
func TestRestoreDBReplacesTheWalBeforeTheDatabase(t *testing.T) {
	f := newDBFake()
	from := ringAt("a", time.Hour).Dir
	f.files[from+"/"+dbName] = "clean"
	f.files[from+"/"+dbName+"-wal"] = "frames"
	f.files[liveDir+"/"+dbName] = "damaged"
	f.files[liveDir+"/"+dbName+"-wal"] = "stale"
	f.files[liveDir+"/"+dbName+"-shm"] = "shm"
	f.files[liveDir+"/"+dbName+".corrupt.2026-10-01T04:12:00"] = "set aside"
	f.files[liveDir+"/.storage/core.config_entries.corrupt.2026-10-01T04:12:00"] = "not ours"
	touched, err := RestoreDB(context.Background(), f, from, liveDir)
	if err != nil || !touched {
		t.Fatalf("touched=%t err=%v", touched, err)
	}
	if f.files[liveDir+"/"+dbName] != "clean" || f.files[liveDir+"/"+dbName+"-wal"] != "frames" {
		t.Fatalf("live = %q / %q", f.files[liveDir+"/"+dbName], f.files[liveDir+"/"+dbName+"-wal"])
	}
	for _, gone := range []string{"-shm", ".corrupt.2026-10-01T04:12:00"} {
		if _, ok := f.files[liveDir+"/"+dbName+gone]; ok {
			t.Errorf("%s survived the restore", dbName+gone)
		}
	}
	rmWal, mvDB := -1, -1
	for i, r := range f.runs {
		if r[0] == "rm" && slices.Contains(r, liveDir+"/"+dbName+"-wal") {
			rmWal = i
		}
		if r[0] == "mv" && r[len(r)-1] == liveDir+"/"+dbName {
			mvDB = i
		}
	}
	if rmWal < 0 || mvDB < 0 || rmWal > mvDB {
		t.Fatalf("the live -wal was not removed before the database landed: %v", f.runs)
	}
}

// TestRestoreDBThatCannotCopyTouchesNothing: the copies come first, so a failed one leaves the
// live database as it was and the app may be started again.
func TestRestoreDBThatCannotCopyTouchesNothing(t *testing.T) {
	f := newDBFake()
	f.files[liveDir+"/"+dbName] = "damaged"
	touched, err := RestoreDB(context.Background(), f, ringAt("a", time.Hour).Dir, liveDir)
	if err == nil || touched {
		t.Fatalf("touched=%t err=%v, want an untouched failure", touched, err)
	}
	if f.files[liveDir+"/"+dbName] != "damaged" {
		t.Fatal("the live database moved on a failed copy")
	}
}

// TestRestorableReadsTheGateAgain: the check that named a copy ran earlier, and Home Assistant may
// have moved since, so the restore asks both schema questions again and needs a clean record.
func TestRestorableReadsTheGateAgain(t *testing.T) {
	ctx := context.Background()
	m := ringAt("a", time.Hour)
	for _, tc := range []struct {
		name   string
		record string
		mine   string
		live   string
		ok     bool
	}{
		{"both positive", DBClean, `{"schema": 48}`, `{"schema": 48}`, true},
		{"not checked clean", DBRetained, `{"schema": 48}`, `{"schema": 48}`, false},
		{"Home Assistant went back a version", DBClean, `{"schema": 48}`, `{"schema": 47}`, false},
		{"the live read fails", DBClean, `{"schema": 48}`, "", false},
		{"the copy's schema is unreadable", DBClean, `{"schema_error": "no such table"}`, `{"schema": 48}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newDBFake()
			hold(t, f, m, tc.record)
			f.answers[m.Dir] = tc.mine
			if tc.live != "" {
				f.answers[liveDir] = tc.live
			}
			if err := Restorable(ctx, f, testImage, liveDir, m); (err == nil) != tc.ok {
				t.Fatalf("Restorable = %v, want ok=%t", err, tc.ok)
			}
		})
	}
}
