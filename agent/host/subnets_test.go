package host

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// numbering is the whole of subnets.go's decision surface, and it is tested here rather than in a
// rig because every branch is a question about two inputs (what the environment says, what the
// record says) with no network in it. The DRAW itself is agent/subnet's, tested there against a
// fabricated host; what this file pins is which of the three sources wins, and that a node is
// never renumbered by accident.

// cfgAt builds a Config whose pet state dir is t.TempDir(), the way ASSIGNMENT_CACHE's directory
// is the node's state dir in production (subnetsPath says why it is derived and not a second key).
func cfgAt(t *testing.T) Config {
	t.Helper()
	return Config{AssignmentCache: filepath.Join(t.TempDir(), "assignment.json")}
}

// A node that has already drawn keeps what it drew, and reads it with no network work: the record
// is the answer, and re-drawing it would renumber a live node under peers that still hold the old
// address. The draw would need a device; passing none proves none was reached.
func TestNumberThisNodeKeepsTheRecord(t *testing.T) {
	cfg := cfgAt(t)
	if err := os.WriteFile(cfg.subnetsPath(),
		[]byte("SYSTEM_SUBNET=10.173.94\nPRIV_SUBNET=10.11.203\nPOD_SUBNET=10.12.7\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := cfg.numberThisNode(context.Background(), "", quiet)
	if err != nil {
		t.Fatalf("numberThisNode: %v", err)
	}
	for _, c := range []struct{ name, got, want string }{
		{"SystemCIDR", got.SystemCIDR, "10.173.94.1/24"},
		{"SystemHostCIDR", got.SystemHostCIDR, "10.173.94.129/32"},
		{"PrivHostCIDR", got.PrivHostCIDR, "10.11.203.1/24"},
		{"WitnessCIDR", got.WitnessCIDR, "10.11.203.2/24"},
		{"PodSubnet", got.PodSubnet, "10.12.7"},
	} {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.name, c.got, c.want)
		}
	}
}

// ⚠️ THE GUARD THAT KEEPS A RIG'S SUBSTRATE ITS OWN. A config that already names this node's
// address has been numbered by somebody else -- a rig that builds its own devices, an operator, a
// pairing's mesh spec -- and drawing there would be us overruling them. Nothing is touched, and no
// record is written.
func TestNumberThisNodeLeavesAStatedAddressAlone(t *testing.T) {
	cfg := cfgAt(t)
	cfg.SystemCIDR = "10.0.0.3/24"
	got, err := cfg.numberThisNode(context.Background(), "", quiet)
	if err != nil {
		t.Fatalf("numberThisNode: %v", err)
	}
	if got.SystemCIDR != "10.0.0.3/24" {
		t.Errorf("SystemCIDR = %q, want the configured 10.0.0.3/24", got.SystemCIDR)
	}
	if got.SystemHostCIDR != "" || got.PrivHostCIDR != "" {
		t.Errorf("a stated address was topped up with derived ones: host=%q priv=%q",
			got.SystemHostCIDR, got.PrivHostCIDR)
	}
	if _, err := os.Stat(cfg.subnetsPath()); !os.IsNotExist(err) {
		t.Errorf("a node that was already numbered wrote a record at %s", cfg.subnetsPath())
	}
}

// The environment wins over the record, and what wins is what gets RECORDED: a range pinned on one
// boot and forgotten on the next must not renumber the node back to the recorded value in silence.
func TestNumberThisNodeRecordsThePin(t *testing.T) {
	cfg := cfgAt(t)
	if err := os.WriteFile(cfg.subnetsPath(),
		[]byte("SYSTEM_SUBNET=10.173.94\nPRIV_SUBNET=10.11.203\nPOD_SUBNET=10.12.7\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SYSTEM_SUBNET", "10.42.7")
	got, err := cfg.numberThisNode(context.Background(), "", quiet)
	if err != nil {
		t.Fatalf("numberThisNode: %v", err)
	}
	if got.SystemCIDR != "10.42.7.1/24" {
		t.Errorf("SystemCIDR = %q, want the pinned 10.42.7.1/24", got.SystemCIDR)
	}
	// The other two come from the record, untouched by the pin.
	if got.WitnessCIDR != "10.11.203.2/24" {
		t.Errorf("WitnessCIDR = %q, want the recorded 10.11.203.2/24", got.WitnessCIDR)
	}
	b, err := os.ReadFile(cfg.subnetsPath())
	if err != nil {
		t.Fatal(err)
	}
	want := "SYSTEM_SUBNET=10.42.7\nPRIV_SUBNET=10.11.203\nPOD_SUBNET=10.12.7\n"
	if string(b) != want {
		t.Errorf("record = %q, want %q", b, want)
	}
}

// A record that does not parse reads as ABSENT rather than as a value: the format's regexp is its
// assertion, and a node must never be numbered out of a typo. The well-formed line survives on its
// own, which is the same rule that lets a node predating a pool gain only that pool.
func TestRecordedSubnetsRejectsGarbage(t *testing.T) {
	cfg := cfgAt(t)
	if err := os.WriteFile(cfg.subnetsPath(),
		[]byte("SYSTEM_SUBNET=10.173.94.0/24\nPRIV_SUBNET=not-an-address\nPOD_SUBNET=10.12.7\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	d := cfg.recordedSubnets()
	if d.System != "" || d.Priv != "" {
		t.Errorf("a malformed record was read as a value: system=%q priv=%q", d.System, d.Priv)
	}
	if d.Pod != "10.12.7" {
		t.Errorf("the well-formed line was lost with the malformed ones: pod=%q", d.Pod)
	}
	if d.Complete() {
		t.Error("a record missing two of three ranges reported Complete")
	}
}

// A node with no pet state dir has nowhere to record and must still not wedge: it numbers itself
// for this life and says nothing. That is every unit test and every hand-run agent.
func TestRecordSubnetsWithoutAStateDir(t *testing.T) {
	var cfg Config // no AssignmentCache -> no path
	if p := cfg.subnetsPath(); p != "" {
		t.Fatalf("subnetsPath = %q with no state dir", p)
	}
	cfg.recordSubnets(cfg.recordedSubnets(), func(string, ...any) {
		t.Error("recording without a state dir should be silent, not an error line")
	})
}
