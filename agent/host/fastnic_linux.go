package host

import (
	"context"

	"briard.io/shared/notify"
)

// checkFastNIC has nothing to check on Linux: no setting of the household's NIC is changed there.
func (Config) checkFastNIC(context.Context, notify.Notifier, func(string, ...any)) {}
