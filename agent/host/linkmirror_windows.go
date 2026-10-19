package host

import (
	"context"

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
	if m.tick(ctx, member, member != "" && nic.Up(member), platform.Adopt(cfg.guestSpec()).SetSystemLink, logf) {
		cfg.lease.askNow() // the carrier back: the lease re-asks (lease.go)
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
