package host

import (
	"context"
	"time"

	"briard.io/agent/quadlet"
	"briard.io/shared/model"
)

// The NIGHTLY MEMBER ([B.143]): one ring member per service per night, taken by the CLOCK rather
// than by an event.
//
// WHY THE RING NEEDS ONE AT ALL. Every other member is taken at a service start, and a stable Home
// Assistant can run for a month without restarting — which would leave DESIGN §5's mistake rung a
// month-wide hole and the retention ladder nothing to thin. It is also what makes the ladder need
// no floor: a service nobody touches still has last night's member, so its ring is never empty.
//
// SCHEDULED BY THE HOST, TAKEN BY THE GUEST (DESIGN §9.8). The host owns cadence and policy; the
// guest owns the volume and does the work. It rides the observe loop rather than a timer of its
// own, because that loop already runs on a cadence, already knows whether this node is serving,
// and already holds the channel — a second scheduler would need all three again.
//
// ⚠️ IT IS THE ONE MEMBER TAKEN AGAINST A RUNNING SERVICE, so it is quadlet.Crash: the bytes are
// whatever a power cut would have left. The per-service quiesce that would promote it (for Home
// Assistant, a truncating WAL checkpoint plus a transaction held across the take) is not built.
// The class is what lets this ship honestly in the meantime — shipping a member whose
// trustworthiness nobody can tell is the thing [B.143] refuses.
const nightlyHour = 3 // local time; the quiet end of a household's night

// memberTaker is the slice of the guest a member costs: read the manifest it is pinned to, ask for
// the member, and — only when this process has forgotten — read back what the ring already holds.
type memberTaker interface {
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
		if err := cfg.takeMember(ctx, g, s.Name, member, quadlet.TriggerDaily, quadlet.Crash,
			s.Name+" nightly", at, logf); err != nil {
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

// hasNightly reports whether the ring already holds a daily member from this date — the check
// that makes an agent restart inside the window cost nothing.
//
// A guest that cannot list says NO, and the take that follows is refused by its own capability
// gate if it cannot happen either. Both directions are safe: the worst case is a second member of
// bytes that have not changed, which copy-on-write makes nearly free and the ladder prunes.
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
