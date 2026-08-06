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
package main

import (
	"encoding/json"
	"fmt"
	"os"

	"briard.io/agent/guest/entrygate"
)

// haEntry is the subset of HA's config-entry JSON the gate needs. The briard_canary
// fixture emits exactly these fields; HA's native /api/config/config_entries/entry
// carries them (plus more the gate ignores), so the same parser fits the real API.
type haEntry struct {
	EntryID string `json:"entry_id"`
	Domain  string `json:"domain"`
	State   string `json:"state"`
}

func load(path string) ([]entrygate.Entry, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var raw []haEntry
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	out := make([]entrygate.Entry, len(raw))
	for i, e := range raw {
		out[i] = entrygate.Entry{ID: e.EntryID, Domain: e.Domain, State: entrygate.State(e.State)}
	}
	return out, nil
}

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: entrygate-eval <pre.json> <post.json>")
		os.Exit(2)
	}
	pre, err := load(os.Args[1])
	if err != nil {
		fmt.Fprintf(os.Stderr, "pre: %v\n", err)
		os.Exit(2)
	}
	post, err := load(os.Args[2])
	if err != nil {
		fmt.Fprintf(os.Stderr, "post: %v\n", err)
		os.Exit(2)
	}
	res := entrygate.Assess(pre, post)
	fmt.Printf("VERDICT=%s %s\n", res.Verdict, res.Reason())
}
