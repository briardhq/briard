package platform

import (
	"context"
	"errors"
)

// The Windows side of the service-manager seam. Every one of these is a decision v5 owes, not a
// translation: the SCM covers Type=notify, Restart=, After= and TimeoutStopSec= closely enough,
// but has no WatchdogSec, no ExecStartPost and no systemd-run. What replaces each -- a frozen
// warden service, a Job Object, and which half of the commit doctrine survives -- is decided in
// DESIGN §9.9.1, not to be improvised here. Until then the host agent refuses on Windows rather
// than half-working.

func startTransient(context.Context, []string) ([]byte, error) { return nil, errors.ErrUnsupported }

func unitShow(string, string) (string, error) { return "", errors.ErrUnsupported }

func unitIsActive(context.Context, string) bool { return false }

func unitResetFailed(string) {}

func unitKill(string) {}

func unitStop(string) ([]byte, error) { return nil, errors.ErrUnsupported }
