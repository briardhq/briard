// Command entrygate-eval runs the S1 health-gate verdict (agent/guest/entrygate)
// over two config-entry-state samples and prints the result. It exists so a nixosTest
// can exercise the *real* gate logic against the
// *real* config-entry states HA computed — read from the two JSON files the
// briard_canary fixture dumps (pre-upgrade and settled post-upgrade) — rather than
// re-implementing the verdict in the test's Python.
//
// Usage: entrygate-eval <pre.json> <post.json>
// Each file is a JSON array of {entry_id, domain, state} (HA's config-entry shape).
// Prints one line: `VERDICT=<pass|hold|rollback> <reason>`, exit 0.
//
// It also TAKES a sample, which is the half [V3b.29](b) added:
//
//	entrygate-eval -sample <port>   # print this node's live config-entry states as JSON
//
// That runs the product's own sampler (agent/hass) — read the control token off tmpfs, exchange
// it, present the Bearer, trim the answer — so a test that samples this way is exercising the
// path an install actually takes rather than a fixture's private dump of the same facts. The
// dump remains useful for WAITING on a state; it is no longer what the verdict is computed from.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strconv"

	"briard.io/agent/guest/entrygate"
	"briard.io/agent/guestagent"
	"briard.io/agent/hass"
)

func load(path string) ([]entrygate.Entry, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	// hass.Entry is the product's own spelling of HA's config-entry triple, and it parses both
	// inputs this command takes: the briard_canary fixture emits exactly those fields, and it is
	// what -sample prints. One spelling, so a file and a live sample cannot diverge.
	var raw []hass.Entry
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	out := make([]entrygate.Entry, len(raw))
	for i, e := range raw {
		out[i] = entrygate.Entry{ID: e.ID, Domain: e.Domain, State: entrygate.State(e.State)}
	}
	return out, nil
}

// sample prints this node's live config-entry states, through the product's own sampler.
//
// The executor is the guest agent's real one, not a local stand-in: the sampler reads the control
// token through it, and a test that used a different reader would be proving a different program.
func sample(port string) {
	p, err := strconv.Atoi(port)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sample: port %q: %v\n", port, err)
		os.Exit(2)
	}
	entries, err := hass.Readiness(context.Background(), guestagent.NewOSExecutor(), p)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sample: %v\n", err)
		os.Exit(1)
	}
	out, err := json.Marshal(entries)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sample: %v\n", err)
		os.Exit(1)
	}
	fmt.Println(string(out))
}

func main() {
	sampleAt := flag.String("sample", "", "print this node's live config-entry states as JSON, sampled from the service on this port")
	flag.Parse()
	if *sampleAt != "" {
		sample(*sampleAt)
		return
	}
	if flag.NArg() != 2 {
		fmt.Fprintln(os.Stderr, "usage: entrygate-eval <pre.json> <post.json> | entrygate-eval -sample <port>")
		os.Exit(2)
	}
	pre, err := load(flag.Arg(0))
	if err != nil {
		fmt.Fprintf(os.Stderr, "pre: %v\n", err)
		os.Exit(2)
	}
	post, err := load(flag.Arg(1))
	if err != nil {
		fmt.Fprintf(os.Stderr, "post: %v\n", err)
		os.Exit(2)
	}
	res := entrygate.Assess(pre, post)
	fmt.Printf("VERDICT=%s %s\n", res.Verdict, res.Reason())
}
