package host

import (
	"context"
	"errors"
	"testing"

	"briard.io/agent/platform"
)

// routeRecorder stands in for the two platform calls so the transition table can be enumerated
// without a live network. It records what was asked, in order, and can be told to fail.
type routeRecorder struct {
	sets     []platform.VIPRoute
	cleared  []string // "addr@dev" per withdrawal
	setErr   error
	clearErr error
}

func (rr *routeRecorder) set(_ context.Context, r platform.VIPRoute) error {
	rr.sets = append(rr.sets, r)
	return rr.setErr
}

func (rr *routeRecorder) clear(_ context.Context, addr, dev string) error {
	rr.cleared = append(rr.cleared, addr+"@"+dev)
	return rr.clearErr
}

func newTestRouter(dev, vipDev string) (*vipRouter, *routeRecorder) {
	rr := &routeRecorder{}
	// The two ends in system-subnet terms: the guest's node IP and the host's own address on the
	// same subnet. Written out here rather than defaulted, so the test states the topology it is
	// asserting about.
	v := &vipRouter{dev: dev, vipDev: vipDev, via: "10.0.0.1", src: "10.0.0.129", set: rr.set, clear: rr.clear}
	return v, rr
}

// The steady-state path, and the two properties that make it correct: the route points at the
// address the guest actually holds, via the guest's node IP over the private link (never
// on-link), and a second cycle with nothing changed does not touch the network at all.
func TestVIPRouter_InstallsThenIsQuiet(t *testing.T) {
	v, rr := newTestRouter("briard-priv0", "eth2")
	r := fakeStatus{vip: "192.168.9.225/24"}

	v.reconcile(t.Context(), r, quiet)
	if len(rr.sets) != 1 {
		t.Fatalf("sets = %+v, want exactly one", rr.sets)
	}
	got := rr.sets[0]
	want := platform.VIPRoute{Addr: "192.168.9.225", Via: "10.0.0.1", Dev: "briard-priv0", Src: "10.0.0.129"}
	if got != want {
		t.Errorf("route = %+v, want %+v", got, want)
	}

	v.reconcile(t.Context(), r, quiet)
	if len(rr.sets) != 1 || len(rr.cleared) != 0 {
		t.Errorf("an unchanged cycle touched the network: sets=%+v cleared=%v", rr.sets, rr.cleared)
	}
}

// A DHCP re-lease moves the household's address. The stale /32 must GO -- left behind it keeps
// answering for an address the flock has already yielded.
func TestVIPRouter_AddressMovedWithdrawsTheOldOne(t *testing.T) {
	v, rr := newTestRouter("briard-priv0", "eth2")
	v.reconcile(t.Context(), fakeStatus{vip: "192.168.9.225/24"}, quiet)
	v.reconcile(t.Context(), fakeStatus{vip: "192.168.9.240/24"}, quiet)

	if len(rr.cleared) != 1 || rr.cleared[0] != "192.168.9.225@briard-priv0" {
		t.Errorf("cleared = %v, want the old address withdrawn", rr.cleared)
	}
	if len(rr.sets) != 2 || rr.sets[1].Addr != "192.168.9.240" {
		t.Errorf("sets = %+v, want the new address installed", rr.sets)
	}
}

// The failover case. A node that no longer holds the VIP must not route it into its own guest:
// the peer that took it over is reachable over the LAN, and a stale /32 would replace a working
// path with a black hole.
func TestVIPRouter_DemotionWithdraws(t *testing.T) {
	v, rr := newTestRouter("briard-priv0", "eth2")
	v.reconcile(t.Context(), fakeStatus{vip: "192.168.9.225/24"}, quiet)
	v.reconcile(t.Context(), fakeStatus{vip: ""}, quiet) // demoted: the service NIC holds nothing

	if len(rr.cleared) != 1 {
		t.Errorf("cleared = %v, want the route withdrawn on demotion", rr.cleared)
	}
	if len(rr.sets) != 1 {
		t.Errorf("sets = %+v, want no second install", rr.sets)
	}
	if v.installed != "" {
		t.Errorf("installed = %q, want none", v.installed)
	}
}

// A guest we cannot ask is certainly not serving the household from this machine, so withdrawing
// fails open, back to the LAN, where a peer that has taken over can be reached. The fake still
// answers with an address alongside the error on purpose: were it empty, the withdrawal would
// follow from the empty address and this test would pass with the fail-open rule deleted. The
// error must be the only reason.
func TestVIPRouter_UnreadableGuestFailsOpen(t *testing.T) {
	v, rr := newTestRouter("briard-priv0", "eth2")
	v.reconcile(t.Context(), fakeStatus{vip: "192.168.9.225/24"}, quiet)
	v.reconcile(t.Context(), fakeStatus{vip: "192.168.9.225/24", vipErr: errors.New("channel down")}, quiet)

	if len(rr.cleared) != 1 {
		t.Errorf("cleared = %v, want the route withdrawn when the guest cannot be asked", rr.cleared)
	}
	if v.installed != "" {
		t.Errorf("installed = %q, want none", v.installed)
	}
}

// ...and with nothing installed, an unreadable guest is not a reason to run `ip` at all.
func TestVIPRouter_UnreadableGuestWithNoRouteIsSilent(t *testing.T) {
	v, rr := newTestRouter("briard-priv0", "eth2")
	v.reconcile(t.Context(), fakeStatus{vip: "192.168.9.225/24", vipErr: errors.New("channel down")}, quiet)
	if len(rr.sets) != 0 || len(rr.cleared) != 0 {
		t.Errorf("touched the network with nothing to reconcile: sets=%+v cleared=%v", rr.sets, rr.cleared)
	}
}

// No private link, or no service NIC (a witness): nothing to route and nothing to route it over.
func TestVIPRouter_NothingToDo(t *testing.T) {
	for _, c := range []struct{ name, dev, vipDev string }{
		{"no private link", "", "eth2"},
		{"no service NIC", "briard-priv0", ""},
		{"neither", "", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			v, rr := newTestRouter(c.dev, c.vipDev)
			v.reconcile(t.Context(), fakeStatus{vip: "192.168.9.225/24"}, quiet)
			if len(rr.sets) != 0 || len(rr.cleared) != 0 {
				t.Errorf("sets=%+v cleared=%v, want no action", rr.sets, rr.cleared)
			}
		})
	}
}

// A failed install is retried, not recorded as done.
func TestVIPRouter_FailedInstallRetries(t *testing.T) {
	v, rr := newTestRouter("briard-priv0", "eth2")
	rr.setErr = errors.New("ip: permission denied")
	v.reconcile(t.Context(), fakeStatus{vip: "192.168.9.225/24"}, quiet)
	if v.installed != "" {
		t.Fatalf("installed = %q after a failed install, want none", v.installed)
	}
	rr.setErr = nil
	v.reconcile(t.Context(), fakeStatus{vip: "192.168.9.225/24"}, quiet)
	if v.installed != "192.168.9.225" || len(rr.sets) != 2 {
		t.Errorf("installed=%q sets=%+v, want the retry to have succeeded", v.installed, rr.sets)
	}
}

// A failed withdrawal must not be followed by an install, and must not be forgotten: the host
// would otherwise hold two routes to one guest and remember neither.
func TestVIPRouter_FailedWithdrawalDoesNotInstallOver(t *testing.T) {
	v, rr := newTestRouter("briard-priv0", "eth2")
	v.reconcile(t.Context(), fakeStatus{vip: "192.168.9.225/24"}, quiet)
	rr.clearErr = errors.New("ip: device busy")
	v.reconcile(t.Context(), fakeStatus{vip: "192.168.9.240/24"}, quiet)

	if len(rr.sets) != 1 {
		t.Errorf("sets = %+v, want no install over a route that could not be withdrawn", rr.sets)
	}
	if v.installed != "192.168.9.225" {
		t.Errorf("installed = %q, want the un-withdrawn address remembered for the retry", v.installed)
	}
}

// A shutdown is not a demotion. The guest is a detached unit and keeps serving across an agent
// stop, and an agent restart is what a self-update is, so an agent on its way out must leave the
// route alone; withdrawing would drop the household's reachability from its own machine on every
// restart, for a node that never stopped serving. agent-readopt is the live version: it polls the
// VIP across a restart and asserts zero dropped ticks. Two guards enforce the rule (one before
// the read, one after for cancellation mid-call), so removing either alone stays green, by design:
// what this catches, and what must never pass, is both being gone.
func TestVIPRouter_ShutdownDoesNotWithdraw(t *testing.T) {
	v, rr := newTestRouter("briard-priv0", "eth2")
	v.reconcile(t.Context(), fakeStatus{vip: "192.168.9.225/24"}, quiet)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	v.reconcile(ctx, fakeStatus{vipErr: context.Canceled}, quiet)

	if len(rr.cleared) != 0 {
		t.Errorf("cleared = %v, want the route left alone: the agent is stopping, the guest is not", rr.cleared)
	}
	if v.installed != "192.168.9.225" {
		t.Errorf("installed = %q, want the route still recorded across a shutdown", v.installed)
	}
}

// ctxProbe answers VIP while recording whether the caller bounded the call. The observe loop's
// watchdog threshold is sized on every read in it carrying a deadline (host.go's Beat rule), so an
// unbounded verb here would let one unresponsive guest hold the loop open past it.
type ctxProbe struct {
	deadline bool
	called   bool
}

func (c *ctxProbe) VIP(ctx context.Context, _ string) (string, error) {
	c.called = true
	_, c.deadline = ctx.Deadline()
	return "192.168.9.225/24", nil
}

func TestVIPRouter_BoundsTheGuestRead(t *testing.T) {
	v, _ := newTestRouter("briard-priv0", "eth2")
	p := &ctxProbe{}
	v.reconcile(t.Context(), p, quiet)
	if !p.called {
		t.Fatal("the guest was never read")
	}
	if !p.deadline {
		t.Error("net.vip was called on an unbounded context -- an unresponsive guest would stall the observe loop")
	}
}

func TestWantVIPRoute(t *testing.T) {
	cases := []struct {
		name string
		cidr string
		err  error
		want string
	}{
		{"holds an address", "192.168.9.225/24", nil, "192.168.9.225"},
		{"holds none", "", nil, ""},
		{"unreadable", "192.168.9.225/24", errors.New("x"), ""},
		{"bare address without a prefix", "192.168.9.225", nil, "192.168.9.225"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := wantVIPRoute(c.cidr, c.err); got != c.want {
				t.Errorf("wantVIPRoute(%q, %v) = %q, want %q", c.cidr, c.err, got, c.want)
			}
		})
	}
}

// A route the kernel dropped comes back at the next network check, and nothing runs while every
// route is there: the node route and its neighbour by SetNodeRoute again, the VIP route by the
// reconcile that follows, which installs it because it no longer believes it installed.
func TestThePrivateLinksDroppedRoutesComeBack(t *testing.T) {
	cfg := Config{WitnessTap: "briard-priv0", SystemCIDR: "10.0.0.1/25", SystemHostCIDR: "10.0.0.129/32", Node: "n0de"}
	v, rr := newTestRouter("briard-priv0", "eth2")
	r := fakeStatus{vip: "192.168.9.225/24"}
	v.reconcile(t.Context(), r, quiet)

	nodeThere, vipThere := true, true
	var nodeSets []platform.NodeRoute
	k := linkKernel{
		nodePresent: func(platform.NodeRoute) bool { return nodeThere },
		vipPresent: func(addr, dev, via string) bool {
			if addr != "192.168.9.225" || dev != "briard-priv0" || via != "10.0.0.1" {
				t.Errorf("asked about %s on %s via %s, not the route the reconcile installed", addr, dev, via)
			}
			return vipThere
		},
		setNode: func(_ context.Context, nr platform.NodeRoute) error { nodeSets = append(nodeSets, nr); return nil },
	}
	cfg.restorePrivateLink(t.Context(), v, k, quiet)
	v.reconcile(t.Context(), r, quiet)
	if len(nodeSets) != 0 || len(rr.sets) != 1 {
		t.Fatalf("with every route there, the check installed: node %+v, vip %+v", nodeSets, rr.sets)
	}

	nodeThere, vipThere = false, false // the tap went down and up
	cfg.restorePrivateLink(t.Context(), v, k, quiet)
	v.reconcile(t.Context(), r, quiet)
	want := platform.NodeRoute{GuestIP: "10.0.0.1", Dev: "briard-priv0", Src: "10.0.0.129", LLAddr: deriveMAC("n0de", "wit")}
	if len(nodeSets) != 1 || nodeSets[0] != want {
		t.Errorf("node route restored as %+v, want once as %+v", nodeSets, want)
	}
	if len(rr.sets) != 2 || len(rr.cleared) != 0 {
		t.Errorf("the VIP route was not put back by the reconcile: sets %+v, cleared %v", rr.sets, rr.cleared)
	}

	// No private link, no routes of ours: nothing is asked.
	(Config{}).restorePrivateLink(t.Context(), &vipRouter{}, linkKernel{
		nodePresent: func(platform.NodeRoute) bool { t.Error("asked about a node route with no link"); return true },
		vipPresent:  func(string, string, string) bool { t.Error("asked about a VIP route never installed"); return true },
	}, quiet)
}
