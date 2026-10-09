package host

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"briard.io/agent/platform"
	"briard.io/agent/reportcard"
	"briard.io/shared/notify"
)

// ⚠️ THE AGENT MAKES WHAT IT WAS TOLD ABOUT and nothing else: a harness that names no path for a
// disk is saying it has none, and a path invented here would hand qemu a `-drive` for a file
// nobody made.
func TestProvisionDisksMakesOnlyWhatItWasToldAbout(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{DataDisk: filepath.Join(dir, "data.img"), DataSize: "1G"} // no StateDisk
	if err := cfg.provisionDisks(func(string, ...any) {}); err != nil {
		t.Fatalf("provisionDisks: %v", err)
	}
	if fi, err := os.Stat(cfg.DataDisk); err != nil || fi.Size() != 1<<30 {
		t.Errorf("the data volume is %v (%v), want 1 GiB", fi, err)
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 1 {
		names := make([]string, 0, len(ents))
		for _, e := range ents {
			names = append(names, e.Name())
		}
		t.Errorf("provisioning made %v, want only the data volume it was given a path for", names)
	}
	// And told about neither: nothing at all.
	empty := t.TempDir()
	if err := (Config{}).provisionDisks(func(string, ...any) {}); err != nil {
		t.Fatalf("provisionDisks with no paths: %v", err)
	}
	if ents, _ := os.ReadDir(empty); len(ents) != 0 {
		t.Errorf("a config naming no disks still made %d file(s)", len(ents))
	}
}

// The size is the one value here the agent defaults, so a config.env that names a bad one must
// refuse rather than silently pick 4G -- a data volume is not a thing to guess the size of.
func TestProvisionDisksRefusesAnUnreadableSize(t *testing.T) {
	cfg := Config{DataDisk: filepath.Join(t.TempDir(), "data.img"), DataSize: "4.5 gigs"}
	err := cfg.provisionDisks(func(string, ...any) {})
	if err == nil {
		t.Fatal("a malformed DATA_SIZE was accepted")
	}
	if _, statErr := os.Stat(cfg.DataDisk); !os.IsNotExist(statErr) {
		t.Error("a volume was created despite the refusal")
	}
}

func TestParseSize(t *testing.T) {
	for _, c := range []struct {
		in   string
		want int64
		ok   bool
	}{
		{"4G", 4 << 30, true},
		{"1g", 1 << 30, true},
		{"64G", 64 << 30, true},
		{"4", 0, false},    // no unit: ambiguous, and this is a disk size
		{"4M", 0, false},   // whole GiB only -- the installer's knob never took anything else
		{"4.5G", 0, false}, // not a whole number
		{"0G", 0, false},   // a zero-byte data volume is not a smaller one, it is a broken node
		{"-4G", 0, false},  // likewise
		{"", 0, false},     // unset reaches here only if somebody wrote DATA_SIZE= on purpose
		{"lots", 0, false}, //
	} {
		got, err := parseSize(c.in)
		if c.ok && (err != nil || got != c.want) {
			t.Errorf("parseSize(%q) = (%d, %v), want (%d, nil)", c.in, got, err, c.want)
		}
		if !c.ok && err == nil {
			t.Errorf("parseSize(%q) = (%d, nil), want a refusal", c.in, got)
		}
	}
}

// The grow decision, exhaustively over its four inputs: no grow when the guest already has the
// space, whole GiB padded for what ext4 keeps when it does, and a refusal -- never a partial grow --
// when the host cannot pay and keep its own reserve. An unreadable host (0) is not a refusal: the
// allocation itself is the check then.
func TestStateDiskGrowth(t *testing.T) {
	const gib = int64(1) << 30
	for _, c := range []struct {
		name                          string
		guestFree, need, file, hostFr int64
		want                          int64
		full                          bool
	}{
		{"already has it", 5 * gib, 4 * gib, 6 * gib, 100 * gib, 0, false},
		{"exactly has it", 4 * gib, 4 * gib, 6 * gib, 100 * gib, 0, false},
		{"short by a little: one GiB", 3*gib + gib/2, 4 * gib, 2 * gib, 100 * gib, 3 * gib, false},
		{"short by a GiB: padded past it", 3 * gib, 4 * gib, 2 * gib, 100 * gib, 4 * gib, false},
		{"first Home Assistant install on a fresh disk", gib / 2, 2490e6 + 622e6 + gib, gib, 100 * gib, 5 * gib, false},
		{"host cannot keep its reserve", gib / 2, 4 * gib, gib, 5 * gib, 0, true},
		{"host free unreadable: the allocation decides", gib / 2, 4 * gib, gib, 0, 5 * gib, false},
	} {
		got, err := stateDiskGrowth(c.guestFree, c.need, c.file, c.hostFr)
		if c.full != errors.Is(err, errHostDiskFull) {
			t.Errorf("%s: err = %v, want host-full=%v", c.name, err, c.full)
		}
		if got != c.want {
			t.Errorf("%s: size = %d GiB+%d, want %d GiB+%d", c.name, got/gib, got%gib, c.want/gib, c.want%gib)
		}
	}
}

type fakeGrower struct {
	free  int64
	steps []string
}

func (f *fakeGrower) StorageFree(context.Context) (int64, int64, error) {
	f.steps = append(f.steps, "free")
	return f.free, 0, nil
}
func (f *fakeGrower) StorageGrow(_ context.Context, size int64) error {
	f.steps = append(f.steps, fmt.Sprintf("guest grow %d", size))
	return nil
}

// The order is the whole mechanism: the FILE grows (thick) before QEMU is told, and QEMU before the
// guest fills it -- a guest resizing into a disk the VM has not grown finds nothing, and a VM grown
// past its file would hand the guest bytes nobody paid for.
func TestGrowStateDiskGrowsFileThenVMThenGuest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.img")
	if err := platform.AllocateThick(path, 4<<20); err != nil {
		t.Fatal(err)
	}
	g := &fakeGrower{free: 0}
	resize := func(_ context.Context, size int64) error {
		fi, _ := os.Stat(path)
		if fi.Size() != size {
			t.Errorf("QEMU was told %d before the file reached it (file is %d)", size, fi.Size())
		}
		g.steps = append(g.steps, fmt.Sprintf("vm resize %d", size))
		return nil
	}
	cfg := Config{StateDisk: path}
	if err := cfg.growStateDisk(context.Background(), g, resize, 1<<20, t.Logf); err != nil {
		t.Fatal(err)
	}
	want := int64(4<<20) + stateDiskStep
	if got := strings.Join(g.steps, ", "); got != fmt.Sprintf("free, vm resize %d, guest grow %d", want, want) {
		t.Errorf("steps = %s", got)
	}
	// And a guest that already has the space costs nothing: no resize, no grow.
	g = &fakeGrower{free: 2 << 20}
	if err := cfg.growStateDisk(context.Background(), g, resize, 1<<20, t.Logf); err != nil || len(g.steps) != 1 {
		t.Errorf("a guest with room was grown anyway: %v %v", g.steps, err)
	}
}

// The state disk is made at its initial size, through AllocateThick (whose own test asserts the
// blocks): the host pays for an empty node's journal and scratch up front, and for each service
// when it arrives (growStateDisk) -- never at write time.
func TestProvisionDisksMakesTheStateDiskThick(t *testing.T) {
	cfg := Config{StateDisk: filepath.Join(t.TempDir(), "state.img")}
	if err := cfg.provisionDisks(func(string, ...any) {}); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(cfg.StateDisk)
	if err != nil || fi.Size() != stateDiskInitial {
		t.Fatalf("the state disk is %v (%v), want %d bytes", fi, err, stateDiskInitial)
	}
}

// The host-disk alert: once when free space drops under the reserve the grow keeps, silent while
// it stays low or hovers between the reserve and the clear margin, nothing on an unreadable
// answer, and one recovered when it is back above the margin. It reads once a minute.
func TestDiskAlerterWarnsOncePerEpisode(t *testing.T) {
	fn := &fakeNotifier{}
	st := testStore(t, fn) // the alerter asserts every read; the store is what makes it once

	free, reads := 0, 0
	a := &diskAlerter{read: func(path string) int {
		reads++
		if path != "/var/lib/briard" {
			t.Errorf("read %q, want the state disk's directory", path)
		}
		return free
	}}
	t0 := time.Now()
	at := func(minute int, mb int) {
		free = mb
		a.observe(context.Background(), st, "n1", "/var/lib/briard/state.img", t0.Add(time.Duration(minute)*time.Minute), func(string, ...any) {})
	}
	at(0, 10*1024)
	a.observe(context.Background(), st, "n1", "/var/lib/briard/state.img", t0.Add(30*time.Second), func(string, ...any) {})
	if reads != 1 {
		t.Fatalf("read %d times inside one diskReadEvery, want 1", reads)
	}
	at(1, 1500) // under the 2 GB reserve: warn
	at(2, 1000) // still low: no fatigue
	at(3, 2200) // above the reserve but under the margin: neither cleared nor re-warned
	at(4, 0)    // unreadable: nothing
	if len(fn.alerts) != 1 || fn.alerts[0].Kind != notify.Open || !strings.Contains(fn.alerts[0].Body, "n1") || !strings.Contains(fn.alerts[0].Body, "/var/lib/briard") {
		t.Fatalf("want one warning naming the node and the disk, got %+v", fn.alerts)
	}
	at(5, 3*1024) // back above the margin: recovered
	if len(fn.alerts) != 2 || fn.alerts[1].Kind != notify.Resolved {
		t.Fatalf("want a recovered alert, got %+v", fn.alerts)
	}
	at(6, 1500) // a new episode warns again
	if len(fn.alerts) != 3 || fn.alerts[2].Kind != notify.Open {
		t.Fatalf("a second episode did not warn: %+v", fn.alerts)
	}
}

// The disk-health alert: a steady count of recorded errors says nothing, a GROWING one warns,
// the disk's own failing verdict is critical, an unreadable read changes nothing, and only a
// clean disk resolves. One read an hour, and never a second while one is still out.
func TestSMARTAlerterWarnsOnGrowthAndFailure(t *testing.T) {
	fn := &fakeNotifier{}
	st := testStore(t, fn)

	var answer reportcard.SMART
	reads := 0
	release := make(chan struct{})
	a := &smartAlerter{
		disks: func(path string) ([]string, error) {
			if path != "/var/lib/briard" {
				t.Errorf("resolved %q, want the data disk's directory", path)
			}
			return []string{"/dev/sda"}, nil
		},
		read: func(context.Context, []string) []reportcard.SMART {
			<-release
			reads++
			return []reportcard.SMART{answer}
		},
	}
	t0 := time.Now()
	observe := func(at time.Time) {
		a.observe(context.Background(), st, "n1", "/var/lib/briard/data.img", at, func(string, ...any) {})
	}
	at := func(hour int, s reportcard.SMART) {
		t.Helper()
		s.Device = "/dev/sda"
		answer = s
		now := t0.Add(time.Duration(hour) * time.Hour)
		observe(now) // starts the read
		release <- struct{}{}
		for a.got != nil { // the loop's later ticks take the answer
			observe(now)
			time.Sleep(time.Millisecond)
		}
	}

	at(0, reportcard.SMART{Read: true, Errors: 3}) // a count it came with: nothing
	observe(t0.Add(30 * time.Minute))              // inside the hour: no read
	at(1, reportcard.SMART{Read: true, Errors: 3})
	if reads != 2 || len(fn.alerts) != 0 {
		t.Fatalf("a steady count: %d reads, alerts %+v; want 2 reads and none", reads, fn.alerts)
	}
	at(2, reportcard.SMART{Read: true, Errors: 5})
	if len(fn.alerts) != 1 || fn.alerts[0].Kind != notify.Open || fn.alerts[0].Severity != notify.Warning ||
		fn.alerts[0].Key != "disk-health:sda" || !strings.Contains(fn.alerts[0].Body, "up from 3") {
		t.Fatalf("want a growth warning for sda, got %+v", fn.alerts)
	}
	at(3, reportcard.SMART{Why: "asleep"}) // unknown: nothing
	at(4, reportcard.SMART{Read: true, Failing: true, Errors: 5, Why: "read-only"})
	if len(fn.alerts) != 2 || fn.alerts[1].Severity != notify.Critical || !strings.Contains(fn.alerts[1].Body, "read-only") {
		t.Fatalf("want a critical failing alert, got %+v", fn.alerts)
	}
	at(5, reportcard.SMART{Read: true}) // a replaced disk
	if len(fn.alerts) != 3 || fn.alerts[2].Kind != notify.Resolved {
		t.Fatalf("want a resolve on a clean disk, got %+v", fn.alerts)
	}

	// A read that does not come back holds the next: one smartctl at a time, however long.
	observe(t0.Add(6 * time.Hour))
	observe(t0.Add(8 * time.Hour))
	if reads != 6 || a.got == nil {
		t.Fatalf("%d reads, in flight %v; want the stuck read alone", reads, a.got != nil)
	}
	release <- struct{}{}
}
