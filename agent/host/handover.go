package host

import (
	"context"
	"fmt"

	"briard.io/shared/api"
)

// A PLANNED HANDOVER -- give this node's work to a peer while it is perfectly
// healthy, which is what lets it reboot into a new generation without taking the house down.
//
// It is the deliberate counterpart to everything else in the failover story. A real failover is
// something that happens TO a node; this is something a node does, on a schedule, in a window,
// with a peer standing by. That difference is why it is a directive and not a reaction: nothing
// about the node's state calls for it, so somebody has to ask.
//
// WHAT THIS FILE DOES NOT DO -- and the omission is the design, not a gap. It does not wait for
// the peer, verify who took over, or sequence a roll. `drbd-reactorctl evict` says "not me", not
// "you": the destination is drbd-reactor's own election. So the only honest thing a node can
// report is that the eviction RAN, and the only place that can say where the work LANDED is the
// side that can see every node -- the cloud (the sequencer, [[logic-on-host-by-default]]).
// A node that claimed more than it can see would be guessing on the house's behalf.

// guestEvictor is the slice of the guest client a handover drives. Narrow on purpose: this path
// must not be able to reach anything that changes what the node runs.
type guestEvictor interface {
	ReactorEvict(ctx context.Context, keepMasked, unmask bool) error
}

// Handover payload words. Deliberately three named states rather than a bool pair: "" is the
// ordinary handover, and the two others are the reboot path's halves, which are meaningless
// without each other and easy to confuse if they arrive as flags.
const (
	handoverPlain      = ""            // evict; the node is immediately eligible again (a hand-back can follow)
	handoverKeepMasked = "keep-masked" // evict and STAY out -- an unverified generation must not reclaim the house
	handoverUnmask     = "unmask"      // release a keep-masked node; evicts nothing
)

// ApplyHandover handles a DirectiveHandover. Idempotent in the way that matters: evicting a node
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
	if err := g.ReactorEvict(ctx, keepMasked, unmask); err != nil {
		logf("directive handover failed: %v", err)
		return failed(err.Error())
	}
	logf("directive handover applied")
	return api.DirectiveOutcome{ID: d.ID, State: api.OutcomeDone}
}
