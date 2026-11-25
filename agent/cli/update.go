package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"

	"briard.io/agent/install"
	"briard.io/agent/selfupdate"
	"briard.io/shared/api"
)

// runUpdate is `briard update` and `briard update -vm`: the human trigger of each release chain;
// and `briard update -catalog` (readCatalog), the list of apps read now.
//
// The verb names the chain, and the chain is the upgrade unit, not the side. Every briard binary
// the machine runs -- the agent, net-wrap, qemu and the guest's own binaries -- rides the briard
// chain; the VM image is the platform underneath them. So the thing a user updates to get newer
// briard is briard, whichever side of the virtio port the binary lands on. `host`/`guest`
// describe where code runs, and our code runs on both sides, so neither names a release line;
// `briard logs -host/-guest` keeps those words because there they name which journal to read,
// which genuinely is a side. `vm` is the design's word and deliberately not `machine` -- a machine
// is the user's box, and "update your machine" is the one reading this must not invite.
//
// The default is the argument: nearly every publish moves the briard chain and few move the VM,
// so the chain that moves on every publish is the bare verb and the rare one is a flag, and a
// reader needs no mapping, because the flag is the chain name and its absence is the other chain.
// Bare `briard update` therefore acts rather than printing the help -- the high-stakes one is the
// flagged one, and an update to `stable` is what the nightly timer does on its own anyway.
//
// The briard chain is not an injector, and that is the whole point. The other verbs speak
// api.Directive to the running agent over the admin socket; this chain must not, because the case
// it exists for is the agent being down -- an agent that cannot fetch its own replacement is
// exactly what the unit below it is for. So it does what the cloud's handler and the timer do,
// through the same door: leave the target as a message, start the update unit (the oneshot on
// Linux, the scheduled task on Windows), wait for its verdict, and print the one line the run
// ended on.
//
// The VM chain is an injector, and correctly so: the guest OS is moved by the agent (fetch and
// stage the image, swap, health-gate, commit or revert), so with the agent down there is nothing
// to do but bring the agent back. It submits the update-vm directive -- the same one the agent's
// own nightly timer submits -- and reports the upgrade's outcome the way `directive
// upgrade-system` would, with the release named. ⚠️ The directive kind's spelling
// (install.DirectiveUpdateVM) matters beyond this file: the fleet tests read it out of journals.
//
// The default is `stable`, on every path. `latest` exists to be proven before it is promoted, by
// a canary pinned to it deliberately; it is not somewhere to arrive by typing four words. Naming
// it explicitly (-to latest) is supported.
func runUpdate(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("briard update", flag.ContinueOnError)
	fs.SetOutput(stderr)
	vm := fs.Bool("vm", false, "update the VM briard runs apps in, instead of briard itself")
	catalog := fs.Bool("catalog", false, "read the catalog's list of apps now, instead of updating briard")
	base := fs.String("base", envOr("UPDATE_BASE", selfupdate.DefaultBase), "briard: where the committed agent lives")
	run := fs.String("run", envOr("UPDATE_RUN_DIR", selfupdate.DefaultRunDir), "briard: the dir the update messages pass through")
	unit := fs.String("unit", selfupdate.DefaultUpdateUnit, "briard: the update unit to start")
	sock := fs.String("sock", sockDefault(), "-vm, -catalog: the agent's admin socket")
	target := fs.String("to", install.TargetStable, "the release to converge to: stable, latest, dev, or an exact id")
	fs.Usage = func() {
		fmt.Fprintf(stderr, "Usage: briard update [-vm | -catalog] [options]\n\nOptions:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *catalog {
		return readCatalog(ctx, fs, *vm, *sock, stdout, stderr)
	}
	// No positional word at all: the chain is the flag, so a word here (`self`, `vm`, `host`,
	// `guest`) is refused rather than guessed at.
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "briard update: unexpected argument %q (the VM is `-vm`; briard itself is the bare verb)\n", fs.Arg(0))
		fs.Usage()
		return 2
	}
	if !*vm {
		l := selfupdate.New(*base, *run)
		line, err := l.Trigger(ctx, *unit, *target, true)
		if err != nil {
			fmt.Fprintf(stderr, "briard update: %s\n", line)
			return 1
		}
		fmt.Fprintln(stdout, line)
		return 0
	}
	fmt.Fprintf(stdout, "resolving vm/%s and upgrading if due (stage, then activate + health-gate; this can take minutes)\n", *target)
	o, err := submit(ctx, *sock, api.Directive{Kind: install.DirectiveUpdateVM, Payload: *target})
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
		// handover-required refusal (a node with a peer), which never touched anything -- so
		// the detail matters, not just the state.
		fmt.Fprintf(stderr, "not applied; the machine is unchanged and serving: %s\n", o.Detail)
		return 1
	default:
		fmt.Fprintf(stderr, "briard update -vm: %s\n", o.Detail)
		return 1
	}
}

// readCatalog is `briard update -catalog`: the list the node reads once a day, read now. It has no
// release to converge to -- which list a node reads is its CATALOG_URL, a setting, not something
// one update moves -- so -to is refused rather than ignored, as is -vm beside it.
func readCatalog(ctx context.Context, fs *flag.FlagSet, vm bool, sock string, stdout, stderr io.Writer) int {
	bad := ""
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "to" {
			bad = "-catalog reads the list this machine is set to read; it takes no -to"
		}
	})
	switch {
	case vm:
		bad = "-catalog and -vm are two updates; ask for one at a time"
	case fs.NArg() != 0:
		bad = fmt.Sprintf("unexpected argument %q", fs.Arg(0))
	}
	if bad != "" {
		fmt.Fprintf(stderr, "briard update: %s\n", bad)
		return 2
	}
	o, err := submit(ctx, sock, api.Directive{Kind: api.DirectiveCatalogRead})
	if err != nil {
		fmt.Fprintf(stderr, "briard: %v\n", err)
		return 1
	}
	if o.State != api.OutcomeDone {
		fmt.Fprintf(stderr, "briard update -catalog: %s\n", o.Detail)
		return 1
	}
	fmt.Fprintln(stdout, o.Detail)
	return 0
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
