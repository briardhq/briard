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

// runUpdate is `briard update <host|guest>`: the human trigger of each release chain.
//
// HOST ([B.86a]) is NOT an injector, and that is the whole point. The other verbs speak
// api.Directive to the running agent over the admin socket; this side must not, because the
// case it exists for is the agent being DOWN -- an agent that cannot fetch its own replacement
// is exactly what the unit below it is for. So it does what the cloud's handler and the timer
// do, through the same door: leave the target as a message, `systemctl start` the oneshot
// (which blocks), and print the one line the run ended on. The unit's exit status is the
// verb's, for free.
//
// GUEST ([B.86d]) IS an injector, and correctly so: the guest OS is moved by the agent (stage
// from the cache, switch or reboot, health-gate, commit or revert), so there is nothing to do
// when the agent is down but bring the agent back. It submits the update-guest directive --
// the same one the agent's own nightly timer submits -- and reports the upgrade's outcome the
// way `directive upgrade-system` would, with the release named.
//
// Bare `briard update` prints the help rather than defaulting to one side: a rare, high-stakes
// admin operation is not worth saving four characters over.
func runUpdate(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("briard update", flag.ContinueOnError)
	fs.SetOutput(stderr)
	base := fs.String("base", envOr("UPDATE_BASE", "/opt/briard/agent"), "host: where the committed agent lives")
	run := fs.String("run", envOr("UPDATE_RUN_DIR", selfupdate.DefaultRunDir), "host: the tmpfs dir the update messages pass through")
	unit := fs.String("unit", selfupdate.DefaultUpdateUnit, "host: the update unit to start")
	sock := fs.String("sock", sockDefault(), "guest: the agent's admin socket")
	target := fs.String("to", "latest", "the release to converge to: latest, stable, or an exact id")
	fs.Usage = func() {
		fmt.Fprintf(stderr, "Usage: briard update <host|guest> [options]\n\nOptions:\n")
		fs.PrintDefaults()
	}
	// The side comes FIRST (`briard update host -to stable`), the way every other verb takes its
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
	case "host":
		l := selfupdate.New(*base, *run)
		line, err := l.Trigger(ctx, *unit, *target)
		if err != nil {
			fmt.Fprintf(stderr, "briard update host: %s\n", line)
			return 1
		}
		fmt.Fprintln(stdout, line)
		return 0
	case "guest":
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
			fmt.Fprintf(stderr, "briard update guest: %s\n", o.Detail)
			return 1
		}
	default:
		fmt.Fprintf(stderr, "briard update: unknown side %q (host or guest)\n", side)
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
