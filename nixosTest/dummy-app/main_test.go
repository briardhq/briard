package main

import "testing"

// An empty data dir reads as tick 0 (first boot); a save then load round-trips
// the counter. This is the persistence the failover tests depend on — the last
// committed tick must come back intact (V0).
func TestStateRoundTrip(t *testing.T) {
	dir := t.TempDir()

	got, err := loadState(dir)
	if err != nil {
		t.Fatalf("loadState empty: %v", err)
	}
	if got.Ticks != 0 {
		t.Fatalf("empty dir: got tick %d, want 0", got.Ticks)
	}

	if err := saveState(dir, state{Ticks: 42}); err != nil {
		t.Fatalf("saveState: %v", err)
	}

	got, err = loadState(dir)
	if err != nil {
		t.Fatalf("loadState after save: %v", err)
	}
	if got.Ticks != 42 {
		t.Fatalf("round-trip: got tick %d, want 42", got.Ticks)
	}
}
