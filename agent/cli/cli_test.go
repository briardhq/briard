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

// `briard rescue` WITHOUT -yes must not reach the agent. This is the only destructive verb in the
// CLI -- everything else here is reversible or health-gated -- so the guard is asserted on the
// wire, not on the exit code: a version that printed the warning and submitted anyway would still
// exit non-zero on a refusing fake and look correct.
func TestRescueWithoutConfirmationSubmitsNothing(t *testing.T) {
	sock, seen := fakeAgent(t, api.DirectiveOutcome{State: api.OutcomeDone})
	var out, errOut bytes.Buffer
	code := Main(context.Background(), []string{"rescue", "-sock", sock}, &out, &errOut)
	if code != 2 {
		t.Errorf("exit = %d, want 2 (usage refusal)", code)
	}
	if ds := seen(); len(ds) != 0 {
		t.Fatalf("agent saw %+v -- an unconfirmed rescue reached the node", ds)
	}
	// The refusal has to answer the question an operator actually has before running this, which
	// is whether they are about to lose their data. If that sentence goes, the guard is still
	// there but the reason to trust it is not.
	if !strings.Contains(errOut.String(), "DATA IS NOT TOUCHED") {
		t.Errorf("refusal did not say the data survives; got:\n%s", errOut.String())
	}
}

// With -yes it submits exactly one rescue directive, and carries no payload -- the node rescues
// itself from its own disk, so there is nothing for a caller to name (or to get wrong).
func TestRescueConfirmedSubmitsOneDirective(t *testing.T) {
	sock, seen := fakeAgent(t, api.DirectiveOutcome{State: api.OutcomeDone})
	var out, errOut bytes.Buffer
	code := Main(context.Background(), []string{"rescue", "-yes", "-sock", sock}, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit = %d (stderr %q), want 0", code, errOut.String())
	}
	ds := seen()
	if len(ds) != 1 || ds[0].Kind != api.DirectiveRescue || ds[0].Payload != "" {
		t.Fatalf("agent saw %+v, want exactly one rescue with no payload", ds)
	}
}

// A refused rescue exits non-zero, so a script driving this can tell a rebuilt node from one that
// declined -- the CLI-wide contract, and it matters most on the verb whose failure means the node
// is still broken.
func TestRescueRefusedExitsNonZero(t *testing.T) {
	sock, _ := fakeAgent(t, api.DirectiveOutcome{State: api.OutcomeFailed, Detail: "not an overlay"})
	var out, errOut bytes.Buffer
	if code := Main(context.Background(), []string{"rescue", "-yes", "-sock", sock}, &out, &errOut); code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	if !strings.Contains(errOut.String(), "not an overlay") {
		t.Errorf("the node's reason was not surfaced; got %q", errOut.String())
	}
}

// TestNoArgsIsHelp: `briard` alone prints the help and SUCCEEDS. It used to start the privileged
// host agent ([V3b.23]) — a bare word away from the CLI, and the one invocation a curious user is
// most likely to try first. The daemon is `briard run` now, and nothing reaches this path
// expecting a process to stay up.
func TestNoArgsIsHelp(t *testing.T) {
	var bare, explicit, errOut bytes.Buffer
	if code := Main(context.Background(), nil, &bare, &errOut); code != 0 {
		t.Fatalf("exit = %d, want 0 — no arguments is the help, not a usage error", code)
	}
	if errOut.Len() != 0 {
		t.Errorf("stderr = %q, want the help on stdout (it was asked for, not complained about)", errOut.String())
	}
	if code := Main(context.Background(), []string{"help"}, &explicit, &errOut); code != 0 {
		t.Fatalf("`help` exit = %d, want 0", code)
	}
	if bare.String() != explicit.String() {
		t.Errorf("`briard` and `briard help` printed different things; they are the same request")
	}
}

// TestEveryCommandIsDocumented pins the user-visible surface to a list written HERE, not read
// from `commands`. That distinction is the whole test: dispatch and the help are one table now,
// so they cannot disagree with each other — but they can both be wrong together, and a test that
// iterates the table would pass while a verb quietly vanished from the product. Declaring the
// expected set independently is the repo's own rule (CONTRIBUTING, "tests declare their own
// values"): a test that inherits the product's list cannot see that list being wrong.
//
// Verified to fail: deleting the `logs` row from `commands` reds this test. The earlier draft,
// which walked `commands` at both ends, stayed green through exactly that deletion.
func TestEveryCommandIsDocumented(t *testing.T) {
	want := map[string]string{
		"alerts":    groupEveryday,
		"logs":      groupEveryday,
		"service":   groupEveryday,
		"handover":  groupEveryday,
		"dashboard": groupEveryday,
		"rescue":    groupRepair,
		"os":        groupRepair,
		"directive": groupRepair,
		"run":       groupRepair,
	}
	var listing bytes.Buffer
	usage(&listing)

	got := map[string]string{}
	for _, c := range commands {
		got[c.name] = c.group
	}
	for name, group := range want {
		if got[name] == "" {
			t.Errorf("`briard %s` is gone from the CLI; if that is deliberate, this test says so first", name)
			continue
		}
		if got[name] != group {
			t.Errorf("`briard %s` moved to the %q tier (was %q) — that is a product decision, not a refactor", name, got[name], group)
		}
	}
	for name := range got {
		if want[name] == "" {
			t.Errorf("`briard %s` was added without a line here; a new verb is a decision about the household's surface", name)
		}
	}
	for _, c := range commands {
		if c.synopsis == "" || c.group == "" {
			t.Errorf("command %q has no synopsis or no group, so it cannot appear in the help", c.name)
		}
		if !strings.Contains(listing.String(), c.name) {
			t.Errorf("command %q dispatches but the help never names it", c.name)
		}
		// `run` is the one row main() intercepts before the CLI is reached; everything else must
		// be reachable from Main, or the help is describing a word that does nothing.
		if c.run == nil && c.name != "run" {
			t.Errorf("command %q is documented but dispatches nowhere", c.name)
		}
		if c.run == nil {
			continue
		}
		var out, errOut bytes.Buffer
		if code := Main(context.Background(), []string{c.name, "--nonsense-flag"}, &out, &errOut); code == 0 {
			t.Errorf("`briard %s --nonsense-flag` exited 0; the verb is not parsing its own arguments", c.name)
		}
		if strings.Contains(errOut.String(), "unknown command") {
			t.Errorf("`briard %s` is documented but Main does not dispatch it", c.name)
		}
	}
}

// TestHelpForACommandPrintsItsOwnOptions: `help <cmd>` must show the flags the verb ACTUALLY
// parses. It gets them by running the verb's own flag set rather than restating them, so this
// asserts the delegation works — a hand-written option list is the drift this whole item is about.
func TestHelpForACommandPrintsItsOwnOptions(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := Main(context.Background(), []string{"help", "rescue"}, &out, &errOut); code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	for _, want := range []string{"rescue", "-yes", "-sock"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("`briard help rescue` never mentioned %q:\n%s", want, out.String())
		}
	}
	if code := Main(context.Background(), []string{"help", "frobnicate"}, &out, &errOut); code != 2 {
		t.Errorf("`briard help frobnicate` exit = %d, want 2", code)
	}
}

// TestServiceInstallPrintsWhereToReachIt: the CLI relays the agent's address line verbatim. It
// does NOT re-derive the URL -- the agent owns the manifest's port and the node's published name,
// so there is exactly one place the address can be wrong. The empty-Detail case is the contract
// with an older agent (and with a witness, which promises no URL): print the outcome, skip the
// line, never print a bare "reach it at".
func TestServiceInstallPrintsWhereToReachIt(t *testing.T) {
	for _, tc := range []struct {
		name, detail, want, absent string
	}{
		{"with a published name", "reach it at http://briard-picked-hornet.local:8123/",
			"reach it at http://briard-picked-hornet.local:8123/", ""},
		{"no published name", "it answers on port 8123", "it answers on port 8123", "://"},
		{"agent said nothing", "", "installed and healthy", "reach it at"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sock, _ := fakeAgent(t, api.DirectiveOutcome{State: api.OutcomeDone, Detail: tc.detail})
			var out, errOut bytes.Buffer
			if code := Main(context.Background(), []string{"service", "install", "-sock", sock, "home-assistant"}, &out, &errOut); code != 0 {
				t.Fatalf("exit = %d, want 0 (stderr: %q)", code, errOut.String())
			}
			if !strings.Contains(out.String(), tc.want) {
				t.Fatalf("stdout = %q, want it to mention %q", out.String(), tc.want)
			}
			if tc.absent != "" && strings.Contains(out.String(), tc.absent) {
				t.Fatalf("stdout = %q, must NOT contain %q", out.String(), tc.absent)
			}
		})
	}
}

func TestAccountLang(t *testing.T) {
	for in, want := range map[string]string{"el_GR.UTF-8": "el", "C.UTF-8": "en", "": "en", "de": "de", "POSIX": "en"} {
		t.Setenv("LANG", in)
		if got := accountLang(); got != want {
			t.Errorf("LANG=%q -> %q, want %q", in, got, want)
		}
	}
	t.Setenv("SUDO_USER", "kostas")
	if accountUser() != "kostas" || accountName() != "Kostas" {
		t.Errorf("under sudo: user %q name %q", accountUser(), accountName())
	}
}
