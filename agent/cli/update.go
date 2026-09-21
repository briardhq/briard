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

// runUpdate is `briard update` and `briard update --vm`: the human trigger of each release chain.
//
// THE VERB NAMES THE CHAIN, AND THE CHAIN IS THE UPGRADE UNIT, NOT THE SIDE ([B.163]). Every
// briard binary the machine runs -- the agent, net-wrap, qemu AND the guest's own binaries
// ([B.86j]) -- rides the briard chain; the VM image is the platform underneath them. So the thing
// a user updates to get newer briard is briard, whichever side of the virtio port the binary
// lands on, and the VM is the platform it runs on. `host`/`guest` describe where code RUNS, and
// our code runs on both sides, so neither names a release line; `briard logs -host/-guest` keeps
// those words because there they name which journal to read, which genuinely is a side. `vm` is
// DESIGN's word and is deliberately NOT `machine` -- a machine is the user's box ([V3c.10]), and
// "update your machine" is the one reading this must not invite.
//
// THE DEFAULT IS THE ARGUMENT. Nearly every publish moves the briard chain and few move the VM,
// so the chain that moves on every publish is the bare verb and the rare one is a flag: a reader
// needs no mapping, because the flag IS the chain name and its absence is the other chain. Bare
// `briard update` therefore acts rather than printing the help -- the high-stakes one is the
// flagged one, and an update to `stable` is what the nightly timer does on its own anyway.
//
// BRIARD ([B.86a]) is NOT an injector, and that is the whole point. The other verbs speak
// api.Directive to the running agent over the admin socket; this chain must not, because the
// case it exists for is the agent being DOWN -- an agent that cannot fetch its own replacement
// is exactly what the unit below it is for. So it does what the cloud's handler and the timer
// do, through the same door: leave the target as a message, `systemctl start` the oneshot
// (which blocks), and print the one line the run ended on. The unit's exit status is the
// verb's, for free.
//
// VM ([B.86d]) IS an injector, and correctly so: the guest OS is moved by the agent (stage
// from the cache, switch or reboot, health-gate, commit or revert), so there is nothing to do
// when the agent is down but bring the agent back. It submits the update-vm directive --
// the same one the agent's own nightly timer submits -- and reports the upgrade's outcome the
// way `directive upgrade-system` would, with the release named. ⚠️ The DIRECTIVE KIND keeps its
// name: it is a wire word, and the fleet tests read it out of journals.
//
// THE DEFAULT IS `stable`, on every path ([B.159](f)). `latest` exists to be proven before it is
// promoted, by a canary pinned to it deliberately; it is not somewhere to arrive by typing four
// words. Naming it explicitly stays supported -- only the default moved.
func runUpdate(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("briard update", flag.ContinueOnError)
	fs.SetOutput(stderr)
	vm := fs.Bool("vm", false, "update the VM briard runs apps in, instead of briard itself")
	base := fs.String("base", envOr("UPDATE_BASE", "/opt/briard/agent"), "briard: where the committed agent lives")
	run := fs.String("run", envOr("UPDATE_RUN_DIR", selfupdate.DefaultRunDir), "briard: the tmpfs dir the update messages pass through")
	unit := fs.String("unit", selfupdate.DefaultUpdateUnit, "briard: the update unit to start")
	sock := fs.String("sock", sockDefault(), "-vm: the agent's admin socket")
	target := fs.String("to", install.TargetStable, "the release to converge to: stable, latest, or an exact id")
	fs.Usage = func() {
		fmt.Fprintf(stderr, "Usage: briard update [-vm] [options]\n\nOptions:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	// No positional word at all: the chain is the flag, and a word here is the retired
	// `<self|vm>` / `<host|guest>` shape, refused rather than guessed at.
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "briard update: unexpected argument %q (the VM is `-vm`; briard itself is the bare verb)\n", fs.Arg(0))
		fs.Usage()
		return 2
	}
	if !*vm {
		l := selfupdate.New(*base, *run)
		line, err := l.Trigger(ctx, *unit, *target)
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
		// HA refusal, which never touched anything -- so the detail matters, not just the
		// state.
		fmt.Fprintf(stderr, "not applied; the node is unchanged and serving: %s\n", o.Detail)
		return 1
	default:
		fmt.Fprintf(stderr, "briard update -vm: %s\n", o.Detail)
		return 1
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
