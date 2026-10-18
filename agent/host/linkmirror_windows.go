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
	m.tick(ctx, member, member != "" && nic.Up(member), platform.Adopt(cfg.guestSpec()).SetSystemLink, logf)
}
