// Package cli is `briard` — the operator front.
//
// It is a MODE of the agent binary, not a second one. The binary was already a multitool
// (the daemon modes, --report-card, --fetch-install), and the decisive argument against a
// separate CLI is self-update is a single-binary story, so a second binary would need
// its own signing, staging and update path PLUS a version-lockstep story with the agent — and a
// CLI speaking api.Directive to a differently-versioned agent is a bug class that one binary
// makes structurally impossible.
//
// Named `briard` rather than `briardctl`: install.sh never puts the binary on $PATH, so the
// operator-facing name is a free symlink independent of the on-disk name. The -ctl precedents
// (systemctl/kubectl/machinectl) front SYSTEM DAEMONS; the product-CLI convention is the bare
// name — docker, podman, nix, gh, and most closely tailscaled/tailscale, which is our exact
// shape: a daemon plus a CLI, for a managed service with an agent on user hardware.
//
// Most verbs here are INJECTORS: they submit an api.Directive to the running agent over
// its admin socket and print the terminal outcome. The agent does the work, through the same
// path the cloud's directives take.
//
// `alerts` and `logs` are the exception, and it is a deliberate one rather than a gap. They READ
// the node's log surfaces directly, touching no socket, because the situation they exist for is
// the agent being DOWN — an injector would answer "cannot reach the agent" precisely when the
// operator most needs an answer. See logs.go.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"strings"

	"briard.io/shared/api"
)

// defaultSock mirrors ConfigFromEnv's ADMIN_SOCK default (agent/host/config.go). The literal is
// written twice rather than shared, because the alternative — a const in shared/api — would put
// a deployment path in the closed wire-contract package. Keep the two in step; a mismatch
// surfaces as "is the agent running?", which is why that error names the path it tried.
const defaultSock = "/run/briard/admin.sock"

// A command is one row of the CLI's SINGLE source of truth: `commands` is both the dispatch table
// and the help text. The fault this shape exists to prevent is the one that produced [V3b.23] —
// a help listing and a real surface that had drifted apart, with no test able to notice, because
// they were two hand-maintained lists. TestEveryCommandIsDocumented walks this table from both
// ends, so a verb cannot be added without a line in the help or removed without losing it.
type command struct {
	name     string // the word the user types
	args     string // argument synopsis, shown after the name
	synopsis string // the one line in `briard help`
	group    string // which heading it appears under
	detail   string // extra prose for `briard help <name>`; may be empty
	// run dispatches the verb. nil means the binary's main() intercepts it before the CLI is
	// reached — true only of `run`, whose daemon modes cannot live in this package.
	run func(ctx context.Context, args []string, stdout, stderr io.Writer) int
	// probe is the argument list that makes this verb print its own flag defaults. `help <name>`
	// uses it so the option list can never drift from the flags the verb actually parses.
	probe []string
}

const (
	groupEveryday = "Everyday"
	groupRepair   = "Repair and maintenance"
)

// The order here is the order in the help. Everyday first, because the first screen should answer
// "what do I do" rather than show the reader everything we can do ([V3b.20]).
var commands = []command{
	{
		name: "alerts", group: groupEveryday,
		synopsis: "what this node has warned about",
		detail: "Reads this node's own log surfaces directly and names any it could not read, so an\n" +
			"empty result is never a guess. A free install talks to no server of ours, so nothing is\n" +
			"pushed to you — this is how you ask. Run it when something looks off, or on a timer.",
		run: runAlerts, probe: []string{"-h"},
	},
	{
		name: "logs", group: groupEveryday,
		synopsis: "what this node has logged (-follow to stream)",
		detail: "Reads both surfaces together: the host agent's journal and the guest's own serial\n" +
			"console, which is the only view into the VM's boot and kernel. A bug report wants both.",
		run: runLogs, probe: []string{"-h"},
	},
	{
		name: "service", args: "install <name>", group: groupEveryday,
		synopsis: "install a catalogued service on this node",
		detail: "The name is an entry in the signed catalog. The install pulls the image, puts its data\n" +
			"on the replicated volume, and starts it behind a health gate that reverts the node if it\n" +
			"does not come up. It blocks until the node reaches a terminal state.",
		run: runService, probe: []string{"install", "-h"},
	},
	{
		name: "handover", group: groupEveryday,
		synopsis: "hand this node's work to a peer (a planned failover)",
		detail: "For when you are about to take this machine away: the peer picks the work up, rather\n" +
			"than the flock discovering the loss on its own.",
		run: runHandover, probe: []string{"-h"},
	},
	{
		name: "rescue", group: groupRepair,
		synopsis: "rebuild this node's guest from its image (-yes to confirm)",
		detail: "Destructive to the guest's own disk, never to the replicated data volume. It asks for\n" +
			"-yes because there is no undo for the rebuild itself.",
		run: runRescue, probe: []string{"-h"},
	},
	{
		name: "os", args: "upgrade <closure>", group: groupRepair,
		synopsis: "switch this node to a system closure, health-gated",
		detail:   "Takes a store path, so in practice this is driven by tooling rather than typed.",
		run:      runOS, probe: []string{"upgrade", "-h"},
	},
	{
		name: "directive", args: "<kind> [payload]", group: groupRepair,
		synopsis: "submit a directive to the local agent",
		detail: "The primitive every other verb above is sugar over. Useful when a support answer names\n" +
			"a directive kind that has no verb of its own.",
		run: runDirective, probe: []string{"-h"},
	},
	{
		name: "run", group: groupRepair,
		synopsis: "run the agent itself — systemd does this for you",
		detail: "The daemon. `run` is the host agent; `run --guest` and `run --deadman` are the two\n" +
			"in-guest modes, started by units inside the guest image. You should not need to type any\n" +
			"of them: the installer writes the units that do.",
		// run: nil — main() intercepts this word before the CLI sees it (the daemon modes cannot
		// live in this package). It is in the table so it is DOCUMENTED; the test knows.
	},
}

// Main runs one CLI invocation and returns the process exit code: 0 when the directive reached a
// clean terminal state, 1 when the node refused or reverted it, 2 for a usage error.
//
// The refused/reverted case exits NON-ZERO on purpose. `briard` is the seam a script (and later
// the UI) drives, so an op that rolled back must be distinguishable from one that worked without
// parsing prose — an admin tool that always exits 0 is one nobody can automate against.
//
// No arguments prints the help and succeeds, which is the ordinary CLI contract ([V3b.23]) and
// used to be "start the privileged host agent". Nothing reaches this path with the daemon in
// mind any more: the units say `run`.
func Main(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stdout)
		return 0
	}
	switch args[0] {
	case "help", "-h", "--help":
		return runHelp(ctx, args[1:], stdout, stderr)
	}
	for _, c := range commands {
		if c.name != args[0] || c.run == nil {
			continue
		}
		return c.run(ctx, args[1:], stdout, stderr)
	}
	fmt.Fprintf(stderr, "briard: unknown command %q\n\n", args[0])
	usage(stderr)
	return 2
}

// runHelp is `briard help [command]`. With no argument it is the whole listing; with one it is
// that command's own prose followed by the command's own flag defaults — printed by the command,
// not restated here, so the two cannot disagree.
func runHelp(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stdout)
		return 0
	}
	for _, c := range commands {
		if c.name != args[0] {
			continue
		}
		fmt.Fprintf(stdout, "briard %s\n\n%s\n", strings.TrimSpace(c.name+" "+c.args), c.synopsis)
		if c.detail != "" {
			fmt.Fprintf(stdout, "\n%s\n", c.detail)
		}
		if c.run != nil {
			// The verb prints its OWN options. It returns 2 (flag.ErrHelp is a usage error to the
			// verb), which is right for `briard rescue -h` and wrong for `briard help rescue` —
			// so the exit code is ours, and both writers are stdout because this output was asked
			// for rather than complained about.
			fmt.Fprintln(stdout, "\nOptions:")
			c.run(ctx, c.probe, stdout, stdout)
		}
		return 0
	}
	fmt.Fprintf(stderr, "briard help: unknown command %q\n\n", args[0])
	usage(stderr)
	return 2
}

func usage(w io.Writer) {
	fmt.Fprint(w, "briard — administer the node this command runs on.\n\nUsage: briard <command> [options]\n")
	for _, g := range []string{groupEveryday, groupRepair} {
		fmt.Fprintf(w, "\n%s\n", g)
		for _, c := range commands {
			if c.group == g {
				fmt.Fprintf(w, "  %-28s %s\n", strings.TrimSpace(c.name+" "+c.args), c.synopsis)
			}
		}
	}
	fmt.Fprint(w, `
  help [command]               this message, or one command's options

`+"`alerts`"+` and `+"`logs`"+` read this node's logs and work even when the agent is down.
The rest talk to the running briard-agent over its admin socket
(`+defaultSock+`, override with -sock or $ADMIN_SOCK). All of them need root.

This node does not notify anyone on its own unless it was configured to: `+"`briard alerts`"+`
is how you ask it what is wrong. Run it when something looks off, or on a schedule.

`)
}

// directive and report what it did. It is deliberately the first verb built — it exercises the
// whole door with kinds that already exist (noop, log), so the local path is proven before any
// new directive kind widens the shared/api allowlist.
func runDirective(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("briard directive", flag.ContinueOnError)
	fs.SetOutput(stderr)
	sock := fs.String("sock", sockDefault(), "the agent's admin socket")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() < 1 || fs.NArg() > 2 {
		fmt.Fprint(stderr, "briard directive: want <kind> [payload]\n")
		return 2
	}
	d := api.Directive{Kind: fs.Arg(0)}
	if fs.NArg() == 2 {
		d.Payload = fs.Arg(1)
	}
	o, err := submit(ctx, *sock, d)
	if err != nil {
		fmt.Fprintf(stderr, "briard: %v\n", err)
		return 1
	}
	if o.Detail != "" {
		fmt.Fprintf(stdout, "%s: %s\n", o.State, o.Detail)
	} else {
		fmt.Fprintf(stdout, "%s\n", o.State)
	}
	if o.State != api.OutcomeDone {
		return 1
	}
	return 0
}

// runService is the first verb that is SUGAR over `directive` rather than a separate mechanism —
// which is the shape every later verb should copy. `briard service install ha` is exactly
// `briard directive service-install ha`, so there is one path through the agent and the CLI adds
// only a name a human would guess.
//
// It blocks until the node reaches a terminal state, because an install is a maintenance
// operation on a live promoted resource: returning early would leave an operator guessing whether
// the thing they just did is still happening.
func runService(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) < 1 || args[0] != "install" {
		fmt.Fprint(stderr, "briard service: want `install <name>`\n")
		return 2
	}
	fs := flag.NewFlagSet("briard service install", flag.ContinueOnError)
	fs.SetOutput(stderr)
	sock := fs.String("sock", sockDefault(), "the agent's admin socket")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprint(stderr, "briard service install: want exactly one service name\n")
		return 2
	}
	name := fs.Arg(0)
	fmt.Fprintf(stdout, "installing %s (this takes a few minutes; the node stays up)\n", name)
	o, err := submit(ctx, *sock, api.Directive{Kind: api.DirectiveServiceInstall, Payload: name})
	if err != nil {
		fmt.Fprintf(stderr, "briard: %v\n", err)
		return 1
	}
	switch o.State {
	case api.OutcomeDone:
		// Say what actually happened, not what the product will eventually do. The gate probes the
		// SERVICE's own endpoint in-guest (awaitHealthy); the front door does not route to a
		// runtime-installed service yet -- its backend is baked at guest-build time and per-domain
		// routing is deferred with the routing work. "serving" reads as "reachable at the VIP",
		// which walks the user to http://<vip>/ and shows them the "nothing is routed here" page.
		fmt.Fprintf(stdout, "%s installed and healthy\n", name)
		fmt.Fprintf(stdout, "it answers on its own port; the front door at / does not route to it yet\n")
		return 0
	case api.OutcomeRolledBack:
		// The distinction an operator most needs: the node is back as it was, so this is a failed
		// install rather than a broken node.
		fmt.Fprintf(stderr, "%s did not come up; the node was reverted and is unchanged: %s\n", name, o.Detail)
		return 1
	default:
		fmt.Fprintf(stderr, "%s install failed: %s\n", name, o.Detail)
		return 1
	}
}

// runHandover is `briard handover` — give this node's work to a peer, on purpose, while it is
// perfectly healthy. Sugar over `directive handover`, the shape `service install`
// established.
//
// WHY A HUMAN WANTS THIS. Failover is the claim this product is bought for, and it is the one
// claim that is otherwise only tested on the day it matters. Running it deliberately — after
// pairing a second anchor, swapping hardware, moving the machine to a different switch — turns
// "it should work" into "it worked, on Tuesday, and here is the log". It is also cheaper than the
// upgrade it borrows from: no reboot, so it never opens the window where a degraded pair is one
// power cut from trouble.
//
// ONE STEP, NO ORCHESTRATION, deliberately. Sequencing a pair of these — hand over, verify, hand
// back — belongs to whatever can see every node (the cloud, or a lab script driving both). This
// verb evicts the node it runs on and says what happened, which is exactly what a node can know:
// `drbd-reactorctl evict` says "not me", never "you". Run it on the peer to come back.
// runRescue rebuilds this node's guest from the verified image under its OS-disk overlay (B.10).
//
// IT REQUIRES -yes, and that is the only place in this CLI that does. Every other verb here is
// reversible or health-gated: an OS upgrade rolls back, a service install restores its data, a
// handover can be handed back. This one discards the guest's OS disk, and nothing brings back what
// was on it. A confirmation flag is the cheapest possible guard against the one invocation nobody
// meant to type, and it costs an operator who did mean it four characters.
//
// It does NOT prompt interactively. The situations this verb exists for include a node being
// driven over SSH from a script, and a prompt that sometimes appears is worse than a flag that
// always must — it makes the safe path the one that varies by terminal.
func runRescue(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("briard rescue", flag.ContinueOnError)
	fs.SetOutput(stderr)
	sock := fs.String("sock", sockDefault(), "the agent's admin socket")
	yes := fs.Bool("yes", false, "confirm: discard this node's guest OS disk and rebuild it")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprint(stderr, "briard rescue: takes no arguments\n")
		return 2
	}
	if !*yes {
		// Say what survives as well as what goes. An operator reaching for this is usually unsure
		// whether they are about to lose their data, and the answer -- no -- is the thing that
		// decides whether they run it.
		fmt.Fprint(stderr, `briard rescue: this discards the guest's OS disk and rebuilds it from
the signed image it was installed from. The node comes back with a factory code
half and re-pulls its service images, which needs working network.

Your DATA IS NOT TOUCHED: the replicated volume, this node's identity and the
service it is pinned to all survive. What is lost is anything written to the
guest's own OS disk since install.

Read `+"`briard logs`"+` first. Re-run with -yes when you have.
`)
		return 2
	}
	fmt.Fprint(stdout, "rebuilding the guest from its backing image (the data disk is not touched)\n")
	o, err := submit(ctx, *sock, api.Directive{Kind: api.DirectiveRescue})
	if err != nil {
		fmt.Fprintf(stderr, "briard: %v\n", err)
		return 1
	}
	if o.State != api.OutcomeDone {
		fmt.Fprintf(stderr, "rescue failed: %s\n", o.Detail)
		return 1
	}
	fmt.Fprint(stdout, "rebuilt and re-converged\n")
	return 0
}

func runHandover(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("briard handover", flag.ContinueOnError)
	fs.SetOutput(stderr)
	sock := fs.String("sock", sockDefault(), "the agent's admin socket")
	keepMasked := fs.Bool("keep-masked", false, "stay ineligible afterwards (release with -unmask); for a node about to reboot")
	unmask := fs.Bool("unmask", false, "release a -keep-masked node; hands nothing over")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprint(stderr, "briard handover: takes no arguments\n")
		return 2
	}
	if *keepMasked && *unmask {
		fmt.Fprint(stderr, "briard handover: -keep-masked and -unmask are opposites\n")
		return 2
	}
	mode := ""
	switch {
	case *unmask:
		mode = "unmask"
	case *keepMasked:
		mode = "keep-masked"
	}
	if *unmask {
		fmt.Fprint(stdout, "releasing this node to hold the resource again\n")
	} else {
		fmt.Fprint(stdout, "handing this node's work to a peer (the front door moves; connections drop)\n")
	}
	o, err := submit(ctx, *sock, api.Directive{Kind: api.DirectiveHandover, Payload: mode})
	if err != nil {
		fmt.Fprintf(stderr, "briard: %v\n", err)
		return 1
	}
	if o.State != api.OutcomeDone {
		fmt.Fprintf(stderr, "handover failed: %s\n", o.Detail)
		return 1
	}
	if *unmask {
		fmt.Fprint(stdout, "released\n")
		return 0
	}
	// Deliberately not "n2 is now serving": this node cannot see who took over, and a CLI that
	// claimed it would be inventing the one fact the operator came for.
	fmt.Fprint(stdout, "handed over — check which peer took it (`drbdadm role r0` on each node)\n")
	return 0
}

// runOS is `briard os upgrade <closure>` — move THIS node to a system closure.
// Sugar over `directive upgrade-system`, which already worked: the admin socket carries no kind
// allowlist, so the mechanism was reachable before this verb existed. What the verb adds is a
// name a human would guess and an honest description of what happens.
//
// WHO IT IS FOR. A cloudless node — the free tier's single-anchor island — receives no directives
// at all, so this is its ONLY route to a new OS. It is also the operator's escape hatch on a
// managed node: the per-home upgrade window binds the cloud's pusher, never a human standing at
// the machine, so this runs at any hour by design.
//
// SINGLE STEP, NO ORCHESTRATION. It upgrades the node it runs on. On an HA pair a serving node
// will DECLINE — going down would hand the house to a peer, which is a failover to sequence and
// not a node's decision about itself — and that refusal is reported as such rather than dressed
// up as a failure.
func runOS(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) < 1 || args[0] != "upgrade" {
		fmt.Fprint(stderr, "briard os: want `upgrade <closure>`\n")
		return 2
	}
	fs := flag.NewFlagSet("briard os upgrade", flag.ContinueOnError)
	fs.SetOutput(stderr)
	sock := fs.String("sock", sockDefault(), "the agent's admin socket")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprint(stderr, "briard os upgrade: want exactly one system closure store path\n")
		return 2
	}
	closure := fs.Arg(0)
	if !strings.HasPrefix(closure, "/nix/store/") {
		// Caught here rather than in the agent because the agent's refusal would arrive after a
		// staging attempt, and "that is not a store path" is a typo a human should be told about
		// immediately.
		fmt.Fprintf(stderr, "briard os upgrade: %q is not a /nix/store path\n", closure)
		return 2
	}
	fmt.Fprintf(stdout, "upgrading to %s (staging, then activate + health-gate; this can take minutes)\n", closure)
	o, err := submit(ctx, *sock, api.Directive{Kind: api.DirectiveUpgradeSystem, Payload: closure})
	if err != nil {
		fmt.Fprintf(stderr, "briard: %v\n", err)
		return 1
	}
	switch o.State {
	case api.OutcomeDone:
		fmt.Fprint(stdout, "upgraded and healthy\n")
		return 0
	case api.OutcomeRolledBack:
		// The distinction an operator most needs, and the one the exit code alone cannot carry:
		// the node is back where it started rather than broken. That includes the HA refusal,
		// which never touched anything -- so the detail matters, not just the state.
		fmt.Fprintf(stderr, "not applied; the node is unchanged and serving: %s\n", o.Detail)
		return 1
	default:
		fmt.Fprintf(stderr, "upgrade failed: %s\n", o.Detail)
		return 1
	}
}

func sockDefault() string {
	if s := os.Getenv("ADMIN_SOCK"); s != "" {
		return s
	}
	return defaultSock
}

// submit sends one directive and blocks for its outcome.
//
// There is no client-side deadline by design: a payload or OS upgrade legitimately runs for
// minutes (the agent bounds it at 10 and 15 respectively), and a CLI that timed out first would
// report failure for an op still in flight — then invite the operator to "retry" a node that is
// mid-upgrade. Waiting is the honest behaviour; Ctrl-C is the operator's own escape.
func submit(ctx context.Context, sock string, d api.Directive) (api.DirectiveOutcome, error) {
	var zero api.DirectiveOutcome
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "unix", sock)
	if err != nil {
		// The two things actually wrong when this fails, named so the operator doesn't have to
		// guess: the agent isn't up, or this isn't root.
		return zero, fmt.Errorf("cannot reach the agent at %s (is briard-agent running, and are you root?): %w", sock, err)
	}
	defer conn.Close()
	if err := json.NewEncoder(conn).Encode(d); err != nil {
		return zero, fmt.Errorf("submitting the directive: %w", err)
	}
	var o api.DirectiveOutcome
	if err := json.NewDecoder(conn).Decode(&o); err != nil {
		if errors.Is(err, io.EOF) {
			return zero, errors.New("the agent closed the connection without reporting an outcome")
		}
		return zero, fmt.Errorf("reading the outcome: %w", err)
	}
	return o, nil
}
