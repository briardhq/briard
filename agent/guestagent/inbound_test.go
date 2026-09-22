package guestagent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"

	"strings"
	"testing"
	"time"

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
		return nil, nil
	}
	return f
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

// ringOf builds `n` plain members for a service, oldest first, one minute apart.
func ringOf(service string, n int) []string {
	var out []string
	base := time.Now().Add(-time.Duration(n+10) * time.Hour)
	for i := 0; i < n; i++ {
		m := quadlet.SnapshotMember(service, quadlet.TriggerStart, base.Add(time.Duration(i)*time.Minute))
		out = append(out, strings.TrimPrefix(m, quadlet.SnapshotsDir))
	}
	return out
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

// TestRingIsBounded: an unbounded ring grows on every service start, costs its space on every
// diskful peer, and arrives as [B.155]'s failure by a new road. The bound is what keeps a
// household's own restarts from filling the volume they are stored on.
func TestRingIsBounded(t *testing.T) {
	existing := ringOf("home-assistant", quadlet.RetainPerWindow+3)
	f := ringExec(existing...)
	serve(t, f, `{"verb":"service.starting","token":"`+haToken+`"}`)

	gone := deleted(f)
	// Three over the bound, so three go. (The member the take just added is not in the fake's
	// listing, which is static -- what is under test is that the ring is brought TO the bound.)
	if len(gone) != 3 {
		t.Fatalf("pruned %d members, want 3 over the count of %d: %v", len(gone), quadlet.RetainPerWindow, gone)
	}
	// THE OLDEST GO, and the order is the property: a ring that evicted the newest would keep
	// history nobody wants and drop the state closest to whatever just went wrong.
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

// TestRingKeepsTitledMembers is the asymmetry that makes the bound safe ([B.143]).
//
// One number over everything evicts the pre-upgrade member within days of Home Assistant's
// ordinary restart cadence -- and "go back to the version before the update that broke my house"
// is the case the whole ring exists for. So the windows are per set: a day's restarts cannot reach
// a member the household's own action produced. Here the upgrade point is 90 hours old, PAST the
// three-day window every member shares and inside the titled week that only it has.
func TestRingKeepsTitledMembers(t *testing.T) {
	base := time.Now().Add(-90 * time.Hour)
	upgrade := strings.TrimPrefix(
		quadlet.SnapshotMember("home-assistant", quadlet.TriggerUpgrade, base),
		quadlet.SnapshotsDir,
	)
	// The restore point rides along because it is the one titled member with NO window of its own
	// beyond the titled week -- so it is what fails if that week stops covering titled members,
	// while the upgrade point would survive on its fortnight alone and prove nothing.
	undo := strings.TrimPrefix(
		quadlet.SnapshotMember("home-assistant", quadlet.TriggerRestoreBefore, base),
		quadlet.SnapshotsDir,
	)
	// Both are the OLDEST things in the ring, so a count that ignored triggers would take them first.
	f := ringExec(append([]string{upgrade, undo}, ringOf("home-assistant", quadlet.RetainPerWindow+3)...)...)
	serve(t, f, `{"verb":"service.starting","token":"`+haToken+`"}`)
	for _, name := range deleted(f) {
		if name == upgrade {
			t.Fatalf("the pre-upgrade member was evicted by the count: %v", deleted(f))
		}
		if name == undo {
			t.Fatalf("the restore point was evicted by the count: %v", deleted(f))
		}
	}
	if len(deleted(f)) == 0 {
		t.Error("nothing was pruned at all; the test proves nothing about the exemption")
	}
}

// TestRingLeavesAnotherServiceAlone: the bound is per service. One busy service must not evict
// another's history -- they share a directory and nothing but the name separates them.
func TestRingLeavesAnotherServiceAlone(t *testing.T) {
	mine := ringOf("home-assistant", quadlet.RetainPerWindow+3)
	theirs := ringOf("mosquitto", 5)
	f := ringExec(append(mine, theirs...)...)
	serve(t, f, `{"verb":"service.starting","token":"`+haToken+`"}`)
	for _, name := range deleted(f) {
		if svc, _ := quadlet.SnapshotMemberService(name); svc != "home-assistant" {
			t.Errorf("pruning %s's ring deleted %s's member %q", "home-assistant", svc, name)
		}
	}
}

// TestRingPrunesByAgeAndNotOnlyByCount: the ladder's ages have to reach the disk, which means the
// prune has to be asked about a CLOCK. A ring far under every count still loses what has aged out,
// and a ring that only ever counted would keep this member for months.
func TestRingPrunesByAgeAndNotOnlyByCount(t *testing.T) {
	member := func(ago time.Duration) string {
		return strings.TrimPrefix(
			quadlet.SnapshotMember("home-assistant", quadlet.TriggerStart, time.Now().Add(-ago)),
			quadlet.SnapshotsDir)
	}
	old, recent := member(9*24*time.Hour), member(6*time.Hour)
	f := ringExec(old, recent)
	serve(t, f, `{"verb":"service.starting","token":"`+haToken+`"}`)
	got := deleted(f)
	if len(got) != 1 || got[0] != old {
		t.Fatalf("deleted %v, want just the nine-day-old member %q", got, old)
	}
}

// TestRingUnderTheBoundPrunesNothing: the cheap case has to stay cheap, and a bound that deleted
// something on an ordinary start would be a bound nobody could reason about.
func TestRingUnderTheBoundPrunesNothing(t *testing.T) {
	f := ringExec(ringOf("home-assistant", 3)...)
	serve(t, f, `{"verb":"service.starting","token":"`+haToken+`"}`)
	if g := deleted(f); len(g) != 0 {
		t.Errorf("a ring of 3 inside every window pruned %v", g)
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
	f.files[quadlet.SnapshotSidecar(good)] = `{"service":"home-assistant","trigger":"start","title":"HA starting"}`
	f.files[quadlet.SnapshotSidecar(garbled)] = `not json`
	// `bare` gets no sidecar at all.

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
	if got[0].Meta.Title != "HA starting" {
		t.Errorf("title = %q, want the sidecar's", got[0].Meta.Title)
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
