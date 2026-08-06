package deadman

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"
)

// Episode is the persisted backoff state for one degraded stretch. It survives a deadman reboot
// (stored node-locally, NEVER on the replicated DRBD volume) so the backoff GROWS across reboots
// instead of resetting to the burst cadence on every boot. Reset to zero the moment the host
// agent's link returns.
type Episode struct {
	Attempt    int       `json:"attempt"`
	LastReboot time.Time `json:"last_reboot"`
}

// StateStore persists the Episode across a reboot. FileState is the production impl; tests inject
// an in-memory one.
type StateStore interface {
	Load() Episode
	Save(Episode) error
}

// Monitor is the guest-side deadman driver. It watches the host-agent link
// (refreshed by Contact on every served request) and, on prolonged silence, reboots — but only
// when Decide says it's quorum-safe. Every side-effecting dependency is injected, so Run is
// exercised in unit tests without DRBD, systemd, or a real reboot.
//
// It lives in-process with the (per-connection) guest agent: Run is started once per agent
// lifetime and keeps evaluating while the serve loop blocks waiting for the host — so a silent-
// but-connected host (stamp not refreshed) and a disconnected dead host (agent waits, no
// requests) are both caught. lastContact resets per lifetime, which is correct: the agent only
// (re)starts right after a disconnect (recent contact) or at boot.
type Monitor struct {
	Node   string        // jitter seed (this node's name) — a stable per-node spread
	Base   time.Duration // T_deadman base (DefaultDeadman)
	Window time.Duration // jitter window (DefaultJitter)
	Tick   time.Duration // evaluation cadence; 0 -> 15s

	Now    func() time.Time                                      // injectable clock
	Quorum func(context.Context) (peers, connected int, e error) // read DRBD quorum locally
	Reboot func(context.Context) error                           // GRACEFUL reboot (systemctl reboot)
	Alert  func(string)                                          // owner-facing degradation alert
	Logf   func(string, ...any)                                  // operational log
	State  StateStore                                            // persisted backoff across a reboot

	// LastContact, when set, is the source of truth for the last host-agent contact — a stamp file
	// the (per-connection, crash-loop-prone) guest agent touches on each request, read by this
	// SEPARATE long-running deadman process. It returns the zero time before the first-ever contact
	// (still booting / bringing up) → the deadman stays disarmed until the host has talked once, so a
	// slow bring-up can't trip it. When nil, the in-process Contact() stamp is used (unit tests).
	LastContact func() time.Time

	mu          sync.Mutex
	lastContact time.Time
}

// Contact records that the host agent just talked to us — call it on every served request.
func (m *Monitor) Contact() {
	m.mu.Lock()
	m.lastContact = m.now()
	m.mu.Unlock()
}

func (m *Monitor) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

func (m *Monitor) logf(format string, a ...any) {
	if m.Logf != nil {
		m.Logf(format, a...)
	}
}

func (m *Monitor) alert(msg string) {
	if m.Alert != nil {
		m.Alert(msg)
	}
}

func (m *Monitor) sinceContact(now time.Time) time.Duration {
	if m.LastContact != nil {
		lc := m.LastContact()
		if lc.IsZero() {
			return 0 // no contact yet this boot → not armed (still bringing up); never fire
		}
		return now.Sub(lc)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return now.Sub(m.lastContact)
}

// Run evaluates the deadman on a ticker until ctx is done. It baselines the contact stamp at
// start (so a routine bring-up has time to connect before the deadman counts) and loads any
// persisted backoff episode (so a cross-reboot degraded stretch keeps growing its cadence).
func (m *Monitor) Run(ctx context.Context) error {
	if m.LastContact == nil {
		m.Contact() // in-process mode: baseline so we don't fire before the host connects this lifetime
	}
	ep := Episode{}
	if m.State != nil {
		ep = m.State.Load()
	}
	tick := m.Tick
	if tick == 0 {
		tick = 15 * time.Second
	}
	degraded := ep.Attempt > 0 // a reboot already happened this episode (loaded from disk)
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
		ep, degraded = m.evaluate(ctx, m.now(), ep, degraded)
	}
}

// Evaluate runs one deadman tick: read quorum, Decide, and act (reboot / alert / persist). It
// returns the updated episode + degraded flag. Pure of timing (now is passed in), so the whole
// loop behaviour is unit-testable without a ticker or a real clock.
func (m *Monitor) evaluate(ctx context.Context, now time.Time, ep Episode, degraded bool) (Episode, bool) {
	// Quorum is read only to gate a reboot; a failed read is treated as NOT safe (never reboot on
	// unknown quorum). It's irrelevant while the link is alive (Decide short-circuits to Serve),
	// so a probe error during normal operation is harmless.
	peers, connected, qerr := 0, 0, error(nil)
	if m.Quorum != nil {
		peers, connected, qerr = m.Quorum(ctx)
	}
	quorumSafe := qerr == nil && QuorumSafe(peers+1, connected)

	eff := EffectiveDeadman(m.Base, m.Window, fmt.Sprintf("%s:%d", m.Node, ep.Attempt))
	sinceReboot := time.Duration(1 << 62) // "never" until a reboot stamps it
	if !ep.LastReboot.IsZero() {
		sinceReboot = now.Sub(ep.LastReboot)
	}

	switch Decide(Input{
		SinceContact: m.sinceContact(now),
		Deadman:      eff,
		QuorumSafe:   quorumSafe,
		SinceReboot:  sinceReboot,
		Attempt:      ep.Attempt,
	}) {
	case Serve:
		if degraded {
			m.alert(fmt.Sprintf("briard %s: host-agent link restored — resuming normal operation", m.Node))
			ep = Episode{}
			if m.State != nil {
				_ = m.State.Save(ep)
			}
			degraded = false
		}
	case Hold:
		if !degraded {
			reason := "quorum-critical (rebooting would drop a serving peer)"
			if qerr != nil {
				reason = fmt.Sprintf("quorum unreadable (%v)", qerr)
			}
			m.alert(fmt.Sprintf("briard %s: host agent unreachable — degraded, holding (%s)", m.Node, reason))
			degraded = true
		}
	case Reboot:
		ep.Attempt++
		ep.LastReboot = now
		if m.State != nil {
			if err := m.State.Save(ep); err != nil {
				m.logf("deadman: persist episode failed: %v", err) // proceed; worst case the backoff resets
			}
		}
		if !degraded {
			m.alert(fmt.Sprintf("briard %s: host agent unreachable — rebooting to recover / fail over", m.Node))
		}
		degraded = true
		m.logf("deadman: rebooting (attempt %d, quorum-safe)", ep.Attempt)
		if m.Reboot != nil {
			if err := m.Reboot(ctx); err != nil {
				m.logf("deadman: reboot request failed: %v", err) // backoff holds the next attempt
			}
		}
	}
	return ep, degraded
}

// FileState persists the Episode as JSON at Path — a node-local path OUTSIDE the replicated DRBD
// volume, so it survives a reboot without being shared/overwritten across nodes.
type FileState struct{ Path string }

func (f FileState) Load() Episode {
	var ep Episode
	b, err := os.ReadFile(f.Path)
	if err != nil {
		return Episode{} // absent / unreadable -> a fresh episode (fresh boot or first run)
	}
	_ = json.Unmarshal(b, &ep) // a corrupt file -> zero episode; the reboot loop self-limits via backoff
	return ep
}

func (f FileState) Save(ep Episode) error {
	if err := os.MkdirAll(dir(f.Path), 0o755); err != nil {
		return err
	}
	b, err := json.Marshal(ep)
	if err != nil {
		return err
	}
	return os.WriteFile(f.Path, b, 0o644)
}

func dir(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' {
			return path[:i]
		}
	}
	return "."
}
