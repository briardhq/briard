package host

import (
	"os"
	"path/filepath"
	"testing"
)

// ⚠️ THE RULE THAT KEEPS FIVE RIGS ALIVE, and the one this session got wrong once already: the
// agent makes what it was TOLD about and nothing else. agent-bringup, agent-deadman, agent-readopt,
// agent-recover and agent-watchdog all run an agent whose guest has no state disk -- they say so by
// naming no path -- and a path invented here would hand qemu a `-drive` for a file nobody made.
func TestProvisionDisksMakesOnlyWhatItWasToldAbout(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{DataDisk: filepath.Join(dir, "data.img"), DataSize: "1G"} // no StateDisk
	if err := cfg.provisionDisks(func(string, ...any) {}); err != nil {
		t.Fatalf("provisionDisks: %v", err)
	}
	if fi, err := os.Stat(cfg.DataDisk); err != nil || fi.Size() != 1<<30 {
		t.Errorf("the data volume is %v (%v), want 1 GiB", fi, err)
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 1 {
		names := make([]string, 0, len(ents))
		for _, e := range ents {
			names = append(names, e.Name())
		}
		t.Errorf("provisioning made %v, want only the data volume it was given a path for", names)
	}
	// And told about neither: nothing at all, which is every agent-* rig.
	empty := t.TempDir()
	if err := (Config{}).provisionDisks(func(string, ...any) {}); err != nil {
		t.Fatalf("provisionDisks with no paths: %v", err)
	}
	if ents, _ := os.ReadDir(empty); len(ents) != 0 {
		t.Errorf("a config naming no disks still made %d file(s)", len(ents))
	}
}

// The size is the one value here the agent defaults, so a config.env that names a bad one must
// refuse rather than silently pick 4G -- a data volume is not a thing to guess the size of.
func TestProvisionDisksRefusesAnUnreadableSize(t *testing.T) {
	cfg := Config{DataDisk: filepath.Join(t.TempDir(), "data.img"), DataSize: "4.5 gigs"}
	err := cfg.provisionDisks(func(string, ...any) {})
	if err == nil {
		t.Fatal("a malformed DATA_SIZE was accepted")
	}
	if _, statErr := os.Stat(cfg.DataDisk); !os.IsNotExist(statErr) {
		t.Error("a volume was created despite the refusal")
	}
}

func TestParseSize(t *testing.T) {
	for _, c := range []struct {
		in   string
		want int64
		ok   bool
	}{
		{"4G", 4 << 30, true},
		{"1g", 1 << 30, true},
		{"64G", 64 << 30, true},
		{"4", 0, false},    // no unit: ambiguous, and this is a disk size
		{"4M", 0, false},   // whole GiB only -- the installer's knob never took anything else
		{"4.5G", 0, false}, // not a whole number
		{"0G", 0, false},   // a zero-byte data volume is not a smaller one, it is a broken node
		{"-4G", 0, false},  // likewise
		{"", 0, false},     // unset reaches here only if somebody wrote DATA_SIZE= on purpose
		{"lots", 0, false}, //
	} {
		got, err := parseSize(c.in)
		if c.ok && (err != nil || got != c.want) {
			t.Errorf("parseSize(%q) = (%d, %v), want (%d, nil)", c.in, got, err, c.want)
		}
		if !c.ok && err == nil {
			t.Errorf("parseSize(%q) = (%d, nil), want a refusal", c.in, got)
		}
	}
}
