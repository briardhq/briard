package host

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"time"

	"briard.io/agent/nic"
	"briard.io/agent/subnet"
	"briard.io/shared/atomicfile"
	"briard.io/shared/notify"
)

// Re-parenting: what happens when the device the guest's L2 hangs off goes away. ⚠️ qemu will not
// tell you: the macvtap child vanishes with its parent and qemu keeps running with a dead NIC, the
// guest alive, healthy by its own account, and unreachable from the household. So the detector
// has to be the observe loop's network check, not the guest's exit: the guest does not exit.
//
// ⚠️ The trigger is the parent being gone (absent, or present without carrier) and another
// candidate named now. Gone alone moves nothing: a cold boot with the cable out, or a pulled cable
// on a one-NIC machine, has no successor and only waits. Never our own device's disappearance
// alone (from our side indistinguishable from the parent going away), and never a cable coming
// back: the parent returning cancels, and a better device appearing beside a working one waits
// for the next cold start (network.go).
//
// The move is expensive in a way nothing else on the tick is: a macvtap cannot be re-parented, so
// the devices must be deleted and recreated, which invalidates the fds qemu holds, which means
// stopping and relaunching the guest: a healthy household loses its apps for the length of a
// boot. That is why the pacing below exists at all.

// The tiers, and what each is paying for. ⚠️ They are a starting point, meant to be revised, and
// the trade is stated so a later reader can judge them rather than guess: too short and an
// ordinary blip (a switch rebooting, a laptop dock re-enumerating, a driver reloading on a kernel
// upgrade) costs the household its apps for a boot; too long and a node whose NIC really was
// replaced stays dark for no reason.
const (
	// reparentSameWire: the gateway MAC matches, so this is the same router on the same segment
	// and the wire changed under us. The only unambiguous case, and the only one where waiting
	// longer buys nothing -- five minutes is "long enough that nothing transient is still in
	// flight", not a hedge about where we are.
	reparentSameWire = 5 * time.Minute
	// reparentSameSubnet: the addressing matches but the gateway MAC does not. Genuinely
	// ambiguous in both directions -- a replaced router renumbering this house looks exactly like
	// 192.168.1.0/24 at a different one -- so the wait is long enough that a human who moved the
	// box has time to notice and stop us, and short enough that a real router swap heals itself
	// overnight.
	reparentSameSubnet = 30 * time.Minute
)

// ⚠️ There is no "different subnet" duration, by decision rather than omission. A machine on a
// different subnet has moved, and a paired node that rebuilds itself on a LAN its peer is not on
// is a split flock, the one outcome this whole mechanism exists to avoid. So it is not a longer
// wait but an explicit operator action, `NIC` in config.env, already the escape hatch for every
// other way our selection can be wrong; any other way out belongs on the directive path, never on
// a timer here. Unknown (too little evidence to say) takes the same answer, because "we could not
// tell" must never be cheaper than "we could tell, and it was elsewhere".

// networkRecordName is the node-local record of the LAN this node last brought its guest up on.
// Pet state, beside the other node-local caches: it has to survive a reboot (the whole point is
// comparing today's LAN against the one before the power cut) and a cattle wipe of /opt.
const networkRecordName = "network.json"

// networkRecord is what is written. A struct rather than the bare fingerprint so the file can
// grow a field without becoming ambiguous, and so a reader can see when it was taken.
type networkRecord struct {
	Fingerprint nic.Fingerprint `json:"fingerprint"`
	At          time.Time       `json:"at"`
}

// networkRecordPath puts the record beside the assignment and mesh caches: ASSIGNMENT_CACHE's
// directory is the node's pet state dir, and deriving from it rather than a second config key
// keeps a harness that points ASSIGNMENT_CACHE elsewhere working without being told twice.
func (cfg Config) networkRecordPath() string {
	if cfg.AssignmentCache == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(cfg.AssignmentCache), networkRecordName)
}

// recordNetwork writes the fingerprint of the LAN this node just brought its guest up on. Durable
// (tmp + fsync + rename, the canonical shape): it is a fact whose only copy is this file, and
// losing it to a power cut turns the next re-parent decision into Unknown, which refuses: safe,
// but it strands a node that could have healed itself.
func (cfg Config) recordNetwork(ctx context.Context, logf func(string, ...any)) {
	path := cfg.networkRecordPath()
	// nil is the ordinary state on every agent that owns no network -- every rig, every lab node,
	// every unit test -- and Run calls this unconditionally after bring-up. There is nothing to
	// record about a LAN we did not choose, and a fingerprint of somebody else's devices would be
	// worse than none: it would pace a re-parent we have no business performing.
	if path == "" || cfg.net == nil || cfg.net.Parent == "" {
		return
	}
	fp := nic.Read(ctx, cfg.net.Parent)
	// ⚠️ An unusable reading is not recorded: overwriting a good record with one is strictly
	// worse than not writing at all, since Compare answers Unknown for a fingerprint with no
	// address and Unknown refuses, so the very act meant to help strands, permanently, a node
	// that could have healed itself. It is a real race: the parent is chosen at start-up and
	// recorded after bring-up, seconds later, and a NIC gone in between (unplugged while the guest
	// boots, or renamed by a udev rule that lost a race) reads as `[eth1 mac=? addr=? gw=?/?]`,
	// which, recorded, refuses every re-parent for the rest of the node's life.
	if !fp.Usable() {
		logf("network: not recording this LAN -- %s could not be read [%s]; keeping the previous record",
			cfg.net.Parent, fp)
		return
	}
	cfg.writeNetworkRecord(learnRecord(cfg.recordedNetwork(), fp), logf)
}

// learnRecord is what a new reading of the same LAN may take from the record it replaces: the
// gateway MAC, when the reading has none and the record has one. The MAC is a property of the LAN,
// not of the moment, and a bring-up with the cable out or a gateway slow to answer ARP reads none
// -- a reading that forgets it would send the next move to the slower tier for a LAN that has not
// changed. The same LAN means the same parent, address and gateway; anything else is a new record.
func learnRecord(rec, fp nic.Fingerprint) nic.Fingerprint {
	if fp.GatewayMAC == "" && rec.GatewayMAC != "" && sameLAN(rec, fp) {
		fp.GatewayMAC = rec.GatewayMAC
	}
	return fp
}

// completesRecord says whether a reading taken while serving fills in what the record lacks: a
// gateway MAC the record has none of, on the LAN the record describes -- or any usable LAN at all
// when there is no record, since the node is serving on it now.
func completesRecord(rec, fp nic.Fingerprint) bool {
	if rec.GatewayMAC != "" || !fp.Usable() || fp.GatewayMAC == "" {
		return false
	}
	return rec.HostCIDR == "" || sameLAN(rec, fp)
}

func sameLAN(a, b nic.Fingerprint) bool {
	return a.Parent == b.Parent && a.HostCIDR == b.HostCIDR && a.GatewayIP == b.GatewayIP
}

// completeNetworkRecord runs on the observe tick: while the record has no gateway MAC, a reading
// that has one on the same LAN completes it. The MAC is captured whenever the gateway first
// answers, not only at bring-up, so a LAN recorded without it (the cable out, a router slower
// than this machine) paces the next move on the strong signal as soon as there is one. The ARP
// it provokes costs one frame per tick, and only while the MAC is missing.
func (cfg Config) completeNetworkRecord(ctx context.Context, logf func(string, ...any)) {
	if cfg.networkRecordPath() == "" || cfg.net == nil || cfg.net.Parent == "" {
		return
	}
	rec := cfg.recordedNetwork()
	if rec.GatewayMAC != "" {
		return
	}
	if fp := nic.Read(ctx, cfg.net.Parent); completesRecord(rec, fp) {
		cfg.writeNetworkRecord(fp, logf)
	}
}

// writeNetworkRecord is the one writer: durable, and it logs the whole fingerprint.
func (cfg Config) writeNetworkRecord(fp nic.Fingerprint, logf func(string, ...any)) {
	path := cfg.networkRecordPath()
	rec := networkRecord{Fingerprint: fp, At: time.Now().UTC()}
	b, err := json.Marshal(rec)
	if err != nil {
		logf("network: could not render the LAN record: %v", err)
		return
	}
	if err := atomicfile.Write(path, b, 0o600, 0o700); err != nil {
		logf("network: could not record the LAN at %s: %v", path, err)
		return
	}
	// The whole fingerprint, not a friendly summary of it. What a later re-parent turns on is
	// which fields were readable, so a line that omits the unreadable ones is a line that cannot
	// explain the decision it is the input to.
	logf("network: recorded this LAN [%s]", rec.Fingerprint)
}

// recordedNetwork reads it back. A missing or unreadable record is the zero fingerprint, which
// Compare answers Unknown for -- the cautious direction.
func (cfg Config) recordedNetwork() nic.Fingerprint {
	path := cfg.networkRecordPath()
	if path == "" {
		return nic.Fingerprint{}
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nic.Fingerprint{}
	}
	var rec networkRecord
	if err := json.Unmarshal(b, &rec); err != nil {
		return nic.Fingerprint{}
	}
	return rec.Fingerprint
}

// vipRouter is the router the guest sends replies to other subnets through while it holds the
// VIP: this host's own gateway on the LAN the guest's service NIC hangs off, which is the VIP's
// LAN. Read now, else the recorded one: a bring-up after a power cut can beat the router back,
// and an empty read is no evidence that the LAN changed. "" = no LAN here (an agent-less or
// witness node), and the guest routes nothing.
func (cfg Config) vipRouter(ctx context.Context) string {
	if cfg.net == nil || cfg.net.Parent == "" {
		return ""
	}
	if gw := nic.Read(ctx, cfg.net.Parent).GatewayIP; gw != "" {
		return gw
	}
	return cfg.recordedNetwork().GatewayIP
}

// reparentWait is the tier: how long to let a candidate stand before rebuilding on it. The second
// return is false when no wait is long enough (the machine has moved, or we cannot tell): the
// answer is an operator rather than a timer.
//
// fixture collapses every tier to a fixed wait; it is a test fixture, not a product knob
// (Config.ReparentTier says so). It exists because a rig would otherwise spend the shipped fast
// tier's five minutes of real wall-clock observing a decision the first second already made.
// ⚠️ It shrinks durations and never converts a refusal into one: `Elsewhere` and `Unknown` are
// not long waits but "an operator decides", so a fixture that reached them would let a rig prove
// a re-parent the product forbids, which is the worst thing a test knob can do.
func reparentWait(rel nic.Relation, fixture time.Duration) (time.Duration, bool) {
	var d time.Duration
	switch rel {
	case nic.SameWire:
		d = reparentSameWire
	case nic.SameSubnet:
		d = reparentSameSubnet
	default: // Elsewhere, Unknown
		return 0, false
	}
	if fixture > 0 && fixture < d {
		return fixture, true
	}
	return d, true
}

// reparenter holds the one piece of state the decision needs across ticks: since when the
// recorded parent has been gone. It lives for the observe loop, like the VIP router and the
// recovery counter, because "how long has this been true" is not answerable from a single tick.
type reparenter struct {
	goneSince time.Time // zero = the parent was present last time we looked
	said      string    // the last thing said about it, so the journal gets one line per change
}

// consider is the tick's whole re-parent decision. It returns the device to rebuild on, or "".
// ⚠️ It never tears down before a new answer exists: a parent gone with nowhere to go is a node
// that waits, holding the guest it still has; the guest is unreachable either way, and stopping
// it would only take away the one thing an operator could still look at. ⚠️ The recorded device
// returning outranks every tier: a NIC back in three seconds must not cost a guest restart, so the
// clock resets the instant the parent is there with carrier again and nothing else is even
// considered. Hence no separate "minimum settle" constant: the shortest tier is five minutes, and
// any return inside it cancels.
func (r *reparenter) consider(ctx context.Context, cfg Config, now time.Time, logf func(string, ...any)) (string, nic.Relation) {
	if cfg.net == nil || cfg.net.Parent == "" {
		return "", nic.Unknown // this agent does not own the network
	}
	// The uplink is what can be gone: the parent itself, or the physical member of briard's
	// Windows bridge, whose own media state is the OR with the TAP and evidence of nothing. A
	// bridge that does not move by a member swap is the user's (Linux): a bridge that disappears
	// is theirs to restore, since we never created it and must not start now.
	parent := cfg.net.Parent
	up, swaps := nic.Uplink(parent)
	if cfg.net.Bridge && !swaps {
		return "", nic.Unknown
	}
	lost := up
	if lost == "" {
		lost = parent + "'s NIC"
	}
	// Gone is absent, without carrier, or (a swapping bridge) no member but ours: a pulled cable
	// leaves the device there, up and carrying nothing. None of them alone moves anything: the
	// move needs a successor in hand (below), so a cold boot with the cable out, or a one-NIC
	// desktop with its cable pulled, only waits.
	if up != "" && nic.Carrier(up) { // a device that is not there has no carrier, on either OS
		if !r.goneSince.IsZero() {
			logf("network: %s is back; nothing to re-parent", up)
			r.goneSince, r.said = time.Time{}, ""
		}
		return "", nic.Unknown
	}
	if r.goneSince.IsZero() {
		r.goneSince = now
		switch {
		case up == "":
			logf("network: %s holds no NIC but ours; the guest is running with a dead NIC", parent)
		case nic.Exists(up):
			logf("network: %s has lost its link; the guest is running with a dead NIC", up)
		default:
			logf("network: %s has gone away; the guest is running with a dead NIC", up)
		}
	}
	// Re-select, without probing yet: there is no point pacing a move onto a device that does not
	// exist. The selection reads carrier too, so the gone uplink is not a candidate, except as
	// BRIARD_NIC or in a race with its return, and either way staying put is the answer. A
	// swapping bridge answers as itself, with the adapter that outranks its member as SwapIn.
	// Nothing usable is the waiting case, and it waits quietly: the loss is logged above, and the
	// selection's remedy (name a device with BRIARD_NIC) is install-time advice that misleads here,
	// where the usual cause is a link that comes back by itself (a sleep, a radio re-associating).
	sel := nic.Select(cfg.NICOverride, parent)
	if !sel.Usable() {
		return "", nic.Unknown
	}
	cand := sel.Dev
	if sel.SwapIn != "" {
		cand = sel.SwapIn
	}
	if cand == parent || cand == up {
		return "", nic.Unknown
	}
	was, seen := cfg.recordedNetwork(), nic.Read(ctx, cand)
	// BRIARD_NIC naming another device is the operator's answer, and it is what the refusals below
	// tell them to set: no tier and no fingerprint stand between them and the move they named.
	if sel.Override {
		return cand, nic.Compare(was, seen)
	}
	// No record at all is a different problem from "a different network", worth its own branch:
	// the remedy is the same (an operator names the device) but the cause is not, and a reader
	// told "not demonstrably the same network" about a node that simply has nothing to compare
	// against will go looking in the wrong place. It happens on a node whose pet state was lost.
	if was == (nic.Fingerprint{}) {
		r.say(logf, fmt.Sprintf(
			"network: %s is gone and there is no record of the LAN this node last served on, so "+
				"nothing can say whether %s is the same one -- NOT re-parenting on my own. Set "+
				"NIC=%s in %s and restart briard-agent if this move is intended",
			lost, cand, cand, cfg.configPathForMessage()))
		return "", nic.Unknown
	}
	rel := nic.Compare(was, seen)
	wait, ok := reparentWait(rel, cfg.ReparentTier)
	if !ok {
		// The machine has moved, or we cannot tell. Never automatic: a paired node that rebuilds
		// itself on a LAN its peer is not on is a split flock. ⚠️ The message carries both
		// fingerprints, and not as decoration: `unknown` means a field could not be read on one
		// side or the other, and which side it was is the entire diagnosis; without it the reader
		// (or the next engineer) can only guess.
		r.say(logf, fmt.Sprintf(
			"network: %s is gone and %s is not demonstrably the same network (%s) -- NOT re-parenting "+
				"on my own. Recorded: [%s]. Now: [%s]. Set NIC=%s in %s and restart "+
				"briard-agent if this move is intended",
			lost, cand, rel, was, seen, cand, cfg.configPathForMessage()))
		return "", rel
	}
	if left := wait - now.Sub(r.goneSince); left > 0 {
		r.say(logf, fmt.Sprintf("network: %s is gone; %s is %s, re-parenting in %s unless it comes back",
			lost, cand, rel, left.Round(time.Second)))
		return "", rel
	}
	return cand, rel
}

// reparent moves the guest's L2 onto dev: rebuild the devices there, restart the guest so it
// opens the new chardevs, record the LAN we landed on, and tell the household. ⚠️ The guest
// restart is not optional, and it is the expensive part: qemu holds an fd per macvtap, a macvtap
// cannot be re-parented, so the devices are new devices and only a relaunch makes qemu open them.
// `RescueGuest` is the canonical stop-and-relaunch, the same VM+channel+Manager swap the upgrade
// legs do; a second owner of that swap would be a second way to do it (the upgrader interface
// says so).
//
// ⚠️ On Linux nothing here touches the host's own addressing or routes, which keeps the agent's
// path to the cloud intact across a re-parent, by construction rather than care: we never move a
// host address anywhere, so the only devices in play are ours. A Windows member swap is the
// exception, and cannot help it: the bridge is the host's own adapter, and it re-leases under the
// arriving member's MAC.
func (cfg Config) reparent(ctx context.Context, mgr upgrader, n notify.Notifier, dev string, rel nic.Relation, logf func(string, ...any)) error {
	from := cfg.net.Parent
	up, swaps := nic.Uplink(from)
	if swaps && up != "" {
		from = up // the alert names the member that went, not the bridge that stays
	}
	logf("network: re-parenting the guest's L2 from %s onto %s (%s)", from, dev, rel)
	// One budget for the whole move, and the watchdog rides it. The lease must cover a bounded
	// context (beat.go): the stop leg inside RescueGuest spends a full shutdownGrace and
	// `vm.Stop()` takes no context at all, so nothing else is watching that stretch; leasing the
	// caller's root context instead would ping forever and switch the watchdog off permanently,
	// the exact failure a lease is shaped to avoid.
	budget := netTick + cfg.BringUpBudget + 3*shutdownGrace
	if swaps {
		budget += netSwap
	}
	mv, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	cfg.beat.Lease(mv)
	if swaps {
		// The swap, then the restart: the guest boots behind the new member, so its own DHCP
		// teaches every switch upstream the new port (swapBeforeLaunch). The spec is the bridge's
		// and stays as it is.
		if err := cfg.swapMember(mv, cfg.net.Parent, dev, logf); err != nil {
			return fmt.Errorf("re-parent onto %s: %w", dev, err)
		}
	} else if err := cfg.rebuildOn(mv, dev); err != nil {
		return err
	}
	if mgr == nil {
		// No guest binding: the rebuild above is the whole move. Run always passes one, so a
		// shipped node takes the restart below.
		cfg.recordNetwork(ctx, logf)
		alertReparent(ctx, n, logf, from, dev, rel)
		return nil
	}
	if err := mgr.RescueGuest(mv); err != nil {
		return fmt.Errorf("re-parent onto %s: restart the guest: %w", dev, err)
	}
	// Recorded after the guest is back, not before: the record is what a future re-parent paces
	// itself against, and recording a LAN we had not actually served on would teach the next
	// decision something we never verified.
	cfg.recordNetwork(ctx, logf)
	alertReparent(ctx, n, logf, from, dev, rel)
	return nil
}

// rebuildOn is the Linux re-parent's act: the guest's devices rebuilt on dev, and cfg.net with
// them. Rebuild before the restart: the children went away with their parent, so there are no
// live fds to protect, and the guest comes back into a network already right rather than one
// being assembled underneath it. The substrate is re-derived with the parent: a move between
// Wi-Fi and wire changes it (ipvtap <-> macvtap), and with it the guest's NICs: their MACs and
// how qemu is handed them. guestSpec reads both off cfg.net, which this updates, so the restart
// that follows launches the guest the new substrate needs. The tiers decide when; this is only
// the what.
func (cfg Config) rebuildOn(ctx context.Context, dev string) error {
	spec := cfg.netSpec(dev, nic.IsBridge(dev), nic.Wireless(dev))
	rb, cancel := context.WithTimeout(ctx, netTick)
	defer cancel()
	if err := nic.Rebuild(rb, spec); err != nil {
		return fmt.Errorf("re-parent onto %s: %w", dev, err)
	}
	*cfg.net = spec
	return nil
}

// say logs once per distinct message. A node waiting out a tier is a node that must not fill its
// journal with the same line at every check for half an hour.
func (r *reparenter) say(logf func(string, ...any), msg string) {
	if msg != r.said {
		logf("%s", msg)
		r.said = msg
	}
}

// configPathForMessage names the file an operator would edit: the message must be actionable, and
// "set NIC" is not without saying where. (The key is NIC in config.env; BRIARD_NIC is its name at
// install time, where the report card reads it, by the one rule that every config key K is
// settable as BRIARD_K when the installer runs.)
func (cfg Config) configPathForMessage() string {
	if p := os.Getenv("BRIARD_CONFIG"); p != "" {
		return p
	}
	return defaultConfigFile
}

// checkMovedLAN is the one thing a re-parent cannot fix and must therefore say out loud: this
// node's system subnet was drawn against the collision landscape of a different network, and on a
// new LAN that check is simply stale. ⚠️ It never renumbers: the subnet is flock-scoped, and
// re-drawing it here would break a peer that still holds the old one, a worse failure than the
// collision it would be avoiding. So the remedy is a human's, and this is the sentence that
// reaches them. That is also why re-running Observe is the right instrument rather than a fresh
// draw: Observe is a read, and the read is what says whether the old answer is still true.
func (cfg Config) checkMovedLAN(ctx context.Context, n notify.Notifier, logf func(string, ...any)) {
	if cfg.net == nil || cfg.SystemCIDR == "" {
		return
	}
	mine, err := netip.ParsePrefix(cfg.SystemCIDR)
	if err != nil {
		return
	}
	mine = mine.Masked()
	obs := subnet.Observe(ctx)
	for _, p := range obs.Prefixes {
		if p != mine && !p.Overlaps(mine) {
			continue
		}
		// Our own address on the system subnet is in this set by construction -- it is on our own
		// tap -- so a match is only evidence when it comes from somewhere else.
		where := obs.Where[p]
		if strings.Contains(where, cfg.WitnessTap) || strings.Contains(where, cfg.SystemTap) {
			continue
		}
		fireAlert(ctx, n, logf, notify.Alert{
			Key:      "subnet",
			Kind:     notify.Open,
			Severity: notify.Warning,
			Title:    "Briard: this machine's private subnet now collides with the network it is on",
			Body: fmt.Sprintf("this machine numbers itself from %s, drawn when it was on a different network; "+
				"%s is now also in use here (%s). The address the household uses is unaffected. "+
				"This is NOT fixed automatically -- the subnet is shared with this machine's peers, so "+
				"re-drawing it here would break them.", cfg.SystemCIDR, p, where),
		})
		return
	}
	// Nothing on this network overlaps the subnet: a collision an earlier network had is over.
	fireAlert(ctx, n, logf, notify.Alert{
		Key:   "subnet",
		Kind:  notify.Resolved,
		Title: "Briard: this machine's private subnet no longer collides with its network",
		Body:  fmt.Sprintf("nothing on the network this machine is on now overlaps %s.", cfg.SystemCIDR),
	})
}

// alertReparent is sent on every re-parent, unconditionally and whatever the tier. ⚠️ It is
// never a quiet self-heal: the household's apps just moved network segments, which
// changes which wire it is on, possibly which addresses it holds, and certainly what a support
// conversation needs to know. A node that healed itself silently is a node whose next problem
// starts with nobody knowing this happened.
func alertReparent(ctx context.Context, n notify.Notifier, logf func(string, ...any), from, to string, rel nic.Relation) {
	fireAlert(ctx, n, logf, notify.Alert{
		Key:      "reparent",
		Kind:     notify.Event,
		Severity: notify.Warning,
		Title:    "Briard: this machine moved to a different network device",
		Body: fmt.Sprintf("the VM's network hung off %s, which disappeared or lost its link; it now hangs off %s (%s). "+
			"The VM was restarted to make the change. If you did not expect this, check the cabling "+
			"and which network this machine is on.", from, to, rel),
	})
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
