package host

import (
	"context"
	"time"
)

// linkMirror makes the guest's system link follow the carrier of the household's NIC in briard's
// Windows bridge. A Windows bridge is connected while any member is, and qemu holds the TAP
// connected for as long as the guest runs, so without it a pulled cable or a dropped radio leaves
// the bridge up: Windows keeps the lease, the default route and the DNS servers of a link that is
// gone, and sends no DHCP request when it comes back. Setting the link down with the NIC's carrier
// lets Windows see the bridge go down and come back with it (the bundled qemu turns the link into
// the TAP's media status).
//
// THE ONE RULE: NEVER UP WHILE THE NIC IS DOWN. A TAP that comes up while the NIC has no carrier
// is a media-connect with nothing behind it, and Windows drops the host's address over it. So the
// link always follows the NIC's carrier, in both directions, and a NIC that is not there at all
// counts as down.
//
// Lives for the observe loop, and starts knowing nothing: the first tick of a loop sets the link
// whatever it is, which puts it in the NIC's state after a launch (qemu opens the TAP connected)
// and re-asserts it under an agent that restarted beside a running guest.
type linkMirror struct {
	set     *bool // the link last set this loop; nil until the first set succeeds
	failing bool  // the last set failed: said once, until one succeeds
}

// tick sets the link to the NIC's carrier when it differs from the link last set, and reports the
// NIC's carrier coming BACK -- an up edge after the loop's first set, the one place the edge is
// known on Windows, where the lease re-asks on it (lease.go's askNow). A failed set
// leaves the record as it was, so the next tick tries again.
func (m *linkMirror) tick(ctx context.Context, member string, carrier bool, set func(context.Context, bool) error, logf func(string, ...any)) (back bool) {
	up := member != "" && carrier
	if m.set != nil && *m.set == up {
		return
	}
	sctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	err := set(sctx, up)
	cancel()
	if err != nil {
		if !m.failing {
			logf("network: could not set the guest's link %s to follow %s: %v", onOff(up), memberName(member), err)
		}
		m.failing = true
		return
	}
	first := m.set == nil
	m.set, m.failing = &up, false
	switch {
	case up && !first:
		logf("network: %s has its carrier back; the guest's link is up again", memberName(member))
		back = true
	case !up:
		logf("network: %s has no carrier; the guest's link is down with it, so Windows sees the bridge down", memberName(member))
	}
	return back
}

func onOff(up bool) string {
	if up {
		return "up"
	}
	return "down"
}

// memberName is how a log line names the household's NIC, including when the bridge has none.
func memberName(member string) string {
	if member == "" {
		return "the bridge's household NIC (none is in the bridge)"
	}
	return member
}
