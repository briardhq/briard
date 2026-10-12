package host

import (
	"context"

	"briard.io/agent/nic"
	"briard.io/shared/notify"
)

// checkFastNIC raises fastNICAlert for the household's NIC in briard's bridge, read from the member
// itself. Once per agent start: link speed changes with a cable or a switch, which is also when a
// person would act on it.
func (cfg Config) checkFastNIC(ctx context.Context, n notify.Notifier, logf func(string, ...any)) {
	if cfg.net == nil || !cfg.net.Bridge {
		return
	}
	member, bps := nic.MemberSpeed(cfg.net.Parent)
	if al, ok := fastNICAlert(member, bps); ok {
		fireAlert(ctx, n, logf, al)
	}
}
