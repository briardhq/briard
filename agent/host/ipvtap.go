package host

import (
	"context"
	"slices"

	"briard.io/agent/guest"
	"briard.io/agent/nic"
)

// THE COPY: the host duty the wireless substrate adds.
//
// ipvlan delivers a frame from the wire to a child only when its destination is an address
// configured on that child (nic/ipvtap.go). The guest's addresses live in the guest, so every
// tick the host copies the guest's live node IP and VIP onto the children in nic.HoldNS, and
// follows the VIP when it moves. LAN reach waits on the copy for at most one tick. The VIP is
// the host's to lease on this substrate (lease.go), and every reply to the host's requests is
// broadcast, so the lease waits on nothing here.
//
// ⚠️ THE GATE: NEVER COPY AN ADDRESS THE HOST HOLDS ON THE PARENT. ipvlan checks duplicates among
// its children and never against the parent, so it accepts the copy -- and the host then drops
// off the LAN in both directions until it is removed. Checked against the host's CURRENT
// addresses every tick, because the host's own lease can move under it.

// ipvtapCopier is the copy's machinery. On Config like beat, because it outlives one tick.
type ipvtapCopier struct {
	// hold and hostAddrs are nic.Hold and nic.IPv4s, as fields so the gate is testable without a
	// live namespace. Not a seam -- a test double inside one component.
	hold      func(context.Context, map[string][]string) error
	hostAddrs func(string) []string
}

func newIPvtapCopier() *ipvtapCopier {
	return &ipvtapCopier{hold: nic.Hold, hostAddrs: nic.IPv4s}
}

// tick is one pass of the copy. It changes nothing on any substrate but ipvtap. vip is the
// cycle's one answer to net.vip.
func (c *ipvtapCopier) tick(ctx context.Context, cfg Config, vip guest.VIPReader, logf func(string, ...any)) {
	if c == nil || cfg.net == nil || !cfg.net.Ipvtap || ctx.Err() != nil {
		return
	}
	host := c.hostAddrs(cfg.net.Parent)
	want := map[string][]string{cfg.net.SystemTap: nil}
	if node := cfg.guestNodeIP(); node != "" && !slices.Contains(host, node) {
		want[cfg.net.SystemTap] = []string{node}
	}
	// The VIP the guest holds now. A guest that does not answer leaves the service child as it
	// is: the read fails on a channel blip far more often than the address moves, and on this
	// substrate there is no peer the address could have moved to. The next answer settles it.
	if svc := cfg.net.ServiceTap; svc != "" && cfg.VIPDev != "" {
		rctx, cancel := context.WithTimeout(ctx, vipVerbTimeout)
		cidr, err := vip.VIP(rctx, cfg.VIPDev)
		cancel()
		if ctx.Err() != nil {
			return // a shutdown, not a guest that stopped answering (viproute.go says why)
		}
		if err == nil {
			want[svc] = nil
			if a := wantVIPRoute(cidr, nil); a != "" && !slices.Contains(host, a) {
				want[svc] = []string{a}
			}
		}
	}
	step, cancel := context.WithTimeout(ctx, vipVerbTimeout)
	defer cancel()
	if err := c.hold(step, want); err != nil {
		logf("network: could not copy the guest's addresses onto its ipvtap children: %v", err)
	}
}
