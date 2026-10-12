package platform

import (
	"errors"
	"strings"
)

// transient is a transient unit's command line read back into the three things a service manager
// other than systemd can honour: the NAME it is found by again, the file its own output goes to,
// and what it runs.
//
// The callers render systemd-run's argv and nothing else, on purpose: launchArgs and
// StartForwarder are pure renderers whose unit tests are the only check on the stop contract, and
// a second, per-OS renderer beside each would be two descriptions of one unit that can drift. So
// a backend that is not systemd reads the one rendering back. The properties it has no use for
// (Restart=no is a Job Object's default; Description is decoration) are skipped rather than
// refused, and ExecStop is skipped too -- on Windows the agent itself is told about a shutdown,
// which is where the clean stop lives there.
type transient struct {
	unit string
	log  string // StandardOutput/StandardError's append: target; "" = the manager's default
	exec []string
}

// parseTransient reads a systemd-run argv: options up to "--", the command after it. An argv with
// no unit name or no command is refused -- a unit that cannot be found again cannot be re-adopted,
// and one that runs nothing is not a unit.
func parseTransient(args []string) (transient, error) {
	var t transient
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			t.exec = args[i+1:]
			i = len(args)
		case strings.HasPrefix(a, "--unit="):
			t.unit = strings.TrimPrefix(a, "--unit=")
		case a == "-p" && i+1 < len(args):
			i++
			k, v, _ := strings.Cut(args[i], "=")
			if (k == "StandardOutput" || k == "StandardError") && strings.HasPrefix(v, "append:") {
				t.log = strings.TrimPrefix(v, "append:")
			}
		}
	}
	if t.unit == "" || len(t.exec) == 0 {
		return t, errors.New("platform: a transient unit needs --unit= and a command after --")
	}
	return t, nil
}
