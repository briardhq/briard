package nic

import (
	"context"
	"os"
	"testing"
	"time"
)

// DESTRUCTIVE: these bridge a real NIC and take the machine's link down while they do. They run
// only when BRIARD_TEST_BRIDGE names the NIC to bridge, on a machine built to be broken
// (lab/vanilla-windows), as an administrator: TestBuildBridge before a restart, then
// TestBridgeAfterRestart after one -- because a Windows bridge shows its TAP as a member, and
// carries it, only once the machine has restarted. TestDestroyBridge runs alone on a machine with
// no bridge and a TAP adapter present, and leaves it with neither.

func bridgeMember(t *testing.T) string {
	member := os.Getenv("BRIARD_TEST_BRIDGE")
	if member == "" {
		t.Skip("BRIARD_TEST_BRIDGE names no NIC to bridge")
	}
	return member
}

func TestBuildBridge(t *testing.T) {
	member := bridgeMember(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	rec, err := BuildBridge(ctx, member)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("offload record: %+v", rec)
	if rec.GUID == "" || rec.Keyword != checksumOffload || rec.Value == "" {
		t.Errorf("record = %+v, want the member's GUID and its previous value", rec)
	}
	// Read back from the stack table, not from what BuildBridge said it did.
	if !IsBridge(BridgeName) {
		t.Errorf("no bridge named %q", BridgeName)
	}
	if got := master(member); got != BridgeName {
		t.Errorf("master(%q) = %q, want %q", member, got, BridgeName)
	}
	if _, ok := byName(adapters(), TapName); !ok {
		t.Errorf("the TAP was not named %q", TapName)
	}
	// Idempotent before the restart too: an installer re-run finds its own bridge.
	if again, err := BuildBridge(ctx, member); err != nil || again != (Offload{}) {
		t.Errorf("second build = %+v, %v; want the zero record and no error", again, err)
	}
}

func TestBridgeAfterRestart(t *testing.T) {
	member := bridgeMember(t)
	for _, m := range []string{member, TapName} {
		if got := master(m); got != BridgeName {
			t.Errorf("master(%q) = %q, want %q", m, got, BridgeName)
		}
	}
	// The bridge keeps the household NIC's MAC across the restart -- without the pin it takes the
	// TAP's and the desktop leases a new address (measured).
	as := adapters()
	br, _ := byName(as, BridgeName)
	m, _ := byName(as, member)
	if br.mac == "" || br.mac != m.mac {
		t.Errorf("bridge MAC %q, member MAC %q: want the member's", br.mac, m.mac)
	}
	if again, err := BuildBridge(context.Background(), member); err != nil || again != (Offload{}) {
		t.Errorf("a build after the restart = %+v, %v; want the zero record and no error", again, err)
	}
}

// TestDestroyBridge builds the bridge, destroys it with the record the build returned, and reads
// the member's settings back from the registry: each one the build turned off must be at its
// recorded value again, the checksum offload and both receive segment coalescing values alike.
func TestDestroyBridge(t *testing.T) {
	member := bridgeMember(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	rec, err := BuildBridge(ctx, member)
	if err != nil {
		t.Fatal(err)
	}
	if rec == (Offload{}) {
		t.Skip("a bridge already exists: this test needs to build its own to know what to put back")
	}
	t.Logf("offload record: %+v", rec)
	for _, s := range rec.settings() {
		if kw, want := s[0], s[1]; want != "" {
			if got, err := readSetting(ctx, member, kw); err != nil || got != "0" {
				t.Errorf("%s on %s after the build = %q, %v; want 0", kw, member, got, err)
			}
		}
	}
	if err := DestroyBridge(ctx, rec); err != nil {
		t.Fatal(err)
	}
	if IsBridge(BridgeName) {
		t.Errorf("a bridge named %q survived DestroyBridge", BridgeName)
	}
	// The device removal is asynchronous: the adapter table lists the TAP for a moment after
	// pnputil returns (measured: a read straight after still showed it; the next did not).
	for deadline := time.Now().Add(15 * time.Second); ; time.Sleep(time.Second) {
		_, ok := byName(adapters(), TapName)
		if !ok {
			break
		}
		if time.Now().After(deadline) {
			t.Errorf("the TAP %q survived DestroyBridge", TapName)
			break
		}
	}
	for _, s := range rec.settings() {
		if kw, want := s[0], s[1]; want != "" {
			if got, err := readSetting(ctx, member, kw); err != nil || got != want {
				t.Errorf("%s on %s after the destroy = %q, %v; want %q back", kw, member, got, err, want)
			}
		}
	}
}
