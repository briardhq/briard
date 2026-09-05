package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"briard.io/agent/selfupdate"
)

// runUpdate is `briard update host` ([B.86a]): the human trigger of the frozen update unit.
//
// It is NOT an injector, and that is the whole point. The other verbs speak api.Directive to
// the running agent over the admin socket; this one must not, because the case it exists for
// is the agent being DOWN -- an agent that cannot fetch its own replacement is exactly what the
// unit below it is for. So it does what the cloud's handler and the timer do, through the same
// door: leave the target as a message, `systemctl start` the oneshot (which blocks), and print
// the one line the run ended on. The unit's exit status is the verb's, for free.
//
// Bare `briard update` prints the help rather than defaulting to one side: a rare, high-stakes
// admin operation is not worth saving four characters over. `briard update guest` arrives with
// the guest chain ([B.86d]).
func runUpdate(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("briard update", flag.ContinueOnError)
	fs.SetOutput(stderr)
	base := fs.String("base", envOr("UPDATE_BASE", "/opt/briard/agent"), "where the committed agent lives")
	run := fs.String("run", envOr("UPDATE_RUN_DIR", selfupdate.DefaultRunDir), "the tmpfs dir the update messages pass through")
	unit := fs.String("unit", selfupdate.DefaultUpdateUnit, "the update unit to start")
	target := fs.String("to", "latest", "the release to converge to: latest, stable, or an exact id")
	fs.Usage = func() {
		fmt.Fprintf(stderr, "Usage: briard update host [options]\n\nOptions:\n")
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
	case "guest":
		fmt.Fprintln(stderr, "briard update guest: not built yet -- the guest OS still moves only when the cloud says so ([B.86d])")
		return 2
	default:
		fmt.Fprintf(stderr, "briard update: unknown side %q (host or guest)\n", side)
		fs.Usage()
		return 2
	}
	l := selfupdate.New(*base, *run)
	line, err := l.Trigger(ctx, *unit, *target)
	if err != nil {
		fmt.Fprintf(stderr, "briard update host: %s\n", line)
		return 1
	}
	fmt.Fprintln(stdout, line)
	return 0
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
