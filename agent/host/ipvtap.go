package host

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"briard.io/agent/guest"
	"briard.io/agent/nic"
	"briard.io/shared/notify"
)

// THE COPY: the host duty the wireless substrate adds.
//
// ipvlan delivers a frame from the wire to a child only when its destination is an address
// configured on that child (nic/ipvtap.go). The guest's addresses live in the guest, so every
// tick the host copies the guest's live node IP and VIP onto the children in nic.HoldNS, and
// follows the VIP when its lease moves. LAN reach and the guest's first unicast renewal wait on
// the copy for at most one tick, well inside a lease's T1. Guest DHCP stays the guest's: the
// broadcast flag (dhcpcd -J) gets the first lease through before anything is copied.
//
// ⚠️ THE GATE: NEVER COPY AN ADDRESS THE HOST HOLDS ON THE PARENT. ipvlan checks duplicates among
// its children and never against the parent, so it accepts the copy -- and the host then drops
// off the LAN in both directions until it is removed. Checked against the host's CURRENT
// addresses every tick, because the host's own lease can move under it.
//
// A guest VIP equal to the host's address means the server told the two apart by hardware address,
// which under ipvtap is the station's for both. Either it ignores DHCP client IDs, or the host's
// own client sends none and the server matched the guest to the host's lease by chaddr (dnsmasq
// does exactly that, against NixOS's stock dhcpcd -- measured in install-wifi-refuse). That is
// REFUSED rather than worked around: the VIP's DHCP client is stopped without a RELEASE (a
// RELEASE would hand back the host's own lease), the copy is never made, and the household is told
// to name an address. Choosing one and defending it ourselves stays out -- we never invent an
// address. The refusal holds until the agent restarts; the remedy names the reinstall that does.

// vipUnit is the guest unit that holds the VIP's lease. Its stop path exits dhcpcd with -x, which
// does not release (guest-image/configuration.nix, briard-vip-down).
const vipUnit = "briard-vip.service"

// vipStopper is the two writes the copier makes into the guest when it refuses: stop the VIP's
// client, then forget the address the flock remembered for it (guestagent.Client).
type vipStopper interface {
	ServiceStop(ctx context.Context, unit string) error
	ForgetVIP(ctx context.Context) error
}

// ipvtapCopier is the copy's state across ticks: only the refusal, which is sticky. Machinery on
// Config like beat, because the doctor reads the refusal and runs on a different call path.
type ipvtapCopier struct {
	refusedAddr string // the host's address the router handed the guest; "" = not refused
	refusedCIDR string // ...with the prefix, for the remedy's example
	// hold and hostAddrs are nic.Hold and nic.IPv4s, as fields so the gate is testable without a
	// live namespace. Not a seam -- a test double inside one component.
	hold      func(context.Context, map[string][]string) error
	hostAddrs func(string) []string
}

func newIPvtapCopier() *ipvtapCopier {
	return &ipvtapCopier{hold: nic.Hold, hostAddrs: nic.IPv4s}
}

// refused reports whether the copier refused the VIP. nil-safe: a zero Config has no copier.
func (c *ipvtapCopier) refused() bool { return c != nil && c.refusedAddr != "" }

// refusal is the refusal's detail and remedy, in the household's words. Empty when not refused.
func (c *ipvtapCopier) refusal() (detail, fix string) {
	if !c.refused() {
		return "", ""
	}
	_, prefix, _ := strings.Cut(c.refusedCIDR, "/")
	if prefix == "" {
		prefix = "24"
	}
	return fmt.Sprintf("your router gave Briard %s, the address of the machine it runs on: on Wi-Fi the two share one hardware address and are told apart only by DHCP client ID, which your router ignores or this machine's own DHCP client does not send", c.refusedAddr),
		fmt.Sprintf("pick a free address outside your router's DHCP range and run `sudo briard config set vip <address>/%s`", prefix)
}

// clear drops a refusal: the household has named an address (config.go's `config set vip`).
func (c *ipvtapCopier) clear() {
	if c != nil {
		c.refusedAddr, c.refusedCIDR = "", ""
	}
}

// tick is one pass of the copy. It changes nothing on any substrate but ipvtap. vip is the
// cycle's one answer to net.vip; r is the guest, for the stop a refusal needs.
func (c *ipvtapCopier) tick(ctx context.Context, cfg Config, vip guest.VIPReader, r any, n notify.Notifier, logf func(string, ...any)) {
	if c == nil || cfg.net == nil || !cfg.net.Ipvtap || ctx.Err() != nil {
		return
	}
	host := c.hostAddrs(cfg.net.Parent)
	want := map[string][]string{cfg.net.SystemTap: nil}
	if node := cfg.guestNodeIP(); node != "" && !slices.Contains(host, node) {
		want[cfg.net.SystemTap] = []string{node}
	}
	// The VIP the guest holds now. A guest that does not answer leaves the service child as it
	// is: the read fails on a channel blip far more often than the address moves, and on this
	// substrate there is no peer the address could have moved to. The next answer settles it.
	if svc := cfg.net.ServiceTap; svc != "" && cfg.VIPDev != "" {
		rctx, cancel := context.WithTimeout(ctx, vipVerbTimeout)
		cidr, err := vip.VIP(rctx, cfg.VIPDev)
		cancel()
		if ctx.Err() != nil {
			return // a shutdown, not a guest that stopped answering (viproute.go says why)
		}
		if err == nil {
			vip := wantVIPRoute(cidr, nil)
			if vip != "" && slices.Contains(host, vip) {
				c.refuse(ctx, r, n, vip, cidr, logf)
			} else if vip != "" {
				// The guest holds an address of its own: a refusal, if one was open, is over.
				fireAlert(ctx, n, logf, notify.Alert{
					Key:   "address",
					Kind:  notify.Resolved,
					Title: "Briard: this node has an address on your network",
					Body:  fmt.Sprintf("Briard is reachable at %s.", vip),
				})
			}
			want[svc] = nil
			if vip != "" && !c.refused() {
				want[svc] = []string{vip}
			}
		}
	}
	step, cancel := context.WithTimeout(ctx, vipVerbTimeout)
	defer cancel()
	if err := c.hold(step, want); err != nil {
		logf("network: could not copy the guest's addresses onto its ipvtap children: %v", err)
	}
}

// refuse stops the guest's VIP client and says why, once. The stop is repeated on every tick that
// sees the address again (a guest restart runs the client again); the words are not.
func (c *ipvtapCopier) refuse(ctx context.Context, r any, n notify.Notifier, addr, cidr string, logf func(string, ...any)) {
	first := !c.refused()
	c.refusedAddr, c.refusedCIDR = addr, cidr
	if s, ok := r.(vipStopper); ok {
		sctx, cancel := context.WithTimeout(ctx, vipVerbTimeout)
		if err := s.ServiceStop(sctx, vipUnit); err != nil {
			logf("network: could not stop the guest's address client: %v", err)
		}
		// ...and forget it, AFTER the stop so no lease event writes it back: a refused address
		// left remembered is claimed optimistically at the next promotion, and refused again.
		if err := s.ForgetVIP(sctx); err != nil {
			logf("network: could not forget the refused address on the volume: %v", err)
		}
		cancel()
	}
	if !first {
		return
	}
	detail, fix := c.refusal()
	// THE LINE install.sh's closing wait ends on, and prints: keep the prefix in step with it.
	logf("network: refused: %s; %s", detail, fix)
	fireAlert(ctx, n, logf, notify.Alert{
		Key:      "address",
		Kind:     notify.Open,
		Severity: notify.Critical,
		Title:    "Briard: this node cannot take an address on your network",

		Body: detail + ". Nothing in your home can reach Briard until you " + fix + ".",
	})
}
