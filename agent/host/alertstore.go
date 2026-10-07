package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"briard.io/shared/atomicfile"
	"briard.io/shared/notify"
)

// AlertStorePath is the node's alert store: the ONE record of every alert this host has raised,
// node-local like the mesh cache beside it. `briard alerts` reads it; nothing reads a log for an
// alert. It lives on the host because the host is the side that is up when the guest is not,
// and it is pushed to the guest only as a copy to display.
const AlertStorePath = stateDir + "/alerts.json"

// alertStoreCap bounds the history. Alerts are rare (a handful a month on a healthy node), so
// this is many months; the trim never drops a key's latest alert, because that is the record of
// whether the key is open.
const alertStoreCap = 200

// AlertRecord is one alert as the store keeps it: the alert, and when the host wrote it.
type AlertRecord struct {
	notify.Alert
	At time.Time `json:"at"`
}

// alertStore is where every host-side alert goes, and the ONLY place that knows whether a key is
// open. It is a notify.Notifier so every emitter keeps its one call, and it sits in front of
// delivery: the record is written first, then the alert is handed on -- delivery is the half
// that can be absent (no endpoint configured) or fail, so the record must never depend on it.
//
// EMITTERS ASSERT; THE STORE TRANSITIONS. An emitter says what it sees every time it looks
// (open while the condition holds, resolved when it does not) and keeps no memory of what it
// said before; the store compares each assertion with the key's latest record and writes only a
// change. That is what survives a restart: the emitter starts blank, says what it sees, and the
// store -- which did not restart -- knows whether that is news. A condition that cleared while
// the agent was down gets its resolved on the first reading; one that persisted gets nothing,
// not a second push.
type alertStore struct {
	path  string
	inner notify.Notifier // delivery, after the record; nil on a node with nobody to push to
	logf  func(string, ...any)

	mu     sync.Mutex
	recs   []AlertRecord
	loaded bool
	now    func() time.Time
}

func newAlertStore(path string, inner notify.Notifier, logf func(string, ...any)) *alertStore {
	return &alertStore{path: path, inner: inner, logf: logf, now: time.Now}
}

// Notify applies the transition rule, records the alert if it is a change, and only then
// delivers it. A dropped assertion is not an error: it is the common case on every tick.
//
//   - an Open is dropped when the key is already open WITH THE SAME TITLE; a different title is
//     the condition changing state within open (reduced redundancy -> no second copy) and is
//     recorded and pushed;
//   - a Resolved is dropped when the key is not open, so "resolve at every act that could end a
//     condition" costs nothing and needs no bookkeeping at the call site;
//   - an Event is always recorded.
func (s *alertStore) Notify(ctx context.Context, a notify.Alert) error {
	s.mu.Lock()
	s.load()
	last, has := s.latestLocked(a.Key)
	switch a.Kind {
	case notify.Open:
		if has && last.Kind == notify.Open && last.Title == a.Title {
			s.mu.Unlock()
			return nil
		}
	case notify.Resolved:
		if !has || last.Kind != notify.Open {
			s.mu.Unlock()
			return nil
		}
	}
	rec := AlertRecord{Alert: a, At: s.now()}
	s.recs = append(s.recs, rec)
	s.trimLocked()
	werr := s.writeLocked()
	s.mu.Unlock()
	// A log line for the person reading the journal next to everything else the agent said;
	// the record above is where the alert is kept.
	s.logf("alert %s %s: %s — %s", a.Kind, a.Key, a.Title, a.Body)
	if werr != nil {
		// The record is kept in memory (so this process still dedups) and delivery goes ahead:
		// a full disk is exactly when the disk alert must still reach somebody.
		s.logf("alerts: could not write %s: %v", s.path, werr)
	}
	if s.inner == nil {
		return nil
	}
	return s.inner.Notify(ctx, a)
}

// latest is the key's most recent record -- what an emitter's "prior state" is, when one needs
// to ask (most never do: they assert and let Notify decide).
func (s *alertStore) latest(key string) (AlertRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.load()
	return s.latestLocked(key)
}

func (s *alertStore) latestLocked(key string) (AlertRecord, bool) {
	for i := len(s.recs) - 1; i >= 0; i-- {
		if s.recs[i].Key == key {
			return s.recs[i], true
		}
	}
	return AlertRecord{}, false
}

// load reads the file once per process. An absent file is the shipped state (nothing has ever
// happened); an unreadable one is reported and treated the same, which can only cost a
// duplicate push, never a lost record -- the next write replaces it.
func (s *alertStore) load() {
	if s.loaded {
		return
	}
	s.loaded = true
	recs, err := ReadAlerts(s.path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		s.logf("alerts: could not read %s (%v); starting from an empty record", s.path, err)
	}
	s.recs = recs
}

// trimLocked keeps the last alertStoreCap records, plus any older record that is still its
// key's latest -- the open/closed state of a key is never trimmed away.
func (s *alertStore) trimLocked() {
	if len(s.recs) <= alertStoreCap {
		return
	}
	cut := len(s.recs) - alertStoreCap
	seen := map[string]bool{}
	for i := len(s.recs) - 1; i >= cut; i-- {
		seen[s.recs[i].Key] = true
	}
	kept := s.recs[:0:0]
	for i, r := range s.recs[:cut] {
		if seen[r.Key] {
			continue
		}
		// The latest of a key that has no record after the cut: keep it, and only it.
		latest := true
		for _, later := range s.recs[i+1 : cut] {
			if later.Key == r.Key {
				latest = false
				break
			}
		}
		if latest {
			kept = append(kept, r)
		}
	}
	s.recs = append(kept, s.recs[cut:]...)
}

func (s *alertStore) writeLocked() error {
	b, err := json.MarshalIndent(s.recs, "", " ")
	if err != nil {
		return err
	}
	return atomicfile.Write(s.path, b, 0o644, 0o755)
}

// ReadAlerts is the store as a reader sees it, oldest first. The caller tells an absent file
// (os.ErrNotExist: nothing has ever happened here) from one it could not read.
func ReadAlerts(path string) ([]AlertRecord, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var recs []AlertRecord
	if err := json.Unmarshal(b, &recs); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return recs, nil
}

// OpenAlerts is what is wrong right now: the latest record of every key whose latest record is
// an Open, in the order they opened. Derived, never stored.
func OpenAlerts(recs []AlertRecord) []AlertRecord {
	latest := map[string]int{}
	for i, r := range recs {
		latest[r.Key] = i
	}
	var open []AlertRecord
	for i, r := range recs {
		if latest[r.Key] == i && r.Kind == notify.Open {
			open = append(open, r)
		}
	}
	return open
}
