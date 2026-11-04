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

// THE RULE AS DESIGNED, on numbers a human can check: a 100 GB volume with 2 GB of slack holding
// 10 GB of service state gives the files tier 100 − 10 − 2 (20 %) − 2 = 86 GB, with a 4.3 GB
// target. A small service state takes the 300 MB floor, a small cap the 1 GB target, and a volume
// the data tier fills leaves a cap of zero rather than a negative one.
func TestFilesAllocationIsTheRule(t *testing.T) {
	for _, tc := range []struct {
		name                string
		volume, slack, svc  int64
		wantCap, wantTarget int64
	}{
		{"a large volume", 100 * gb, 2 * gb, 10 * gb, 86 * gb, 4.3 * gb},
		{"the headroom floor", 100 * gb, 2 * gb, 1 * gb, 100*gb - 1*gb - 300*mb - 2*gb, (100*gb - 1*gb - 300*mb - 2*gb) / 20},
		{"the target floor", 10 * gb, 1 * gb, 1 * gb, 10*gb - 1*gb - 300*mb - 1*gb, 1 * gb},
		{"a full data tier", 10 * gb, 1 * gb, 9 * gb, 0, 1 * gb},
	} {
		cap, target := filesAllocation(tc.volume, tc.slack, tc.svc)
		if cap != tc.wantCap || target != tc.wantTarget {
			t.Errorf("%s: cap %d target %d, want %d %d", tc.name, cap, target, tc.wantCap, tc.wantTarget)
		}
	}
}

// THE SLACK IS MEASURED: what is on the disk past what the tree charges, and one metadata chunk
// in every copy -- 256 MiB on a volume up to 50 GiB, 1 GiB above, never over a tenth of it.
func TestTheSlackIsWhatBtrfsKeepsAndOneChunk(t *testing.T) {
	for _, tc := range []struct {
		name          string
		u             volumeUse
		charged, want int64
	}{
		{"the default volume", volumeUse{4 << 30, 2*gb + 60*mb, 2}, 2 * gb, 60*mb + 512<<20},
		{"a large volume", volumeUse{1000 * gb, 503 * gb, 2}, 500 * gb, 3*gb + 2<<30},
		{"a tiny volume", volumeUse{1 << 30, 200 * mb, 2}, 190 * mb, 10*mb + 2*((1<<30)/10)},
		{"single metadata", volumeUse{4 << 30, gb, 1}, gb, 256 << 20},
		{"charged past used", volumeUse{4 << 30, gb, 2}, gb + 1, 512 << 20},
	} {
		if got := fsSlack(tc.u, tc.charged); got != tc.want {
			t.Errorf("%s: slack %d, want %d", tc.name, got, tc.want)
		}
	}
}

// THE APP'S CAP: a tenth of its size and the free space for a small app, the tier's keep for a big
// one, and nothing under zero.
func TestAnAppsCapLeavesTheOthersTheirShare(t *testing.T) {
	keep := int64(300 * mb)
	for _, tc := range []struct {
		name        string
		total, want int64
	}{
		{"a small app", 1 * gb, 900 * mb},
		{"at ten times the keep", 3 * gb, 2700 * mb},
		{"a big app", 500 * gb, 500*gb - 300*mb},
		{"a tier past full", -1 * gb, 0},
	} {
		if got := appCap(tc.total, keep); got != tc.want {
			t.Errorf("%s: cap %d, want %d", tc.name, got, tc.want)
		}
	}
}

// EVERY APP IN EACH TIER IS CAPPED: a service app by the volume's physical free space and the
// volume's keep, a files area by the files tier's free space and its own. An app grown alone keeps
// its cap -- what it adds, the tier loses -- and a tick with nothing changed sets no limit again.
func TestTheTickCapsEveryApp(t *testing.T) {
	v := newUserVolume("immich", "jellyfin")
	v.svcApp = map[string]int64{"immich": 2 * gb, "jellyfin": 8 * gb}
	v.userFiles = map[string]int64{"immich": 45 * gb, "jellyfin": 5 * gb}
	v.member("immich", tickNow.Add(-30*time.Minute)) // no take: only the files count
	v.member("jellyfin", tickNow.Add(-30*time.Minute))
	rep := filesTick(context.Background(), v, tickNow)
	if len(rep.Errors) > 0 {
		t.Fatalf("errors: %v", rep.Errors)
	}
	// 100 GB, 2 GiB of slack, 10 GB of service, 50 GB of files.
	free := rep.Volume - rep.Slack - rep.DataUsed - rep.FilesUsed
	if rep.Slack != 2<<30 || free != 100*gb-2<<30-10*gb-50*gb {
		t.Fatalf("slack %d, free %d", rep.Slack, free)
	}
	want := map[string]int64{
		appQgroup(tierData, "immich"):    appCap(2*gb+free, 1*gb),
		appQgroup(tierData, "jellyfin"):  appCap(8*gb+free, 1*gb),
		appQgroup(tierFiles, "immich"):   appCap(45*gb+rep.FilesFree, max(300*mb, rep.FilesCap/100)),
		appQgroup(tierFiles, "jellyfin"): appCap(5*gb+rep.FilesFree, max(300*mb, rep.FilesCap/100)),
	}
	for g, c := range want {
		if v.qlimits[g] != strconv.FormatInt(c, 10) {
			t.Errorf("%s: limit %q, want %d", g, v.qlimits[g], c)
		}
	}
	before := len(v.ran("btrfs", "qgroup", "limit"))
	v.svcApp["jellyfin"] += 3 * gb
	v.dataUsed += 3 * gb
	filesTick(context.Background(), v, tickNow.Add(10*time.Minute))
	if v.qlimits[appQgroup(tierData, "jellyfin")] != strconv.FormatInt(want[appQgroup(tierData, "jellyfin")], 10) {
		t.Errorf("jellyfin grew alone and its cap moved: %q, want %d", v.qlimits[appQgroup(tierData, "jellyfin")], want[appQgroup(tierData, "jellyfin")])
	}
	filesTick(context.Background(), v, tickNow.Add(20*time.Minute))
	limits := len(v.ran("btrfs", "qgroup", "limit"))
	filesTick(context.Background(), v, tickNow.Add(30*time.Minute))
	if n := len(v.ran("btrfs", "qgroup", "limit")); n != limits || limits == before {
		t.Errorf("limit calls: %d before the growth, %d after it, %d after an unchanged tick", before, limits, n)
	}
}

// WHILE THE TIER HAS ROOM, NO APP IS AT ITS CAP: the valve holds filesFree at its target, and every
// app in either tier is still short of its limit, so the household's warning comes first -- on
// the default volume, and on a terabyte whichever tier holds it.
func TestNoAppIsAtItsCapBeforeTheWarning(t *testing.T) {
	for _, tc := range []struct {
		name        string
		volume, svc int64
		files       int64 // the one app's files, at most what the target and the hour's member leave
	}{
		{"the default volume", 4 << 30, 500 * mb, 1 * gb},
		{"a terabyte, services large", 1000 * gb, 700 * gb, 150 * gb},
		{"a terabyte, services small", 1000 * gb, 10 * gb, 900 * gb},
	} {
		v := newUserVolume("immich")
		v.volume, v.dataUsed = tc.volume, tc.svc
		v.svcApp = map[string]int64{"immich": tc.svc}
		v.memberBytes = 100 * mb
		v.member("immich", tickNow.Add(-30*time.Minute))
		for d := 30; d >= 6; d-- {
			v.member("immich", tickNow.Add(-time.Duration(d)*24*time.Hour))
		}
		cap, target := v.allocation()
		v.userFiles = map[string]int64{"immich": min(tc.files, cap-target-v.memberBytes)} // the hour's member stays
		rep := filesTick(context.Background(), v, tickNow)
		if rep.Short || rep.FilesFree < rep.Target {
			t.Errorf("%s: free %d target %d short %v", tc.name, rep.FilesFree, rep.Target, rep.Short)
			continue
		}
		for _, g := range []string{appQgroup(tierData, "immich"), appQgroup(tierFiles, "immich")} {
			if l, _ := strconv.ParseInt(v.qlimits[g], 10, 64); l <= v.qused[g] {
				t.Errorf("%s: %s holds %d, at or over its limit %d", tc.name, g, v.qused[g], l)
			}
		}
	}
}

// THE RING'S RETENTION, run the way the tick runs it: a member every hour for forty days, pruned at
// every take. What is left is every member of the last day, one a day for the week, one a week to
// thirty days, and nothing older.
func TestTheUserRingKeepsHoursDaysAndWeeks(t *testing.T) {
	start := time.Date(2026, 9, 1, 0, 30, 0, 0, time.UTC)
	var ring []fileVersion
	var now time.Time
	for h := 0; h < 40*24; h++ {
		now = start.Add(time.Duration(h) * time.Hour)
		ring = append(ring, fileVersion{app: "a", path: strconv.Itoa(h), at: now})
		for _, m := range versionRetention(ring, now) {
			ring = slices.DeleteFunc(ring, func(r fileVersion) bool { return r.path == m.path })
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

// userVolume is a fake guest holding a volume with files areas, their rings and the quota tree.
// Each member costs memberBytes of the files tier until the cleaner has dropped it.
type userVolume struct {
	*fakeExec
	apps        []string
	members     map[string]bool // full paths
	memberBytes int64
	areaBytes   int64 // the household's own files, across every files area
	volume      int64
	dataUsed    int64
	// Per app, when a test is about the apps' caps: the service state each holds (inside
	// dataUsed), and files on top of areaBytes. A member's bytes are its app's.
	svcApp, userFiles map[string]int64
	own               int64 // on the disk and charged to nobody
}

// allocation is the rule's cap and target for the volume as it stands.
func (v *userVolume) allocation() (int64, int64) {
	charged := v.charged()
	return filesAllocation(v.volume, fsSlack(volumeUse{v.volume, charged + v.own, 2}, charged), v.dataUsed)
}

func (v *userVolume) charged() int64 {
	var files int64
	for _, b := range v.userFiles {
		files += b
	}
	return v.dataUsed + v.areaBytes + files + int64(len(v.members))*v.memberBytes
}

func newUserVolume(apps ...string) *userVolume {
	v := &userVolume{fakeExec: &fakeExec{}, apps: apps, members: map[string]bool{}, volume: 100 * gb, dataUsed: 10 * gb}
	v.qgroups = map[string][]string{"0/5": nil, tierQgroup(tierData): nil, tierQgroup(tierFiles): nil}
	for _, app := range apps {
		v.qgroups[appQgroup(tierData, app)] = []string{tierQgroup(tierData)}
		v.qgroups[appQgroup(tierFiles, app)] = []string{tierQgroup(tierFiles)}
	}
	v.runFn = func(name string, args []string) ([]byte, error) {
		switch {
		case name == "ls" && args[1] == filesRoot():
			return []byte(strings.Join(append(slices.Clone(v.apps), ".snapshots"), "\n")), nil
		case name == "ls" && args[1] == filesVersionsDir():
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
			return []byte("Overall:\n    Device size:\t\t " + strconv.FormatInt(v.volume, 10) +
				"\n    Used:\t\t\t " + strconv.FormatInt(v.charged()+v.own, 10) +
				"\n    Metadata ratio:\t\t      2.00\nData,single: Size:1, Used:1 (100.00%)\n"), nil
		}
		return nil, nil
	}
	return v
}

// Run keeps the tree's numbers true to the volume before every read of it.
func (v *userVolume) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	v.qused = map[string]int64{
		"0/5":                 0,
		tierQgroup(tierData):  v.dataUsed,
		tierQgroup(tierFiles): v.charged() - v.dataUsed,
	}
	for app, b := range v.svcApp {
		v.qused[appQgroup(tierData, app)] = b
	}
	for _, app := range v.apps {
		g := appQgroup(tierFiles, app)
		v.qused[g] += v.userFiles[app]
		for m := range v.members {
			if a, _, _, _ := quadlet.ParseSnapshotMember(filepath.Base(m)); a == app {
				v.qused[g] += v.memberBytes
			}
		}
	}
	return v.fakeExec.Run(ctx, name, args...)
}

// member puts a member of app taken at `at` in the ring.
func (v *userVolume) member(app string, at time.Time) string {
	m := filepath.Join(filesVersionsDir(), filepath.Base(quadlet.SnapshotMember(app, quadlet.TriggerClock, at)))
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

// A FRESH INSTALL: one member taken into the app's files group, the cap set, nothing shed and no
// warning -- there is no pressure. A second tick an hour later takes the next member and does not
// set the same limit again.
func TestAFreshInstallTakesAMemberAndSetsTheCap(t *testing.T) {
	v := newUserVolume("immich")
	rep := filesTick(context.Background(), v, tickNow)
	if len(rep.Errors) > 0 {
		t.Fatalf("errors: %v", rep.Errors)
	}
	if len(rep.Taken) != 1 || !strings.HasPrefix(filepath.Base(rep.Taken[0]), "immich-clock-") {
		t.Fatalf("taken %v", rep.Taken)
	}
	if len(v.ran("btrfs", "subvolume", "snapshot", "-r", "-i", appQgroup(tierFiles, "immich"), filesArea("immich"))) != 1 {
		t.Errorf("the member was not born into immich's files group: %v", v.runs)
	}
	if _, ok := v.files[quadlet.SnapshotSidecar(rep.Taken[0])]; !ok {
		t.Error("the member has no sidecar")
	}
	if rep.Short || len(rep.Shed) > 0 {
		t.Errorf("a fresh install warned or shed: %+v", rep)
	}
	wantCap, _ := v.allocation()
	if rep.FilesCap != wantCap || v.qlimits[tierQgroup(tierFiles)] != strconv.FormatInt(wantCap, 10) {
		t.Errorf("cap %d, limit %q, want %d", rep.FilesCap, v.qlimits[tierQgroup(tierFiles)], wantCap)
	}

	before := len(v.ran("btrfs", "qgroup", "limit"))
	rep = filesTick(context.Background(), v, tickNow.Add(10*time.Minute))
	if len(rep.Taken) != 0 {
		t.Errorf("a member ten minutes after the last: %v", rep.Taken)
	}
	rep = filesTick(context.Background(), v, tickNow.Add(time.Hour))
	if len(rep.Taken) != 1 {
		t.Errorf("no member an hour after the last: %v", rep.Taken)
	}
	if n := len(v.ran("btrfs", "qgroup", "limit")); n != before {
		t.Errorf("the unchanged cap was set again (%d limit calls, %d before)", n, before)
	}
}

// A GROWING SERVICE LOWERS THE CAP, and the limit follows it -- below what the files tier holds,
// which the kernel accepts and which stops the next write.
func TestAGrowingServiceLowersTheCap(t *testing.T) {
	v := newUserVolume("immich")
	v.areaBytes = 60 * gb
	filesTick(context.Background(), v, tickNow)
	v.dataUsed = 40 * gb
	rep := filesTick(context.Background(), v, tickNow.Add(10*time.Minute))
	wantCap, _ := v.allocation()
	if v.qlimits[tierQgroup(tierFiles)] != strconv.FormatInt(wantCap, 10) || wantCap >= v.areaBytes {
		t.Errorf("limit %q after the service grew, want %d (under the %d the files tier holds)", v.qlimits[tierQgroup(tierFiles)], wantCap, v.areaBytes)
	}
	if rep.FilesFree >= 0 {
		t.Errorf("filesFree %d, want it negative: the cap is under what the tier holds", rep.FilesFree)
	}
}

// THE VALVE sheds the oldest eligible member first, across apps, and stops when there is room:
// members younger than a day are never shed, and nothing outside the versions is ever deleted.
// History of ten days gave the room, so there is no warning.
func TestTheValveShedsTheOldestUntilThereIsRoom(t *testing.T) {
	v := newUserVolume("immich", "jellyfin")
	v.memberBytes = 2 * gb
	cap, target := v.allocation()
	var old []string
	for d := 10; d >= 1; d-- { // one member a day, ten days back, alternating apps
		app := []string{"immich", "jellyfin"}[d%2]
		old = append(old, v.member(app, tickNow.Add(-time.Duration(d)*24*time.Hour-time.Hour)))
	}
	fresh := v.member("immich", tickNow.Add(-30*time.Minute))
	v.member("jellyfin", tickNow.Add(-30*time.Minute))
	// The household's files leave room for exactly 7 of the 12 members' bytes above the target.
	v.areaBytes = cap - target - 7*v.memberBytes
	rep := filesTick(context.Background(), v, tickNow)
	if len(rep.Errors) > 0 {
		t.Fatalf("errors: %v", rep.Errors)
	}
	if !slices.Equal(rep.Shed, old[:5]) {
		t.Errorf("shed %v, want the five oldest %v", rep.Shed, old[:5])
	}
	if rep.FilesFree < rep.Target || rep.Short {
		t.Errorf("free %d target %d short %v after the valve", rep.FilesFree, rep.Target, rep.Short)
	}
	if !v.members[fresh] {
		t.Error("a member younger than a day was shed")
	}
	for _, d := range v.ran("btrfs", "subvolume", "delete") {
		if !strings.HasPrefix(d[3], filesVersionsDir()+"/") {
			t.Errorf("deleted outside the versions: %v", d)
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
	cap, _ := v.allocation()
	v.member("immich", tickNow.Add(-3*24*time.Hour))
	young := v.member("immich", tickNow.Add(-2*time.Hour))
	v.areaBytes = cap
	rep := filesTick(context.Background(), v, tickNow)
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
		cap, target := v.allocation()
		v.member("immich", tickNow.Add(-tc.age))
		v.member("immich", tickNow.Add(-30*time.Minute)) // the hour's member: no take, so only these two count
		v.areaBytes = cap - target - v.memberBytes*3/2
		rep := filesTick(context.Background(), v, tickNow)
		if len(rep.Shed) != 1 || rep.Short != tc.short {
			t.Errorf("%s: shed %v short %v, want one shed and short=%v", tc.name, rep.Shed, rep.Short, tc.short)
		}
	}
}

// NOTHING INSTALLED, NOTHING BOUNDED: with no files aggregate there is no limit to set and no
// warning to give.
func TestATickBeforeAnyInstallDoesNothing(t *testing.T) {
	v := newUserVolume()
	delete(v.qgroups, tierQgroup(tierFiles))
	rep := filesTick(context.Background(), v, tickNow)
	if len(rep.Errors) > 0 || rep.Short || len(rep.Taken) > 0 || len(v.ran("btrfs", "qgroup", "limit")) > 0 {
		t.Errorf("a tick on an empty volume acted: %+v %v", rep, v.runs)
	}
}

// ONE AT A TIME, and a finished report waits to be collected.
func TestOneTickAtATime(t *testing.T) {
	v := newUserVolume()
	if !startFilesTick(v) {
		t.Fatal("the first tick did not start")
	}
	if startFilesTick(v) {
		t.Error("a second tick started beside the first, or over its uncollected report")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		s := filesTickResult()
		if s.Report != nil {
			break
		}
		if !s.Running || time.Now().After(deadline) {
			t.Fatalf("no report: %+v", s)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if s := filesTickResult(); s.Report != nil || s.Running {
		t.Errorf("the report was handed over twice: %+v", s)
	}
}
