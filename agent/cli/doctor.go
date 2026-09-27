// `briard doctor` -- the report card as a runtime verb ([V3c.3]).
//
// Two halves, and the split is the point. The host half (agent/reportcard, AssessLive) reads what
// this machine can see by itself, touching no socket, the way `alerts` and `logs` do, because the
// agent being DOWN is when someone most needs an answer. The agent half is a local-only
// directive (agent/host, doctor.go) for what only the agent can see: the network it built, the
// guest behind the channel it holds, and what that guest says. An agent that cannot answer is a
// finding of its own, printed with the rest -- never a reason to print nothing.
package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"briard.io/agent/reportcard"
	"briard.io/shared/api"
)

// doctorWait bounds the agent half. The agent applies a directive between observe cycles, so a
// node mid-upgrade or mid-bring-up does not answer at once, and doctor must still print.
const doctorWait = 30 * time.Second

func runDoctor(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("briard doctor", flag.ContinueOnError)
	fs.SetOutput(stderr)
	sock := fs.String("sock", sockDefault(), "the agent's admin socket")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprint(stderr, "briard doctor: takes no arguments\n")
		return 2
	}
	checks := reportcard.AssessLive(reportcard.GatherLive(ctx))
	actx, cancel := context.WithTimeout(ctx, doctorWait)
	o, err := submit(actx, *sock, api.Directive{Kind: api.DirectiveDoctor})
	cancel()
	checks = append(checks, agentChecks(o, err, agentUnitState(ctx))...)

	host, _ := os.Hostname()
	fmt.Fprintf(stdout, "briard doctor -- %s -- %s\n\n", host, time.Now().UTC().Format(time.RFC3339))
	return printDoctor(stdout, checks)
}

// agentChecks turns the agent's answer, or its absence, into checks. unit is systemd's view of
// briard-agent.service, which is what says WHY the socket did not answer.
func agentChecks(o api.DirectiveOutcome, err error, unit string) []reportcard.Check {
	if err != nil {
		return []reportcard.Check{{Name: "agent", Status: reportcard.Refuse,
			Detail: fmt.Sprintf("the agent did not answer (briard-agent is %s): %v", unit, err),
			Fix:    "`briard logs` shows why; `systemctl restart briard-agent` if it is stuck"}}
	}
	var cs []reportcard.Check
	if o.State != api.OutcomeDone || json.Unmarshal([]byte(o.Detail), &cs) != nil {
		return []reportcard.Check{{Name: "agent", Status: reportcard.Refuse,
			Detail: fmt.Sprintf("the agent answered without a diagnosis (%s): %s", o.State, o.Detail),
			Fix:    "run `briard update`: this agent may predate the check"}}
	}
	return append([]reportcard.Check{{Name: "agent", Status: reportcard.Pass, Detail: "briard-agent is running and answered"}}, cs...)
}

// agentUnitState is `systemctl is-active briard-agent`'s one word, or "unknown".
func agentUnitState(ctx context.Context) string {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, _ := exec.CommandContext(ctx, "systemctl", "is-active", "briard-agent.service").Output()
	if s := strings.TrimSpace(string(out)); s != "" {
		return s
	}
	return "unknown"
}

// printDoctor writes the checks in the report card's shape -- one line each, fix beneath -- so it
// pastes into an email intact, and returns 1 when anything failed.
func printDoctor(w io.Writer, cs []reportcard.Check) int {
	mark := map[reportcard.Status]string{reportcard.Pass: "OK", reportcard.Warn: "WARN", reportcard.Refuse: "FAIL"}
	failed := 0
	for _, c := range cs {
		fmt.Fprintf(w, "[%-4s] %-11s %s\n", mark[c.Status], c.Name, c.Detail)
		if c.Fix != "" {
			fmt.Fprintf(w, "              -> %s\n", c.Fix)
		}
		if c.Status == reportcard.Refuse {
			failed++
		}
	}
	if failed > 0 {
		fmt.Fprintf(w, "\nresult: %d problem(s) -- the FAIL lines above, with what to do.\n", failed)
		return 1
	}
	fmt.Fprintln(w, "\nresult: nothing wrong found.")
	return 0
}
