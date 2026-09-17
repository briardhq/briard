package host

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// identity minting has exactly two questions -- has somebody else named this node, and has this
// node named itself before -- and both answers are destructive to get wrong. A re-mint costs the
// node its own DRBD metadata ([V3.20]: the `on <name>` is keyed to it); a mint where a harness
// already said who it is hands every rig a random service MAC and a random mDNS name.

func idCfg(t *testing.T) Config {
	t.Helper()
	// The rigs name NODE; clear it so these tests exercise the SHIPPED path unless they say
	// otherwise. os.Unsetenv rather than t.Setenv("") -- an explicitly empty entry is a different
	// thing everywhere else in this file's neighbourhood, and here it would read as "named".
	os.Unsetenv("NODE")
	t.Cleanup(func() { os.Unsetenv("NODE") })
	return Config{AssignmentCache: filepath.Join(t.TempDir(), "assignment.json")}
}

// A node that has never minted gets all three, recorded, with the modes their audiences imply.
func TestMintIdentityGivesAVirginNodeAllThree(t *testing.T) {
	cfg := idCfg(t)
	got, err := cfg.mintIdentity(func(string, ...any) {})
	if err != nil {
		t.Fatalf("mintIdentity: %v", err)
	}
	if !regexp.MustCompile(`^briard-node-[0-9a-f]{6}$`).MatchString(got.Node) {
		t.Errorf("Node = %q, want briard-node-<6 hex>", got.Node)
	}
	if !regexp.MustCompile(`^[0-9a-f-]{36}$`).MatchString(got.FlockID) {
		t.Errorf("FlockID = %q, want a uuid", got.FlockID)
	}
	if len(got.FlockName) == 0 {
		t.Error("FlockName is empty -- the household has nothing to type")
	}
	dir := cfg.stateDir()
	for _, c := range []struct {
		name string
		mode os.FileMode
	}{{nodeIDName, 0o600}, {flockIDName, 0o600}, {flockNameName, 0o644}} {
		fi, err := os.Stat(filepath.Join(dir, c.name))
		if err != nil {
			t.Fatalf("%s was not recorded: %v", c.name, err)
		}
		if perm := fi.Mode().Perm(); perm != c.mode {
			t.Errorf("%s is mode %04o, want %04o", c.name, perm, c.mode)
		}
	}
}

// ⚠️ THE DESTRUCTIVE ONE. A second start must read the records, never mint over them: the
// replicated volume's metadata is keyed to the node id, so a fresh one is a node that no longer
// recognises its own disk.
func TestMintIdentityNeverRemints(t *testing.T) {
	cfg := idCfg(t)
	first, err := cfg.mintIdentity(func(string, ...any) {})
	if err != nil {
		t.Fatal(err)
	}
	second, err := cfg.mintIdentity(func(string, ...any) {})
	if err != nil {
		t.Fatal(err)
	}
	if second.Node != first.Node || second.FlockID != first.FlockID || second.FlockName != first.FlockName {
		t.Errorf("a second start re-minted: %+v then %+v",
			[]string{first.Node, first.FlockID, first.FlockName},
			[]string{second.Node, second.FlockID, second.FlockName})
	}
}

// ⚠️ THE GUARD EVERY RIG DEPENDS ON, and the same predicate the subnet draw uses: NODE in the
// environment means a harness owns this node's identity, so NOTHING is minted -- not the id it
// named, and not the two it did not.
//
// Minting a flock id beside a named node would give every agent-* rig a random service MAC where
// today it derives from the node name, and a random mDNS name where today it publishes nothing.
// Neither rig asks for either, and neither would notice until an assertion about a MAC moved.
func TestMintIdentityLeavesANamedNodeAlone(t *testing.T) {
	cfg := idCfg(t)
	t.Setenv("NODE", "n1")
	cfg.Node = "n1"
	got, err := cfg.mintIdentity(func(string, ...any) {})
	if err != nil {
		t.Fatalf("mintIdentity: %v", err)
	}
	if got.Node != "n1" || got.FlockID != "" || got.FlockName != "" {
		t.Errorf("a named node was given an identity: node=%q flock=%q name=%q",
			got.Node, got.FlockID, got.FlockName)
	}
	if ents, err := os.ReadDir(cfg.stateDir()); err != nil || len(ents) != 0 {
		t.Errorf("a named node recorded %d file(s) (err %v) -- it should write nothing", len(ents), err)
	}
}

// The records are the node's answer on every later start, ConfigFromEnv included -- which is how
// the value reaches DRBD, the VM UUID and the dashboard now that config.env does not carry it.
func TestConfigFromEnvReadsTheRecordedIdentity(t *testing.T) {
	dir := t.TempDir()
	for name, v := range map[string]string{
		nodeIDName:    "briard-node-abc123\n",
		flockIDName:   "11111111-2222-4333-8444-555555555555\n",
		flockNameName: "brave-elf\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(v), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	os.Unsetenv("NODE")
	t.Cleanup(func() { os.Unsetenv("NODE") })
	t.Setenv("ASSIGNMENT_CACHE", filepath.Join(dir, "assignment.json"))
	cfg := ConfigFromEnv()
	if cfg.Node != "briard-node-abc123" {
		t.Errorf("Node = %q, want the recorded id", cfg.Node)
	}
	if cfg.FlockID != "11111111-2222-4333-8444-555555555555" {
		t.Errorf("FlockID = %q, want the recorded one", cfg.FlockID)
	}
	if cfg.FlockName != "brave-elf" {
		t.Errorf("FlockName = %q, want the recorded one", cfg.FlockName)
	}
	// ...and the environment still wins over the record, as it does over every other source.
	t.Setenv("NODE", "n1")
	if got := ConfigFromEnv().Node; got != "n1" {
		t.Errorf("Node = %q, want the environment's n1 to beat the record", got)
	}
}

// A node told nothing and holding no record is still the literal every install answered to before
// [V3.20]. That fallback is what keeps a hand-run agent and the unit tests working.
func TestConfigFromEnvFallsBackToTheDefaultNodeName(t *testing.T) {
	os.Unsetenv("NODE")
	t.Cleanup(func() { os.Unsetenv("NODE") })
	t.Setenv("ASSIGNMENT_CACHE", filepath.Join(t.TempDir(), "assignment.json"))
	if got := ConfigFromEnv().Node; got != defaultNodeName {
		t.Errorf("Node = %q, want %q", got, defaultNodeName)
	}
}
