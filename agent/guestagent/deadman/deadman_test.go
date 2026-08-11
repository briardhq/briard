package deadman

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

// memState is an in-memory StateStore for the driver tests.
type memState struct{ ep Episode }

func (m *memState) Load() Episode        { return m.ep }
func (m *memState) Save(e Episode) error { m.ep = e; return nil }

// harness wires a Monitor with fakes and records reboots + alerts.
type harness struct {
	mon       *Monitor
	now       time.Time
	peers     int
	connected int
	quorumErr error
	reboots   int
	alerts    []string
}

func newHarness() *harness {
	h := &harness{now: time.Unix(1_000_000, 0)}
	h.mon = &Monitor{
		Node: "n1", Base: DefaultDeadman, Window: 0, // window 0 -> deterministic (base) for the tests
		Now:    func() time.Time { return h.now },
		Quorum: func(context.Context) (int, int, error) { return h.peers, h.connected, h.quorumErr },
		Reboot: func(context.Context) error { h.reboots++; return nil },
		Alert:  func(level, s string) { h.alerts = append(h.alerts, "["+level+"] "+s) },
		State:  &memState{},
	}
	return h
}

func (h *harness) contactNow()             { h.mon.Contact() }
func (h *harness) advance(d time.Duration) { h.now = h.now.Add(d) }

// A single evaluate tick starting from a fresh (serving) state.
func (h *harness) tick(ep Episode, degraded bool) (Episode, bool) {
	return h.mon.evaluate(context.Background(), h.now, ep, degraded)
}

// Link alive: no reboot, no alert, whatever the quorum.
func TestMonitorServesWhileLinkAlive(t *testing.T) {
	h := newHarness()
	h.contactNow() // fresh contact
	h.peers, h.connected = 2, 2
	h.advance(1 * time.Minute) // < DefaultDeadman
	ep, degraded := h.tick(Episode{}, false)
	if h.reboots != 0 || len(h.alerts) != 0 || degraded || ep.Attempt != 0 {
		t.Errorf("link-alive tick disturbed state: reboots=%d alerts=%v degraded=%v ep=%+v", h.reboots, h.alerts, degraded, ep)
	}
}

// Deadman fires + quorum-safe (2 of 3 remain) → reboot, persisted, one alert on entry.
func TestMonitorRebootsWhenQuorumSafe(t *testing.T) {
	h := newHarness()
	h.contactNow()
	h.peers, h.connected = 2, 2 // 3-node cluster (peers+1), all connected -> leaving keeps 2 >= majority 2
	h.advance(DefaultDeadman + time.Minute)
	ep, degraded := h.tick(Episode{}, false)
	if h.reboots != 1 {
		t.Fatalf("quorum-safe deadman reboots = %d, want 1", h.reboots)
	}
	if !degraded || ep.Attempt != 1 || ep.LastReboot != h.now {
		t.Errorf("episode not advanced: degraded=%v ep=%+v", degraded, ep)
	}
	if len(h.alerts) != 1 {
		t.Errorf("want exactly one entry alert, got %v", h.alerts)
	}
}

// The load-bearing negative: deadman fires but rebooting would tip the Primary out of quorum
// (edge cluster: only 1 peer connected of 3) → NEVER reboot; hold + one alert.
func TestMonitorHoldsWhenQuorumCritical(t *testing.T) {
	h := newHarness()
	h.contactNow()
	h.peers, h.connected = 2, 1 // 3 configured, only 1 connected -> leaving leaves 1 < majority 2
	h.advance(DefaultDeadman + time.Minute)
	ep, degraded := h.tick(Episode{}, false)
	if h.reboots != 0 {
		t.Fatalf("quorum-critical deadman rebooted (%d) — must never self-outage", h.reboots)
	}
	if !degraded || ep.Attempt != 0 {
		t.Errorf("want degraded-holding with no reboot, got degraded=%v ep=%+v", degraded, ep)
	}
	if len(h.alerts) != 1 {
		t.Errorf("want one hold alert, got %v", h.alerts)
	}
}

// A lone node (solo/free) is quorum-critical → holds (keeps serving), never reboots.
func TestMonitorSoloHolds(t *testing.T) {
	h := newHarness()
	h.contactNow()
	h.peers, h.connected = 0, 0 // single node: total 1
	h.advance(DefaultDeadman + time.Minute)
	if _, degraded := h.tick(Episode{}, false); h.reboots != 0 || !degraded {
		t.Errorf("solo node: reboots=%d degraded=%v, want hold (0 reboots, degraded)", h.reboots, degraded)
	}
}

// An unreadable quorum probe is treated as NOT safe → hold, never reboot on unknown.
func TestMonitorHoldsOnQuorumError(t *testing.T) {
	h := newHarness()
	h.contactNow()
	h.quorumErr = fmt.Errorf("drbdsetup failed")
	h.advance(DefaultDeadman + time.Minute)
	if _, _ = h.tick(Episode{}, false); h.reboots != 0 {
		t.Errorf("rebooted on an unreadable quorum (%d) — must hold", h.reboots)
	}
}

// Backoff: after a reboot, a second tick within the backoff window holds (no double reboot);
// past it (with the link still dead) it reboots again — the burst→slow cadence.
func TestMonitorBacksOffBetweenReboots(t *testing.T) {
	h := newHarness()
	h.contactNow()
	h.peers, h.connected = 2, 2
	h.advance(DefaultDeadman + time.Minute)
	ep, degraded := h.tick(Episode{}, false) // reboot #1, attempt=1
	if h.reboots != 1 {
		t.Fatalf("first reboot missing")
	}
	// A tick 10s later: attempt 1 backoff is 30s -> still holding.
	h.advance(10 * time.Second)
	ep, degraded = h.tick(ep, degraded)
	if h.reboots != 1 {
		t.Errorf("rebooted within the backoff window (%d) — should hold", h.reboots)
	}
	// A tick well past the backoff: reboots again (attempt=2).
	h.advance(2 * time.Minute)
	ep, _ = h.tick(ep, degraded)
	if h.reboots != 2 || ep.Attempt != 2 {
		t.Errorf("did not reboot past the backoff: reboots=%d ep=%+v", h.reboots, ep)
	}
}

// Recovery: after being degraded, the host link returning → one recovery alert, episode reset.
func TestMonitorRecoversOnContact(t *testing.T) {
	h := newHarness()
	h.contactNow()
	h.peers, h.connected = 2, 2
	h.advance(DefaultDeadman + time.Minute)
	ep, degraded := h.tick(Episode{}, false) // reboot, degraded
	if !degraded {
		t.Fatal("expected degraded after a reboot")
	}
	// The agent reconnects: a fresh contact, and a tick within the deadman window.
	h.advance(30 * time.Second)
	h.contactNow()
	ep, degraded = h.tick(ep, degraded)
	if degraded || ep.Attempt != 0 {
		t.Errorf("did not recover on contact: degraded=%v ep=%+v", degraded, ep)
	}
	// The last alert is the recovery one -- and it is LEVELLED as one. The level is asserted
	// separately from the wording because a reader of the local trail (`briard alerts`) sorts by
	// it: this alert says the house is fine again, and shipping it as a warning would make the
	// all-clear indistinguishable from the trouble it clears.
	if len(h.alerts) == 0 || !contains(h.alerts[len(h.alerts)-1], "restored") {
		t.Errorf("missing recovery alert, alerts=%v", h.alerts)
	}
	if last := h.alerts[len(h.alerts)-1]; !contains(last, "["+LevelRecovered+"]") {
		t.Errorf("recovery alert = %q, want it levelled %s", last, LevelRecovered)
	}
	// ...and the degradation alerts before it are NOT recovered-level.
	for _, a := range h.alerts[:len(h.alerts)-1] {
		if !contains(a, "["+LevelWarning+"]") {
			t.Errorf("degradation alert = %q, want it levelled %s", a, LevelWarning)
		}
	}
}

// LastContact (the stamp-based source) arms only after the first contact: a zero time (no host
// contact yet this boot) → never fires, however long we've been ticking, so a slow bring-up can't
// trip it; once contact lands, staleness past T_deadman fires (quorum-safe here).
func TestMonitorLastContactArmsOnFirstContact(t *testing.T) {
	h := newHarness()
	h.peers, h.connected = 2, 2
	var stamp time.Time // zero = not yet contacted
	h.mon.LastContact = func() time.Time { return stamp }

	h.advance(time.Hour) // ages pass with no contact...
	if _, degraded := h.tick(Episode{}, false); h.reboots != 0 || degraded {
		t.Fatalf("fired before the first-ever contact (reboots=%d degraded=%v) — must stay disarmed", h.reboots, degraded)
	}

	// First contact lands, then goes stale past T_deadman → now it fires.
	stamp = h.now
	h.advance(DefaultDeadman + time.Minute)
	if _, _ = h.tick(Episode{}, false); h.reboots != 1 {
		t.Errorf("did not fire after contact went stale (reboots=%d)", h.reboots)
	}
}

// FileState round-trips the episode (the cross-reboot backoff persistence).
func TestFileStateRoundTrip(t *testing.T) {
	fs := FileState{Path: filepath.Join(t.TempDir(), "sub", "episode.json")}
	if fs.Load().Attempt != 0 {
		t.Error("absent file should load a zero episode")
	}
	want := Episode{Attempt: 4, LastReboot: time.Unix(1234, 0).UTC()}
	if err := fs.Save(want); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if got := fs.Load(); got.Attempt != want.Attempt || !got.LastReboot.Equal(want.LastReboot) {
		t.Errorf("round-trip = %+v, want %+v", got, want)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
