package host

import (
	"fmt"

	"briard.io/agent/platform"
)

// stateDiskSize is the node-local state disk's ceiling. Sparse, so this is a bound and
// not a charge against the report card's free-space floor -- the guest grows into it as it pulls
// service images, keeps a journal and records the deadman's backoff.
//
// A constant rather than a knob, for the reason the install layout makes most of them one: nothing ever set
// the installer's version of it, and a ceiling on a sparse file is not a decision a household has
// an opinion about.
const stateDiskSize = 8 << 30 // 8 GiB

// provisionDisks makes this node's pet volumes exist, once, before the guest is launched.
//
// ⚠️ IT PROVISIONS ONLY WHAT IT WAS TOLD ABOUT, and that predicate is doing real work rather than
// being defensive. An empty path is how a harness says "this node has no such disk", so a
// path invented here would hand qemu a `-drive` for a file nobody made. Same rule as everywhere else in the install layout: the installer
// decides the LAYOUT and writes the paths down; the agent makes what those paths name.
//
// The guest's OS disk is not here: it is the release's image, attached read-only.
func (cfg Config) provisionDisks(logf func(string, ...any)) error {
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
		if err := platform.AllocateSparse(cfg.StateDisk, stateDiskSize); err != nil {
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
