// Command app-probe is a test helper (nixosTest/apps-pair.nix), not a product binary.
//
// It runs the real guest-side probe (agent/mosquitto's, which the S1 gate's assessor drives over
// the app.mosquitto.probe verb) against a container on this node and prints what it found, so a
// rig asserting "the token survived the upgrade" asserts it about the code that ships, not about
// a shell pipeline that happens to resemble it.
//
// It deliberately does not judge: the verdict is the host's (agent/host/readiness.go), unit-tested
// there. A lib.nix rig cannot run the host's install path at all, so the split is the one
// hass-upgrade-rollback.nix makes: drive and assert on the real signal, leave the decision to the
// tests that can run it.
//
//	app-probe <container> [token]     # token given => write it first
//
// Prints `serving=<bool> token=<value>`.
package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"

	"briard.io/agent/mosquitto"
)

// runner is the smallest Executor that satisfies agent/mosquitto: run a command, return its
// combined output. Run is the guest's own (agent/guestfirmware's NewOSExecutor), copied rather
// than linking the guest firmware into a test helper.
type runner struct{}

func (runner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}
func (runner) WriteFile(path string, data []byte) error { return os.WriteFile(path, data, 0o600) }
func (runner) ReadFile(path string) ([]byte, error)     { return os.ReadFile(path) }

func main() {
	args := os.Args[1:]
	if len(args) < 1 || len(args) > 2 {
		fmt.Fprintln(os.Stderr, "usage: app-probe <container> [token]")
		os.Exit(2)
	}
	token := ""
	if len(args) == 2 {
		token = args[1]
	}
	s, err := mosquitto.Probe(context.Background(), runner{}, args[0], token)
	if err != nil {
		fmt.Fprintf(os.Stderr, "app-probe: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("serving=%t token=%s\n", s.Serving, s.Token)
}
