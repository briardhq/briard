package host

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"briard.io/agent/platform"
	"briard.io/agent/reportcard"
	"briard.io/shared/notify"
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

// diskAlerter tells the household when the host disk holding this node's disks runs short --
// BEFORE an operation is refused for it. Nothing the guest writes reaches the host any more (its
// OS is read-only and its disks thick), so a full host disk no longer stops the guest; what it
// stops is the next thing that needs room: an app install or upgrade (growStateDisk keeps
// hostDiskReserve) and the next update, which stages beside the running one. Without this, the
// first a household hears of it is that refusal.
//
// It asserts "low" under hostDiskReserve and "back" only above diskClearMB -- a margin, so a
// host hovering at the line does not flap; in between it says nothing, and the alert store
// keeps whichever it last said. An unreadable answer (0) changes nothing in either direction.
type diskAlerter struct {
	read func(path string) int // reportcard.DiskFreeMB in production: MB free, 0 if unreadable
	next time.Time
}

const (
	diskReadEvery = time.Minute
	diskClearMB   = reportcard.HostDiskReserveMB + 512
)

// observe reads the filesystem holding disk (the state disk: where the guest's disk grows).
func (a *diskAlerter) observe(ctx context.Context, n notify.Notifier, node, disk string, now time.Time, logf func(string, ...any)) {
	if disk == "" || now.Before(a.next) {
		return
	}
	path := filepath.Dir(disk)
	a.next = now.Add(diskReadEvery)
	free := a.read(path)
	switch {
	case free == 0:
		return
	case free < reportcard.HostDiskReserveMB:
		fireAlert(ctx, n, logf, notify.Alert{
			Key:      "disk",
			Kind:     notify.Open,
			Severity: notify.Warning,
			Title:    "Briard: this computer is running out of disk space",
			Body: fmt.Sprintf("node %s's computer has %s free on the disk holding its data (%s). Briard keeps about %s free "+
				"for the computer's own system, so installing or updating apps, and the next Briard update, will be "+
				"refused until there is more room. Your apps keep running. Free some space on that disk.",
				node, gb(int64(free)<<20), path, fmt.Sprintf("%d GB", reportcard.HostDiskReserveMB/1024)),
		})
	case free >= diskClearMB:
		fireAlert(ctx, n, logf, notify.Alert{
			Key:   "disk",
			Kind:  notify.Resolved,
			Title: "Briard: disk space is back",

			Body: fmt.Sprintf("node %s's computer has %s free again; installs and updates can go ahead.", node, gb(int64(free)<<20)),
		})
	}
}

// smartAlerter tells the household when a disk under this node's data says it is failing, or
// records new bad sectors -- the likeliest way a home machine dies, and otherwise the first sign
// is the failure itself.
//
// IT READS IN THE BACKGROUND, one read in flight at a time, and the loop takes the answer at a
// later tick: a disk behind a slow bridge can hold smartctl in a command timeout that no context
// can cut short, and the loop must keep beating through it.
//
// FAILING opens at Critical, and GROWTH opens at Warning: more recorded errors than the first
// read this agent made. A steady count opens nothing -- some disks run for years with a few
// remapped sectors -- and an agent restarted under an open growth alert says nothing, so the
// store keeps it. Only a disk reading clean (no verdict of failing, no errors) resolves: a
// replaced disk, in practice. AN UNKNOWN IS NOT AN ALARM: an unreadable disk, or one asleep,
// changes nothing.
type smartAlerter struct {
	disks func(path string) ([]string, error)                // reportcard.DataDisks in production
	read  func(context.Context, []string) []reportcard.SMART // reportcard.ReadSMART in production
	next  time.Time
	got   chan []reportcard.SMART // non-nil while a read is in flight
	base  map[string]int64        // per disk: the errors it had at this agent's first read of it
}

const smartReadEvery = time.Hour

// observe starts a read of the disks under disk (the data disk) when one is due, and judges one
// when it is done.
func (a *smartAlerter) observe(ctx context.Context, n notify.Notifier, node, disk string, now time.Time, logf func(string, ...any)) {
	if a.got != nil {
		select {
		case ss := <-a.got:
			a.got = nil
			a.judge(ctx, n, node, ss, logf)
		default:
		}
		return
	}
	if now.Before(a.next) {
		return
	}
	a.next = now.Add(smartReadEvery)
	devs, err := a.disks(filepath.Dir(disk))
	if err != nil || len(devs) == 0 {
		return
	}
	got := make(chan []reportcard.SMART, 1)
	a.got = got
	go func() { got <- a.read(ctx, devs) }()
}

func (a *smartAlerter) judge(ctx context.Context, n notify.Notifier, node string, ss []reportcard.SMART, logf func(string, ...any)) {
	if a.base == nil {
		a.base = map[string]int64{}
	}
	for _, s := range ss {
		if !s.Read {
			continue
		}
		base, seen := a.base[s.Device]
		if !seen {
			a.base[s.Device], base = s.Errors, s.Errors
		}
		key := "disk-health:" + filepath.Base(s.Device)
		switch {
		case s.Failing:
			fireAlert(ctx, n, logf, notify.Alert{
				Key:      key,
				Kind:     notify.Open,
				Severity: notify.Critical,
				Title:    "Briard: a disk is failing",
				Body: fmt.Sprintf("disk %s on node %s reports it is failing (%s). Your data lives on it: replace it soon.",
					s.Device, node, s.Why),
			})
		case s.Errors > base:
			fireAlert(ctx, n, logf, notify.Alert{
				Key:      key,
				Kind:     notify.Open,
				Severity: notify.Warning,
				Title:    "Briard: a disk is recording errors",
				Body: fmt.Sprintf("disk %s on node %s has recorded %d bad sectors or media errors, up from %d. A disk whose "+
					"count keeps growing is wearing out: plan to replace it.", s.Device, node, s.Errors, base),
			})
		case s.Errors == 0:
			fireAlert(ctx, n, logf, notify.Alert{
				Key:   key,
				Kind:  notify.Resolved,
				Title: "Briard: the disk reports no warnings",
				Body:  fmt.Sprintf("disk %s on node %s reports no warnings in its health report.", s.Device, node),
			})
		}
	}
}
