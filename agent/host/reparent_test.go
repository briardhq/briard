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
// "Different subnet" is an explicit operator action rather than a longer wait: a paired node
// that rebuilds itself on a LAN its peer is not on is a split flock,
// which is the outcome this whole mechanism exists to avoid. Unknown takes the same answer, because
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
// created one, and a bridge that disappears is theirs to restore. Nor is a node that
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

	// AN UNUSABLE READING NEVER OVERWRITES A GOOD RECORD. The parent is chosen at start-up and
	// recorded after BRING-UP, seconds later, so a NIC that goes away in between reads as nothing
	// -- and writing that would answer Unknown forever, stranding a node that could have healed
	// itself. Caught by install-macvtap (run 35029767682), where the agent replaced a complete
	// fingerprint with `[eth1 mac=? addr=? gw=?/?]` and then refused to re-parent for good.
	//
	// Re-recorded first, because the corruption above deliberately emptied the file and comparing
	// nothing against nothing would pass whatever the code did.
	cfg.recordNetwork(t.Context(), discard)
	good := cfg.recordedNetwork()
	if !good.Usable() {
		t.Fatalf("the fixture is vacuous: nothing good was recorded to overwrite (%+v)", good)
	}
	gone := Config{
		AssignmentCache: filepath.Join(dir, "assignment.json"),
		net:             &nic.Spec{Parent: "briard-no-such-device"},
	}
	gone.recordNetwork(t.Context(), discard)
	if after := cfg.recordedNetwork(); after != good {
		t.Errorf("an unreadable device overwrote the record: %+v, was %+v", after, good)
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

// THE GATEWAY MAC IS A PROPERTY OF THE LAN, NOT OF THE MOMENT. A bring-up with the cable out or a
// gateway slow to answer ARP reads none, and the record must neither forget one it had for the
// same LAN nor stay without one once the gateway answers: the tick completes it. The rig caught
// the first half (a restart during an outage wrote `gw=192.168.78.1/?` over a complete record).
func TestNetworkRecordLearnsTheGatewayMAC(t *testing.T) {
	complete := nic.Fingerprint{Parent: "eth0", ParentMAC: "aa:bb:cc:dd:ee:01", HostCIDR: "192.168.1.50/24", GatewayIP: "192.168.1.1", GatewayMAC: "11:22:33:44:55:66"}
	blind := complete
	blind.GatewayMAC = ""
	elsewhere := nic.Fingerprint{Parent: "eth0", HostCIDR: "10.0.0.5/24", GatewayIP: "10.0.0.1"}

	// A reading with no MAC keeps the record's, on the same LAN only.
	if got := learnRecord(complete, blind); got != complete {
		t.Errorf("a blind reading of the same LAN forgot the MAC: %+v", got)
	}
	if got := learnRecord(complete, elsewhere); got != elsewhere {
		t.Errorf("a different LAN borrowed the old MAC: %+v", got)
	}
	if got := learnRecord(nic.Fingerprint{}, blind); got != blind {
		t.Errorf("no record, yet the reading changed: %+v", got)
	}

	// The tick completes a record without a MAC, from a reading with one, on the same LAN -- or
	// from any usable reading when there is no record at all. Never a complete record, never a
	// blind reading, never another LAN.
	for _, tc := range []struct {
		name    string
		rec, fp nic.Fingerprint
		want    bool
	}{
		{"blind record, the gateway answers now", blind, complete, true},
		{"no record at all", nic.Fingerprint{}, complete, true},
		{"the record is already complete", complete, complete, false},
		{"the gateway still does not answer", blind, blind, false},
		{"a different LAN answers", blind, nic.Fingerprint{Parent: "eth0", HostCIDR: "10.0.0.5/24", GatewayIP: "10.0.0.1", GatewayMAC: "de:ad:be:ef:00:01"}, false},
		{"an unusable reading", nic.Fingerprint{}, nic.Fingerprint{Parent: "eth0", GatewayMAC: "de:ad:be:ef:00:01"}, false},
	} {
		if got := completesRecord(tc.rec, tc.fp); got != tc.want {
			t.Errorf("%s: completesRecord = %v, want %v", tc.name, got, tc.want)
		}
	}

	// Through the file: a record with a MAC survives recordNetwork reading the device blind.
	// lo has an address and no gateway, which is exactly a reading with no MAC.
	dir := t.TempDir()
	cfg := Config{AssignmentCache: filepath.Join(dir, "assignment.json"), net: &nic.Spec{Parent: "lo"}}
	cfg.recordNetwork(t.Context(), discard)
	rec := cfg.recordedNetwork()
	if !rec.Usable() || rec.GatewayMAC != "" {
		t.Fatalf("the fixture is vacuous: lo read as %+v", rec)
	}
	rec.GatewayMAC = "11:22:33:44:55:66"
	cfg.writeNetworkRecord(rec, discard)
	cfg.recordNetwork(t.Context(), discard)
	if got := cfg.recordedNetwork(); got != rec {
		t.Errorf("a blind re-record forgot the MAC: %+v, want %+v", got, rec)
	}
	// And the tick leaves a complete record alone, and a blind reading changes nothing.
	cfg.completeNetworkRecord(t.Context(), discard)
	if got := cfg.recordedNetwork(); got != rec {
		t.Errorf("the tick rewrote a complete record: %+v", got)
	}
}
