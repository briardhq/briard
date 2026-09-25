package guestagent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"briard.io/agent/hass"
	"briard.io/agent/quadlet"
	"briard.io/agent/services"
)

// ringExec is a guest with a .snapshots directory: `ls` answers with `members`, `btrfs subvolume
// show` fails for anything absent (nothing is at a fresh member's path), and everything else
// succeeds. It is the minimum a ring's rate limit and its take both read.
const haToken = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func ringExec(members ...string) *fakeExec {
	f := &fakeExec{files: map[string]string{
		manifestPath("home-assistant"):              `{"name":"home-assistant","version":"2026.7.1"}`,
		services.InboundTokenPath("home-assistant"): haToken,
		services.InboundTokenPath("mosquitto"):      "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff",
	}}
	// Every member we can name has a plain sidecar, as the take path guarantees; a test that wants
	// an event on one, or none at all, edits f.files afterwards.
	for _, n := range members {
		if svc, tr, at, ok := quadlet.ParseSnapshotMember(n); ok {
			b, _ := json.Marshal(quadlet.SnapshotMeta{Service: svc, Trigger: tr, TakenAt: at})
			f.files[quadlet.SnapshotSidecar(quadlet.SnapshotsDir+n)] = string(b)
		}
	}
	members = slices.Clone(members) // the caller's slice is theirs; the listing below edits this one
	// THE LISTING FOLLOWS THE TAKES AND DELETES, so a prune after a take sees the member it just
	// took -- which is what makes every earlier plain sample replaceable.
	f.runFn = func(name string, args []string) ([]byte, error) {
		// The run directory holds the per-service dirs and plenty that is not one.
		if name == "ls" && len(args) > 1 && args[1] == services.RunDir() {
			return []byte("home-assistant\nmosquitto\nagent.sock\nnode-storage.json"), nil
		}
		if name == "ls" {
			return []byte(strings.Join(members, "\n")), nil
		}
		if len(args) > 1 && args[1] == "show" {
			return nil, errors.New("ERROR: not a subvolume")
		}
		if name == "mv" && len(args) > 1 { // a sidecar replaced by rename, as writeEvent does
			src, dst := args[len(args)-2], args[len(args)-1]
			f.files[dst] = f.files[src]
			delete(f.files, src)
			return nil, nil
		}
		if name == "btrfs" && len(args) > 3 && args[1] == "snapshot" {
			members = append(members, strings.TrimPrefix(args[len(args)-1], quadlet.SnapshotsDir))
		}
		if name == "btrfs" && len(args) > 2 && args[1] == "delete" {
			gone := strings.TrimPrefix(args[2], quadlet.SnapshotsDir)
			members = slices.DeleteFunc(members, func(n string) bool { return n == gone })
		}
		return nil, nil
	}
	return f
}

// restoreRig is a ring whose Home Assistant manifest names the container that holds the data, so
// the registry can say where the restore marker would be ([B.143]). `marker` is that file's
// content, or "" for a service with no restore in flight. Its `rm` really removes, because the
// pending fact is consumed by one and a fake that kept it would hide a second restore half.
func restoreRig(marker string, members ...string) *fakeExec {
	f := ringExec(members...)
	f.files[manifestPath("home-assistant")] =
		`{"name":"home-assistant","version":"2026.7.1","containers":[{"name":"app",` +
			`"image":"ghcr.io/x/ha@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",` +
			`"mount":"/config","primary":true,"port":8123,"healthPath":"/"}]}`
	if marker != "" {
		f.files[quadlet.DataRoot("home-assistant")+"/app/"+hass.RestoreMarker] = marker
	}
	inner := f.runFn
	f.runFn = func(name string, args []string) ([]byte, error) {
		if name == "rm" && len(args) > 1 {
			delete(f.files, args[len(args)-1])
			return nil, nil
		}
		return inner(name, args)
	}
	return f
}

// tookMember is the member the ring just wrote, with the sidecar the history will read.
func tookMember(t *testing.T, f *fakeExec) (string, quadlet.SnapshotMeta) {
	t.Helper()
	var took string
	for _, r := range f.runs {
		if len(r) > 4 && r[1] == "subvolume" && r[2] == "snapshot" && r[3] == "-r" {
			took = r[5]
		}
	}
	if took == "" {
		t.Fatalf("nothing was snapshotted: %v", f.runs)
	}
	var meta quadlet.SnapshotMeta
	if err := json.Unmarshal([]byte(f.files[quadlet.SnapshotSidecar(took)]), &meta); err != nil {
		t.Fatalf("the member has no readable sidecar: %v", err)
	}
	return took, meta
}

// A CONTAINER START WHOSE DATA WAS NEVER FLUSHED ([B.143]). This is the case quadlet.Consistency
// exists for and the one a stopped container cannot answer: after a promotion nothing is running
// either, but the old primary never shut the service down.
func TestStartAfterAnUncleanStopSaysSo(t *testing.T) {
	f := restoreRig("") // no clean-stop marker: whatever held this service last did not stop, it died
	if _, err := TakeStartMember(context.Background(), f, "home-assistant"); err != nil {
		t.Fatal(err)
	}
	member, meta := tookMember(t, f)
	if meta.Consistency != quadlet.Crash {
		t.Errorf("consistency = %q, want crash -- nothing flushed this data", meta.Consistency)
	}
	if _, tr, _, _ := quadlet.ParseSnapshotMember(member); tr != quadlet.TriggerStart {
		t.Errorf("trigger = %q -- an unclean start is still an ordinary start", tr)
	}
}

// TestACleanStopMakesTheNextMemberQuiesced, and SPENDS the claim: the marker says the last stop
// flushed this data, which stops being true the moment the container runs again.
func TestACleanStopMakesTheNextMemberQuiesced(t *testing.T) {
	f := restoreRig("")
	if err := RecordServiceStop(context.Background(), f, "home-assistant", "success"); err != nil {
		t.Fatal(err)
	}
	if _, err := TakeStartMember(context.Background(), f, "home-assistant"); err != nil {
		t.Fatal(err)
	}
	if _, meta := tookMember(t, f); meta.Consistency != quadlet.Quiesced {
		t.Errorf("consistency = %q, want quiesced after a clean stop", meta.Consistency)
	}
	if _, ok := f.files[cleanStopPath("home-assistant")]; ok {
		t.Error("the marker outlived the start it described; the NEXT member would claim it too")
	}
}

// TestAKilledStopClaimsNothing: systemd runs ExecStopPost whatever the result, so this path is
// reached on a stop that timed out or was killed too -- and a claim written there would be the
// field saying the opposite of the truth.
func TestAKilledStopClaimsNothing(t *testing.T) {
	f := restoreRig("")
	f.files[cleanStopPath("home-assistant")] = "success\n" // a claim left by some earlier stop
	if err := RecordServiceStop(context.Background(), f, "home-assistant", "timeout"); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.files[cleanStopPath("home-assistant")]; ok {
		t.Error("a timed-out stop left a claim that this service's data was flushed")
	}
}

// TestAnInnerRestartDoesNotReadTheMarker: Home Assistant restarting itself is not a container
// stop, so the marker has nothing to say about it -- and reading one there would mark every
// ordinary HA restart crash-consistent, which is wrong and is also the most common member there is.
func TestAnInnerRestartDoesNotReadTheMarker(t *testing.T) {
	f := restoreRig("")
	serve(t, f, `{"verb":"service.starting","token":"`+haToken+`"}`)
	if _, meta := tookMember(t, f); meta.Consistency != quadlet.Quiesced {
		t.Errorf("consistency = %q, want quiesced: the app stopped itself and the container never went down", meta.Consistency)
	}
}

// TestInboundRecordsTheBackupRestorePair ([B.143]) is the one operation only this channel can see.
// Home Assistant's own restore unlinks its marker before the wipe, so nothing that polls from
// outside can ever catch one in flight -- and the household's history gets a row that says what
// happened, on the point that undoes it.
func TestInboundRecordsTheBackupRestorePair(t *testing.T) {
	f := restoreRig(`{"path": "/config/backups/e1a2b3c4.tar"}`)
	serve(t, f, `{"verb":"service.starting","token":"`+haToken+`"}`)
	member, meta := tookMember(t, f)
	if _, tr, _, _ := quadlet.ParseSnapshotMember(member); tr != quadlet.TriggerRestoreBefore {
		t.Errorf("trigger = %q, want the before half of the pair", tr)
	}
	// The restore is a row in the history, on the point that undoes it ([B.167]).
	if meta.Event == nil || meta.Event.Kind != quadlet.EventBackupRestore || !strings.Contains(meta.Event.What, "e1a2b3c4.tar") {
		t.Errorf("event = %+v, want the backup restore naming the backup", meta.Event)
	}

	// THE SECOND HALF, after HA has consumed its own marker: nothing on the volume says a restore
	// just happened, so this is the node-local fact doing the one job it exists for.
	f2 := restoreRig("")
	f2.files[restorePendingPath("home-assistant")] = f.files[restorePendingPath("home-assistant")]
	serve(t, f2, `{"verb":"service.starting","token":"`+haToken+`"}`)
	member2, meta2 := tookMember(t, f2)
	if _, tr, _, _ := quadlet.ParseSnapshotMember(member2); tr != quadlet.TriggerRestoreAfter {
		t.Errorf("trigger = %q, want the after half of the pair", tr)
	}
	if meta2.Event != nil {
		t.Errorf("the after half carries %+v; it is a baseline and has no event", meta2.Event)
	}
	// AND THE FACT IS SPENT. Left behind, it would read the next ordinary start as the second
	// half of a restore that finished hours ago.
	if _, ok := f2.files[restorePendingPath("home-assistant")]; ok {
		t.Error("the restore fact survived the member it described")
	}
}

// TestInboundTakesThePairInsideTheRateLimit: a restore is two members minutes apart by
// construction, and the rate limit exists for a crash loop's hundreds of identical ones. Skipping
// half a pair would leave a household's own restore with no way back.
func TestInboundTakesThePairInsideTheRateLimit(t *testing.T) {
	recent := strings.TrimPrefix(
		quadlet.SnapshotMember("home-assistant", quadlet.TriggerStart, time.Now().Add(-5*time.Second)),
		quadlet.SnapshotsDir)
	f := restoreRig(`{"path": "/config/backups/x.tar"}`, recent)
	resp := serve(t, f, `{"verb":"service.starting","token":"`+haToken+`"}`)
	if strings.Contains(resp.Detail, "still current") {
		t.Fatalf("the rate limit skipped half a restore pair: %q", resp.Detail)
	}
	if _, meta := tookMember(t, f); meta.Event == nil || meta.Event.Kind != quadlet.EventBackupRestore {
		t.Errorf("event = %+v, want the pair's first half", meta.Event)
	}
}

// TestInboundIgnoresAStaleRestoreFact: a restore that never completed leaves the fact behind, and
// it lives in tmpfs so nothing else will clear it. Using it hours later would read an ordinary
// start as the second half of something that never happened.
func TestInboundIgnoresAStaleRestoreFact(t *testing.T) {
	f := restoreRig("")
	f.files[restorePendingPath("home-assistant")] =
		fmt.Sprintf("%d\tbackup old.tar", time.Now().Add(-restorePendingTTL-time.Hour).Unix())
	serve(t, f, `{"verb":"service.starting","token":"`+haToken+`"}`)
	member, meta := tookMember(t, f)
	if _, tr, _, _ := quadlet.ParseSnapshotMember(member); tr != quadlet.TriggerStart {
		t.Errorf("trigger = %q, want an ordinary start", tr)
	}
	if meta.Event != nil {
		t.Errorf("event = %+v, want an ordinary start with none", meta.Event)
	}
}

// TestBackupNameSurvivesWhateverTheMarkerSays: the content is Home Assistant's, its format has
// changed upstream before, and it lands in a line an operator reads. Every shape has to end in
// something safe to print.
func TestBackupNameSurvivesWhateverTheMarkerSays(t *testing.T) {
	for body, want := range map[string]string{
		`{"path": "/config/backups/a1b2.tar"}`: "backup a1b2.tar",
		"/config/backups/plain.tar\n":          "backup plain.tar",
		"":                                     "a backup",
		"{}":                                   "a backup",
		"/config/backups/we\x00ird\x1bname":    "backup weirdname",
	} {
		if got := backupName([]byte(body)); got != want {
			t.Errorf("backupName(%q) = %q, want %q", body, got, want)
		}
	}
	if got := backupName([]byte("/x/" + strings.Repeat("y", 500))); len(got) > 100 {
		t.Errorf("backupName did not cap a long name: %d chars", len(got))
	}
}

// serve sends one request as Home Assistant unless the body already carries its own token.
func serve(t *testing.T, f *fakeExec, body string) inboundResponse {
	t.Helper()
	var out bytes.Buffer
	_ = ServeInbound(context.Background(), f, strings.NewReader(body), &out)
	var resp inboundResponse
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &resp); err != nil {
		t.Fatalf("response %q does not parse: %v", out.String(), err)
	}
	return resp
}

// TestInboundStartingTakesAMember is the channel's whole purpose: a service about to start says
// so, and the member exists before it is answered — which is what makes the caller's wait the
// ordering guarantee ([B.143]).
func TestInboundStartingTakesAMember(t *testing.T) {
	f := ringExec()
	resp := serve(t, f, `{"verb":"service.starting","token":"`+haToken+`"}`)
	if resp.Error != "" {
		t.Fatalf("error = %q, want a member taken", resp.Error)
	}
	var took string
	for _, r := range f.runs {
		if len(r) > 4 && r[1] == "subvolume" && r[2] == "snapshot" && r[3] == "-r" {
			took = r[5]
		}
	}
	if took == "" {
		t.Fatalf("nothing was snapshotted: %v", f.runs)
	}
	svc, ok := quadlet.SnapshotMemberService(took)
	if !ok || svc != "home-assistant" {
		t.Errorf("member %q is not home-assistant's", took)
	}
	// The sidecar carries the manifest the service is running, read from the volume by the guest
	// itself — the host is not in the start path on a promotion or a crash restart.
	var meta quadlet.SnapshotMeta
	if err := json.Unmarshal([]byte(f.files[quadlet.SnapshotSidecar(took)]), &meta); err != nil {
		t.Fatalf("the member has no readable sidecar: %v", err)
	}
	if meta.Trigger != quadlet.TriggerStart {
		t.Errorf("trigger = %q, want %q", meta.Trigger, quadlet.TriggerStart)
	}
	if !strings.Contains(meta.Manifest, "2026.7.1") {
		t.Errorf("sidecar manifest = %q, want the version on the volume", meta.Manifest)
	}
	// QUIESCED, and STATED rather than left to be inferred: this runs in the unit's pre-start, so
	// the container is not up and the previous one was stopped before the restart. A member that
	// does not say what its bytes are is one [B.32] may not use and the picker cannot rank, which
	// is the whole reason the field exists.
	if meta.Consistency != quadlet.Quiesced {
		t.Errorf("consistency = %q, want quiesced", meta.Consistency)
	}
}

// TestInboundStartingRateLimitsACrashLoop: a crash loop restarts every few seconds and would fill
// the picker with hundreds of identical entries. The member that matters is the FIRST — taken
// before the bad change — so the rest are skipped and that one survives.
//
// It is also the abuse bound. The caller is a container, so this is what stops anything running
// as the service from filling the replicated volume on purpose ([B.155] by a new road).
func TestInboundStartingRateLimitsACrashLoop(t *testing.T) {
	recent := quadlet.SnapshotMember("home-assistant", quadlet.TriggerStart, time.Now().Add(-5*time.Second))
	f := ringExec(strings.TrimPrefix(recent, quadlet.SnapshotsDir))
	resp := serve(t, f, `{"verb":"service.starting","token":"`+haToken+`"}`)
	if resp.Error != "" {
		t.Fatalf("error = %q, want a quiet skip", resp.Error)
	}
	for _, r := range f.runs {
		if len(r) > 2 && r[1] == "subvolume" && r[2] == "snapshot" {
			t.Fatalf("a second member was taken inside the floor: %v", f.runs)
		}
	}
	if !strings.Contains(resp.Detail, "still current") {
		t.Errorf("detail = %q, want it to say why nothing was taken", resp.Detail)
	}
}

// TestInboundStartingTakesOneOutsideTheFloor is the other half: the rate limit must not be a mute
// button. A member older than the floor does not suppress the next one.
func TestInboundStartingTakesOneOutsideTheFloor(t *testing.T) {
	old := quadlet.SnapshotMember("home-assistant", quadlet.TriggerStart, time.Now().Add(-2*time.Hour))
	f := ringExec(strings.TrimPrefix(old, quadlet.SnapshotsDir))
	serve(t, f, `{"verb":"service.starting","token":"`+haToken+`"}`)
	var took bool
	for _, r := range f.runs {
		if len(r) > 2 && r[1] == "subvolume" && r[2] == "snapshot" {
			took = true
		}
	}
	if !took {
		t.Fatalf("an hours-old ring suppressed a new member: %v", f.runs)
	}
}

// TestInboundIgnoresAnotherServicesMembers: the rate limit is per service. A busy Home Assistant
// must not be able to suppress the broker's members, or one service's crash loop silently costs
// every other service its history.
func TestInboundIgnoresAnotherServicesMembers(t *testing.T) {
	recent := quadlet.SnapshotMember("mosquitto", quadlet.TriggerStart, time.Now().Add(-5*time.Second))
	f := ringExec(strings.TrimPrefix(recent, quadlet.SnapshotsDir))
	serve(t, f, `{"verb":"service.starting","token":"`+haToken+`"}`)
	var took bool
	for _, r := range f.runs {
		if len(r) > 2 && r[1] == "subvolume" && r[2] == "snapshot" {
			took = true
		}
	}
	if !took {
		t.Fatalf("another service's member suppressed this one: %v", f.runs)
	}
}

// TestInboundIgnoresStrangersInTheSnapshotsDir: `.snapshots` is a directory on a volume a human
// can reach. Anything whose name the ring did not write is not a ring member, and must not be
// able to stop one being taken — a stray directory called `backup` should cost nothing.
func TestInboundIgnoresStrangersInTheSnapshotsDir(t *testing.T) {
	f := ringExec("a-human-copy", "home-assistant-preupgrade", "notes.txt")
	serve(t, f, `{"verb":"service.starting","token":"`+haToken+`"}`)
	var took bool
	for _, r := range f.runs {
		if len(r) > 2 && r[1] == "subvolume" && r[2] == "snapshot" {
			took = true
		}
	}
	if !took {
		t.Fatalf("a stranger's directory entry suppressed a member: %v", f.runs)
	}
}

// TestInboundRequestCannotNameItsService is the trust boundary, asserted.
//
// The socket is shared by every participating container, and everything running as a service can
// reach it — for Home Assistant, every custom component the household ever installed. So the
// identity comes from the TOKEN and nothing else in the request may touch it: a caller that could
// name its own service would act for any other, which with one shared socket is the whole point
// of failure.
func TestInboundRequestCannotNameItsService(t *testing.T) {
	f := ringExec()
	serve(t, f, `{"verb":"service.starting","service":"mosquitto","path":"/etc","token":"`+haToken+`"}`)
	for _, r := range f.runs {
		for _, a := range r {
			if a == "/etc" || strings.Contains(a, "briard/mosquitto") {
				t.Fatalf("the request body reached a command: %v", f.runs)
			}
		}
	}
	var took string
	for _, r := range f.runs {
		if len(r) > 5 && r[2] == "snapshot" {
			took = r[5]
		}
	}
	if svc, _ := quadlet.SnapshotMemberService(took); svc != "home-assistant" {
		t.Errorf("member %q was taken for the service the REQUEST named, not the one its token identifies", took)
	}
}

// TestInboundRefusesACallerItCannotIdentify: with one shared socket the token IS the boundary, so
// everything that is not a minted token has to land in the same place — refused, before any verb
// is reached and before anything is read or written.
func TestInboundRefusesACallerItCannotIdentify(t *testing.T) {
	for _, tok := range []string{
		"",    // none offered
		"xyz", // too short to be one of ours
		"fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210", // right shape, never minted
	} {
		f := ringExec()
		resp := serve(t, f, `{"verb":"service.starting","token":"`+tok+`"}`)
		if resp.Error == "" {
			t.Errorf("token %q was accepted", tok)
		}
		for _, r := range f.runs {
			if len(r) > 2 && r[2] == "snapshot" {
				t.Errorf("token %q reached a snapshot: %v", tok, f.runs)
			}
		}
	}
}

// TestInboundResolvesEachServiceToItsOwnToken: two services share one socket, and the only thing
// separating them is which secret they hold. A token must act for its own service and no other.
func TestInboundResolvesEachServiceToItsOwnToken(t *testing.T) {
	f := ringExec()
	f.files[manifestPath("mosquitto")] = `{"name":"mosquitto","version":"2.1.2"}`
	serve(t, f, `{"verb":"service.starting","token":"ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"}`)
	var took string
	for _, r := range f.runs {
		if len(r) > 5 && r[2] == "snapshot" {
			took = r[5]
		}
	}
	if svc, _ := quadlet.SnapshotMemberService(took); svc != "mosquitto" {
		t.Errorf("member %q, want the broker's — its token is the only thing that says so", took)
	}
}

// TestInboundRefusesAnUnknownVerb: the set is closed, and the name comes back because the
// realistic cause is a wrapper newer than the agent beneath it.
func TestInboundRefusesAnUnknownVerb(t *testing.T) {
	f := ringExec()
	resp := serve(t, f, `{"verb":"data.restore","token":"`+haToken+`"}`)
	if resp.Error == "" {
		t.Fatal("an unknown verb was accepted")
	}
	if !strings.Contains(resp.Error, "data.restore") {
		t.Errorf("error = %q, want the rejected verb named", resp.Error)
	}
	// Resolving the caller necessarily reads the token directory, so "ran nothing" is the wrong
	// bar. What must not happen is any of the WORK: an unrecognised verb touches no subvolume.
	for _, r := range f.runs {
		if r[0] == "btrfs" {
			t.Errorf("an unknown verb reached the volume: %v", f.runs)
		}
	}
}

// TestInboundAlwaysAnswers: the caller is holding its service stopped until this replies, so
// every path has to write a response — a handler that returned without answering would hang a
// household's Home Assistant on its own start.
func TestInboundAlwaysAnswers(t *testing.T) {
	for _, body := range []string{
		`{"verb":"service.starting","token":"` + haToken + `"}`,
		`{"verb":"service.starting","token":"nope"}`,
		`{"verb":"nonsense"}`,
		`not json at all`,
		``,
	} {
		var out bytes.Buffer
		_ = ServeInbound(context.Background(), ringExec(), strings.NewReader(body), &out)
		if out.Len() == 0 {
			t.Errorf("request %q got no response; the caller would wait forever", body)
			continue
		}
		if !bytes.HasSuffix(out.Bytes(), []byte("\n")) {
			t.Errorf("request %q got an unterminated response %q; a line reader would wait forever", body, out.String())
		}
	}
}

// TestInboundRejectsAServiceNameThatIsAPath: the service name is read out of a token FILENAME in
// a directory on /run, and it then becomes a path element in every path the verb derives. Writing
// there takes root, so this is defence in depth rather than a live hole -- but the cost of being
// wrong is a verb pointed at an arbitrary subvolume, and the check is one line.
func TestInboundRejectsAServiceNameThatIsAPath(t *testing.T) {
	const evil = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	f := ringExec()
	f.runFn = func(name string, args []string) ([]byte, error) {
		if name == "ls" && len(args) > 1 && args[1] == services.RunDir() {
			return []byte("../../etc"), nil
		}
		return nil, errors.New("ERROR: not a subvolume")
	}
	f.files[services.InboundTokenPath("../../etc")] = evil
	resp := serve(t, f, `{"verb":"service.starting","token":"`+evil+`"}`)
	if resp.Error == "" {
		t.Fatal("a traversing service name was accepted")
	}
	for _, r := range f.runs {
		if r[0] == "btrfs" {
			t.Errorf("a traversing service name reached the volume: %v", f.runs)
		}
	}
}

// TestServeInboundSocketOverARealSocket drives the whole transport the way a container does:
// connect to a unix socket, write one line, read one line. The per-connection handler is covered
// above; this is the loop around it, and the thing it proves is that a second caller is served
// TestListenInboundOverARealSocket drives the whole transport the way a container does: connect
// to the socket, write one line, read one line. The per-request handler is covered above; this is
// the listener around it, and what it proves is that a second caller is served after the first
// rather than finding the channel gone.
func TestListenInboundOverARealSocket(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIARD_RUN_DIR", dir)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	f := ringExec()
	go func() { done <- ListenInbound(ctx, f) }()

	sock := filepath.Join(dir, "agent.sock")
	ask := func() string {
		var c net.Conn
		var err error
		for i := 0; i < 100; i++ { // the listener binds in another goroutine
			if c, err = net.Dial("unix", sock); err == nil {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer c.Close()
		if _, err := c.Write([]byte(`{"verb":"service.starting","token":"` + haToken + `"}` + "\n")); err != nil {
			t.Fatalf("write: %v", err)
		}
		line, err := bufio.NewReader(c).ReadString('\n')
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		return line
	}
	if first := ask(); strings.Contains(first, `"error"`) {
		t.Fatalf("first request errored: %s", first)
	}
	// THE SECOND CALLER IS THE POINT. A listener that served one connection and stopped would
	// leave the next service start with nothing to talk to, silently.
	if second := ask(); second == "" {
		t.Fatal("a second caller got nothing")
	}
	cancel()
	if err := <-done; err != nil {
		t.Errorf("listen returned %v, want a clean stop on cancellation", err)
	}
}

// TestListenInboundClearsAStaleSocket: /run is tmpfs and this process is the only thing that ever
// binds here, so a socket file left by a previous agent is a dead path rather than a listener --
// and bind would fail EADDRINUSE against it, leaving the channel down for the whole life of the
// agent with nothing but one log line to say so.
func TestListenInboundClearsAStaleSocket(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIARD_RUN_DIR", dir)
	sock := filepath.Join(dir, "agent.sock")
	stale, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	stale.Close() // leaves the file behind, as a killed agent would
	if _, err := os.Stat(sock); err != nil {
		t.Skip("this platform removes the socket file on close; nothing to clear")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- ListenInbound(ctx, ringExec()) }()
	var c net.Conn
	for i := 0; i < 100; i++ {
		if c, err = net.Dial("unix", sock); err == nil {
			c.Close()
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("the stale socket was not cleared: %v", err)
	}
	cancel()
	<-done
}

// TestInboundRefusesAShortTokenWithoutReadingAnything: a token too short to be one of ours is
// refused before the token directory is even listed. That keeps the cheap refusal cheap under
// exactly the conditions that produce it -- a caller probing the channel -- so a prober cannot
// make the agent walk a directory per request.
func TestInboundRefusesAShortTokenWithoutReadingAnything(t *testing.T) {
	for _, tok := range []string{"", "xyz"} {
		f := ringExec()
		resp := serve(t, f, `{"verb":"service.starting","token":"`+tok+`"}`)
		if resp.Error == "" {
			t.Errorf("token %q was accepted", tok)
		}
		if len(f.runs) != 0 {
			t.Errorf("token %q made the agent read something: %v", tok, f.runs)
		}
	}
}

// ringOf builds `n` plain members for a service, oldest first, one second apart and just clear of
// the rate limit.
//
// ⚠️ MINUTES AGO, NEVER HOURS: a take records a day event when the ring's first member is from an
// earlier local day, so a fixture hours in the past makes the prune tests depend on the time of day
// they run. Only the first two minutes after midnight remain.
func ringOf(service string, n int) []string {
	var out []string
	base := time.Now().Add(-2*time.Minute - time.Duration(n)*time.Second)
	for i := 0; i < n; i++ {
		m := quadlet.SnapshotMember(service, quadlet.TriggerStart, base.Add(time.Duration(i)*time.Second))
		out = append(out, strings.TrimPrefix(m, quadlet.SnapshotsDir))
	}
	return out
}

// withEvent puts an event on a member the fake already holds, as the take that found it would.
func withEvent(f *fakeExec, name string, kind quadlet.EventKind, ago time.Duration) {
	member := quadlet.SnapshotsDir + name
	var meta quadlet.SnapshotMeta
	_ = json.Unmarshal([]byte(f.files[quadlet.SnapshotSidecar(member)]), &meta)
	meta.Event = &quadlet.Event{Kind: kind, At: time.Now().Add(-ago), What: "something"}
	b, _ := json.Marshal(meta)
	f.files[quadlet.SnapshotSidecar(member)] = string(b)
}

func deleted(f *fakeExec) []string {
	var out []string
	for _, r := range f.runs {
		if len(r) > 3 && r[0] == "btrfs" && r[2] == "delete" {
			out = append(out, strings.TrimPrefix(r[3], quadlet.SnapshotsDir))
		}
	}
	return out
}

// TestRingReplacesSamplesThatAnchorNothing: an unbounded ring grows on every service start,
// costs its space on every diskful peer, and arrives as [B.155]'s failure by a new road. What
// bounds it is that a sample anchoring no event is replaced by the next one ([B.167]).
func TestRingReplacesSamplesThatAnchorNothing(t *testing.T) {
	existing := ringOf("home-assistant", 5)
	f := ringExec(existing...)
	serve(t, f, `{"verb":"service.starting","token":"`+haToken+`"}`)

	gone := deleted(f)
	if len(gone) != len(existing) {
		t.Fatalf("pruned %v, want every earlier sample -- the new member replaces them all", gone)
	}
	// THE OLDEST GO FIRST, so an interrupted prune has dropped the least useful ones.
	for i, name := range gone {
		if name != existing[i] {
			t.Errorf("pruned[%d] = %q, want %q -- the oldest first", i, name, existing[i])
		}
	}
	// A member and its sidecar go together: the take path refuses to leave a member without one,
	// so the delete path must not create that state either.
	for _, name := range gone {
		if !f.ran("rm", "-f", quadlet.SnapshotsDir+name+".json") {
			t.Errorf("%s was pruned but its sidecar was left behind", name)
		}
	}
}

// TestRingKeepsWhatAnchorsAnEvent: the member an event restores to is what the history exists
// for, however many starts come after it.
func TestRingKeepsWhatAnchorsAnEvent(t *testing.T) {
	ring := ringOf("home-assistant", 6)
	f := ringExec(ring...)
	withEvent(f, ring[0], quadlet.EventUpdate, 90*time.Hour)
	withEvent(f, ring[2], quadlet.EventChange, 2*time.Hour)
	serve(t, f, `{"verb":"service.starting","token":"`+haToken+`"}`)
	for _, name := range deleted(f) {
		if name == ring[0] || name == ring[2] {
			t.Fatalf("a restore point was pruned: %v", deleted(f))
		}
	}
	if len(deleted(f)) != 4 {
		t.Errorf("pruned %v, want the four plain samples", deleted(f))
	}
}

// TestRingLeavesAnotherServiceAlone: the prune is per service. One busy service must not evict
// another's history -- they share a directory and nothing but the name separates them.
func TestRingLeavesAnotherServiceAlone(t *testing.T) {
	mine := ringOf("home-assistant", 5)
	theirs := ringOf("mosquitto", 5)
	f := ringExec(append(mine, theirs...)...)
	serve(t, f, `{"verb":"service.starting","token":"`+haToken+`"}`)
	for _, name := range deleted(f) {
		if svc, _ := quadlet.SnapshotMemberService(name); svc != "home-assistant" {
			t.Errorf("pruning %s's ring deleted %s's member %q", "home-assistant", svc, name)
		}
	}
}

// TestRingPrunesAnEventThatHasAgedOut: the history's ages have to reach the disk, which means the
// prune has to be asked about a CLOCK.
func TestRingPrunesAnEventThatHasAgedOut(t *testing.T) {
	ring := ringOf("home-assistant", 2)
	f := ringExec(ring...)
	withEvent(f, ring[0], quadlet.EventChange, 9*24*time.Hour)
	withEvent(f, ring[1], quadlet.EventChange, 6*time.Hour)
	serve(t, f, `{"verb":"service.starting","token":"`+haToken+`"}`)
	got := deleted(f)
	if len(got) != 1 || got[0] != ring[0] {
		t.Fatalf("deleted %v, want just the nine-day-old event's point %q", got, ring[0])
	}
}

// ran reports whether the fake was asked to run exactly this argv.
func (f *fakeExec) ran(argv ...string) bool {
	for _, r := range f.runs {
		if len(r) != len(argv) {
			continue
		}
		same := true
		for i := range r {
			if r[i] != argv[i] {
				same = false
			}
		}
		if same {
			return true
		}
	}
	return false
}

// TestRateLimitReadsTheNewestMemberAcrossTriggers is the regression guard for a bug the injection
// round found, and the shape is worth keeping in mind.
//
// Members are `<service>-<trigger>-<stamp>`, so the TRIGGER sits between the service and the
// stamp and dominates any string comparison: every `-start-` member sorts before every
// `-upgrade-` one whatever their times. The ring read the lexically last member as the newest, so
// on any service that had ever been upgraded the rate limit compared against the UPGRADE point's
// age -- an old one here, which means it would have taken a member it should have skipped.
//
// Every earlier rate-limit test used one trigger, so all of them were blind to it.
func TestRateLimitReadsTheNewestMemberAcrossTriggers(t *testing.T) {
	// An upgrade point from long ago, and a start member from seconds ago. Ordered by NAME the
	// upgrade point is last; ordered by TIME the start member is.
	oldUpgrade := quadlet.SnapshotMember("home-assistant", quadlet.TriggerUpgrade, time.Now().Add(-72*time.Hour))
	recentStart := quadlet.SnapshotMember("home-assistant", quadlet.TriggerStart, time.Now().Add(-5*time.Second))
	f := ringExec(
		strings.TrimPrefix(oldUpgrade, quadlet.SnapshotsDir),
		strings.TrimPrefix(recentStart, quadlet.SnapshotsDir),
	)
	resp := serve(t, f, `{"verb":"service.starting","token":"`+haToken+`"}`)
	for _, r := range f.runs {
		if len(r) > 2 && r[1] == "subvolume" && r[2] == "snapshot" {
			t.Fatalf("a member was taken inside the floor; the newest was read as the 72h-old upgrade point: %v", f.runs)
		}
	}
	if !strings.Contains(resp.Detail, "still current") {
		t.Errorf("detail = %q, want the skip to name the recent member", resp.Detail)
	}
}

// TestListMembersSkipsWhatItCannotIdentify: the picker offers a household a rollback point, so an
// entry it cannot describe must not appear at all ([B.143]).
//
// The take path REMOVES a member it could not label, so a member with no readable sidecar here
// means something outside the ring made it -- a human's copy, an interrupted older build. Offering
// it would mean offering a restore whose code identity nobody knows.
func TestListMembersSkipsWhatItCannotIdentify(t *testing.T) {
	good := quadlet.SnapshotMember("home-assistant", quadlet.TriggerStart, time.Now().Add(-time.Hour))
	bare := quadlet.SnapshotMember("home-assistant", quadlet.TriggerStart, time.Now().Add(-2*time.Hour))
	garbled := quadlet.SnapshotMember("home-assistant", quadlet.TriggerUpgrade, time.Now().Add(-3*time.Hour))
	f := ringExec(
		strings.TrimPrefix(good, quadlet.SnapshotsDir),
		strings.TrimPrefix(bare, quadlet.SnapshotsDir),
		strings.TrimPrefix(garbled, quadlet.SnapshotsDir),
	)
	f.files[quadlet.SnapshotSidecar(good)] = `{"service":"home-assistant","trigger":"start","event":{"kind":"day","what":"Ran normally"}}`
	f.files[quadlet.SnapshotSidecar(garbled)] = `not json`
	delete(f.files, quadlet.SnapshotSidecar(bare)) // `bare` gets no sidecar at all.

	got, err := listMembers(context.Background(), f, "home-assistant")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("listed %d members, want only the one that can be described: %+v", len(got), got)
	}
	if got[0].Member != good {
		t.Errorf("member = %q, want %q", got[0].Member, good)
	}
	if got[0].Meta.Event == nil || got[0].Meta.Event.What != "Ran normally" {
		t.Errorf("event = %+v, want the sidecar's", got[0].Meta.Event)
	}
}

// TestListMembersIsOldestFirstAcrossTriggers: the picker shows a timeline, so the order is the
// answer -- and it must be by TIME, which member NAMES do not give (the trigger sits between the
// service and the stamp; see ringMembers).
func TestListMembersIsOldestFirstAcrossTriggers(t *testing.T) {
	newStart := quadlet.SnapshotMember("home-assistant", quadlet.TriggerStart, time.Now().Add(-time.Hour))
	oldUpgrade := quadlet.SnapshotMember("home-assistant", quadlet.TriggerUpgrade, time.Now().Add(-48*time.Hour))
	f := ringExec(
		strings.TrimPrefix(newStart, quadlet.SnapshotsDir),
		strings.TrimPrefix(oldUpgrade, quadlet.SnapshotsDir),
	)
	f.files[quadlet.SnapshotSidecar(newStart)] = `{"service":"home-assistant","trigger":"start"}`
	f.files[quadlet.SnapshotSidecar(oldUpgrade)] = `{"service":"home-assistant","trigger":"upgrade"}`

	got, err := listMembers(context.Background(), f, "home-assistant")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("listed %d members, want 2: %+v", len(got), got)
	}
	// By NAME the upgrade point sorts last; by TIME it is two days older and comes first.
	if got[0].Member != oldUpgrade {
		t.Errorf("first = %q, want the 48h-old upgrade point %q", got[0].Member, oldUpgrade)
	}
}

// A home-assistant ring for recordMember: the manifest names the container whose directory the
// detector reads, and each member gets a sidecar pinned to it.
const haManifest = `{"name":"home-assistant","version":"2026.7.1","containers":[{"name":"app",` +
	`"image":"ghcr.io/x/ha@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",` +
	`"mount":"/config","primary":true,"port":8123,"healthPath":"/"}]}`

func haMember(f *fakeExec, trigger quadlet.Trigger, at time.Time, automations string) (string, quadlet.SnapshotMeta) {
	member := quadlet.SnapshotMember("home-assistant", trigger, at)
	meta := quadlet.SnapshotMeta{Service: "home-assistant", Trigger: trigger, TakenAt: at, Manifest: haManifest}
	b, _ := json.Marshal(meta)
	f.files[quadlet.SnapshotSidecar(member)] = string(b)
	f.files[member+"/app/automations.yaml"] = automations
	return member, meta
}

func eventOn(t *testing.T, f *fakeExec, member string) *quadlet.Event {
	t.Helper()
	var meta quadlet.SnapshotMeta
	if err := json.Unmarshal([]byte(f.files[quadlet.SnapshotSidecar(member)]), &meta); err != nil {
		t.Fatalf("%s has no readable sidecar: %v", member, err)
	}
	return meta.Event
}

// recordRing is a ring holding the named members, with nothing else in it.
func recordRing(members ...string) *fakeExec {
	var names []string
	for _, m := range members {
		names = append(names, strings.TrimPrefix(m, quadlet.SnapshotsDir))
	}
	f := ringExec(names...)
	for k := range f.files { // ringExec's plain sidecars; haMember writes the real ones
		if strings.HasPrefix(k, quadlet.SnapshotsDir) {
			delete(f.files, k)
		}
	}
	return f
}

// TestADetectedChangeLandsOnThePointBeforeIt is the model ([B.167]): the change happened between
// two samples, so undoing it puts back the EARLIER one -- which is where its event is written.
// Written by replacement, so a power cut cannot leave a half-written sidecar.
func TestADetectedChangeLandsOnThePointBeforeIt(t *testing.T) {
	now := time.Now()
	prevAt, nextAt := now.Add(-time.Hour), now
	prev := quadlet.SnapshotMember("home-assistant", quadlet.TriggerStart, prevAt)
	next := quadlet.SnapshotMember("home-assistant", quadlet.TriggerStart, nextAt)
	f := recordRing(prev, next)
	haMember(f, quadlet.TriggerStart, prevAt, "- id: 1\n")
	_, meta := haMember(f, quadlet.TriggerStart, nextAt, "- id: 1\n- id: 2\n")

	recordMember(context.Background(), f, next, meta)
	ev := eventOn(t, f, prev)
	if ev == nil || ev.Kind != quadlet.EventChange || ev.What != "Changed automations" {
		t.Fatalf("the earlier member carries %+v, want the detected change", ev)
	}
	if !ev.At.Equal(nextAt) {
		t.Errorf("the event is at %s, want the sample that found it (%s)", ev.At, nextAt)
	}
	if eventOn(t, f, next) != nil {
		t.Error("the new member carries an event; it is the point of whatever happens NEXT")
	}
	if !f.ran("mv", "-f", quadlet.SnapshotSidecar(prev)+".tmp", quadlet.SnapshotSidecar(prev)) ||
		!f.ran("sync", "-f", quadlet.SnapshotSidecar(prev)) {
		t.Errorf("the sidecar was not replaced by a flushed rename: %v", f.runs)
	}
	if slices.Contains(deleted(f), strings.TrimPrefix(prev, quadlet.SnapshotsDir)) {
		t.Error("the point of the change was pruned")
	}
}

// TestNothingIsComparedAcrossAPerformedAct: an update migrates Home Assistant's files and an undo
// rewinds them, by design. Comparing across either would put a phantom "Changed ..." directly above
// the act -- so a baseline compares with nothing, and a point that already carries an act keeps it.
func TestNothingIsComparedAcrossAPerformedAct(t *testing.T) {
	now := time.Now()
	t.Run("baseline", func(t *testing.T) {
		prev := quadlet.SnapshotMember("home-assistant", quadlet.TriggerStart, now.Add(-time.Hour))
		next := quadlet.SnapshotMember("home-assistant", quadlet.TriggerRestoreAfter, now)
		f := recordRing(prev, next)
		haMember(f, quadlet.TriggerStart, now.Add(-time.Hour), "a")
		_, meta := haMember(f, quadlet.TriggerRestoreAfter, now, "b")
		recordMember(context.Background(), f, next, meta)
		if ev := eventOn(t, f, prev); ev != nil {
			t.Errorf("a baseline wrote %+v on the member before it", ev)
		}
	})
	t.Run("act", func(t *testing.T) {
		prev := quadlet.SnapshotMember("home-assistant", quadlet.TriggerUpgrade, now.Add(-time.Hour))
		next := quadlet.SnapshotMember("home-assistant", quadlet.TriggerStart, now)
		f := recordRing(prev, next)
		haMember(f, quadlet.TriggerUpgrade, now.Add(-time.Hour), "a")
		withEvent(f, strings.TrimPrefix(prev, quadlet.SnapshotsDir), quadlet.EventUpdate, time.Hour)
		_, meta := haMember(f, quadlet.TriggerStart, now, "b")
		recordMember(context.Background(), f, next, meta)
		if ev := eventOn(t, f, prev); ev == nil || ev.Kind != quadlet.EventUpdate {
			t.Errorf("the update's point now carries %+v; its event was overwritten", ev)
		}
	})
}

// TestTheFirstQuietSampleOfADayIsTheDayEvent: the safety net for everything the detectors miss --
// one restore point a day, and only when nothing else has happened since the last event.
func TestTheFirstQuietSampleOfADayIsTheDayEvent(t *testing.T) {
	now := time.Now()
	yesterday := now.Add(-30 * time.Hour) // a calendar day back in any zone
	prev := quadlet.SnapshotMember("home-assistant", quadlet.TriggerClock, yesterday)
	next := quadlet.SnapshotMember("home-assistant", quadlet.TriggerClock, now)
	f := recordRing(prev, next)
	haMember(f, quadlet.TriggerClock, yesterday, "same")
	_, meta := haMember(f, quadlet.TriggerClock, now, "same")
	recordMember(context.Background(), f, next, meta)
	if ev := eventOn(t, f, prev); ev == nil || ev.Kind != quadlet.EventDay {
		t.Fatalf("a quiet day left %+v, want the day event", ev)
	}

	// THE SAME DAY IS NOT A NEW ONE: a second quiet sample today finds today's event and adds none,
	// and the sample it replaces is pruned.
	later := now.Add(time.Second)
	third := quadlet.SnapshotMember("home-assistant", quadlet.TriggerStart, later)
	f.runFn(`btrfs`, []string{"subvolume", "snapshot", "-r", "x", third}) // into the fake's listing
	_, meta3 := haMember(f, quadlet.TriggerStart, later, "same")
	recordMember(context.Background(), f, third, meta3)
	if ev := eventOn(t, f, next); ev != nil {
		t.Errorf("a second quiet sample today wrote %+v", ev)
	}
	if !slices.Contains(deleted(f), strings.TrimPrefix(next, quadlet.SnapshotsDir)) {
		t.Errorf("the replaced sample was kept: %v", deleted(f))
	}
}

// TestTheInstallDayIsNotAQuietDay: a ring with no event yet measures from its first member, so the
// day a service is installed does not end in "Ran normally" an hour later.
func TestTheInstallDayIsNotAQuietDay(t *testing.T) {
	now := time.Now()
	prev := quadlet.SnapshotMember("home-assistant", quadlet.TriggerStart, now.Add(-time.Second))
	next := quadlet.SnapshotMember("home-assistant", quadlet.TriggerStart, now)
	f := recordRing(prev, next)
	haMember(f, quadlet.TriggerStart, now.Add(-time.Second), "same")
	_, meta := haMember(f, quadlet.TriggerStart, now, "same")
	recordMember(context.Background(), f, next, meta)
	if ev := eventOn(t, f, prev); ev != nil {
		t.Errorf("the install day produced %+v", ev)
	}
}

// TestTheHostsTakesAreRecordedToo: the upgrade point and the restore pair come in over data.member,
// not through the start path, and the history must not depend on which door a sample came through
// ([B.167]). Here the host's take replaces the plain sample before it.
func TestTheHostsTakesAreRecordedToo(t *testing.T) {
	old := ringOf("home-assistant", 1)
	f := ringExec(old...)
	g := dial(t, f)
	at := time.Now()
	member := quadlet.SnapshotMember("home-assistant", quadlet.TriggerUpgrade, at)
	sidecar, _ := json.Marshal(quadlet.SnapshotMeta{Service: "home-assistant", Trigger: quadlet.TriggerUpgrade, TakenAt: at,
		Event: &quadlet.Event{Kind: quadlet.EventUpdate, At: at, What: "Updated to 2026.9.0"}})
	if err := g.Snapshot(context.Background(), quadlet.DataRoot("home-assistant"), member, string(sidecar)); err != nil {
		t.Fatal(err)
	}
	if got := deleted(f); len(got) != 1 || got[0] != old[0] {
		t.Errorf("deleted %v, want the plain sample the host's take replaced", got)
	}
}
