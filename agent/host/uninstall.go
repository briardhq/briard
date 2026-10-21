// `briard uninstall`: take briard off this machine and leave the host the way its installer
// found it.
//
// THE INSTALL IS THE SPEC. Everything here undoes one thing that the installer or the agent made,
// and nothing else; each OS's arm (uninstall_linux.go, uninstall_windows.go) lists what that is
// there. The pet -- this node's state directory, the data volume and the identity it is keyed to --
// is KEPT unless the operator asks for it to go.
//
// KEEP MEANS THE WHOLE PET DIR, not the data file alone. The volume is keyed to this node's
// identity, and its address and name are records beside it, so a data.img without them is a file
// nothing can open -- while the whole dir is exactly what a reinstall resumes, the same way the
// cattle/pet reinstall does (install-macvtap). Deleting it is the one destructive act here, and it
// is gated on the explicit flag the CLI asks for, never implied.
//
// ORDER IS THE SAFETY. Services first, and a service that will not stop ends the uninstall there:
// nothing is deleted from under a guest that is still running. The install tree goes last, because
// it holds this very binary -- an uninstall that stopped half way can be run again.
package host

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
)

// Uninstall removes briard from this machine; deleteData also removes its pet state, the data
// volume included. It returns the first thing it could not do, having said what it did.
func (cfg Config) Uninstall(ctx context.Context, deleteData bool, logf func(string, ...any)) error {
	if err := cfg.removeServices(ctx, logf); err != nil {
		return fmt.Errorf("%w -- nothing else was removed; fix that and run this again", err)
	}
	logf("stopped and removed briard's services")

	var errs []error
	if err := cfg.removeNetwork(ctx, logf); err != nil {
		errs = append(errs, err)
	} else {
		logf("removed briard's network devices")
	}
	// Absolute and not a root: this is a recursive delete of a path read out of a config.
	dir := cfg.stateDir()
	del := deleteData && filepath.IsAbs(dir) && filepath.Dir(dir) != dir
	errs = append(errs, cfg.removeFiles(del, logf))
	if del {
		logf("deleted %s: this machine's data volume and identity", dir)
	} else if dir != "" {
		logf("kept %s: your data and this machine's identity. Installing again picks them up; "+
			deleteHint, dir, dir)
	}
	if err := errors.Join(errs...); err != nil {
		return err
	}
	logf("briard is uninstalled")
	return nil
}
