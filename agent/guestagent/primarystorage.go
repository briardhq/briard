package guestagent

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"briard.io/shared/nodestorage"
)

// THE FILESYSTEM HALF OF STORAGE, ON THE ONE NODE THAT PROMOTED.
//
// briard-primary-storage.service's ExecStart and ExecStop -- a promoter chain member, between the
// promotion and everything that needs the volume. Where briard-node-storage does block work on
// EVERY node, this does filesystem work only where the volume is mounted, and the cut between
// them is by scope rather than by tidiness.
//
// IT NEVER FORMATS. The one-time format lives in briard-node-storage, in the same
// process and the same branch as the `lvcreate` that made the volume, so "this is brand new" is
// a fact known where it is acted on rather than carried here on a marker. What that removes is
// the shape the blank probe and the storage spec each had to guard: a destructive operation on a path that runs
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
	// MOUNT GUARDED, because this unit may be RETRIED: mounting an already-mounted
	// path stacks a second mount rather than failing, so the retry has to ask first.
	//
	// COMPRESSED (zstd, level 1): the volume is thick and has no resize path, so what fits in it is
	// what compression buys -- the recorder's SQLite pages above all, the one thing that grows in a
	// house -- and, sitting above DRBD, it also cuts the bytes a flock replicates. Level 1 because a
	// Pi is the floor. A mount option, so it applies to what is written from now on and costs no
	// migration; files marked nodatacow are not compressed (btrfs's rule).
	if _, err := x.Run(ctx, "mountpoint", "-q", dataMountRoot); err != nil {
		if err := run("mount", "-o", "compress=zstd:1", spec.Resource.Device, dataMountRoot); err != nil {
			return err
		}
	}
	// SIMPLE QUOTAS, OR NO VOLUME. Every subvolume is born into the quota tree (qgroup.go), which
	// needs the mode the format set, so a volume without it is refused rather than converted:
	// enabling quotas on a populated volume leaves what is already there owned by nobody. Read
	// positively -- the mode file names the mode, and a volume with quotas off has no such file --
	// and unmounted on refusal, so the demote that follows a failed chain finds the device free.
	if mode, err := quotaMode(ctx, x); err != nil || mode != "squota" {
		_, _ = x.Run(ctx, "umount", dataMountRoot)
		return fmt.Errorf("the volume is not in simple-quota mode (mode %q, %v): it was formatted by an older Briard and needs a reinstall", mode, err)
	}
	// First use: the snapshots dir. Idempotent. A service's own data subvolume is created when
	// that service is INSTALLED (the guest's renderer makes it), so a node nobody has given a
	// workload to mounts an empty volume -- which is the honest state of one.
	return run("mkdir", "-p", snapshotsDir())
}

// quotaMode is the mounted volume's quota mode as the kernel states it: `squota`, `qgroup`, or an
// error when quotas are off (the qgroups directory does not exist then).
func quotaMode(ctx context.Context, x Executor) (string, error) {
	out, err := x.Run(ctx, "findmnt", "-no", "UUID", dataMountRoot)
	if err != nil {
		return "", fmt.Errorf("findmnt %s: %w: %s", dataMountRoot, err, out)
	}
	uuid := strings.TrimSpace(string(out))
	if uuid == "" || strings.ContainsAny(uuid, "/\n") {
		return "", fmt.Errorf("findmnt %s gave no filesystem uuid: %q", dataMountRoot, out)
	}
	raw, err := x.ReadFile("/sys/fs/btrfs/" + uuid + "/qgroups/mode")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(raw)), nil
}

// PrimaryStorageStop unmounts the volume: briard-primary-storage.service's ExecStop, which
// drbd-reactor runs on demote before it hands the resource on.
func PrimaryStorageStop(ctx context.Context, x Executor) error {
	if out, err := x.Run(ctx, "umount", dataMountRoot); err != nil {
		return fmt.Errorf("umount %s: %w: %s", dataMountRoot, err, out)
	}
	return nil
}
