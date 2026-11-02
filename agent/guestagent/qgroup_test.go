package guestagent

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"
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
	if got, want := appQgroup(tierService, "home-assistant"), "1/1150203105091"; got != want {
		t.Errorf("home-assistant's service group = %s, want %s: the id changed, and every installed volume still holds the old one", got, want)
	}
	if got, want := tierQgroup(tierService), "2/"+strconv.FormatUint(1<<40, 10); got != want {
		t.Errorf("the service aggregate = %s, want %s", got, want)
	}
	if got, want := tierQgroup(tierUser), "2/"+strconv.FormatUint(2<<40, 10); got != want {
		t.Errorf("the user aggregate = %s, want %s", got, want)
	}
	for _, tier := range []uint64{tierService, tierUser} {
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
	if appQgroup(tierUser, "jellyfin") == appQgroup(tierUser, "jellyseerr") {
		t.Error("jellyfin and jellyseerr share a group")
	}
	if appQgroup(tierService, "jellyfin") == appQgroup(tierUser, "jellyfin") {
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
		{"/var/lib/briard/home-assistant", tierService, "home-assistant"},
		{"/var/lib/briard/home-assistant/", tierService, "home-assistant"},
		{"/var/lib/briard/user/home-assistant", tierUser, "home-assistant"},
	} {
		tier, app, err := areaOf(tc.area)
		if err != nil || tier != tc.tier || app != tc.app {
			t.Errorf("areaOf(%s) = %d %q %v, want %d %q", tc.area, tier, app, err, tc.tier, tc.app)
		}
	}
	for _, bad := range []string{"/var/lib/briard/user", "/var/lib/briard/.snapshots/ha-start-x", "/data/ha", "/var/lib/briard/user/ha/x"} {
		if _, _, err := areaOf(bad); err == nil {
			t.Errorf("areaOf(%s) was accepted", bad)
		}
	}
}

// AN INSTALL MAKES BOTH AREAS, EACH BORN INTO ITS TIER: the service's data and the app's user
// area, each created with -i to its own group, each group under its tier's aggregate. And a
// second install of the same app makes nothing: its subvolumes exist, and the tree already
// holds them.
func TestAnInstallBearsBothAreasIntoTheTree(t *testing.T) {
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
	svc, user := "/var/lib/briard/home-assistant", "/var/lib/briard/user/home-assistant"
	if err := ensureServiceData(context.Background(), f, run, svc, []string{"app"}); err != nil {
		t.Fatal(err)
	}
	for _, want := range [][]string{
		{"btrfs", "subvolume", "create", "-i", appQgroup(tierService, "home-assistant"), svc},
		{"btrfs", "subvolume", "create", "-i", appQgroup(tierUser, "home-assistant"), user},
	} {
		if runIndex(f, 0, want...) < 0 {
			t.Errorf("never ran %v: %v", want, f.runs)
		}
	}
	if !slices.Equal(f.qgroups[appQgroup(tierService, "home-assistant")], []string{tierQgroup(tierService)}) ||
		!slices.Equal(f.qgroups[appQgroup(tierUser, "home-assistant")], []string{tierQgroup(tierUser)}) {
		t.Errorf("the groups are not under their aggregates: %v", f.qgroups)
	}

	before := len(f.runs)
	if err := ensureServiceData(context.Background(), f, run, svc, []string{"app"}); err != nil {
		t.Fatalf("a re-install failed: %v", err)
	}
	for _, r := range f.runs[before:] {
		if len(r) > 2 && r[0] == "btrfs" && (r[2] == "create" || r[2] == "assign") {
			t.Errorf("a re-install made %v again", r)
		}
	}
}

// A RACER IS NOT A FAILURE. A container's start is a separate process and may make the same group
// between our read and our create; the create then fails on an id that exists, and the re-read
// is what says the tree is right.
func TestAreaGroupLosesARaceGracefully(t *testing.T) {
	f := &fakeExec{}
	racer := &racingExec{fakeExec: f}
	group, err := areaGroup(context.Background(), racer, "/var/lib/briard/mosquitto")
	if err != nil {
		t.Fatalf("a lost race was reported as a failure: %v", err)
	}
	if group != appQgroup(tierService, "mosquitto") {
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
			{"create", tierQgroup(tierService), dataMountRoot},
			{"create", appQgroup(tierService, "mosquitto"), dataMountRoot},
			{"assign", appQgroup(tierService, "mosquitto"), tierQgroup(tierService), dataMountRoot},
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
	if err := ensureServiceData(context.Background(), x, run, "/var/lib/briard/home-assistant", nil); err == nil {
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
		_, _ = f.qgroup([]string{"create", tierQgroup(tierUser), dataMountRoot})
		return f
	}
	r := gatherResources(context.Background(), soak(true), resourcesRequest{Soak: true})
	if r.QuotaMode != "squota" {
		t.Errorf("mode = %q", r.QuotaMode)
	}
	want := []string{"0/5 16384 16384 -", tierQgroup(tierUser) + " 16384 16384 -"}
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
