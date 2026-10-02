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

// dbCheckBudget bounds a background check: quick_check on every candidate of a large database.
const dbCheckBudget = 30 * time.Minute

// startDBCheck starts the nightly check in the background and reports whether it did. It does
// not when a check or a restore is already running.
func startDBCheck(x Executor) bool {
	dbChecks.Lock()
	defer dbChecks.Unlock()
	if dbChecks.busy {
		return false
	}
	dbChecks.busy, dbChecks.report = true, nil
	go func() {
		// NOT THE REQUEST'S CONTEXT: that ends with the reply, and this outlives it.
		ctx, cancel := context.WithTimeout(context.Background(), dbCheckBudget)
		defer cancel()
		rep := hassDBCheck(ctx, x)
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
	return restoreRecorder(ctx, x, run, m, raw, *from, live)
}

// restoreRecorder puts the recorder database back from one member: stop Home Assistant, take the
// hass-db-restore-before sample carrying the event, copy the database, start it again.
//
// THE SAMPLE IS THE UNDO, and it exists before anything is touched: undoing the event puts the
// damaged database back. Nothing is compared against a *-before sample, so the restore registers
// no change of its own.
//
// ONLY THE UNITS THAT WERE RUNNING are stopped and started. Container units, never the pod: a pod
// stop unmounts the shared volume under every other service (quadlet.Rendered.ContainerUnits).
//
// A failure before the live database is touched starts Home Assistant again on what it had. A
// failure after leaves it stopped: starting it on a half-restored directory would be silent
// damage, and a stopped app with an undo point is recoverable.
func restoreRecorder(ctx context.Context, x Executor, run func(string, ...string) error, m manifest.Manifest, rawManifest string, from hass.Member, live string) error {
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
		return errors.Join(err, start())
	}
	member := quadlet.SnapshotMember(hass.Name, quadlet.TriggerHassDBRestoreBefore, at)
	if err := takeSnapshot(ctx, x, run, quadlet.DataRoot(hass.Name), member, string(sidecar)); err != nil {
		return errors.Join(fmt.Errorf("take the undo point (nothing was changed): %w", err), start())
	}
	recordMember(ctx, x, member, meta)
	touched, err := hass.RestoreDB(ctx, x, from.Dir, live)
	switch {
	case err != nil && touched:
		return fmt.Errorf("put the database back (Home Assistant is left stopped; undoing the event restores the damaged one): %w", err)
	case err != nil:
		return errors.Join(fmt.Errorf("copy the database (nothing was changed): %w", err), start())
	}
	log.Printf("hass-db: restored the recorder database from %s", path.Base(from.Path))
	return start()
}
