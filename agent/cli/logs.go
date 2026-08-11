package cli

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// `briard alerts` and `briard logs` — the two verbs that READ the node instead of acting on it,
// and the first that are not directive injectors (see this package's doc).
//
// WHY THEY ARE NOT INJECTORS, which is not an implementation shortcut but the requirement:
// the outage they are most needed for is the host agent being down. A verb routed through the
// admin socket answers "cannot reach the agent" in exactly the case the operator is trying to
// investigate. Reading the surfaces directly means these two work on a node where nothing else
// does.
//
// WHY TWO SURFACES. Alerts on a node are written by processes that share no logger:
//
//	the host agent  → its journal (briard-agent.service): the redundancy alerter and every
//	                  failed upgrade / cert renewal / self-update (agent/host/alert.go, report.go)
//	the guest       → the deadman, which is INSIDE the VM. Its stderr goes to the guest's own
//	                  journal, out the guest's ttyS0, into a file on the host. Under macvtap the
//	                  host cannot reach the guest over the network at all, so that file is the
//	                  only witness to anything in there.
//
// A reader that tailed one of them would be silent about a whole class of trouble while looking
// like it had checked. That is why `alerts` reads both and says so per surface, including when a
// surface is unavailable — an empty result from a surface that could not be opened must never
// render as "nothing wrong".
//
// WHAT THEY DELIBERATELY DO NOT DO: push. Surfacing degradation to an owner who is not sitting
// at a terminal needs a channel that reaches them unprompted; that is a separate piece of work.
// These verbs are the PULL half — they make "where do I look" answerable, which it previously
// was not on a free node, where alerts are never delivered anywhere at all.

const (
	// The units that carry the host's own story: the agent (all the logic) and the VM it runs
	// (qemu's own output). Written here rather than imported from agent/platform, which owns
	// GuestUnit: this package is linked into the `-tags guest` binary, and platform pulls in the
	// QEMU launcher and net/http, which that build exists to leave out.
	agentUnit = "briard-agent.service"
	guestUnit = "briard-guest.service"

	// Where scripts/install.sh captures the guest's serial console by default (BRIARD_CONSOLE).
	// Only a fallback: consolePath asks systemd what the unit is ACTUALLY configured with, so a
	// node that moved or disabled it is reported truthfully rather than read from here.
	defaultConsole = "/var/log/briard-guest-console.log"

	// What an alert line begins with, on every surface. It is notify.LogMarker, copied for the
	// same reason as the unit names above — shared/notify reaches an ntfy endpoint over HTTP, so
	// importing it here would put net/http in the guest binary. The pairing is not left to trust:
	// a test in this package asserts the two are identical (tests are not the shipped binary, so
	// the test may import what the code may not).
	alertMarker = "alert ["
)

// A surface is one place log lines land on this node. Read is best-effort by contract: an error
// makes the surface unavailable, never the command, because one readable surface is worth more
// than a clean failure.
type surface struct {
	name  string // "host" / "guest" — the line prefix
	where string // human description, printed in the header
	lines []string
	err   error
}

// logSources is the set of things these verbs touch outside the process. Injected so the verbs
// are testable without systemd, a journal, or a guest.
type logSources struct {
	journal     func(ctx context.Context, args ...string) ([]byte, error)
	unitProps   func(ctx context.Context, unit string, props ...string) (map[string]string, error)
	readFile    func(path string) ([]byte, error)
	consoleFlag string // -console: an explicit path, bypassing resolution
	env         func(string) string
}

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
			// NOT --value: a unit that does not exist is reported by `systemctl show` with exit 0
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

// runAlerts is `briard alerts` — every alert this node has raised, from both surfaces.
//
// It scans a long window by default (30 days) rather than tailing a fixed number of lines,
// because alerts are RARE and the interesting one is usually old: a node that lost its second
// anchor three weeks ago has been one failure from an outage ever since, and a tail sized for
// ordinary log traffic would have scrolled that away. The cost of the long window is a slower
// journalctl, once, on a command a human runs by hand.
//
// EXIT CODE. 0 when at least one surface was read, 1 when none were — deliberately NOT "1 if
// there are warnings". Deciding whether a warning is still live means pairing it with a later
// recovery, and this command reads a text trail rather than alert state; a node whose warning
// was followed by "redundancy restored" is fine, and an exit code that called it broken would be
// wrong in the direction that trains people to ignore it.
func runAlerts(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("briard alerts", flag.ContinueOnError)
	fs.SetOutput(stderr)
	n := fs.Int("n", 20, "show at most this many of the most recent alerts per surface (0 = all)")
	since := fs.String("since", "30 days ago", "how far back to look (any journalctl --since spec)")
	console := fs.String("console", "", "read the guest console from this file instead of the node's configured one")
	only := surfaceFlags(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprint(stderr, "briard alerts: takes no arguments\n")
		return 2
	}
	src := defaultSources()
	src.consoleFlag = *console
	surfaces := src.collect(ctx, only(), 0, *since, alertMarker)
	return render(stdout, stderr, surfaces, *n, "no alerts on this node")
}

// runLogs is `briard logs` — the same two surfaces, unfiltered. The verb that makes a support
// request answerable ("send me what the node says") without the requester having to know that
// the story is split across a journal and a file only qemu writes.
func runLogs(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("briard logs", flag.ContinueOnError)
	fs.SetOutput(stderr)
	n := fs.Int("n", 100, "show the last this many lines per surface (0 = all)")
	since := fs.String("since", "", "only lines after this (any journalctl --since spec); host surface only")
	grep := fs.String("grep", "", "show only lines containing this substring")
	console := fs.String("console", "", "read the guest console from this file instead of the node's configured one")
	follow := fs.Bool("follow", false, "keep streaming new lines until interrupted")
	only := surfaceFlags(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprint(stderr, "briard logs: takes no arguments\n")
		return 2
	}
	src := defaultSources()
	src.consoleFlag = *console
	if *follow {
		return src.follow(ctx, only(), *grep, stdout, stderr)
	}
	// journalctl's own -n is used for the tail, so the journal isn't decoded further back than
	// asked for; the guest console is a plain file and is trimmed after reading.
	surfaces := src.collect(ctx, only(), *n, *since, *grep)
	return render(stdout, stderr, surfaces, *n, "nothing logged yet")
}

// surfaceFlags registers -host/-guest; chosen() resolves them once parsing is done. Neither flag
// means BOTH, which is the answer that is right when the operator does not yet know where to
// look -- i.e. every time this is reached for in anger. Both flags together also means both,
// since asking for everything is not a usage error.
func surfaceFlags(fs *flag.FlagSet) func() string {
	host := fs.Bool("host", false, "only the host agent's journal")
	guest := fs.Bool("guest", false, "only the guest's serial console")
	return func() string {
		switch {
		case *host && !*guest:
			return "host"
		case *guest && !*host:
			return "guest"
		default:
			return "both"
		}
	}
}

// collect reads the requested surfaces. filter, when non-empty, keeps only lines containing it.
// Errors are captured per surface rather than returned: the whole point is that a node with a
// broken journal still shows its guest console, and vice versa.
func (s *logSources) collect(ctx context.Context, only string, tail int, since, filter string) []surface {
	var out []surface
	if only != "guest" {
		out = append(out, s.readHost(ctx, tail, since, filter))
	}
	if only != "host" {
		out = append(out, s.readGuest(ctx, filter))
	}
	return out
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

// readGuest reads the guest's serial console file. Note what it does NOT do: apply -since. The
// file is a raw console capture whose lines are stamped by the guest kernel's monotonic clock
// (and, after boot, by nothing at all), so there is no wall-clock field to filter on. Saying so
// is better than filtering on a timestamp that does not mean what the flag implies.
func (s *logSources) readGuest(ctx context.Context, filter string) surface {
	path, note, err := s.consolePath(ctx)
	sf := surface{name: "guest", where: "console: " + path}
	if err != nil {
		sf.where = "console"
		sf.err = err
		return sf
	}
	if note != "" {
		sf.where = note
	}
	b, err := s.readFile(path)
	if err != nil {
		sf.err = fmt.Errorf("%v", err)
		return sf
	}
	sf.lines = filterLines(splitLines(string(b)), filter)
	return sf
}

// consolePath answers where this node's guest console actually is, asking SYSTEMD rather than
// assuming the default: the path is a unit environment variable the installer writes, and it can
// be moved or switched off entirely (BRIARD_CONSOLE=). A node with capture disabled gets a
// straight answer -- "not captured here" -- instead of a stale file from an earlier install,
// which would be the worst of the three outcomes: content, believed current, that is not.
//
// Returns (path, note, err). A non-empty note replaces the surface's header when the path was
// guessed rather than read from the unit, so the reader can tell one from the other.
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
		for kv := range strings.FieldsSeq(props["Environment"]) {
			if v, ok := strings.CutPrefix(kv, "GUEST_SERIAL="); ok {
				if v == "" {
					break
				}
				return v, "", nil
			}
		}
		return "", "", fmt.Errorf("this node does not capture the guest console "+
			"(%s sets no GUEST_SERIAL) — reinstall without BRIARD_CONSOLE= to enable it", agentUnit)
	}
	// No systemd to ask (a container, a test rig, not root). Fall back, but SAY it is a guess.
	path := s.env("GUEST_SERIAL")
	if path == "" {
		path = defaultConsole
	}
	return path, "console: " + path + " (guessed; could not ask systemd)", nil
}

// follow streams both surfaces until the context ends. Two mechanisms because the surfaces are
// two different things: the journal has its own follow, and the console is a file another
// process appends to, which is polled.
func (s *logSources) follow(ctx context.Context, only, filter string, stdout, stderr io.Writer) int {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	var mu sync.Mutex // one line at a time, so two streams don't interleave mid-line
	emit := func(name, line string) {
		if filter != "" && !strings.Contains(line, filter) {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		fmt.Fprintf(stdout, "%-5s | %s\n", name, line)
	}
	started := 0
	if only != "guest" {
		started++
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
	}
	if only != "host" {
		path, _, err := s.consolePath(ctx)
		if err != nil {
			fmt.Fprintf(stderr, "guest surface unavailable: %v\n", err)
		} else {
			started++
			wg.Go(func() { s.tailFile(ctx, path, func(l string) { emit("guest", l) }, stderr) })
		}
	}
	if started == 0 {
		return 1
	}
	wg.Wait()
	return 0
}

// tailFile polls a file for growth. Polling rather than inotify because the writer is qemu
// appending to a chardev and the file is ROTATED by the agent's ExecStartPre: a poll that
// notices the file shrank simply starts over, where a watch on the old inode would go quiet
// forever without ever reporting that it had.
func (s *logSources) tailFile(ctx context.Context, path string, emit func(string), stderr io.Writer) {
	f, err := os.Open(path)
	if err != nil {
		fmt.Fprintf(stderr, "guest surface unavailable: %v\n", err)
		return
	}
	defer f.Close()
	// Start at the end: -follow is about what happens next. The preceding lines are what the
	// non-following form is for.
	off, _ := f.Seek(0, io.SeekEnd)
	rd := bufio.NewReader(f)
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	for {
		for {
			line, err := rd.ReadString('\n')
			if err != nil {
				break
			}
			off += int64(len(line))
			emit(strings.TrimRight(line, "\r\n"))
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		if fi, err := os.Stat(path); err == nil && fi.Size() < off {
			// Rotated out from under us. Reopen and read from the top of the new file.
			if nf, err := os.Open(path); err == nil {
				f.Close()
				f = nf
				rd = bufio.NewReader(f)
				off = 0
			}
		}
	}
}

// render prints the surfaces, newest last, at most tail lines each.
//
// It prints a HEADER PER SURFACE even when that surface is empty or broken, and that is
// load-bearing rather than decoration: the failure this command must not have is looking like it
// checked everything when one surface was unreadable. Silence about a surface reads as "clean"
// to every operator who has ever run a log command.
//
// The same reasoning governs the all-clear line, which was wrong on its first run and is the
// reason this note exists. "no alerts on this node" is a claim about the NODE, and it is only
// true when every surface was read; with one surface down the honest sentence is narrower, and
// naming which surface is missing is the difference between a report and a reassurance.
func render(stdout, stderr io.Writer, surfaces []surface, tail int, empty string) int {
	ok, total := 0, 0
	var missing []string
	for _, sf := range surfaces {
		fmt.Fprintf(stdout, "── %s (%s)\n", sf.name, sf.where)
		if sf.err != nil {
			fmt.Fprintf(stdout, "   unavailable: %v\n", sf.err)
			missing = append(missing, sf.name)
			continue
		}
		ok++
		lines := sf.lines
		if tail > 0 && len(lines) > tail {
			lines = lines[len(lines)-tail:]
		}
		total += len(lines)
		for _, l := range lines {
			fmt.Fprintf(stdout, "%-5s | %s\n", sf.name, l)
		}
	}
	if ok == 0 {
		fmt.Fprint(stderr, "briard: no log surface on this node could be read (are you root?)\n")
		return 1
	}
	if total == 0 {
		if len(missing) > 0 {
			fmt.Fprintf(stdout, "\n%s — but the %s surface could not be read, so this is not the whole node.\n",
				empty, strings.Join(missing, " and "))
		} else {
			fmt.Fprintf(stdout, "\n%s.\n", empty)
		}
	}
	return 0
}

func splitLines(s string) []string {
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

func filterLines(lines []string, filter string) []string {
	if filter == "" {
		return lines
	}
	var out []string
	for _, l := range lines {
		if strings.Contains(l, filter) {
			out = append(out, l)
		}
	}
	return out
}
