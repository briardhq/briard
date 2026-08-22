package host

import (
	"context"
	"errors"
	"net"
	"os"
	"testing"
	"time"

	"briard.io/agent/guest"
	"briard.io/agent/guestagent"
	"briard.io/agent/guestagent/deadman"
)

// noGate is what the host learns when the guest's gate cannot be reached at all -- the zero
// verdict. It is the common case on a truly dead guest, and it must NOT block the reflex.
var noGate = gateVerdict{}

// allowGate / denyGate are fresh verdicts from a guest whose deadman is alive and evaluating.
// Uptime is well past guestFreshBoot so it is not that check doing the work.
var (
	allowGate = gateVerdict{reached: true, fresh: true, allowed: true}
	denyGate  = gateVerdict{reached: true, fresh: true, allowed: false}
)

// never is a sinceReboot meaning "we have not power-cycled this guest during this incident".
const never = time.Duration(1 << 62)

// The ladder reboots, waits out the cadence, and reboots again -- indefinitely. The old version
// stopped after K attempts; this asserts it does not, because "stop" is what left a node with no
// reflex at all and nothing reporting that fact.
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

// THE GATE'S ONE JOB: a guest that reports rebooting it would drop the rest of the cluster below
// quorum does not get rebooted. This is the knowledge the host cannot obtain for itself -- DRBD's
// view lives in the guest -- so without the gate this case is indistinguishable from any other
// silent guest and the host power-cycles a node its peers depend on.
func TestGateDenialWithholdsTheReboot(t *testing.T) {
	r := &guestRecovery{}
	if got := r.next(denyGate, never, false); got != stepHold {
		t.Fatalf("denied gate = %v, want stepHold", got)
	}
	if r.attempts != 0 {
		t.Errorf("a held expiry consumed an attempt (attempts=%d); only reboots may", r.attempts)
	}
	// The same ladder reboots the moment the guest stops objecting, with no cadence to wait out
	// (nothing was spent).
	if got := r.next(allowGate, never, false); got != stepReboot {
		t.Errorf("allowed gate after a denial = %v, want stepReboot", got)
	}
}

// UNREACHABLE IS NOT A REFUSAL. Failing to reach a gate that answers in microseconds from a
// healthy guest means the guest is far past wedged -- the case this rung exists for. Treating it
// as a denial would restore the wedged-forever outcome, and would do it silently, since a node
// that never reboots looks exactly like a node that never needed to.
//
// Same for a STALE verdict: a deadman whose evaluation loop wedged while its accept loop lives
// keeps answering, and what it answers describes some other moment.
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

// A STOPPED VM IS NOT A SILENT ONE, and telling them apart is what makes the guest's own reboot
// work. Because of `-no-reboot`, every guest restart -- its deadman's graceful reboot, a panic, an
// OOM kill -- ends the unit, so the host's only signal (a dead channel) is shared by two very
// different situations. When the unit is down no socket can ever appear, so the window would be
// pure outage; there is nothing running to consult a gate about; and a VM that is not running
// cannot outage a peer, so there is nothing the gate could protect. Relaunch at once.
//
// This is what closes the circularity in the guest deadman: its graceful reboot needs the agent to
// finish, and the agent now finishes it in seconds rather than after a ten-minute wait for a
// socket belonging to a dead process. A regression here is SILENT -- the node still recovers, just
// an outage later -- which is why the gate is asserted to be actively ignored rather than merely
// not consulted.
func TestAStoppedUnitIsRelaunchedAtOnceAndUngated(t *testing.T) {
	r := &guestRecovery{cadence: time.Hour}
	// Even a denial does not withhold a relaunch: the VM is already down, so going down is not
	// something the gate can still prevent.
	if got := r.next(denyGate, never, true); got != stepRelaunch {
		t.Fatalf("stopped unit with a denied gate = %v, want stepRelaunch (nothing left to protect)", got)
	}
	// And no window is waited out: immediate even with zero elapsed time.
	if got := r.next(noGate, 0, true); got != stepRelaunch {
		t.Errorf("stopped unit, no time elapsed = %v, want stepRelaunch", got)
	}
}

// The relaunch burst damps a crash loop WITHOUT ever giving up: instant while attempts remain,
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

// A channel that stayed up long enough to call the node recovered ends the incident, so the next
// failure announces itself afresh instead of being folded into an alert the owner already acted
// on. The short-stretch case must NOT clear it: a guest that reboots, converges and wedges again
// ninety seconds later is one guest failing repeatedly, and re-alerting on each cycle is the flap
// the announce-once rule exists to prevent.
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

// ONE trouble alert and ONE all-clear per incident, however many times the guest recovers and
// fails again inside it. The all-clear used to clear the trouble latch, which re-armed it: a guest
// flapping on a ten-minute period sent a fresh pair every ten minutes forever. Only a healthy
// stretch long enough to END the incident may re-arm either one.
//
// Written as a loop over recover-and-fail cycles because a single-cycle test passes under both the
// old and new behaviour -- the defect only shows from the second cycle on.
func TestAlertsFireOncePerIncidentAcrossRepeatedRecoveries(t *testing.T) {
	r := &guestRecovery{reset: 30 * time.Minute}
	troubles, clears := 0, 0
	// The PRODUCT's latches, not a re-implementation of them. A local model of the same condition
	// passes whatever announce()/resolved() actually do -- checked by mutation: re-coupling the two
	// latches in the real code left a model-based version of this test green.
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
		r.served(90 * time.Second) // recovered, then failed again -- the SAME incident
	}
	if troubles != 1 || clears != 1 {
		t.Errorf("%d trouble + %d all-clear alerts across one incident, want 1 + 1", troubles, clears)
	}

	// A genuinely healthy stretch ends the incident, and the next failure is announced afresh --
	// otherwise the latch would silence a node permanently after its first bad episode.
	r.served(time.Hour)
	announce()
	resolve()
	if troubles != 2 || clears != 2 {
		t.Errorf("after the incident ended: %d trouble + %d all-clear, want 2 + 2 (a new incident alerts)",
			troubles, clears)
	}
}

// The zero value is the shipped configuration. The fields exist so a test can shorten the ladder,
// and a caller that sets none of them must get the product's constants -- if this drifts, the
// unit tests above would be proving a ladder nobody runs.
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

// THE BURST IS THREE TRIES, NOT THREE SHOTS. A relaunch that fails in milliseconds -- a unit
// name that has not come free, a device the kernel still holds -- must not spend the next attempt
// before the condition it failed on could possibly have changed. In the field all three landed in
// the same logged second and the node then sat out the two-hour cadence with nothing left to try
// ([V3b.18]).
//
// The assertion is on the SPREAD rather than on the floor alone: what matters is how long the
// budget takes to spend, so raising the burst or dropping the floor both have to keep it real.
func TestTheRelaunchBurstCannotBeSpentInOneSecond(t *testing.T) {
	if guestRelaunchFloor <= 0 {
		t.Fatalf("guestRelaunchFloor = %v: no floor at all, so a fast-failing launch burns the budget", guestRelaunchFloor)
	}
	if spread := time.Duration(guestRelaunchBurst) * guestRelaunchFloor; spread < time.Minute {
		t.Errorf("the whole relaunch budget is spent in %v (%d attempts x %v floor): too fast for "+
			"a transient condition to clear", spread, guestRelaunchBurst, guestRelaunchFloor)
	}
	// And it stays well inside the cadence it falls back to -- a floor that approached the cadence
	// would quietly delete the fast-relaunch rung the ladder exists to have.
	if guestRelaunchFloor >= guestRebootCadence {
		t.Errorf("guestRelaunchFloor (%v) >= guestRebootCadence (%v): the immediate relaunch is no "+
			"longer immediate", guestRelaunchFloor, guestRebootCadence)
	}
}

// The window has to be long enough that no ordinary channel bounce reaches this rung. The
// reconnect that matters lands in about a second (B.23); a guest agent that exits per connection
// is back within its RestartSec. A window of seconds would make the host restart VMs for events
// the guest heals by itself.
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

// THE TWO REFLEXES MUST NOT RACE, and the ordering is GUEST FIRST.
//
// The guest's reflex is a graceful `systemctl reboot`: its promoter teardown demotes cleanly and
// DRBD records its quorum metadata on the way down. The host's only lever is a power cycle (it
// tries ACPI first, but a wedged guest is exactly the one that may not answer it). Whoever fires
// first decides which of those the house gets, so the guest gets its chance and the host
// backstops the case the guest side cannot cover -- one where `systemctl reboot` itself failed.
//
// This inverts what the ladder shipped with, and the inversion is the reason this test is worth
// its weight: the two numbers live in different packages, are read by different processes, and a
// future tuning of either would silently flip the design back with nothing else failing.
func TestTheGuestDeadmanFiresBeforeTheHostActs(t *testing.T) {
	// The LATEST the guest can fire: its base plus the whole jitter window.
	guestFires := deadman.DefaultDeadman + deadman.DefaultJitter
	// The EARLIEST the host acts: the window closing, before it even tries a clean stop.
	hostActs := guestRecoveryWindow
	if hostActs <= guestFires {
		t.Errorf("host acts at %v but the guest deadman can fire as late as %v: the host would "+
			"power-cycle guests that were about to reboot themselves gracefully",
			hostActs, guestFires)
	}
	// There is no double-reboot race left to guard against, and it is worth saying why rather than
	// leaving the absence to look like an oversight. When the guest deadman fires, its graceful
	// reboot ENDS the VM's unit (`-no-reboot`), so from the host's side the guest never "rebooted"
	// at all -- it stopped, and the host started it. The two rungs cannot both act on one guest:
	// whichever runs first changes the state the other observes.
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

// [B.102]: a guest that reboots underneath the agent comes back on a channel that looks exactly
// like a bounced in-guest agent's, and the host must not resume observing a guest it never
// converged. The boot id is the only thing that separates the two, and it must be conclusive in
// BOTH directions -- silence from an old guest is not a reboot, or every ordinary bounce would
// re-converge a healthy serving Primary.
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
