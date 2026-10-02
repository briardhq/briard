package guestagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"path"
	"sync"
	"time"

	"briard.io/agent/hass"
	"briard.io/agent/quadlet"
	"briard.io/shared/manifest"
)

// THE RECORDER CHECK RUNS IN THE BACKGROUND, and the restore does not.
//
// The check reads read-only members and can take minutes on a big database, while this channel
// serves one verb at a time: run inline, it would stall every status read the host makes behind
// it. So verbHassDBCheck starts it in this long-running process and answers at once, and the
// host collects the report with verbHassDBCheckResult on a later cycle.
//
// The restore stops Home Assistant and writes its data, so it stays an ordinary verb
// (verbHassDBRestore), serialised with every other operation on the app by the same channel.
// Neither runs while the other does.
//
// THE STATE IS THIS PROCESS'S. A restart loses a running check and its report; the check's own
// hold is cleared the next night, and the host, finding nothing running, starts again.
var dbChecks struct {
	sync.Mutex
	busy   bool           // a check or a restore is running
	report *hass.DBReport // the finished check's report, until the host collects it
}

// startDBCheck starts the nightly check in the background and reports whether it did. It does
// not when a check or a restore is running, or when a finished check's report has not been
// collected: that report may name a restore, and a new check must not replace it unread.
//
// NO DEADLINE ON THE CHECK. It only reads, and it walks back through as many copies as it takes
// to find a clean one; each read is bounded on its own (agent/hass's checkTimeout).
func startDBCheck(x Executor) bool {
	dbChecks.Lock()
	defer dbChecks.Unlock()
	if dbChecks.busy || dbChecks.report != nil {
		return false
	}
	dbChecks.busy = true
	go func() {
		// NOT THE REQUEST'S CONTEXT: that ends with the reply, and this outlives it.
		rep := hassDBCheck(context.Background(), x)
		dbChecks.Lock()
		dbChecks.busy, dbChecks.report = false, &rep
		dbChecks.Unlock()
	}()
	return true
}

// dbCheckResult answers whether a check is running, and hands over a finished one's report once.
func dbCheckResult() hass.DBCheckState {
	dbChecks.Lock()
	defer dbChecks.Unlock()
	s := hass.DBCheckState{Running: dbChecks.busy, Report: dbChecks.report}
	dbChecks.report = nil
	return s
}

// recorderOnVolume reads Home Assistant's manifest off the volume and the container that holds
// its config directory.
func recorderOnVolume(x Executor) (manifest.Manifest, string, manifest.Container, error) {
	raw, err := x.ReadFile(manifestPath(hass.Name))
	if err != nil {
		return manifest.Manifest{}, "", manifest.Container{}, fmt.Errorf("no Home Assistant on this volume")
	}
	m, _, err := manifest.Parse(raw)
	if err != nil {
		return manifest.Manifest{}, "", manifest.Container{}, fmt.Errorf("%s's manifest does not parse: %w", hass.Name, err)
	}
	c, ok := hass.ConfigContainer(m)
	if !ok {
		return manifest.Manifest{}, "", manifest.Container{}, fmt.Errorf("%s's manifest names no config directory", hass.Name)
	}
	return m, string(raw), c, nil
}

// recorderRing is Home Assistant's ring as the recorder check sees it.
func recorderRing(ctx context.Context, x Executor, c manifest.Container) ([]hass.Member, error) {
	entries, err := listMembers(ctx, x, hass.Name)
	if err != nil {
		return nil, err
	}
	var ring []hass.Member
	for _, e := range entries {
		at, _ := quadlet.SnapshotMemberTime(e.Member)
		ring = append(ring, hass.Member{
			Path: e.Member, Dir: e.Member + "/" + c.Name, App: quadlet.AppSidecar(e.Member), At: at,
			Quiesced: e.Meta.Consistency == quadlet.Quiesced,
		})
	}
	return ring, nil
}

// hassDBCheck is one night's check, read-only on the data: hass.Nightly over Home Assistant's
// ring. Everything that stops it becomes the report's reason.
func hassDBCheck(ctx context.Context, x Executor) hass.DBReport {
	ensureToolsOnPath()
	_, _, c, err := recorderOnVolume(x)
	if err != nil {
		return hass.DBReport{Why: err.Error()}
	}
	ring, err := recorderRing(ctx, x, c)
	if err != nil {
		return hass.DBReport{Why: "the ring could not be read: " + err.Error()}
	}
	rep := hass.Nightly(ctx, x, c.Image, quadlet.DataPath(hass.Name, c.Name), ring)
	log.Printf("hass-db: checked %s: verdict=%q candidate=%s %s", path.Base(rep.Checked), rep.Verdict, path.Base(rep.Candidate), rep.Why)
	return rep
}

// hassDBRestore puts the recorder database back from the member the check named. The member is
// READ BACK FROM THE RING, never trusted from the payload, and the gate is asked again
// (hass.Restorable) before anything stops.
func hassDBRestore(ctx context.Context, x Executor, run func(string, ...string) error, member string) error {
	dbChecks.Lock()
	if dbChecks.busy {
		dbChecks.Unlock()
		return fmt.Errorf("a recorder check is running")
	}
	dbChecks.busy = true
	dbChecks.Unlock()
	defer func() {
		dbChecks.Lock()
		dbChecks.busy = false
		dbChecks.Unlock()
	}()
	ensureToolsOnPath()
	m, raw, c, err := recorderOnVolume(x)
	if err != nil {
		return err
	}
	ring, err := recorderRing(ctx, x, c)
	if err != nil {
		return fmt.Errorf("the ring could not be read: %w", err)
	}
	var from *hass.Member
	for i := range ring {
		if ring[i].Path == member {
			from = &ring[i]
		}
	}
	if from == nil {
		return fmt.Errorf("%s has no member %q", hass.Name, member)
	}
	live := quadlet.DataPath(hass.Name, c.Name)
	if err := hass.Restorable(ctx, x, c.Image, live, *from); err != nil {
		return fmt.Errorf("not restoring: %w", err)
	}
	return restoreRecorder(ctx, x, run, m, raw, *from, c)
}

// restoreRecorder puts the recorder database back from one member: stop Home Assistant, build the
// restored data in a staged copy, take the hass-db-restore-before sample carrying the event, swap
// the copy in, start it again.
//
// NOTHING LIVE IS EDITED. The staged copy is a writable snapshot of the live subvolume, taken
// with Home Assistant stopped, and the database is put into it there. Only then is it exchanged
// with the live subvolume, in one rename(2) (RENAME_EXCHANGE): either the live data is the
// restored copy or it is what it was. So every failure deletes the staged copy and starts Home
// Assistant on its own data: nothing changed, and nothing is left stopped. The replaced subvolume
// is deleted after the start.
//
// THE SAMPLE IS THE UNDO, taken once the copy is ready and before the swap: undoing the event
// puts the damaged database back. Taken any earlier, a failure while staging would leave a
// History row saying the database was restored when it was not; a swap that fails after it
// removes it for the same reason. Nothing is compared against a *-before sample, so the restore
// registers no change of its own.
//
// ONLY THE UNITS THAT WERE RUNNING are stopped and started. Container units, never the pod: a pod
// stop unmounts the shared volume under every other service (quadlet.Rendered.ContainerUnits).
func restoreRecorder(ctx context.Context, x Executor, run func(string, ...string) error, m manifest.Manifest, rawManifest string, from hass.Member, c manifest.Container) error {
	rendered, err := quadlet.Render(m, "")
	if err != nil {
		return fmt.Errorf("render the running manifest: %w", err)
	}
	var active []string
	for _, u := range rendered.ContainerUnits {
		if _, err := x.Run(ctx, "systemctl", "is-active", "--quiet", u); err == nil {
			active = append(active, u)
		}
	}
	start := func() error {
		var errs []error
		for _, u := range active {
			errs = append(errs, run("systemctl", "start", u))
		}
		return errors.Join(errs...)
	}
	for i := len(active) - 1; i >= 0; i-- {
		if err := run("systemctl", "stop", active[i]); err != nil {
			return errors.Join(fmt.Errorf("stop %s (nothing was changed): %w", active[i], err), start())
		}
	}

	// A staging name beside the live subvolume, on the same btrfs so the exchange is a rename. A
	// leftover SUBVOLUME is a restore that died before or after its swap, and is ours to clear.
	// Anything else there is not ours, and it is in the way: `btrfs subvolume snapshot` into an
	// existing directory nests the copy inside it rather than failing.
	root := quadlet.DataRoot(hass.Name)
	staged := root + ".dbrestore"
	drop := func() {
		if _, err := x.Run(ctx, "btrfs", "subvolume", "show", staged); err == nil {
			if err := run("btrfs", "subvolume", "delete", staged); err != nil {
				log.Printf("hass-db: could not delete %s: %v", staged, err)
			}
		}
	}
	unchanged := func(what string, err error) error {
		drop()
		return errors.Join(fmt.Errorf("%s (nothing was changed): %w", what, err), start())
	}
	drop()
	if _, err := x.Run(ctx, "test", "-e", staged); err == nil {
		return unchanged("stage a copy of the data", fmt.Errorf("%s is in the way and is not a subvolume", staged))
	}
	if err := run("btrfs", "subvolume", "snapshot", root, staged); err != nil {
		return unchanged("stage a copy of the data", err)
	}
	if err := hass.RestoreDB(ctx, x, from.Dir, staged+"/"+c.Name); err != nil {
		return unchanged("put the database into the staged copy", err)
	}
	if err := run("sync", "-f", staged); err != nil {
		return unchanged("flush the staged copy", err)
	}

	// QUIESCED ONLY IF WE STOPPED IT: an app that was not running may have died rather than
	// stopped, and the weaker claim is the true one then.
	cons := quadlet.Crash
	if len(active) > 0 {
		cons = quadlet.Quiesced
	}
	at := time.Now().UTC()
	meta := quadlet.SnapshotMeta{
		Service: hass.Name, Trigger: quadlet.TriggerHassDBRestoreBefore, TakenAt: at,
		Consistency: cons, Manifest: rawManifest,
		Event: &quadlet.Event{At: at, Reasons: []quadlet.Reason{{Kind: quadlet.ReasonDBRestore, What: "Restored corrupted database"}}},
	}
	sidecar, err := json.Marshal(meta)
	if err != nil {
		return unchanged("render the undo point", err)
	}
	undo := quadlet.SnapshotMember(hass.Name, quadlet.TriggerHassDBRestoreBefore, at)
	if err := takeSnapshot(ctx, x, run, root, undo, string(sidecar)); err != nil {
		return unchanged("take the undo point", err)
	}
	recordMember(ctx, x, undo, meta)
	if err := run("mv", "--exchange", staged, root); err != nil {
		if derr := run("btrfs", "subvolume", "delete", undo); derr == nil {
			_ = run("rm", "-f", quadlet.SnapshotSidecar(undo), quadlet.AppSidecar(undo))
		} else {
			log.Printf("hass-db: the swap failed and its undo point could not be removed: %v", derr)
		}
		return unchanged("swap the staged copy in", err)
	}
	// Swapped: the live subvolume is the restored one, and `staged` now holds what it replaced.
	if err := run("sync", "-f", root); err != nil {
		log.Printf("hass-db: the swap is done but could not be flushed: %v", err)
	}
	log.Printf("hass-db: restored the recorder database from %s", path.Base(from.Path))
	err = start()
	drop()
	return err
}

// CheckRecorder and RestoreRecorder are the recorder check's two halves FOR A GUEST WITH NO HOST,
// the same accommodation TakeClockMember makes: the rigs that run Home Assistant are agent-less,
// and this is how they drive the product's own check and restore against a real one. The check
// runs in line here; its background runner is the verb's. Nothing in the product invokes them.
func CheckRecorder(ctx context.Context, x Executor) hass.DBReport { return hassDBCheck(ctx, x) }

func RestoreRecorder(ctx context.Context, x Executor, member string) error {
	run := func(name string, args ...string) error {
		if out, err := x.Run(ctx, name, args...); err != nil {
			return fmt.Errorf("%s %v: %w: %s", name, args, err, out)
		}
		return nil
	}
	return hassDBRestore(ctx, x, run, member)
}
