package host

import (
	"context"
	"fmt"
	"time"

	"briard.io/shared/api"
	"briard.io/shared/notify"
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
	last  int // -1 unprimed, 0 full, 1 reduced
}

func newRedundancyAlerter(n notify.Notifier, node string, peers int, logf func(string, ...any)) *redundancyAlerter {
	return &redundancyAlerter{n: n, node: node, peers: peers, logf: logf, last: -1}
}

// Observe classifies the current status and fires on a full<->reduced transition. Not
// quorate (an outage or the minority side of a partition) is out of scope for this
// warning -- a single node can't distinguish a minority partition from a true outage;
// that's the controller's fleet view -- so it holds state without firing.
func (a *redundancyAlerter) observe(ctx context.Context, st api.NodeStatus) {
	if a == nil || a.peers <= 0 {
		return
	}
	if !st.Quorum.Quorate {
		return // outage / minority: not this alert
	}
	cur := 0
	if st.Quorum.Connected < a.peers {
		cur = 1
	}
	if a.last == -1 { // prime silently on the first definite reading (no startup false-positive)
		a.last = cur
		return
	}
	if cur == a.last {
		return // no transition
	}
	a.last = cur
	if cur == 1 {
		a.fire(ctx, notify.Alert{
			Level: notify.Warning,
			Title: "Briard: reduced redundancy",
			Body: fmt.Sprintf("node %s lost a replica connection (%d/%d connected) — still serving, "+
				"but one more failure would cause an outage.", a.node, st.Quorum.Connected, a.peers),
		})
	} else {
		a.fire(ctx, notify.Alert{
			Level: notify.Recovered,
			Title: "Briard: redundancy restored",
			Body:  fmt.Sprintf("node %s reconnected all replicas (%d/%d connected).", a.node, st.Quorum.Connected, a.peers),
		})
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
