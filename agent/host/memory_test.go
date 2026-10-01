package host

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"briard.io/shared/model"
	"briard.io/shared/telemetry"
)

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

// fakeVM is a running guest's memory as QMP would report it.
type fakeVM struct {
	mb, adds int
	sizeErr  error
	addErr   error
}

func (v *fakeVM) size(context.Context) (int, error) { return v.mb, v.sizeErr }
func (v *fakeVM) add(context.Context) error {
	if v.addErr != nil {
		return v.addErr
	}
	v.adds++
	v.mb += 512
	return nil
}

// Growth starts from what the VM says it has -- not from the configured size -- so a guest that
// grew before an agent restart keeps growing from where it is.
func TestGrowGuestMemoryGrowsFromTheVMsOwnSize(t *testing.T) {
	vm := &fakeVM{mb: 1536}
	for _, want := range []int{2048, 2560} {
		got, err := growGuestMemory(context.Background(), 8192, vm.size, vm.add)
		if err != nil || got != want {
			t.Fatalf("grow = %d, %v; want %d", got, err, want)
		}
	}
	if vm.adds != 2 {
		t.Errorf("adds = %d, want 2", vm.adds)
	}
}

// Every launch boots at the configured size, however far the last one grew: a relaunch is when
// memory goes back to the host.
func TestEveryLaunchBootsAtTheConfiguredSize(t *testing.T) {
	cfg := Config{MemoryMB: 1024}
	vm := &fakeVM{mb: 1024}
	if _, err := growGuestMemory(context.Background(), 8192, vm.size, vm.add); err != nil {
		t.Fatal(err)
	}
	if got := cfg.guestSpec().MemoryMB; got != 1024 {
		t.Errorf("the next launch boots at %d MB, want the configured 1024", got)
	}
}

func TestGrowGuestMemoryFailures(t *testing.T) {
	// Refused by QEMU: the error says so, and the size reported is the one the guest still has.
	vm := &fakeVM{mb: 1024, addErr: errors.New("no slot")}
	if got, err := growGuestMemory(context.Background(), 8192, vm.size, vm.add); err == nil || got != 1024 {
		t.Errorf("refused add = %d, %v; want 1024 and an error", got, err)
	}
	// A size QEMU would not tell: nothing is added blind.
	vm = &fakeVM{mb: 1024, sizeErr: errors.New("qmp down")}
	if _, err := growGuestMemory(context.Background(), 8192, vm.size, vm.add); err == nil || vm.adds != 0 {
		t.Errorf("err = %v, adds = %d; want an error and no add", err, vm.adds)
	}
	// At the ceiling nothing is asked of QEMU at all.
	vm = &fakeVM{mb: 1536}
	if _, err := growGuestMemory(context.Background(), 3584, vm.size, vm.add); !errors.Is(err, errMemoryCeiling) || vm.adds != 0 {
		t.Errorf("err = %v, adds = %d; want errMemoryCeiling and no add (1536 + 512 > 3584 - 2048)", err, vm.adds)
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

func withServices(mb int, mins ...int) Config {
	cfg := Config{MemoryMB: mb}
	for i, m := range mins {
		cfg.Services = append(cfg.Services, model.ServiceSpec{Name: fmt.Sprintf("s%d", i), MinMemoryMB: m})
	}
	return cfg
}

// The boot size is the configured size, or more when the installed services' minimums (plus the
// system's share) say so -- and the next launch boots there, never at a size grown since.
func TestBootMemoryMB(t *testing.T) {
	for _, c := range []struct {
		cfg  Config
		want int
	}{
		{withServices(1024), 1024},           // nothing installed: the configured size
		{withServices(1024, 512), 1024},      // Home Assistant fits: 384 + 512 < 1024
		{withServices(1024, 512, 32), 1024},  // ...and so does a broker beside it
		{withServices(1024, 512, 400), 1296}, // minimums past the configured size win
		{withServices(2048, 512, 400), 2048}, // a configured size above them stands
		{withServices(1024, 0, 0), 1024},     // services that declare nothing count for nothing
	} {
		if got := c.cfg.bootMemoryMB(); got != c.want {
			t.Errorf("bootMemoryMB(%+v) = %d, want %d", c.cfg.Services, got, c.want)
		}
		if got := c.cfg.guestSpec().MemoryMB; got != c.want {
			t.Errorf("guestSpec boots at %d, want %d", got, c.want)
		}
	}
}

// An install counts the service it installs once: an upgrade replaces its own old minimum rather
// than adding to it.
func TestMemoryNeededCountsTheInstalledServiceOnce(t *testing.T) {
	cfg := withServices(1024, 512, 32) // s0, s1
	if got := cfg.memoryNeeded("s0", 700); got != 384+32+700 {
		t.Errorf("upgrading s0 to a 700 MB minimum needs %d, want %d", got, 384+32+700)
	}
	if got := cfg.memoryNeeded("new", 256); got != 384+512+32+256 {
		t.Errorf("adding a third needs %d, want %d", got, 384+512+32+256)
	}
}

func TestGrowGuestMemoryTo(t *testing.T) {
	// Short by more than one step: grown step by step until it holds the target.
	vm := &fakeVM{mb: 1024}
	if err := growGuestMemoryTo(context.Background(), 1900, 8192, vm.size, vm.add); err != nil || vm.mb != 2048 || vm.adds != 2 {
		t.Errorf("err = %v, size = %d, adds = %d; want 2048 after 2 adds", err, vm.mb, vm.adds)
	}
	// Already there: nothing is added.
	vm = &fakeVM{mb: 2048}
	if err := growGuestMemoryTo(context.Background(), 1900, 8192, vm.size, vm.add); err != nil || vm.adds != 0 {
		t.Errorf("err = %v, adds = %d; want nothing added", err, vm.adds)
	}
	// A target past the ceiling is refused up front -- not half-reached.
	vm = &fakeVM{mb: 1024}
	if err := growGuestMemoryTo(context.Background(), 3000, 4096, vm.size, vm.add); !errors.Is(err, errMemoryCeiling) || vm.adds != 0 {
		t.Errorf("err = %v, adds = %d; want errMemoryCeiling with nothing added", err, vm.adds)
	}
	// ...unless the guest already holds it (it booted there, above what the host could grow it to).
	vm = &fakeVM{mb: 3072}
	if err := growGuestMemoryTo(context.Background(), 3000, 4096, vm.size, vm.add); err != nil {
		t.Errorf("a guest already holding the target was refused: %v", err)
	}
}

// Growth is silent until it is unusual: one alert at 2.5x the boot size, or at the ceiling,
// whichever comes first -- once per launch, re-armed by the first size back under the threshold.
func TestMemoryAlerterSpeaksOncePerLaunch(t *testing.T) {
	a := &memoryAlerter{}
	say := func(size int, ceiling bool) string {
		al, ok := a.observe("n1", size, 1024, 6144, ceiling)
		if !ok {
			return ""
		}
		return al.Title
	}
	for _, size := range []int{1536, 2048} {
		if got := say(size, false); got != "" {
			t.Errorf("%d MB of a 1024 MB boot alerted %q; ordinary growth must stay silent", size, got)
		}
	}
	if got := say(2560, false); got != "Briard: memory growing unusually" {
		t.Errorf("2.5x the boot size: %q, want the unusual-growth alert", got)
	}
	if got := say(3072, false); got != "" {
		t.Errorf("a second alert in the same launch: %q", got)
	}
	if got := say(6144, true); got != "" {
		t.Errorf("the ceiling after the unusual-growth alert, same launch: %q; once per launch", got)
	}
	// A relaunch hands the memory back; the first growth after it is back under the threshold.
	if got := say(1536, false); got != "" {
		t.Errorf("growth after a relaunch alerted: %q", got)
	}
	// A small host reaches its ceiling before 2.5x: that is the alert, and it names the ceiling.
	if got := say(2048, true); got != "Briard: out of memory to give" {
		t.Errorf("the ceiling below 2.5x: %q, want the out-of-memory alert", got)
	}
}
