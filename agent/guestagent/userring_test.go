package guestagent

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"briard.io/agent/quadlet"
)

const gb = 1_000_000_000

// THE RULE AS DESIGNED, on numbers a human can check: a 100 GB volume holding 10 GB of service
// state gives the user tier 100 − 10 − 2 (20 %) − 2 (2 %) = 86 GB, with a 4.3 GB target. A small
// service state takes the 300 MB floor, a small volume the 1 GB slack and the 1 GB target, and a
// volume the service tier fills leaves a cap of zero rather than a negative one.
func TestUserAllocationIsTheRule(t *testing.T) {
	for _, tc := range []struct {
		name                string
		volume, svc         int64
		wantCap, wantTarget int64
	}{
		{"a large volume", 100 * gb, 10 * gb, 86 * gb, 4.3 * gb},
		{"the headroom floor", 100 * gb, 1 * gb, 100*gb - 1*gb - 300_000_000 - 2*gb, (100*gb - 1*gb - 300_000_000 - 2*gb) / 20},
		{"the slack and target floors", 10 * gb, 1 * gb, 10*gb - 1*gb - 300_000_000 - 1*gb, 1 * gb},
		{"a full service tier", 10 * gb, 9 * gb, 0, 1 * gb},
	} {
		cap, target := userAllocation(tc.volume, tc.svc)
		if cap != tc.wantCap || target != tc.wantTarget {
			t.Errorf("%s: cap %d target %d, want %d %d", tc.name, cap, target, tc.wantCap, tc.wantTarget)
		}
	}
}

// THE RING'S RETENTION, run the way the tick runs it: a member every hour for forty days, pruned at
// every take. What is left is every member of the last day, one a day for the week, one a week to
// thirty days, and nothing older.
func TestTheUserRingKeepsHoursDaysAndWeeks(t *testing.T) {
	start := time.Date(2026, 9, 1, 0, 30, 0, 0, time.UTC)
	var ring []userMember
	var now time.Time
	for h := 0; h < 40*24; h++ {
		now = start.Add(time.Duration(h) * time.Hour)
		ring = append(ring, userMember{app: "a", path: strconv.Itoa(h), at: now})
		for _, m := range userRetention(ring, now) {
			ring = slices.DeleteFunc(ring, func(r userMember) bool { return r.path == m.path })
		}
	}
	var hours, days, weeks int
	daySeen, weekSeen := map[string]bool{}, map[string]bool{}
	for _, m := range ring {
		age := now.Sub(m.at)
		switch {
		case age < 24*time.Hour:
			hours++
		case age < 7*24*time.Hour:
			d := m.at.Format(time.DateOnly)
			if daySeen[d] {
				t.Errorf("two members kept for %s", d)
			}
			daySeen[d] = true
			days++
		case age < 30*24*time.Hour:
			y, w := m.at.ISOWeek()
			k := strconv.Itoa(y) + "-" + strconv.Itoa(w)
			if weekSeen[k] {
				t.Errorf("two members kept for week %s", k)
			}
			weekSeen[k] = true
			weeks++
		default:
			t.Errorf("a member %s old was kept", age)
		}
	}
	if hours != 24 || days < 6 || days > 7 || weeks < 3 || weeks > 5 {
		t.Errorf("kept %d hourly, %d daily, %d weekly (%d in all)", hours, days, weeks, len(ring))
	}
}

// userVolume is a fake guest holding a volume with user areas, their rings and the quota tree.
// Each member costs memberBytes of the user tier until the cleaner has dropped it.
type userVolume struct {
	*fakeExec
	apps        []string
	members     map[string]bool // full paths
	memberBytes int64
	areaBytes   int64 // the household's own files, across every user area
	volume      int64
	svcUsed     int64
}

func newUserVolume(apps ...string) *userVolume {
	v := &userVolume{fakeExec: &fakeExec{}, apps: apps, members: map[string]bool{}, volume: 100 * gb, svcUsed: 10 * gb}
	v.qgroups = map[string][]string{"0/5": nil, tierQgroup(tierService): nil, tierQgroup(tierUser): nil}
	for _, app := range apps {
		v.qgroups[appQgroup(tierService, app)] = []string{tierQgroup(tierService)}
		v.qgroups[appQgroup(tierUser, app)] = []string{tierQgroup(tierUser)}
	}
	v.runFn = func(name string, args []string) ([]byte, error) {
		switch {
		case name == "ls" && args[1] == userRoot():
			return []byte(strings.Join(append(slices.Clone(v.apps), ".snapshots"), "\n")), nil
		case name == "ls" && args[1] == userSnapshots():
			var ls []string
			for m := range v.members {
				ls = append(ls, filepath.Base(m), filepath.Base(m)+".json")
			}
			return []byte(strings.Join(ls, "\n")), nil
		case name == "btrfs" && args[0] == "subvolume" && args[1] == "show":
			if v.members[args[2]] {
				return []byte(args[2]), nil
			}
			return nil, errors.New("ERROR: not a subvolume")
		case name == "btrfs" && args[0] == "subvolume" && args[1] == "snapshot":
			v.members[args[len(args)-1]] = true
		case name == "btrfs" && args[0] == "subvolume" && args[1] == "delete":
			delete(v.members, args[2])
		case name == "btrfs" && args[0] == "filesystem":
			return []byte("Overall:\n    Device size:\t\t " + strconv.FormatInt(v.volume, 10) + "\n"), nil
		}
		return nil, nil
	}
	return v
}

// Run keeps the tree's numbers true to the volume before every read of it.
func (v *userVolume) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	v.qused = map[string]int64{
		tierQgroup(tierService): v.svcUsed,
		tierQgroup(tierUser):    v.areaBytes + int64(len(v.members))*v.memberBytes,
	}
	return v.fakeExec.Run(ctx, name, args...)
}

// member puts a member of app taken at `at` in the ring.
func (v *userVolume) member(app string, at time.Time) string {
	m := filepath.Join(userSnapshots(), filepath.Base(quadlet.SnapshotMember(app, quadlet.TriggerClock, at)))
	v.members[m] = true
	return m
}

func (v *userVolume) ran(prefix ...string) [][]string {
	var out [][]string
	for _, r := range v.runs {
		if len(r) >= len(prefix) && slices.Equal(r[:len(prefix)], prefix) {
			out = append(out, r)
		}
	}
	return out
}

var tickNow = time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)

// A FRESH INSTALL: one member taken into the app's user group, the cap set, nothing shed and no
// warning -- there is no pressure. A second tick an hour later takes the next member and does not
// set the same limit again.
func TestAFreshInstallTakesAMemberAndSetsTheCap(t *testing.T) {
	v := newUserVolume("immich")
	rep := userTick(context.Background(), v, tickNow)
	if len(rep.Errors) > 0 {
		t.Fatalf("errors: %v", rep.Errors)
	}
	if len(rep.Taken) != 1 || !strings.HasPrefix(filepath.Base(rep.Taken[0]), "immich-clock-") {
		t.Fatalf("taken %v", rep.Taken)
	}
	if len(v.ran("btrfs", "subvolume", "snapshot", "-r", "-i", appQgroup(tierUser, "immich"), userArea("immich"))) != 1 {
		t.Errorf("the member was not born into immich's user group: %v", v.runs)
	}
	if _, ok := v.files[quadlet.SnapshotSidecar(rep.Taken[0])]; !ok {
		t.Error("the member has no sidecar")
	}
	if rep.Short || len(rep.Shed) > 0 {
		t.Errorf("a fresh install warned or shed: %+v", rep)
	}
	wantCap, _ := userAllocation(v.volume, v.svcUsed)
	if rep.UserCap != wantCap || v.qlimits[tierQgroup(tierUser)] != strconv.FormatInt(wantCap, 10) {
		t.Errorf("cap %d, limit %q, want %d", rep.UserCap, v.qlimits[tierQgroup(tierUser)], wantCap)
	}

	before := len(v.ran("btrfs", "qgroup", "limit"))
	rep = userTick(context.Background(), v, tickNow.Add(10*time.Minute))
	if len(rep.Taken) != 0 {
		t.Errorf("a member ten minutes after the last: %v", rep.Taken)
	}
	rep = userTick(context.Background(), v, tickNow.Add(time.Hour))
	if len(rep.Taken) != 1 {
		t.Errorf("no member an hour after the last: %v", rep.Taken)
	}
	if n := len(v.ran("btrfs", "qgroup", "limit")); n != before {
		t.Errorf("the unchanged cap was set again (%d limit calls, %d before)", n, before)
	}
}

// A GROWING SERVICE LOWERS THE CAP, and the limit follows it -- below what the user tier holds,
// which the kernel accepts and which stops the next write.
func TestAGrowingServiceLowersTheCap(t *testing.T) {
	v := newUserVolume("immich")
	v.areaBytes = 60 * gb
	userTick(context.Background(), v, tickNow)
	v.svcUsed = 40 * gb
	rep := userTick(context.Background(), v, tickNow.Add(10*time.Minute))
	wantCap, _ := userAllocation(v.volume, v.svcUsed)
	if v.qlimits[tierQgroup(tierUser)] != strconv.FormatInt(wantCap, 10) || wantCap >= v.areaBytes {
		t.Errorf("limit %q after the service grew, want %d (under the %d the user tier holds)", v.qlimits[tierQgroup(tierUser)], wantCap, v.areaBytes)
	}
	if rep.UserFree >= 0 {
		t.Errorf("userFree %d, want it negative: the cap is under what the tier holds", rep.UserFree)
	}
}

// THE VALVE sheds the oldest eligible member first, across apps, and stops when there is room:
// members younger than a day are never shed, and nothing outside the user ring is ever deleted.
// History of ten days gave the room, so there is no warning.
func TestTheValveShedsTheOldestUntilThereIsRoom(t *testing.T) {
	v := newUserVolume("immich", "jellyfin")
	v.memberBytes = 2 * gb
	cap, target := userAllocation(v.volume, v.svcUsed)
	var old []string
	for d := 10; d >= 1; d-- { // one member a day, ten days back, alternating apps
		app := []string{"immich", "jellyfin"}[d%2]
		old = append(old, v.member(app, tickNow.Add(-time.Duration(d)*24*time.Hour-time.Hour)))
	}
	fresh := v.member("immich", tickNow.Add(-30*time.Minute))
	v.member("jellyfin", tickNow.Add(-30*time.Minute))
	// The household's files leave room for exactly 7 of the 12 members' bytes above the target.
	v.areaBytes = cap - target - 7*v.memberBytes
	rep := userTick(context.Background(), v, tickNow)
	if len(rep.Errors) > 0 {
		t.Fatalf("errors: %v", rep.Errors)
	}
	if !slices.Equal(rep.Shed, old[:5]) {
		t.Errorf("shed %v, want the five oldest %v", rep.Shed, old[:5])
	}
	if rep.UserFree < rep.Target || rep.Short {
		t.Errorf("free %d target %d short %v after the valve", rep.UserFree, rep.Target, rep.Short)
	}
	if !v.members[fresh] {
		t.Error("a member younger than a day was shed")
	}
	for _, d := range v.ran("btrfs", "subvolume", "delete") {
		if !strings.HasPrefix(d[3], userSnapshots()+"/") {
			t.Errorf("deleted outside the user ring: %v", d)
		}
	}
	if len(v.ran("btrfs", "subvolume", "sync")) != len(rep.Shed) {
		t.Errorf("each shed member was not waited for: %v", v.runs)
	}
}

// AT THE FLOOR THE VALVE STOPS AND SAYS SO: the household's own files fill the cap, the day's
// members are all that is left, and the report is short -- the uploader meets EDQUOT next, and
// the household has been told before it does.
func TestTheValveStopsAtTheDayAndWarns(t *testing.T) {
	v := newUserVolume("immich")
	v.memberBytes = 1 * gb
	cap, _ := userAllocation(v.volume, v.svcUsed)
	v.member("immich", tickNow.Add(-3*24*time.Hour))
	young := v.member("immich", tickNow.Add(-2*time.Hour))
	v.areaBytes = cap
	rep := userTick(context.Background(), v, tickNow)
	if len(rep.Shed) != 1 || !v.members[young] {
		t.Errorf("shed %v; the day's member kept: %v", rep.Shed, v.members[young])
	}
	if !rep.Short {
		t.Error("the valve ran out and did not say the household is short")
	}
}

// OLD HISTORY IS SHED WITHOUT A WARNING; recent history is not. A valve that had to reach into
// the last five days is the warning, even when it found the room.
func TestTheWarningIsAboutRecentHistory(t *testing.T) {
	for _, tc := range []struct {
		name  string
		age   time.Duration
		short bool
	}{{"twenty days old", 20 * 24 * time.Hour, false}, {"three days old", 3 * 24 * time.Hour, true}} {
		v := newUserVolume("immich")
		v.memberBytes = 2 * gb
		cap, target := userAllocation(v.volume, v.svcUsed)
		v.member("immich", tickNow.Add(-tc.age))
		v.member("immich", tickNow.Add(-30*time.Minute)) // the hour's member: no take, so only these two count
		v.areaBytes = cap - target - v.memberBytes*3/2
		rep := userTick(context.Background(), v, tickNow)
		if len(rep.Shed) != 1 || rep.Short != tc.short {
			t.Errorf("%s: shed %v short %v, want one shed and short=%v", tc.name, rep.Shed, rep.Short, tc.short)
		}
	}
}

// NOTHING INSTALLED, NOTHING BOUNDED: with no user aggregate there is no limit to set and no
// warning to give.
func TestATickBeforeAnyInstallDoesNothing(t *testing.T) {
	v := newUserVolume()
	delete(v.qgroups, tierQgroup(tierUser))
	rep := userTick(context.Background(), v, tickNow)
	if len(rep.Errors) > 0 || rep.Short || len(rep.Taken) > 0 || len(v.ran("btrfs", "qgroup", "limit")) > 0 {
		t.Errorf("a tick on an empty volume acted: %+v %v", rep, v.runs)
	}
}

// ONE AT A TIME, and a finished report waits to be collected.
func TestOneTickAtATime(t *testing.T) {
	v := newUserVolume()
	if !startUserTick(v) {
		t.Fatal("the first tick did not start")
	}
	if startUserTick(v) {
		t.Error("a second tick started beside the first, or over its uncollected report")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		s := userTickResult()
		if s.Report != nil {
			break
		}
		if !s.Running || time.Now().After(deadline) {
			t.Fatalf("no report: %+v", s)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if s := userTickResult(); s.Report != nil || s.Running {
		t.Errorf("the report was handed over twice: %+v", s)
	}
}
