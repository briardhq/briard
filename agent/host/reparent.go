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

// RE-PARENTING ([B.150](e)) — what happens when the device the guest's L2 hangs off goes away.
//
// ⚠️ QEMU WILL NOT TELL YOU. When a macvtap's parent disappears the child device vanishes with it,
// and qemu keeps running with a dead NIC: the guest is alive, healthy by its own account, and
// unreachable from the household. So the detector has to be the status tick. It cannot hang off
// the guest's exit, because the guest does not exit.
//
// ⚠️ THE TRIGGER IS THE PARENT BEING ABSENT — never carrier-down (a cold boot with the cable out
// would otherwise re-parent onto some other device before the real one had finished coming up),
// and never our own device's disappearance alone (which is indistinguishable, from our side, from
// the parent going away).
//
// And re-parenting is EXPENSIVE in a way nothing else on the tick is: a macvtap cannot be
// re-parented, so the devices must be deleted and recreated, which invalidates the fds qemu holds
// — which means stopping and relaunching the guest. A healthy household loses its service for the
// length of a boot. That is why the pacing below exists at all.

// The tiers, and what each is paying for ([B.150](e), durations set by the owner 2026-09-15).
//
// ⚠️ THESE ARE A STARTING POINT AND ARE MEANT TO BE REVISED. What they are trading is stated so
// that a later reader can judge them rather than guess at them: too short and an ordinary blip --
// a switch rebooting, a laptop dock re-enumerating, a driver reloading on a kernel upgrade --
// costs the household its service for a boot; too long and a node whose NIC really was replaced
// stays dark for no reason.
const (
	// reparentSameWire: the gateway MAC matches, so this is the same router on the same segment
	// and the wire changed under us. The only unambiguous case, and the only one where waiting
	// longer buys nothing -- five minutes is "long enough that nothing transient is still in
	// flight", not a hedge about where we are.
	reparentSameWire = 5 * time.Minute
	// reparentSameSubnet: the addressing matches but the gateway MAC does not. Genuinely
	// ambiguous in BOTH directions -- a replaced router renumbering this house looks exactly like
	// 192.168.1.0/24 at a different one -- so the wait is long enough that a human who moved the
	// box has time to notice and stop us, and short enough that a real router swap heals itself
	// overnight.
	reparentSameSubnet = 30 * time.Minute
)

// ⚠️ THERE IS NO "DIFFERENT SUBNET" DURATION, and that is the decision rather than an omission
// (owner, 2026-09-15). A machine on a different subnet has MOVED, and a paired node that rebuilds
// itself on a LAN its peer is not on is a split flock -- the one outcome this whole item exists to
// avoid. So it is not a longer wait: it is an explicit operator action, `BRIARD_NIC`, which is
// already the escape hatch for every other way our selection can be wrong. (A cloud-side way out
// for managed nodes may come later; it would arrive through the directive path, not through a
// timer here.)
//
// Unknown -- too little evidence to say -- takes the same answer, because "we could not tell"
// must never be cheaper than "we could tell, and it was elsewhere".

// networkRecordName is the node-local record of the LAN this node last brought its guest up on.
// PET state, beside the other node-local caches: it has to survive a reboot (the whole point is
// comparing today's LAN against the one before the power cut) and a cattle wipe of /opt.
const networkRecordName = "network.json"

// networkRecord is what is written. A struct rather than the bare fingerprint so the file can
// grow a field without becoming ambiguous, and so a reader can see WHEN it was taken.
type networkRecord struct {
	Fingerprint nic.Fingerprint `json:"fingerprint"`
	At          time.Time       `json:"at"`
}

// networkRecordPath puts the record beside the assignment and mesh caches -- ASSIGNMENT_CACHE's
// directory is the node's pet state dir, and deriving from it rather than from a second config
// key is what keeps a non-default BRIARD_STATE working without being told twice.
func (cfg Config) networkRecordPath() string {
	if cfg.AssignmentCache == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(cfg.AssignmentCache), networkRecordName)
}

// recordNetwork writes the fingerprint of the LAN this node just brought its guest up on.
//
// Durable, because it is a fact whose only copy is this file and a power cut must not lose it:
// losing it turns the next re-parent decision into Unknown, which refuses -- safe, but it strands
// a node that could have healed itself. tmp + fsync + rename is the canonical shape (AGENTS §5).
func (cfg Config) recordNetwork(ctx context.Context, logf func(string, ...any)) {
	path := cfg.networkRecordPath()
	// nil is the ordinary state on every agent that owns no network -- every rig, every lab node,
	// every unit test -- and Run calls this unconditionally after bring-up. There is nothing to
	// record about a LAN we did not choose, and a fingerprint of somebody else's devices would be
	// worse than none: it would pace a re-parent we have no business performing.
	if path == "" || cfg.net == nil || cfg.net.Parent == "" {
		return
	}
	rec := networkRecord{Fingerprint: nic.Read(ctx, cfg.net.Parent), At: time.Now().UTC()}
	b, err := json.Marshal(rec)
	if err != nil {
		logf("network: could not render the LAN record: %v", err)
		return
	}
	if err := atomicfile.Write(path, b, 0o600, 0o700); err != nil {
		logf("network: could not record the LAN at %s: %v", path, err)
		return
	}
	logf("network: recorded this LAN (parent %s, gateway %s at %s)",
		rec.Fingerprint.Parent, rec.Fingerprint.GatewayIP, macOrUnknown(rec.Fingerprint.GatewayMAC))
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

// reparentWait is the tier: how long to let a candidate stand before rebuilding on it. The second
// return is false when no wait is long enough -- the machine has moved, or we cannot tell, and
// the answer is an operator rather than a timer.
func reparentWait(rel nic.Relation) (time.Duration, bool) {
	switch rel {
	case nic.SameWire:
		return reparentSameWire, true
	case nic.SameSubnet:
		return reparentSameSubnet, true
	default: // Elsewhere, Unknown
		return 0, false
	}
}

// reparenter holds the one piece of state the decision needs across ticks: since when the
// recorded parent has been gone. It lives for the observe loop, like the VIP router and the
// recovery counter, because "how long has this been true" is not answerable from a single tick.
type reparenter struct {
	goneSince time.Time // zero = the parent was present last time we looked
	said      string    // the last thing said about it, so the journal gets one line per change
}

// consider is the tick's whole re-parent decision. It returns the device to rebuild on, or "".
//
// ⚠️ IT NEVER TEARS DOWN BEFORE A NEW ANSWER EXISTS. A parent that is gone with nowhere to go is
// a node that waits, holding the guest it still has -- the guest is unreachable either way, and
// stopping it would only take away the one thing an operator could still look at.
//
// ⚠️ AND THE RECORDED DEVICE RETURNING OUTRANKS EVERY TIER. A NIC that comes back in three
// seconds must not cost a guest restart, so the clock resets the instant the parent is visible
// again and nothing else is even considered. That is also why there is no separate "minimum
// settle" constant: the shortest tier is five minutes, and any return inside it cancels.
func (r *reparenter) consider(ctx context.Context, cfg Config, now time.Time, logf func(string, ...any)) (string, nic.Relation) {
	if cfg.net == nil || cfg.net.Parent == "" || cfg.net.Bridge {
		// Nothing to re-parent: either this agent does not own the network, or the guest's L2
		// hangs off a bridge the USER owns. A bridge that disappears is the user's to restore --
		// we never created it and must not start now ([B.150](c)).
		return "", nic.Unknown
	}
	if nic.Up(cfg.net.Parent) || exists("/sys/class/net/"+cfg.net.Parent) {
		if !r.goneSince.IsZero() {
			logf("network: %s is back; nothing to re-parent", cfg.net.Parent)
			r.goneSince, r.said = time.Time{}, ""
		}
		return "", nic.Unknown
	}
	if r.goneSince.IsZero() {
		r.goneSince = now
		logf("network: %s has gone away; the guest is running with a dead NIC", cfg.net.Parent)
	}
	// Re-SELECT, without probing yet: there is no point pacing a move onto a device that does not
	// exist. A candidate that is the recorded parent again cannot happen here (we just established
	// it is absent), so any answer is genuinely a different device.
	sel := nic.Select(cfg.NICOverride)
	if !sel.Usable() {
		r.say(logf, "network: nowhere to re-parent yet: "+sel.Fix())
		return "", nic.Unknown
	}
	rel := nic.Compare(cfg.recordedNetwork(), nic.Read(ctx, sel.Dev))
	wait, ok := reparentWait(rel)
	if !ok {
		// THE MACHINE HAS MOVED, or we cannot tell. Never automatic: a paired node that rebuilds
		// itself on a LAN its peer is not on is a split flock.
		r.say(logf, fmt.Sprintf(
			"network: %s is gone and %s is on a different network (%s) -- NOT re-parenting on my own. "+
				"Set BRIARD_NIC=%s in %s and restart briard-agent if this move is intended",
			cfg.net.Parent, sel.Dev, rel, sel.Dev, cfg.configPathForMessage()))
		return "", rel
	}
	if left := wait - now.Sub(r.goneSince); left > 0 {
		r.say(logf, fmt.Sprintf("network: %s is gone; %s is %s, re-parenting in %s unless it comes back",
			cfg.net.Parent, sel.Dev, rel, left.Round(time.Second)))
		return "", rel
	}
	return sel.Dev, rel
}

// reparent moves the guest's L2 onto dev: rebuild the devices there, restart the guest so it
// opens the new chardevs, record the LAN we landed on, and tell the household.
//
// ⚠️ THE GUEST RESTART IS NOT OPTIONAL and it is the expensive part. qemu holds an fd per
// macvtap; a macvtap cannot be re-parented, so the devices are new devices, and only a relaunch
// makes qemu open them. `RescueGuest` is the canonical stop-and-relaunch -- it performs the same
// VM+channel+Manager swap the upgrade legs do, and a second owner of that swap would be a second
// way to do it (the upgrader interface says so).
//
// ⚠️ NOTHING HERE TOUCHES THE HOST'S OWN ADDRESSING OR ROUTES, which is what keeps the agent's
// path to the cloud intact across a re-parent (owner, 2026-09-15). It is true by construction
// rather than by care: since [B.150](c) we never move a host address anywhere, so the only
// devices in play are ours.
func (cfg Config) reparent(ctx context.Context, mgr upgrader, n notify.Notifier, dev string, rel nic.Relation, logf func(string, ...any)) error {
	from := cfg.net.Parent
	logf("network: re-parenting the guest's L2 from %s onto %s (%s)", from, dev, rel)
	// ONE BUDGET FOR THE WHOLE MOVE, and the watchdog rides it. The lease must cover a BOUNDED
	// context (beat.go argues this at length): the stop leg inside RescueGuest spends a full
	// shutdownGrace and `vm.Stop()` takes no context at all, so nothing else is watching that
	// stretch -- and leasing the caller's root context instead would ping forever and switch the
	// watchdog off permanently, which is the exact failure a lease is shaped to avoid.
	mv, cancel := context.WithTimeout(ctx, netTick+cfg.BringUpBudget+3*shutdownGrace)
	defer cancel()
	cfg.beat.Lease(mv)
	// Rebuild BEFORE the restart. The children went away with their parent, so there are no live
	// fds to protect; and doing it first means the guest comes back into a network that is
	// already right rather than one being assembled underneath it.
	spec := cfg.netSpec(dev, nic.IsBridge(dev))
	rb, rbCancel := context.WithTimeout(mv, netTick)
	err := nic.Rebuild(rb, spec)
	rbCancel()
	if err != nil {
		return fmt.Errorf("re-parent onto %s: %w", dev, err)
	}
	*cfg.net = spec
	if mgr == nil {
		// A node with no guest to restart (a witness) is fully re-parented by the rebuild above.
		cfg.recordNetwork(ctx, logf)
		alertReparent(ctx, n, logf, from, dev, rel)
		return nil
	}
	if err := mgr.RescueGuest(mv); err != nil {
		return fmt.Errorf("re-parent onto %s: restart the guest: %w", dev, err)
	}
	// RECORDED AFTER the guest is back, not before: the record is what a future re-parent paces
	// itself against, and recording a LAN we had not actually served on would teach the next
	// decision something we never verified.
	cfg.recordNetwork(ctx, logf)
	alertReparent(ctx, n, logf, from, dev, rel)
	return nil
}

// say logs once per distinct message. A node waiting out a tier is a node that must not fill its
// journal with the same line every ten seconds for half an hour.
func (r *reparenter) say(logf func(string, ...any), msg string) {
	if msg != r.said {
		logf("%s", msg)
		r.said = msg
	}
}

// configPathForMessage names the file an operator would edit. It is the message's job to be
// actionable, and "set BRIARD_NIC" is not actionable without saying where.
func (cfg Config) configPathForMessage() string {
	if p := os.Getenv("BRIARD_CONFIG"); p != "" {
		return p
	}
	return defaultConfigFile
}

// checkMovedLAN is the one thing a re-parent cannot fix and must therefore say out loud
// ([B.150](e)): this node's system subnet was DRAWN against the collision landscape of a
// different network, and on a new LAN that check is simply stale.
//
// ⚠️ IT NEVER RENUMBERS. The subnet is flock-scoped -- re-drawing it here would break a peer that
// still holds the old one, which is a worse failure than the collision it would be avoiding. So
// the remedy is a human's, and this is the sentence that reaches them.
//
// It is also why "re-run Observe" is the right instrument rather than a fresh draw: Observe is a
// read, and the read is what says whether the old answer is still true.
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
		al := notify.Alert{
			Level: notify.Warning,
			Title: "Briard: this node's private subnet now collides with the network it is on",
			Body: fmt.Sprintf("this node numbers itself from %s, drawn when it was on a different network; "+
				"%s is now also in use here (%s). The address the household uses is unaffected. "+
				"This is NOT fixed automatically -- the subnet is shared with this node's peers, so "+
				"re-drawing it here would break them.", cfg.SystemCIDR, p, where),
		}
		logf("%s", notify.LogLine(al))
		if n != nil {
			nctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			_ = n.Notify(nctx, al)
			cancel()
		}
		return
	}
}

// alertReparent is sent on EVERY re-parent, unconditionally and whatever the tier.
//
// ⚠️ It is never a quiet self-heal. The household's service just moved network segments, which
// changes which wire it is on, possibly which addresses it holds, and certainly what a support
// conversation needs to know. A node that healed itself silently is a node whose next problem
// starts with nobody knowing this happened.
func alertReparent(ctx context.Context, n notify.Notifier, logf func(string, ...any), from, to string, rel nic.Relation) {
	al := notify.Alert{
		Level: notify.Warning,
		Title: "Briard: this node moved to a different network device",
		Body: fmt.Sprintf("the guest's network hung off %s, which disappeared; it now hangs off %s (%s). "+
			"The service was restarted to make the change. If you did not expect this, check the cabling "+
			"and which network this machine is on.", from, to, rel),
	}
	logf("%s", notify.LogLine(al))
	if n == nil {
		return
	}
	nctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_ = n.Notify(nctx, al)
}

func macOrUnknown(mac string) string {
	if mac == "" {
		return "an address it could not resolve"
	}
	return mac
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
