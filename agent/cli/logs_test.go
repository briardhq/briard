package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"briard.io/agent/guestagent/deadman"
	"briard.io/agent/host"
	"briard.io/shared/notify"
)

// The three constants copied out of packages this one must not import, asserted against their
// originals. Each copy exists for a stated reason (the guest binary must not link
// net/http, which shared/notify pulls in via Ntfy); this test is what makes the copy safe rather
// than a promise. A TEST may import what the shipped code may not, since it is not in the binary.
//
// If this fails, `briard alerts` has silently stopped matching what the emitters write -- which
// presents to a user as a node with no alerts, the single most dangerous wrong answer this
// command can give.
func TestAlertMarkerMatchesEmitters(t *testing.T) {
	if alertMarker != notify.LogMarker {
		t.Errorf("cli marker %q != notify.LogMarker %q", alertMarker, notify.LogMarker)
	}
	// The host agent's own line must contain it.
	line := notify.LogLine(notify.Alert{Level: notify.Warning, Title: "t", Body: "b"})
	if !strings.Contains(line, alertMarker) {
		t.Errorf("host alert line %q does not contain the marker %q", line, alertMarker)
	}
	// So must the guest deadman's, which formats the shape by hand (guestagent.RunDeadman).
	guest := fmt.Sprintf("briard-deadman: alert [%s] %s", deadman.LevelWarning, "briard n1: host agent unreachable")
	if !strings.Contains(guest, alertMarker) {
		t.Errorf("guest deadman line %q does not contain the marker %q", guest, alertMarker)
	}
}

// fakeSources builds a logSources whose surfaces are canned, so the verbs are exercised with no
// systemd, no journal and no guest.
func fakeSources(t *testing.T, journal string, journalErr error, consoleBody string) *logSources {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "guest-console.log")
	if consoleBody != "" {
		if err := os.WriteFile(path, []byte(consoleBody), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// A node's console path lives in ITS CONFIG FILE, not on its unit ([B.150](a)) -- so the fake
	// is shaped the way a shipped node is: the unit names the config file, the config file names
	// the console. A fake that put GUEST_SERIAL on the unit is exactly what hid [B.157]'s bug.
	conf := filepath.Join(dir, "config.env")
	if err := os.WriteFile(conf, []byte("NODE=n1\nGUEST_SERIAL="+path+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return &logSources{
		journal: func(context.Context, ...string) ([]byte, error) {
			return []byte(journal), journalErr
		},
		unitProps: func(context.Context, string, ...string) (map[string]string, error) {
			return map[string]string{"LoadState": "loaded", "Environment": "BRIARD_CONFIG=" + conf}, nil
		},
		readFile: os.ReadFile,
		env:      func(string) string { return "" },
	}
}

// The core promise: an alert raised INSIDE the guest is reported, not just the host agent's own.
// The two surfaces share no logger, so a reader that tailed the journal alone would be silent
// about the deadman -- the very alert this command was built for.
func TestAlertsReadsBothSurfaces(t *testing.T) {
	src := fakeSources(t,
		"2026-08-11T10:00:00+0000 n1 briard-agent[1]: alert [warning] Briard: reduced redundancy — node n1 lost a replica connection\n"+
			"2026-08-11T10:00:01+0000 n1 briard-agent[1]: observe tick\n",
		nil,
		"[   12.3] systemd[1]: Started briard-deadman.\n"+
			"[  942.1] briard-deadman: alert [warning] briard n1: host agent unreachable — degraded, holding\n")
	var out, errOut bytes.Buffer
	surfaces := src.collect(context.Background(), "both", 0, "", alertMarker)
	if code := render(&out, &errOut, surfaces, 20, "no alerts on this machine"); code != 0 {
		t.Fatalf("exit = %d (stderr %q), want 0", code, errOut.String())
	}
	got := out.String()
	if !strings.Contains(got, "reduced redundancy") {
		t.Errorf("host alert missing from output:\n%s", got)
	}
	if !strings.Contains(got, "host agent unreachable") {
		t.Errorf("GUEST deadman alert missing — the surface this command exists to add:\n%s", got)
	}
	if strings.Contains(got, "observe tick") {
		t.Errorf("non-alert line leaked into `alerts` output:\n%s", got)
	}
}

// An unreadable surface must never render as "nothing wrong". This is the failure mode the
// command is most likely to have in the field -- a journal it cannot read (not root) while the
// guest console is fine, or the reverse -- and the one that would quietly teach a user that
// their node is healthy.
func TestAlertsNeverClaimsCleanWithASurfaceDown(t *testing.T) {
	src := fakeSources(t, "", errors.New("exit status 1"), "")
	src.unitProps = func(context.Context, string, ...string) (map[string]string, error) {
		return map[string]string{"LoadState": "loaded", "Environment": "BRIARD_CONFIG=" + noConsole(t)}, nil // no GUEST_SERIAL
	}
	var out, errOut bytes.Buffer
	surfaces := src.collect(context.Background(), "both", 0, "", alertMarker)
	code := render(&out, &errOut, surfaces, 20, "no alerts on this machine")
	if code != 1 {
		t.Errorf("exit = %d, want 1 when NO surface could be read", code)
	}
	got := out.String() + errOut.String()
	for _, want := range []string{"unavailable", "no log surface"} {
		if !strings.Contains(got, want) {
			t.Errorf("output does not say the surfaces failed (missing %q):\n%s", want, got)
		}
	}
}

// One surface up, one down: report what was read, and say plainly that it is not the whole node.
func TestAlertsQualifiesTheAllClearWhenPartiallyBlind(t *testing.T) {
	src := fakeSources(t, "", nil, "")
	src.unitProps = func(context.Context, string, ...string) (map[string]string, error) {
		return map[string]string{"LoadState": "loaded", "Environment": "BRIARD_CONFIG=" + noConsole(t)}, nil // console not captured
	}
	var out, errOut bytes.Buffer
	surfaces := src.collect(context.Background(), "both", 0, "", alertMarker)
	if code := render(&out, &errOut, surfaces, 20, "no alerts on this machine"); code != 0 {
		t.Fatalf("exit = %d, want 0 with one surface readable", code)
	}
	got := out.String()
	if !strings.Contains(got, "not the whole machine") {
		t.Errorf("the all-clear was stated unqualified while a surface was down:\n%s", got)
	}
}

// A node that never captured the console is told so, and is NOT confused with a machine that has
// no briard on it at all -- different facts, different fixes.
func TestConsolePathDistinguishesNotInstalledFromNotCaptured(t *testing.T) {
	base := fakeSources(t, "", nil, "")
	t.Run("not installed", func(t *testing.T) {
		src := *base
		src.unitProps = func(context.Context, string, ...string) (map[string]string, error) {
			return map[string]string{"LoadState": "not-found", "Environment": ""}, nil
		}
		if _, _, err := src.consolePath(context.Background()); err == nil ||
			!strings.Contains(err.Error(), "not installed") {
			t.Errorf("err = %v, want it to say briard is not installed here", err)
		}
	})
	t.Run("not captured", func(t *testing.T) {
		src := *base
		src.unitProps = func(context.Context, string, ...string) (map[string]string, error) {
			return map[string]string{"LoadState": "loaded", "Environment": "BRIARD_CONFIG=" + noConsole(t)}, nil
		}
		if _, _, err := src.consolePath(context.Background()); err == nil ||
			!strings.Contains(err.Error(), "does not capture") {
			t.Errorf("err = %v, want it to say the console is not captured", err)
		}
	})
	t.Run("no systemd to ask", func(t *testing.T) {
		src := *base
		src.unitProps = func(context.Context, string, ...string) (map[string]string, error) {
			return nil, errors.New("systemctl: not found")
		}
		path, note, err := src.consolePath(context.Background())
		if err != nil || path != defaultConsole {
			t.Fatalf("path=%q err=%v, want the default path and no error", path, err)
		}
		if !strings.Contains(note, "guessed") {
			t.Errorf("note = %q, want it to admit the path was guessed", note)
		}
	})
	t.Run("-console wins", func(t *testing.T) {
		src := *base
		src.consoleFlag = "/tmp/elsewhere.log"
		src.unitProps = func(context.Context, string, ...string) (map[string]string, error) {
			t.Error("systemd was consulted despite an explicit -console")
			return nil, nil
		}
		if path, _, err := src.consolePath(context.Background()); err != nil || path != "/tmp/elsewhere.log" {
			t.Errorf("path=%q err=%v, want the flag's path", path, err)
		}
	})
}

// journalctl's empty-window marker is not content: an empty journal must reach the all-clear
// rather than counting as one line of output.
func TestEmptyJournalIsNotALine(t *testing.T) {
	src := fakeSources(t, "-- No entries --\n", nil, "")
	sf := src.readHost(context.Background(), 0, "", "")
	if len(sf.lines) != 0 {
		t.Errorf("lines = %q, want none for an empty journal", sf.lines)
	}
}

// `briard logs` is unfiltered where `briard alerts` is not: the ordinary lines an alert-only view
// drops are exactly what a support request needs.
func TestLogsShowsNonAlertLines(t *testing.T) {
	src := fakeSources(t, "2026-08-11T10:00:01+0000 n1 briard-agent[1]: observe tick\n", nil,
		"[   12.3] systemd[1]: Started briard-deadman.\n")
	var out, errOut bytes.Buffer
	surfaces := src.collect(context.Background(), "both", 100, "", "")
	if code := render(&out, &errOut, surfaces, 100, "nothing logged yet"); code != 0 {
		t.Fatalf("exit = %d (stderr %q)", code, errOut.String())
	}
	got := out.String()
	for _, want := range []string{"observe tick", "Started briard-deadman"} {
		if !strings.Contains(got, want) {
			t.Errorf("logs dropped %q:\n%s", want, got)
		}
	}
}

// -host / -guest restrict to one surface; neither flag reads both.
func TestSurfaceSelection(t *testing.T) {
	src := fakeSources(t, "2026-08-11T10:00:01+0000 n1 briard-agent[1]: host line\n", nil, "guest line\n")
	for _, tc := range []struct {
		only        string
		want, avoid string
	}{
		{"host", "host line", "guest line"},
		{"guest", "guest line", "host line"},
	} {
		t.Run(tc.only, func(t *testing.T) {
			var out, errOut bytes.Buffer
			surfaces := src.collect(context.Background(), tc.only, 100, "", "")
			render(&out, &errOut, surfaces, 100, "nothing")
			if got := out.String(); !strings.Contains(got, tc.want) || strings.Contains(got, tc.avoid) {
				t.Errorf("-%s read the wrong surfaces:\n%s", tc.only, got)
			}
		})
	}
}

// The tail bound applies per surface, keeping the MOST RECENT lines -- the end of a log is the
// part a reader wants.
func TestTailKeepsTheNewest(t *testing.T) {
	var j strings.Builder
	for i := range 10 {
		fmt.Fprintf(&j, "line-%d\n", i)
	}
	src := fakeSources(t, j.String(), nil, "")
	var out, errOut bytes.Buffer
	surfaces := src.collect(context.Background(), "host", 0, "", "")
	render(&out, &errOut, surfaces, 3, "nothing")
	got := out.String()
	if !strings.Contains(got, "line-9") || strings.Contains(got, "line-6") {
		t.Errorf("tail 3 did not keep the newest three:\n%s", got)
	}
}

// Usage errors exit 2, matching every other verb (a bad flag is not a node problem).
func TestReadVerbsRejectArguments(t *testing.T) {
	for _, verb := range []string{"alerts", "logs"} {
		var out, errOut bytes.Buffer
		if code := Main(context.Background(), []string{verb, "extra"}, &out, &errOut); code != 2 {
			t.Errorf("%s with a stray argument: exit = %d, want 2", verb, code)
		}
	}
}

// noConsole writes a config.env that switches the capture OFF -- GUEST_SERIAL named, and named
// EMPTY, which is the only thing that means off ([B.157]'s `declared`).
//
// ⚠️ NOT an absent file and NOT a missing key: both of those mean "take the shipped default", which
// is what an ordinary node has. Writing the fixture the other way is precisely the mistake that
// reached the runner -- the stub said "no console" where a real node says "the default one".
func noConsole(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.env")
	if err := os.WriteFile(p, []byte("NODE=n1\nGUEST_SERIAL=\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// ⚠️ THE REGRESSION THIS FILE EXISTS FOR ([B.157]). consolePath asked systemd for GUEST_SERIAL,
// which was true until [B.150](a) moved the node's values off the frozen unit and into config.env
// -- after which `systemctl show` reported nothing and this verb told every installed node it
// captured no console while the capture sat on disk. It stayed green because the only test stubbed
// unitProps with a GUEST_SERIAL it had written itself.
//
// So the stub here carries what a SHIPPED unit carries: BRIARD_CONFIG and nothing else. A
// consolePath that goes back to reading GUEST_SERIAL from the unit fails this.
func TestConsolePathReadsTheNodesConfigFileNotTheUnit(t *testing.T) {
	dir := t.TempDir()
	conf := filepath.Join(dir, "config.env")
	if err := os.WriteFile(conf, []byte(
		"# briard node configuration\nNODE=briard-node-abc123\nGUEST_SERIAL=/var/log/elsewhere.log\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	src := fakeSources(t, "", nil, "")
	src.unitProps = func(context.Context, string, ...string) (map[string]string, error) {
		return map[string]string{"LoadState": "loaded", "Environment": "BRIARD_CONFIG=" + conf}, nil
	}
	path, note, err := src.consolePath(context.Background())
	if err != nil {
		t.Fatalf("consolePath: %v", err)
	}
	if path != "/var/log/elsewhere.log" {
		t.Errorf("path = %q, want the config file's GUEST_SERIAL", path)
	}
	if note != "" {
		t.Errorf("note = %q, want none -- the path was read, not guessed", note)
	}
}

// configValue is a SECOND reader of the format agent/host's loadConfigFile owns, and the four
// rules are the whole contract: blank lines and `#` comments ignored, first `=` splits, both
// halves trimmed, nothing else parsed. This table is the contract written down -- change either
// implementation and change this, or the two drift where nothing would notice.
func TestConfigValueFollowsTheFormatsFourRules(t *testing.T) {
	conf := filepath.Join(t.TempDir(), "config.env")
	if err := os.WriteFile(conf, []byte(strings.Join([]string{
		"# a comment that mentions GUEST_SERIAL=/wrong/path",
		"",
		"   ",
		"  SPACED   =   /var/log/spaced.log   ",
		"EMPTY=",
		"NOEQUALS",
		"VIP_ADDR=192.168.1.50/24",
		"URL=http://host:8099/x=y",
		"# DUPLICATE=first-wins does not apply to a commented line",
		"DUPLICATE=first",
		"DUPLICATE=second",
	}, "\n")), 0o600); err != nil {
		t.Fatal(err)
	}
	// ⚠️ THE THIRD COLUMN IS THE ONE THAT MATTERED. "named, and named empty" and "not named at all"
	// are different answers -- the first is a decision to switch a thing off, the second means take
	// the shipped default -- and collapsing them is the bug that reached the runner.
	for _, c := range []struct {
		key, want string
		named     bool
	}{
		{"SPACED", "/var/log/spaced.log", true}, // both halves trimmed
		{"EMPTY", "", true},                     // NAMED empty: a decision, not an absence
		{"NOEQUALS", "", false},                 // a line with no `=` is skipped, not a key
		{"VIP_ADDR", "192.168.1.50/24", true},   // a `/` in a value is just a value
		{"URL", "http://host:8099/x=y", true},   // only the FIRST `=` splits
		{"DUPLICATE", "first", true},            // first wins, as the agent's env layering does
		{"GUEST_SERIAL", "", false},             // a comment is not a setting
		{"MISSING", "", false},                  // absent
	} {
		got, named := configValue(conf, c.key)
		if got != c.want || named != c.named {
			t.Errorf("configValue(%q) = (%q, %v), want (%q, %v)", c.key, got, named, c.want, c.named)
		}
	}
	if got, named := configValue(filepath.Join(t.TempDir(), "absent"), "GUEST_SERIAL"); got != "" || named {
		t.Errorf("a missing file read as %q, want empty", got)
	}
}

// The CLI's fallback config path must be the one the SHIPPED unit names, and the unit file is the
// thing an installed node actually gets -- so it is the pin, not a second literal in a comment.
// Same discipline as the alertMarker pairing above: the test may read what the code may not.
func TestDefaultConfigFileMatchesTheShippedUnit(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "scripts", "units", "briard-agent.service"))
	if err != nil {
		t.Fatalf("read the shipped unit: %v", err)
	}
	want := "Environment=BRIARD_CONFIG=" + defaultConfigFile
	if !strings.Contains(string(b), want) {
		t.Errorf("the shipped unit does not carry %q -- the CLI's fallback and the installed "+
			"node's config path have drifted", want)
	}
}

// ⚠️ THE PIN THAT WOULD HAVE CAUGHT [B.157]'s RUNNER FAILURE, and the reason it is worth a test
// rather than a comment.
//
// `briard logs` resolves the console path from config.env, falling back to a literal copied here.
// The agent resolves the SAME question with its own default. While install.sh wrote GUEST_SERIAL
// into config.env the two never had to agree; the moment it stopped (defaults moved to config.go),
// the fallback became the answer for every ordinary node -- and a stale copy would tell a user
// their console is somewhere it is not, on the one verb they reach for when things are wrong.
//
// A test may import agent/host where the shipped code may not (same rule as alertMarker above):
// this is not in the binary, so the five-dependency budget is unaffected.
func TestDefaultConsoleMatchesTheAgents(t *testing.T) {
	// Cleared, so what is compared is the AGENT'S DEFAULT and not this process's environment.
	os.Unsetenv("GUEST_SERIAL")
	t.Setenv("BRIARD_CONFIG", filepath.Join(t.TempDir(), "no-such-config.env"))
	if got := host.ConfigFromEnv().SerialLog; got != defaultConsole {
		t.Errorf("the CLI falls back to %q while the agent captures to %q", defaultConsole, got)
	}
}

// ⚠️ THE ORDINARY NODE, and the case no unit test covered until the rig failed on it.
//
// config.env carries GUEST_SERIAL only when an operator named one ([B.157]), so on every normal
// install the key is simply ABSENT and the AGENT's default decides where the console goes. A reader
// that treats absent as "capture is off" contradicts the file sitting on disk -- which is exactly
// what `briard logs` did on install-macvtap: the console was there, non-empty, correctly moded, and
// the verb said the node did not capture one.
//
// The fixture is therefore a REAL node's config.env: identifiers, no GUEST_SERIAL line.
func TestConsolePathDefaultsWhenTheConfigNamesNoConsole(t *testing.T) {
	conf := filepath.Join(t.TempDir(), "config.env")
	if err := os.WriteFile(conf, []byte("NODE=briard-node-abc123\nFLOCK_NAME=brave-elf\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	src := fakeSources(t, "", nil, "")
	src.unitProps = func(context.Context, string, ...string) (map[string]string, error) {
		return map[string]string{"LoadState": "loaded", "Environment": "BRIARD_CONFIG=" + conf}, nil
	}
	path, note, err := src.consolePath(context.Background())
	if err != nil {
		t.Fatalf("consolePath: %v -- an ordinary node was told it captures no console", err)
	}
	if path != defaultConsole {
		t.Errorf("path = %q, want the agent's default %q", path, defaultConsole)
	}
	if note != "" {
		t.Errorf("note = %q, want none -- this is the node's real path, not a guess", note)
	}
}
