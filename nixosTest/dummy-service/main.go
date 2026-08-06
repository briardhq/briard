// Command dummy-service is Briard's stateful test fixture (V0).
//
// It stands in for Home Assistant on the failover/upgrade path with HA's shape
// minus its quirks: it persists state to a fixed data dir (the DRBD volume in
// the unit), serves /healthz, and starts slowly so start-ordering and
// the health-gate get exercised. Its monotonic tick counter, fsync'd on every
// write, is what lets the failover tests assert the last record survives a
// takeover (the data crossed the DRBD link before the primary died).
//
// Decisions are baked in, not configured (CONTRIBUTING.md: no new flags): the data dir is the DRBD
// mount point and the listen address is fixed.
package main

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"
)

const (
	dataDir      = "/var/lib/briard/dummy" // the payload's data subvolume on the DRBD volume
	listenAddr   = ":8080"
	startupDelay = 3 * time.Second // "slow start" — enough to exercise the health-gate; kept short for test speed
	tickInterval = 1 * time.Second
	// PoisonTicks marks a broken upgrade's write (BRIARD_BROKEN). It is far above
	// any value the good payload reaches, so if it survives a rollback the data was NOT
	// restored from the pre-upgrade snapshot.
	poisonTicks = 999_999_999
)

// state is the persisted payload: a monotonic counter the failover tests read
// before and after a takeover to confirm committed data crossed the DRBD link.
type state struct {
	Ticks int64 `json:"ticks"`
}

func main() {
	loaded, err := loadState(dataDir)
	if err != nil {
		log.Fatalf("load state: %v", err)
	}

	var ticks atomic.Int64
	ticks.Store(loaded.Ticks)
	var ready atomic.Bool

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		if !ready.Load() {
			http.Error(w, "starting", http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/state", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(state{Ticks: ticks.Load()})
	})

	go func() {
		if err := http.ListenAndServe(listenAddr, mux); err != nil {
			log.Fatalf("serve: %v", err)
		}
	}()

	// Broken mode (BRIARD_BROKEN): the deliberately-bad upgrade. Stamp a poison
	// tick into the persisted state and never become ready, so a managed upgrade's
	// health-gate trips. A correct rollback restores the pre-upgrade snapshot, erasing
	// the poison — the data half of {code+data} recovery.
	if os.Getenv("BRIARD_BROKEN") == "1" {
		ticks.Store(poisonTicks)
		if err := saveState(dataDir, state{Ticks: poisonTicks}); err != nil {
			log.Fatalf("poison state: %v", err)
		}
		log.Printf("BROKEN: poisoned state to tick %d; /healthz stays 503", poisonTicks)
		select {} // never ready; hold the process so the container stays up but unhealthy
	}

	// Slow start: /healthz answers 503 until the workload is "up".
	log.Printf("starting from tick %d; ready in %s", loaded.Ticks, startupDelay)
	time.Sleep(startupDelay)
	ready.Store(true)
	log.Printf("ready on %s", listenAddr)

	// The workload's persistent writes — one durable tick per interval.
	for range time.Tick(tickInterval) {
		n := ticks.Add(1)
		if err := saveState(dataDir, state{Ticks: n}); err != nil {
			log.Fatalf("save state at tick %d: %v", n, err)
		}
	}
}

// loadState reads the persisted counter, treating a missing file as tick 0
// (first boot). Corrupt or unreadable state is fatal — never silently reset.
func loadState(dir string) (state, error) {
	b, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if errors.Is(err, os.ErrNotExist) {
		return state{}, nil
	}
	if err != nil {
		return state{}, err
	}
	var s state
	if err := json.Unmarshal(b, &s); err != nil {
		return state{}, err
	}
	return s, nil
}

// saveState writes the counter durably: temp file, fsync, atomic rename, then
// fsync the directory. The fsync is what makes "the last record survives" hold
// under real power loss and across the DRBD link on failover.
func saveState(dir string, s state) error {
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, "state.json.tmp")
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, filepath.Join(dir, "state.json")); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
