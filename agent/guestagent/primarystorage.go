package guestagent

import (
	"context"
	"fmt"
	"path/filepath"

	"briard.io/shared/nodestorage"
)

// THE FILESYSTEM HALF OF STORAGE, ON THE ONE NODE THAT PROMOTED ([V3b.33](d)).
//
// briard-primary-storage.service's ExecStart and ExecStop -- a promoter chain member, between the
// promotion and everything that needs the volume. Where briard-node-storage does block work on
// EVERY node, this does filesystem work only where the volume is mounted, and the cut between
// them is by scope rather than by tidiness.
//
// IT NEVER FORMATS ([B.145a]). The one-time format lives in briard-node-storage, in the same
// process and the same branch as the `lvcreate` that made the volume, so "this is brand new" is
// a fact known where it is acted on rather than carried here on a marker. What that removes is
// the shape [B.126] and [V3b.33](d) each had to guard: a destructive operation on a path that runs
// at every promotion, on every node, forever. There is no such operation on this path now.

// snapshotsDir is the pre-upgrade snapshot store, a sibling of the service data subvolumes rather
// than a child of one: it replicates with the volume, so it survives a failover.
func snapshotsDir() string { return filepath.Join(dataMountRoot, ".snapshots") }

// PrimaryStorage mounts the replicated device the node's storage spec names, and makes the
// snapshots directory.
//
// The device is READ OFF THE SPEC rather than restated as a constant here: the same document the
// block layer was built from says what sits on top of it, so the mount unit and the storage unit
// cannot disagree about which device is the volume.
func PrimaryStorage(ctx context.Context, x Executor) error {
	run := func(name string, args ...string) error {
		out, err := x.Run(ctx, name, args...)
		if err != nil {
			return fmt.Errorf("%s: %w: %s", name, err, out)
		}
		return nil
	}
	raw, err := x.ReadFile(nodestorage.Path)
	if err != nil {
		return fmt.Errorf("read %s: %w", nodestorage.Path, err)
	}
	spec, err := nodestorage.Parse(raw)
	if err != nil {
		return err
	}
	if err := run("mkdir", "-p", dataMountRoot); err != nil {
		return err
	}
	// MOUNT GUARDED, because this unit may be RETRIED ([B.125](b)): mounting an already-mounted
	// path stacks a second mount rather than failing, so the retry has to ask first.
	if _, err := x.Run(ctx, "mountpoint", "-q", dataMountRoot); err != nil {
		if err := run("mount", spec.Resource.Device, dataMountRoot); err != nil {
			return err
		}
	}
	// First use: the snapshots dir. Idempotent. A service's own data subvolume is created when
	// that service is INSTALLED (the guest's renderer makes it), so a node nobody has given a
	// workload to mounts an empty volume -- which is the honest state of one.
	return run("mkdir", "-p", snapshotsDir())
}

// PrimaryStorageStop unmounts the volume: briard-primary-storage.service's ExecStop, which
// drbd-reactor runs on demote before it hands the resource on.
func PrimaryStorageStop(ctx context.Context, x Executor) error {
	if out, err := x.Run(ctx, "umount", dataMountRoot); err != nil {
		return fmt.Errorf("umount %s: %w: %s", dataMountRoot, err, out)
	}
	return nil
}
