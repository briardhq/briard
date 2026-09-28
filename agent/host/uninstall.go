// `briard uninstall` ([V3c.2]): take briard off this machine and leave the host the way install.sh
// found it.
//
// THE INSTALL IS THE SPEC. Everything here undoes one thing that install.sh or the agent made, and
// nothing else:
//
//   - the units: briard-agent, briard-update and its timer (install.sh), and the two transient
//     units the agent starts, the guest and the witness forwarder;
//   - the host's side of the guest's L2 (nic.Converge): the taps, and the address on a user's bridge;
//   - the files: /opt/briard, the `briard` link on $PATH, the tmpfs under /run, the guest console log;
//   - /var/lib/briard, the pet -- KEPT unless the operator asks for it to go.
//
// KEEP MEANS THE WHOLE PET DIR, not the data file alone. The volume is keyed to this node's
// identity, and its address and name are records beside it, so a data.img without them is a file
// nothing can open -- while the whole dir is exactly what a reinstall resumes, the same way the
// cattle/pet reinstall does (install-macvtap). Deleting it is the one destructive act here, and it
// is gated on the explicit flag the CLI asks for, never implied (AGENTS §4.9).
//
// ORDER IS THE SAFETY. Units first, and a unit that will not stop ends the uninstall there: nothing
// is deleted from under a guest that is still running. /opt/briard goes last, because it holds this
// very binary -- an uninstall that stopped half way can be run again.
package host

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"briard.io/agent/nic"
	"briard.io/agent/platform"
)

// cliLink is where install.sh puts `briard` on $PATH: a symlink to the agent binary.
const cliLink = "/usr/local/bin/briard"

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

// Uninstall removes briard from this machine; deleteData also removes its pet state, the data
// volume included. It returns the first thing it could not do, having said what it did.
func (cfg Config) Uninstall(ctx context.Context, deleteData bool, logf func(string, ...any)) error {
	for _, u := range uninstallUnits {
		if err := platform.RemoveUnit(u); err != nil {
			return fmt.Errorf("%w -- nothing else was removed; fix that and run this again", err)
		}
	}
	if err := platform.ReloadUnits(); err != nil {
		return err
	}
	logf("stopped and removed briard's services")

	var errs []error
	if err := nic.Remove(ctx, cfg.uninstallSpec()); err != nil {
		errs = append(errs, err)
	} else {
		logf("removed briard's network devices")
	}

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
	// Absolute and not the root: this is a recursive delete of a path read out of a config.
	if dir := cfg.stateDir(); deleteData && filepath.IsAbs(dir) && dir != "/" {
		remove(dir)
		logf("deleted %s: this machine's data volume and identity", dir)
	} else if dir != "" {
		logf("kept %s: your data and this machine's identity. Installing again picks them up; "+
			"`sudo rm -rf %s` deletes them for good", dir, dir)
	}
	if t, err := os.Readlink(cliLink); err == nil && strings.HasPrefix(t, prefixDir+"/") {
		remove(cliLink)
	}
	remove(prefixDir)
	if err := errors.Join(errs...); err != nil {
		return err
	}
	logf("briard is uninstalled")
	return nil
}

// uninstallSpec names what nic.Remove takes down: every tap this node's config names -- all three,
// whatever the substrate, since a node that moved onto a bridge may still hold the other two --
// and, on a bridge, the system-subnet address the agent put on it. That one is derived exactly as
// the agent derives it (numbered from the recorded subnets, re-prefixed for the bridge), because
// it is on a device the user owns and only an exact match may be taken off it.
//
// nic.Select, not Choose: the probe validates a device something is about to be built on, and
// nothing is.
func (cfg Config) uninstallSpec() nic.Spec {
	s := nic.Spec{SystemTap: cfg.SystemTap, ServiceTap: cfg.ServiceTap, PrivTap: cfg.WitnessTap}
	if sel := nic.Select(cfg.NICOverride); sel.Usable() && sel.Bridge {
		s.Addrs = cfg.applyDraw(cfg.recordedSubnets()).netSpec(sel.Dev, true).Addrs
	}
	return s
}
