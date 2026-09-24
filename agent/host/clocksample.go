package host

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"briard.io/agent/quadlet"
	"briard.io/shared/model"
)

// THE CLOCK SAMPLE ([B.143], [B.167]): a ring member taken by the CLOCK rather than by an event,
// whenever a service's newest member is an hour old.
//
// WHY THE RING NEEDS ONE AT ALL. Every other member is taken at a service start or an act, and a
// stable Home Assistant can run for a month without restarting. The clock's sample is what still
// compares it with an hour ago -- a detected change lands within the hour it happened, and a quiet
// day still gets its day event -- so undo's granularity is the interval, not the uptime.
//
// HOURLY, AND MEASURED CHEAP: the recorder lock costs 76 ms p95 at 100 writes/s and never broke
// (hass-quiesce-cost, B.167a in V3c.md), and Home Assistant runs its automations with the recorder
// down entirely -- only history waits. "Unless a recent one exists": any member counts, so a
// service that restarts often is sampled by its starts and the clock adds nothing.
//
// SCHEDULED BY THE HOST, TAKEN BY THE GUEST (DESIGN §9.8). The host owns cadence and policy; the
// guest owns the volume and does the work. It rides the observe loop rather than a timer of its
// own, because that loop already runs on a cadence, already knows whether this node is serving,
// and already holds the channel — a second scheduler would need all three again.
//
// ⚠️ IT IS THE ONE MEMBER TAKEN AGAINST A RUNNING SERVICE, so it is the one that has to ASK the
// service to hold still — and the one whose class is decided by whether it did. Home Assistant
// offers exactly the mechanism its own backups use (a truncating WAL checkpoint plus a held
// transaction, agent/hass/quiesce.go); a service that offers nothing gets a member that says
// crash-consistent, which is what it is. Shipping a member whose trustworthiness nobody can tell
// is the thing [B.143] refuses, and the class is how that stays true when the asking fails.
const clockInterval = time.Hour

// memberTaker is the slice of the guest a member costs: read the manifest it is pinned to, ask for
// the member, and — only when this process has forgotten — read back what the ring already holds.
type memberTaker interface {
	// QuiescedSnapshot takes a member of a RUNNING service, asking it to hold still across the
	// snapshot, and answers whether it did ([B.143]). The guest writes that sidecar itself.
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

// takeRunning takes a member of a RUNNING service -- tonight's, or the sample after an update
// ([B.143]) -- asking the service to hold still if the guest can ask.
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
