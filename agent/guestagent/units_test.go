package guestagent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// diskExec is fakeExec with a REAL WriteFile. The thing under test is a file systemd can load,
// so a fake that keeps writes in a map would let every assertion below pass over a renderer that
// wrote nothing at all -- which is exactly the vacuity this item was created by.
type diskExec struct{ fakeExec }

func (d *diskExec) WriteFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// guestWithTools points the renderer at a temporary unit directory and a temporary tool profile,
// and returns both. Every assertion below is about a real file on disk, because the thing under
// test is a file on disk: a unit systemd can load.
func guestWithTools(t *testing.T) (unitDir, tools string) {
	t.Helper()
	unitDir = t.TempDir()
	tools = filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(tools, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BRIARD_UNIT_DIR", unitDir)
	t.Setenv("BRIARD_TOOLS_BIN", tools)
	t.Setenv("BRIARD_BIN_DIR", "/var/lib/briard-bin")
	return unitDir, tools
}

// THE UNIT EXISTS, AND IT IS THE ONE THE HOST IS ABOUT TO START ([B.160]). The whole item is
// "the unit can never be older than the binary that wrote it", so what is asserted is that this
// binary's start produces a loadable briard-node-storage.service naming this binary's committed
// path and this image's tool profile -- the two halves the crash of [B.159](c) had disagree.
func TestWriteUnitsRendersNodeStorage(t *testing.T) {
	dir, tools := guestWithTools(t)
	f := &diskExec{}
	if err := WriteUnits(context.Background(), f); err != nil {
		t.Fatalf("WriteUnits: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "briard-node-storage.service"))
	if err != nil {
		t.Fatalf("the unit the host starts was not written: %v", err)
	}
	got := string(b)
	for _, want := range []string{
		"[Service]",
		"Type=oneshot",
		"RemainAfterExit=no",
		"Environment=PATH=" + tools,
		"ExecStart=/var/lib/briard-bin/briard-guest-agent --node-storage",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered unit is missing %q:\n%s", want, got)
		}
	}
	// NO [Install], which is the old `wantedBy = [ ]`: this unit runs when the host says so and
	// never at boot. A rendered [Install] would be silently inert here (nothing enables it) and
	// wrong the moment anything did.
	if strings.Contains(got, "[Install]") {
		t.Errorf("node storage must not be enablable:\n%s", got)
	}
}

// A UNIT WRITTEN AND NOT RELOADED IS A UNIT systemctl SAYS DOES NOT EXIST -- the same symptom
// [B.159](c) measured, reached by a different route. systemd reads unit files at load, so the
// reload is not housekeeping and its absence would pass every content assertion above.
func TestWriteUnitsReloadsSystemd(t *testing.T) {
	guestWithTools(t)
	f := &diskExec{}
	if err := WriteUnits(context.Background(), f); err != nil {
		t.Fatalf("WriteUnits: %v", err)
	}
	var reloaded bool
	for _, r := range f.runs {
		if len(r) == 2 && r[0] == "systemctl" && r[1] == "daemon-reload" {
			reloaded = true
		}
	}
	if !reloaded {
		t.Fatalf("no daemon-reload after writing the units; ran %v", f.runs)
	}
}

// AN IMAGE THAT PREDATES [B.160] HAS NO TOOL PROFILE, and the agent must refuse to serve on it
// rather than write units whose PATH resolves to nothing. That refusal is what makes a
// too-new agent safe: the trial fails, the picker restores the committed binary, and the node
// keeps running the release it had -- instead of promoting into units it cannot support.
func TestWriteUnitsRefusesAnImageWithNoToolProfile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIARD_UNIT_DIR", dir)
	t.Setenv("BRIARD_TOOLS_BIN", filepath.Join(t.TempDir(), "absent"))
	f := &diskExec{}
	err := WriteUnits(context.Background(), f)
	if err == nil {
		t.Fatal("rendered units against an image with no tool profile")
	}
	if !strings.Contains(err.Error(), "tool profile") {
		t.Errorf("the refusal must name what is missing, got %q", err)
	}
	// AND IT MUST NOT HAVE WRITTEN ANYTHING FIRST. A half-rendered set that then fails is worse
	// than none: the next start's reload would load whatever landed before the error.
	ents, _ := os.ReadDir(dir)
	if len(ents) != 0 {
		t.Errorf("refusing left %d file(s) behind in %s", len(ents), dir)
	}
}

// A TOOL PROFILE THAT IS A FILE, OR A DANGLING SYMLINK, IS NOT A TOOL PROFILE. Both are how a
// half-applied image presents, and both must route to the same refusal as absence rather than to
// a unit that fails at exec time on the promotion path.
func TestWriteUnitsRefusesAToolProfileThatIsNotADirectory(t *testing.T) {
	for _, tc := range []struct {
		name string
		make func(t *testing.T, base string) string
	}{
		{"a plain file", func(t *testing.T, base string) string {
			p := filepath.Join(base, "bin")
			if err := os.WriteFile(p, []byte("not a profile"), 0o644); err != nil {
				t.Fatal(err)
			}
			return p
		}},
		{"a dangling symlink", func(t *testing.T, base string) string {
			p := filepath.Join(base, "bin")
			if err := os.Symlink(filepath.Join(base, "gone"), p); err != nil {
				t.Fatal(err)
			}
			return p
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := t.TempDir()
			t.Setenv("BRIARD_UNIT_DIR", t.TempDir())
			t.Setenv("BRIARD_TOOLS_BIN", tc.make(t, base))
			if err := WriteUnits(context.Background(), &diskExec{}); err == nil {
				t.Fatal("rendered units against a tool profile that is not a directory")
			}
		})
	}
}

// THE RENDER IS IDEMPOTENT, because it runs on every start of the agent -- a crash-restart, a
// re-adopt, a trial that came back. Two renders must leave exactly the state one leaves, or the
// mechanism that removes the defect class introduces a new one.
func TestWriteUnitsIsIdempotent(t *testing.T) {
	dir, _ := guestWithTools(t)
	for i := 0; i < 2; i++ {
		if err := WriteUnits(context.Background(), &diskExec{}); err != nil {
			t.Fatalf("WriteUnits #%d: %v", i+1, err)
		}
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != len(units()) {
		t.Fatalf("two renders left %d files for %d units", len(ents), len(units()))
	}
}
