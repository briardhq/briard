package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"briard.io/agent/install"
	"briard.io/agent/selfupdate"
	"briard.io/shared/api"
)

// runUpdate is `briard update <self|vm>`: the human trigger of each release chain.
//
// THE VERBS NAME THE AXIS, WHICH IS NOT host-vs-guest ([B.159](i)). Every briard binary the
// machine runs -- the agent, net-wrap, qemu AND the guest's own binaries ([B.86j]) -- rides the
// host bundle; the guest IMAGE is the platform underneath them. So the thing a user updates to
// get newer briard is `self`, whichever side of the virtio port the binary lands on, and `vm` is
// the platform it runs on. `host`/`guest` described where code sits, and our code sits on both
// sides. `self` is this repo's existing word for it (agent/selfupdate/); `vm` is DESIGN's, and it
// is deliberately NOT `machine` -- a machine is the user's box ([V3c.10]), and "update your
// machine" is the one reading this must not invite. `briard logs -host/-guest` keeps its words:
// there they name which journal to read, which genuinely is a side.
//
// SELF ([B.86a]) is NOT an injector, and that is the whole point. The other verbs speak
// api.Directive to the running agent over the admin socket; this side must not, because the
// case it exists for is the agent being DOWN -- an agent that cannot fetch its own replacement
// is exactly what the unit below it is for. So it does what the cloud's handler and the timer
// do, through the same door: leave the target as a message, `systemctl start` the oneshot
// (which blocks), and print the one line the run ended on. The unit's exit status is the
// verb's, for free.
//
// VM ([B.86d]) IS an injector, and correctly so: the guest OS is moved by the agent (stage
// from the cache, switch or reboot, health-gate, commit or revert), so there is nothing to do
// when the agent is down but bring the agent back. It submits the update-guest directive --
// the same one the agent's own nightly timer submits -- and reports the upgrade's outcome the
// way `directive upgrade-system` would, with the release named. ⚠️ The DIRECTIVE KIND keeps its
// name: it is a wire word, and the fleet tests read it out of journals.
//
// THE DEFAULT IS `stable`, on every path ([B.159](f)). `latest` exists to be proven before it is
// promoted, by a canary pinned to it deliberately; it is not somewhere to arrive by typing four
// words. Naming it explicitly stays supported -- only the default moved.
//
// Bare `briard update` prints the help rather than defaulting to one side: a rare, high-stakes
// admin operation is not worth saving four characters over.
func runUpdate(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("briard update", flag.ContinueOnError)
	fs.SetOutput(stderr)
	base := fs.String("base", envOr("UPDATE_BASE", "/opt/briard/agent"), "self: where the committed agent lives")
	run := fs.String("run", envOr("UPDATE_RUN_DIR", selfupdate.DefaultRunDir), "self: the tmpfs dir the update messages pass through")
	unit := fs.String("unit", selfupdate.DefaultUpdateUnit, "self: the update unit to start")
	sock := fs.String("sock", sockDefault(), "vm: the agent's admin socket")
	target := fs.String("to", install.TargetStable, "the release to converge to: stable, latest, or an exact id")
	fs.Usage = func() {
		fmt.Fprintf(stderr, "Usage: briard update <self|vm> [options]\n\nOptions:\n")
		fs.PrintDefaults()
	}
	// The side comes FIRST (`briard update self -to latest`), the way every other verb takes its
	// subject before its options; Go's flag package stops at the first positional word, so the
	// options are parsed from what follows it.
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		if err := fs.Parse(args); err != nil {
			return 2
		}
		fs.Usage()
		return 2
	}
	side, rest := args[0], args[1:]
	if err := fs.Parse(rest); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fs.Usage()
		return 2
	}
	switch side {
	case "self":
		l := selfupdate.New(*base, *run)
		line, err := l.Trigger(ctx, *unit, *target)
		if err != nil {
			fmt.Fprintf(stderr, "briard update self: %s\n", line)
			return 1
		}
		fmt.Fprintln(stdout, line)
		return 0
	case "vm":
		fmt.Fprintf(stdout, "resolving guest/%s and upgrading if due (stage, then activate + health-gate; this can take minutes)\n", *target)
		o, err := submit(ctx, *sock, api.Directive{Kind: install.DirectiveUpdateGuest, Payload: *target})
		if err != nil {
			fmt.Fprintf(stderr, "briard: %v\n", err)
			return 1
		}
		switch o.State {
		case api.OutcomeDone:
			fmt.Fprintln(stdout, o.Detail) // "already running …" or "now running …"
			return 0
		case api.OutcomeRolledBack:
			// The distinction an operator most needs, and the one the exit code alone cannot
			// carry: the node is back where it started rather than broken. That includes the
			// HA refusal, which never touched anything -- so the detail matters, not just the
			// state.
			fmt.Fprintf(stderr, "not applied; the node is unchanged and serving: %s\n", o.Detail)
			return 1
		default:
			fmt.Fprintf(stderr, "briard update vm: %s\n", o.Detail)
			return 1
		}
	default:
		fmt.Fprintf(stderr, "briard update: unknown side %q (self or vm)\n", side)
		fs.Usage()
		return 2
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
