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
	a.logf("alert [%s] %s — %s", al.Level, al.Title, al.Body) // local trail, regardless of notifier
	nctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := a.n.Notify(nctx, al); err != nil {
		a.logf("alert delivery failed (%s): %v", al.Level, err)
	}
}
