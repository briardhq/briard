package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"path/filepath"
	"strings"
	"testing"

	"briard.io/internal/testsock"
	"briard.io/shared/api"
)

// fakeAgent stands in for the host agent's admin door: accept one connection, decode the
// directive, reply with a canned outcome. Returns the socket path and the directives it saw.
func fakeAgent(t *testing.T, reply api.DirectiveOutcome) (string, func() []api.Directive) {
	t.Helper()
	sock := filepath.Join(testsock.Dir(t), "admin.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	seen := make(chan api.Directive, 8)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			var d api.Directive
			if err := json.NewDecoder(conn).Decode(&d); err == nil {
				seen <- d
				o := reply
				o.ID = d.ID
				_ = json.NewEncoder(conn).Encode(o)
			}
			conn.Close()
		}
	}()
	return sock, func() []api.Directive {
		close(seen)
		var out []api.Directive
		for d := range seen {
			out = append(out, d)
		}
		return out
	}
}

// TestDirectiveRoundTrip: the CLI submits what it was asked to and prints what came back.
func TestDirectiveRoundTrip(t *testing.T) {
	sock, seen := fakeAgent(t, api.DirectiveOutcome{State: api.OutcomeDone, Detail: "acked"})
	var out, errOut bytes.Buffer
	code := Main(context.Background(), []string{"directive", "-sock", sock, "log", "marker"}, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit = %d (stderr %q), want 0", code, errOut.String())
	}
	if got := out.String(); !strings.Contains(got, "done") || !strings.Contains(got, "acked") {
		t.Fatalf("stdout = %q, want the outcome state and detail", got)
	}
	ds := seen()
	if len(ds) != 1 || ds[0].Kind != "log" || ds[0].Payload != "marker" {
		t.Fatalf("agent saw %+v, want one log/marker directive", ds)
	}
}

// TestDirectiveExitsNonZeroWhenRefused is the assertion that makes the CLI automatable: a node
// that refused or reverted an op must be distinguishable from one that applied it, WITHOUT
// parsing prose. A tool that always exits 0 cannot be scripted against, and this is the seam the
// UI will drive.
func TestDirectiveExitsNonZeroWhenRefused(t *testing.T) {
	for _, state := range []string{api.OutcomeFailed, api.OutcomeRolledBack} {
		t.Run(state, func(t *testing.T) {
			sock, _ := fakeAgent(t, api.DirectiveOutcome{State: state, Detail: "nope"})
			var out, errOut bytes.Buffer
			if code := Main(context.Background(), []string{"directive", "-sock", sock, "upgrade", "x"}, &out, &errOut); code != 1 {
				t.Fatalf("exit = %d, want 1 for a %s outcome", code, state)
			}
			if !strings.Contains(out.String(), state) {
				t.Fatalf("stdout = %q, want it to name the %s state", out.String(), state)
			}
		})
	}
}

// TestDirectiveUnreachableAgent: the failure an operator actually hits first. The message has to
// name the path and the two real causes, or the next step is a guess.
func TestDirectiveUnreachableAgent(t *testing.T) {
	sock := filepath.Join(testsock.Dir(t), "absent.sock")
	var out, errOut bytes.Buffer
	if code := Main(context.Background(), []string{"directive", "-sock", sock, "noop"}, &out, &errOut); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	msg := errOut.String()
	for _, want := range []string{sock, "briard-agent", "root"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("stderr = %q, want it to mention %q", msg, want)
		}
	}
}

// TestUsageErrors: usage problems exit 2, distinct from an op the node refused (1). Conflating
// them would make "I typed it wrong" and "the node rolled back" the same event to a script.
func TestUsageErrors(t *testing.T) {
	sock, _ := fakeAgent(t, api.DirectiveOutcome{State: api.OutcomeDone})
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"no command", nil},
		{"unknown command", []string{"frobnicate"}},
		{"no kind", []string{"directive", "-sock", sock}},
		{"too many args", []string{"directive", "-sock", sock, "log", "a", "b"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			if code := Main(context.Background(), tc.args, &out, &errOut); code != 2 {
				t.Fatalf("exit = %d, want 2", code)
			}
		})
	}
}

// TestHelpSucceeds: `briard help` is a success, not a usage error, and names the socket so an
// operator can find the door without reading the source.
func TestHelpSucceeds(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := Main(context.Background(), []string{"help"}, &out, &errOut); code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(out.String(), defaultSock) {
		t.Fatalf("help = %q, want it to name %s", out.String(), defaultSock)
	}
}

// TestSockDefaultFollowsEnv: ADMIN_SOCK moves both halves of the door together — the agent reads
// it in ConfigFromEnv, the CLI here. Without this a non-default deployment would have a listener
// nobody could find.
func TestSockDefaultFollowsEnv(t *testing.T) {
	if got := sockDefault(); got != defaultSock {
		t.Fatalf("sockDefault() = %q, want %q", got, defaultSock)
	}
	t.Setenv("ADMIN_SOCK", "/tmp/elsewhere.sock")
	if got := sockDefault(); got != "/tmp/elsewhere.sock" {
		t.Fatalf("sockDefault() = %q, want the env override", got)
	}
}

// `briard handover` sends the right mode and, crucially, does NOT claim who took over: the node
// cannot see that (drbd-reactorctl evict says "not me", never "you"), so a CLI that named a
// successor would be inventing the one fact the operator ran the command for.
func TestHandoverModes(t *testing.T) {
	for _, c := range []struct {
		name    string
		args    []string
		payload string
	}{
		{"plain", []string{"handover"}, ""},
		{"keep-masked, for a node about to reboot", []string{"handover", "-keep-masked"}, "keep-masked"},
		{"unmask, the deliberate release", []string{"handover", "-unmask"}, "unmask"},
	} {
		t.Run(c.name, func(t *testing.T) {
			sock, seen := fakeAgent(t, api.DirectiveOutcome{State: api.OutcomeDone})
			var out, errOut bytes.Buffer
			code := Main(context.Background(), append(c.args, "-sock", sock), &out, &errOut)
			if code != 0 {
				t.Fatalf("exit = %d (stderr %q), want 0", code, errOut.String())
			}
			ds := seen()
			if len(ds) != 1 || ds[0].Kind != api.DirectiveHandover || ds[0].Payload != c.payload {
				t.Fatalf("agent saw %+v, want one handover with payload %q", ds, c.payload)
			}
		})
	}
}

func TestHandoverUsageErrors(t *testing.T) {
	for _, args := range [][]string{
		{"handover", "-keep-masked", "-unmask"}, // opposites
		{"handover", "node2"},                   // it evicts THIS node; naming another is a misunderstanding
	} {
		var out, errOut bytes.Buffer
		if code := Main(context.Background(), args, &out, &errOut); code != 2 {
			t.Errorf("%v exited %d, want 2 (usage)", args, code)
		}
	}
}

// A refused handover exits non-zero, like every other verb -- the property that makes the CLI
// scriptable, which the drill script depends on.
func TestHandoverExitsNonZeroWhenRefused(t *testing.T) {
	sock, _ := fakeAgent(t, api.DirectiveOutcome{State: api.OutcomeFailed, Detail: "no peer"})
	var out, errOut bytes.Buffer
	if code := Main(context.Background(), []string{"handover", "-sock", sock}, &out, &errOut); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(errOut.String(), "no peer") {
		t.Errorf("stderr = %q, want the reason", errOut.String())
	}
}

// `briard os upgrade` is the cloudless node's ONLY route to a new OS, and the operator's escape
// hatch on a managed one -- the per-home upgrade window binds the cloud's pusher, never a human
// at the machine.
func TestOSUpgradeSubmitsTheClosure(t *testing.T) {
	const closure = "/nix/store/abc-nixos-system"
	sock, seen := fakeAgent(t, api.DirectiveOutcome{State: api.OutcomeDone})
	var out, errOut bytes.Buffer
	if code := Main(context.Background(), []string{"os", "upgrade", "-sock", sock, closure}, &out, &errOut); code != 0 {
		t.Fatalf("exit = %d (stderr %q), want 0", code, errOut.String())
	}
	ds := seen()
	if len(ds) != 1 || ds[0].Kind != api.DirectiveUpgradeSystem || ds[0].Payload != closure {
		t.Fatalf("agent saw %+v, want one upgrade-system for %s", ds, closure)
	}
}

// A REFUSAL is not a failure, and the difference is what an operator most needs: on an HA pair a
// serving node declines (moving it is a handover), and the node is untouched and still serving.
// The exit code says "not applied"; the detail has to say which kind of not-applied.
func TestOSUpgradeDistinguishesARefusalFromABreakage(t *testing.T) {
	for _, c := range []struct{ state, detail, want string }{
		{api.OutcomeRolledBack, "reboot needs a handover", "unchanged and serving"},
		{api.OutcomeFailed, "staging failed", "upgrade failed"},
	} {
		sock, _ := fakeAgent(t, api.DirectiveOutcome{State: c.state, Detail: c.detail})
		var out, errOut bytes.Buffer
		code := Main(context.Background(), []string{"os", "upgrade", "-sock", sock, "/nix/store/x"}, &out, &errOut)
		if code != 1 {
			t.Errorf("%s exited %d, want 1", c.state, code)
		}
		if !strings.Contains(errOut.String(), c.want) || !strings.Contains(errOut.String(), c.detail) {
			t.Errorf("%s stderr = %q, want %q and the detail", c.state, errOut.String(), c.want)
		}
	}
}

// A typo is caught before anything is submitted: the agent's own refusal would arrive after a
// staging attempt, and "that is not a store path" is something a human should hear at once.
func TestOSUpgradeUsageErrors(t *testing.T) {
	for _, args := range [][]string{
		{"os"},                              // no subcommand
		{"os", "downgrade", "/nix/store/x"}, // not a verb
		{"os", "upgrade"},                   // no closure
		{"os", "upgrade", "/nix/store/a", "/nix/b"}, // two
		{"os", "upgrade", "nixos-system"},           // not a store path
	} {
		var out, errOut bytes.Buffer
		if code := Main(context.Background(), args, &out, &errOut); code != 2 {
			t.Errorf("%v exited %d, want 2 (usage)", args, code)
		}
	}
}
