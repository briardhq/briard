package host

import (
	"context"
	"strings"
	"time"

	"briard.io/agent/guest"
	"briard.io/agent/guestagent/deadman"
	"briard.io/agent/platform"
)

// vipVerbTimeout bounds each call this reconcile makes -- the guest read and the `ip` invocations
// alike. The same 5s the snapshot's reads use: both are answered in milliseconds when they are
// answered at all, so this is not a budget but the point at which waiting longer stops telling us
// anything -- and the observe loop's watchdog threshold is sized on every call in it being bounded.
const vipVerbTimeout = 5 * time.Second

// KEEPING THE HOST'S ROUTE TO ITS OWN GUEST IN STEP WITH REALITY.
//
// platform/route.go says why the route exists; this says when it may be there. The rule is one
// line and everything else follows from it: the route is present exactly when THIS node's guest
// actually holds the VIP.
//
// That is narrower than "this node is configured with a VIP", and the difference is the whole
// reason this is a loop rather than a line in install.sh. On a two-node flock the VIP lives on
// one node at a time, and the host of the OTHER node reaches it perfectly well over the LAN --
// that traffic goes out the wire to a different machine and back, which macvtap does not
// obstruct. Routing a configured-but-not-held VIP into our own guest would take a working path
// away and replace it with a black hole. So the route follows net.vip -- the address the guest's
// service NIC is measured to hold -- and never cfg.VIPAddr or cfg.HealthURL, which say what we
// asked for rather than what happened.
//
// This OBSERVES a failover; it never drives one (AGENTS §4.2). Nothing here promotes, demotes or
// claims an address -- drbd-reactor moves the VIP and this notices afterwards.

// vipRouter holds the one fact the reconcile needs to remember: what it last installed. Without
// it the agent would shell out to `ip` on every cycle to re-assert a route that has not changed.
type vipRouter struct {
	dev    string // the host's end of the private link (WITNESS_TAP); "" = no link, nothing to do
	vipDev string // the guest NIC the VIP lands on (VIP_DEV); "" = this node claims no VIP
	// installed is the address currently routed over the link; "" = no route of ours exists.
	installed string
	// set/clear are the platform calls, as fields so the transition table below can be tested
	// without a live network. Not a seam (AGENTS §4.6) -- a test double inside one component.
	set   func(context.Context, platform.VIPRoute) error
	clear func(context.Context, string, string) error
}

func newVIPRouter(dev, vipDev string) *vipRouter {
	return &vipRouter{dev: dev, vipDev: vipDev, set: platform.SetVIPRoute, clear: platform.ClearVIPRoute}
}

// wantVIPRoute answers what the host should route, from what the guest answered.
//
// A verb ERROR means no route, and that is a decision rather than a fallback. An unreachable
// guest is certainly not serving the household from this machine, so withdrawing fails OPEN: it
// hands the address back to the LAN, where a peer that has taken the VIP over is reachable. The
// opposite reading -- keep the route because we cannot see well enough to change it -- keeps
// pointing the host at a guest that may have stopped serving, which is the only harm this path
// is capable of causing.
func wantVIPRoute(cidr string, err error) string {
	if err != nil {
		return ""
	}
	addr, _, _ := strings.Cut(cidr, "/")
	return addr
}

// reconcile brings the host's route into line with the guest's live VIP. It acts only on a
// CHANGE, so the steady state costs one control-channel verb per cycle and no subprocess at all.
// Every failure is logged and retried next cycle: the route is a convenience for the household's
// own machine, and nothing about the node's health, quorum or serving depends on it.
func (v *vipRouter) reconcile(ctx context.Context, r guest.VIPReader, logf func(string, ...any)) {
	// No private link (WITNESS_TAP unset) or no service NIC (a witness claims no VIP): there is
	// nothing to route and nothing to route it over.
	if v.dev == "" || v.vipDev == "" {
		return
	}
	// A SHUTDOWN IS NOT A DEMOTION, and this guard is the whole of that distinction.
	//
	// The guest is a DETACHED unit: it keeps serving across an agent stop, and an agent restart is
	// exactly what a self-update is ([V3.4]). Reading a cancelled context as "the guest did not
	// answer" would withdraw the route on the way out, so every restart would blip the
	// household's reachability from its own machine for a node that never stopped serving.
	// agent-readopt polls the VIP across a restart and asserts it never drops -- which is where
	// this was caught. The failure the fail-open rule exists for is the opposite shape: a LIVE
	// agent seeing a DEAD channel, i.e. a guest that has gone away while a peer may hold the VIP.
	if ctx.Err() != nil {
		return
	}
	// Bounded, like every other read in the observe loop: a guest that never answers must not hold
	// the loop open past the watchdog threshold (the Beat rule in host.go).
	rctx, rcancel := context.WithTimeout(ctx, vipVerbTimeout)
	defer rcancel()
	cidr, err := r.VIP(rctx, v.vipDev)
	if ctx.Err() != nil {
		return // cancelled mid-read: same reasoning as above, and err would only say so
	}
	want := wantVIPRoute(cidr, err)
	if want == v.installed {
		return
	}
	// The `ip` calls need their own bound -- rctx's is spent by the read above.
	actx, acancel := context.WithTimeout(ctx, vipVerbTimeout)
	defer acancel()
	// Withdraw first, always -- including when the address merely MOVED (a DHCP re-lease hands
	// the guest a different one). Two /32s to the same guest would both work, which is exactly
	// why the stale one must go: it would keep answering long after the household's address had
	// changed, and nobody would notice until a failover made it wrong.
	if v.installed != "" {
		if cerr := v.clear(actx, v.installed, v.dev); cerr != nil {
			logf("vip route: withdrawing %s failed: %v -- retrying next cycle", v.installed, cerr)
			return // leave `installed` set so the next cycle tries again
		}
		logf("vip route: withdrew %s from %s", v.installed, v.dev)
		v.installed = ""
	}
	if want == "" {
		if err != nil {
			logf("vip route: the guest did not answer where its VIP is (%v) -- no route from this host", err)
		}
		return
	}
	if serr := v.set(actx, platform.VIPRoute{Addr: want, Via: deadman.GuestIP, Dev: v.dev, Src: deadman.HostIP}); serr != nil {
		logf("vip route: %v -- this host cannot reach %s (the rest of the LAN can)", serr, want)
		return
	}
	v.installed = want
	logf("vip route: %s reachable from this host over %s", want, v.dev)
}
