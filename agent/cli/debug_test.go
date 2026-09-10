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

// The console socket is DERIVED from the monitor's path, never configured beside it. That is
// what keeps it in the 0700 root directory Launch creates: a second setting could point it at
// /tmp, and a world-reachable socket onto a root shell is the one way this feature becomes the
// vulnerability it currently is not.
func TestDebugConsoleLivesBesideTheMonitor(t *testing.T) {
	if got, want := qmpSockDefault(), defaultQMPSock; got != want {
		t.Errorf("qmpSockDefault() = %q, want %q", got, want)
	}
	t.Setenv("QMP_SOCK", "/run/elsewhere/qmp/guest.sock")
	if got, want := qmpSockDefault(), "/run/elsewhere/qmp/guest.sock"; got != want {
		t.Errorf("QMP_SOCK override = %q, want %q", got, want)
	}
}
