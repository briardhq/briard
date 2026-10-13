package host

import (
	"context"
	"errors"
)

// restartSelf has no Windows arm yet: the update path there is built around the service manager
// as one piece, and until it exists an armed candidate is refused rather than half-trialled.
func restartSelf(context.Context, string) error { return errors.ErrUnsupported }
