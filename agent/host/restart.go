package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"briard.io/agent/nic"
	"briard.io/shared/atomicfile"
	"briard.io/shared/notify"
)

// THE RESTART AN INSTALL OWES, COUNTED, NEVER PROBED.
//
// A Windows bridge carries its TAP member only once the bridge object has been through a boot
// (nic.BuildBridge), and which driver gate closes is not readable. A probe -- the guest, an ARP
// through the TAP -- needs a bound, and the only bound is "a restart has been done", after which
// there is no second act: the right behaviour then is to launch and report whatever a probe would
// have said. So the gate is the count alone: one cold boot after the install built the bridge,
// seen once and saved on the bridge record, never asked again.
//
// The record is born when an install builds the bridge and goes with it at uninstall, which is
// what makes the three cases fall out: a first install asks until a cold boot; a reinstall over a
// running node finds the record marked and asks nothing; an uninstall then an install owes the
// restart again, rightly, since that bridge is new. On Linux there is no record and no gate.

// bridgeRecord is the bridge's record, pet state: the only copy of what this machine's NIC was set
// to before briard changed it, and whether the machine has restarted since the bridge was built.
const bridgeRecord = "bridge.json"

// bridgeRec is bridge.json. Built is zero on a record no install-time build wrote (a member swap's
// first record), which owes nothing.
type bridgeRec struct {
	nic.Offload
	Built     time.Time `json:"built"`
	Restarted bool      `json:"restarted"`
}

// readBridgeRecord reads the record at path; ok is false when there is none.
func readBridgeRecord(path string) (rec bridgeRec, ok bool, err error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return bridgeRec{}, false, nil
	}
	if err != nil {
		return bridgeRec{}, false, err
	}
	if err := json.Unmarshal(b, &rec); err != nil {
		return bridgeRec{}, false, fmt.Errorf("reading %s: %w", path, err)
	}
	return rec, true, nil
}

func writeBridgeRecord(path string, rec bridgeRec) error {
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	return atomicfile.Write(path, b, 0o600, 0o700)
}

// keepBuiltBridge is the install's half: the record of a bridge build at built, carrying the
// offload it changed (zero when it changed none). An existing record is the same bridge -- a
// reinstall, or a re-run after a build that failed part-way -- so its count is kept; an absent
// one is this build's, and owes a restart.
func keepBuiltBridge(path string, off nic.Offload, built time.Time) error {
	rec, ok, err := readBridgeRecord(path)
	if err != nil {
		return err
	}
	if !ok {
		rec = bridgeRec{Built: built}
	}
	if off != (nic.Offload{}) {
		rec.Offload = off
	}
	return writeBridgeRecord(path, rec)
}

// restartOwed answers the gate from the record: owed when the bridge was built and no cold boot has
// been seen since; seen when this read found one, which the caller saves. A record already marked,
// or one no build wrote, asks nothing.
func restartOwed(rec bridgeRec, coldBoot func(since time.Time) (bool, error)) (owed, seen bool, err error) {
	if rec.Built.IsZero() || rec.Restarted {
		return false, false, nil
	}
	ok, err := coldBoot(rec.Built)
	if err != nil {
		return false, false, err
	}
	return !ok, ok, nil
}

// restartAlertKey is the gate's alert, open while the restart is owed.
const restartAlertKey = "install-restart"

// restartFix is what the gate says, in the log and the alert. "Restart", by name: a Shut down with
// Fast Startup on is a hibernate, and its power-on is no boot of the bridge.
const restartFix = "Windows has not restarted since briard was installed: restart it (Restart, not Shut down) to finish the install"

// restartGate is the first thing a start asks: true means the restart is still owed, said, and the
// start ends here -- cleanly, a STOPPED service and exit 0, because a non-zero exit is the
// service's recovery restarting it every few seconds, forever. The line is the network refusal's
// own, so `install -wait` ends on it with the reason.
//
// ⚠️ A READ THAT FAILS PROCEEDS. Refusing on "could not tell" is a node that never starts;
// proceeding on it is a node that may not reach the LAN and says so (the no-LAN-presence alert).
func (cfg Config) restartGate(ctx context.Context, coldBoot func(time.Time) (bool, error), n notify.Notifier, logf func(string, ...any)) bool {
	if cfg.stateDir() == "" {
		return false
	}
	path := filepath.Join(cfg.stateDir(), bridgeRecord)
	rec, ok, err := readBridgeRecord(path)
	if err != nil {
		logf("install: could not read the bridge record (%v); starting without confirming the restart", err)
		return false
	}
	if !ok {
		return false
	}
	owed, seen, err := restartOwed(rec, coldBoot)
	switch {
	case err != nil:
		logf("install: could not confirm Windows restarted since the install (%v); starting anyway", err)
	case owed:
		logf("network: refused: %s", restartFix)
		fireAlert(ctx, n, logf, notify.Alert{Key: restartAlertKey, Kind: notify.Open, Severity: notify.Critical,
			Title: "Briard: restart Windows to finish the install", Body: restartFix + "."})
		return true
	case seen:
		logf("install: Windows has restarted since the install; the bridge carries the guest")
		rec.Restarted = true
		if err := writeBridgeRecord(path, rec); err != nil {
			logf("install: could not save that the restart was seen (%v); it is asked again at the next start", err)
		}
		fireAlert(ctx, n, logf, notify.Alert{Key: restartAlertKey, Kind: notify.Resolved,
			Title: "Briard: the install is finished", Body: "Windows has restarted since briard was installed."})
	}
	return false
}
