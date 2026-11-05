package guestagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"briard.io/agent/quadlet"
)

// THE FILES TIER'S TICK: the versions and the allocation rule, one job every five minutes.
//
// THE VERSIONS are the History mechanism with a file-version product's retention: one trigger,
// the clock, hourly, taken live (crash-consistent always; nothing evaluates it), kept hourly for
// a day, daily for a week, weekly to thirty days, nothing older. They sit under
// filesVersionsDir with the History's names and sidecars, and each is born into its app's files
// group, so its Versions spend that app's own cap.
//
// THE ALLOCATION RULE gives the files tier whatever the data tier does not need, and keeps it
// there by shedding old Versions when the household's files need the room:
//
//	slack    = (raw used − charged) + one metadata chunk     what btrfs keeps for itself, and room to grow it
//	filesCap  = volume − dataUsed − max(20 % × dataUsed, 300 MB) − slack
//	filesFree = filesCap − referenced(files tier)
//	target   = max(1 GB, 5 % × filesCap)
//
// dataUsed is everything charged outside the files tier. The slack is measured, not guessed: what
// the disk holds (each metadata copy counted) less what the quota tree charges is the
// filesystem's own trees and the second copy of every metadata block, and one more metadata chunk
// is kept free so btrfs can always allocate the next one.
//
// The cap is the files aggregate's limit, re-set only when it changes. The data tier has no
// limit of its own: its reserve is the cap's headroom, so a growing app lowers the cap and the
// valve answers. THE VALVE deletes the oldest version older than a day, of any app, waits
// for the space to come back, and re-reads, while filesFree is under the target. It is blind by
// design -- simple quotas carry no per-snapshot exclusive number -- and it never touches an app's
// History or a version younger than a day: when those hold the space, the uploader meets EDQUOT and
// the house keeps running. Short says the valve could not keep five days of Versions, which is the
// household's warning before the uploader's error.
//
// NO APP TAKES THE LAST OF ITS TIER'S FREE SPACE. Each app's group in each tier is limited to
//
//	appCap = max(90 % × (used + free), used + free − R)
//
// where free is what the tier has left (the volume's physical free space for a data area, filesFree
// for a files area) and R = max(300 MB, 1 % of what the tier divides: the volume, or filesCap). An
// app growing alone does not move its own cap, since its growth is the tier's loss; it stops with
// a tenth of the two together or R left for the others, whichever is less. Its older copies count, as
// it does in the tier. An app's room is never less than free − R, the valve holds filesFree at a
// target no smaller than R, and a data area's free space is filesFree and the headroom: while the
// household has not been warned, no app in either tier is at its cap.
//
// THE HOST SCHEDULES IT AND TELLS THE HOUSEHOLD; the work is here because only the guest mounts
// the volume. It runs in the guest's background, one at a time, like the recorder check: a
// valve that waits on the cleaner must hold neither the host's loop nor the channel.

const (
	versionInterval    = time.Hour
	versionsKeepHourly = 24 * time.Hour
	versionsKeepDaily  = 7 * 24 * time.Hour
	versionsKeepWeekly = 30 * 24 * time.Hour
	// versionEligibleAge is the valve's floor: nothing younger is shed, so a mistake made today can
	// still be undone tomorrow.
	versionEligibleAge = 24 * time.Hour
	// versionsShortAge is what the household is warned below: Versions the valve had to cut under
	// five days.
	versionsShortAge = 5 * 24 * time.Hour
)

// filesVersionsDir is the Versions' directory, a plain directory beside the files areas.
func filesVersionsDir() string { return filepath.Join(filesRoot(), ".snapshots") }

// FilesTickReport is what one tick did and found. Bytes throughout.
type FilesTickReport struct {
	At        time.Time `json:"at"`
	Volume    int64     `json:"volume"`
	Slack     int64     `json:"slack"`
	DataUsed  int64     `json:"data_used"`
	FilesUsed int64     `json:"files_used"`
	FilesCap  int64     `json:"files_cap"`
	FilesFree int64     `json:"files_free"`
	Target    int64     `json:"target"`
	Taken     []string  `json:"taken,omitempty"`  // ring members the clock took
	Pruned    []string  `json:"pruned,omitempty"` // members retention let go
	Shed      []string  `json:"shed,omitempty"`   // members the valve deleted for room
	// Short: filesFree was under the target while the oldest member the valve could shed was under
	// five days old, or there was none left to shed.
	Short  bool     `json:"short,omitempty"`
	Errors []string `json:"errors,omitempty"`
}

// FilesTickState answers whether a tick is running, with a finished one's report, once.
type FilesTickState struct {
	Running bool             `json:"running"`
	Report  *FilesTickReport `json:"report,omitempty"`
}

var filesTicks struct {
	sync.Mutex
	busy   bool
	report *FilesTickReport
}

// startFilesTick starts a tick in the background unless one is running or a finished one's report
// has not been collected, and answers whether it did.
func startFilesTick(x Executor) bool {
	filesTicks.Lock()
	defer filesTicks.Unlock()
	if filesTicks.busy || filesTicks.report != nil {
		return false
	}
	filesTicks.busy = true
	go func() {
		// NOT THE REQUEST'S CONTEXT: that ends with the reply, and this outlives it.
		rep := filesTick(context.Background(), x, time.Now().UTC())
		// The ring has just changed, and Files names its members: the view follows it here,
		// and catches anything else that moved since it was last refreshed.
		if err := refreshFilesView(context.Background(), x); err != nil {
			log.Printf("files tick: %v", err)
		}
		filesTicks.Lock()
		filesTicks.busy, filesTicks.report = false, &rep
		filesTicks.Unlock()
	}()
	return true
}

func filesTickResult() FilesTickState {
	filesTicks.Lock()
	defer filesTicks.Unlock()
	s := FilesTickState{Running: filesTicks.busy, Report: filesTicks.report}
	filesTicks.report = nil
	return s
}

// fileVersion is one of an app's versions.
type fileVersion struct {
	app, path string
	at        time.Time
}

func filesTick(ctx context.Context, x Executor, now time.Time) FilesTickReport {
	rep := FilesTickReport{At: now}
	failed := func(what string, err error) { rep.Errors = append(rep.Errors, what+": "+err.Error()) }
	run := func(name string, args ...string) error {
		out, err := x.Run(ctx, name, args...)
		if err != nil {
			return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
		}
		return nil
	}

	// THE RING: a member for every area whose newest is an hour old, and its retention at the take.
	ring := fileVersions(ctx, x)
	for _, app := range filesApps(ctx, x) {
		var newest time.Time
		for _, m := range ring {
			if m.app == app && m.at.After(newest) {
				newest = m.at
			}
		}
		if now.Sub(newest) < versionInterval {
			continue
		}
		member := filepath.Join(filesVersionsDir(), filepath.Base(quadlet.SnapshotMember(app, quadlet.TriggerClock, now)))
		sidecar, _ := json.Marshal(quadlet.SnapshotMeta{App: app, Trigger: quadlet.TriggerClock, TakenAt: now, Consistency: quadlet.Crash})
		if err := run("mkdir", "-p", filesVersionsDir()); err != nil {
			failed("make the versions' directory", err)
			break
		}
		if err := takeSnapshot(ctx, x, run, filesArea(app), member, string(sidecar)); err != nil {
			failed("take "+app+"'s version", err)
			continue
		}
		rep.Taken = append(rep.Taken, member)
		ring = append(ring, fileVersion{app: app, path: member, at: now})
		var mine []fileVersion
		for _, m := range ring {
			if m.app == app {
				mine = append(mine, m)
			}
		}
		for _, m := range versionRetention(mine, now) {
			if err := deleteFileVersion(run, m.path); err != nil {
				failed("prune "+m.path, err)
				continue
			}
			rep.Pruned = append(rep.Pruned, m.path)
			ring = slices.DeleteFunc(ring, func(r fileVersion) bool { return r.path == m.path })
		}
	}

	// THE ALLOCATION. Nothing to bound until an app has a files area.
	use, err := volumeUsage(ctx, x)
	if err != nil {
		failed("read the volume's usage", err)
		return rep
	}
	tree, err := readQgroups(ctx, x)
	if err != nil {
		failed("read the quota tree", err)
		return rep
	}
	filesAgg, dataAgg := tierQgroup(tierFiles), tierQgroup(tierData)
	if _, ok := tree[filesAgg]; !ok {
		return rep
	}
	// The slack once a tick: what btrfs keeps for itself does not move while the valve deletes.
	slack := fsSlack(use, tree.charged())
	measure := func() {
		rep.Volume, rep.Slack, rep.FilesUsed = use.size, slack, tree[filesAgg].referenced
		rep.DataUsed = tree.charged() - rep.FilesUsed
		rep.FilesCap, rep.Target = filesAllocation(use.size, slack, rep.DataUsed)
		rep.FilesFree = rep.FilesCap - rep.FilesUsed
	}
	measure()
	if tree[filesAgg].limit != rep.FilesCap {
		if err := run("btrfs", "qgroup", "limit", strconv.FormatInt(rep.FilesCap, 10), filesAgg, dataMountRoot); err != nil {
			failed("set the files tier's limit", err)
		}
	}

	// THE VALVE: the oldest eligible member, anywhere, until there is room or nothing to shed.
	eligible := slices.DeleteFunc(slices.Clone(ring), func(m fileVersion) bool { return now.Sub(m.at) <= versionEligibleAge })
	slices.SortFunc(eligible, func(a, b fileVersion) int { return a.at.Compare(b.at) })
	for rep.FilesFree < rep.Target {
		if len(eligible) == 0 || now.Sub(eligible[0].at) < versionsShortAge {
			rep.Short = true
		}
		if len(eligible) == 0 {
			break
		}
		m := eligible[0]
		eligible = eligible[1:]
		if err := deleteFileVersion(run, m.path); err != nil {
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

	// THE APPS' CAPS, from what the valve left: each tier's free space and the share it keeps back.
	tiers := map[string]struct{ free, keep int64 }{
		dataAgg:  {rep.Volume - rep.Slack - rep.DataUsed - rep.FilesUsed, max(300*mb, rep.Volume/100)},
		filesAgg: {rep.FilesFree, max(300*mb, rep.FilesCap/100)},
	}
	for id, g := range tree {
		for agg, t := range tiers {
			if !strings.HasPrefix(id, "1/") || !tree.holds(id, agg) {
				continue
			}
			if c := appCap(g.referenced+t.free, t.keep); g.limit != c {
				if err := run("btrfs", "qgroup", "limit", strconv.FormatInt(c, 10), id, dataMountRoot); err != nil {
					failed("set "+id+"'s limit", err)
				}
			}
		}
	}
	return rep
}

const mb, gb = 1_000_000, 1_000_000_000

// filesAllocation is the rule's cap and target for a volume of this size, with this much slack,
// holding this much outside the files tier. A cap below what the files tier already holds is still
// the cap: the limit takes it, and the next write meets EDQUOT while the valve makes room.
func filesAllocation(volume, slack, dataUsed int64) (filesCap, target int64) {
	headroom := max(dataUsed/5, 300*mb)
	filesCap = max(volume-dataUsed-headroom-slack, 0)
	return filesCap, max(gb, filesCap/20)
}

// appCap is an app's limit when it and its tier's free space come to `total` and the tier keeps
// back `keep`: a tenth of the total for a small app, `keep` for one over ten times it.
func appCap(total, keep int64) int64 {
	return max(total*9/10, total-keep, 0)
}

// fsSlack is the space btrfs needs that the quota tree charges to nobody: what is on the disk
// beyond what is charged (its own trees, the second copy of each metadata block), and one
// metadata chunk -- the kernel's size for it, every copy -- so the next one can always be
// allocated. A volume with no room for that chunk turns writes into ENOSPC however much data
// space is free.
func fsSlack(u volumeUse, charged int64) int64 {
	chunk := int64(256 << 20)
	if u.size > 50<<30 {
		chunk = 1 << 30
	}
	chunk = min(chunk, u.size/10)
	return max(u.used-charged, 0) + int64(float64(chunk)*u.metaCopies)
}

// versionRetention is the members of one app's ring that its retention lets go: every one younger
// than a day is kept; past that, the newest of each day for a week, then of each week to thirty
// days; nothing older. Days and weeks are UTC, like the members' names.
func versionRetention(ring []fileVersion, now time.Time) []fileVersion {
	ring = slices.Clone(ring)
	slices.SortFunc(ring, func(a, b fileVersion) int { return b.at.Compare(a.at) }) // newest first
	seen := map[string]bool{}
	var drop []fileVersion
	for _, m := range ring {
		age := now.Sub(m.at)
		var bucket string
		switch {
		case age < versionsKeepHourly:
			continue
		case age < versionsKeepDaily:
			bucket = "day " + m.at.UTC().Format(time.DateOnly)
		case age < versionsKeepWeekly:
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

// filesApps is every app with a files area: the subvolumes under filesRoot.
func filesApps(ctx context.Context, x Executor) []string {
	out, err := x.Run(ctx, "ls", "-1", filesRoot())
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

// fileVersions is every version of every app, read off the names; anything whose name is not
// one of ours is not counted, and so never deleted.
func fileVersions(ctx context.Context, x Executor) []fileVersion {
	out, err := x.Run(ctx, "ls", "-1", filesVersionsDir())
	if err != nil {
		return nil
	}
	var ring []fileVersion
	for _, n := range strings.Fields(string(out)) {
		if strings.HasSuffix(n, ".json") {
			continue
		}
		if app, tr, at, ok := quadlet.ParseSnapshotMember(n); ok && tr == quadlet.TriggerClock {
			ring = append(ring, fileVersion{app: app, path: filepath.Join(filesVersionsDir(), n), at: at})
		}
	}
	return ring
}

// deleteFileVersion removes a member and then its sidecar, so a member never outlives its label.
func deleteFileVersion(run func(string, ...string) error, member string) error {
	if err := run("btrfs", "subvolume", "delete", member); err != nil {
		return err
	}
	return run("rm", "-f", quadlet.SnapshotSidecar(member))
}

// volumeUse is the volume as btrfs states it: its size, what is on the disk (every copy of
// every block), and how many copies of metadata it keeps.
type volumeUse struct {
	size, used int64
	metaCopies float64
}

// volumeUsage reads the overall lines of `btrfs filesystem usage`; the per-profile lines start
// with their profile, so no prefix below matches one of them.
func volumeUsage(ctx context.Context, x Executor) (volumeUse, error) {
	out, err := x.Run(ctx, "btrfs", "filesystem", "usage", "-b", dataMountRoot)
	if err != nil {
		return volumeUse{}, fmt.Errorf("btrfs filesystem usage: %w: %s", err, strings.TrimSpace(string(out)))
	}
	var u volumeUse
	var errs []error
	read := func(line, prefix string) (string, bool) {
		v, ok := strings.CutPrefix(strings.TrimSpace(line), prefix)
		return strings.TrimSpace(v), ok
	}
	for _, line := range strings.Split(string(out), "\n") {
		var err error
		if v, ok := read(line, "Device size:"); ok {
			u.size, err = strconv.ParseInt(v, 10, 64)
		} else if v, ok := read(line, "Used:"); ok {
			u.used, err = strconv.ParseInt(v, 10, 64)
		} else if v, ok := read(line, "Metadata ratio:"); ok {
			u.metaCopies, err = strconv.ParseFloat(v, 64)
		}
		errs = append(errs, err)
	}
	if err := errors.Join(errs...); err != nil {
		return volumeUse{}, fmt.Errorf("btrfs filesystem usage: %w", err)
	}
	if u.size == 0 || u.used == 0 || u.metaCopies == 0 {
		return volumeUse{}, fmt.Errorf("btrfs filesystem usage names no size, used or metadata ratio")
	}
	return u, nil
}
