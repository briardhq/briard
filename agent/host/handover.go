package host

import (
	"context"
	"fmt"
	"time"

	"briard.io/shared/api"
)

// A planned handover: give this node's work to a peer while it is perfectly healthy, which is
// what lets it reboot onto a new release without taking the house down. It is the deliberate
// counterpart to everything else in the failover story: a real failover is something that happens
// to a node; this is something a node does, on a schedule, in a window, with a peer standing by.
// That difference is why it is a directive and not a reaction: nothing about the node's state
// calls for it, so somebody has to ask.
//
// What this file does not do is the design, not a gap: it does not wait for the peer, verify who
// took over, or sequence a roll. `drbd-reactorctl evict` says "not me", not "you": the
// destination is drbd-reactor's own election. So the only honest thing a node can report is that
// the eviction ran, and the only place that can say where the work landed is the side that can
// see every node, the cloud (the sequencer); a node that claimed more than it can see would be
// guessing on the house's behalf.

// guestEvictor is the slice of the guest client a handover drives. Narrow on purpose: this path
// must not be able to reach anything that changes what the node runs. FsSync clears that bar —
// it flushes dirty pages that were going to be written anyway, changing only their timing.
type guestEvictor interface {
	ReactorEvict(ctx context.Context, keepMasked, unmask bool) error
	FsSync(ctx context.Context) (string, error)
}

// fsSyncTimeout bounds the pre-eviction flush. Generous — a write-heavy app can owe the
// volume real data — but finite, and the timeout path continues into the eviction rather than
// aborting it: a device on which sync hangs is a device to evict away from, and the unmount
// will retry the same writeback with the same result either way.
const fsSyncTimeout = 60 * time.Second

// evictBudget bounds the eviction itself, generously for the reason the number looks arbitrary:
// the evict stops the apps and unmounts, and an unmount syncs. The pre-eviction flush above
// usually means that writeback owes little (the sequencer sends a `sync` directive ahead of the
// handover, and this function's own FsSync catches the settle window), but "usually" is not a
// bound, and a write-heavy app on a slow device is exactly when a handover matters most.
//
// It must exist at all: without it this call runs on the caller's unbounded context, on the
// observe loop, the one shape beat.budget cannot paper over. Lease refuses a context with no
// deadline, correctly, because an unbounded guest RPC on that loop is the wedge the watchdog
// exists to catch rather than something to excuse.
const evictBudget = 15 * time.Minute

// Handover payload words. Deliberately three named states rather than a bool pair: "" is the
// ordinary handover, and the two others are the reboot path's halves, which are meaningless
// without each other and easy to confuse if they arrive as flags.
const (
	handoverPlain      = ""            // evict; the node is immediately eligible again (a hand-back can follow)
	handoverKeepMasked = "keep-masked" // evict and stay out -- an unverified release must not reclaim the house
	handoverUnmask     = "unmask"      // release a keep-masked node; evicts nothing
)

// applyHandover handles a DirectiveHandover. Idempotent in the way that matters: evicting a node
// that does not hold the resource is a no-op, so a re-delivered directive re-reports done rather
// than shuffling the house a second time.
func (cfg Config) applyHandover(ctx context.Context, g guestEvictor, d api.Directive, logf func(string, ...any)) api.DirectiveOutcome {
	failed := func(detail string) api.DirectiveOutcome {
		return api.DirectiveOutcome{ID: d.ID, State: api.OutcomeFailed, Detail: detail}
	}
	var keepMasked, unmask bool
	switch d.Payload {
	case handoverPlain:
	case handoverKeepMasked:
		keepMasked = true
	case handoverUnmask:
		unmask = true
	default:
		// An unknown word is refused rather than treated as the plain case: the three differ in
		// whether the node may take the house back, and guessing that is not a small mistake.
		return failed(fmt.Sprintf("unknown handover mode %q", d.Payload))
	}
	switch {
	case unmask:
		logf("directive kind=handover: releasing this node to hold the resource again")
	case keepMasked:
		logf("directive kind=handover: handing the work to a peer and staying out")
	default:
		logf("directive kind=handover: handing the work to a peer")
	}
	if !unmask {
		// Flush the data volume first: the eviction's unmount writes back every
		// dirty page inside the demote path — under the peer's promotion and DRBD's ping
		// deadline — and that flush is unbounded (proportional to dirty data, not to time).
		// Syncing here moves the bulk across while nothing is racing; the unmount then owes
		// only the seconds since. Best-effort by design: on error or timeout the eviction
		// proceeds — a sick device argues for moving the house, not for keeping it — and an
		// older guest without the verb answers unknown-verb, which lands here too.
		sctx, cancel := cfg.beat.budget(ctx, fsSyncTimeout)
		detail, err := g.FsSync(sctx)
		cancel()
		if err != nil {
			logf("directive kind=handover: pre-eviction sync failed (evicting anyway): %v", err)
		} else {
			logf("directive kind=handover: pre-eviction sync: %s", detail)
		}
	}
	ectx, ecancel := cfg.beat.budget(ctx, evictBudget)
	defer ecancel()
	if err := g.ReactorEvict(ectx, keepMasked, unmask); err != nil {
		logf("directive handover failed: %v", err)
		return failed(err.Error())
	}
	logf("directive handover applied")
	return api.DirectiveOutcome{ID: d.ID, State: api.OutcomeDone}
}

// applySync handles a DirectiveSync: flush the replicated volume now, on its own, so a handover
// sent moments later unmounts a volume that owes almost nothing (the sequencer's
// sync → settle → evict ordering; applyHandover's own sync then moves only the settle window's
// accumulation). Unlike the in-handover sync this one reports failure: the caller asked for
// exactly this flush and deserves the truth about it, and nothing downstream is blocked on the
// answer.
func (cfg Config) applySync(ctx context.Context, g guestEvictor, d api.Directive, logf func(string, ...any)) api.DirectiveOutcome {
	sctx, cancel := cfg.beat.budget(ctx, fsSyncTimeout)
	detail, err := g.FsSync(sctx)
	cancel()
	if err != nil {
		logf("directive kind=sync failed: %v", err)
		return api.DirectiveOutcome{ID: d.ID, State: api.OutcomeFailed, Detail: err.Error()}
	}
	logf("directive kind=sync: %s", detail)
	return api.DirectiveOutcome{ID: d.ID, State: api.OutcomeDone, Detail: detail}
}
