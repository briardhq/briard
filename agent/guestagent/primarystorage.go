package guestagent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

// THE FILESYSTEM HALF OF STORAGE, ON THE ONE NODE THAT PROMOTED ([V3b.33](d)).
//
// briard-primary-storage.service's ExecStart and ExecStop -- a promoter chain member, between the
// promotion and everything that needs the volume. Where briard-node-storage does block work on
// EVERY node, this does filesystem work only where the volume is mounted, and the cut between
// them is by scope rather than by tidiness.
//
// IT IS THE UNIT FORMERLY SPELLED IN SHELL, and moving it into Go deletes a hazard that unit's
// own comment records: `[ -e $X ]` with an empty X is a ONE-argument test on the string "-e",
// which is always TRUE -- so a lost interpolation did not mean "never format", it meant "format
// on EVERY promotion, on every node". Measured, by doing it: an editing slip emptied the path,
// drbd-failover went red, and the survivor had reformatted the replicated volume mid-failover. In
// Go that failure mode does not exist, and the decision becomes unit-testable, which for a
// destructive act it was not.

// drbdDevice is the replicated device this mounts. One resource, one volume, one device: the
// number is a product constant, restated in the guest image's `.res` and in the host's config
// default the way the /run paths are.
const drbdDevice = "/dev/drbd0"

// snapshotsDir is the pre-upgrade snapshot store, a sibling of the service data subvolumes rather
// than a child of one: it replicates with the volume, so it survives a failover.
func snapshotsDir() string { return filepath.Join(dataMountRoot, ".snapshots") }

// PrimaryStorage formats the volume if -- and only if -- storage bring-up armed the one-time
// format on this boot, mounts it, and makes the snapshots directory.
//
// THE FORMAT IS THE WHOLE RISK HERE and every guard on it is deliberate. The marker is written by
// briard-node-storage only when this node is the seed of a NEW flock and the metadata was created
// by that same run; it lives on tmpfs, so it cannot outlive the boot that created the volume,
// which is what makes "a reboot can never format" a property of the filesystem rather than of our
// care. It is CONSUMED FIRST, so a format that fails is not retried. And `-f` is right here,
// where the installer has just claimed the disk for a brand-new flock and overwriting a previous
// life is the intent -- the bug [B.126] fixed was never the flag, it was a destructive operation
// on a path that runs at every promotion forever.
func PrimaryStorage(ctx context.Context, x Executor) error {
	run := func(name string, args ...string) error {
		out, err := x.Run(ctx, name, args...)
		if err != nil {
			return fmt.Errorf("%s: %w: %s", name, err, out)
		}
		return nil
	}
	if err := run("mkdir", "-p", dataMountRoot); err != nil {
		return err
	}
	if _, err := x.ReadFile(dataFormatMarker); err == nil {
		if err := run("rm", "-f", dataFormatMarker); err != nil {
			return fmt.Errorf("consume %s: %w", dataFormatMarker, err)
		}
		if err := run("mkfs.btrfs", "-f", drbdDevice); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		// A marker we could not READ is not a marker that is absent. Refusing here is the same
		// direction the rest of this file takes: an unformatted volume fails the mount below,
		// fails the chain and demotes the node -- loud and recoverable, which "silently
		// reformatted" is not.
		return fmt.Errorf("read %s: %w", dataFormatMarker, err)
	}
	// MOUNT GUARDED, because this unit may be RETRIED ([B.125](b)): mounting an already-mounted
	// path stacks a second mount rather than failing, so the retry has to ask first.
	if _, err := x.Run(ctx, "mountpoint", "-q", dataMountRoot); err != nil {
		if err := run("mount", drbdDevice, dataMountRoot); err != nil {
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
