package host

import "context"

// mirrorLink does nothing on Linux. What makes set_link reach the host is the link hook in the
// bundled qemu's Windows tap backend; qemu's Linux tap backend has none, so here a set would take
// down the guest's NIC and leave the host's view of the link as it was.
func (Config) mirrorLink(context.Context, *linkMirror, func(string, ...any)) {}

func (Config) linkChanges(func(string, ...any)) <-chan struct{} { return nil }
