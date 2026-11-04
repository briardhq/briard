package guestagent

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"

	"briard.io/agent/quadlet"
)

// withoutQgroups drops the quota tree's bookkeeping from a run list, so a test about the order
// of a verb's other steps reads them as it always has.
func withoutQgroups(runs [][]string) [][]string {
	var out [][]string
	for _, r := range runs {
		if len(r) > 1 && r[0] == "btrfs" && r[1] == "qgroup" {
			continue
		}
		out = append(out, r)
	}
	return out
}

// THE IDS ARE A FORMAT. Two nodes, and two releases on one node, must name the same app's group
// the same way, because nothing is stored: the id is recomputed at every creation. So one is
// pinned, and the shape of every id is checked against the layout qgroup.go draws.
func TestQgroupIDsAreTheLayout(t *testing.T) {
	if got, want := appQgroup(tierData, "home-assistant"), "1/1150203105091"; got != want {
		t.Errorf("home-assistant's app group = %s, want %s: the id changed, and every installed volume still holds the old one", got, want)
	}
	if got, want := tierQgroup(tierData), "2/"+strconv.FormatUint(1<<40, 10); got != want {
		t.Errorf("the data aggregate = %s, want %s", got, want)
	}
	if got, want := tierQgroup(tierFiles), "2/"+strconv.FormatUint(2<<40, 10); got != want {
		t.Errorf("the files aggregate = %s, want %s", got, want)
	}
	for _, tier := range []uint64{tierData, tierFiles} {
		for _, app := range []string{"home-assistant", "jellyfin", "jellyseerr", "a"} {
			id := appQgroup(tier, app)
			n, err := strconv.ParseUint(strings.TrimPrefix(id, "1/"), 10, 64)
			if !strings.HasPrefix(id, "1/") || err != nil {
				t.Fatalf("%s: %q is not a level-1 id", app, id)
			}
			if n>>40 != tier || n >= 1<<48 {
				t.Errorf("%s: %s carries tier %d in a %d-bit id, want tier %d under 48 bits", app, id, n>>40, 64, tier)
			}
		}
	}
	// The collision a name-prefix scheme hits at once.
	if appQgroup(tierFiles, "jellyfin") == appQgroup(tierFiles, "jellyseerr") {
		t.Error("jellyfin and jellyseerr share a group")
	}
	if appQgroup(tierData, "jellyfin") == appQgroup(tierFiles, "jellyfin") {
		t.Error("an app's two tiers share a group")
	}
}

// The PATH decides the tier, and anything that is not an app's area is refused rather than
// filed under a guess.
func TestAreaOfReadsTheTierOffThePath(t *testing.T) {
	for _, tc := range []struct {
		area string
		tier uint64
		app  string
	}{
		{"/var/lib/briard/data/home-assistant", tierData, "home-assistant"},
		{"/var/lib/briard/data/home-assistant/", tierData, "home-assistant"},
		{"/var/lib/briard/files/home-assistant", tierFiles, "home-assistant"},
	} {
		tier, app, err := areaOf(tc.area)
		if err != nil || tier != tc.tier || app != tc.app {
			t.Errorf("areaOf(%s) = %d %q %v, want %d %q", tc.area, tier, app, err, tc.tier, tc.app)
		}
	}
	for _, bad := range []string{"/var/lib/briard/files", "/var/lib/briard/data/.snapshots/ha-start-x", "/data/ha", "/var/lib/briard/files/ha/x"} {
		if _, _, err := areaOf(bad); err == nil {
			t.Errorf("areaOf(%s) was accepted", bad)
		}
	}
}

// AN INSTALL MAKES AN AREA IN EVERY TIER THE APP MOUNTS, EACH BORN INTO ITS GROUP: created with
// -i, the cache and unversioned areas into the data group, each group under its tier's
// aggregate, the mounted directories inside them. An app that mounts only its data gets no other
// area. And a second install of the same app makes nothing: its subvolumes exist, and the tree
// already holds them.
func TestAnInstallBearsItsAreasIntoTheTree(t *testing.T) {
	made := map[string]bool{}
	f := &fakeExec{}
	f.runFn = func(name string, args []string) ([]byte, error) {
		if len(args) > 2 && args[0] == "subvolume" && args[1] == "show" {
			if made[args[2]] {
				return []byte(args[2]), nil
			}
			return nil, errors.New("ERROR: not a subvolume")
		}
		if len(args) > 2 && args[0] == "subvolume" && args[1] == "create" {
			made[args[len(args)-1]] = true
		}
		return nil, nil
	}
	run := func(name string, args ...string) error {
		_, err := f.Run(context.Background(), name, args...)
		return err
	}
	svc := "/var/lib/briard/data/home-assistant"
	subdirs := []string{"cache/thumbs", "data/config", "files/media", "unversioned/backups"}
	if err := ensureAppData(context.Background(), f, run, svc, subdirs); err != nil {
		t.Fatal(err)
	}
	data, files := appQgroup(tierData, "home-assistant"), appQgroup(tierFiles, "home-assistant")
	for _, want := range [][]string{
		{"btrfs", "subvolume", "create", "-i", data, svc},
		{"btrfs", "subvolume", "create", "-i", files, "/var/lib/briard/files/home-assistant"},
		{"btrfs", "subvolume", "create", "-i", data, "/var/lib/briard/cache/home-assistant"},
		{"btrfs", "subvolume", "create", "-i", data, "/var/lib/briard/unversioned/home-assistant"},
		{"mkdir", "-p", "/var/lib/briard/cache/home-assistant/thumbs"},
		{"mkdir", "-p", svc + "/config"},
		{"mkdir", "-p", "/var/lib/briard/files/home-assistant/media"},
		{"mkdir", "-p", "/var/lib/briard/unversioned/home-assistant/backups"},
	} {
		if runIndex(f, 0, want...) < 0 {
			t.Errorf("never ran %v: %v", want, f.runs)
		}
	}
	if !slices.Equal(f.qgroups[data], []string{tierQgroup(tierData)}) || !slices.Equal(f.qgroups[files], []string{tierQgroup(tierFiles)}) {
		t.Errorf("the groups are not under their aggregates: %v", f.qgroups)
	}

	before := len(f.runs)
	if err := ensureAppData(context.Background(), f, run, svc, subdirs); err != nil {
		t.Fatalf("a re-install failed: %v", err)
	}
	for _, r := range f.runs[before:] {
		if len(r) > 2 && r[0] == "btrfs" && (r[2] == "create" || r[2] == "assign") {
			t.Errorf("a re-install made %v again", r)
		}
	}

	if err := ensureAppData(context.Background(), f, run, "/var/lib/briard/data/mosquitto", []string{"data/data"}); err != nil {
		t.Fatal(err)
	}
	for _, tier := range []string{"files", "cache", "unversioned"} {
		if made["/var/lib/briard/"+tier+"/mosquitto"] {
			t.Errorf("an app that mounts only its data got a %s area", tier)
		}
	}
	if err := ensureAppData(context.Background(), f, run, svc, []string{"backups/x"}); err == nil {
		t.Error("a subdir naming no tier was made")
	}
}

// A RACER IS NOT A FAILURE. A container's start is a separate process and may make the same group
// between our read and our create; the create then fails on an id that exists, and the re-read
// is what says the tree is right.
func TestAreaGroupLosesARaceGracefully(t *testing.T) {
	f := &fakeExec{}
	racer := &racingExec{fakeExec: f}
	group, err := areaGroup(context.Background(), racer, "/var/lib/briard/data/mosquitto")
	if err != nil {
		t.Fatalf("a lost race was reported as a failure: %v", err)
	}
	if group != appQgroup(tierData, "mosquitto") {
		t.Errorf("group = %s", group)
	}
	if !racer.raced {
		t.Fatal("the racer never ran; this test proves nothing")
	}
}

// racingExec makes the whole tree the moment the code under test first reads it, as a second
// process would, so every create and assign that follows meets an id that exists.
type racingExec struct {
	*fakeExec
	raced bool
}

func (r *racingExec) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	out, err := r.fakeExec.Run(ctx, name, args...)
	if !r.raced && name == "btrfs" && len(args) > 1 && args[1] == "show" {
		r.raced = true
		for _, a := range [][]string{
			{"create", tierQgroup(tierData), dataMountRoot},
			{"create", appQgroup(tierData, "mosquitto"), dataMountRoot},
			{"assign", appQgroup(tierData, "mosquitto"), tierQgroup(tierData), dataMountRoot},
		} {
			_, _ = r.fakeExec.qgroup(a)
		}
	}
	return out, err
}

// A TREE THAT CANNOT BE MADE STOPS THE CREATION: no subvolume is born outside it, which is the
// one state the limit cannot see.
func TestNoSubvolumeIsBornOutsideTheTree(t *testing.T) {
	x := &refusingExec{fakeExec: absentDest()}
	run := func(name string, args ...string) error {
		_, err := x.Run(context.Background(), name, args...)
		return err
	}
	if err := ensureAppData(context.Background(), x, run, "/var/lib/briard/data/home-assistant", nil); err == nil {
		t.Fatal("an install whose group could not be made was accepted")
	}
	for _, r := range x.runs {
		if len(r) > 2 && r[1] == "subvolume" && r[2] == "create" {
			t.Errorf("a subvolume was created anyway: %v", r)
		}
	}
}

// refusingExec fails every qgroup write, as a volume with quotas off does.
type refusingExec struct{ *fakeExec }

func (r *refusingExec) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	if name == "btrfs" && len(args) > 1 && args[0] == "qgroup" && args[1] != "show" {
		r.runs = append(r.runs, append([]string{name}, args...))
		return []byte("ERROR: quotas not enabled"), errors.New("exit status 1")
	}
	return r.fakeExec.Run(ctx, name, args...)
}

// THE SOAK SEES THE TREE AS THE KERNEL PRINTS IT: the mode, and every group with its parents and
// nothing of the Path column, whose aggregate cells are not UTF-8. A shipped node's tick never
// asks (only the oracle reads it), and a node without the volume mounted reports nothing.
func TestTheSoakReportCarriesTheQuotaTree(t *testing.T) {
	soak := func(mounted bool) *fakeExec {
		f := &fakeExec{runFn: func(name string, args []string) ([]byte, error) {
			if name == "findmnt" {
				if !mounted {
					return nil, errors.New("exit status 1")
				}
				return []byte(fakeVolumeUUID + "\n"), nil
			}
			return nil, nil
		}}
		_ = f.WriteFile(quotaModePath(), []byte("squota\n"))
		_, _ = f.qgroup([]string{"create", tierQgroup(tierFiles), dataMountRoot})
		return f
	}
	r := gatherResources(context.Background(), soak(true), resourcesRequest{Soak: true})
	if r.QuotaMode != "squota" {
		t.Errorf("mode = %q", r.QuotaMode)
	}
	want := []string{"0/5 16384 16384 -", tierQgroup(tierFiles) + " 16384 16384 -"}
	slices.Sort(r.Qgroups)
	if !slices.Equal(r.Qgroups, want) {
		t.Errorf("tree = %q, want %q", r.Qgroups, want)
	}
	for _, tc := range []struct {
		name    string
		mounted bool
		soak    bool
	}{{"a shipped tick", true, false}, {"an unmounted node", false, true}} {
		f := soak(tc.mounted)
		r := gatherResources(context.Background(), f, resourcesRequest{Soak: tc.soak})
		if r.QuotaMode != "" || r.Qgroups != nil {
			t.Errorf("%s reported the tree: %q %q", tc.name, r.QuotaMode, r.Qgroups)
		}
		if runIndex(f, 0, append([]string{"btrfs"}, qgroupShow...)...) >= 0 {
			t.Errorf("%s read the tree", tc.name)
		}
	}
}

// A START EMPTIES THE CACHE WHEN SPACE IS SHORT: under a tenth of the app's data group's limit
// left, for an app of one container that mounts a cache. Not with room to spare, not with no
// limit, not for an app with no cache, and not for an app of two containers, where the other
// may be running with the cache open.
func TestAStartEmptiesTheCacheWhenSpaceIsShort(t *testing.T) {
	const one = `{"name":"immich","version":"1","containers":[{"name":"app","image":"ghcr.io/x/i@sha256:` +
		`aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","mounts":[{"tier":"data","dir":"app","path":"/data"},` +
		`{"tier":"cache","dir":"thumbs","path":"/cache"}],"primary":true,"port":8080,"healthPath":"/"}]}`
	two := strings.Replace(one, `}]}`, `},{"name":"ml","image":"ghcr.io/x/m@sha256:`+
		`bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}]}`, 1)
	none := strings.Replace(one, `,{"tier":"cache","dir":"thumbs","path":"/cache"}`, "", 1)
	cache := quadlet.Area("cache", "immich")
	group := appQgroup(tierData, "immich")
	for _, c := range []struct {
		name, manifest, limit string
		used                  int64
		emptied               bool
	}{
		{"short", one, "10000000000", 9_500_000_000, true},
		{"room to spare", one, "10000000000", 8_000_000_000, false},
		{"no limit", one, "", 9_500_000_000, false},
		{"no cache", none, "10000000000", 9_500_000_000, false},
		{"two containers", two, "10000000000", 9_500_000_000, false},
	} {
		f := &fakeExec{files: map[string]string{manifestPath("immich"): c.manifest}}
		f.qgroups = map[string][]string{tierQgroup(tierData): nil, group: {tierQgroup(tierData)}}
		f.qused = map[string]int64{group: c.used}
		if c.limit != "" {
			f.qlimits = map[string]string{group: c.limit}
		}
		f.runFn = func(name string, args []string) ([]byte, error) {
			if name == "btrfs" && args[1] == "show" {
				return []byte(args[2]), nil
			}
			if name == "ls" && args[1] == cache {
				return []byte("thumbs\n"), nil
			}
			return nil, nil
		}
		if _, err := cacheAtStart(context.Background(), f, "immich"); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got := runIndex(f, 0, "btrfs", "subvolume", "delete", cache) >= 0; got != c.emptied {
			t.Errorf("%s: emptied %t, want %t: %v", c.name, got, c.emptied, f.runs)
		}
	}
}
