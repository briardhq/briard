package host

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"briard.io/agent/hass"
	"briard.io/agent/quadlet"
	"briard.io/shared/model"
	"briard.io/shared/notify"
)

// THE CLOCK SAMPLE: a ring member taken by the CLOCK rather than by an event,
// whenever a service's newest member is an hour old.
//
// WHY THE RING NEEDS ONE AT ALL. Every other member is taken at a service start or an operation,
// and a stable Home Assistant can run for a month without restarting. The clock's sample is what
// still compares it with an hour ago -- a detected change lands within the hour it happened, and
// quiet time still gets its restore points -- so undo's granularity is the interval, not the uptime.
//
// HOURLY, AND MEASURED CHEAP: the recorder lock costs 76 ms p95 at 100 writes/s and never broke
// in the quiesce-cost measurement, and Home Assistant runs its automations with the recorder
// down entirely -- only history waits. "Unless a recent one exists": any member counts, so a
// service that restarts often is sampled by its starts and the clock adds nothing.
//
// SCHEDULED BY THE HOST, TAKEN BY THE GUEST. The host owns cadence and policy; the
// guest owns the volume and does the work. It rides the observe loop rather than a timer of its
// own, because that loop already runs on a cadence, already knows whether this node is serving,
// and already holds the channel — a second scheduler would need all three again.
//
// ⚠️ IT IS THE ONE MEMBER TAKEN AGAINST A RUNNING SERVICE, so it is the one that has to ASK the
// service to hold still — and the one whose class is decided by whether it did. Home Assistant
// offers exactly the mechanism its own backups use (a truncating WAL checkpoint plus a held
// transaction, agent/hass/quiesce.go); a service that offers nothing gets a member that says
// crash-consistent, which is what it is. Shipping a member whose trustworthiness nobody can tell
// is the thing the ring refuses, and the class is how that stays true when the asking fails.
const clockInterval = time.Hour

// memberTaker is the slice of the guest a member costs: read the manifest it is pinned to, ask for
// the member, and — only when this process has forgotten — read back what the ring already holds.
type memberTaker interface {
	// QuiescedSnapshot takes a member of a RUNNING service, asking it to hold still across the
	// snapshot, and answers whether it did. The guest writes that sidecar itself.
	QuiescedSnapshot(ctx context.Context, service, dataDir, dest, sidecar string) (bool, string, error)
	SupportsQuiescedSnapshot() bool
	ServiceInstalled(ctx context.Context, name string) (string, error)
	Snapshot(ctx context.Context, dataDir, dest, sidecar string) error
	SupportsSnapshotMember() bool
	Members(ctx context.Context, service string) ([]quadlet.SnapshotEntry, error)
	SupportsMembers() bool
}

// clockSampler remembers each service's newest member. It lives for the observe loop, like the
// reparenter and the VIP router: "is the newest member an hour old" is not a question one cycle
// can answer cheaply.
type clockSampler struct {
	newest map[string]time.Time // service -> the newest member this process knows of
}

func newClockSampler() *clockSampler { return &clockSampler{newest: map[string]time.Time{}} }

// consider takes a clock sample for every service whose newest member is an interval old.
//
// SERVING ONLY. The volume is mounted on one node, and a secondary asking for a member would be
// asking about data it does not hold.
//
// THE IN-MEMORY RECORD IS THE FAST PATH, NOT THE TRUTH. The guest takes members this process never
// hears of (every start), and the agent restarts with an empty map. So a service whose record is
// an interval old is checked against the RING, which is the durable answer, and the map is what
// keeps that check to about once per service per interval.
func (cfg Config) consider(ctx context.Context, g memberTaker, c *clockSampler, services []model.ServiceSpec, serving bool, now time.Time, logf func(string, ...any)) {
	if !serving || !g.SupportsSnapshotMember() {
		return
	}
	for _, s := range services {
		if now.Sub(c.newest[s.Name]) < clockInterval {
			continue
		}
		if at, ok := cfg.newestMember(ctx, g, s.Name); ok && now.Sub(at) < clockInterval {
			c.newest[s.Name] = at
			continue
		}
		at := cfg.takenAt()
		member := quadlet.SnapshotMember(s.Name, quadlet.TriggerClock, at)
		// RECORDED WHETHER OR NOT IT WORKS: a failure waits an interval rather than being retried
		// every cycle. A volume that is having trouble is not helped by being asked again, and the
		// next sample costs the same as this one. The ring is a convenience; the service is the
		// product.
		c.newest[s.Name] = at
		if err := cfg.takeRunning(ctx, g, s.Name, member, quadlet.TriggerClock, at, logf); err != nil {
			logf("clock %s: could not take a sample (%v)", s.Name, err)
			continue
		}
		logf("clock %s: took %s", s.Name, member)
	}
}

// takeRunning takes the clock's member of a RUNNING service, asking the service to hold
// still if the guest can ask.
//
// ⚠️ THE SIDECAR IS RENDERED SAYING `crash` EITHER WAY, and the guest upgrades it when the service
// actually held. The host cannot see whether a lock survived — Home Assistant reports that to
// whoever released it — so the claim is made by the only party that watched it, and every failure
// in between leaves the member saying the weaker, true thing.
//
// A guest too old to ask gets the plain take, which is what this did before the quiesce existed:
// a member that says crash-consistent, which is exactly what it is.
func (cfg Config) takeRunning(ctx context.Context, g memberTaker, service, member string, tr quadlet.Trigger, at time.Time, logf func(string, ...any)) error {
	if !g.SupportsQuiescedSnapshot() {
		return cfg.takeMember(ctx, g, service, member, tr, quadlet.Crash, nil, at, logf)
	}
	raw, err := g.ServiceInstalled(ctx, service)
	if err != nil {
		return fmt.Errorf("read the running manifest: %w", err)
	}
	sidecar, err := json.Marshal(quadlet.SnapshotMeta{
		Service: service, Trigger: tr, TakenAt: at,
		Consistency: quadlet.Crash, Manifest: raw,
	})
	if err != nil {
		return err
	}
	held, why, err := g.QuiescedSnapshot(ctx, service, quadlet.DataRoot(service), member, string(sidecar))
	if err != nil {
		return err
	}
	if !held {
		// NOT A FAILURE, and the log line says so: the member exists and is as good as any live
		// snapshot, which is what its class now says. The reason is worth a line because "the
		// clock sample is crash-consistent again" is how a household would find out that Home
		// Assistant stopped answering, or that an upgrade moved the API this leans on.
		logf("%s %s: taken without holding the service still (%s)", service, tr, why)
	}
	return nil
}

// newestMember is when the ring's newest member of this service was taken, by the time in its
// name — the check that makes an agent restart, or a start sample the guest took, count.
//
// A guest that cannot list says NO, and the take that follows is refused by its own capability
// gate if it cannot happen either. Both directions are safe: the worst case is a second member of
// bytes that have not changed, which copy-on-write makes nearly free and the next take replaces.
func (cfg Config) newestMember(ctx context.Context, g memberTaker, service string) (time.Time, bool) {
	if !g.SupportsMembers() {
		return time.Time{}, false
	}
	members, err := g.Members(ctx, service)
	if err != nil {
		return time.Time{}, false
	}
	var newest time.Time
	for _, m := range members {
		if at, ok := quadlet.SnapshotMemberTime(m.Member); ok && at.After(newest) {
			newest = at
		}
	}
	return newest, !newest.IsZero()
}

// THE NIGHTLY RECORDER CHECK, 05:30 local: after the guest-update window (03:00 plus up to two
// hours) and Home Assistant's own 04:12 purge, so it reads the database after both have
// finished with it. The guest does the work (agent/hass/dbcheck.go says what and why); this
// schedules it, asks for the restore a corrupt report names, and tells the household.
//
// THE CHECK NEVER BLOCKS THIS LOOP. It reads read-only members and can take minutes, so the
// guest runs it in the background: this starts it, then asks for the report once a cycle with
// an ordinary short call until it arrives. The RESTORE runs in line, leased for its budget,
// because it stops the app and must not overlap any other operation on it.
//
// ONCE A NIGHT WHATEVER HAPPENS: a failure is logged and waits a day. An agent that is down at
// 05:30 still starts it within the window; one down for the whole window skips that night.
const (
	dbCheckAt     = 5*60 + 30 // minutes after local midnight
	dbCheckWindow = 60        // minutes after dbCheckAt a late agent may still start it
	// dbCheckWait is how long a started check's report is waited for: the guest bounds the check
	// at 30 minutes, and a report that has not come by then never will.
	dbCheckWait     = 45 * time.Minute
	dbRestoreBudget = 10 * time.Minute
	dbCallTimeout   = 5 * time.Second
)

// recorderChecker is the slice of the guest the check costs.
type recorderChecker interface {
	HassDBCheck(ctx context.Context) (bool, error)
	HassDBCheckResult(ctx context.Context) (hass.DBCheckState, error)
	HassDBRestore(ctx context.Context, member string) error
}

// dbChecker remembers the local date it last started a check, in the household's zone, and
// since when it has been waiting for one's report.
type dbChecker struct {
	loc     *time.Location
	done    string
	waiting time.Time // zero when no report is awaited
}

func newDBChecker() *dbChecker {
	loc := time.Local
	if tz := localTimezone("/"); tz != "" {
		if l, err := time.LoadLocation(tz); err == nil {
			loc = l
		}
	}
	return &dbChecker{loc: loc}
}

// checkRecorder collects a check that is running, or starts tonight's if it is due: this node
// serves, it runs Home Assistant, and the local time is inside tonight's window.
func (cfg Config) checkRecorder(ctx context.Context, g recorderChecker, d *dbChecker, services []model.ServiceSpec, serving bool, now time.Time, n notify.Notifier, logf func(string, ...any)) {
	if !serving || !slices.ContainsFunc(services, func(s model.ServiceSpec) bool { return s.Name == hass.Name }) {
		return
	}
	if !d.waiting.IsZero() {
		cfg.collectRecorder(ctx, g, d, now, n, logf)
		return
	}
	local := now.In(d.loc)
	day := local.Format(time.DateOnly)
	mins := local.Hour()*60 + local.Minute()
	if d.done == day || mins < dbCheckAt || mins >= dbCheckAt+dbCheckWindow {
		return
	}
	d.done = day
	sctx, cancel := context.WithTimeout(ctx, dbCallTimeout)
	started, err := g.HassDBCheck(sctx)
	cancel()
	if err != nil {
		logf("hass-db: could not start the nightly check: %v", err)
		return
	}
	if !started {
		// The guest is already busy with one, e.g. started before this agent restarted: its report
		// is as good as tonight's.
		logf("hass-db: a check is already running in the guest; collecting that one")
	}
	d.waiting = now
}

// collectRecorder asks for a started check's report, and acts on it once it has come.
func (cfg Config) collectRecorder(ctx context.Context, g recorderChecker, d *dbChecker, now time.Time, n notify.Notifier, logf func(string, ...any)) {
	sctx, cancel := context.WithTimeout(ctx, dbCallTimeout)
	s, err := g.HassDBCheckResult(sctx)
	cancel()
	late := now.Sub(d.waiting) > dbCheckWait
	switch {
	case err != nil && !late:
		return // asked again next cycle
	case err != nil:
		logf("hass-db: gave up on tonight's check: %v", err)
		d.waiting = time.Time{}
		return
	case s.Report == nil && s.Running && !late:
		return
	case s.Report == nil:
		// Running past its own bound, or gone without a report: the guest agent restarted.
		logf("hass-db: tonight's check left no report (running=%t)", s.Running)
		d.waiting = time.Time{}
		return
	}
	d.waiting = time.Time{}
	rep := *s.Report
	logf("hass-db: verdict=%q checked=%s candidate=%s %s", rep.Verdict, rep.Checked, rep.Candidate, rep.Why)
	if rep.Verdict != hass.VerdictCorrupt {
		return
	}
	if rep.Candidate == "" {
		fireAlert(ctx, n, logf, recorderAlert(cfg.Node, rep.Why, time.Time{}, d.loc))
		return
	}
	bctx, cancel := cfg.beat.budget(ctx, dbRestoreBudget)
	defer cancel()
	if err := g.HassDBRestore(bctx, rep.Candidate); err != nil {
		logf("hass-db: the restore from %s failed: %v", rep.Candidate, err)
		fireAlert(ctx, n, logf, recorderAlert(cfg.Node, err.Error(), time.Time{}, d.loc))
		return
	}
	fireAlert(ctx, n, logf, recorderAlert(cfg.Node, "", rep.CandidateAt, d.loc))
}

// recorderAlert is what the household is told when the check found damage: repaired from a copy
// taken at `from`, or not, and why. A History row alone is not enough: nothing else would make
// anyone look.
func recorderAlert(node, why string, from time.Time, loc *time.Location) notify.Alert {
	if from.IsZero() {
		return notify.Alert{
			Level: notify.Warning,
			Title: "Briard: Home Assistant's history is damaged",
			Body: fmt.Sprintf("Home Assistant's history database on node %s is damaged and briard could not repair it (%s). "+
				"Home Assistant will start an empty history when it next reads the damaged part.", node, why),
		}
	}
	return notify.Alert{
		Level: notify.Warning,
		Title: "Briard: Home Assistant's history was repaired",
		Body: fmt.Sprintf("Home Assistant's history database on node %s was damaged. Briard put back the last good copy, from %s; "+
			"history recorded after that is lost. Undoing \"Restored corrupted database\" in the app's History puts the damaged copy back.",
			node, from.In(loc).Format("Mon 2 Jan, 15:04")),
	}
}
