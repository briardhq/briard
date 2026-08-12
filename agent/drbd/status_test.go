package drbd

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"briard.io/shared/model"
)

// Fixtures are real `drbdsetup status --json` captured from the drbd-witness
// nixosTest (status-primary/status-witness); status-noquorum is the minority
// case (a primary that lost quorum), hand-built on the same captured schema.
func TestParseStatus(t *testing.T) {
	tests := []struct {
		fixture   string
		primary   bool
		quorate   bool
		connected int
	}{
		{"status-primary.json", true, true, 2},     // quorate primary, both peers connected
		{"status-witness.json", false, true, 2},    // diskless witness: secondary, quorate, 2 peers
		{"status-noquorum.json", true, false, 0},   // primary in the minority: no quorum, no peers
		{"status-lone-anchor.json", true, true, 1}, // sole diskful anchor + a connected witness: quorate
	}
	for _, tt := range tests {
		t.Run(tt.fixture, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join("testdata", tt.fixture))
			if err != nil {
				t.Fatal(err)
			}
			got, err := ParseStatus(raw, "r0")
			if err != nil {
				t.Fatal(err)
			}
			if got.Primary != tt.primary || got.Quorate != tt.quorate || got.Connected != tt.connected {
				t.Errorf("ParseStatus(%s) = %+v; want Primary=%v Quorate=%v Connected=%d",
					tt.fixture, got, tt.primary, tt.quorate, tt.connected)
			}
		})
	}
}

// ParseCluster adds the peer half: can anyone else take the workload?
// The fixtures carry all three answers — a real successor, a diskless witness that
// only looks like one, and a partitioned peer that reports no devices at all.
func TestParseCluster(t *testing.T) {
	tests := []struct {
		fixture   string
		peers     map[string]model.PeerState // by name
		successor bool                       // want PeerCanTakeOver()
	}{
		// The primary's view: node2 is a real successor; the witness is connected and
		// Secondary and yet must not count — it holds no data.
		{"status-primary.json", map[string]model.PeerState{
			"node2":   {Name: "node2", NodeID: 1, Connected: true, Role: "Secondary", Diskful: true, UpToDate: true},
			"witness": {Name: "witness", NodeID: 2, Connected: true, Role: "Secondary", Diskful: false, UpToDate: false},
		}, true},
		// The witness's own view: it can see who is Primary — the fact that makes a
		// handover observable rather than inferred.
		{"status-witness.json", map[string]model.PeerState{
			"node1": {Name: "node1", NodeID: 0, Connected: true, Role: "Primary", Diskful: true, UpToDate: true},
			"node2": {Name: "node2", NodeID: 1, Connected: true, Role: "Secondary", Diskful: true, UpToDate: true},
		}, true},
		// The minority primary: both peers Connecting, neither reporting a device.
		// Nobody can take over, so demoting here would be an outage, not a handover.
		{"status-noquorum.json", map[string]model.PeerState{
			"node2":   {Name: "node2", NodeID: 1, Connected: false, Role: "Unknown", Diskful: false, UpToDate: false},
			"witness": {Name: "witness", NodeID: 2, Connected: false, Role: "Unknown", Diskful: false, UpToDate: false},
		}, false},
		// THE DISCRIMINATING CASE: one anchor holding the data, one connected witness. This
		// node is Primary and fully quorate, and there is still nobody to take over. It is the
		// only fixture where "quorate" and "has a successor" disagree, which is what makes it
		// the one that proves the predicate is not a quorum test wearing a different name.
		{"status-lone-anchor.json", map[string]model.PeerState{
			"witness": {Name: "witness", NodeID: 2, Connected: true, Role: "Secondary", Diskful: false, UpToDate: false},
		}, false},
	}
	for _, tt := range tests {
		t.Run(tt.fixture, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join("testdata", tt.fixture))
			if err != nil {
				t.Fatal(err)
			}
			got, err := ParseCluster(raw, "r0")
			if err != nil {
				t.Fatal(err)
			}
			if len(got.Peers) != len(tt.peers) {
				t.Fatalf("got %d peers, want %d: %+v", len(got.Peers), len(tt.peers), got.Peers)
			}
			for _, p := range got.Peers {
				want, ok := tt.peers[p.Name]
				if !ok {
					t.Errorf("unexpected peer %q", p.Name)
					continue
				}
				if p != want {
					t.Errorf("peer %s = %+v; want %+v", p.Name, p, want)
				}
			}
			if got.PeerCanTakeOver() != tt.successor {
				t.Errorf("PeerCanTakeOver() = %v; want %v", got.PeerCanTakeOver(), tt.successor)
			}
		})
	}
}

// The three ways a peer stops being a successor, each on its own. Every conjunct in
// CanTakeOver must be able to fail the predicate by itself, or one of them is dead
// weight that would let a bad demote through.
func TestPeerCanTakeOverIsNotVacuous(t *testing.T) {
	tests := []struct {
		name string
		conn string // connection-state
		peer string // peer-disk-state
		cli  bool   // peer-client
	}{
		{"disconnected", "Connecting", "UpToDate", false},
		{"diskless witness", "Connected", "Diskless", true},
		{"stale replica", "Connected", "Outdated", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw := []byte(`[{"name":"r0","role":"Primary","devices":[{"quorum":true}],"connections":[` +
				`{"name":"peer","connection-state":"` + tt.conn + `","peer-role":"Secondary","peer_devices":[` +
				`{"peer-disk-state":"` + tt.peer + `","peer-client":` + strconv.FormatBool(tt.cli) + `}]}]}]`)
			got, err := ParseCluster(raw, "r0")
			if err != nil {
				t.Fatal(err)
			}
			if got.PeerCanTakeOver() {
				t.Errorf("a %s peer must not count as a successor: %+v", tt.name, got.Peers)
			}
		})
	}

	// ...and the same shape with none of those defects DOES pass, or the three
	// negatives above would be proving nothing.
	healthy := []byte(`[{"name":"r0","role":"Primary","devices":[{"quorum":true}],"connections":[` +
		`{"name":"peer","connection-state":"Connected","peer-role":"Secondary","peer_devices":[` +
		`{"peer-disk-state":"UpToDate","peer-client":false}]}]}]`)
	got, err := ParseCluster(healthy, "r0")
	if err != nil {
		t.Fatal(err)
	}
	if !got.PeerCanTakeOver() {
		t.Errorf("a connected, diskful, UpToDate peer must count as a successor: %+v", got.Peers)
	}

	// A peer with no peer_devices at all: the vacuous-truth trap. An empty fold over
	// "every volume is UpToDate" is true, and that would make an absent peer look
	// like the best successor there is.
	bare := []byte(`[{"name":"r0","role":"Primary","devices":[{"quorum":true}],"connections":[` +
		`{"name":"peer","connection-state":"Connected","peer-role":"Secondary"}]}]`)
	got, err = ParseCluster(bare, "r0")
	if err != nil {
		t.Fatal(err)
	}
	if got.PeerCanTakeOver() {
		t.Errorf("a peer reporting no devices must not count as a successor: %+v", got.Peers)
	}
}

func TestParseStatusErrors(t *testing.T) {
	if _, err := ParseStatus([]byte("not json"), "r0"); err == nil {
		t.Error("want error on malformed JSON")
	}
	if _, err := ParseStatus([]byte("[]"), "r0"); err == nil {
		t.Error("want error when the resource is absent from status")
	}
}

// PeerCounts reports configured peers vs. currently-connected — the deadman quorum gate inputs.
func TestPeerCounts(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "status-primary.json"))
	if err != nil {
		t.Fatal(err)
	}
	peers, connected, quorate, err := PeerCounts(raw)
	if err != nil {
		t.Fatal(err)
	}
	if peers != 2 || connected != 2 || !quorate {
		t.Errorf("status-primary PeerCounts = (peers=%d, connected=%d, quorate=%v), want (2, 2, true)",
			peers, connected, quorate)
	}

	// A configured-but-disconnected peer still counts toward the total (quorum denominator),
	// but not toward connected — so a node at the quorum edge is detectable.
	disconnected := []byte(`[{"name":"r0","role":"Primary","devices":[{"quorum":true}],` +
		`"connections":[{"connection-state":"Connected"},{"connection-state":"StandAlone"}]}]`)
	peers, connected, quorate, err = PeerCounts(disconnected)
	if err != nil {
		t.Fatal(err)
	}
	if peers != 2 || connected != 1 || !quorate {
		t.Errorf("mixed PeerCounts = (peers=%d, connected=%d, quorate=%v), want (2, 1, true)",
			peers, connected, quorate)
	}

	// Quorum lost — the input to the gate's "already failed, nothing to protect" clause. Folded
	// per-device: ANY volume without quorum makes the node non-quorate, since DRBD refuses writes.
	noQuorum := []byte(`[{"name":"r0","role":"Primary","devices":[{"quorum":true},{"quorum":false}],` +
		`"connections":[{"connection-state":"StandAlone"},{"connection-state":"StandAlone"}]}]`)
	if _, _, quorate, err = PeerCounts(noQuorum); err != nil || quorate {
		t.Errorf("PeerCounts on a partly non-quorate node = (quorate=%v, err=%v), want (false, nil)", quorate, err)
	}

	// No devices at all must not come out quorate by vacuous truth — the same fold ParseCluster
	// makes, and the reason a diskless/unprovisioned node cannot claim the write path.
	if _, _, quorate, err = PeerCounts([]byte(`[{"name":"r0","role":"Secondary","devices":[]}]`)); err != nil || quorate {
		t.Errorf("PeerCounts with no devices = (quorate=%v, err=%v), want (false, nil)", quorate, err)
	}

	if _, _, _, err := PeerCounts([]byte("[]")); err == nil {
		t.Error("want error when status is empty")
	}
}

// This node's OWN disk, read from the same sample as everyone else's. The gap it closes
// is not cosmetic: without these fields a node could say whether a PEER was a viable failover
// target but not whether IT was — so a Secondary's OS-upgrade gate had nothing node-local to ask
// and probed the VIP, which its peer answers.
//
// The witness row is the one that makes the fold matter. A diskless node reports disk-state
// "Diskless", so a `!= "UpToDate"` test alone would call it not-UpToDate AND leave Diskful true —
// and `diskful ∧ quorate ⇒ UpToDate` would then fail every witness upgrade, undoing.
func TestParseClusterReadsThisNodesOwnDisk(t *testing.T) {
	for _, tc := range []struct {
		fixture           string
		diskful, uptodate bool
	}{
		{"status-primary.json", true, true},     // serving anchor, disk-state UpToDate
		{"status-lone-anchor.json", true, true}, // sole anchor beside a witness
		{"status-witness.json", false, false},   // diskless: never UpToDate, and that is correct
		{"status-noquorum.json", true, true},    // peers gone, but this node's own disk is fine
	} {
		t.Run(tc.fixture, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join("testdata", tc.fixture))
			if err != nil {
				t.Fatal(err)
			}
			cl, err := ParseCluster(raw, "r0")
			if err != nil {
				t.Fatal(err)
			}
			if cl.Diskful != tc.diskful || cl.UpToDate != tc.uptodate {
				t.Errorf("self = {diskful:%v uptodate:%v}, want {diskful:%v uptodate:%v}",
					cl.Diskful, cl.UpToDate, tc.diskful, tc.uptodate)
			}
		})
	}
}
