package host

import (
	"context"
	"errors"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"briard.io/agent/guest"
	"briard.io/agent/guestagent"
	"briard.io/agent/guestagent/deadman"
	"briard.io/shared/api"
	"briard.io/shared/notify"
)

// noGate is what the host learns when the guest's gate cannot be reached at all -- the zero
// verdict. It is the common case on a truly dead guest, and it must not block the reflex.
var noGate = gateVerdict{}

// allowGate / denyGate are fresh verdicts from a guest whose deadman is alive and evaluating.
var (
	allowGate = gateVerdict{reached: true, fresh: true, allowed: true}
	denyGate  = gateVerdict{reached: true, fresh: true, allowed: false}
)

// never is a sinceReboot meaning "we have not power-cycled this guest during this incident".
const never = time.Duration(1 << 62)

// The ladder reboots, waits out the cadence, and reboots again, indefinitely: an attempt bound
// would leave a node with no reflex at all and nothing reporting that fact.
func TestLadderKeepsRetryingAtTheCadence(t *testing.T) {
	r := &guestRecovery{cadence: time.Hour}

	if got := r.next(noGate, never, false); got != stepReboot {
		t.Fatalf("first expiry = %v, want stepReboot", got)
	}
	// Inside the cadence: hold, however many times the window closes.
	for i := range 5 {
		if got := r.next(noGate, 10*time.Minute, false); got != stepHold {
			t.Fatalf("expiry %d inside the cadence = %v, want stepHold", i+2, got)
		}
	}
	// Past it: reboot again. And again after that -- there is no attempt budget to run out.
	for i := range 20 {
		if got := r.next(noGate, 2*time.Hour, false); got != stepReboot {
			t.Fatalf("expiry %d past the cadence = %v, want stepReboot (the ladder must never stop)", i, got)
		}
	}
	if r.attempts != 21 {
		t.Errorf("attempts = %d, want 21 counted reboots", r.attempts)
	}
}

// The gate's one job: a guest that reports rebooting it would drop the rest of the cluster below
// quorum is not rebooted. The host cannot know this itself (DRBD's view lives in the guest), so
// without the gate it looks like any other silent guest and the host power-cycles a node its
// peers depend on.
func TestGateDenialWithholdsTheReboot(t *testing.T) {
	r := &guestRecovery{}
	if got := r.next(denyGate, never, false); got != stepHold {
		t.Fatalf("denied gate = %v, want stepHold", got)
	}
	if r.attempts != 0 {
		t.Errorf("a held expiry consumed an attempt (attempts=%d); only reboots may", r.attempts)
	}
	// It reboots the moment the guest stops objecting: nothing was spent, so no cadence to wait.
	if got := r.next(allowGate, never, false); got != stepReboot {
		t.Errorf("allowed gate after a denial = %v, want stepReboot", got)
	}
}

// Unreachable is not a refusal. A healthy guest answers the gate in microseconds, so failing to
// reach it means the guest is far past wedged, the case this rung exists for; reading it as a
// denial restores the wedged-forever outcome, silently, since a node that never reboots looks like
// one that never needed to. Likewise a stale verdict: a deadman whose evaluation loop wedged while
// its accept loop lives keeps answering about some other moment.
func TestUnreachableOrStaleGateDoesNotBlockTheReflex(t *testing.T) {
	for _, c := range []struct {
		name string
		g    gateVerdict
	}{
		{"unreachable", noGate},
		{"reached but never published a verdict", gateVerdict{reached: true}},
		{"reached, verdict too old to mean anything", gateVerdict{reached: true, allowed: false}},
	} {
		r := &guestRecovery{}
		if got := r.next(c.g, never, false); got != stepReboot {
			t.Errorf("%s: next = %v, want stepReboot (the gate may withhold, never authorise)", c.name, got)
		}
	}
}

// A stopped VM is not a silent one, and telling them apart is what makes the guest's own reboot
// work. With `-no-reboot` every guest restart (its deadman's graceful reboot, a panic, an OOM kill)
// ends the unit, so a dead channel is the host's only signal for both a stopped VM and a silent
// one. A down unit can never produce a socket (the window would be pure outage), has nothing to
// consult a gate about, and cannot outage a peer: relaunch at once. That closes the deadman's
// circularity: its reboot needs the agent to finish it, in seconds rather than after a ten-minute
// wait for a dead process's socket. A regression is silent (the node still recovers, an outage
// later), which is why the gate is asserted to be actively ignored, not merely unconsulted.
func TestAStoppedUnitIsRelaunchedAtOnceAndUngated(t *testing.T) {
	r := &guestRecovery{cadence: time.Hour}
	// Even a denial does not withhold a relaunch: the VM is already down.
	if got := r.next(denyGate, never, true); got != stepRelaunch {
		t.Fatalf("stopped unit with a denied gate = %v, want stepRelaunch (nothing left to protect)", got)
	}
	// And no window is waited out: immediate even with zero elapsed time.
	if got := r.next(noGate, 0, true); got != stepRelaunch {
		t.Errorf("stopped unit, no time elapsed = %v, want stepRelaunch", got)
	}
}

// The relaunch burst damps a crash loop without ever giving up: instant while attempts remain,
// then the same slow cadence as everything else. A guest that panics its way through boot exits
// (`-no-reboot`), so without this the agent would relaunch it as fast as QEMU could die.
func TestRelaunchBurstDampsACrashLoopButNeverStops(t *testing.T) {
	r := &guestRecovery{cadence: time.Hour}
	for i := range guestRelaunchBurst {
		if got := r.next(noGate, 0, true); got != stepRelaunch {
			t.Fatalf("relaunch %d = %v, want stepRelaunch (inside the burst)", i+1, got)
		}
	}
	// Burst spent and the VM died again immediately: slow down.
	if got := r.next(noGate, time.Minute, true); got != stepHold {
		t.Fatalf("relaunch past the burst, %v elapsed = %v, want stepHold", time.Minute, got)
	}
	// But it never stops -- past the cadence it tries again, and keeps trying.
	for i := range 10 {
		if got := r.next(noGate, 2*time.Hour, true); got != stepRelaunch {
			t.Fatalf("relaunch %d past the cadence = %v, want stepRelaunch (must never give up)", i, got)
		}
	}
}

// A channel up long enough to call the node recovered ends the incident, so the next failure
// alerts afresh rather than folding into an alert the owner already acted on. A short stretch must
// not: a guest that wedges again ninety seconds after converging is one guest failing repeatedly,
// and re-alerting each cycle is the flap the announce-once rule prevents.
func TestOnlyALongHealthyStretchEndsTheIncident(t *testing.T) {
	r := &guestRecovery{reset: time.Hour}
	r.next(noGate, never, false) // one reboot spent
	r.announced = true

	r.served(90 * time.Second)
	if r.attempts != 1 || !r.announced {
		t.Errorf("a 90s stretch ended the incident (attempts=%d announced=%v); it is the same incident",
			r.attempts, r.announced)
	}

	r.served(2 * time.Hour)
	if r.attempts != 0 || r.announced {
		t.Errorf("after a 2h healthy stretch: attempts=%d announced=%v, want 0/false", r.attempts, r.announced)
	}
}

// One trouble alert and one all-clear per incident, however often the guest recovers and fails
// inside it. An all-clear that cleared the trouble latch would re-arm it: a guest flapping every
// ten minutes would send a fresh pair every ten minutes forever. Only a healthy stretch long enough
// to end the incident re-arms either. A loop, because a single cycle passes with the latches
// coupled or apart; the defect shows from the second cycle on.
func TestAlertsFireOncePerIncidentAcrossRepeatedRecoveries(t *testing.T) {
	r := &guestRecovery{reset: 30 * time.Minute}
	troubles, clears := 0, 0
	// The product's latches, not a model of them: a model-based version of this test stays green
	// with the two latches coupled in the real code.
	announce := func() {
		if r.takeAnnounce() {
			troubles++
		}
	}
	resolve := func() {
		if r.takeClear() {
			clears++
		}
	}

	for range 5 {
		announce()
		resolve()
		r.served(90 * time.Second) // recovered, then failed again -- the same incident
	}
	if troubles != 1 || clears != 1 {
		t.Errorf("%d trouble + %d all-clear alerts across one incident, want 1 + 1", troubles, clears)
	}

	// A healthy stretch ends the incident and the next failure alerts afresh; otherwise the latch
	// would silence a node permanently after its first bad episode.
	r.served(time.Hour)
	announce()
	resolve()
	if troubles != 2 || clears != 2 {
		t.Errorf("after the incident ended: %d trouble + %d all-clear, want 2 + 2 (a new incident alerts)",
			troubles, clears)
	}
}

// The zero value is the shipped configuration: the fields exist so a test can shorten the ladder,
// and if a caller setting none got anything but the constants, the tests above would prove a
// ladder nobody runs.
func TestZeroValueIsTheShippedLadder(t *testing.T) {
	var r guestRecovery
	if got := r.waitFor(); got != guestRecoveryWindow {
		t.Errorf("waitFor() = %v, want %v", got, guestRecoveryWindow)
	}
	if got := r.cadenceFor(); got != guestRebootCadence {
		t.Errorf("cadenceFor() = %v, want %v", got, guestRebootCadence)
	}
	if got := r.floorFor(); got != guestRelaunchFloor {
		t.Errorf("floorFor() = %v, want %v", got, guestRelaunchFloor)
	}
	if got := r.resetAfter(); got != guestRecoveryReset {
		t.Errorf("resetAfter() = %v, want %v", got, guestRecoveryReset)
	}
}

// The burst is three tries, not three shots. A relaunch that fails in milliseconds (a unit name
// not yet free, a device the kernel still holds) must not spend the next attempt before its cause
// could have changed; without a floor all three land in one second and the node sits out the
// two-hour cadence with nothing left to try. The assertion is on the spread, not the floor alone:
// what matters is how long the budget takes to spend, whatever the burst and floor become.
func TestTheRelaunchBurstCannotBeSpentInOneSecond(t *testing.T) {
	if guestRelaunchFloor <= 0 {
		t.Fatalf("guestRelaunchFloor = %v: no floor at all, so a fast-failing launch burns the budget", guestRelaunchFloor)
	}
	if spread := time.Duration(guestRelaunchBurst) * guestRelaunchFloor; spread < time.Minute {
		t.Errorf("the whole relaunch budget is spent in %v (%d attempts x %v floor): too fast for "+
			"a transient condition to clear", spread, guestRelaunchBurst, guestRelaunchFloor)
	}
	// And well inside the cadence: a floor near it would quietly delete the fast-relaunch rung.
	if guestRelaunchFloor >= guestRebootCadence {
		t.Errorf("guestRelaunchFloor (%v) >= guestRebootCadence (%v): the immediate relaunch is no "+
			"longer immediate", guestRelaunchFloor, guestRebootCadence)
	}
}

// No ordinary channel bounce may reach this rung: the reconnect lands in about a second, and a
// guest agent that exits per connection is back within its RestartSec. A window of seconds would
// restart VMs for events the guest heals by itself.
func TestRecoveryWindowIsLongerThanAnyOrdinaryBounce(t *testing.T) {
	if guestRecoveryWindow < time.Minute {
		t.Errorf("guestRecoveryWindow = %v: too short to distinguish a wedged guest from a bounce", guestRecoveryWindow)
	}
	if guestRecoveryReset <= guestRecoveryWindow {
		t.Errorf("guestRecoveryReset (%v) <= guestRecoveryWindow (%v): a node could be declared "+
			"recovered without ever having outlived the wait that judges it",
			guestRecoveryReset, guestRecoveryWindow)
	}
}

// The two reflexes must not race, and the guest goes first. Its reflex is a graceful `systemctl
// reboot` (the promoter teardown demotes cleanly, DRBD records its quorum metadata on the way
// down); the host's only lever is a power cycle (ACPI first, which a wedged guest may not
// answer). Whoever fires first decides which the house gets, so the host only backstops a failed
// `systemctl reboot`. The two numbers live in different packages and processes, so a later
// tuning of either would silently flip the order with nothing else failing.
func TestTheGuestDeadmanFiresBeforeTheHostActs(t *testing.T) {
	// The latest the guest can fire: its base plus the whole jitter window.
	guestFires := deadman.DefaultDeadman + deadman.DefaultJitter
	// The earliest the host acts: the window closing, before it even tries a clean stop.
	hostActs := guestRecoveryWindow
	if hostActs <= guestFires {
		t.Errorf("host acts at %v but the guest deadman can fire as late as %v: the host would "+
			"power-cycle guests that were about to reboot themselves gracefully",
			hostActs, guestFires)
	}
	// No double-reboot race is guarded, deliberately: the deadman's graceful reboot ends the VM's
	// unit (`-no-reboot`), so to the host the guest stopped and was started, never "rebooted". The
	// two rungs cannot both act on one guest: whichever runs first changes what the other sees.
}

// bootExec is a guest that answers the handshake with a given boot id and refuses everything
// else -- enough to build the two Clients guestRebooted compares, over the real wire.
type bootExec struct{ boot string }

func (e *bootExec) Run(_ context.Context, name string, _ ...string) ([]byte, error) {
	return nil, errors.New("bootExec: refusing " + name)
}
func (e *bootExec) WriteFile(string, []byte) error { return errors.New("bootExec: refusing WriteFile") }
func (e *bootExec) Sethostname(string) error       { return errors.New("bootExec: refusing Sethostname") }
func (e *bootExec) ReadFile(string) ([]byte, error) {
	if e.boot == "" {
		return nil, os.ErrNotExist // a guest too old to report one
	}
	return []byte(e.boot + "\n"), nil
}

func dialBoot(t *testing.T, boot string) *guestagent.Client {
	t.Helper()
	cconn, sconn := net.Pipe()
	go guestagent.Serve(context.Background(), sconn, &bootExec{boot: boot})
	c := guestagent.NewClient(cconn)
	t.Cleanup(func() { c.Close() })
	if _, err := c.Handshake(context.Background()); err != nil {
		t.Fatal(err)
	}
	return c
}

// A guest rebooted underneath the agent comes back on a channel exactly like a bounced in-guest
// agent's, and the host must not resume observing a guest it never converged. Only the boot id
// separates the two, and it must be conclusive both ways: silence from an old guest is not a
// reboot, or every ordinary bounce would re-converge a healthy serving Primary.
func TestGuestRebootedNeedsBothBootIDs(t *testing.T) {
	for _, tc := range []struct {
		name     string
		was, now string
		want     bool
	}{
		{"a different boot is a reboot", "boot-a", "boot-b", true},
		{"the same boot is a bounced agent", "boot-a", "boot-a", false},
		{"a guest too old to answer is not evidence", "", "boot-b", false},
		{"a host that never learned one is not evidence", "boot-a", "", false},
		{"neither side knows", "", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Config{}
			u := newOSUpgrade(cfg, nil, dialBoot(t, tc.was), guest.Config{}, func(string, ...any) {})
			got, from, to := u.guestRebooted(dialBoot(t, tc.now))
			if got != tc.want {
				t.Errorf("guestRebooted(%q -> %q) = %v, want %v", tc.was, tc.now, got, tc.want)
			}
			if from != tc.was || to != tc.now {
				t.Errorf("reported %q -> %q, want %q -> %q", from, to, tc.was, tc.now)
			}
		})
	}
}

// A nil channel on either side is the agent's own startup or teardown, never a reboot.
func TestGuestRebootedIsNilSafe(t *testing.T) {
	u := newOSUpgrade(Config{}, nil, nil, guest.Config{}, func(string, ...any) {})
	if got, _, _ := u.guestRebooted(dialBoot(t, "boot-b")); got {
		t.Error("no prior channel must not read as a reboot")
	}
	u = newOSUpgrade(Config{}, nil, dialBoot(t, "boot-a"), guest.Config{}, func(string, ...any) {})
	if got, _, _ := u.guestRebooted(nil); got {
		t.Error("no fresh channel must not read as a reboot")
	}
}

// episodeStatus is a guest that hands over deadman episodes: a fakeStatus with the verb.
type episodeStatus struct {
	fakeStatus
	episodes []deadman.LastEpisode // handed over in order, each once
	asked    int
}

func (e *episodeStatus) SupportsDeadmanEpisode() bool { return true }
func (e *episodeStatus) DeadmanEpisode(context.Context) (deadman.LastEpisode, bool, error) {
	e.asked++
	if len(e.episodes) == 0 {
		return deadman.LastEpisode{}, false, nil
	}
	ep := e.episodes[0]
	e.episodes = e.episodes[1:]
	return ep, true, nil
}

// The guest reports, the host alerts. A stretch the guest spent without the host agent reaches
// the owner as one event from the host, saying how long and what the guest did; a guest with
// nothing to hand over, or one too old to have the verb, raises nothing.
func TestCollectDeadmanEpisodeAlertsOnce(t *testing.T) {
	ctx := context.Background()
	cfg := Config{Node: "n1"}
	fn := &fakeNotifier{}
	st := testStore(t, fn)
	since := time.Date(2026, 10, 7, 3, 0, 0, 0, time.UTC)
	g := &episodeStatus{episodes: []deadman.LastEpisode{{Since: since, Until: since.Add(47 * time.Minute), Reboots: 2}}}

	cfg.collectDeadmanEpisode(ctx, g, st, func(string, ...any) {})
	cfg.collectDeadmanEpisode(ctx, g, st, func(string, ...any) {}) // nothing left: quiet
	if g.asked != 2 || len(fn.alerts) != 1 {
		t.Fatalf("asked %d, alerted %d: %+v", g.asked, len(fn.alerts), fn.alerts)
	}
	al := fn.alerts[0]
	if al.Key != "host-unreachable" || al.Kind != notify.Event || al.Severity != notify.Warning {
		t.Errorf("alert = %+v, want the host-unreachable warning event", al)
	}
	for _, want := range []string{"n1", "47m", "restarted itself 2 times"} {
		if !strings.Contains(al.Body, want) {
			t.Errorf("body does not say %q: %q", want, al.Body)
		}
	}
	// A guest without the verb is not asked.
	cfg.collectDeadmanEpisode(ctx, &fakeStatus{}, st, func(string, ...any) {})
	if len(fn.alerts) != 1 {
		t.Errorf("an old guest produced an alert: %+v", fn.alerts)
	}
}

// The door answers while the ladder runs. Nothing else reads it while recover() does (observe is
// not running, and at bring-up never was), so the wait answers as the finish's does: a stop is
// taken, anything else refused with what the ladder is doing now (read afresh, so a wait that
// moves on says so) and the way to cut it short. Without this the CLI blocks until the agent
// exits and the tray reads a healthy process that says nothing. The test drives holdDoor with the
// closure recoverAnswering hands it, not recoverAnswering itself.
func TestTheDoorIsAnsweredWhileTheLadderRuns(t *testing.T) {
	ctx, down := context.WithCancelCause(context.Background())
	cfg := Config{down: down}
	local := make(chan localRequest)
	until := make(chan struct{})
	r := &guestRecovery{}
	r.say("the VM is not answering; Briard waits until %s for it before restarting it", "10:00")
	done := make(chan error, 1)
	go func() {
		done <- cfg.holdDoor(ctx, local, func() string { return r.status() + ". " + tryNow }, until, t.Logf)
	}()

	ask := func(kind string) api.DirectiveOutcome {
		t.Helper()
		rq := localRequest{d: api.Directive{ID: kind + "-1", Kind: kind}, resp: make(chan api.DirectiveOutcome, 1)}
		select {
		case local <- rq:
		case <-time.After(5 * time.Second):
			t.Fatal("the door did not take the request")
		}
		return <-rq.resp
	}
	o := ask(api.DirectiveStatus)
	if o.State != api.OutcomeFailed || !strings.Contains(o.Detail, "waits until 10:00") || !strings.Contains(o.Detail, "briard stop") {
		t.Errorf("status during the wait = %+v; want refused with the ladder's sentence and the way out", o)
	}
	r.say("the VM is not answering; Briard is restarting it")
	if o := ask(api.DirectiveDoctor); !strings.Contains(o.Detail, "is restarting it") {
		t.Errorf("the refusal must read the ladder's CURRENT sentence; got %q", o.Detail)
	}
	if o := ask(api.DirectiveStop); o.State != api.OutcomeDone {
		t.Errorf("a stop is the one directive the wait takes; got %+v", o)
	}
	if !errors.Is(context.Cause(ctx), ErrShutdown) {
		t.Errorf("the stop must bring the node down; cause = %v", context.Cause(ctx))
	}
	if err := <-done; err == nil {
		t.Error("holdDoor must return once the stop ended its context")
	}
}
