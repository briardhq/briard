package host

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"briard.io/shared/notify"
)

// THE ADDRESS MOVED, SAID ONCE: the first address is only remembered; the same address, a failed
// read and a node holding none say nothing; a new one is one Event naming both; and the record
// survives a restart, since a restart is when the address most likely changes.
func TestAddressMovedIsSaidOnce(t *testing.T) {
	dir := t.TempDir()
	n := &fakeNotifier{}
	w := newAddressWatch(dir)
	w.running = func(string) bool { return false }
	tick := func(cidr string, err error) {
		t.Helper()
		w.tick(context.Background(), Config{}, vipAnswer{cidr: cidr, err: err}, n, t.Logf)
	}

	tick("192.168.1.50/24", nil)
	tick("192.168.1.50/24", nil)
	tick("", nil)
	tick("192.168.1.77/24", errors.New("channel down"))
	if len(n.alerts) != 0 {
		t.Fatalf("no move, yet alerts: %+v", n.alerts)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, addressFile)); strings.TrimSpace(string(b)) != "192.168.1.50" {
		t.Fatalf("the record holds %q, want 192.168.1.50", b)
	}

	tick("192.168.1.77/24", nil)
	tick("192.168.1.77/24", nil)
	if len(n.alerts) != 1 {
		t.Fatalf("a move said %d times, want once: %+v", len(n.alerts), n.alerts)
	}
	a := n.alerts[0]
	if a.Key != "address-moved" || a.Kind != notify.Event || a.Severity != notify.Warning ||
		!strings.Contains(a.Body, "now 192.168.1.77") || !strings.Contains(a.Body, "was 192.168.1.50") {
		t.Fatalf("the notice = %+v", a)
	}
	if strings.Contains(a.Body, "from away") {
		t.Errorf("no private-network app runs here, yet the notice speaks of one: %s", a.Body)
	}

	// A restart: a new watch reads the record, so the same address is no move and a new one is.
	w = newAddressWatch(dir)
	w.running = func(string) bool { return false }
	tick("192.168.1.77/24", nil)
	if len(n.alerts) != 1 {
		t.Fatalf("a restart on the same address said it moved: %+v", n.alerts[1:])
	}
	tick("192.168.1.90/24", nil)
	if len(n.alerts) != 2 || !strings.Contains(n.alerts[1].Body, "was 192.168.1.77") {
		t.Fatalf("a move after a restart = %+v", n.alerts)
	}
}

// With Tailscale or NetBird running here, the notice names it, says the route must be set up
// again and links the setup page; the OS service is asked by its name on this system.
func TestAddressMovedNamesTheRemoteAccessApp(t *testing.T) {
	for _, c := range []struct {
		linux, windows, tool string
	}{
		{"tailscaled", "Tailscale", "Tailscale"},
		{"netbird", "netbird", "NetBird"},
	} {
		svc := c.linux
		if runtime.GOOS == "windows" {
			svc = c.windows
		}
		n := &fakeNotifier{}
		w := newAddressWatch(t.TempDir())
		w.running = func(name string) bool { return name == svc }
		w.tick(context.Background(), Config{}, vipAnswer{cidr: "192.168.1.50/24"}, n, t.Logf)
		w.tick(context.Background(), Config{}, vipAnswer{cidr: "192.168.1.77/24"}, n, t.Logf)
		if len(n.alerts) != 1 {
			t.Fatalf("%s: %d alerts, want 1", c.tool, len(n.alerts))
		}
		a := n.alerts[0]
		if a.Severity != notify.Warning || !strings.Contains(a.Body, "set up its route in "+c.tool+" again") ||
			!strings.Contains(a.Body, remoteAccessPage) {
			t.Errorf("%s: the notice = %+v", c.tool, a)
		}
	}
}

// No state dir, nothing to keep the record in: no watch, and a nil one is inert.
func TestAddressWatchNeedsAStateDir(t *testing.T) {
	w := newAddressWatch("")
	if w != nil {
		t.Fatal("a watch with nowhere to keep its record")
	}
	w.tick(context.Background(), Config{}, vipAnswer{cidr: "192.168.1.50/24"}, &fakeNotifier{}, t.Logf)
}
