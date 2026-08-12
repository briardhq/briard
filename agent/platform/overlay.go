package platform

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// REBUILDING THE GUEST'S OS DISK FROM THE IMAGE UNDER IT.
//
// The guest disk shipped by install.sh is a qcow2 OVERLAY on a read-only backing file -- the
// Ed25519-verified `nixos.qcow2` that arrived with the signed artifact set. Everything the guest
// has written since install lives in the overlay; nothing does in the backing file. So discarding
// the overlay and making a fresh one is a complete reinstall of the guest's CODE half, in about a
// second, with the replicated DATA disk untouched and the node's identity (which lives on that
// disk) intact.
//
// That is the whole of B.10's Tier 3 -- and it is deliberately NOT wired to any reflex. Failures
// during an OS upgrade are common and their cause is known, which is why rollback there is
// automatic. Failures during normal operation are rare and of unknown cause, and this is a drastic
// remedy with an uncertain result: the rebuilt guest must re-pull its OCI images over the WAN at
// exactly the moment something is already wrong. As a considered action after someone has read the
// logs, it is the right tool. As an automatic response to an unknown fault it is a coin flip that
// can deepen the outage.
//
// THE SAFETY GATE IS THE BACKING FILE, and it is the reason this is safe to expose at all. A disk
// with no backing file is not an overlay: it is the only copy of itself, and deleting it destroys
// the guest with nothing to rebuild from. So the backing file is read off the DISK rather than
// from config -- the same "ask reality rather than the caller" the rollback leg uses -- and its
// absence is a refusal, not a warning.
//
// The old overlay is DELETED rather than kept aside. Keeping it would double the worst-case disk
// use of a node that may well be broken *because* its disk filled, and the forensic material is
// already somewhere safer: the guest's serial console is captured on the HOST
// (`GUEST_SERIAL`, what `briard logs` reads), outside the file being discarded.

// infoArgs renders the `qemu-img info` argv. Pure, so the one flag that is not obvious can be
// asserted without a running VM -- the same reason qemuArgs is pure and unit-tested.
//
// --force-share is that flag, and it is load-bearing. This query is made while the GUEST IS STILL
// RUNNING: the backing-file check has to refuse BEFORE anything is stopped, or "this disk cannot
// be rebuilt" arrives after the VM is already down and becomes an outage instead of a refusal.
// QEMU holds a write lock on its disk, and qemu-img declines a locked image unless told the access
// is shared -- so without this, reading the backing file fails on every live node, which is every
// node anyone would ever ask about. Reading an image header is read-only, so sharing is honest.
//
// It cost a full runner cycle to find, because the unit tests below inspect STOPPED images and a
// stopped image is not locked -- the one way the test environment differed from production was the
// one that mattered.
func infoArgs(disk string) []string {
	return []string{"info", "--output=json", "--force-share", disk}
}

// diskInfo is the subset of `qemu-img info --output=json` this needs.
type diskInfo struct {
	Format          string `json:"format"`
	BackingFilename string `json:"backing-filename"`
	// The backing path resolved against the overlay's own directory. qemu-img reports both because
	// a relative backing reference is legal; recreating from the resolved one keeps a rebuilt
	// overlay pointing at the same image even if the two are not siblings.
	FullBackingFilename string `json:"full-backing-filename"`
}

// BackingFile reports the image this guest disk is an overlay of, and "" when it is not an
// overlay at all. Callers MUST treat "" as a refusal to rebuild: there is nothing to rebuild from.
func (s QEMUSpec) BackingFile(ctx context.Context) (string, error) {
	if s.DiskImage == "" {
		return "", fmt.Errorf("platform: no guest disk configured")
	}
	cmd := exec.CommandContext(ctx, s.qemuImg(), infoArgs(s.DiskImage)...)
	out, err := cmd.Output()
	if err != nil {
		// qemu-img says WHY on stderr ("Failed to get shared lock", "Could not open") and Output()
		// discards it, which cost a whole test run reporting only "exit status 1".
		var detail string
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			detail = ": " + strings.TrimSpace(string(ee.Stderr))
		}
		return "", fmt.Errorf("platform: qemu-img info %s: %w%s", s.DiskImage, err, detail)
	}
	var info diskInfo
	if err := json.Unmarshal(out, &info); err != nil {
		return "", fmt.Errorf("platform: parse qemu-img info for %s: %w", s.DiskImage, err)
	}
	if info.FullBackingFilename != "" {
		return info.FullBackingFilename, nil
	}
	return info.BackingFilename, nil
}

// RebuildOverlay discards the guest's OS disk and lays down a fresh overlay on the same backing
// image. The VM must be STOPPED -- qemu-img refuses a file QEMU still has open, and a rebuild
// under a running guest would be a corruption rather than a repair.
//
// It re-reads the backing file itself rather than taking it as an argument, so the check and the
// act cannot disagree about which disk is being rebuilt: a caller that probed a minute ago and
// passed the answer down would rebuild from whatever it was told, including "".
//
// The data disk is not named here, not passed here, and not reachable from here. That is the
// separation the whole verb rests on, and it is kept by the signature rather than by a comment.
func (s QEMUSpec) RebuildOverlay(ctx context.Context) (backing string, err error) {
	backing, err = s.BackingFile(ctx)
	if err != nil {
		return "", err
	}
	if backing == "" {
		return "", fmt.Errorf("platform: %s has no backing image -- it is not an overlay, so "+
			"rebuilding it would destroy the only copy of this guest", s.DiskImage)
	}
	if _, err := os.Stat(backing); err != nil {
		return "", fmt.Errorf("platform: backing image %s is not readable (%w) -- refusing to "+
			"discard the overlay it would have to be rebuilt from", backing, err)
	}
	if Running(ctx, s) {
		return "", fmt.Errorf("platform: the guest is still running; stop it before rebuilding %s", s.DiskImage)
	}

	// Remove first: qemu-img create on an existing path is not the same operation as create on a
	// fresh one, and install.sh's own overlay creation removes first for the same reason.
	if err := os.Remove(s.DiskImage); err != nil && !os.IsNotExist(err) {
		return "", fmt.Errorf("platform: discard %s: %w", s.DiskImage, err)
	}
	out, err := exec.CommandContext(ctx, s.qemuImg(), "create", "-f", "qcow2",
		"-b", backing, "-F", "qcow2", s.DiskImage).CombinedOutput()
	if err != nil {
		// The overlay is gone and could not be replaced: say so plainly, because this is the one
		// state this function can leave behind that a human has to fix.
		return "", fmt.Errorf("platform: recreate %s on %s: %w: %s -- the old overlay has already "+
			"been discarded, so this node needs its disk laid down again (reinstall)",
			s.DiskImage, backing, err, strings.TrimSpace(string(out)))
	}
	return backing, nil
}
