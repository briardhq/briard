package host

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"briard.io/agent/quadlet"
	"briard.io/shared/model"
)

// The NIGHTLY MEMBER ([B.143]): one ring member per service per night, taken by the CLOCK rather
// than by an event.
//
// WHY THE RING NEEDS ONE AT ALL. Every other member is taken at a service start or an act, and a
// stable Home Assistant can run for a month without restarting. The clock's sample is what still
// compares it with yesterday -- a detected change, or the day event that marks a quiet day
// ([B.167]) -- so the history has a restore point a day even when nothing restarts.
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
const nightlyHour = 3 // local time; the quiet end of a household's night

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

// nightly remembers which services already have tonight's member. It lives for the observe loop,
// like the reparenter and the VIP router: "has this already happened today" is not a question one
// cycle can answer.
type nightly struct {
	taken map[string]string // service -> the date its nightly was taken, as 2006-01-02
}

func newNightly() *nightly { return &nightly{taken: map[string]string{}} }

// consider takes tonight's member for every service that does not have one yet.
//
// SERVING ONLY. The volume is mounted on one node, and a secondary asking for a member would be
// asking about data it does not hold.
//
// THE IN-MEMORY RECORD IS THE FAST PATH, NOT THE TRUTH. The agent restarts — an update, a crash,
// a re-dial — and a fresh map inside the window would take a second member every cycle for an
// hour. So a service this process has no record of is checked against the RING, which is the
// durable answer, and the map is what keeps that check to once per service per night.
func (cfg Config) consider(ctx context.Context, g memberTaker, n *nightly, services []model.ServiceSpec, serving bool, now time.Time, logf func(string, ...any)) {
	if !serving || now.Hour() != nightlyHour || !g.SupportsSnapshotMember() {
		return
	}
	today := now.Format("2006-01-02")
	for _, s := range services {
		if n.taken[s.Name] == today {
			continue
		}
		if cfg.hasNightly(ctx, g, s.Name, today) {
			n.taken[s.Name] = today // somebody's earlier run took it; do not take a second
			continue
		}
		at := cfg.takenAt()
		member := quadlet.SnapshotMember(s.Name, quadlet.TriggerDaily, at)
		if err := cfg.takeRunning(ctx, g, s.Name, member, quadlet.TriggerDaily, s.Name+" nightly", at, logf); err != nil {
			// Never fatal, and never retried inside the window: a household's night is not the
			// place to hammer a volume that is having trouble, and tomorrow's member costs the
			// same as today's. The ring is a convenience; the service is the product.
			logf("nightly %s: could not take tonight's member (%v)", s.Name, err)
			n.taken[s.Name] = today
			continue
		}
		n.taken[s.Name] = today
		logf("nightly %s: took %s", s.Name, member)
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
func (cfg Config) takeRunning(ctx context.Context, g memberTaker, service, member string, tr quadlet.Trigger, title string, at time.Time, logf func(string, ...any)) error {
	if !g.SupportsQuiescedSnapshot() {
		return cfg.takeMember(ctx, g, service, member, tr, quadlet.Crash, title, nil, at, logf)
	}
	raw, err := g.ServiceInstalled(ctx, service)
	if err != nil {
		return fmt.Errorf("read the running manifest: %w", err)
	}
	sidecar, err := json.Marshal(quadlet.SnapshotMeta{
		Service: service, Trigger: tr, Title: title, TakenAt: at,
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
		// nightly is crash-consistent again tonight" is how a household would find out that Home
		// Assistant stopped answering, or that an upgrade moved the API this leans on.
		logf("%s: taken without holding the service still (%s)", title, why)
	}
	return nil
}

// hasNightly reports whether the ring already holds a daily member from this date — the check
// that makes an agent restart inside the window cost nothing.
//
// A guest that cannot list says NO, and the take that follows is refused by its own capability
// gate if it cannot happen either. Both directions are safe: the worst case is a second member of
// bytes that have not changed, which copy-on-write makes nearly free and the next take replaces.
func (cfg Config) hasNightly(ctx context.Context, g memberTaker, service, date string) bool {
	if !g.SupportsMembers() {
		return false
	}
	members, err := g.Members(ctx, service)
	if err != nil {
		return false
	}
	for _, m := range members {
		if tr := m.Meta.Trigger; tr != quadlet.TriggerDaily {
			continue
		}
		if m.Meta.TakenAt.Local().Format("2006-01-02") == date {
			return true
		}
	}
	return false
}
