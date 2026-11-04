package guestagent

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"briard.io/agent/quadlet"
	"briard.io/shared/dashboard"
)

// usageVolume is a fake guest with one app's areas: which subvolume each area is, what compsize
// finds in it, and the two rings. Its own lock, because the walk runs beside the test.
type usageVolume struct {
	*fakeExec
	mu       sync.Mutex
	roots    map[string]string // area -> subvolume id; absent: no such subvolume
	compsize map[string]string // area -> output; absent: compsize fails
	walks    int
}

const usageApp = "immich"

var (
	usageService = quadlet.DataRoot(usageApp)
	usageUser    = filesArea(usageApp)
	usageNow     = time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
)

func compsizeTotal(disk, uncompressed, referenced string) string {
	return "Processed 3 files, 9 regular extents (9 refs), 1 inline.\n" +
		"Type       Perc     Disk Usage   Uncompressed Referenced  \n" +
		"TOTAL       22%     " + disk + "     " + uncompressed + "    " + referenced + "   \n" +
		"zstd         3%     6250496      200003584    200003584   \n"
}

func newUsageVolume() *usageVolume {
	v := &usageVolume{fakeExec: &fakeExec{}}
	svc, user := appQgroup(tierData, usageApp), appQgroup(tierFiles, usageApp)
	v.qgroups = map[string][]string{"0/5": nil, tierQgroup(tierData): nil, tierQgroup(tierFiles): nil,
		svc: {tierQgroup(tierData)}, user: {tierQgroup(tierFiles)}}
	v.qused = map[string]int64{svc: 3 * gb, user: 50 * gb}
	v.qlimits = map[string]string{svc: "40000000000", user: "300000000000"}
	v.roots = map[string]string{usageService: "257", usageUser: "300"}
	v.compsize = map[string]string{
		usageService: compsizeTotal("2000000000", "5000000000", "5000000000"),
		usageUser:    compsizeTotal("45000000000", "46000000000", "46000000000"),
	}
	at := usageNow.Add(-time.Hour)
	v.runFn = func(name string, args []string) ([]byte, error) {
		v.mu.Lock()
		defer v.mu.Unlock()
		switch {
		case name == "btrfs" && args[0] == "inspect-internal":
			if id, ok := v.roots[args[2]]; ok {
				return []byte(id + "\n"), nil
			}
			return []byte("ERROR: cannot access: No such file or directory"), errors.New("exit status 1")
		case name == "compsize":
			v.walks++
			if out, ok := v.compsize[args[2]]; ok {
				if strings.Contains(out, "No files.") {
					return []byte(out), errors.New("exit status 1")
				}
				return []byte(out), nil
			}
			return []byte("compsize: not found"), errors.New("exec: not found")
		case name == "ls" && args[1] == quadlet.SnapshotsDir:
			var ls []string
			for i := range 3 {
				m := filepath.Base(quadlet.SnapshotMember(usageApp, quadlet.TriggerClock, at.Add(-time.Duration(i)*time.Hour)))
				ls = append(ls, m, m+".json")
			}
			ls = append(ls, filepath.Base(quadlet.SnapshotMember("jellyfin", quadlet.TriggerClock, at)))
			return []byte(strings.Join(ls, "\n")), nil
		case name == "ls" && args[1] == filesVersionsDir():
			m := filepath.Base(quadlet.SnapshotMember(usageApp, quadlet.TriggerClock, at))
			return []byte(m + "\n" + m + ".json"), nil
		}
		return nil, nil
	}
	resetUsage()
	return v
}

func (v *usageVolume) set(f func()) {
	v.mu.Lock()
	defer v.mu.Unlock()
	f()
}

func (v *usageVolume) walked() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.walks
}

func resetUsage() {
	waitUsage()
	usageWalks.Lock()
	defer usageWalks.Unlock()
	usageWalks.done = nil
}

// waitUsage waits for a walk in the background to end.
func waitUsage() {
	for {
		usageWalks.Lock()
		busy := usageWalks.busy
		usageWalks.Unlock()
		if !busy {
			return
		}
		time.Sleep(time.Millisecond)
	}
}

func (v *usageVolume) ask(t *testing.T, now time.Time) dashboard.AppUsage {
	t.Helper()
	u, err := appUsage(context.Background(), v, usageApp, now)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// THE FIRST ASK answers the totals, caps and counts at once and starts the walk; the next has the
// split: the history is the group less what the current data holds on disk, and the files' size
// is compsize's referenced bytes, larger than the disk's under compression.
func TestTheFirstAskHasTheTotalsAndTheNextTheSplit(t *testing.T) {
	v := newUsageVolume()
	u := v.ask(t, usageNow)
	if !u.Measuring || u.Data == nil || u.Files == nil {
		t.Fatalf("first ask: %+v", u)
	}
	if s := *u.Data; s.Total != 3*gb || s.Cap != 40*gb || s.Members != 3 || s.Measured {
		t.Errorf("service before the walk: %+v", s)
	}
	if s := *u.Files; s.Total != 50*gb || s.Cap != 300*gb || s.Members != 1 || s.Measured {
		t.Errorf("user before the walk: %+v", s)
	}
	waitUsage()
	u = v.ask(t, usageNow.Add(time.Minute))
	if u.Measuring {
		t.Error("still measuring after the walk")
	}
	want := dashboard.TierUsage{Total: 3 * gb, Cap: 40 * gb, Members: 3, Measured: true, Current: 2 * gb, Uncompressed: 5 * gb, History: 1 * gb}
	if *u.Data != want {
		t.Errorf("service: %+v, want %+v", *u.Data, want)
	}
	want = dashboard.TierUsage{Total: 50 * gb, Cap: 300 * gb, Members: 1, Measured: true, Current: 45 * gb, Uncompressed: 46 * gb, History: 5 * gb}
	if *u.Files != want {
		t.Errorf("user: %+v, want %+v", *u.Files, want)
	}
	if v.walked() != 2 {
		t.Errorf("%d walks, want one per area", v.walked())
	}
}

// THE WALK IS PULLED AND CACHED: asking again inside ten minutes walks nothing; after them the
// last numbers are kept while the next walk runs; an undo -- another subvolume at the area --
// discards the old numbers for that area and walks it at once.
func TestTheWalkIsCachedUntilItIsStaleOrTheAreaIsSwapped(t *testing.T) {
	v := newUsageVolume()
	v.ask(t, usageNow)
	waitUsage()
	v.ask(t, usageNow.Add(9*time.Minute))
	waitUsage()
	if v.walked() != 2 {
		t.Fatalf("%d walks inside the window, want the first two", v.walked())
	}

	u := v.ask(t, usageNow.Add(11*time.Minute))
	if !u.Measuring || !u.Data.Measured || u.Data.Current != 2*gb {
		t.Errorf("a stale answer loses its numbers or is not re-measured: %+v", *u.Data)
	}
	waitUsage()
	if v.walked() != 4 {
		t.Errorf("%d walks after the window, want four", v.walked())
	}

	v.set(func() { v.roots[usageService] = "412" })
	u = v.ask(t, usageNow.Add(12*time.Minute))
	if !u.Measuring || u.Data.Measured || !u.Files.Measured {
		t.Errorf("after the swap: service %+v, user %+v", *u.Data, *u.Files)
	}
	waitUsage()
	if v.walked() != 5 {
		t.Errorf("%d walks after the swap, want the data area's alone", v.walked())
	}
}

// AN EMPTY AREA is zero bytes, measured; a WALK THAT FAILS is no numbers, and is not tried again
// inside the window, so a page left open does not walk every time it asks. An app with no user
// area has no files tier.
func TestEmptyFailedAndAbsentAreas(t *testing.T) {
	v := newUsageVolume()
	v.set(func() {
		v.compsize[usageService] = "No files.\n"
		delete(v.compsize, usageUser)
	})
	v.ask(t, usageNow)
	waitUsage()
	u := v.ask(t, usageNow.Add(time.Minute))
	waitUsage()
	if s := *u.Data; !s.Measured || s.Current != 0 || s.History != 3*gb {
		t.Errorf("an empty area: %+v", s)
	}
	if u.Files.Measured || u.Measuring {
		t.Errorf("a failed walk: %+v, measuring %t", *u.Files, u.Measuring)
	}
	if v.walked() != 2 {
		t.Errorf("%d walks, want a failed one not retried", v.walked())
	}

	v.set(func() {
		delete(v.qgroups, appQgroup(tierFiles, usageApp))
		delete(v.roots, usageUser)
	})
	if u := v.ask(t, usageNow.Add(2*time.Minute)); u.Files != nil {
		t.Errorf("an app with no files group: %+v", *u.Files)
	}
}

// ONE WALK AT A TIME: an ask while another app's walk runs starts none of its own, says it is
// measuring, and its next ask after that walk starts it.
func TestOneWalkAtATime(t *testing.T) {
	v := newUsageVolume()
	usageWalks.Lock()
	usageWalks.busy = true
	usageWalks.Unlock()
	if u := v.ask(t, usageNow); !u.Measuring {
		t.Error("not measuring while another walk runs")
	}
	if v.walked() != 0 {
		t.Errorf("%d walks beside a running one", v.walked())
	}
	usageWalks.Lock()
	usageWalks.busy = false
	usageWalks.Unlock()
	v.ask(t, usageNow)
	waitUsage()
	if v.walked() != 2 {
		t.Errorf("%d walks once the other ended, want two", v.walked())
	}
}

// THE UNVERSIONED AREA IS PART OF THE DATA AND THE CACHE IS SAID APART: both are charged to the
// data group, so the data's current is the data and unversioned areas together, the history is
// the group less both and the cache, and the cache is its own walk. Until every area of the group
// is walked, the data is not measured: a history that counted the cache would be wrong.
func TestUnversionedCountsAsDataAndTheCacheApart(t *testing.T) {
	v := newUsageVolume()
	unversioned, cache := quadlet.Area("unversioned", usageApp), quadlet.Area("cache", usageApp)
	v.set(func() {
		v.roots[unversioned], v.roots[cache] = "301", "302"
		v.compsize[unversioned] = compsizeTotal("500000000", "600000000", "600000000")
		v.compsize[cache] = compsizeTotal("300000000", "900000000", "900000000")
	})
	u := v.ask(t, usageNow)
	if u.Data.Measured || u.Cache == nil || u.Cache.Measured {
		t.Fatalf("before the walk: data %+v, cache %+v", *u.Data, u.Cache)
	}
	waitUsage()
	u = v.ask(t, usageNow.Add(time.Minute))
	want := dashboard.TierUsage{Total: 3 * gb, Cap: 40 * gb, Members: 3, Measured: true,
		Current: 2*gb + 500*mb, Uncompressed: 5*gb + 600*mb, History: 200 * mb}
	if *u.Data != want {
		t.Errorf("data: %+v, want %+v", *u.Data, want)
	}
	if want := (dashboard.TierUsage{Cap: -1, Measured: true, Current: 300 * mb, Uncompressed: 900 * mb}); *u.Cache != want {
		t.Errorf("cache: %+v, want %+v", *u.Cache, want)
	}
	if v.walked() != 4 {
		t.Errorf("%d walks, want one per area", v.walked())
	}
	v.set(func() { delete(v.roots, cache) })
	if u := v.ask(t, usageNow.Add(2*time.Minute)); u.Cache != nil {
		t.Errorf("an app with no cache area has a cache: %+v", *u.Cache)
	}
}
