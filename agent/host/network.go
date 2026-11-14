package host

import (
	"context"
	"fmt"
	"time"

	"briard.io/agent/cloud"
	"briard.io/agent/nic"
	"briard.io/agent/platform"
	"briard.io/shared/api"
)

// The agent owns the network: the host's side of the guest's L2 is built and kept here, never by
// an installer-generated script or a unit of its own. A script the installer writes is frozen: a
// fix ships in a release and reaches no installed node (accept_ra, ALLMULTI's expiring-cache
// trap). A oneshot cannot adapt: a macvtap cannot be re-parented, only deleted and recreated,
// which invalidates the fd handed to qemu and so means relaunching the guest; only the thing that
// runs the guest can sequence that. A `Requires=` on a network unit takes the node dark: a failed
// unit means the agent never starts, with nothing left to report, retry, or answer the admin door,
// so a cable out at boot is a node nobody can diagnose. The agent starts, says what is wrong, and
// keeps converging.

// netTick is how often the wait loop re-asks while the node has no usable device -- a node waiting
// for a cable is not a node that should be busy -- and the bound on one network step (a choice, a
// converge, a rebuild).
const netTick = 10 * time.Second

// netSettle bounds how long a cold start holds its choice for a NIC whose lease is still in
// flight. Thirty seconds covers a home router's DHCP at boot with room to spare, and is what a
// NIC that never gets a gateway costs each cold start.
const netSettle = 30 * time.Second

// netCheckTicks is how many status ticks apart the observe loop runs its network checks (the
// converger, the reparenter, the link mirror), the loop's first tick included: 15 s on a shipped
// node. None needs 5 s: the reparenter waits minutes before moving anything, the converger repairs
// drift, and the mirror's edges arrive as Windows' notification, which wakes the loop at once. On
// Windows each check is also a handful of full IP Helper walks.
const netCheckTicks = 3

// linksPresent reports whether every device this node's config names is already built, the hot
// path's whole question: an agent finding its devices there has nothing to build, whoever built
// them (its own previous life, or a rig that owns the substrate). Callers have already established
// that the config names devices at all. It asks of construction, not media (nic.Built): a cable
// out under a running guest is no cold start. A device not in this namespace is asked of
// nic.HoldNS, where an ipvtap node's children live, as present as a macvtap's: an agent restarting
// under its Wi-Fi guest must keep the parent they stand on rather than re-select (awaitNetwork).
func (cfg Config) linksPresent(ctx context.Context) bool {
	for _, d := range []string{cfg.SystemTap, cfg.ServiceTap, cfg.WitnessTap} {
		if d == "" || nic.Built(d) {
			continue
		}
		if up, _ := nic.Held(ctx, d); !up {
			return false
		}
	}
	return true
}

// netSpec derives the host side of the guest's L2 from the selected device. It is derived, not
// configured: whether the parent is a bridge or a wireless station is decided by the machine, not
// a mode the operator typed. A wireless station is macvtap's spec with ipvtap children
// (nic/ipvtap.go).
func (cfg Config) netSpec(dev string, bridge, wireless bool) nic.Spec {
	// ⚠️ Narrowed first, and here rather than at the caller so the two cannot disagree: the spec
	// must describe the node the substrate makes, and on a bridge there is no service tap and no
	// private link, so the config as written would name devices the guest cannot use.
	cfg = cfg.applySubstrate(bridge)
	s := nic.Spec{
		Parent: dev, Bridge: bridge, Ipvtap: wireless && !bridge,
		SystemTap: cfg.SystemTap, ServiceTap: cfg.ServiceTap, PrivTap: cfg.WitnessTap,
	}
	if bridge {
		// One port on the bridge, and the host's own system-subnet address on the bridge itself:
		// a /32 plus one on-link route to its guest's node IP. The host reaches the one machine on
		// that subnet it has business with and claims nothing else of the range: a /24 would add
		// only an implicit second connected route for the whole system subnet on the host's
		// primary device, beside its real LAN address. ARP is unaffected, because the guest's
		// eth1 is on this same L2 and the request arrives on the interface holding the address.
		s.Addrs = []nic.Addr{{CIDR: cfg.SystemHostCIDR, Dev: dev}}
		if g := cfg.guestNodeIP(); g != "" && cfg.hostNodeIP() != "" {
			s.Routes = []nic.Route{{Dst: g, Dev: dev, Src: cfg.hostNodeIP()}}
		}
		return s
	}
	if s.PrivTap != "" {
		// Both addresses on the private tap: the link's own host end, and this node's
		// system-subnet /32. A /32 because under macvtap this wire reaches exactly one machine,
		// and a /24 there would claim an on-link route for the whole system subnet over it,
		// competing with the route the guest's own peers need.
		s.Addrs = []nic.Addr{
			{CIDR: cfg.PrivHostCIDR, Dev: s.PrivTap},
			{CIDR: cfg.SystemHostCIDR, Dev: s.PrivTap},
		}
	}
	return s
}

// applySubstrate narrows the config to what the derived substrate has: the default device names
// are the macvtap shape (three taps, the one every Linux node ships in), and this is the one
// transformation that makes a bridge parent's config true. ⚠️ It is the one place in the agent
// that overrides config; everywhere else the environment wins, and here too for the values it does
// not touch. But a bridge-parented node has no second tap and no private link, and honouring a
// configured name for a device that cannot exist would render a NIC the guest cannot use: the
// substrate is a fact about the machine, not a preference.
func (cfg Config) applySubstrate(bridge bool) Config {
	if !bridge {
		if cfg.NetMode == "" {
			cfg.NetMode = platform.NetMacvtap
		}
		return cfg
	}
	// The Linux clone of the Windows shape: one tap, so the guest gets one kernel NIC and makes
	// its service identity as a macvlan child of it. No second tap, so no eth2 from us; no third,
	// so no eth3: host and guest already share one L2 here, and the private link would be a
	// second tap Windows cannot give us anyway.
	cfg.ServiceTap, cfg.WitnessTap, cfg.WitnessCIDR = "", "", ""
	cfg.VIPParent = "eth1"
	cfg.NetMode = platform.NetBridge
	return cfg
}

// awaitNetwork brings the host's side of the guest's L2 up, waiting for a usable device if there
// is none yet, and returns the config the derived substrate implies. ⚠️ It runs after READY, not
// in the READY path: the unit is Type=notify + WatchdogSec=20 + Restart=on-failure with
// StartLimitIntervalSec=0, so network work before READY turns a transient fault into a crashloop
// that never stops. READY already means "the agent started", not "the node is healthy": that
// distinction exists for exactly this kind of work.
//
// The hot path (a device there and already converged) returns on the first pass, so an ordinary
// boot or agent restart pays nothing. The cold path waits rather than failing: no default route
// yet and none at all are the same reading (nic's sentinels say so), and only elapsed time tells
// them apart. While it waits it answers: the admin door is already listening, and every directive
// gets the refusal the report card would have given, naming the device it wanted and the override
// that fixes it. That is the whole difference from `Requires=`, which leaves nothing running to
// ask.
func (cfg Config) awaitNetwork(ctx context.Context, local <-chan localRequest, dg *degraded, logf func(string, ...any)) (Config, error) {
	// This agent was not given a network to build: no device names means no guest NICs, as in
	// every agent-less harness and every unit test that constructs a Config by hand. It must be
	// answered before the wait below, whose exit is a successful Converge, which on an empty spec
	// can never succeed; without it the loop is unbounded on exactly the configurations that have
	// nothing for it to do.
	if cfg.SystemTap == "" {
		return cfg, nil
	}
	// The hot path, a check rather than a cache: the devices the config names are already up, so
	// there is nothing to create or validate. An agent restart (every self-update is one) takes
	// it, as does every rig and lab node that builds its own devices, so it must not probe: a
	// probe is a netlink write answering a question nobody asked, and where it cannot succeed (a
	// container's veth) it would send a perfectly healthy node into the degraded wait below.
	//
	// It reads the parent off the device rather than selecting, so the tick has a parent to
	// re-assert against and the guest keeps the one it stands on. Re-selecting would turn a
	// self-update into the household's apps moving networks, and an upgrade's rollback would then
	// compare health across two changes, not one. The parent moves only at a cold start (the loop
	// below) or when it is gone (reparent.go); BRIARD_NIC does not move it either, and the live
	// rule takes it as the answer once the standing parent is gone. A device stacked on nothing (a
	// rig's own tap) has no parent to read and gets the selection. One that comes up empty is not
	// a failure: the devices are up, and an agent that did not build them has no business
	// insisting on which parent they hang off.
	if cfg.linksPresent(ctx) {
		sel := nic.Selection{Dev: nic.ParentOf(cfg.SystemTap)}
		if sel.Dev == "" {
			_, sel.Dev = nic.Held(ctx, cfg.SystemTap) // an ipvtap child, in nic.HoldNS
		}
		if sel.Dev == "" {
			sel = nic.Select(cfg.NICOverride, cfg.recordedNetwork().Parent)
		} else {
			sel.Bridge, sel.Wireless = nic.IsBridge(sel.Dev), nic.Wireless(sel.Dev)
			// Asked through the selector, because on Windows NIC names the bridge's member.
			if o := cfg.NICOverride; o != "" && o != sel.Dev {
				if s := nic.Select(o, sel.Dev); s.Dev != sel.Dev || s.SwapIn != "" {
					logf("network: NIC names %s, but the guest's L2 stands on %s; it moves when %s is gone, or at the next machine start",
						o, sel.Dev, sel.Dev)
				}
			}
		}
		// Numbered before the spec is built, because the spec is made of this node's addresses
		// (subnets.go). This keeps the no-probing rule above: a node that has drawn before (every
		// restart, every self-update) reads its record and touches nothing, and a rig states its
		// own address, which stops a draw dead. What is left is a product node whose record is
		// gone, where probing is exactly right. A draw that refuses falls through to the wait
		// below rather than building a substrate with no addresses in it.
		numbered, err := cfg.numberThisNode(ctx, sel.Dev, logf)
		if err == nil {
			cfg = numbered
			if sel.Dev != "" {
				spec := cfg.netSpec(sel.Dev, sel.Bridge, sel.Wireless)
				cfg = cfg.applySubstrate(sel.Bridge)
				cfg.net = &spec
				logf("network: the guest's L2 is already up on %s (%s)", sel.Dev, substrateName(spec))
			} else {
				logf("network: the guest's L2 is already up; this agent did not build it and will not touch it")
			}
			return cfg, nil
		}
		logf("network: %v", err)
	}
	// The cold start, the one moment the parent is chosen afresh. The parent last served on is
	// preferred on a tie, so a restart that finds two equal uplinks stays where it was.
	var said, held string // the last degradation said, and the last settle (not a degradation)
	settled := time.Now().Add(netSettle)
	prefer := cfg.recordedNetwork().Parent
	for {
		// Hold the choice while a NIC has a link and no default route yet (DHCP still in flight
		// at boot), so a boot picks neither the radio that associated first nor nothing. Bounded,
		// because a NIC with no gateway by design (a storage link) holds it every cold start, and
		// waived for BRIARD_NIC, which needs no ranking.
		if d := nic.Settling(); d != "" && cfg.NICOverride == "" && time.Now().Before(settled) {
			why := d + " has a link and no default route yet; waiting for it before choosing"
			if why != held {
				logf("network: %s", why)
				held = why
			}
			if werr := cfg.waitTick(ctx, local, why, logf); werr != nil {
				return cfg, werr
			}
			continue
		}
		choose, chosen := context.WithTimeout(ctx, netTick)
		sel := nic.Choose(choose, cfg.NICOverride, prefer)
		chosen()
		var spec nic.Spec
		var err error
		var numbered Config
		if sel.Usable() && sel.SwapIn != "" {
			err = cfg.swapBeforeLaunch(ctx, sel, logf) // its own budget: a swap outlasts a tick
		}
		step, cancel := context.WithTimeout(ctx, netTick)
		if sel.Usable() && err == nil {
			// The draw is bounded by its own budget rather than by this tick (subnets.go), so it
			// is given ctx and not step.
			if numbered, err = cfg.numberThisNode(ctx, sel.Dev, logf); err == nil {
				spec = numbered.netSpec(sel.Dev, sel.Bridge, sel.Wireless)
				err = nic.Converge(step, spec)
			}
		}
		cancel()
		if sel.Usable() && err == nil {
			if said != "" {
				logf("network: %s is usable again", sel.Dev)
			}
			cfg = numbered.applySubstrate(spec.Bridge)
			cfg.net = &spec
			logf("network: the guest's L2 hangs off %s (%s)", spec.Parent, substrateName(spec))
			return cfg, nil
		}
		// One line per distinct problem, not per tick: a node waiting out a cable should not fill
		// its journal, and the line that matters is the one that changed. A device we selected but
		// could not build on has no Fix of its own (the selector is happy with it; the kernel
		// refused what we asked for), so say what failed, and add the override only when there is
		// something to say about the choice itself.
		why := sel.Fix()
		if err != nil {
			why = err.Error()
			if fix := sel.Fix(); fix != "" {
				why += " -- " + fix
			}
		}
		if why != said {
			logf("network: no usable device yet: %s", why)
			said = why
		}
		// Say so up-channel before sleeping on it, so the first tick of a degradation is the one
		// the fleet hears about rather than the second.
		if dg != nil {
			dg.waited = true
		}
		cfg.reportDegraded(ctx, dg, logf)
		if werr := cfg.waitTick(ctx, local, why, logf); werr != nil {
			return cfg, werr
		}
	}
}

// netSwap bounds a member swap: the leaving member's exit and the arriving one's join are each
// waited on for up to 30 s, the re-pin restarts the bridge's driver, and the swap then waits for
// the bridge to re-lease under the arriving member's MAC.
const netSwap = 2 * time.Minute

// swapBeforeLaunch swaps sel.SwapIn into the bridge the cold start chose. Swap first, then boot:
// a guest that boots after the swap sends its DHCP request through the new member, and every
// switch upstream learns its port at once; a running guest announces nothing after a swap.
// ⚠️ Never under a running guest: an agent whose guest is still up is no cold start, and the hot
// path keeps it off this one (linksPresent). This guards what slips past, a rig that owns the
// substrate itself: the guest keeps the member it has, and the live rule decides whether that
// member is gone.
func (cfg Config) swapBeforeLaunch(ctx context.Context, sel nic.Selection, logf func(string, ...any)) error {
	if platform.Running(ctx, cfg.guestSpec()) {
		logf("network: %s outranks %s's member, but the guest is running on it; it keeps the member it has",
			sel.SwapIn, sel.Dev)
		return nil
	}
	logf("network: %s outranks %s's member; swapping it in before the guest starts", sel.SwapIn, sel.Dev)
	sw, cancel := context.WithTimeout(ctx, netSwap)
	defer cancel()
	cfg.beat.Lease(sw)
	if err := cfg.swapMember(sw, sel.Dev, sel.SwapIn, logf); err != nil {
		return fmt.Errorf("swapping %s into %s: %w", sel.SwapIn, sel.Dev, err)
	}
	logf("network: %s is %s's member", sel.SwapIn, sel.Dev)
	return nil
}

// degraded is what the wait loop needs to keep talking while it waits. Without it, a node whose
// host network is fine but whose selected device cannot carry the guest's L2 is silent up-channel
// for as long as that lasts: the fleet sees nothing, and a managed node (the one tier where being
// told matters most, because the household cannot fix it themselves) cannot be told anything. A
// nil rep is the standalone node, which has nobody to tell; the whole thing is then a no-op.
type degraded struct {
	rep    cloud.CloudClient
	tenant string
	// pending carries refusals to the next report, exactly as the observe loop carries outcomes:
	// a directive answered but never acked is one the controller re-delivers forever.
	pending []api.DirectiveOutcome
	// waited records that this node actually spent time degraded, so the caller knows to
	// re-resolve its identity once a network exists. A node that took the hot path never was.
	waited bool
}

// reportDegraded sends this node's identity and refuses whatever comes back. The status is the
// identity half only (node, role, agent version, tenant, and what is installed from the
// node-local cache) with Healthy false and no Quorum: the shape snapshot already produces when it
// cannot reach the guest, and the honest one, since the node exists, is registered, and is not
// serving. ⚠️ It does not say why: the upward schema is a closed allowlist and widening it is a
// deliberate, surfaced act. The reason lives in the journal and behind the admin door, where it
// already is.
func (cfg Config) reportDegraded(ctx context.Context, dg *degraded, logf func(string, ...any)) {
	if dg == nil || dg.rep == nil {
		return
	}
	st := api.NodeStatus{
		NodeName: cfg.Node, Role: cfg.Role, AgentVersion: cfg.Version, Tenant: dg.tenant,
		// nil reader is safe here and only here: appStatuses touches it only when this node
		// is Primary, and a node with no guest is not.
		Apps: cfg.appStatuses(ctx, nil, false),
	}
	rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	directives, err := dg.rep.Report(rctx, api.ReportRequest{Status: st, Outcomes: dg.pending})
	if err != nil {
		// Transient by assumption, and on this path often not even that -- a node with no default
		// route has no cloud either. Keep waiting; the outcomes retry on the next tick.
		logf("report to controller failed while degraded: %v", err)
		return
	}
	dg.pending = nil
	// ⚠️ Every directive is refused, and refused terminally. Almost all of them need a guest, and
	// this node has none, but a directive that is merely ignored is re-delivered forever, which
	// turns a degraded node into a retry loop against a cloud that cannot help it. One refusal
	// path rather than a nil check per verb, for the same reason the admin door has one.
	for _, d := range directives {
		logf("refusing %s from the controller: this node has no usable network device", d.Kind)
		dg.pending = append(dg.pending, api.DirectiveOutcome{
			ID: d.ID, State: api.OutcomeFailed,
			Detail: "this machine has no usable network device for its VM, so it is not running one",
		})
	}
}

// waitTick is one pass of the degraded wait: answer whatever the admin door was handed, beat the
// watchdog, and sleep until the next tick. Beat rather than Lease, and the distinction is
// load-bearing (beat.go says why): a lease pings until its own context is done, and this wait has
// no deadline, so leasing across it would switch the watchdog off for as long as the node stayed
// degraded, exactly backwards.
func (cfg Config) waitTick(ctx context.Context, local <-chan localRequest, why string, logf func(string, ...any)) error {
	cfg.beat.Beat()
	t := time.NewTimer(netTick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			return nil
		case req := <-local:
			// A stop is the one directive a node with no network still takes: going down needs none.
			if req.d.Kind == api.DirectiveStop {
				req.resp <- cfg.applyStop(req.d, logf)
				continue
			}
			// Refused immediately, with the reason: a channel with no reader blocks the CLI until
			// the agent exits, which to the operator is indistinguishable from a wedged agent.
			logf("admin socket: refusing %s -- the node has no usable network device", req.d.Kind)
			req.resp <- api.DirectiveOutcome{
				ID: req.d.ID, State: api.OutcomeFailed,
				Detail: "this machine has no usable network device: " + why,
			}
		}
	}
}

// convergeNetwork re-asserts the host's side of the guest's L2 at each network check
// (netCheckTicks). Check-first, so the overwhelmingly common answer, "already right", reads a few
// sysfs files and stops. It exists because the host's devices are not ours alone to keep: under
// the bridge substrate our system-subnet address sits on a device NetworkManager manages, and NM
// reconciles addresses when a connection reactivates (an `nmcli con up`, a carrier bounce or an NM
// restart can flush ours), so we converge rather than being careful once. It does not re-select:
// re-running the selector every check would macvtap-probe the parent forever, and re-parenting is
// a different act with its own trigger and its own cost (it restarts the guest), in reparent.go.
func (cfg Config) convergeNetwork(ctx context.Context, logf func(string, ...any)) {
	if cfg.net == nil || cfg.net.Parent == "" || nic.Converged(*cfg.net) {
		return
	}
	step, cancel := context.WithTimeout(ctx, netTick)
	defer cancel()
	if err := nic.Converge(step, *cfg.net); err != nil {
		logf("network: could not converge the guest's L2 on %s: %v", cfg.net.Parent, err)
		return
	}
	logf("network: re-converged the guest's L2 on %s", cfg.net.Parent)
}

func substrateName(s nic.Spec) string {
	switch {
	case s.Bridge:
		return bridgeWhose + " -- one port, the guest makes its own service identity"
	case s.Ipvtap:
		return "ipvtap children on the station's own MAC, held in " + nic.HoldNS + " -- wireless, so no failover"
	}
	return "macvtap children, no bridge and no host-IP move"
}
