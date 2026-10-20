package host

import (
	"context"
	"fmt"
	"net/netip"
	"time"

	"briard.io/agent/cloud"
	"briard.io/agent/nic"
	"briard.io/agent/platform"
	"briard.io/shared/api"
)

// THE AGENT OWNS THE NETWORK NOW.
//
// It used to be `net-up.sh`: a script the installer generated with this host's NIC, device names,
// addresses and gateway baked into a heredoc, run inline once and again at every boot from
// `briard-net.service`, which the agent unit `Requires=`d. Three things came with that and all
// three are gone.
//
// A FROZEN FILE NO RELEASE COULD REACH. Every bug in those lines shipped a fix that landed on no
// installed node (accept_ra, ALLMULTI's expiring-cache trap).
//
// A ONESHOT THAT COULD NOT ADAPT. A macvtap cannot be re-parented — you delete and recreate,
// invalidating the fd handed to qemu, which means relaunching the guest. Only the thing that runs
// the guest can sequence that, which is why this could not stay a separate unit even in principle.
//
// AND A `Requires=` THAT TOOK THE NODE DARK. A failed network unit meant the agent never started:
// nothing left to report, retry, or answer the admin door. A cable out at boot was a node you
// could not diagnose. Now the agent starts, says what is wrong, and keeps converging.

// netTick is how often the wait loop re-asks while the node has no usable device. It is the
// status tick's own cadence rather than a number of its own -- a node waiting for a cable is not
// a node that should be busy.
const netTick = 10 * time.Second

// netSettle bounds how long a cold start holds its choice for a NIC whose lease is still in
// flight. Thirty seconds covers a home router's DHCP at boot with room to spare, and is what a
// NIC that never gets a gateway costs each cold start.
const netSettle = 30 * time.Second

// linksPresent reports whether every device this node's config names is already built. It is the
// hot path's whole question: an agent that finds its devices there has nothing to build, whether
// they were built by its own previous life or by a rig that owns the substrate itself. Callers
// have already established that the config names devices at all. Asked of the device's
// construction, not its media (nic.Built): a cable out under a running guest is no cold start.
//
// A device not in this namespace is asked of nic.HoldNS, where an ipvtap node's children live:
// they are as present as a macvtap's, and an agent restarting under its Wi-Fi guest must keep the
// parent they stand on rather than re-select (awaitNetwork).
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

// netSpec derives the host side of the guest's L2 from the selected device. It is DERIVED and not
// configured: the substrate is the answer to "is the parent a bridge, or a wireless station", not
// a mode somebody typed, because the thing that decides it is the machine rather than the
// operator. A wireless station is macvtap's spec with ipvtap children (nic/ipvtap.go).
func (cfg Config) netSpec(dev string, bridge, wireless bool) nic.Spec {
	// ⚠️ NARROWED FIRST, and here rather than at the caller so the two cannot disagree. The spec
	// must describe the node the substrate actually makes: on a bridge there is no service tap and
	// no private link, and reading the config as WRITTEN would name devices the guest cannot use.
	cfg = cfg.applySubstrate(bridge)
	s := nic.Spec{
		Parent: dev, Bridge: bridge, Ipvtap: wireless && !bridge,
		SystemTap: cfg.SystemTap, ServiceTap: cfg.ServiceTap, PrivTap: cfg.WitnessTap,
	}
	if bridge {
		// ONE PORT ON THE BRIDGE, and the host's own system-subnet address on the bridge itself:
		// a /32, plus ONE on-link route to its guest's node IP. The host reaches the one machine
		// on that subnet it has business with and claims nothing else of the range -- a /24 here
		// would put a second connected route for the whole system subnet on the host's PRIMARY
		// device, beside its real LAN address. What a /24 added was only that implicit route;
		// ARP is unaffected, because the guest's eth1 is on this same L2 and the request arrives
		// on the interface holding the address.
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

// applySubstrate narrows the config to what the derived substrate actually has. config.env carries
// the MACVTAP shape -- the one every Linux node ships in -- and this is the one transformation
// that makes a bridge parent's config true.
//
// ⚠️ It overrides what config said, which is the one place in the agent that does. The rule
// everywhere else is that the environment wins, and it still does for the values
// nothing here touches; but a node whose parent is a bridge HAS no second tap and no private
// link, and honouring a configured name for a device that cannot exist would render a NIC the
// guest then cannot use. The substrate is a fact about the machine, not a preference.
func (cfg Config) applySubstrate(bridge bool) Config {
	if !bridge {
		if cfg.NetMode == "" {
			cfg.NetMode = platform.NetMacvtap
		}
		return cfg
	}
	// The Linux clone of the Windows shape: ONE tap, so the guest gets one kernel NIC
	// and MAKES its service identity as a macvlan child of it. No second tap, so no eth2 from us;
	// no third, so no eth3 -- host and guest already share one L2 here, and the private link
	// would be a second tap Windows cannot give us anyway.
	cfg.ServiceTap, cfg.WitnessTap, cfg.WitnessCIDR = "", "", ""
	cfg.VIPParent = "eth1"
	cfg.NetMode = platform.NetBridge
	return cfg
}

// awaitNetwork brings the host's side of the guest's L2 up, waiting for a usable device if there
// is none yet, and returns the config the derived substrate implies.
//
// ⚠️ IT RUNS AFTER READY, NOT IN THE READY PATH. The unit is Type=notify + WatchdogSec=20 +
// Restart=on-failure with StartLimitIntervalSec=0, so network work before READY turns a transient
// fault into a crashloop that never stops. READY already means "the agent started", not "the node
// is healthy" -- this is exactly the kind of work that distinction exists for.
//
// THE HOT PATH COSTS NOTHING. A device that is there and already converged returns on the first
// pass, so an ordinary boot starts as fast as it did when a oneshot did this.
//
// THE COLD PATH WAITS RATHER THAN FAILING. No default route yet and no default route at all are
// the same reading (nic's sentinels say so), and only elapsed time tells them apart -- so the
// agent waits, and while it waits it ANSWERS: the admin door is already listening, and every
// directive submitted gets the same refusal the report card would have given, naming the device
// it wanted and the override that fixes it. That is the whole difference from `Requires=`, which
// left a node with nothing running to ask.
func (cfg Config) awaitNetwork(ctx context.Context, local <-chan localRequest, dg *degraded, logf func(string, ...any)) (Config, error) {
	// THIS AGENT WAS NOT GIVEN A NETWORK TO BUILD. No device names means no guest NICs to make,
	// which is every agent-less harness and every unit test that constructs a Config by hand --
	// and it has to be answered BEFORE the wait below, because the wait's exit condition is a
	// successful Converge and Converge on an empty spec can never succeed. Without this the loop
	// is unbounded on exactly the configurations that have nothing for it to do.
	if cfg.SystemTap == "" {
		return cfg, nil
	}
	// THE HOT PATH, and it is a check rather than a cache: the devices this node's config names
	// are already up, so there is nothing to create and therefore nothing to validate. An agent
	// restart (every self-update is one) takes it, and so does every rig and lab node that builds
	// its own devices -- which is why it must not probe. Probing here would be a netlink write to
	// answer a question nobody asked, and on a substrate where the probe cannot succeed (a
	// container's veth) it would send a perfectly healthy node into the degraded wait below.
	//
	// It READS THE PARENT OFF THE DEVICE rather than selecting, so the tick has a parent to
	// re-assert against and the guest keeps the one it stands on. Re-selecting here would turn a
	// self-update into the household's service moving networks, and an upgrade's rollback would
	// then compare health across two changes rather than one; the parent moves only at a cold
	// start (the loop below) or when it is gone (reparent.go). BRIARD_NIC does not move it either:
	// the live rule takes it as the answer once the standing parent is gone. A device stacked on
	// nothing (a rig's own tap) has no parent to read, and gets the selection. One that comes up
	// empty is not a failure: the devices are up, and an agent that did not build them has no
	// business insisting on which parent they hang off.
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
		// NUMBERED BEFORE THE SPEC IS BUILT, because the spec is made of this node's addresses
		// (subnets.go). It does not violate the no-probing rule above: a node that has drawn
		// before -- every restart, every self-update -- reads its record and touches nothing, and
		// a rig states its own address, which is the guard that stops a draw dead. What is left is
		// a product node whose record is gone, where probing is exactly right. A draw that REFUSES
		// falls through to the wait below rather than building a substrate with no addresses in it.
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
	// THE COLD START, the one moment the parent is chosen afresh. The parent last served on is
	// preferred on a tie, so a restart that finds two equal uplinks stays where it was.
	var said, held string // the last degradation said, and the last settle (not a degradation)
	settled := time.Now().Add(netSettle)
	prefer := cfg.recordedNetwork().Parent
	for {
		// HOLD THE CHOICE while a NIC has a link and no default route yet -- DHCP still in flight
		// at boot -- so a boot picks neither the radio that associated first nor nothing. Bounded,
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
		// One line per DISTINCT problem, not one per tick: a node waiting out a cable is not a
		// node that should fill its journal, and the line that matters is the one that changed.
		// A device we SELECTED but could not build on has no Fix of its own -- the selector is
		// happy with it; the kernel refused the thing we asked for. Say what failed, and add the
		// override only when there is something to say about the choice itself.
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
		// Say so up-channel BEFORE sleeping on it, so the first tick of a degradation is the one
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
// waited on for up to 30 s, and the re-pin restarts the bridge's driver.
const netSwap = 2 * time.Minute

// swapBeforeLaunch swaps sel.SwapIn into the bridge the cold start chose. SWAP FIRST, THEN BOOT:
// a guest that boots after the swap sends its DHCP request through the new member, and every
// switch upstream learns its port at once; a running guest announces nothing after a swap.
//
// ⚠️ NEVER UNDER A RUNNING GUEST. An agent whose guest is still up is no cold start, and the
// hot path keeps it off this one (linksPresent); this is the guard for what slips past it, a
// rig that owns the substrate itself: the guest keeps the member it has, and the live rule
// decides whether that member is gone.
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
	// AND ITS LEASE BEFORE THE BOOT: the bridge re-leases under the arriving member's MAC, and
	// until it has, everything read off it -- the LAN fingerprint the bring-up compares, the
	// address the guest is reached by -- is a reading of an adapter mid-DHCP. Bounded by the
	// swap's budget; a lease that never comes is the guest's to wait out, as at any boot. Asked of
	// the fingerprint, not the route table: Windows keeps the bridge's default route through the
	// re-lease, while its address is the link-local placeholder and its gateway does not answer.
	for sw.Err() == nil {
		fp := nic.Read(sw, sel.Dev)
		if p, err := netip.ParsePrefix(fp.HostCIDR); err == nil && !p.Addr().IsLinkLocalUnicast() && fp.GatewayMAC != "" {
			break
		}
		select {
		case <-sw.Done():
		case <-time.After(time.Second):
		}
	}
	logf("network: %s is %s's member", sel.SwapIn, sel.Dev)
	return nil
}

// degraded is what the wait loop needs to keep TALKING while it waits.
//
// A node whose host network is fine but whose selected device cannot carry the guest's L2 used to
// go silent up-channel for as long as it stayed that way: the fleet saw nothing, and a managed
// node could not be told anything. That is the one tier where being told matters most, because
// the household cannot fix it themselves.
//
// nil rep is the standalone node, which has nobody to tell; the whole thing is then a no-op.
type degraded struct {
	rep    cloud.CloudClient
	tenant string
	// pending carries refusals to the NEXT report, exactly as the observe loop carries outcomes:
	// a directive answered but never acked is one the controller re-delivers forever.
	pending []api.DirectiveOutcome
	// waited records that this node actually spent time degraded, so the caller knows to
	// re-resolve its identity once a network exists. A node that took the hot path never was.
	waited bool
}

// reportDegraded sends this node's identity and refuses whatever comes back.
//
// THE STATUS IS THE IDENTITY HALF ONLY -- node, role, agent version, tenant, and what is
// installed from the node-local cache -- with Healthy false and no Quorum. That is the same
// shape snapshot already produces when it cannot reach the guest, and it is the honest one: the
// node exists, it is registered, it is not serving. ⚠️ It does NOT say WHY, because the upward
// schema is a closed allowlist and widening it is a deliberate, surfaced act --
// the reason lives in the journal and behind the admin door, where it already is.
func (cfg Config) reportDegraded(ctx context.Context, dg *degraded, logf func(string, ...any)) {
	if dg == nil || dg.rep == nil {
		return
	}
	st := api.NodeStatus{
		NodeName: cfg.Node, Role: cfg.Role, AgentVersion: cfg.Version, Tenant: dg.tenant,
		// nil reader is safe here and only here: serviceStatuses touches it only when this node
		// is Primary, and a node with no guest is not.
		Services: cfg.serviceStatuses(ctx, nil, false),
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
	// ⚠️ EVERY DIRECTIVE IS REFUSED, AND REFUSED TERMINALLY. Almost all of them need a guest, and
	// this node has none -- but a directive that is merely ignored is re-delivered forever, which
	// turns a degraded node into a retry loop against a cloud that cannot help it. One refusal
	// path rather than a nil check per verb, for the same reason the admin door has one.
	for _, d := range directives {
		logf("refusing %s from the controller: this node has no usable network device", d.Kind)
		dg.pending = append(dg.pending, api.DirectiveOutcome{
			ID: d.ID, State: api.OutcomeFailed,
			Detail: "this node has no usable network device for its guest, so it is not running one",
		})
	}
}

// waitTick is one pass of the degraded wait: answer whatever the admin door was handed, beat the
// watchdog, and sleep until the next tick.
//
// Beat rather than Lease, and the distinction is load-bearing (beat.go says why): a lease pings
// until ITS context is done, and this wait has no deadline, so leasing across it would switch the
// watchdog off for as long as the node stayed degraded -- exactly backwards.
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
			// REFUSED, IMMEDIATELY, WITH THE REASON. The alternative is what the channel does
			// with no reader -- the CLI blocks until the agent exits, which is indistinguishable
			// to the operator from the agent being wedged. The one moment they most need an
			// answer is the one this used to have none for.
			logf("admin socket: refusing %s -- the node has no usable network device", req.d.Kind)
			req.resp <- api.DirectiveOutcome{
				ID: req.d.ID, State: api.OutcomeFailed,
				Detail: "this node has no usable network device: " + why,
			}
		}
	}
}

// convergeNetwork re-asserts the host's side of the guest's L2 on the status tick. Check-first, so
// the overwhelmingly common answer is "already right" and it reads a few sysfs files and stops.
//
// It exists because the host's devices are not ours alone to keep: under the bridge substrate our
// system-subnet address sits on a device NetworkManager manages, and NM reconciles addresses when
// a connection reactivates -- an `nmcli con up`, a carrier bounce or an NM restart can flush ours.
// Converging is the answer to that rather than being careful once.
//
// It does NOT re-select. Re-running the selector every tick would macvtap-probe the parent every
// ten seconds forever, and re-parenting is a different act with its own trigger and its own cost
// (it restarts the guest) -- that is re-parenting (reparent.go), not this.
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
