package cli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
)

// The Linux arm of `briard logs`: the host surface is systemd's journal, and systemd is asked
// where the node's config file is.

const (
	// The units that carry the host's own story: the agent (all the logic) and the VM it runs
	// (qemu's own output). Literals here; agent/platform's GuestUnit is the agent's own name for
	// the second.
	agentUnit = "briard-agent.service"
	guestUnit = "briard-guest.service"

	// The agent's own default for the guest's serial console, pinned by
	// TestDefaultConsoleMatchesTheAgents (it equals what ConfigFromEnv answers with GUEST_SERIAL
	// unset). config.env carries the key only when the install was given one, so this is the path
	// on an ordinary node -- not a guess about one.
	defaultConsole = "/var/log/briard-guest-console.log"

	// The node's configuration file, for a node whose unit does not name it. Pinned by
	// TestDefaultConfigFileMatchesTheShippedUnit against the BRIARD_CONFIG the shipped
	// briard-agent.service carries.
	defaultConfigFile = "/opt/briard/config.env"

	privHint = "are you root?"
)

func defaultSources() *logSources {
	return &logSources{
		journal: func(ctx context.Context, args ...string) ([]byte, error) {
			// CombinedOutput: journalctl's refusals ("no journal files were found", a permission
			// error) go to stderr, and they are the message the operator needs, not noise to drop.
			return exec.CommandContext(ctx, "journalctl", args...).CombinedOutput()
		},
		unitProps: func(ctx context.Context, unit string, props ...string) (map[string]string, error) {
			args := []string{"show", unit}
			for _, p := range props {
				args = append(args, "--property="+p)
			}
			// Not --value: a unit that does not exist is reported by `systemctl show` with exit 0
			// and empty properties, which is indistinguishable from a unit that exists and is
			// configured with nothing. LoadState is what tells the two apart, so the properties are
			// read by name.
			out, err := exec.CommandContext(ctx, "systemctl", args...).Output()
			if err != nil {
				return nil, err
			}
			m := map[string]string{}
			for _, l := range splitLines(string(out)) {
				if k, v, ok := strings.Cut(l, "="); ok {
					m[k] = v
				}
			}
			return m, nil
		},
		readFile: os.ReadFile,
		env:      os.Getenv,
	}
}

// hostSurfaces is the host's story: one journal, both units.
func (s *logSources) hostSurfaces(ctx context.Context, tail int, since, filter string) []surface {
	return []surface{s.readHost(ctx, tail, since, filter)}
}

// followHost streams the journal; it reports how many streams it started.
func (s *logSources) followHost(ctx context.Context, wg *sync.WaitGroup, emit func(name, line string), stderr io.Writer) int {
	wg.Go(func() {
		cmd := exec.CommandContext(ctx, "journalctl", "-u", agentUnit, "-u", guestUnit,
			"--no-pager", "-o", "short-iso", "-f", "-n", "20")
		pipe, err := cmd.StdoutPipe()
		if err == nil {
			err = cmd.Start()
		}
		if err != nil {
			fmt.Fprintf(stderr, "host surface unavailable: %v\n", err)
			return
		}
		sc := bufio.NewScanner(pipe)
		for sc.Scan() {
			emit("host", sc.Text())
		}
		// A scan error here is the journal going away mid-stream, which is worth one line:
		// the alternative is a follow that silently stops producing and looks like a quiet node.
		if err := sc.Err(); err != nil {
			fmt.Fprintf(stderr, "host surface stopped: %v\n", err)
		}
		_ = cmd.Wait()
	})
	return 1
}

func (s *logSources) readHost(ctx context.Context, tail int, since, filter string) surface {
	sf := surface{name: "host", where: "journal: " + agentUnit + " + " + guestUnit}
	args := []string{"-u", agentUnit, "-u", guestUnit, "--no-pager", "-o", "short-iso"}
	if since != "" {
		args = append(args, "--since", since)
	}
	if tail > 0 {
		args = append(args, "-n", fmt.Sprint(tail))
	} else if since == "" {
		args = append(args, "--no-tail")
	}
	out, err := s.journal(ctx, args...)
	if err != nil {
		// journalctl's own words, not ours: "Operation not permitted" and "No journal files were
		// found" are different problems with different fixes, and paraphrasing loses that.
		sf.err = fmt.Errorf("journalctl: %v: %s", err, strings.TrimSpace(string(out)))
		return sf
	}
	lines := splitLines(string(out))
	// journalctl says "-- No entries --" on stdout when a window holds nothing. Passing it
	// through would make an empty journal look like one line of content, and the caller counts
	// lines to decide whether to print the all-clear -- so an empty result would never say so.
	if len(lines) == 1 && strings.TrimSpace(lines[0]) == "-- No entries --" {
		lines = nil
	}
	sf.lines = filterLines(lines, filter)
	return sf
}

// consolePath answers where this node's guest console actually is, asking the node rather than
// assuming the default: the path can be moved or switched off entirely (BRIARD_GUEST_SERIAL= at
// install), and a node with capture disabled gets a straight answer -- "not captured here" --
// instead of a stale file from an earlier install, the worst of the three outcomes: content,
// believed current, that is not.
//
// ⚠️ Two hops, both the node's own word: systemd is asked where the config file is
// (BRIARD_CONFIG on the unit), and the config file is asked where the console is (GUEST_SERIAL).
// Asking systemd for GUEST_SERIAL directly looks right and is wrong: the node's values live in
// config.env, not on the frozen unit, and `systemctl show` reports only what the unit declares,
// so every installed node would be told it captured no console while the capture sat on disk.
// TestConsolePathReadsTheNodesConfigFileNotTheUnit stubs a unit carrying only BRIARD_CONFIG, and
// nixosTest/install-macvtap.nix runs the verb on a real install. It returns (path, note, err): a
// non-empty note replaces the surface's header when the path was guessed rather than read from
// the node, so the reader can tell one from the other.
func (s *logSources) consolePath(ctx context.Context) (string, string, error) {
	if s.consoleFlag != "" {
		return s.consoleFlag, "", nil
	}
	props, err := s.unitProps(ctx, agentUnit, "LoadState", "Environment")
	if err == nil {
		// "briard is not installed here" and "briard is installed but captures no console" are
		// different facts with different fixes, and only LoadState separates them -- an absent
		// unit shows up as an empty Environment, which otherwise reads as the second.
		if ls := props["LoadState"]; ls != "" && ls != "loaded" {
			return "", "", fmt.Errorf("%s is not installed on this machine (LoadState=%s)", agentUnit, ls)
		}
		conf := defaultConfigFile
		for kv := range strings.FieldsSeq(props["Environment"]) {
			if v, ok := strings.CutPrefix(kv, "BRIARD_CONFIG="); ok && v != "" {
				conf = v
			}
		}
		// ⚠️ Absent is not off. config.env carries GUEST_SERIAL only when the install was given
		// one -- an ordinary node has no line, and the agent defaults it -- so a reader that
		// treated the missing key as "capture is off" would contradict the file sitting on disk.
		// Only an explicitly empty entry means off.
		path, named := configValue(conf, "GUEST_SERIAL")
		if !named {
			path = defaultConsole
		}
		if path != "" {
			return path, "", nil
		}
		return "", "", fmt.Errorf("this machine does not capture the guest console "+
			"(%s sets GUEST_SERIAL to nothing) — reinstall without BRIARD_GUEST_SERIAL= to enable it", conf)
	}
	// No systemd to ask (a container, a test rig, not root). Fall back, but say it is a guess.
	path := s.env("GUEST_SERIAL")
	if path == "" {
		path = defaultConsole
	}
	return path, "console: " + path + " (guessed; could not ask systemd)", nil
}
