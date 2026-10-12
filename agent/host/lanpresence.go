package host

import (
	"context"
	"time"

	"briard.io/agent/platform"
)

// IS THE GUEST ON THE LAN, OR ONLY INSIDE THE BRIDGE?
//
// Measured on a Windows bridge, twice: the first guest launched after a restart put frames INTO
// the bridge and none reached the wire, while the next launch, seconds later, leased normally.
// Nothing on the host can see the difference -- the host shares the bridge, so its own path to the
// guest is fine either way, and so is the agent's channel, which is built to survive the LAN being
// gone. Only a THIRD PARTY's answer says the guest's frames crossed the wire: the household's DHCP
// server answering the guest on this boot (guestagent.VIPLeased). Holding the VIP is NOT that
// answer -- a promotion claims the flock's remembered address before any server is asked, so a
// guest cut off from the LAN still holds one (measured: the obvious check passed its own control).
//
// So after every launch this agent makes, it waits for that answer, and a launch that never gets
// one is relaunched -- the cure that was measured -- a bounded number of times. It judges only what
// it can: a guest this agent LAUNCHED (an adopted one has already served), on a node whose VIP comes
// from DHCP (nothing answers for a fixed address), with no peers (a standby asks no server).

// lanWindow is how long a launch has to be answered: a guest boots in ~30 s and its promotion's
// DHCP exchange follows in seconds, so two minutes is a launch that did not reach the LAN rather
// than a slow one.
const lanWindow = 2 * time.Minute

// lanRelaunches bounds the cure per agent life. A router whose DHCP is down looks the same as the
// fault, and costs the household this many reboots of its guest before the agent stops trying.
const lanRelaunches = 2

type lanVerdict int

const (
	lanWait      lanVerdict = iota // nothing to say yet
	lanConfirmed                   // a server answered this boot: said once
	lanRelaunch                    // no answer in the window: relaunch this boot
	lanGiveUp                      // still no answer after every relaunch: said once
)

type lanPresence struct {
	boot     string    // the guest boot being judged
	since    time.Time // when it was first seen
	settled  bool      // this boot is decided: confirmed, adopted, relaunched or given up on
	left     int       // relaunches left
	adoptOne bool      // the first boot seen is the one this agent adopted, not launched
}

func newLANPresence(adopted bool) *lanPresence {
	return &lanPresence{left: lanRelaunches, adoptOne: adopted}
}

// open notes the boot the channel reached and reports whether it is still undecided -- i.e.
// whether the guest is worth asking. A boot it has not seen starts its window now.
func (p *lanPresence) open(boot string, now time.Time) bool {
	if p == nil || boot == "" {
		return false
	}
	if boot != p.boot {
		p.boot, p.since, p.settled = boot, now, p.adoptOne
		p.adoptOne = false
	}
	return !p.settled
}

// judge decides the open boot on one answer: did a DHCP server answer it.
func (p *lanPresence) judge(answered bool, now time.Time) lanVerdict {
	switch {
	case p == nil || p.settled:
		return lanWait
	case answered:
		p.settled = true
		return lanConfirmed
	case now.Sub(p.since) < lanWindow:
		return lanWait
	case p.left > 0:
		p.left--
		p.settled = true
		return lanRelaunch
	default:
		p.settled = true
		return lanGiveUp
	}
}

// lanReader is what judgeLAN asks of the guest channel.
type lanReader interface {
	BootID() string
	SupportsVIPLeased() bool
	VIPLeased(ctx context.Context, dev string) (bool, error)
}

// judgeLAN asks and acts, once per observe cycle, only while the boot is undecided. The relaunch
// is a clean powerdown run OFF the loop -- it takes up to a minute, and the guest may already be
// serving -- after which the recovery ladder sees a stopped guest and launches it again at once,
// as it does for any guest that stopped.
func (cfg Config) judgeLAN(ctx context.Context, r any, logf func(string, ...any)) {
	g, ok := r.(lanReader)
	if !ok || cfg.lan == nil || !g.SupportsVIPLeased() || !cfg.lan.open(g.BootID(), time.Now()) {
		return
	}
	rctx, cancel := context.WithTimeout(ctx, vipVerbTimeout)
	defer cancel()
	answered, err := g.VIPLeased(rctx, cfg.VIPDev)
	if err != nil {
		return
	}
	switch cfg.lan.judge(answered, time.Now()) {
	case lanConfirmed:
		logf("network: the household's DHCP server answered the guest on this boot -- its frames reach the LAN")
	case lanRelaunch:
		logf("network: no DHCP server answered the guest in the %s since it started -- relaunching it "+
			"(a launch can leave its frames inside the bridge, and the next one reaches the wire)", lanWindow)
		go func() {
			sctx, scancel := context.WithTimeout(context.Background(), platform.GuestShutdownGrace+15*time.Second)
			defer scancel()
			if err := platform.ShutdownVM(sctx, cfg.QMPSock, platform.GuestShutdownGrace); err != nil {
				logf("network: the guest did not power off for its relaunch (%v); stopping it", err)
				_ = platform.Adopt(cfg.guestSpec()).Stop()
			}
		}()
	case lanGiveUp:
		logf("network: still no DHCP answer for the guest after %d relaunches; leaving it running "+
			"-- is the router's DHCP up?", lanRelaunches)
	}
}
