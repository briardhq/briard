package host

// THE ADDRESS MOVED. The household reaches Briard at the service address its router hands out,
// and anything set to that address -- a bookmark, a device's settings, a private network's route
// to it -- stops working when the address changes, with no word anywhere. So a change is said,
// once: an Event, which changes no state and has nothing to close it.
//
// The last address is node-local pet state, durable, because a restart is exactly when an address
// is most likely to change: the guest takes one at bring-up. The first address a node ever sees
// moved from nothing, and is only remembered.
//
// The wording names Tailscale or NetBird when its OS service runs on this host, read and never
// touched: those are the tools the setup page covers, and each carries a route to the old address
// that has to be set up again.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"briard.io/agent/guest"
	"briard.io/agent/platform"
	"briard.io/shared/atomicfile"
	"briard.io/shared/notify"
)

// addressFile is where the last address seen is kept, in the state dir.
const addressFile = "address-seen"

// remoteAccessPage is the setup page an address alert points at.
const remoteAccessPage = "https://briard.io/docs/remote-access/"

// addressWatch fires the notice. Machinery like beat; nil-safe. Written by the observe loop only.
type addressWatch struct {
	path    string                 // the durable record of the last address
	last    string                 // the last address seen; read from path once
	loaded  bool                   // path has been read
	running func(name string) bool // whether an OS service runs on this host; faked by the tests
}

// newAddressWatch builds the watch; nil without a state dir to keep the record in.
func newAddressWatch(stateDir string) *addressWatch {
	if stateDir == "" {
		return nil
	}
	return &addressWatch{path: filepath.Join(stateDir, addressFile), running: serviceRunning}
}

// serviceRunning reports whether an OS service is installed here and not stopped; one that is
// absent reads as not running on both systems.
func serviceRunning(name string) bool {
	stopped, err := platform.ServiceStopped(name)
	return err == nil && !stopped
}

// tick compares the cycle's one answer to net.vip with the last address seen. An address this
// node does not hold (not serving, or a read that failed) is no change.
func (w *addressWatch) tick(ctx context.Context, cfg Config, vip guest.VIPReader, n notify.Notifier, logf func(string, ...any)) {
	if w == nil {
		return
	}
	cidr, err := vip.VIP(ctx, cfg.VIPDev)
	if err != nil || cidr == "" {
		return
	}
	addr, _, _ := strings.Cut(cidr, "/")
	if !w.loaded {
		if b, err := os.ReadFile(w.path); err == nil {
			w.last = strings.TrimSpace(string(b))
		}
		w.loaded = true
	}
	if addr == w.last {
		return
	}
	from := w.last
	w.last = addr
	if err := atomicfile.Write(w.path, []byte(addr+"\n"), 0o644, 0o755); err != nil {
		logf("address: remember %s: %v", addr, err)
	}
	if from == "" {
		return
	}
	logf("address: Briard's address moved from %s to %s", from, addr)
	fireAlert(ctx, n, logf, addressMoved(from, addr, remoteTool(w.running)))
}

// remoteTool names the private-network app whose OS service runs on this host, "" for neither.
func remoteTool(running func(string) bool) string {
	for _, t := range []struct{ name, linux, windows string }{
		{"Tailscale", "tailscaled", "Tailscale"},
		{"NetBird", "netbird", "netbird"},
	} {
		svc := t.linux
		if runtime.GOOS == "windows" {
			svc = t.windows
		}
		if running(svc) {
			return t.name
		}
	}
	return ""
}

// addressMoved is the notice -- a warning, since whatever was set to the old address is broken --
// and, when a private network's route is known to point at the old address, how to set it again.
func addressMoved(from, to, tool string) notify.Alert {
	a := notify.Alert{
		Key:      "address-moved",
		Kind:     notify.Event,
		Severity: notify.Warning,
		Title:    "Briard: its address on your home network changed",
		Body: fmt.Sprintf("Briard's address is now %s; it was %s. Anything set to the old address, "+
			"such as a bookmark, needs the new one.", to, from),
	}
	if tool != "" {
		a.Body += fmt.Sprintf(" To reach Briard from away, set up its route in %s again with the new address: %s",
			tool, remoteAccessPage)
	}
	return a
}
