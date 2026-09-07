package platform

import (
	"path/filepath"
)

// Snapshots of the guest's OS disk -- the rollback point the reboot half of an OS upgrade
// restores to. These are qcow2 INTERNAL snapshots, taken with the VM stopped.
//
// WHAT THIS COVERS, AND WHY IT IS NOT REDUNDANT. NixOS generations roll back *code*;'s
// btrfs snapshot rolls back a service's *data*. Neither covers mutable state on the guest
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
