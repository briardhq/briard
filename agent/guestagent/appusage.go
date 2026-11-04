package guestagent

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"briard.io/agent/quadlet"
	"briard.io/shared/dashboard"
)

// AN APP'S SHARE OF THE VOLUME, as its page shows it: per tier, what its group holds -- its
// current data and its history together, exact and on disk -- and how much of that is history.
//
// THE SPLIT IS ONE WALK OF THE CURRENT DATA, never of the history. Simple quotas charge a byte to
// the subvolume that wrote it, so a ring member's own group holds almost nothing and no group
// separates the two; and adding up the members counts every byte they share once per member.
// compsize reads the extent tree and counts each extent once, so what it finds under the current
// data is that data on disk, and the history is the group less it -- what deleting the history
// would free. du will not do: on btrfs it reports a file's bytes before compression, whatever
// its flags, and the volume is compressed. The same walk reports those bytes too, which is what
// the files would take uncompressed.
//
// THE WALK IS PULLED, NEVER SCHEDULED: only a request starts one, only when the last walk of that
// area is older than usageFresh or measured a different subvolume (an undo swaps the area for
// another), and one at a time in the background -- a library of photos is minutes of reading.
// The totals, the history's count and the cap are read fresh on every request.

const usageFresh = 10 * time.Minute

// areaWalk is one finished walk of an area: which subvolume it measured, when, and what it found
// -- or that it failed, which is kept as long as a result would be, so a page left open does not
// walk again every time it asks.
type areaWalk struct {
	root        string
	at          time.Time
	disk, files int64
	failed      bool
}

var usageWalks struct {
	sync.Mutex
	busy bool
	done map[string]areaWalk // by area
}

// appUsage answers an app's share, starting a walk of each area whose last one is stale.
func appUsage(ctx context.Context, x Executor, app string, now time.Time) (dashboard.AppUsage, error) {
	if err := safeUnitName(app); err != nil {
		return dashboard.AppUsage{}, err
	}
	tree, err := readQgroups(ctx, x)
	if err != nil {
		return dashboard.AppUsage{}, err
	}
	var stale []string
	tier := func(tier uint64, area string, members int) *dashboard.TierUsage {
		g, ok := tree[appQgroup(tier, app)]
		if !ok {
			return nil
		}
		t := &dashboard.TierUsage{Total: g.referenced, Cap: g.limit, Members: members}
		root, err := subvolumeID(ctx, x, area)
		if err != nil {
			return t // no current data, only history: nothing to walk
		}
		usageWalks.Lock()
		w, ok := usageWalks.done[area]
		usageWalks.Unlock()
		if ok && w.root == root && !w.failed {
			t.Measured, t.Current, t.Uncompressed, t.History = true, w.disk, w.files, max(t.Total-w.disk, 0)
		}
		if !ok || w.root != root || now.Sub(w.at) >= usageFresh {
			stale = append(stale, area)
		}
		return t
	}
	var mine int
	for _, m := range fileVersions(ctx, x) {
		if m.app == app {
			mine++
		}
	}
	u := dashboard.AppUsage{
		Data:  tier(tierData, quadlet.DataRoot(app), len(ringMembers(ctx, x, app))),
		Files: tier(tierFiles, filesArea(app), mine),
	}
	if len(stale) > 0 {
		startUsageWalk(x, stale, now)
		u.Measuring = true
	}
	return u, nil
}

// startUsageWalk walks the areas in the background, unless a walk is already running. Each walk
// is stamped with the moment it was asked for.
func startUsageWalk(x Executor, areas []string, asked time.Time) {
	usageWalks.Lock()
	defer usageWalks.Unlock()
	if usageWalks.busy {
		return
	}
	usageWalks.busy = true
	go func() {
		// NOT THE REQUEST'S CONTEXT: that ends with the reply, and this outlives it.
		ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
		defer cancel()
		for _, area := range areas {
			w, err := walkArea(ctx, x, area, asked)
			if err != nil {
				log.Printf("app usage: %v", err)
			}
			usageWalks.Lock()
			if usageWalks.done == nil {
				usageWalks.done = map[string]areaWalk{}
			}
			usageWalks.done[area] = w
			usageWalks.Unlock()
		}
		usageWalks.Lock()
		usageWalks.busy = false
		usageWalks.Unlock()
	}()
}

// walkArea measures one area's current data. The subvolume is named before the walk, so a swap
// during it makes the next request walk again rather than credit the new subvolume with the old.
// compsize answers "No files." for an empty tree, the area's existence having been asked first;
// a file that vanishes mid-walk is its complaint and not the total's, so the total is read
// whenever there is one.
func walkArea(ctx context.Context, x Executor, area string, asked time.Time) (areaWalk, error) {
	w := areaWalk{at: asked, failed: true}
	root, err := subvolumeID(ctx, x, area)
	if err != nil {
		return w, err
	}
	w.root = root
	out, err := x.Run(ctx, "compsize", "-b", "-x", area)
	for _, line := range strings.Split(string(out), "\n") {
		// TOTAL <percent> <disk usage> <uncompressed> <referenced>
		if f := strings.Fields(line); len(f) == 5 && f[0] == "TOTAL" {
			var errDisk, errFiles error
			w.disk, errDisk = strconv.ParseInt(f[2], 10, 64)
			w.files, errFiles = strconv.ParseInt(f[4], 10, 64)
			if errDisk != nil || errFiles != nil {
				return w, fmt.Errorf("compsize %s: unreadable total %q", area, line)
			}
			w.failed = false
			return w, nil
		}
	}
	if strings.Contains(string(out), "No files.") {
		w.failed = false
		return w, nil
	}
	return w, fmt.Errorf("compsize %s: %v: %s", area, err, strings.TrimSpace(string(out)))
}

// subvolumeID is btrfs's id for the subvolume at path; an error when there is none.
func subvolumeID(ctx context.Context, x Executor, path string) (string, error) {
	out, err := x.Run(ctx, "btrfs", "inspect-internal", "rootid", path)
	if err != nil {
		return "", fmt.Errorf("btrfs inspect-internal rootid %s: %w: %s", path, err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}
