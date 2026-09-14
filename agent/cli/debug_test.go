package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// THE VERB MUST STAY INVISIBLE. `debug` is deliberately not a row in `commands`, and the value
// of that is only real if nothing prints it: a listing that mentions it turns "not part of the
// product" into "an option we did not explain". This asserts the absence from every surface a
// user can reach, so re-adding it to the table -- the natural thing for someone tidying up --
// fails here rather than shipping.
func TestDebugVerbIsUndocumented(t *testing.T) {
	var help bytes.Buffer
	usage(&help)
	if strings.Contains(help.String(), "debug") {
		t.Errorf("`briard` help must not mention debug:\n%s", help.String())
	}
	for _, c := range commands {
		if c.name == "debug" {
			t.Error("debug must not be a row in `commands` -- that table is the help")
		}
	}
	// `briard help debug` must answer the way it answers for any word that is not a command.
	var out, errb bytes.Buffer
	if code := runHelp(context.Background(), []string{"debug"}, &out, &errb); code != 2 {
		t.Errorf("`briard help debug` = %d, want 2 (unknown command)", code)
	}
	if !strings.Contains(errb.String(), "unknown command") {
		t.Errorf("`briard help debug` should say unknown command, got: %s", errb.String())
	}
}

// Hidden is not the same as absent: Main must still route it, or the support answer that names
// it is wrong. Checked by way of the usage error, which reaches no monitor and opens nothing --
// the success path needs a running guest and belongs to the rigs.
func TestDebugVerbIsReachable(t *testing.T) {
	for _, args := range [][]string{{"debug"}, {"debug", "nonsense"}} {
		var out, errb bytes.Buffer
		code := Main(context.Background(), args, &out, &errb)
		if code != 2 {
			t.Errorf("briard %v = %d, want 2", args, code)
		}
		if !strings.Contains(errb.String(), "usage: briard debug shell") {
			t.Errorf("briard %v should print its usage, got: %s", args, errb.String())
		}
		if strings.Contains(errb.String(), "unknown command") {
			t.Errorf("briard %v must be routed, not rejected as unknown", args)
		}
	}
}

// THE CLIENT CANNOT NAME A MONITOR ([B.142a]). The console socket is derived by the AGENT from
// its own QMP path, which is what keeps it inside the 0700 root directory Launch creates. A
// `-qmp` flag here would hand that choice back to the caller, and a console socket somewhere
// world-reachable is the one way this feature becomes the vulnerability it currently is not.
// The derivation itself is asserted in agent/platform (TestDebugConsolePathIsBesideTheMonitor).
func TestDebugShellCannotBePointedAtAMonitor(t *testing.T) {
	var out, errb bytes.Buffer
	code := Main(context.Background(), []string{"debug", "shell", "-qmp", "/tmp/anywhere.sock"}, &out, &errb)
	if code != 2 {
		t.Errorf("`briard debug shell -qmp ...` = %d, want 2 (no such flag)", code)
	}
	if !strings.Contains(errb.String(), "not defined: -qmp") {
		t.Errorf("-qmp should be rejected as an undefined flag, got: %s", errb.String())
	}
}
