package host

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"briard.io/agent/nic"
	"briard.io/shared/notify"
)

// bootRead is a stubbed Kernel-Boot read: cold says whether a cold boot is logged, and asked
// counts the queries, which a marked record must never make.
type bootRead struct {
	cold  bool
	err   error
	asked int
	since time.Time
}

func (b *bootRead) read(t time.Time) (bool, error) {
	b.asked++
	b.since = t
	return b.cold, b.err
}

func TestRestartGate(t *testing.T) {
	built := time.Date(2026, 10, 25, 9, 0, 0, 0, time.UTC)
	off := nic.Offload{GUID: "{g}", Keyword: "*IPChecksumOffloadIPv4", Value: "3"}
	setup := func(t *testing.T) (Config, string) {
		dir := t.TempDir()
		return Config{AssignmentCache: filepath.Join(dir, "assignment.json")}, filepath.Join(dir, bridgeRecord)
	}
	gate := func(cfg Config, b *bootRead) (bool, *fakeNotifier) {
		n := &fakeNotifier{}
		return cfg.restartGate(context.Background(), b.read, n, func(string, ...any) {}), n
	}

	// A FIRST INSTALL asks at every start until a cold boot, then marks the record and asks nothing.
	t.Run("first install", func(t *testing.T) {
		cfg, path := setup(t)
		if err := keepBuiltBridge(path, off, built); err != nil {
			t.Fatal(err)
		}
		b := &bootRead{}
		refused, n := gate(cfg, b)
		if !refused || b.asked != 1 || !b.since.Equal(built) {
			t.Fatalf("before the restart: refused=%v asked=%d since=%v, want refused, one query since the build", refused, b.asked, b.since)
		}
		if len(n.alerts) != 1 || n.alerts[0].Key != restartAlertKey || n.alerts[0].Kind != notify.Open {
			t.Fatalf("before the restart: alerts %+v, want the restart alert open", n.alerts)
		}
		b.cold = true
		if refused, n = gate(cfg, b); refused || b.asked != 2 {
			t.Fatalf("after the restart: refused=%v asked=%d, want a start after one more query", refused, b.asked)
		}
		if len(n.alerts) != 1 || n.alerts[0].Kind != notify.Resolved {
			t.Fatalf("after the restart: alerts %+v, want the restart alert resolved", n.alerts)
		}
		rec, _, _ := readBridgeRecord(path)
		if !rec.Restarted || rec.Offload != off {
			t.Fatalf("record %+v, want marked with the offload kept", rec)
		}
		// Restart-Service afterwards: no query at all.
		if refused, _ = gate(cfg, b); refused || b.asked != 2 {
			t.Fatalf("a later start: refused=%v asked=%d, want a start with no query", refused, b.asked)
		}
	})

	// A REINSTALL over a running node: the install's build finds its record marked and keeps it.
	t.Run("reinstall over a running node", func(t *testing.T) {
		cfg, path := setup(t)
		if err := writeBridgeRecord(path, bridgeRec{Offload: off, Built: built, Restarted: true}); err != nil {
			t.Fatal(err)
		}
		// The bridge is already ours, so BuildBridge changes nothing and reports a zero offload.
		if err := keepBuiltBridge(path, nic.Offload{}, built.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		b := &bootRead{}
		if refused, _ := gate(cfg, b); refused || b.asked != 0 {
			t.Fatalf("refused=%v asked=%d, want a start with no query", refused, b.asked)
		}
		if rec, _, _ := readBridgeRecord(path); rec.Offload != off || !rec.Built.Equal(built) {
			t.Fatalf("record %+v, want the first build's offload and time kept", rec)
		}
	})

	// AN UNINSTALL THEN AN INSTALL: the record went with the bridge, so the new bridge owes its own
	// restart -- the earlier cold boots are before its build and do not count.
	t.Run("uninstall then install", func(t *testing.T) {
		cfg, path := setup(t)
		if err := writeBridgeRecord(path, bridgeRec{Offload: off, Built: built, Restarted: true}); err != nil {
			t.Fatal(err)
		}
		os.Remove(path) // removeNetwork's
		again := built.Add(24 * time.Hour)
		if err := keepBuiltBridge(path, off, again); err != nil {
			t.Fatal(err)
		}
		b := &bootRead{}
		if refused, _ := gate(cfg, b); !refused || b.asked != 1 || !b.since.Equal(again) {
			t.Fatalf("refused=%v asked=%d since=%v, want refused after one query since the new build", refused, b.asked, b.since)
		}
	})

	// A READ THAT FAILS PROCEEDS, unmarked, so the next start asks again; and a record no install
	// built (a node that had one before the count existed, or no record at all) asks nothing.
	t.Run("could not tell, and nothing to count", func(t *testing.T) {
		cfg, path := setup(t)
		b := &bootRead{}
		if refused, _ := gate(cfg, b); refused || b.asked != 0 {
			t.Fatalf("no record: refused=%v asked=%d, want a start with no query", refused, b.asked)
		}
		if err := writeBridgeRecord(path, bridgeRec{Offload: off}); err != nil {
			t.Fatal(err)
		}
		if refused, _ := gate(cfg, b); refused || b.asked != 0 {
			t.Fatalf("a record no build wrote: refused=%v asked=%d, want a start with no query", refused, b.asked)
		}
		if err := keepBuiltBridge(path, off, built); err != nil {
			t.Fatal(err)
		}
		if rec, _, _ := readBridgeRecord(path); !rec.Built.IsZero() {
			t.Fatalf("record %+v: an existing record keeps its count", rec)
		}
		os.Remove(path)
		if err := keepBuiltBridge(path, off, built); err != nil {
			t.Fatal(err)
		}
		b.err = errors.New("wevtutil: access denied")
		refused, n := gate(cfg, b)
		if refused || b.asked != 1 || len(n.alerts) != 0 {
			t.Fatalf("a failed read: refused=%v asked=%d alerts=%+v, want a start, no alert", refused, b.asked, n.alerts)
		}
		if rec, _, _ := readBridgeRecord(path); rec.Restarted {
			t.Fatal("a failed read marked the record")
		}
	})
}
