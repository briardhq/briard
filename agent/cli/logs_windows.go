package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"sync"

	"briard.io/agent/host"
)

// The Windows arm of `briard logs`. The SCM keeps no journal, so every surface is a file the node
// writes itself: the agent's capped log (rolled to `.1`), qemu's own output, and the guest's serial
// capture. There is no service manager to ask where the config file is either; the tree is fixed.

const (
	// The agent's defaults, copied for the reason logs_linux.go gives and pinned the same way
	// (TestDefaultConsoleMatchesTheAgents).
	defaultConsole    = `C:\ProgramData\briard\log\guest-console.log`
	defaultConfigFile = `C:\ProgramData\briard\config.env`
	// qemuLog is where the guest unit's own stdout and stderr go: qemu's words, not the guest's.
	qemuLog = `C:\ProgramData\briard\log\briard-guest.log`

	privHint = "is this terminal running as administrator?"
)

func defaultSources() *logSources {
	return &logSources{readFile: os.ReadFile, env: os.Getenv}
}

// hostSurfaces is the host's story in two files, because two processes write it: the agent and
// qemu. Each is its own surface so one unreadable file never hides the other.
//
// -since is not applied, and the header says so: the journal answers it on Linux, and a file has
// nobody to ask. -grep on the date the agent's lines carry (2026/10/13) is the way to narrow.
func (s *logSources) hostSurfaces(_ context.Context, _ int, since, filter string) []surface {
	agent := surface{name: "host", where: "file: " + host.AgentLog()}
	agent.lines, agent.err = s.readRolled(host.AgentLog())
	qemu := surface{name: "qemu", where: "file: " + qemuLog}
	qemu.lines, qemu.err = s.readRolled(qemuLog)
	out := []surface{agent, qemu}
	for i := range out {
		out[i].lines = filterLines(out[i].lines, filter)
		if since != "" {
			out[i].where += " (-since not applied: a file has no journal to ask)"
		}
	}
	return out
}

// readRolled reads a log and the `.1` generation its writer rolled it to, oldest first. A missing
// `.1` is a log that has not rolled yet; a missing log is the surface being unavailable.
func (s *logSources) readRolled(path string) ([]string, error) {
	var lines []string
	if b, err := s.readFile(path + ".1"); err == nil {
		lines = splitLines(string(b))
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	b, err := s.readFile(path)
	if err != nil {
		return nil, err
	}
	return append(lines, splitLines(string(b))...), nil
}

// followHost tails both host files; it reports how many streams it started.
func (s *logSources) followHost(ctx context.Context, wg *sync.WaitGroup, emit func(name, line string), stderr io.Writer) int {
	wg.Go(func() { s.tailFile(ctx, "host", host.AgentLog(), emit, stderr) })
	wg.Go(func() { s.tailFile(ctx, "qemu", qemuLog, emit, stderr) })
	return 2
}

// consolePath answers where the guest console is from the node's own config file: GUEST_SERIAL
// when it names one, off when it is set empty, the agent's default when it is absent (logs_linux.go
// says why absent is not off).
func (s *logSources) consolePath(context.Context) (string, string, error) {
	if s.consoleFlag != "" {
		return s.consoleFlag, "", nil
	}
	path, named := configValue(defaultConfigFile, "GUEST_SERIAL")
	if !named {
		path = defaultConsole
	}
	if path == "" {
		return "", "", fmt.Errorf("this machine does not capture the guest console "+
			"(%s sets GUEST_SERIAL to nothing)", defaultConfigFile)
	}
	return path, "", nil
}
