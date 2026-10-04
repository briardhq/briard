package host

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"briard.io/agent/platform"
	"briard.io/agent/reportcard"
)

// stateDiskInitial is the state disk's size when the agent makes it: THICK, paid for up front, and
// only as big as an empty node needs -- the journal (capped at 128 MB), the boot's scratch (the
// dressed binaries, ~30 MB) and the deadman's backoff, with room to spare. Everything a service
// brings is charged when it arrives: an install grows the disk first (growStateDisk), so the host
// pays at the operation that needs the space and never underneath a running guest.
const stateDiskInitial = reportcard.StateDiskInitialMB << 20 // the report card's line, so its floor and this agree

// stateDiskStep is what a grow rounds up to: whole GiB, so a node's disk sizes stay readable and
// small installs do not each cost a resize.
const stateDiskStep = 1 << 30

// hostDiskReserve is what a grow leaves free on the HOST's filesystem. The host's own OS keeps
// writing -- its logs, its updates, the next release staged beside the running one -- and a guest
// disk that took the last of it would move the failure out of the guest and into the machine.
const hostDiskReserve = reportcard.HostDiskReserveMB << 20 // the report card's line, likewise

// errHostDiskFull is a grow the host cannot pay for: the operation that needed it is refused, and
// the running service is untouched.
var errHostDiskFull = errors.New("not enough free space on this computer")

// stateDiskGrowth decides a grow: the size the state disk FILE must reach for the guest to have
// need bytes free on it, or 0 when it already has them. The range is padded by an eighth before
// rounding, because the guest is not handed the whole of it -- ext4 keeps 5 % of a grown range
// for root and a little more for its own tables, and a grow that came up a few MB short would
// pass here and fail at the gate it exists to satisfy. hostFree is 0 when the host's free space
// cannot be read; the allocation itself then refuses a host that cannot back it.
func stateDiskGrowth(guestFree, need, fileSize, hostFree int64) (int64, error) {
	if guestFree >= need {
		return 0, nil
	}
	short := need - guestFree
	grow := (short + short/8 + stateDiskStep - 1) / stateDiskStep * stateDiskStep
	if hostFree > 0 && hostFree < grow+hostDiskReserve {
		return 0, fmt.Errorf("%w: the guest's disk needs %s more and this computer has %s free (keeping %s for itself)",
			errHostDiskFull, gb(grow), gb(hostFree), gb(hostDiskReserve))
	}
	return fileSize + grow, nil
}

// stateGrower is what growStateDisk needs of the guest (guestagent.Client).
type stateGrower interface {
	StorageFree(ctx context.Context) (free, total int64, err error)
	StorageGrow(ctx context.Context, size int64) error
}

// growStateDisk grows this node's state disk until the guest has need bytes free on it: the thick
// file first (the host pays, or refuses with errHostDiskFull), then QEMU is told the new size,
// then the guest grows its filesystem into it -- all while the guest runs. It never shrinks: the
// disk is a cache, and shrinking it is recreating it at a relaunch.
func (cfg Config) growStateDisk(ctx context.Context, g stateGrower, resize func(context.Context, int64) error, need int64, logf func(string, ...any)) error {
	free, _, err := g.StorageFree(ctx)
	if err != nil {
		return fmt.Errorf("measure the guest's disk: %w", err)
	}
	fi, err := os.Stat(cfg.StateDisk)
	if err != nil {
		return err
	}
	size, err := stateDiskGrowth(free, need, fi.Size(), int64(reportcard.DiskFreeMB(filepath.Dir(cfg.StateDisk)))<<20)
	if err != nil || size == 0 {
		return err
	}
	if err := platform.ExtendThick(cfg.StateDisk, size); err != nil {
		return fmt.Errorf("%w: %v", errHostDiskFull, err)
	}
	if err := resize(ctx, size); err != nil {
		return err
	}
	if err := g.StorageGrow(ctx, size); err != nil {
		return err
	}
	logf("state disk: grown from %s to %s (the guest had %s free, needs %s)", gb(fi.Size()), gb(size), gb(free), gb(need))
	return nil
}

// provisionDisks makes this node's pet volumes exist, once, before the guest is launched.
//
// ⚠️ IT PROVISIONS ONLY WHAT IT WAS TOLD ABOUT, and that predicate is doing real work rather than
// being defensive. An empty path is how a harness says "this node has no such disk", so a
// path invented here would hand qemu a `-drive` for a file nobody made. Same rule as everywhere else in the install layout: the installer
// decides the LAYOUT and writes the paths down; the agent makes what those paths name.
//
// Both disks are THICK, and their directory is marked no-copy-on-write before either is made
// (platform.SetNoCOW): on a btrfs host a reservation does not cover a later overwrite otherwise.
// The guest's OS disk is not here: it is the release's image, attached read-only.
func (cfg Config) provisionDisks(logf func(string, ...any)) error {
	for _, p := range []string{cfg.DataDisk, cfg.StateDisk} {
		if p == "" {
			continue
		}
		if err := platform.SetNoCOW(filepath.Dir(p)); err != nil {
			logf("disks: %v; a copy-on-write host filesystem may still run this node out of space underneath its guest", err)
		}
	}
	if cfg.DataDisk != "" {
		size, err := parseSize(cfg.DataSize)
		if err != nil {
			return fmt.Errorf("host: this node's data volume size: %w", err)
		}
		if err := platform.AllocateThick(cfg.DataDisk, size); err != nil {
			return fmt.Errorf("host: %w", err)
		}
	}
	if cfg.StateDisk != "" {
		if err := platform.AllocateThick(cfg.StateDisk, stateDiskInitial); err != nil {
			return fmt.Errorf("host: %w", err)
		}
	}
	return nil
}

// parseSize reads the data volume's size: whole GiB, written the way the installer's knob always
// took it ("4G"). Deliberately narrow -- this is one value on one line of one file, and a parser
// that accepted "4.5GiB" would be inventing a vocabulary nobody asked for.
func parseSize(s string) (int64, error) {
	var n int64
	var unit byte
	if _, err := fmt.Sscanf(s, "%d%c", &n, &unit); err != nil || n <= 0 || (unit != 'G' && unit != 'g') {
		return 0, fmt.Errorf("%q is not a whole number of GiB (e.g. 4G)", s)
	}
	return n << 30, nil
}
