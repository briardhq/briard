package platform

import (
	"context"
	"errors"
)

func runIP(context.Context, ...string) ([]byte, error) { return nil, errors.ErrUnsupported }

// NodeRoutePresent and VIPRoutePresent answer present: the Windows host installs neither route
// (runIP is unsupported here), so there is nothing to restore.
func NodeRoutePresent(NodeRoute) bool            { return true }
func VIPRoutePresent(addr, dev, via string) bool { return true }
