package guestagent

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"path"
	"time"

	"briard.io/agent/quadlet"
	"briard.io/agent/services"
	"briard.io/shared/manifest"
)

// quiescedResult is what the host learns from a quiesced take: whether the service actually held
// still, and — when it did not — why, so the journal says which of the several honest failures
// this was.
type quiescedResult struct {
	Held bool   `json:"held"`
	Why  string `json:"why,omitempty"`
	// How long getting the lock and holding it took ([B.167]): what an hourly sample costs Home
	// Assistant, and the first thing to look at if its quiesce starts degrading after an update.
	// Zero when there was no lock to take.
	Acquire time.Duration `json:"acquire,omitempty"`
	Hold    time.Duration `json:"hold,omitempty"`
}

// quiescedMember takes one member of a RUNNING service, asking it to hold still across the
// snapshot ([B.143]). It is the nightly's verb and so far nothing else's.
//
// THE ORDER IS HOLD, SNAPSHOT, RELEASE, LABEL — and the label is last because only the release
// knows what to write. Home Assistant reports whether its lock survived the window (it breaks its
// own lock if the events it is buffering pile up), so the sidecar is written from what the release
// answered rather than from what the hold hoped for.
//
// ⚠️ IT NEVER REFUSES TO TAKE A MEMBER. Every way this can fail — a service with no way to hold
// still, a Home Assistant that is down or too old to have the view, a lock that timed out, a
// release that says the lock broke — ends with the member taken and the class saying
// crash-consistent. A nightly that did not happen is a hole in the ring; a nightly that happened
// and said what it is worth is the product working.
//
// The SERVICE is named rather than the port, because the manifest on the volume is the one place
// that knows how to reach it, and it is the same manifest the member is pinned to.
func quiescedMember(ctx context.Context, x Executor, run func(string, ...string) error, req snapshotRequest) (quiescedResult, error) {
	if err := safeUnitName(req.Service); err != nil { // the name becomes a path element
		return quiescedResult{}, err
	}
	var meta quadlet.SnapshotMeta
	if err := json.Unmarshal([]byte(req.Sidecar), &meta); err != nil {
		return quiescedResult{}, fmt.Errorf("the member's sidecar does not parse: %w", err)
	}
	// ⚠️ THE HOST SENDS `crash` AND THE GUEST UPGRADES IT, never the other way round. A bug
	// anywhere below leaves the member saying the weaker, true thing; the claim to have been held
	// still is made only by the code that watched it happen.
	meta.Consistency = quadlet.Crash

	start := time.Now()
	release, why := quiesceService(ctx, x, req.Service)
	var acquire, hold time.Duration
	if release != nil {
		acquire = time.Since(start)
	}
	locked := time.Now()
	if release != nil {
		// DEFERRED AS WELL AS CALLED BELOW, because a snapshot that fails must still let the
		// household's recorder write again: Home Assistant would take about ten seconds to notice
		// and break its own lock, and those are ten seconds of events buffered for nothing.
		defer func() {
			if release != nil {
				if _, err := release(ctx); err != nil {
					log.Printf("ring %s: the quiesce window was not released cleanly (%v)", req.Service, err)
				}
			}
		}()
	}
	if err := run("btrfs", "subvolume", "snapshot", "-r", req.DataDir, req.Path); err != nil {
		return quiescedResult{Why: why}, err
	}
	if release != nil {
		held, err := release(ctx)
		release = nil // released here; the defer above has nothing left to do
		hold = time.Since(locked)
		log.Printf("ring %s: the quiesce took %s to acquire and held %s (held=%t)", req.Service, acquire, hold, held && err == nil)
		switch {
		case err != nil:
			why = "the service could not be released cleanly: " + err.Error()
		case !held:
			// Home Assistant resumed writing under us. The member is real and is exactly as good
			// as an ordinary live snapshot, which is what it now says.
			why = "the service resumed writing before the snapshot finished"
		default:
			meta.Consistency = quadlet.Quiesced
		}
	}
	sidecar, err := json.Marshal(meta)
	if err != nil {
		return quiescedResult{Why: why}, err
	}
	// The member exists and is unlabelled for exactly this long. takeSnapshot's rule applies the
	// same way: a member that cannot be labelled is removed rather than left for a human to
	// identify by hand.
	if err := x.WriteFile(quadlet.SnapshotSidecar(req.Path), sidecar); err != nil {
		if derr := run("btrfs", "subvolume", "delete", req.Path); derr != nil {
			return quiescedResult{Why: why}, fmt.Errorf("write the member's sidecar: %w; AND the unlabelled member could not be removed: %v", err, derr)
		}
		return quiescedResult{Why: why}, fmt.Errorf("write the member's sidecar (the member was removed): %w", err)
	}
	recordMember(ctx, x, req.Path, meta)
	return quiescedResult{Held: meta.Consistency == quadlet.Quiesced, Why: why, Acquire: acquire, Hold: hold}, nil
}

// TakeNightlyMember is the quiesced take FOR A GUEST WITH NO HOST ([B.143]) — the same
// accommodation `--inbound-listen` and `--write-units` make, and for the same reason.
//
// In the product this verb arrives from the host, which owns the cadence and renders the sidecar
// (agent/host/nightly.go). The rigs that get Home Assistant running are agent-less, so without
// this the one thing that can silently drift under us — Home Assistant's own recorder lock, an
// internal API — would have no coverage on a real HA at all, and an L0 run would prove nothing
// about it.
//
// It builds exactly what the host would send, including the `crash` the guest then upgrades, so
// the harness supplies the trigger and nothing else. Nothing in the product invokes it.
func TakeNightlyMember(ctx context.Context, x Executor, service string, at time.Time) (string, error) {
	ensureToolsOnPath()
	raw, err := x.ReadFile(manifestPath(service))
	if err != nil {
		return "", fmt.Errorf("read the running manifest: %w", err)
	}
	sidecar, err := json.Marshal(quadlet.SnapshotMeta{
		Service: service, Trigger: quadlet.TriggerDaily, TakenAt: at,
		Consistency: quadlet.Crash, Manifest: string(raw),
	})
	if err != nil {
		return "", err
	}
	member := quadlet.SnapshotMember(service, quadlet.TriggerDaily, at)
	run := func(name string, args ...string) error { _, err := x.Run(ctx, name, args...); return err }
	res, err := quiescedMember(ctx, x, run, snapshotRequest{
		Service: service, DataDir: quadlet.DataRoot(service), Path: member, Sidecar: string(sidecar),
	})
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("took %s (held=%t acquire_ms=%d hold_ms=%d %s)", path.Base(member), res.Held,
		res.Acquire.Milliseconds(), res.Hold.Milliseconds(), res.Why), nil
}

// quiesceService asks the registry for this service's way of holding still, reading the manifest
// on the volume for how to reach it. A nil release with a reason is the ordinary answer for a
// service that has none, and for one that could not be asked.
func quiesceService(ctx context.Context, x Executor, service string) (func(context.Context) (bool, error), string) {
	raw, err := x.ReadFile(manifestPath(service))
	if err != nil {
		return nil, fmt.Sprintf("the running manifest could not be read: %v", err)
	}
	m, _, err := manifest.Parse(raw)
	if err != nil {
		return nil, fmt.Sprintf("the running manifest does not parse: %v", err)
	}
	var port int
	for _, c := range m.Containers {
		if c.Primary {
			port = c.Port
		}
	}
	release, err := services.Quiesce(ctx, x, m, port)
	if err != nil {
		return nil, err.Error()
	}
	return release, ""
}
