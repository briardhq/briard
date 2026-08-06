package platform

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

// Snapshots of the guest's OS disk -- the rollback point the reboot half of an OS upgrade
// restores to. These are qcow2 INTERNAL snapshots, taken with the VM stopped.
//
// WHAT THIS COVERS, AND WHY IT IS NOT REDUNDANT. NixOS generations roll back *code*;'s
// btrfs snapshot rolls back the payload's *data*. Neither covers mutable state on the guest
// OS disk -- /var/lib/nixos, /var/lib/containers, systemd state, anything a stateVersion
// migration touches. That gap is this snapshot's whole justification: cheap insurance against
// the upgrades that corrupt state OUTSIDE the closure, where a generation rollback would
// leave old code running against migrated state. It covers the OS disk ONLY; the replicated
// data volume is deliberately untouched, since conflating them would let an OS rollback
// silently revert user data.
//
// OFFLINE, deliberately. A live snapshot is crash-consistent -- open files, a journal
// awaiting replay -- so restoring one is restoring from a power cut, precisely the failure
// this exists to insure against. The reboot path stops the guest anyway (Guest.Shutdown), so
// it can afford a clean, quiesced snapshot and takes one. Every function here therefore
// requires the VM to be stopped; qemu-img refuses to write an image QEMU still has open.
//
// INTERNAL, not external. An external snapshot makes a NEW file the active layer, so "which
// file is live" becomes state the agent must carry across restarts -- bookkeeping the transient-service design exists to avoid. Internal snapshots live inside the same qcow2, so the disk
// path never changes. Two properties fall out, both load-bearing: it costs metadata rather
// than data (measured: creating one on a fully-allocated 2.5 GB image is ~0.5 s and does not
// grow the file), and the snapshot is visible offline via `qemu-img snapshot -l`, which is
// what makes an in-flight upgrade SELF-DESCRIBING -- an agent that restarts mid-upgrade
// discovers the state by inspecting the qcow2, with no marker file or journal to keep true.
// That last property is why the tag below is a fixed constant rather than a caller's choice.
//
// Verified against the pinned qemu on the shape production actually uses -- a qcow2 overlay
// with a read-only backing file: the snapshot is taken in the overlay, a revert restores the
// pre-snapshot contents, the backing chain survives, and reverting a tag that does not exist
// exits non-zero rather than silently succeeding.

// UpgradeSnapshot is the tag the OS-upgrade rollback point is stored under. Fixed, not
// caller-supplied: a recognisable name is what lets `qemu-img snapshot -l` answer "was an
// upgrade in flight when this agent died?" without any state kept beside the disk.
const UpgradeSnapshot = "briard-preupgrade"

// qemuImg locates the qemu-img binary beside the configured qemu-system binary. Both the Nix
// package and the relocatable bundle ship them in the same bin/ directory, so deriving
// the path keeps the pair consistent -- picking qemu-img off $PATH could pair a bundled qemu
// with a distro qemu-img of a different vintage. A bare Binary (found on $PATH itself) falls
// back to the same treatment for qemu-img.
func (s QEMUSpec) qemuImg() string {
	dir := filepath.Dir(s.Binary)
	if dir == "" || dir == "." {
		return "qemu-img"
	}
	return filepath.Join(dir, "qemu-img")
}

// SnapshotCmd runs one `qemu-img snapshot` verb against the guest's OS disk.
func (s QEMUSpec) snapshotCmd(ctx context.Context, flag, tag string) error {
	if s.DiskImage == "" {
		return fmt.Errorf("platform: no guest disk configured to snapshot")
	}
	out, err := exec.CommandContext(ctx, s.qemuImg(), "snapshot", flag, tag, s.DiskImage).CombinedOutput()
	if err != nil {
		return fmt.Errorf("platform: qemu-img snapshot %s %s %s: %w: %s",
			flag, tag, s.DiskImage, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// SnapshotCreate takes the pre-upgrade rollback point. The guest MUST be stopped: qemu-img
// refuses an image QEMU holds open, so a caller that forgets gets an error rather than a
// crash-consistent snapshot it would later mistake for a clean one.
func (s QEMUSpec) SnapshotCreate(ctx context.Context) error {
	return s.snapshotCmd(ctx, "-c", UpgradeSnapshot)
}

// SnapshotRestore rolls the OS disk back to the point SnapshotCreate took. This is the whole
// rollback of the reboot path: the staged closure is still in the restored store (so the
// retry costs no re-download), and the boot selector was never written to the disk, so the
// next launch simply does not pass it and comes up on the old generation.
func (s QEMUSpec) SnapshotRestore(ctx context.Context) error {
	return s.snapshotCmd(ctx, "-a", UpgradeSnapshot)
}

// SnapshotDelete drops the rollback point once the upgrade has been committed. Deleting it
// is what ends the upgrade window, so it is also what makes SnapshotExists a truthful answer
// to "is an upgrade in flight?".
func (s QEMUSpec) SnapshotDelete(ctx context.Context) error {
	return s.snapshotCmd(ctx, "-d", UpgradeSnapshot)
}

// SnapshotExists reports whether the rollback point is present -- i.e. whether an upgrade was
// in flight. An agent that restarts mid-upgrade asks the disk this instead of consulting a
// marker it would have had to keep in sync with reality.
func (s QEMUSpec) SnapshotExists(ctx context.Context) (bool, error) {
	if s.DiskImage == "" {
		return false, fmt.Errorf("platform: no guest disk configured to snapshot")
	}
	out, err := exec.CommandContext(ctx, s.qemuImg(), "snapshot", "-l", s.DiskImage).CombinedOutput()
	if err != nil {
		return false, fmt.Errorf("platform: qemu-img snapshot -l %s: %w: %s",
			s.DiskImage, err, strings.TrimSpace(string(out)))
	}
	return hasSnapshotTag(string(out), UpgradeSnapshot), nil
}

// hasSnapshotTag parses `qemu-img snapshot -l` output for a tag. The listing is a header line
// ("Snapshot list:"), a column header, then one row per snapshot: ID, TAG, VM_SIZE, DATE... .
// Matching the TAG *column* rather than substring-searching the whole blob keeps a tag from
// being found in, say, a date or another snapshot's name. An image with no snapshots prints
// nothing at all, which is the common case and correctly reads as absent.
func hasSnapshotTag(listing, tag string) bool {
	for line := range strings.SplitSeq(listing, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[1] == tag {
			return true
		}
	}
	return false
}
