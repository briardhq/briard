package host

import (
	"context"
	"fmt"
	"time"

	"briard.io/shared/api"
	"briard.io/shared/model"
	"briard.io/shared/notify"
)

// redundancy is what this node's replica set currently is, and it is three states rather than
// two because "a peer is gone" and "the only other copy of the household's data is gone" are
// different facts that the peer COUNT cannot tell apart. On
// the shipped anchor+anchor+witness flock both losses read as connected 1-of-2; only one of
// them means the house is down to a single disk.
type redundancy int

const (
	redundancyFull    redundancy = iota // every expected peer connected
	redundancyReduced                   // a peer is gone; a usable copy of the data remains, or we cannot tell
	redundancyAlone                     // no connected peer carries a usable copy: one disk left
)

// redundancyAlerter says what the replica set is, every reading, and the alert store turns that
// into the edge-triggered alert the owner sees: open when a data node loses redundancy -- still
// quorate and serving, but a peer connection dropped, so "one more failure would cause an
// outage" -- and resolved when all replicas reconnect. It is the agent-side signal derived from
// the DRBD/quorum state the observe loop already reads. Not built on a witness (its view is
// redundant with the data nodes') nor a single-node cluster (no redundancy to lose).
//
// PRIMED ON THE FIRST READING, ONE WAY: a flock still converging at startup reads reduced for a
// few seconds, so the first reading may resolve but never open. After that every reading is
// asserted and the store decides whether it is news.
type redundancyAlerter struct {
	n     notify.Notifier
	node  string
	peers int // expected connected peers (mesh size - 1)
	logf  func(string, ...any)
	armed bool // false until the first definite reading
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

// observe classifies the current cluster and asserts it. Not quorate (an outage or the
// minority side of a partition) is out of scope for this alert -- a single node can't
// distinguish a minority partition from a true outage; that's the controller's fleet view --
// so it says nothing.
//
// Every change between the three states is a change of title under one key, so the store
// records and pushes each: the reduced -> alone edge is the one this exists for. A household
// whose peer anchor drops out while a witness keeps it quorate has lost its second copy, and
// under a two-state machine that transition looked like more of what had already been reported.
func (a *redundancyAlerter) observe(ctx context.Context, cl model.Cluster) {
	if a == nil || a.peers <= 0 {
		return
	}
	if !cl.Quorate {
		return // outage / minority: not this alert
	}
	cur := a.classify(cl)
	if !a.armed {
		a.armed = true
		if cur != redundancyFull {
			return // the first reading may not open: the flock may still be converging
		}
	}
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
			Key:   "redundancy",
			Kind:  notify.Resolved,
			Title: "Briard: redundancy restored",
			Body:  fmt.Sprintf("node %s reconnected all replicas (%d/%d connected).", a.node, cl.Connected, a.peers),
		}
	case redundancyAlone:
		return notify.Alert{
			Key:      "redundancy",
			Kind:     notify.Open,
			Severity: notify.Critical,
			Title:    "Briard: no second copy",
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
			Key:      "redundancy",
			Kind:     notify.Open,
			Severity: notify.Warning,
			Title:    "Briard: reduced redundancy",
			Body: fmt.Sprintf("node %s lost a replica connection (%d/%d connected) — still serving, "+
				"but one more failure would cause an outage.%s", a.node, cl.Connected, a.peers, reassurance),
		}
	}
}

func (a *redundancyAlerter) fire(ctx context.Context, al notify.Alert) {
	fireAlert(ctx, a.n, a.logf, al)
}

// clockAlerter warns when the host's clock has not been synchronised with a time server for
// clockUnsyncedFor, and says so again when it is. A wrong clock turns a valid cert into
// a refusal and misdates every alert and backup; an RTC-less board boots with whatever time it
// last saved. We only report: keeping time is the OS's job, and a host whose NTP is on keeps it.
//
// THE HOST'S CLOCK, NOT THE GUEST'S. The guest takes the host's time at every boot and then keeps
// its own with its own timesyncd over the same network, so a network that starves one starves
// both, and the host is the one we can read without a verb.
//
// THE GRACE IS THE PRIMING. A host that just booted reads "no" until its first sync, so only an
// hour of continuous "no" opens the alert. AN UNKNOWN IS NOT AN ALARM: a host with no timedatectl
// (Windows) or one that did not answer neither starts nor clears the hour.
type clockAlerter struct {
	read     func(context.Context) string // reportcard.NTPSynced in production
	unsynced time.Time                    // start of the current run of "no"; zero otherwise
	next     time.Time                    // when to read again: timedatectl wakes timedated over D-Bus
}

const (
	clockUnsyncedFor = time.Hour
	clockReadEvery   = 5 * time.Minute
)

func (c *clockAlerter) observe(ctx context.Context, n notify.Notifier, node string, now time.Time, logf func(string, ...any)) {
	if now.Before(c.next) {
		return
	}
	c.next = now.Add(clockReadEvery)
	switch c.read(ctx) {
	case "no":
		if c.unsynced.IsZero() {
			c.unsynced = now
		}
		if now.Sub(c.unsynced) >= clockUnsyncedFor {
			fireAlert(ctx, n, logf, notify.Alert{
				Key:      "clock",
				Kind:     notify.Open,
				Severity: notify.Warning,
				Title:    "Briard: clock not synchronised",
				Body: fmt.Sprintf("node %s has not synchronised its clock with a time server for over an hour. "+
					"Certificates, alerts and backups are dated by this clock; check that the machine can "+
					"reach the internet and that `timedatectl set-ntp true` is on.", node),
			})
		}
	case "yes":
		c.unsynced = time.Time{}
		fireAlert(ctx, n, logf, notify.Alert{
			Key:   "clock",
			Kind:  notify.Resolved,
			Title: "Briard: clock synchronised",
			Body:  fmt.Sprintf("node %s is synchronised with a time server again.", node),
		})
	}
}

// serviceAlerter tells the owner when an installed service has stopped working on the node that
// runs it, and again when it works. Nothing else does: a service is not a promoter-chain member,
// so a crashed one demotes nothing, and the front door answers for the node, so the node reads
// healthy while the service behind it is down.
//
// DOWN IS "STOPPED" OR "RUNNING AND UNHEALTHY", one condition under one title -- a crash-loop
// moves between the two every few seconds, and the store records a change of title as news.
// WORKING IS "RUNNING AND HEALTHY", and nothing short of it resolves. STARTING AND EMPTY SAY
// NOTHING: a service booting is not down, and empty is "not asked" (a Secondary, a channel
// hiccup, an app that told us nothing) -- an unknown is not an alarm.
//
// THE GRACE IS THE DEBOUNCE, and it is held across the readings that say nothing: a crash-loop
// reads stopped, then starting, then stopped, and never healthy, so its run of "down" is never
// broken and it opens. It opens only on a down reading, so a service that blipped once and is now
// starting through a long migration does not. The deliberate stops -- an install's quiesce,
// a restore -- are shorter than the grace.
//
// A NODE THAT NO LONGER HOLDS THE VOLUME RESOLVES what it opened: the services run on whichever
// node does, and that node asserts its own.
type serviceAlerter struct {
	down map[string]time.Time // per service: start of the current run of "down"
}

const serviceDownFor = 10 * time.Minute

// observe takes the cycle's report. known is false when the cluster read failed, which says
// nothing about who holds the volume; serving is whether this node does.
func (a *serviceAlerter) observe(ctx context.Context, n notify.Notifier, node string, svcs []api.ServiceStatus, known, serving bool, now time.Time, logf func(string, ...any)) {
	if a.down == nil {
		a.down = map[string]time.Time{}
	}
	for _, s := range svcs {
		key := "service:" + s.Name
		switch {
		case known && !serving:
			delete(a.down, s.Name)
			fireAlert(ctx, n, logf, notify.Alert{
				Key:   key,
				Kind:  notify.Resolved,
				Title: "Briard: " + s.Name + " handed over",
				Body:  fmt.Sprintf("node %s no longer runs %s; the node holding your data does, and says so if it is not working.", node, s.Name),
			})
		case s.State == api.StateRunning && s.Health == api.StateHealthy:
			delete(a.down, s.Name)
			fireAlert(ctx, n, logf, notify.Alert{
				Key:   key,
				Kind:  notify.Resolved,
				Title: "Briard: " + s.Name + " is working again",
				Body:  fmt.Sprintf("%s is running and answering on node %s.", s.Name, node),
			})
		case s.State == api.StateStopped || s.Health == api.StateUnhealthy:
			since, ok := a.down[s.Name]
			if !ok {
				a.down[s.Name], since = now, now
			}
			if now.Sub(since) < serviceDownFor {
				continue
			}
			// No "Briard keeps restarting it": a crashed unit is, but one converge declined to
			// prepare was never started, and a running one that does not answer is left alone.
			what := "has not been running"
			if s.State == api.StateRunning {
				what = "has been running without answering"
			}
			fireAlert(ctx, n, logf, notify.Alert{
				Key:      key,
				Kind:     notify.Open,
				Severity: notify.Critical,
				Title:    "Briard: " + s.Name + " is not working",
				Body: fmt.Sprintf("%s %s on node %s for over %d minutes. The rest of the node is unaffected.",
					s.Name, what, node, int(serviceDownFor/time.Minute)),
			})
		}
	}
}

// fireAlert is how every alert on the host side leaves: through the notifier, which in
// production is the alert store in front of delivery (alertStore). The store writes the record
// FIRST and only then pushes, so the record of an alert never depends on the alert having been
// delivered -- the free tier delivers nothing, and its record is the whole of the alerting.
//
// Emitters call this with what they see, every time they look; the store decides whether it is
// a change. The log line is a log, for a person reading the journal next to everything else
// the agent said; it is not where an alert is kept.
//
// A nil notifier is a supported case in tests and on a witness built without one; production
// always has the store.
func fireAlert(ctx context.Context, n notify.Notifier, logf func(string, ...any), al notify.Alert) {
	if n == nil {
		return
	}
	nctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := n.Notify(nctx, al); err != nil {
		logf("alert delivery failed (%s %s): %v", al.Key, al.Kind, err)
	}
}
