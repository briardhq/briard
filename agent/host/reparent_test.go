package host

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"briard.io/agent/nic"
)

// THE TIERS, and the one that is not a duration.
//
// "Different subnet" is an explicit operator action rather than a longer wait (owner,
// 2026-09-15): a paired node that rebuilds itself on a LAN its peer is not on is a split flock,
// which is the outcome this whole item exists to avoid. Unknown takes the same answer, because
// "we could not tell" must never be cheaper than "we could tell, and it was elsewhere".
func TestReparentWait(t *testing.T) {
	for _, tc := range []struct {
		rel   nic.Relation
		want  time.Duration
		timed bool
	}{
		{nic.SameWire, 5 * time.Minute, true},
		{nic.SameSubnet, 30 * time.Minute, true},
		{nic.Elsewhere, 0, false},
		{nic.Unknown, 0, false},
	} {
		got, ok := reparentWait(tc.rel, 0)
		if ok != tc.timed || got != tc.want {
			t.Errorf("reparentWait(%q) = %v,%v; want %v,%v", tc.rel, got, ok, tc.want, tc.timed)
		}
	}
}

// THE TEST FIXTURE SHRINKS DURATIONS AND NOTHING ELSE. install-macvtap uses it to drive a
// re-parent without spending the shipped five minutes -- but "different subnet" and "cannot tell"
// are not long waits, they are an operator's decision, and a fixture that could reach them would
// let a rig prove a re-parent the product forbids. That is the worst thing a test knob can do, so
// it is the thing asserted.
func TestReparentWaitFixtureCannotUnlockARefusal(t *testing.T) {
	for _, rel := range []nic.Relation{nic.Elsewhere, nic.Unknown} {
		if _, ok := reparentWait(rel, time.Millisecond); ok {
			t.Errorf("the fixture turned %q into a wait; it must stay an operator's decision", rel)
		}
	}
	// It does shrink the real tiers...
	if got, ok := reparentWait(nic.SameWire, 2*time.Second); !ok || got != 2*time.Second {
		t.Errorf("reparentWait(same-wire, 2s) = %v,%v; want 2s,true", got, ok)
	}
	if got, ok := reparentWait(nic.SameSubnet, 2*time.Second); !ok || got != 2*time.Second {
		t.Errorf("reparentWait(same-subnet, 2s) = %v,%v; want 2s,true", got, ok)
	}
	// ...and never LENGTHENS one: a fixture longer than the tier is ignored, so a stray value
	// cannot make a production node more sluggish than the shipped policy.
	if got, _ := reparentWait(nic.SameWire, time.Hour); got != reparentSameWire {
		t.Errorf("reparentWait(same-wire, 1h) = %v; want the shipped tier", got)
	}
}

// THE RECORDED DEVICE RETURNING OUTRANKS EVERY TIER. A NIC back in three seconds must not cost a
// guest restart -- so the clock resets the moment the parent is visible again, and a later
// disappearance starts a fresh wait rather than resuming the old one. Without the reset, a device
// that flapped for six minutes would re-parent on the seventh even though it was present
// throughout.
func TestReparenterResetsWhenTheParentComesBack(t *testing.T) {
	r := &reparenter{}
	cfg := Config{net: &nic.Spec{Parent: "briard-no-such-parent", SystemTap: "sys0"}}
	t0 := time.Now()

	// Absent: the clock starts. (No candidate exists in this environment either, so nothing is
	// returned -- which is itself the "never tear down before a new answer exists" rule.)
	if dev, _ := r.consider(t.Context(), cfg, t0, discard); dev != "" {
		t.Fatalf("re-parented onto %q with no recorded LAN to compare against", dev)
	}
	if r.goneSince.IsZero() {
		t.Fatal("the parent is absent and the clock did not start")
	}

	// Present again -- a real device this host certainly has. The clock must clear.
	cfg.net.Parent = "lo"
	if dev, _ := r.consider(t.Context(), cfg, t0.Add(time.Minute), discard); dev != "" {
		t.Fatalf("re-parented onto %q while the parent was present", dev)
	}
	if !r.goneSince.IsZero() {
		t.Error("the parent came back and the clock was not reset")
	}
}

// A node whose L2 hangs off a bridge is never re-parented: the bridge is the USER's, we never
// created one, and a bridge that disappears is theirs to restore ([B.150](c)). Nor is a node that
// owns no network at all.
func TestReparenterLeavesWhatIsNotOurs(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  Config
	}{
		{"no network of ours", Config{}},
		{"a bridge the user owns", Config{net: &nic.Spec{Parent: "br-no-such", Bridge: true, SystemTap: "t"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &reparenter{}
			if dev, _ := r.consider(t.Context(), tc.cfg, time.Now(), discard); dev != "" {
				t.Errorf("re-parented onto %q", dev)
			}
			if !r.goneSince.IsZero() {
				t.Error("started a clock on a network this agent does not own")
			}
		})
	}
}

// The record is PET state and a round trip has to survive a process, because the whole point is
// comparing today's LAN against the one from before a reboot. A missing or corrupt file reads as
// the zero fingerprint, which Compare answers Unknown for -- safe, and never a false match.
func TestNetworkRecordRoundTrip(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{
		AssignmentCache: filepath.Join(dir, "assignment.json"),
		net:             &nic.Spec{Parent: "lo"},
	}
	cfg.recordNetwork(t.Context(), discard)

	back := cfg.recordedNetwork()
	if back.Parent != "lo" {
		t.Errorf("recorded parent = %q, want lo", back.Parent)
	}
	// It is written where the other node-local caches live, not somewhere of its own.
	if _, err := os.Stat(filepath.Join(dir, "network.json")); err != nil {
		t.Errorf("the record is not beside the assignment cache: %v", err)
	}

	// Corrupt it: the reader must answer "no record" rather than a partial one, because a
	// half-read fingerprint could match a LAN it was never taken on.
	if err := os.WriteFile(filepath.Join(dir, "network.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := cfg.recordedNetwork(); got != (nic.Fingerprint{}) {
		t.Errorf("a corrupt record read as %+v, want nothing", got)
	}
	if got := nic.Compare(cfg.recordedNetwork(), nic.Fingerprint{Parent: "eth0", HostCIDR: "10.0.0.2/24"}); got != nic.Unknown {
		t.Errorf("a corrupt record compares as %q, want %q", got, nic.Unknown)
	}

	// A config with no pet state dir writes nothing and reads nothing, rather than guessing a path.
	if got := (Config{net: &nic.Spec{Parent: "lo"}}).recordedNetwork(); got != (nic.Fingerprint{}) {
		t.Errorf("read %+v with no state dir configured", got)
	}

	// AN AGENT THAT OWNS NO NETWORK MUST NOT CRASH RECORDING ONE. Run calls this unconditionally
	// after bring-up, and nil is the ordinary state on every rig and every lab node -- so this is
	// the difference between the fleet running and the fleet panicking on the first bring-up.
	(Config{AssignmentCache: filepath.Join(dir, "assignment.json")}).recordNetwork(t.Context(), discard)
	if _, err := os.Stat(filepath.Join(dir, "none.json")); err == nil {
		t.Error("recorded a LAN for an agent that owns no network")
	}
}

// The record must be readable by the shape that writes it -- a field renamed on one side only
// would leave every future re-parent decision Unknown, which refuses silently and forever.
func TestNetworkRecordIsTheShapeOnDisk(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{AssignmentCache: filepath.Join(dir, "assignment.json"), net: &nic.Spec{Parent: "lo"}}
	cfg.recordNetwork(t.Context(), discard)
	b, err := os.ReadFile(filepath.Join(dir, "network.json"))
	if err != nil {
		t.Fatal(err)
	}
	var rec networkRecord
	if err := json.Unmarshal(b, &rec); err != nil {
		t.Fatalf("the record does not parse as what wrote it: %v (%s)", err, b)
	}
	if rec.At.IsZero() {
		t.Error("the record carries no timestamp; a reader cannot tell how old the comparison is")
	}
}

func discard(string, ...any) {}
