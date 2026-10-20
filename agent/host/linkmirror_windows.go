package host

import (
	"context"
	"time"

	"briard.io/agent/nic"
	"briard.io/agent/platform"
)

// mirrorLink runs m's tick over the household's NIC in briard's bridge (linkmirror.go). The NIC is
// read itself, never the bridge: the bridge's own state is the OR with the TAP, which is what the
// mirror exists to correct.
func (cfg Config) mirrorLink(ctx context.Context, m *linkMirror, logf func(string, ...any)) {
	if cfg.net == nil || !cfg.net.Bridge {
		return
	}
	member, _ := nic.MemberSpeed(cfg.net.Parent)
	g := platform.Adopt(cfg.guestSpec())
	if m.tick(ctx, member, member != "" && nic.Up(member), g.SetSystemLink, logf) {
		cfg.lease.askNow() // the carrier back: the lease re-asks (lease.go)
		// AND THE GUEST SAYS WHERE IT IS. The bridge forgets where the guest's addresses live
		// when its member's media cycles (measured on a radio re-association: the host back in
		// 3 s, the VIP dark 35-46 s to every peer that had it cached, until each one's own ARP
		// entry expired), and the guest, holding the same addresses, sends nothing unasked.
		actx, cancel := context.WithTimeout(ctx, 5*time.Second)
		if err := g.AnnounceSelf(actx); err != nil {
			logf("network: could not have the guest announce itself after the link came back: %v", err)
		}
		cancel()
	}
}

// linkChanges wakes the observe loop for the mirror alone when Windows reports an interface
// change, so a carrier edge is mirrored in milliseconds rather than at the next tick; the tick
// still runs the mirror, so a lost notification costs at most one tick. nil (never ready) without
// a bridge, or when the notification cannot be registered.
func (cfg Config) linkChanges(logf func(string, ...any)) <-chan struct{} {
	if cfg.net == nil || !cfg.net.Bridge {
		return nil
	}
	ch, err := nic.LinkChanges()
	if err != nil {
		logf("network: no interface-change notifications (%v); the guest's link follows the NIC's carrier at each tick only", err)
	}
	return ch
}
