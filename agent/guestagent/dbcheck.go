package guestagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"path"
	"time"

	"briard.io/agent/hass"
	"briard.io/agent/quadlet"
	"briard.io/shared/manifest"
)

// hassDBCheck is the guest half of the nightly recorder check (verbHassDBCheck): it reads Home
// Assistant's ring and hands it to hass.Nightly, with the restore that needs the ring and the
// units, which agent/hass cannot reach.
//
// A node with no Home Assistant answers with no verdict and the reason, not an error: the host
// asks only when it believes one is installed, and a stale belief is not a fault.
func hassDBCheck(ctx context.Context, x Executor, run func(string, ...string) error) (hass.DBReport, error) {
	ensureToolsOnPath()
	raw, err := x.ReadFile(manifestPath(hass.Name))
	if err != nil {
		return hass.DBReport{Why: "no Home Assistant on this volume"}, nil
	}
	m, _, err := manifest.Parse(raw)
	if err != nil {
		return hass.DBReport{}, fmt.Errorf("%s: %s does not parse: %w", verbHassDBCheck, hass.Name, err)
	}
	c, ok := hass.ConfigContainer(m)
	if !ok {
		return hass.DBReport{Why: "its manifest names no config directory"}, nil
	}
	entries, err := listMembers(ctx, x, hass.Name)
	if err != nil {
		return hass.DBReport{}, err
	}
	var ring []hass.Member
	for _, e := range entries {
		at, _ := quadlet.SnapshotMemberTime(e.Member)
		ring = append(ring, hass.Member{
			Dir: e.Member + "/" + c.Name, App: quadlet.AppSidecar(e.Member), At: at,
			Quiesced: e.Meta.Consistency == quadlet.Quiesced,
		})
	}
	live := quadlet.DataPath(hass.Name, c.Name)
	rep := hass.Nightly(ctx, x, c.Image, live, ring, func(from hass.Member) error {
		return restoreRecorder(ctx, x, run, m, string(raw), from, live)
	})
	log.Printf("hass-db: checked %s: verdict=%q restored_from=%v %s", path.Base(path.Dir(rep.Checked)), rep.Verdict, rep.RestoredFrom, rep.Why)
	return rep, nil
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
	log.Printf("hass-db: restored the recorder database from %s", path.Base(path.Dir(from.Dir)))
	return start()
}
