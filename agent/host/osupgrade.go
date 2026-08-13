package host

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"briard.io/agent/guest"
	"briard.io/agent/guestagent"
	"briard.io/agent/platform"
	"briard.io/shared/model"
)

// AN OS UPGRADE, end to end, by both of its methods -- switch and reboot.
//
// WHY IT IS HERE AND NOT IN guest.Manager. Both methods roll back to the same thing: a
// snapshot of the OS DISK, which only the host can take and only the host can restore. The
// reboot method adds a second reason of its own -- it destroys the control channel a Manager
// is bound to, and the boot selector deciding which generation comes up is a property of the
// launch rather than anything written to the disk. host.go already owns
// launch/adopt/reconnect, so this is where both sequences belong. What they do NOT do is keep
// a second copy of the upgrade bracket: baseline, maintenance, health-gate and assess are the
// Manager's own phases, called from here in order.
//
// THE SHAPE, and it is one shape with two middles (the (c2) procedure, phases A/B/C):
//
//	switch: snapshot the OS disk LIVE -> switch-to-configuration switch
//	reboot: arm the boot -> stop cleanly -> snapshot the OS disk offline -> relaunch with the
//	        selector -> prove the target actually booted
//
// then, identically for both: health-gate -> commit (+ collect, drop the point) or restore.
//
// WHAT NEITHER OF THEM TOUCHES: the workload. No payload stop, no payload
// start, no data snapshot, no data restore. `switch-to-configuration switch` restarts a unit
// only if that unit changed, which is what makes "the new OS runs the same containers on the
// same data" a mechanism rather than an aspiration; the reboot method stops everything by
// going down, and systemd does that in order without being asked. If the update fails, THE OS
// rolls back -- the service has no reason to, and the replicated volume is never in scope.
// The one thing that still reads service state is the health-gate, which is correct and is the
// line: reading a service to decide whether to revert the OS is not touching it.
//
// WHAT THE ROLLBACK POINT COVERS: the qcow2 snapshot, and deliberately no btrfs data snapshot.
// An OS upgrade cannot change payload code -- the payload's identity is a runtime pin on the
// replicated volume, re-pointed only by UpgradePayload -- so there is no data migration to
// undo, and reverting the subvolume would delete real user data written during the gate
// window. What an OS upgrade *can* disturb (/var/lib/nixos, /var/lib/containers, systemd
// state) all lives on the qcow2, which is exactly what the snapshot holds.

// shutdownGrace bounds each clean-stop attempt. A guest that has not powered off in this long
// is not going to: a converged guest -- DRBD Primary, volume mounted, payload serving, VIP up --
// was MEASURED powering itself off in 1.5 s, so 30 s is twenty times the observed cost and still
// well inside a stop worth waiting for.
//
// IT WAS 90 s, WHICH IS SYSTEMD'S DefaultTimeoutStopSec, AND THE COLLISION HID A BUG FOR MONTHS
// ([B.85]). A unit inside the guest was deadlocking on stop and eating that exact timeout; this
// grace expired in the same instant the guest's SIGKILL fired, so the ACPI fallback appeared to
// power the machine off in 1.5 s when all it had done was arrive as the deadlock resolved
// itself. Every clean stop paid 90 s and looked like it worked. A grace SHORTER than the guest's
// own patience is what makes the next fault of that shape fail loudly instead of silently: the
// fallback then genuinely does the work, and a fallback that fires every time is visible.
const shutdownGrace = 30 * time.Second

// ErrHandoverRequired is the refusal: this node is serving, a peer could take the work, and
// applying the update means a reboot -- which on an HA pair IS a failover. Sequencing a
// failover is not a decision a node makes about itself, so the node-local path declines and
// leaves everything exactly as it found it.
//
// Refusing is not conservatism, it is the only correct answer. Reboot here and a peer takes
// over while the node is down, so it returns Secondary -- where the payload never runs, since
// drbd-reactor promotes exactly one node. The health-gate asks a Primary-shaped question
// (PayloadActive && probe), which a Secondary can never satisfy, so every such upgrade would
// false-roll-back. Demoting deliberately first is the right operation but the wrong owner:
// that is a handover, it must be scheduled so both anchors do not go at once, and only the
// cloud sees the whole flock.
//
// Hence a clean no-op rather than a partial attempt: nothing has moved when this returns.
var ErrHandoverRequired = errors.New("reboot needs a handover: a peer can take over, so this is a failover to schedule, not a local update")

// osUpgrade is the host's binding to the guest, and the upgrade surface the directive path
// drives: a guest.Manager plus the one operation only the host can perform. Everything the
// Manager already does is promoted unchanged; RebootUpgrade is the addition.
//
// It holds the binding rather than a snapshot of it because a reboot REPLACES all three parts
// at once -- VM, channel and Manager -- and something has to own that swap. Run rebinds it on
// an ordinary reconnect; the reboot path rebinds it from the inside.
type osUpgrade struct {
	*guest.Manager // over client, below; swapped whenever the channel is

	cfg      Config
	guestCfg guest.Config
	vm       *platform.Guest
	client   *guestagent.Client
	logf     func(string, ...any)
}

func newOSUpgrade(cfg Config, vm *platform.Guest, client *guestagent.Client, guestCfg guest.Config, logf func(string, ...any)) *osUpgrade {
	u := &osUpgrade{cfg: cfg, guestCfg: guestCfg, vm: vm, logf: logf}
	u.rebind(client)
	return u
}

// Rebind points this at a fresh control channel. The Manager is rebuilt rather than mutated
// because it is a thin wrapper over the channel -- there is no state in it worth carrying
// across, which is also why Run has always rebuilt it on reconnect.
func (u *osUpgrade) rebind(client *guestagent.Client) {
	u.client = client
	u.Manager = guest.NewManager(client, u.guestCfg)
}

// Channel reports the control channel this is currently bound to, so Run can notice when the
// reboot path has already re-established one and adopt it rather than dial a second. The guest
// agent serves a single connection at a time, so a second dial would not merely be wasteful --
// it would block until the first was dropped.
func (u *osUpgrade) channel() *guestagent.Client { return u.client }

// Upgrade upgrades this node to target IN BAND -- (c1) has already established that the
// change touches no kernel, initrd, module or boot parameter, so activating it does not need
// the machine to go down. It shadows the embedded Manager's primitives with the sequence that
// composes them, exactly as RebootUpgrade does below, and reports the same pair: rolledBack
// says where the NODE ended up, err says what went wrong.
//
// It takes no ServiceSpec, and that absence is the point. This path has nothing to say to a
// workload: it does not stop one, start one, snapshot its data or restore it. It waits on the
// OS gate (AwaitOSReady), which asks this node about the job it currently holds — so a service
// down for its own reasons cannot revert a healthy OS upgrade, and a Secondary is not judged
// by a front door its peer answers.
//
// WHAT IT COSTS: the rollback leg goes through the disk, so a switch that trips its gate stops
// and relaunches the guest rather than switching back in band. Deliberate — the snapshot
// covers state a generation rollback cannot reach (/var/lib/nixos, /var/lib/containers,
// systemd state), and one rollback path for both methods is one path to test and to trust.
// Rollbacks are the rare leg; the common one is a switch that works and stops nothing.
func (u *osUpgrade) Upgrade(ctx context.Context, target string) (rolledBack bool, err error) {
	mgr := u.Manager
	qspec := u.cfg.guestSpec()

	// Read the rollback target before anything moves, and let it double as proof the control
	// channel is live before the sequence starts leaning on it.
	prev, err := mgr.SystemPath(ctx)
	if err != nil {
		return false, fmt.Errorf("read current system: %w", err)
	}
	u.logf("switch-upgrade: %s -> %s", prev, target)

	// Sample readiness while the old code still serves.
	rd := mgr.CaptureBaseline(ctx)

	// Hold the promoter across the switch. This is the whole quiesce now: it stops
	// drbd-reactor from reading a unit that switch-to-configuration is restarting as a
	// failure and promoting or demoting underneath it. It is NOT a payload stop -- the
	// payload keeps serving, and if the switch does not change its unit, nothing interrupts
	// it at all.
	if e := mgr.EnterMaintenance(ctx); e != nil {
		return false, fmt.Errorf("enter maintenance: %w", e)
	}

	// The rollback point, taken live because the VM stays up by definition here (qmp.go).
	// Before the switch, so a failure to take it is a refusal rather than an unprotected
	// upgrade: nothing has moved, so resume and report.
	if e := u.vm.SnapshotCreateLive(ctx); e != nil {
		return false, errors.Join(fmt.Errorf("snapshot: %w", e), u.resume(ctx))
	}

	// Switch this node, and only this node. There is no whole-OS pin on the replicated volume
	// any more: a system closure is a property of the NODE, not of the data, so a peer
	// that promotes mid-window is not wrong to serve on the generation it has. What the data
	// does pin -- the payload image and the service manifest -- an OS upgrade never touches.
	if e := mgr.Switch(ctx, target); e != nil {
		return u.restore(ctx, qspec, prev, fmt.Errorf("switch to %s: %w", target, e))
	}
	u.logf("switch-upgrade: switched to %s, health-gating", target)

	if e := mgr.AwaitOSReady(ctx); e != nil {
		return u.restore(ctx, qspec, prev, e)
	}
	if e := mgr.Assess(ctx, rd); e != nil { // differential S1 gate above the floor
		return u.restore(ctx, qspec, prev, e)
	}

	// Commit. The switch already installed the bootloader, so promotion is a no-op on this
	// path ((c2) phase C) and what remains is to collect the generation this displaced and
	// end the upgrade window -- in that order, because while the snapshot stands it still owns
	// the blocks a collection frees.
	mgr.CollectStore(ctx)
	if e := u.vm.SnapshotDropLive(ctx); e != nil {
		// Not worth undoing a healthy upgrade over: the node is running the target and gated.
		// It does leave the disk claiming an upgrade is in flight, so say so.
		u.logf("switch-upgrade: WARNING committed %s but could not drop the rollback point: %v", target, e)
	}
	u.logf("switch-upgrade: %s committed", target)
	return false, u.resume(ctx)
}

// RebootUpgrade upgrades this node to target by rebooting into it. It reports whether the node
// is on its previous generation (rolledBack) alongside the error, because the two carry
// different news: rolledBack means the node is healthy on its old code, while a bare failure
// means the host could not finish the job and the node needs looking at. Both are false/nil
// only when the node is up and gated on target.
//
// The gate below returns (true, ErrHandoverRequired) without having done anything, which reads
// oddly until you take rolledBack at its word: it says where the NODE is, not what was done to
// it. Untouched and serving on prev is the same answer as reverted to prev for everyone
// downstream, and a caller that needs to tell them apart has errors.Is.
func (u *osUpgrade) RebootUpgrade(ctx context.Context, target string) (rolledBack bool, err error) {
	mgr := u.Manager
	qspec := u.cfg.guestSpec()

	// The gate, before anything moves and before anything is read that could go stale: would
	// going down hand the work to someone else? If it would, decline (ErrHandoverRequired).
	//
	// The predicate is "a peer CAN TAKE OVER" -- connected, diskful, UpToDate (model.PeerState)
	// -- deliberately not "would the flock still be quorate without me". Quorum answers who may
	// write, not who can serve: a 1-anchor + 2-witness flock is quorate without this node and has
	// nobody to hand the workload to, so a quorum-shaped test would wave through exactly the
	// reboot that takes the house offline.
	//
	// A read failure declines too. Not knowing whether a peer would take over is not the same as
	// knowing none would, and the two outcomes are a reboot apart.
	cl, err := u.client.Cluster(ctx, u.cfg.Resource.Name)
	if err != nil {
		return true, fmt.Errorf("read cluster before reboot upgrade: %w", err)
	}
	// BOTH halves, and the first one was missing until: a handover only exists if this node
	// is actually SERVING. The predicate asked only about peers, so a Secondary -- and even a
	// diskless witness, which holds nothing whatsoever -- declined its own reboot because some
	// anchor "could take over". That refused the two ordinary update shapes outright: a lone node,
	// and the common HA one of rolling the pair by upgrading the SECONDARY first. Both are exactly
	// where a reboot is safe, since the peer stays up and the returning node sees two storage
	// nodes. The doc on ErrHandoverRequired always said "this node is serving"; only the
	// code disagreed.
	if cl.Primary && cl.PeerCanTakeOver() {
		return true, fmt.Errorf("%w (peers: %s)", ErrHandoverRequired, describePeers(cl))
	}

	// Read the rollback target before anything moves: the closure running right now. Kept as a
	// name rather than re-derived later because between here and the verdict this node is
	// stopped, restored and rebooted, and the host is the only thing that stays continuous
	// across all three.
	prev, err := mgr.SystemPath(ctx)
	if err != nil {
		return false, fmt.Errorf("read current system: %w", err)
	}
	u.logf("reboot-upgrade: %s -> %s", prev, target)

	// Sample readiness while the old code still serves.
	rd := mgr.CaptureBaseline(ctx)

	// Hold the promoter for the shutdown. This is not the switch path's reason -- there the
	// hold spans a payload stop -- but the shutdown's: drbd-reactor firing a promote into a
	// system that is tearing down is the deadlock, and a deadlocked shutdown is a SIGKILL,
	// i.e. the power cut this whole path exists to avoid. There is no matching resume, because
	// the hold is `systemctl stop drbd-reactor` and the boot discharges it: the guest that
	// comes back has a reactor that was never stopped. The bracket does not cross the reboot.
	if e := mgr.EnterMaintenance(ctx); e != nil {
		return false, fmt.Errorf("enter maintenance: %w", e)
	}

	// Arm the boot first: os.stageboot is inert (the disk still boots what it booted before)
	// and it is the step most likely to fail, so failing it while the node is still serving
	// costs nothing but a resume.
	//
	// Nothing else is asked of the guest before it goes down. Stopping the payload here would be
	// both a service mutation the OS has no business making and redundant -- the machine is about
	// to shut down, and systemd stops its units in order.
	if e := mgr.StageBoot(ctx, target); e != nil {
		return false, errors.Join(fmt.Errorf("stage boot %s: %w", target, e), u.resume(ctx))
	}

	// From here the node is committed to going down. A failure to stop cleanly is NOT resolved
	// by stopping dirtily: the guest has just rewritten its bootloader, and power-cutting it at
	// that moment is what hung the next boot half the time. So a guest that ignores
	// both routes is left running, on its old generation -- degraded but serving, and loud.
	if e := stopCleanly(ctx, u.vm, u.client, u.logf); e != nil {
		if re := u.resume(ctx); re != nil {
			return false, fmt.Errorf("clean shutdown refused AND could not restore the node: %w",
				errors.Join(e, re))
		}
		return true, fmt.Errorf("clean shutdown refused, node left running %s: %w", prev, e)
	}

	// The rollback point, taken offline on a guest that shut itself down -- so it is a quiesced
	// filesystem rather than a crash-consistent one (snapshot.go).
	if e := qspec.SnapshotCreate(ctx); e != nil {
		// Nothing on the disk has changed that a launch cannot undo: relaunching without the
		// selector comes up on prev, and restore handles the rest.
		return u.restore(ctx, qspec, prev, fmt.Errorf("snapshot: %w", e))
	}

	staged := qspec
	staged.BootStaging = true // the selector: which generation boots, decided outside the disk
	g, client, e := u.cfg.bringUp(ctx, staged, u.logf)
	if e != nil {
		return u.restore(ctx, qspec, prev, fmt.Errorf("boot target: %w", e))
	}
	u.vm = g
	u.rebind(client) // the reboot replaced the channel; Run adopts this one when it notices
	mgr = u.Manager

	// Prove the target is what actually booted. The selector fails SAFE -- a missing or
	// unmatched SMBIOS string makes grub fall through to its default -- so a node that quietly
	// came back on the old generation is a real outcome, and one that would otherwise pass the
	// health-gate with flying colours and be recorded as an upgrade.
	booted, e := mgr.SystemPath(ctx)
	switch {
	case e != nil:
		return u.restore(ctx, qspec, prev, fmt.Errorf("read booted system: %w", e))
	case booted != target:
		return u.restore(ctx, qspec, prev,
			fmt.Errorf("booted %s, not the staged target %s (boot selector did not take)", booted, target))
	}
	u.logf("reboot-upgrade: booted %s, health-gating", target)

	// Not the service's question, on either method ( deletion 5): a spec would AND in "the
	// payload unit is active", reverting a perfectly good OS upgrade whenever a service is down
	// for reasons of its own. It is not the FRONT DOOR's question either, which is what
	// corrected — the front door lives at the VIP, and a node that comes back Secondary (as this
	// one legitimately may,) would be asking its peer. AwaitOSReady asks this node about
	// the job it holds: serving ⇒ the door answers, storage ⇒ the replica is UpToDate.
	if e := mgr.AwaitOSReady(ctx); e != nil {
		return u.restore(ctx, qspec, prev, e)
	}
	if e := mgr.Assess(ctx, rd); e != nil { // differential S1 gate above the floor
		return u.restore(ctx, qspec, prev, e)
	}

	// Commit. os.switch points the SYSTEM profile at the target and reinstalls the bootloader
	// from it, which is what makes the next ordinary launch -- one with no selector passed --
	// come up here. Until this runs, the node is one relaunch away from its old generation,
	// which is precisely the property the gate above needed.
	if e := mgr.Switch(ctx, target); e != nil {
		return u.restore(ctx, qspec, prev, fmt.Errorf("commit %s: %w", target, e))
	}
	// Collect the displaced generation, then end the upgrade window -- (c2) phase C's order,
	// and it matters: while the snapshot stands, the blocks a GC frees are still owned by it,
	// so collecting first and dropping second is what actually returns them.
	mgr.CollectStore(ctx)
	// End the upgrade window. The guest is up and staying up, so this goes through QEMU.
	if e := u.vm.SnapshotDropLive(ctx); e != nil {
		// Not worth undoing a healthy upgrade over: the node is running the target and gated.
		// It does leave the disk claiming an upgrade is in flight, so say so.
		u.logf("reboot-upgrade: WARNING committed %s but could not drop the rollback point: %v", target, e)
	}
	u.logf("reboot-upgrade: %s committed", target)
	return false, nil
}

// describePeers renders why the gate decided as it did, so the refusal names the successor
// rather than merely asserting one exists -- the difference between a log line a human can act
// on and one they have to reproduce.
func describePeers(cl model.Cluster) string {
	parts := make([]string, 0, len(cl.Peers))
	for _, p := range cl.Peers {
		why := "cannot take over"
		if p.CanTakeOver() {
			why = "CAN take over"
		}
		parts = append(parts, fmt.Sprintf("%s[%s connected=%v diskful=%v uptodate=%v: %s]",
			p.Name, p.Role, p.Connected, p.Diskful, p.UpToDate, why))
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, " ")
}

// Resume ends the maintenance bracket on a guest that is still up -- the commit leg of a
// switch, and either path's failure BEFORE the guest went down. Nothing else needs undoing:
// os.stageboot is inert (the disk still boots what it booted before), there is no longer a
// whole-OS pin to restore, and there is no payload to raise because neither path ever
// stopped one.
//
// It runs on the CURRENT binding rather than a passed-in Manager, because a caller that has
// been through the rollback leg is holding a channel that no longer exists.
func (u *osUpgrade) resume(ctx context.Context) error {
	if e := u.Manager.ExitMaintenance(ctx); e != nil {
		return fmt.Errorf("resume promoter: %w", e)
	}
	return nil
}

// Restore is THE rollback leg -- one for both methods (the (c2) procedure, phase B): stop the
// guest, put the OS disk back, and bring it up on the generation it came from. It runs on a
// context detached from the upgrade's own deadline (the rule) -- a gate that tripped because
// time ran out must not then find there is no time left to recover.
//
// It reads both facts it needs off reality rather than taking the caller's word, because the
// legs that arrive here do so from different states -- a switch that failed its gate leaves a
// live VM on the new generation, a failed clean shutdown leaves a live VM on the old one, a
// failed snapshot leaves a stopped one with no point to restore. Asking the disk whether a
// rollback point exists is exactly what the fixed snapshot tag was for (snapshot.go): the same
// question an agent that died mid-upgrade would have to ask.
//
// STOPPING IS CLEAN FIRST, FORCED AS THE FALLBACK, and the reason is the DATA disk rather
// than the OS one. Reverting the OS disk wholesale would justify forcing -- nothing it might
// still write matters. But the restore never touches the data disk, and that is where DRBD
// keeps its metadata: a clean stop is what lets DRBD record MDF_HAVE_QUORUM + prev_members on
// the way down, which is the difference between a node that comes back quorate and one that
// is stranded. Forcing would make every rollback the stranded case by choice, in exactly the
// degraded topology a reboot upgrade creates.
//
// The fallback is not a formality. A guest arriving here has just failed a health gate, so it
// may well be one that answers neither os.poweroff nor an ACPI button. When both routes
// decline, force: a node that may come back non-quorate beats a rollback that hangs forever
// on a guest that is never going down.
//
// It says nothing to the workload on the way back, and does not need to. The restored disk
// boots the generation it came from, and that boot raises the payload the way every boot does
// -- through the promoter, which the shutdown discharged. What the data pins (the payload
// image, the service manifest) an OS upgrade never touched.
func (u *osUpgrade) restore(ctx context.Context, qspec platform.QEMUSpec, prev string, cause error) (bool, error) {
	// The budget is what it always was -- one shutdownGrace for the stop and the disk work,
	// plus a bring-up -- widened by the two graces stopCleanly can spend trying its two routes
	// before the forced stop it falls back to.
	rb, cancel := context.WithTimeout(context.WithoutCancel(ctx), u.cfg.BringUpBudget+3*shutdownGrace)
	defer cancel()
	// Leased like the recovery ladder's power-cycle, and for the same reason: the stop ahead of the
	// bring-up spends real time in calls that do not all take a context. This leg matters more than
	// most -- it runs on a node that is already degraded, where a watchdog misfire would restart the
	// agent in the middle of the rollback it is performing (V3.32).
	u.cfg.beat.Lease(rb)
	u.logf("os-upgrade: rolling back to %s (%v)", prev, cause)

	errs := []error{cause}
	if platform.Running(rb, qspec) {
		// Both outcomes are logged, including the good one. Which stop a rollback used is
		// invisible from outside otherwise -- the guest is gone either way -- and it is the
		// single fact that decides whether the node comes back quorate, so it is also the only
		// thing a test can assert about this leg: our end-to-end rollback demo.
		if e := stopCleanly(rb, u.vm, u.client, u.logf); e != nil {
			// Carried in the returned error too, not just the log: a forced stop is what
			// predicts a node returning without quorum, and the alternative to saying so here
			// is learning it from the crash loop afterwards.
			errs = append(errs, fmt.Errorf("rollback could not stop the guest cleanly, forcing (%w)", e))
			if e := u.vm.Stop(); e != nil {
				errs = append(errs, fmt.Errorf("rollback stop: %w", e))
			}
		} else {
			u.logf("os-upgrade: rollback stopped the guest cleanly")
		}
	}
	snapped, e := qspec.SnapshotExists(rb)
	if e != nil {
		return false, fmt.Errorf("rollback FAILED, guest left stopped: %w", errors.Join(append(errs, e)...))
	}
	if snapped {
		// Restore and drop the point in the same offline moment -- both need the image closed,
		// and leaving the tag behind would leave the disk describing an upgrade that is over.
		if e := qspec.SnapshotRestore(rb); e != nil {
			// The node is down with an unrestored disk. Do not relaunch a half-upgraded system
			// on top of it; report the whole truth and stop.
			return false, fmt.Errorf("rollback FAILED, guest left stopped: %w", errors.Join(append(errs, e)...))
		}
		if e := qspec.SnapshotDelete(rb); e != nil {
			errs = append(errs, fmt.Errorf("rollback drop point: %w", e))
		}
	}

	// No selector: that is the whole rollback of the boot decision (nothing was ever armed on
	// the disk to disarm).
	g, client, e := u.cfg.bringUp(rb, qspec, u.logf)
	if e != nil {
		return false, fmt.Errorf("rollback FAILED to boot %s: %w", prev, errors.Join(append(errs, e)...))
	}
	u.vm = g
	u.rebind(client)
	return true, fmt.Errorf("OS upgrade rolled back to %s: %w", prev, errors.Join(errs...))
}

// stopCleanly takes the guest down without power-cutting it, by two independent routes with
// the agent first: ask the guest OS in its own terms over the channel we already
// hold, and fall back to the ACPI power button for the case that route cannot cover -- the
// agent itself being what died. Neither route's request means the machine stopped, so both
// end in WaitStopped: the caller's next act is to touch a disk QEMU may still hold open.
//
// Every stop that is not a self-fence goes through here -- the reboot upgrade's, which needs
// the guest's bootloader rewrite to survive; the rollback leg's, which needs DRBD to record its
// quorum state; and the recovery ladder's, which needs the same DRBD record on a guest that is
// wedged rather than upgrading. They differ in what they do when it fails: the reboot path
// abandons the upgrade (a guest that will not go down keeps serving its old generation), while
// the rollback leg and the recovery ladder force it (there is nothing left to preserve by
// waiting). Hence the neutral log prefix below: by the time this runs, which of the three is
// calling is not something the messages can assume.
func stopCleanly(ctx context.Context, g *platform.Guest, client *guestagent.Client, logf func(string, ...any)) error {
	perr := client.PowerOff(ctx)
	if perr == nil {
		if err := g.WaitStopped(ctx, shutdownGrace); err == nil {
			return nil
		} else {
			perr = err
		}
	}
	logf("guest-stop: guest agent did not stop the machine (%v); trying the power button", perr)
	if err := g.Shutdown(ctx, shutdownGrace); err != nil {
		// A FAILED REQUEST IS NOT A FAILED SHUTDOWN, and reading it as one is what defeated
		// on its first instrumented run: os.poweroff is `systemctl poweroff --no-block`
		// exactly so the reply is written before systemd tears the machine down -- a race the
		// guest can lose. A lost reply then looks identical to a dead agent (both are EOF), so
		// this escalated to a power button that had nothing left to press: the monitor socket
		// was already gone (`dial ...: no such file or directory`), that error joined the first,
		// and the caller force-killed a guest that had ALREADY stopped cleanly -- manufacturing
		// the very act-2b restart the clean stop exists to avoid.
		//
		// Both routes' errors are therefore evidence about a REQUEST, never about the machine.
		// Only one question settles it, and it is free to ask: is the VM still there? A zero
		// grace makes this a probe rather than another wait -- WaitStopped answers immediately
		// when the unit is already gone.
		if g.WaitStopped(ctx, 0) == nil {
			logf("guest-stop: the guest was already down — the request landed, its reply did not")
			return nil
		}
		return errors.Join(perr, err)
	}
	return nil
}
