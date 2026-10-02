package guestagent

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"briard.io/agent/hass"
	"briard.io/agent/quadlet"
)

const dbRigManifest = `{"name":"home-assistant","version":"2026.7.1","containers":[{"name":"app",` +
	`"image":"ghcr.io/x/ha@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",` +
	`"mount":"/config","primary":true,"port":8123,"healthPath":"/"}]}`

// dbRig is a ring holding Home Assistant, whose `podman run` answers the recorder check from a
// table keyed by the directory mounted at /db, and whose rm, cp and test act on its files.
func dbRig(answers map[string]string, members ...string) *fakeExec {
	f := ringExec(members...)
	f.files[manifestPath("home-assistant")] = dbRigManifest
	for _, n := range members {
		p := quadlet.SnapshotSidecar(quadlet.SnapshotsDir + n)
		var meta quadlet.SnapshotMeta
		_ = json.Unmarshal([]byte(f.files[p]), &meta)
		meta.Consistency, meta.Manifest = quadlet.Quiesced, dbRigManifest
		b, _ := json.Marshal(meta)
		f.files[p] = string(b)
	}
	inner := f.runFn
	f.runFn = func(name string, args []string) ([]byte, error) {
		switch name {
		case "podman":
			for i, a := range args {
				if a == "-v" {
					if ans, ok := answers[strings.TrimSuffix(args[i+1], ":/db:ro")]; ok {
						return []byte(ans), nil
					}
				}
			}
			return nil, errors.New("exit status 125")
		case "rm":
			for _, p := range args[1:] {
				delete(f.files, p)
			}
			return nil, nil
		case "cp":
			v, ok := f.files[args[len(args)-2]]
			if !ok {
				return nil, errors.New("no such file")
			}
			f.files[args[len(args)-1]] = v
			return nil, nil
		case "test":
			if f.files[args[1]] == "" {
				return nil, errors.New("exit status 1")
			}
			return nil, nil
		case "systemctl":
			return nil, nil // every container unit is active, and stops and starts
		}
		return inner(name, args)
	}
	return f
}

func appState(f *fakeExec, member string) string {
	r, ok := hass.Record(f, quadlet.AppSidecar(member))
	if !ok {
		return ""
	}
	return r.State
}

// TestAQuiescedTakeIsHeldForTheRecorder: the take path is where a restore candidate comes from,
// so a quiesced member taken long enough after the last hold gets one, and the next does not.
func TestAQuiescedTakeIsHeldForTheRecorder(t *testing.T) {
	ctx := context.Background()
	f := dbRig(nil)
	run := func(name string, args ...string) error { _, err := f.Run(ctx, name, args...); return err }
	take := func(at time.Time, cons quadlet.Consistency) string {
		member := quadlet.SnapshotMember("home-assistant", quadlet.TriggerClock, at)
		meta := quadlet.SnapshotMeta{Service: "home-assistant", Trigger: quadlet.TriggerClock, TakenAt: at, Consistency: cons, Manifest: dbRigManifest}
		b, _ := json.Marshal(meta)
		if err := takeSnapshot(ctx, f, run, quadlet.DataRoot("home-assistant"), member, string(b)); err != nil {
			t.Fatal(err)
		}
		recordMember(ctx, f, member, meta)
		return member
	}
	now := time.Now().UTC()
	crash := take(now.Add(-10*time.Hour), quadlet.Crash)
	first := take(now.Add(-9*time.Hour), quadlet.Quiesced)
	soon := take(now.Add(-2*time.Hour), quadlet.Quiesced)
	if appState(f, crash) != "" {
		t.Error("a crash-consistent take was held")
	}
	if appState(f, first) != hass.DBRetained {
		t.Fatalf("the first quiesced take was not retained: %q", f.files[quadlet.AppSidecar(first)])
	}
	if appState(f, soon) != "" {
		t.Error("a take 7 h after the last hold was retained")
	}
	// THE HOLD IS WHAT KEEPS IT: the history would have replaced it with the next sample.
	members, _ := listMembers(ctx, f, "home-assistant")
	for _, m := range members {
		if m.Member == first && !m.App {
			t.Fatal("the listing does not report the app's hold, so the prune cannot honour it")
		}
	}
	if slices.Contains(deleted(f), strings.TrimPrefix(first, quadlet.SnapshotsDir)) {
		t.Fatalf("the held member was pruned: %v", deleted(f))
	}
}

// TestTheNightlyCheckRestoresFromAHeldCleanCopy is check then restore, end to end in the guest: the
// damaged newest member is found, the held clean one is put back, and around it Home Assistant is
// stopped, an undo point carrying the event is taken BEFORE the live database moves, and the app
// is started again.
func TestTheNightlyCheckRestoresFromAHeldCleanCopy(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	good := strings.TrimPrefix(quadlet.SnapshotMember("home-assistant", quadlet.TriggerClock, now.Add(-20*time.Hour)), quadlet.SnapshotsDir)
	bad := strings.TrimPrefix(quadlet.SnapshotMember("home-assistant", quadlet.TriggerClock, now.Add(-time.Hour)), quadlet.SnapshotsDir)
	live := quadlet.DataPath("home-assistant", "app")
	f := dbRig(map[string]string{
		quadlet.SnapshotsDir + bad + "/app":  `{"check": "corrupt", "detail": "database disk image is malformed"}`,
		quadlet.SnapshotsDir + good + "/app": `{"check": "ok", "schema": 48}`,
		live:                                 `{"schema": 48}`,
	}, good, bad)
	f.files[quadlet.AppSidecar(quadlet.SnapshotsDir+good)] = `{"hass-db":{"state":"retained"}}`
	f.files[quadlet.SnapshotsDir+good+"/app/home-assistant_v2.db"] = "clean history"
	f.files[live+"/home-assistant_v2.db"] = "damaged history"

	rep := hassDBCheck(ctx, f)
	if rep.Verdict != hass.VerdictCorrupt || rep.Candidate != quadlet.SnapshotsDir+good {
		t.Fatalf("report %+v, want a corrupt verdict naming %s", rep, good)
	}
	if f.files[live+"/home-assistant_v2.db"] != "damaged history" {
		t.Fatal("the check touched the live database; it only reads")
	}
	for _, r := range f.runs {
		if r[0] == "systemctl" {
			t.Fatalf("the check stopped or started a unit: %v", r)
		}
	}
	if err := hassDBRestore(ctx, f, func(name string, args ...string) error { _, err := f.Run(ctx, name, args...); return err }, rep.Candidate); err != nil {
		t.Fatal(err)
	}
	if f.files[live+"/home-assistant_v2.db"] != "clean history" {
		t.Fatalf("live database = %q", f.files[live+"/home-assistant_v2.db"])
	}
	// The order: stop, the undo point, the database, start.
	step := func(match func([]string) bool) int {
		for i, r := range f.runs {
			if match(r) {
				return i
			}
		}
		return -1
	}
	stop := step(func(r []string) bool { return r[0] == "systemctl" && r[1] == "stop" })
	undo := step(func(r []string) bool {
		return r[0] == "btrfs" && r[2] == "snapshot" && strings.Contains(r[len(r)-1], string(quadlet.TriggerHassDBRestoreBefore))
	})
	moved := step(func(r []string) bool { return r[0] == "mv" && r[len(r)-1] == live+"/home-assistant_v2.db" })
	start := step(func(r []string) bool { return r[0] == "systemctl" && r[1] == "start" })
	if stop < 0 || undo < stop || moved < undo || start < moved {
		t.Fatalf("stop %d, undo %d, database %d, start %d: %v", stop, undo, moved, start, f.runs)
	}
	members, _ := listMembers(ctx, f, "home-assistant")
	var ev *quadlet.Event
	for _, m := range members {
		if m.Meta.Trigger == quadlet.TriggerHassDBRestoreBefore {
			ev = m.Meta.Event
			if m.App {
				t.Error("the undo point, which holds the damaged database, was held as a restore candidate")
			}
		}
	}
	if !ev.Has(quadlet.ReasonDBRestore) {
		t.Fatalf("the undo point carries no restore event: %+v", ev)
	}
	if appState(f, quadlet.SnapshotsDir+good) != hass.DBClean || appState(f, quadlet.SnapshotsDir+bad) != "" {
		t.Errorf("after the restore: good %q, bad %q", appState(f, quadlet.SnapshotsDir+good), appState(f, quadlet.SnapshotsDir+bad))
	}
}

// TestTheVerbIsAdvertised: a verb absent from the handshake is one no host calls.
func TestTheDBCheckVerbIsAdvertised(t *testing.T) {
	if !slices.Contains(guestCapabilities, verbHassDBCheck) {
		t.Fatal("the guest does not advertise the recorder check")
	}
}

// TestTheCheckRunsInTheBackground: the verb answers at once, a second start is refused while one
// runs, the report is handed over once, and a restore waits for the check to finish.
func TestTheCheckRunsInTheBackground(t *testing.T) {
	ctx := context.Background()
	release := make(chan struct{})
	f := dbRig(nil)
	inner := f.runFn
	f.runFn = func(name string, args []string) ([]byte, error) {
		if name == "ls" {
			<-release // the check is "reading" until the test lets it go
		}
		return inner(name, args)
	}
	if !startDBCheck(f) {
		t.Fatal("the check did not start")
	}
	if startDBCheck(f) {
		t.Fatal("a second check started beside the first")
	}
	if s := dbCheckResult(); !s.Running || s.Report != nil {
		t.Fatalf("state %+v, want running with no report", s)
	}
	if err := hassDBRestore(ctx, f, func(string, ...string) error { return nil }, "/x"); err == nil {
		t.Fatal("a restore ran beside a check")
	}
	close(release)
	deadline := time.Now().Add(5 * time.Second)
	var s hass.DBCheckState
	for s = dbCheckResult(); s.Report == nil && time.Now().Before(deadline); s = dbCheckResult() {
		time.Sleep(10 * time.Millisecond)
	}
	if s.Running || s.Report == nil {
		t.Fatalf("state %+v, want a finished report", s)
	}
	if again := dbCheckResult(); again.Report != nil {
		t.Fatal("the report was handed over twice")
	}
}

// TestTheRestoreReadsItsMemberBackFromTheRing: a name that is not a member of the ring is refused
// before anything stops.
func TestTheRestoreReadsItsMemberBackFromTheRing(t *testing.T) {
	ctx := context.Background()
	f := dbRig(nil)
	err := hassDBRestore(ctx, f, func(name string, args ...string) error { _, err := f.Run(ctx, name, args...); return err }, quadlet.SnapshotsDir+"home-assistant-clock-20260101T000000Z")
	if err == nil {
		t.Fatal("restored from a member the ring does not hold")
	}
	for _, r := range f.runs {
		if r[0] == "systemctl" {
			t.Fatalf("a refused restore touched a unit: %v", r)
		}
	}
}

// TestTheRestoreAsksTheGateAgain: Home Assistant went back a version between the check and the
// restore, so the copy the check named is now newer than the live database and nothing stops.
func TestTheRestoreAsksTheGateAgain(t *testing.T) {
	ctx := context.Background()
	good := strings.TrimPrefix(quadlet.SnapshotMember("home-assistant", quadlet.TriggerClock, time.Now().Add(-20*time.Hour)), quadlet.SnapshotsDir)
	f := dbRig(map[string]string{
		quadlet.SnapshotsDir + good + "/app":      `{"check": "ok", "schema": 48}`,
		quadlet.DataPath("home-assistant", "app"): `{"schema": 47}`,
	}, good)
	f.files[quadlet.AppSidecar(quadlet.SnapshotsDir+good)] = `{"hass-db":{"state":"clean"}}`
	err := hassDBRestore(ctx, f, func(name string, args ...string) error { _, err := f.Run(ctx, name, args...); return err }, quadlet.SnapshotsDir+good)
	if err == nil || !strings.Contains(err.Error(), "newer") {
		t.Fatalf("err = %v, want the schema gate's refusal", err)
	}
	for _, r := range f.runs {
		if r[0] == "systemctl" {
			t.Fatalf("a refused restore touched a unit: %v", r)
		}
	}
}
