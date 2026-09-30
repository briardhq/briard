package host

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"briard.io/shared/telemetry"
	"testing"
)

func memoryCfg(t *testing.T, mb int) Config {
	t.Helper()
	return Config{MemoryMB: mb, AssignmentCache: filepath.Join(t.TempDir(), "assignment.json")}
}

// The boot size is what the guest has grown to, and never below the configured floor: a record
// smaller than the floor (the floor was raised since) or unreadable reads as the floor.
func TestGuestMemoryMBReadsTheRecordAboveTheFloor(t *testing.T) {
	cfg := memoryCfg(t, 1024)
	if got := cfg.guestMemoryMB(); got != 1024 {
		t.Fatalf("no record: %d, want the floor 1024", got)
	}
	for _, c := range []struct {
		record string
		want   int
	}{{"2048\n", 2048}, {"512\n", 1024}, {"junk", 1024}} {
		if err := os.WriteFile(cfg.guestMemoryPath(), []byte(c.record), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := cfg.guestMemoryMB(); got != c.want {
			t.Errorf("record %q: %d, want %d", c.record, got, c.want)
		}
	}
}

func TestGuestMemoryCeiling(t *testing.T) {
	for _, c := range []struct{ host, want int }{
		{0, 0},       // unreadable (a Windows host today): no growth, not a guess
		{2048, 0},    // nothing above the reserve
		{4096, 2048}, // the smallest host the report card admits
		{16384, 14336},
	} {
		if got := guestMemoryCeilingMB(c.host); got != c.want {
			t.Errorf("guestMemoryCeilingMB(%d) = %d, want %d", c.host, got, c.want)
		}
	}
}

// A step is added, THEN recorded, and the next launch boots at the recorded size.
func TestGrowGuestMemoryAddsThenRecords(t *testing.T) {
	cfg := memoryCfg(t, 1024)
	adds := 0
	add := func(context.Context) error { adds++; return nil }
	for _, want := range []int{1536, 2048} {
		got, err := cfg.growGuestMemory(context.Background(), 8192, add, t.Logf)
		if err != nil || got != want {
			t.Fatalf("grow = %d, %v; want %d", got, err, want)
		}
		if spec := cfg.guestMemoryMB(); spec != want {
			t.Errorf("after growing to %d the next launch boots at %d", want, spec)
		}
	}
	if adds != 2 {
		t.Errorf("adds = %d, want 2", adds)
	}
}

// Refused by QEMU: nothing is recorded, so the record never claims memory the guest does not have.
func TestGrowGuestMemoryRecordsNothingWhenTheAddFails(t *testing.T) {
	cfg := memoryCfg(t, 1024)
	_, err := cfg.growGuestMemory(context.Background(), 8192, func(context.Context) error { return errors.New("no slot") }, t.Logf)
	if err == nil {
		t.Fatal("a refused add must be an error")
	}
	if got := cfg.guestMemoryMB(); got != 1024 {
		t.Errorf("after a refused add the guest boots at %d, want 1024", got)
	}
}

// At the ceiling nothing is asked of QEMU at all.
func TestGrowGuestMemoryStopsAtTheCeiling(t *testing.T) {
	cfg := memoryCfg(t, 1536)
	called := false
	_, err := cfg.growGuestMemory(context.Background(), 3584, func(context.Context) error { called = true; return nil }, t.Logf)
	if !errors.Is(err, errMemoryCeiling) || called {
		t.Errorf("err = %v, add called = %v; want errMemoryCeiling and no add (1536 + 512 > 3584 - 2048)", err, called)
	}
}

// memSample builds one cycle's reading of a 1 GB guest.
func memSample(availMB int, psi float64) *telemetry.NodeResources {
	return &telemetry.NodeResources{MemTotalKB: 1000 << 10, MemAvailableKB: int64(availMB) << 10, MemPSISome60: psi}
}

// feed runs the grower over one sample every 10 s for d, and returns the offsets at which it said grow.
func feed(m *memoryGrower, t0 time.Time, d time.Duration, s *telemetry.NodeResources) []time.Duration {
	var fired []time.Duration
	for at := time.Duration(0); at <= d; at += 10 * time.Second {
		if m.decide(t0.Add(at), s) {
			fired = append(fired, at)
		}
	}
	return fired
}

func TestMemoryGrowerDecides(t *testing.T) {
	t0 := time.Unix(1_800_000_000, 0)
	for _, c := range []struct {
		name  string
		s     *telemetry.NodeResources
		d     time.Duration
		fired []time.Duration
	}{
		// A 1000 MB guest: 15 % is 150 MB, so the 256 MB floor is the threshold.
		{"healthy: plenty available, no pressure", memSample(600, 0), 20 * time.Minute, nil},
		{"page cache is not demand: available stays high however full", memSample(900, 0), 20 * time.Minute, nil},
		{"low, but not yet for five minutes", memSample(200, 0), 4*time.Minute + 50*time.Second, nil},
		// The window restarts at each step and runs through the cooldown, so a guest that stays
		// short grows once per window -- each one measured on the guest AFTER its last step.
		{"low for five minutes: grow, and again each window it stays low",
			memSample(200, 0), 16 * time.Minute, []time.Duration{5 * time.Minute, 10*time.Minute + 10*time.Second, 15*time.Minute + 20*time.Second}},
		// Pressure's window is shorter than the cooldown, so the cooldown is what paces it.
		{"sustained pressure grows at most once per cooldown",
			memSample(600, 50), 12 * time.Minute, []time.Duration{time.Minute, 6 * time.Minute, 11 * time.Minute}},
		{"just above the floor never grows", memSample(257, 0), 20 * time.Minute, nil},
		{"pressure for a minute grows even with memory available", memSample(600, 12), 2 * time.Minute, []time.Duration{time.Minute}},
		{"pressure at the threshold is not above it", memSample(600, 10), 5 * time.Minute, nil},
		{"an unread sample never grows", &telemetry.NodeResources{}, 20 * time.Minute, nil},
		{"no sample never grows", nil, 20 * time.Minute, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := feed(&memoryGrower{}, t0, c.d, c.s)
			if fmt.Sprint(got) != fmt.Sprint(c.fired) {
				t.Errorf("fired at %v, want %v", got, c.fired)
			}
		})
	}
}

// A condition must hold CONTINUOUSLY: one healthy sample, or one unread one, restarts its clock.
func TestMemoryGrowerNeedsAnUnbrokenWindow(t *testing.T) {
	t0 := time.Unix(1_800_000_000, 0)
	for _, gap := range []*telemetry.NodeResources{memSample(600, 0), {}} {
		m := &memoryGrower{}
		if feed(m, t0, 4*time.Minute, memSample(200, 0)) != nil {
			t.Fatal("grew inside the window")
		}
		m.decide(t0.Add(4*time.Minute+5*time.Second), gap)
		if got := feed(m, t0.Add(4*time.Minute+10*time.Second), 4*time.Minute, memSample(200, 0)); got != nil {
			t.Errorf("after a %+v sample the window did not restart: fired at %v", gap, got)
		}
	}
}
