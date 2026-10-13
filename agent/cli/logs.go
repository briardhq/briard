package cli

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"briard.io/agent/host"
	"briard.io/shared/notify"
)

// `briard alerts` and `briard logs` — the two verbs that READ the node instead of acting on it,
// and the first that are not directive injectors (see this package's doc).
//
// WHY THEY ARE NOT INJECTORS, which is not an implementation shortcut but the requirement:
// the outage they are most needed for is the host agent being down. A verb routed through the
// admin socket answers "cannot reach the agent" in exactly the case the operator is trying to
// investigate. Reading the node's files directly means these two work on a node where nothing
// else does.
//
// `alerts` reads the node's ALERT STORE (agent/host/alertstore.go): the one record of every
// alert the host has raised, which is also where "what is wrong right now" is answered -- the
// latest record per key. Nothing reads a log for an alert.
//
// `logs` reads the node's TWO LOG SURFACES, written by processes that share no logger:
//
//	the host agent  → its journal (briard-agent.service); on Windows, its own log file and qemu's
//	the guest       → INSIDE the VM: its stderr goes to the guest's own journal, out the guest's
//	                  ttyS0, into a file on the host. Under macvtap the host cannot reach the
//	                  guest over the network at all, so that file is the only witness to
//	                  anything in there.
//
// A reader that tailed one of them would be silent about a whole class of trouble while looking
// like it had checked. That is why `logs` reads both and says so per surface, including when a
// surface is unavailable — an empty result from a surface that could not be opened must never
// render as "nothing logged".
//
// WHAT THEY DELIBERATELY DO NOT DO: push. Surfacing degradation to an owner who is not sitting
// at a terminal needs a channel that reaches them unprompted; that is a separate piece of work.
// These verbs are the PULL half — they make "where do I look" answerable.

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

// runAlerts is `briard alerts` — what is wrong right now, then what has happened: the node's
// alert store, read directly from the file so it answers while the agent is down.
//
// EXIT CODE. 0 when the store was read (or has never been written: nothing has happened), 1
// when it could not be — deliberately NOT "1 if something is open". A script that wants that
// can read the "open now" count; a command people run by hand when worried should not fail at
// them on top.
func runAlerts(_ context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("briard alerts", flag.ContinueOnError)
	fs.SetOutput(stderr)
	n := fs.Int("n", 20, "show at most this many of the most recent alerts in the history (0 = all)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprint(stderr, "briard alerts: takes no arguments\n")
		return 2
	}
	recs, err := host.ReadAlerts(host.AlertStorePath)
	return renderAlerts(stdout, stderr, recs, err, *n)
}

func renderAlerts(stdout, stderr io.Writer, recs []notify.Record, err error, tail int) int {
	switch {
	case errors.Is(err, os.ErrNotExist):
		fmt.Fprint(stdout, "no alerts on this machine: nothing has happened yet.\n")
		return 0
	case err != nil:
		fmt.Fprintf(stderr, "briard: the alert store could not be read (%s): %v\n", privHint, err)
		return 1
	}
	line := func(r notify.Record) string {
		sev := string(r.Severity)
		if r.Kind == notify.Resolved {
			sev = "resolved"
		}
		return fmt.Sprintf("%s  %-8s %-24s %s — %s", r.At.Local().Format("2006-01-02 15:04"), sev, r.Key, r.Title, r.Body)
	}
	open := notify.OpenNow(recs)
	fmt.Fprintf(stdout, "── open now (%d)\n", len(open))
	if len(open) == 0 {
		fmt.Fprint(stdout, "   nothing is wrong right now.\n")
	}
	for _, r := range open {
		fmt.Fprintf(stdout, "   %s\n", line(r))
	}
	hist := recs
	if tail > 0 && len(hist) > tail {
		hist = hist[len(hist)-tail:]
	}
	fmt.Fprintf(stdout, "── history (%d of %d)\n", len(hist), len(recs))
	for _, r := range hist {
		fmt.Fprintf(stdout, "   %s\n", line(r))
	}
	return 0
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
	host := fs.Bool("host", false, "only the host agent's log")
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
		out = append(out, s.hostSurfaces(ctx, tail, since, filter)...)
	}
	if only != "host" {
		out = append(out, s.readGuest(ctx, filter))
	}
	return out
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
		started += s.followHost(ctx, &wg, emit, stderr)
	}
	if only != "host" {
		path, _, err := s.consolePath(ctx)
		if err != nil {
			fmt.Fprintf(stderr, "guest surface unavailable: %v\n", err)
		} else {
			started++
			wg.Go(func() { s.tailFile(ctx, "guest", path, emit, stderr) })
		}
	}
	if started == 0 {
		return 1
	}
	wg.Wait()
	return 0
}

// tailFile polls a file for growth and emits its lines as the surface name. Polling rather than
// inotify because the writer is qemu appending to a chardev and the file is ROTATED by the agent's
// ExecStartPre (on Windows, the agent rolling its own log to `.1`): a poll that notices the file
// shrank simply starts over, where a watch on the old inode would go quiet forever without ever
// reporting that it had.
func (s *logSources) tailFile(ctx context.Context, name, path string, emit func(name, line string), stderr io.Writer) {
	f, err := os.Open(path)
	if err != nil {
		fmt.Fprintf(stderr, "%s surface unavailable: %v\n", name, err)
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
			emit(name, strings.TrimRight(line, "\r\n"))
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
// reason this note exists. "no alerts on this machine" is a claim about the MACHINE, and it is only
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
		fmt.Fprintf(stderr, "briard: no log surface on this machine could be read (%s)\n", privHint)
		return 1
	}
	if total == 0 {
		if len(missing) > 0 {
			fmt.Fprintf(stdout, "\n%s — but the %s surface could not be read, so this is not the whole machine.\n",
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

// configValue reads one key out of the node's config file (/opt/briard/config.env), the format
// install.sh writes and agent/host's loadConfigFile reads: KEY=value one per line, blank lines and
// `#` comments ignored, whitespace either side of the `=` trimmed, and nothing else parsed.
//
// A SECOND READER OF THE SAME FORMAT, and that is a real cost accepted for a reason. Importing
// agent/host here would pull the whole orchestrator -- drbd, platform, cloud, net/http -- into a
// package deliberately kept to five dependencies (the unit names above say why). The format is
// four rules, and the duplication is not left to trust: a test in this package feeds an awkward
// fixture to both implementations and asserts they agree. Keep it that way if either changes.
//
// ⚠️ IT REPORTS THREE STATES, NOT TWO, because the agent's own reader does (its `declared`):
// a key that is ABSENT takes the shipped default, a key set to EMPTY is a decision to switch the
// thing off, and those must not collapse into one answer. Collapsing them is exactly the bug that
// reached the runner: config.env stopped carrying GUEST_SERIAL (the agent defaults it), and a
// reader that read absent as "" told every node it captured no console.
//
// An unreadable or missing file is (", false) -- absent, so the caller takes the default, which is
// what the agent running on that node would have done too.
func configValue(path, key string) (string, bool) {
	f, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if ok && strings.TrimSpace(k) == key {
			return strings.TrimSpace(v), true
		}
	}
	_ = sc.Err()
	return "", false
}
