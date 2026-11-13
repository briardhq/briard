package host

import (
	"context"

	"briard.io/agent/nic"
	"briard.io/shared/notify"
)

// checkFastNIC asserts fastNICAlert for the household's NIC in briard's bridge, read from the
// member itself; a node with no bridge has no NIC in one, which resolves it. Once per agent start:
// link speed changes with a cable or a switch, which is also when a person would act on it.
func (cfg Config) checkFastNIC(ctx context.Context, n notify.Notifier, logf func(string, ...any)) {
	if cfg.net == nil {
		return
	}
	member, bps := "", uint64(0)
	if cfg.net.Bridge {
		member, bps, _ = nic.Member(cfg.net.Parent)
	}
	if al, ok := fastNICAlert(member, bps); ok {
		fireAlert(ctx, n, logf, al)
	}
}
