package cli

import (
	"bytes"
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"briard.io/agent/host"
)

// filesSource answers readFile from a map; a path not in it does not exist.
func filesSource(files map[string]string) *logSources {
	return &logSources{
		readFile: func(p string) ([]byte, error) {
			if b, ok := files[p]; ok {
				return []byte(b), nil
			}
			return nil, &fs.PathError{Op: "open", Path: p, Err: fs.ErrNotExist}
		},
		env: func(string) string { return "" },
	}
}

// The agent rolls its log to `.1` at the cap, so the stretch just before a roll is in the older
// file. A reader of the live file alone loses it exactly when the node has been busy.
func TestHostReadsTheRolledGenerationFirst(t *testing.T) {
	src := filesSource(map[string]string{
		host.AgentLog() + ".1": "old 1\nold 2\n",
		host.AgentLog():        "new 1\n",
		qemuLog:                "qemu says\n",
	})
	got := src.hostSurfaces(context.Background(), 0, "", "")
	if len(got) != 2 || got[0].name != "host" || got[1].name != "qemu" {
		t.Fatalf("surfaces = %+v, want host then qemu", got)
	}
	if want := []string{"old 1", "old 2", "new 1"}; strings.Join(got[0].lines, "|") != strings.Join(want, "|") {
		t.Errorf("host lines = %q, want %q", got[0].lines, want)
	}
	if got[1].err != nil || len(got[1].lines) != 1 {
		t.Errorf("qemu = %+v, want its one line", got[1])
	}
}

// A log that has not rolled has no `.1`; that is not an error. A missing live log is: the surface
// is unavailable, and the other host file is still shown.
func TestHostMissingFileIsUnavailableNotEmpty(t *testing.T) {
	src := filesSource(map[string]string{host.AgentLog(): "agent line\n"})
	got := src.hostSurfaces(context.Background(), 0, "", "")
	if got[0].err != nil || len(got[0].lines) != 1 {
		t.Errorf("host = %+v, want its line and no error for a log that never rolled", got[0])
	}
	if got[1].err == nil {
		t.Errorf("qemu surface has no error with its file missing")
	}
	var out, errOut bytes.Buffer
	if code := render(&out, &errOut, got, 20, "nothing logged yet"); code != 0 ||
		!strings.Contains(out.String(), "unavailable") || !strings.Contains(out.String(), "agent line") {
		t.Errorf("exit %d, output:\n%s", code, out.String())
	}
}

// -since has nothing to ask on a file; the header says it was not applied rather than the verb
// quietly showing everything as though it had filtered.
func TestHostSaysSinceWasNotApplied(t *testing.T) {
	src := filesSource(map[string]string{host.AgentLog(): "a\n", qemuLog: "b\n"})
	for _, sf := range src.hostSurfaces(context.Background(), 0, "1 hour ago", "") {
		if !strings.Contains(sf.where, "-since not applied") {
			t.Errorf("%s header = %q, want it to say -since was not applied", sf.name, sf.where)
		}
	}
}

// The CLI's default console path is the agent's, on Windows as on Linux.
func TestDefaultConsoleMatchesTheAgents(t *testing.T) {
	os.Unsetenv("GUEST_SERIAL")
	t.Setenv("BRIARD_CONFIG", filepath.Join(t.TempDir(), "no-such-config.env"))
	if got := host.ConfigFromEnv().SerialLog; got != defaultConsole {
		t.Errorf("the CLI falls back to %q while the agent captures to %q", defaultConsole, got)
	}
	if filepath.Dir(host.AgentLog()) != filepath.Dir(qemuLog) {
		t.Errorf("qemu's log %q is not beside the agent's %q", qemuLog, host.AgentLog())
	}
}
