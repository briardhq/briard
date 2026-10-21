package host

import (
	"context"
	"errors"
	"os"
	"strings"

	"briard.io/agent/nic"
	"briard.io/agent/platform"
)

// What the Linux install made, and so what goes:
//
//   - the units: briard-agent, briard-update and its timer (install.sh), and the two transient
//     units the agent starts, the guest and the witness forwarder;
//   - the host's side of the guest's L2 (nic.Converge): the taps, and the address on a user's bridge;
//   - the files: /opt/briard, the `briard` link on $PATH, the tmpfs under /run, the guest console log;
//   - /var/lib/briard, the pet -- KEPT unless the operator asks for it to go.

// cliLink is where install.sh puts `briard` on $PATH: a symlink to the agent binary.
const cliLink = "/usr/local/bin/briard"

// deleteHint finishes the kept-data line with how to delete it by hand.
const deleteHint = "`sudo rm -rf %s` deletes them for good"

// uninstallUnits is every unit a node runs, in the order they must stop. The timer before the
// update it fires, the agent before its guest -- an agent alive when its guest stops relaunches it
// -- and the guest by its ExecStop, which is the clean powerdown the kept volume depends on.
var uninstallUnits = []string{
	"briard-update.timer",
	"briard-update.service",
	"briard-agent.service",
	platform.GuestUnit,
	platform.ForwarderUnit,
}

func (cfg Config) removeServices(context.Context, func(string, ...any)) error {
	for _, u := range uninstallUnits {
		if err := platform.RemoveUnit(u); err != nil {
			return err
		}
	}
	return platform.ReloadUnits()
}

func (cfg Config) removeNetwork(ctx context.Context, _ func(string, ...any)) error {
	return nic.Remove(ctx, cfg.uninstallSpec())
}

func (cfg Config) removeFiles(deleteData bool, _ func(string, ...any)) error {
	var errs []error
	remove := func(p string) {
		if err := os.RemoveAll(p); err != nil {
			errs = append(errs, err)
		}
	}
	for _, p := range []string{runDir, cfg.AdminPortSock} {
		remove(p)
	}
	if cfg.SerialLog != "" {
		remove(cfg.SerialLog)
		remove(cfg.SerialLog + ".prev")
	}
	if deleteData {
		remove(cfg.stateDir())
	}
	if t, err := os.Readlink(cliLink); err == nil && strings.HasPrefix(t, prefixDir+"/") {
		remove(cliLink)
	}
	remove(prefixDir)
	return errors.Join(errs...)
}

// uninstallSpec names what nic.Remove takes down: every tap this node's config names -- all three,
// whatever the substrate, since a node that moved onto a bridge may still hold the other two --
// and, on a bridge, the system-subnet address and the route to the guest the agent put on it.
// Those are derived exactly as the agent derives them (numbered from the recorded subnets), because
// they are on a device the user owns and only an exact match may be taken off it.
//
// nic.Select, not Choose: the probe validates a device something is about to be built on, and
// nothing is.
func (cfg Config) uninstallSpec() nic.Spec {
	s := nic.Spec{SystemTap: cfg.SystemTap, ServiceTap: cfg.ServiceTap, PrivTap: cfg.WitnessTap}
	if sel := nic.Select(cfg.NICOverride, ""); sel.Usable() && sel.Bridge {
		ns := cfg.applyDraw(cfg.recordedSubnets()).netSpec(sel.Dev, true, false)
		s.Addrs, s.Routes = ns.Addrs, ns.Routes
	}
	return s
}
