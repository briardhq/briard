package guestagent

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"briard.io/agent/quadlet"
)

// THE USER TIER'S TICK: the user ring and the allocation rule, one job every ten minutes.
//
// THE USER RING is the History mechanism with a file-version product's retention: one trigger,
// the clock, hourly, taken live (crash-consistent always; nothing evaluates it), kept hourly for
// a day, daily for a week, weekly to thirty days, nothing older. Its members sit under
// userSnapshots with the service ring's names and sidecars, and each is born into its app's user
// group, so history spends the household's own cap.
//
// THE ALLOCATION RULE gives the user tier whatever the service tier does not need, and keeps it
// there by shedding history when the household's files need the room:
//
//	userCap  = volume − svcUsed − max(20 % × svcUsed, 300 MB) − max(1 GB, 2 % × volume)
//	userFree = userCap − referenced(user tier)
//	target   = max(1 GB, 5 % × userCap)
//
// The cap is the user aggregate's limit, re-set only when it changes. The service tier has no
// limit of its own: its reserve is the cap's headroom, so a growing service lowers the cap and the
// valve answers. THE VALVE deletes the oldest user-ring member older than a day, anywhere, waits
// for the space to come back, and re-reads, while userFree is under the target. It is blind by
// design -- simple quotas carry no per-snapshot exclusive number -- and it never touches a service
// ring or a member younger than a day: when those hold the space, the uploader meets EDQUOT and
// the house keeps running. Short says the valve could not buy five days of history, which is the
// household's warning before the uploader's error.
//
// THE HOST SCHEDULES IT AND TELLS THE HOUSEHOLD; the work is here because only the guest mounts
// the volume. It runs in the guest's background, one at a time, like the recorder check: a
// valve that waits on the cleaner must hold neither the host's loop nor the channel.

const (
	userClockInterval = time.Hour
	userKeepHourly    = 24 * time.Hour
	userKeepDaily     = 7 * 24 * time.Hour
	userKeepWeekly    = 30 * 24 * time.Hour
	// userEligibleAge is the valve's floor: nothing younger is shed, so a mistake made today can
	// still be undone tomorrow.
	userEligibleAge = 24 * time.Hour
	// userShortAge is what the household is warned below: history the valve had to cut under
	// five days.
	userShortAge = 5 * 24 * time.Hour
)

// userSnapshots is the user ring's directory, a plain directory beside the user areas.
func userSnapshots() string { return filepath.Join(userRoot(), ".snapshots") }

// UserTickReport is what one tick did and found. Bytes throughout.
type UserTickReport struct {
	At          time.Time `json:"at"`
	Volume      int64     `json:"volume"`
	ServiceUsed int64     `json:"service_used"`
	UserUsed    int64     `json:"user_used"`
	UserCap     int64     `json:"user_cap"`
	UserFree    int64     `json:"user_free"`
	Target      int64     `json:"target"`
	Taken       []string  `json:"taken,omitempty"`  // ring members the clock took
	Pruned      []string  `json:"pruned,omitempty"` // members retention let go
	Shed        []string  `json:"shed,omitempty"`   // members the valve deleted for room
	// Short: userFree was under the target while the oldest member the valve could shed was under
	// five days old, or there was none left to shed.
	Short  bool     `json:"short,omitempty"`
	Errors []string `json:"errors,omitempty"`
}

// UserTickState answers whether a tick is running, with a finished one's report, once.
type UserTickState struct {
	Running bool            `json:"running"`
	Report  *UserTickReport `json:"report,omitempty"`
}

var userTicks struct {
	sync.Mutex
	busy   bool
	report *UserTickReport
}

// startUserTick starts a tick in the background unless one is running or a finished one's report
// has not been collected, and answers whether it did.
func startUserTick(x Executor) bool {
	userTicks.Lock()
	defer userTicks.Unlock()
	if userTicks.busy || userTicks.report != nil {
		return false
	}
	userTicks.busy = true
	go func() {
		// NOT THE REQUEST'S CONTEXT: that ends with the reply, and this outlives it.
		rep := userTick(context.Background(), x, time.Now().UTC())
		userTicks.Lock()
		userTicks.busy, userTicks.report = false, &rep
		userTicks.Unlock()
	}()
	return true
}

func userTickResult() UserTickState {
	userTicks.Lock()
	defer userTicks.Unlock()
	s := UserTickState{Running: userTicks.busy, Report: userTicks.report}
	userTicks.report = nil
	return s
}

// userMember is one member of a user ring.
type userMember struct {
	app, path string
	at        time.Time
}

func userTick(ctx context.Context, x Executor, now time.Time) UserTickReport {
	rep := UserTickReport{At: now}
	failed := func(what string, err error) { rep.Errors = append(rep.Errors, what+": "+err.Error()) }
	run := func(name string, args ...string) error {
		out, err := x.Run(ctx, name, args...)
		if err != nil {
			return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
		}
		return nil
	}

	// THE RING: a member for every area whose newest is an hour old, and its retention at the take.
	ring := userRing(ctx, x)
	for _, app := range userApps(ctx, x) {
		var newest time.Time
		for _, m := range ring {
			if m.app == app && m.at.After(newest) {
				newest = m.at
			}
		}
		if now.Sub(newest) < userClockInterval {
			continue
		}
		member := filepath.Join(userSnapshots(), filepath.Base(quadlet.SnapshotMember(app, quadlet.TriggerClock, now)))
		sidecar, _ := json.Marshal(quadlet.SnapshotMeta{Service: app, Trigger: quadlet.TriggerClock, TakenAt: now, Consistency: quadlet.Crash})
		if err := run("mkdir", "-p", userSnapshots()); err != nil {
			failed("make the user ring's directory", err)
			break
		}
		if err := takeSnapshot(ctx, x, run, userArea(app), member, string(sidecar)); err != nil {
			failed("take "+app+"'s user member", err)
			continue
		}
		rep.Taken = append(rep.Taken, member)
		ring = append(ring, userMember{app: app, path: member, at: now})
		var mine []userMember
		for _, m := range ring {
			if m.app == app {
				mine = append(mine, m)
			}
		}
		for _, m := range userRetention(mine, now) {
			if err := deleteUserMember(run, m.path); err != nil {
				failed("prune "+m.path, err)
				continue
			}
			rep.Pruned = append(rep.Pruned, m.path)
			ring = slices.DeleteFunc(ring, func(r userMember) bool { return r.path == m.path })
		}
	}

	// THE ALLOCATION. Nothing to bound until an app has a user area.
	volume, err := volumeSize(ctx, x)
	if err != nil {
		failed("read the volume's size", err)
		return rep
	}
	tree, err := readQgroups(ctx, x)
	if err != nil {
		failed("read the quota tree", err)
		return rep
	}
	userAgg := tierQgroup(tierUser)
	if _, ok := tree[userAgg]; !ok {
		return rep
	}
	measure := func() {
		rep.Volume, rep.ServiceUsed, rep.UserUsed = volume, tree[tierQgroup(tierService)].referenced, tree[userAgg].referenced
		rep.UserCap, rep.Target = userAllocation(volume, rep.ServiceUsed)
		rep.UserFree = rep.UserCap - rep.UserUsed
	}
	measure()
	if tree[userAgg].limit != rep.UserCap {
		if err := run("btrfs", "qgroup", "limit", strconv.FormatInt(rep.UserCap, 10), userAgg, dataMountRoot); err != nil {
			failed("set the user tier's limit", err)
		}
	}

	// THE VALVE: the oldest eligible member, anywhere, until there is room or nothing to shed.
	eligible := slices.DeleteFunc(slices.Clone(ring), func(m userMember) bool { return now.Sub(m.at) <= userEligibleAge })
	slices.SortFunc(eligible, func(a, b userMember) int { return a.at.Compare(b.at) })
	for rep.UserFree < rep.Target {
		if len(eligible) == 0 || now.Sub(eligible[0].at) < userShortAge {
			rep.Short = true
		}
		if len(eligible) == 0 {
			break
		}
		m := eligible[0]
		eligible = eligible[1:]
		if err := deleteUserMember(run, m.path); err != nil {
			failed("shed "+m.path, err)
			continue
		}
		rep.Shed = append(rep.Shed, m.path)
		// The space comes back when the cleaner has dropped the subvolume, not when the delete
		// returns; only then does the re-read count it.
		if err := run("btrfs", "subvolume", "sync", dataMountRoot); err != nil {
			failed("wait for the cleaner", err)
			break
		}
		if tree, err = readQgroups(ctx, x); err != nil {
			failed("re-read the quota tree", err)
			break
		}
		measure()
	}
	return rep
}

// userAllocation is the rule's cap and target for a volume of this size holding this much
// service state. A cap below what the user tier already holds is still the cap: the limit takes
// it, and the next write meets EDQUOT while the valve makes room.
func userAllocation(volume, svcUsed int64) (userCap, target int64) {
	const mb, gb = 1_000_000, 1_000_000_000
	headroom := max(svcUsed/5, 300*mb)
	slack := max(gb, volume/50)
	userCap = max(volume-svcUsed-headroom-slack, 0)
	return userCap, max(gb, userCap/20)
}

// userRetention is the members of one app's ring that its retention lets go: every one younger
// than a day is kept; past that, the newest of each day for a week, then of each week to thirty
// days; nothing older. Days and weeks are UTC, like the members' names.
func userRetention(ring []userMember, now time.Time) []userMember {
	ring = slices.Clone(ring)
	slices.SortFunc(ring, func(a, b userMember) int { return b.at.Compare(a.at) }) // newest first
	seen := map[string]bool{}
	var drop []userMember
	for _, m := range ring {
		age := now.Sub(m.at)
		var bucket string
		switch {
		case age < userKeepHourly:
			continue
		case age < userKeepDaily:
			bucket = "day " + m.at.UTC().Format(time.DateOnly)
		case age < userKeepWeekly:
			y, w := m.at.UTC().ISOWeek()
			bucket = fmt.Sprintf("week %d-%d", y, w)
		}
		if bucket == "" || seen[bucket] {
			drop = append(drop, m)
			continue
		}
		seen[bucket] = true
	}
	return drop
}

// userApps is every app with a user area: the subvolumes under userRoot.
func userApps(ctx context.Context, x Executor) []string {
	out, err := x.Run(ctx, "ls", "-1", userRoot())
	if err != nil {
		return nil // no user root yet: nothing has been installed
	}
	var apps []string
	for _, n := range strings.Fields(string(out)) {
		if !strings.HasPrefix(n, ".") && safeUnitName(n) == nil {
			apps = append(apps, n)
		}
	}
	return apps
}

// userRing is every member of every user ring, read off the names; anything whose name is not
// one of ours is not counted, and so never deleted.
func userRing(ctx context.Context, x Executor) []userMember {
	out, err := x.Run(ctx, "ls", "-1", userSnapshots())
	if err != nil {
		return nil
	}
	var ring []userMember
	for _, n := range strings.Fields(string(out)) {
		if strings.HasSuffix(n, ".json") {
			continue
		}
		if app, tr, at, ok := quadlet.ParseSnapshotMember(n); ok && tr == quadlet.TriggerClock {
			ring = append(ring, userMember{app: app, path: filepath.Join(userSnapshots(), n), at: at})
		}
	}
	return ring
}

// deleteUserMember removes a member and then its sidecar, so a member never outlives its label.
func deleteUserMember(run func(string, ...string) error, member string) error {
	if err := run("btrfs", "subvolume", "delete", member); err != nil {
		return err
	}
	return run("rm", "-f", quadlet.SnapshotSidecar(member))
}

// volumeSize is the volume's size as btrfs states it.
func volumeSize(ctx context.Context, x Executor) (int64, error) {
	out, err := x.Run(ctx, "btrfs", "filesystem", "usage", "-b", dataMountRoot)
	if err != nil {
		return 0, fmt.Errorf("btrfs filesystem usage: %w: %s", err, strings.TrimSpace(string(out)))
	}
	for _, line := range strings.Split(string(out), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "Device size:"); ok {
			return strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		}
	}
	return 0, fmt.Errorf("btrfs filesystem usage names no device size")
}
