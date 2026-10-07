package deadman

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// memState is an in-memory StateStore for the driver tests.
type memState struct {
	ep   Episode
	last []LastEpisode
}

func (m *memState) Load() Episode                { return m.ep }
func (m *memState) Save(e Episode) error         { m.ep = e; return nil }
func (m *memState) SaveLast(l LastEpisode) error { m.last = append(m.last, l); return nil }

// harness wires a Monitor with fakes and records reboots + what it said.
type harness struct {
	mon       *Monitor
	now       time.Time
	peers     int
	connected int
	quorate   bool
	quorumErr error
	reboots   int
	said      []string // the owner-facing transitions: "unreachable" on entry, "restored" on exit
	state     *memState
	gate      *Gate
}

func newHarness() *harness {
	h := &harness{now: time.Unix(1_000_000, 0), quorate: true}
	h.gate = &Gate{
		Now: func() time.Time { return h.now },
	}
	h.mon = &Monitor{
		Node: "n1", Base: DefaultDeadman, Window: 0, // window 0 -> deterministic (base) for the tests
		Now: func() time.Time { return h.now },
		Fabric: func(context.Context) (Fabric, error) {
			return Fabric{Peers: h.peers, Connected: h.connected, Quorate: h.quorate}, h.quorumErr
		},
		Reboot: func(context.Context) error { h.reboots++; return nil },
		Logf: func(f string, a ...any) {
			s := fmt.Sprintf(f, a...)
			if contains(s, "unreachable") || contains(s, "restored") {
				h.said = append(h.said, s)
			}
		},
		State: &memState{},
		Gate:  h.gate,
	}
	h.state = h.mon.State.(*memState)
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
	if h.reboots != 0 || len(h.said) != 0 || degraded || ep.Attempt != 0 {
		t.Errorf("link-alive tick disturbed state: reboots=%d said=%v degraded=%v ep=%+v", h.reboots, h.said, degraded, ep)
	}
}

// Deadman fires + quorum-safe (2 of 3 remain) → reboot, persisted, said once on entry.
func TestMonitorRebootsWhenQuorumSafe(t *testing.T) {
	h := newHarness()
	h.contactNow()
	h.peers, h.connected = 2, 2 // 3-node cluster (peers+1), all connected -> leaving keeps 2 >= majority 2
	h.advance(DefaultDeadman + time.Minute)
	ep, degraded := h.tick(Episode{}, false)
	if h.reboots != 1 {
		t.Fatalf("quorum-safe deadman reboots = %d, want 1", h.reboots)
	}
	if !degraded || ep.Attempt != 1 || ep.LastReboot != h.now || ep.Since != h.now {
		t.Errorf("episode not advanced: degraded=%v ep=%+v", degraded, ep)
	}
	if len(h.said) != 1 {
		t.Errorf("want exactly one entry line, got %v", h.said)
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
	if !degraded || ep.Attempt != 0 || ep.Since != h.now {
		t.Errorf("want degraded-holding with no reboot and the episode's start, got degraded=%v ep=%+v", degraded, ep)
	}
	if len(h.said) != 1 {
		t.Errorf("want one hold line, got %v", h.said)
	}
	// Holding persists the episode's start: a reboot the host performs meanwhile must still end
	// ONE episode, dated from here, when the link returns.
	if h.state.ep.Since != h.now {
		t.Errorf("hold did not persist the episode start: %+v", h.state.ep)
	}
}

// A lone node (solo/free) REBOOTS. It used to hold, on the reasoning that a single node is its
// own quorum and departing loses it — which is true and irrelevant: there is no peer to outage,
// quorum is guaranteed the moment it comes back, and the data is on its own disk. Holding meant
// the reflex did nothing at all on the shape most briard installs have, leaving a wedged node
// wedged forever behind a single alert.
//
// This is the assertion that would silently rot if the gate were ever re-derived from the
// majority formula, so it names the whole reason rather than just the expected number.
func TestMonitorSoloReboots(t *testing.T) {
	h := newHarness()
	h.contactNow()
	h.peers, h.connected = 0, 0 // single node: no configured peers
	h.advance(DefaultDeadman + time.Minute)
	ep, degraded := h.tick(Episode{}, false)
	if h.reboots != 1 || !degraded || ep.Attempt != 1 {
		t.Errorf("solo node: reboots=%d degraded=%v ep=%+v, want one reboot (nobody else to outage)",
			h.reboots, degraded, ep)
	}
}

// The other clause the old gate lacked: a node that has ALREADY lost quorum is serving nobody
// (DRBD refuses its writes), so there is nothing to preserve by staying up and a reboot is free.
// The partitioned anchor — WAN out, peer and cloud witness both unreachable.
func TestMonitorRebootsWhenAlreadyOutOfQuorum(t *testing.T) {
	h := newHarness()
	h.contactNow()
	h.peers, h.connected, h.quorate = 2, 0, false // 3-voter cluster, fully partitioned
	h.advance(DefaultDeadman + time.Minute)
	if _, _ = h.tick(Episode{}, false); h.reboots != 1 {
		t.Errorf("out-of-quorum node: reboots=%d, want 1 (it is serving nobody)", h.reboots)
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

// Cadence: after a reboot, ticks within RebootCadence hold (no reboot loop); past it, with the
// link still dead, it reboots again — forever, never a hard stop.
func TestMonitorHoldsTheCadenceBetweenReboots(t *testing.T) {
	h := newHarness()
	h.contactNow()
	h.peers, h.connected = 2, 2
	h.advance(DefaultDeadman + time.Minute)
	ep, degraded := h.tick(Episode{}, false) // reboot #1, attempt=1
	if h.reboots != 1 {
		t.Fatalf("first reboot missing")
	}
	// Ticks across the whole cadence must not produce a second reboot. The old ramp let one
	// through at 30s; on a lone node — which now reaches this path at all — that is a second
	// service interruption half a minute after the first.
	for _, d := range []time.Duration{10 * time.Second, time.Minute, 30 * time.Minute} {
		h.advance(d)
		ep, degraded = h.tick(ep, degraded)
		if h.reboots != 1 {
			t.Fatalf("rebooted %v into the %v cadence (reboots=%d) — should hold", d, RebootCadence, h.reboots)
		}
	}
	// Past the cadence: reboots again (attempt=2).
	h.advance(RebootCadence)
	ep, _ = h.tick(ep, degraded)
	if h.reboots != 2 || ep.Attempt != 2 {
		t.Errorf("did not reboot past the cadence: reboots=%d ep=%+v", h.reboots, ep)
	}
}

// Recovery: after being degraded, the host link returning → the episode is handed to the host
// as a LastEpisode (what the guest did, from when to when) and the live one is reset.
func TestMonitorRecoversOnContact(t *testing.T) {
	h := newHarness()
	h.contactNow()
	h.peers, h.connected = 2, 2
	h.advance(DefaultDeadman + time.Minute)
	ep, degraded := h.tick(Episode{}, false) // reboot, degraded
	if !degraded {
		t.Fatal("expected degraded after a reboot")
	}
	since := h.now
	// The agent reconnects: a fresh contact, and a tick within the deadman window.
	h.advance(30 * time.Second)
	h.contactNow()
	ep, degraded = h.tick(ep, degraded)
	if degraded || ep.Attempt != 0 || !ep.Since.IsZero() {
		t.Errorf("did not recover on contact: degraded=%v ep=%+v", degraded, ep)
	}
	if len(h.said) == 0 || !contains(h.said[len(h.said)-1], "restored") {
		t.Errorf("missing the restored line, said=%v", h.said)
	}
	// THE RECORD FOR THE HOST. The deadman has nobody to tell (its only way out of the house is
	// the host agent, the thing that was down), so it leaves the episode for the host to turn
	// into the owner's alert when it collects it.
	if len(h.state.last) != 1 {
		t.Fatalf("recovery left %d episodes for the host, want 1: %+v", len(h.state.last), h.state.last)
	}
	if l := h.state.last[0]; l.Since != since || l.Until != h.now || l.Reboots != 1 {
		t.Errorf("handed-over episode = %+v, want since=%v until=%v reboots=1", l, since, h.now)
	}
	// Serving on is quiet: no second record.
	h.advance(time.Minute)
	h.contactNow()
	h.tick(ep, degraded)
	if len(h.state.last) != 1 {
		t.Errorf("a serving tick added an episode: %+v", h.state.last)
	}
}

// A held episode (never rebooted) is still an episode: it ends with a record for the host too,
// and across a deadman restart it is recognised as in progress from its persisted start.
func TestMonitorHeldEpisodeIsHandedOver(t *testing.T) {
	h := newHarness()
	h.contactNow()
	h.peers, h.connected = 2, 1 // quorum-critical: hold
	h.advance(DefaultDeadman + time.Minute)
	ep, degraded := h.tick(Episode{}, false)
	if !degraded || h.reboots != 0 {
		t.Fatalf("want a hold, got degraded=%v reboots=%d", degraded, h.reboots)
	}
	since := h.now
	h.advance(10 * time.Minute)
	h.contactNow()
	h.tick(ep, degraded)
	if len(h.state.last) != 1 || h.state.last[0].Reboots != 0 || h.state.last[0].Since != since {
		t.Errorf("held episode not handed over: %+v", h.state.last)
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
	want := Episode{Attempt: 4, LastReboot: time.Unix(1234, 0).UTC(), Since: time.Unix(1000, 0).UTC()}
	if err := fs.Save(want); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if got := fs.Load(); got.Attempt != want.Attempt || !got.LastReboot.Equal(want.LastReboot) || !got.Since.Equal(want.Since) {
		t.Errorf("round-trip = %+v, want %+v", got, want)
	}
	// The finished episode lands beside it, where the guest agent collects it.
	last := LastEpisode{Since: want.Since, Until: time.Unix(2000, 0).UTC(), Reboots: 4}
	if err := fs.SaveLast(last); err != nil {
		t.Fatalf("SaveLast: %v", err)
	}
	if fs.LastPath() != filepath.Join(filepath.Dir(fs.Path), "last-episode.json") {
		t.Errorf("LastPath = %s", fs.LastPath())
	}
	if _, err := os.Stat(fs.LastPath()); err != nil {
		t.Errorf("SaveLast wrote nothing at %s: %v", fs.LastPath(), err)
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
