package host

import (
	"context"
	"fmt"
	"time"

	"briard.io/shared/model"
	"briard.io/shared/notify"
)

// redundancy is what this node's replica set currently is, and it is three states rather than
// two because "a peer is gone" and "the only other copy of the household's data is gone" are
// different facts that the peer COUNT cannot tell apart ([B.102], inherited from [B.100]). On
// the shipped anchor+anchor+witness flock both losses read as connected 1-of-2; only one of
// them means the house is down to a single disk.
type redundancy int

const (
	redundancyFull    redundancy = iota // every expected peer connected
	redundancyReduced                   // a peer is gone; a usable copy of the data remains, or we cannot tell
	redundancyAlone                     // no connected peer carries a usable copy: one disk left
)

// redundancyAlerter fires an edge-triggered alert when a data node loses replica
// redundancy -- still quorate and serving, but a peer connection dropped, so "one more
// failure would cause an outage" -- and again (recovered) when all replicas
// reconnect. It is the agent-side signal derived from the DRBD/quorum state the observe
// loop already reads. Edge-triggered + primed on the first reading so a steady degrade
// doesn't spam and startup (post-convergence) doesn't false-positive. Not built on a
// witness (its view is redundant with the data nodes') nor a single-node cluster (no
// redundancy to lose).
type redundancyAlerter struct {
	n     notify.Notifier
	node  string
	peers int // expected connected peers (mesh size - 1)
	logf  func(string, ...any)
	last  redundancy
	armed bool // false until the first definite reading has primed `last`
}

func newRedundancyAlerter(n notify.Notifier, node string, peers int, logf func(string, ...any)) *redundancyAlerter {
	return &redundancyAlerter{n: n, node: node, peers: peers, logf: logf}
}

// classify reads the replica set the way the household experiences it. The peer COUNT decides
// whether anything is missing; the peer LIST decides whether what is missing was the second
// copy -- model.Cluster carries both from one sample, which is why this takes a Cluster rather
// than the QuorumState summary that rides the cloud wire.
//
// AN UNKNOWN IS NOT AN ALARM. A guest too old to report peers sends none, and a resource with
// nothing to say about its peers must fall back to the weaker claim rather than announce a lost
// second copy it cannot see -- the same rule the reconnect check follows for boot ids. So
// redundancyAlone requires positive evidence: peers we can read, none of which can take over.
func (a *redundancyAlerter) classify(cl model.Cluster) redundancy {
	switch {
	case cl.Connected >= a.peers:
		return redundancyFull
	case cl.PeerCanTakeOver():
		return redundancyReduced
	case len(cl.Peers) > 0:
		return redundancyAlone
	default:
		return redundancyReduced // no peer detail: say only what the count supports
	}
}

// observe classifies the current cluster and fires on a change of state. Not
// quorate (an outage or the minority side of a partition) is out of scope for this
// warning -- a single node can't distinguish a minority partition from a true outage;
// that's the controller's fleet view -- so it holds state without firing.
//
// It fires on EVERY change between the three states, not only on entering and leaving trouble.
// The reduced -> alone edge is the one this exists for: a household whose peer anchor drops out
// while a witness keeps it quorate has lost its second copy, and under a two-state machine that
// transition looked like more of what had already been reported.
func (a *redundancyAlerter) observe(ctx context.Context, cl model.Cluster) {
	if a == nil || a.peers <= 0 {
		return
	}
	if !cl.Quorate {
		return // outage / minority: not this alert
	}
	cur := a.classify(cl)
	if !a.armed { // prime silently on the first definite reading (no startup false-positive)
		a.last, a.armed = cur, true
		return
	}
	if cur == a.last {
		return // no transition
	}
	a.last = cur
	a.fire(ctx, a.alertFor(cur, cl))
}

// alertFor is what the owner is actually told, and each body claims only what was read.
//
// The reduced body's reassurance is CONDITIONAL for that reason: it is added when a peer that
// could take over was actually seen, and omitted when the peer list was empty, where the
// honest statement is the count alone. Saying "another node still holds a copy" on no evidence
// is the failure this whole item is about, pointed the other way.
func (a *redundancyAlerter) alertFor(cur redundancy, cl model.Cluster) notify.Alert {
	switch cur {
	case redundancyFull:
		return notify.Alert{
			Level: notify.Recovered,
			Title: "Briard: redundancy restored",
			Body:  fmt.Sprintf("node %s reconnected all replicas (%d/%d connected).", a.node, cl.Connected, a.peers),
		}
	case redundancyAlone:
		return notify.Alert{
			Level: notify.Warning,
			Title: "Briard: no second copy",
			Body: fmt.Sprintf("node %s is the only node left holding your data (%d/%d peers connected, "+
				"none of them with a usable copy). It is still serving, but until a peer comes back "+
				"there is no second copy of your files and nothing to fail over to.",
				a.node, cl.Connected, a.peers),
		}
	default:
		reassurance := ""
		if cl.PeerCanTakeOver() {
			reassurance = " Another node still holds a full copy of your data."
		}
		return notify.Alert{
			Level: notify.Warning,
			Title: "Briard: reduced redundancy",
			Body: fmt.Sprintf("node %s lost a replica connection (%d/%d connected) — still serving, "+
				"but one more failure would cause an outage.%s", a.node, cl.Connected, a.peers, reassurance),
		}
	}
}

func (a *redundancyAlerter) fire(ctx context.Context, al notify.Alert) {
	fireAlert(ctx, a.n, a.logf, al)
}

// FireAlert is how every alert on the host side leaves: the local trail FIRST, then delivery.
// The order is the point. Delivery is the half that can be absent (the free tier configures no
// notifier at all) or can simply fail, so writing the trail after it would make the RECORD of
// an alert conditional on the alert having been delivered -- exactly backwards for the tier
// that delivers nothing, and what makes `briard alerts` truthful there (notify.LogLine).
//
// A nil notifier is a supported case, not a bug: a witness builds none (it has no redundancy
// signal to report), and it can still reach here from the recovery ladder, whose subject is the
// guest rather than the replica set. The trail is then the whole of the delivery.
func fireAlert(ctx context.Context, n notify.Notifier, logf func(string, ...any), al notify.Alert) {
	logf("%s", notify.LogLine(al)) // local trail, regardless of notifier -- what `briard alerts` reads
	if n == nil {
		return
	}
	nctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := n.Notify(nctx, al); err != nil {
		logf("alert delivery failed (%s): %v", al.Level, err)
	}
}
