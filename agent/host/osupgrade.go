package host

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"briard.io/agent/guest"
	"briard.io/agent/guestagent"
	"briard.io/agent/install"
	"briard.io/agent/platform"
	"briard.io/shared/atomicfile"
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
// a second copy of the upgrade bracket: maintenance and the health-gate are the Manager's own
// phases, called from here in order.
//
// THE SHAPE, and it is one shape with two middles (the (c2) procedure, phases A/B/C):
//
//	switch: snapshot the OS disk LIVE -> switch-to-configuration switch
//	reboot: arm the boot -> stop cleanly -> snapshot the OS disk offline -> relaunch with the
//	        selector -> prove the target actually booted
//
// then, identically for both: health-gate -> commit (+ collect, drop the point) or restore.
//
// WHAT NEITHER OF THEM TOUCHES: the workload. No service stop, no service
// start, no data snapshot, no data restore. `switch-to-configuration switch` restarts a unit
// only if that unit changed, which is what makes "the new OS runs the same containers on the
// same data" a mechanism rather than an aspiration; the reboot method stops everything by
// going down, and systemd does that in order without being asked. If the update fails, THE OS
// rolls back -- the service has no reason to, and the replicated volume is never in scope.
// The one thing that still reads service state is the health-gate, which is correct and is the
// line: reading a service to decide whether to revert the OS is not touching it.
//
// WHAT THE ROLLBACK POINT COVERS: the qcow2 snapshot, and deliberately no btrfs data snapshot.
// An OS upgrade cannot change service code -- a service's identity is the manifest on the
// replicated volume, re-pointed only by a service install -- so there is no data migration to
// undo, and reverting the subvolume would delete real user data written during the gate
// window. What an OS upgrade *can* disturb (/var/lib/nixos, /var/lib/containers, systemd
// state) all lives on the qcow2, which is exactly what the snapshot holds.

// shutdownGrace bounds each clean-stop attempt. A guest that has not powered off in this long
// is not going to: a converged guest -- DRBD Primary, volume mounted, services serving, VIP up --
// was MEASURED powering itself off in 1.5 s, so 30 s is twenty times the observed cost and still
// well inside a stop worth waiting for.
//
// IT WAS 90 s, WHICH IS SYSTEMD'S DefaultTimeoutStopSec, AND THE COLLISION HID A BUG FOR MONTHS
// A unit inside the guest was deadlocking on stop and eating that exact timeout; this
// grace expired in the same instant the guest's SIGKILL fired, so the ACPI fallback appeared to
// power the machine off in 1.5 s when all it had done was arrive as the deadlock resolved
// itself. Every clean stop paid 90 s and looked like it worked. A grace SHORTER than the guest's
// own patience is what makes the next fault of that shape fail loudly instead of silently: the
// fallback then genuinely does the work, and a fallback that fires every time is visible.
const shutdownGrace = 30 * time.Second

// reapGrace bounds the last question stopCleanly asks -- "is the machine gone?" -- after both
// stop routes have failed to say anything useful about it. It is deliberately much shorter than
// shutdownGrace because it waits for something much smaller: not a guest shutting down (that has
// already had its grace by then) but systemd finishing with a QEMU process that has already
// exited. Measured at ~3 s live; this is three times that and still cheap enough that
// the reboot path's worst case stays inside rebootGuest's budget.
const reapGrace = 10 * time.Second

// ErrHandoverRequired is the refusal: this node is serving, a peer could take the work, and
// applying the update means a reboot -- which on an HA pair IS a failover. Sequencing a
// failover is not a decision a node makes about itself, so the node-local path declines and
// leaves everything exactly as it found it.
//
// Refusing is not conservatism, it is the only correct answer. Reboot here and a peer takes
// over while the node is down, so it returns Secondary -- where the services never run, since
// drbd-reactor promotes exactly one node. The health-gate asks a Primary-shaped question
// (ServiceActive && probe), which a Secondary can never satisfy, so every such upgrade would
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
// the image swap is a rename (the disk still boots what it booted before), there is no longer a
// whole-OS pin to restore, and there is nothing to raise because neither path ever
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

// guestStopper is the two-method slice of *platform.Guest that stopCleanly actually uses: ask
// the machine to stop, and ask whether it has. Narrowed to an interface so the STOPPING window
// -- a guest that is on its way down but not yet gone -- can be exercised without a live systemd
// unit, which is the state the lost-reply bug hid in and which no test could reach while this took a
// concrete *Guest. Both methods are nil-receiver-safe on the real type, so nothing changes for
// a caller holding a nil guest.
type guestStopper interface {
	Shutdown(ctx context.Context, grace time.Duration) error
	WaitStopped(ctx context.Context, grace time.Duration) error
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
func stopCleanly(ctx context.Context, g guestStopper, client *guestagent.Client, logf func(string, ...any)) error {
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
		// Only one question settles it: is the VM still there?
		//
		// A ZERO GRACE ANSWERS ONE INSTANT TOO EARLY. Stopping
		// is not instantaneous and it is not one event: QEMU unlinks its monitor socket the
		// moment it exits, while the unit stays active until systemd reaps it. Measured live on
		// a standby, socket gone at :25 and unit inactive at :28 -- so a glance inside that gap
		// says "still running" about a machine that has already stopped, and the reboot upgrade
		// was abandoned on a guest that had done exactly as it was told.
		//
		// reapGrace, not shutdownGrace, and the difference is the whole reason this stays cheap:
		// by here the machine has ALREADY had its chance to shut down -- either Shutdown could
		// not reach QEMU at all (it is gone; only the unit is outstanding) or it pressed the
		// button and waited a full grace. Neither leaves a guest shutdown still to run, so what
		// remains is systemd finishing with a dead process, which is seconds. A short, named
		// window keeps the worst case inside the budget rebootGuest was always sized for
		// (BringUpBudget + 3*shutdownGrace) instead of spending a third grace here.
		if g.WaitStopped(ctx, reapGrace) == nil {
			logf("guest-stop: the guest was already down — the request landed, its reply did not")
			return nil
		}
		return errors.Join(perr, err)
	}
	return nil
}

// IMAGE-LEVEL OS UPGRADE. The guest chain's release is an IMAGE, and moving the node
// to it is the reboot method with the file swap where the snapshot used to be: stop cleanly,
// rename the image the guest boots (the old one kept beside it), bring the guest up on the new
// one, prove the booted closure is the one the release's signed manifest names, health-gate,
// then drop the old image or put it back. No generation, no boot selector, no in-guest step:
// the guest is an appliance and the host holds the file.
//
// It coexists with the closure path (Upgrade / RebootUpgrade) until the closure path is
// retired together with the lab and cloud demos that still drive closures; a node with a
// cloud sees the closure directive, an OSS node the image path.

// nextImage / prevImage name the staged and the superseded image beside the one in use. The
// staged one is written by the update (decompressed, so the swap is a rename); the previous one
// is kept until the gate passes, and is what a restore renames back.
func nextImage(backing string) string { return backing + ".next" }
func prevImage(backing string) string { return backing + ".prev" }

// ImageUpgrade moves THIS node to the guest release rel, whose image is already staged at
// nextImage(backing) by the caller. rolledBack says where the node ended up, err what went
// wrong -- the same three outcomes as RebootUpgrade, for the same reasons.
func (u *osUpgrade) ImageUpgrade(ctx context.Context, rel install.Manifest) (rolledBack bool, err error) {
	mgr := u.Manager
	qspec := u.cfg.guestSpec()
	if rel.System == "" {
		return true, fmt.Errorf("image-upgrade: release %s names no system closure; nothing to prove the boot against", rel.Version)
	}
	// The same refusal as the reboot method, for the same reason: a serving node with a
	// takeover-capable peer is a failover to schedule, not a node's decision about itself.
	cl, err := u.client.Cluster(ctx, u.cfg.Resource.Name)
	if err != nil {
		return true, fmt.Errorf("read cluster before image upgrade: %w", err)
	}
	if cl.Serving() && cl.PeerCanTakeOver() {
		return true, fmt.Errorf("%w (peers: %s)", ErrHandoverRequired, describePeers(cl))
	}
	// A serving node past that refusal is the LONE holder of the house: nobody could take the
	// work, so after the reboot nobody else can be serving it. That node's gate therefore has
	// to see it SERVING again -- Primary with its front door answering -- where a standby's
	// gate is satisfied by the job it has (guest.OSReadyServing says what this closes).
	mustServe := cl.Serving()
	backing := u.cfg.GuestImage
	if backing == "" {
		return true, errors.New("image-upgrade: no GUEST_IMAGE configured; this node's launch does not name the image it boots")
	}
	if _, err := os.Stat(nextImage(backing)); err != nil {
		return true, fmt.Errorf("image-upgrade: no staged image at %s: %w", nextImage(backing), err)
	}
	prev, err := mgr.SystemPath(ctx)
	if err != nil {
		return true, fmt.Errorf("read current system: %w", err)
	}
	u.logf("image-upgrade: %s (%s) -> %s (%s)", prev, backing, rel.Version, rel.System)
	if e := mgr.EnterMaintenance(ctx); e != nil {
		return true, fmt.Errorf("enter maintenance: %w", e)
	}
	if e := stopCleanly(ctx, u.vm, u.client, u.logf); e != nil {
		if re := u.resume(ctx); re != nil {
			return false, fmt.Errorf("clean shutdown refused AND could not restore the node: %w", errors.Join(e, re))
		}
		return true, fmt.Errorf("clean shutdown refused, node left running %s: %w", prev, e)
	}
	// THE SWAP: two renames on one filesystem. The previous image stays until the gate passes,
	// so a restore is the same two renames the other way.
	if e := os.Remove(prevImage(backing)); e != nil && !os.IsNotExist(e) {
		return u.restoreImage(ctx, qspec, backing, prev, fmt.Errorf("clear %s: %w", prevImage(backing), e))
	}
	if e := os.Rename(backing, prevImage(backing)); e != nil {
		return u.restoreImage(ctx, qspec, backing, prev, fmt.Errorf("set aside the running image: %w", e))
	}
	if e := os.Rename(nextImage(backing), backing); e != nil {
		return u.restoreImage(ctx, qspec, backing, prev, fmt.Errorf("place the new image: %w", e))
	}
	// The swap is flushed before the guest boots it, so the image a power cut leaves in place is
	// the one that booted.
	if e := atomicfile.SyncDir(filepath.Dir(backing)); e != nil {
		return u.restoreImage(ctx, qspec, backing, prev, fmt.Errorf("flush the image swap: %w", e))
	}
	g, client, e := u.cfg.bringUp(ctx, qspec, u.logf)
	if e != nil {
		return u.restoreImage(ctx, qspec, backing, prev, fmt.Errorf("boot %s: %w", rel.Version, e))
	}
	u.vm = g
	u.rebind(client)
	mgr = u.Manager
	booted, e := mgr.SystemPath(ctx)
	switch {
	case e != nil:
		return u.restoreImage(ctx, qspec, backing, prev, fmt.Errorf("read booted system: %w", e))
	case booted != rel.System:
		// The image booted SOMETHING else than its manifest says it is: a mis-published
		// release, or a swap that did not take. Either way not the release we were asked for.
		return u.restoreImage(ctx, qspec, backing, prev,
			fmt.Errorf("booted %s, not %s's system %s", booted, rel.Version, rel.System))
	}
	if mustServe {
		u.logf("image-upgrade: booted %s, health-gating (it held the house alone, so it must serve again)", rel.Version)
		if e := mgr.AwaitOSReadyServing(ctx); e != nil {
			return u.restoreImage(ctx, qspec, backing, prev, e)
		}
	} else {
		u.logf("image-upgrade: booted %s, health-gating", rel.Version)
		if e := mgr.AwaitOSReady(ctx); e != nil {
			return u.restoreImage(ctx, qspec, backing, prev, e)
		}
	}
	if e := systemRunning(ctx, u.client, systemPollEvery); e != nil {
		return u.restoreImage(ctx, qspec, backing, prev, e)
	}
	if e := os.Remove(prevImage(backing)); e != nil {
		u.logf("image-upgrade: WARNING committed %s but could not drop the previous image %s: %v", rel.Version, prevImage(backing), e)
	}
	u.logf("image-upgrade: %s committed", rel.Version)
	return false, nil
}

// systemPollEvery paces systemRunning's wait for a guest still booting.
const systemPollEvery = 2 * time.Second

// systemStater is the one guest read systemRunning needs -- a narrow interface for DI, not a seam.
type systemStater interface {
	SystemState(ctx context.Context) (guestagent.SystemState, error)
}

// systemRunning is the OS gate's GENERIC check: did every unit the new image starts actually
// start? systemd's own `running` (no failed unit) is a question any guest can answer without
// knowing what it runs, and its failures -- a missing kernel module, a unit the new image broke --
// are things reverting the image repairs. The OS gate asks nothing service-specific: a release's
// fitness for a household's services is established before it ships, not judged per house.
//
// READ ONCE, after the node-local gate has passed -- never polled until it turns `running`. Our own
// recovery clears failed state (the promotion hold's `reset-failed` on the chain members, the firmware
// bin units' resets), so a poll could wait out the very evidence it is looking for. Only
// `starting`/`initializing` -- boot jobs still queued -- and a channel error are waited out; any
// other state is the answer. What it cannot see: a member still inside its restart budget (not yet
// `failed`, and covered for the storage chain by the gate before it), and a mount that came up but
// wrong (a read-only btrfs is a mounted filesystem, not a failed unit).
//
// ONE FAILED UNIT IS NOT THE IMAGE'S: drbd-reactor's promote unit (drbd-promote@<res>). The
// promoter tries to promote on every node that comes up, and on a standby -- a peer already
// Primary -- `drbdadm primary` refuses and the unit is left failed; that is the promoter losing
// the race it is meant to lose, and the reactor retries on its own. Whether the storage came up
// is the node-local gate's question, answered before this one.
//
// Each read runs on a context detached from ctx and bounded on its own, for AwaitReady's reason:
// a deadline landing inside a call would close the channel the restore then needs.
func systemRunning(ctx context.Context, g systemStater, every time.Duration) error {
	for {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		st, err := g.SystemState(rctx)
		cancel()
		if err == nil && st.State != "starting" && st.State != "initializing" {
			failed := slices.DeleteFunc(slices.Clone(st.Failed), func(u string) bool { return strings.HasPrefix(u, "drbd-promote@") })
			switch {
			case st.State == "running", st.State == "degraded" && len(st.Failed) > 0 && len(failed) == 0:
				return nil
			case len(failed) > 0:
				return fmt.Errorf("guest system is %s: failed units %s", st.State, strings.Join(failed, ", "))
			default:
				return fmt.Errorf("guest system is %q, not running", st.State)
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("guest system never settled (last %q, %v): %w", st.State, err, ctx.Err())
		case <-time.After(every):
		}
	}
}

// restoreImage puts the previous image back and boots it: the image path's restore(). Where
// the swap had not happened yet (the previous image is not set aside) it only boots the image
// still in use. The rejected image is removed -- the channel still has it, and a release that
// failed its gate is not something to keep a copy of.
func (u *osUpgrade) restoreImage(ctx context.Context, qspec platform.QEMUSpec, backing, prev string, cause error) (bool, error) {
	rb, cancel := context.WithTimeout(context.WithoutCancel(ctx), u.cfg.BringUpBudget+3*shutdownGrace)
	defer cancel()
	u.cfg.beat.Lease(rb)
	u.logf("image-upgrade: rolling back to %s (%v)", prev, cause)
	errs := []error{cause}
	if platform.Running(rb, qspec) {
		if e := stopCleanly(rb, u.vm, u.client, u.logf); e != nil {
			errs = append(errs, fmt.Errorf("rollback could not stop the guest cleanly, forcing (%w)", e))
			if e := u.vm.Stop(); e != nil {
				errs = append(errs, fmt.Errorf("rollback stop: %w", e))
			}
		} else {
			// Said positively: a clean stop is what lets DRBD record its quorum on the way out,
			// so the rebooted base can re-quorate with no successor; a rig asserts this
			// line rather than the absence of "forcing".
			u.logf("image-upgrade: rollback stopped the guest cleanly")
		}
	}
	if _, e := os.Stat(prevImage(backing)); e == nil {
		if e := os.Remove(backing); e != nil && !os.IsNotExist(e) {
			return false, fmt.Errorf("rollback FAILED, guest left stopped: %w", errors.Join(append(errs, e)...))
		}
		if e := os.Rename(prevImage(backing), backing); e != nil {
			return false, fmt.Errorf("rollback FAILED, guest left stopped: %w", errors.Join(append(errs, e)...))
		}
	}
	_ = os.Remove(nextImage(backing))
	if e := atomicfile.SyncDir(filepath.Dir(backing)); e != nil { // same reason as the forward swap
		return false, fmt.Errorf("rollback FAILED to flush the image swap, guest left stopped: %w", errors.Join(append(errs, e)...))
	}
	g, client, e := u.cfg.bringUp(rb, qspec, u.logf)
	if e != nil {
		return false, fmt.Errorf("rollback FAILED to boot %s: %w", prev, errors.Join(append(errs, e)...))
	}
	u.vm = g
	u.rebind(client)
	return true, fmt.Errorf("OS upgrade rolled back to %s: %w", prev, errors.Join(errs...))
}
