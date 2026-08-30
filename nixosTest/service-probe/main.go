// Command service-probe is a TEST HELPER (nixosTest/services-pair.nix), not a product binary.
//
// It runs the REAL guest-side probe — agent/mosquitto's, the one the S1 gate's assessor drives
// over the service.probe verb — against a container on this node, and prints what it found. The
// point is that a rig asserting "the token survived the upgrade" is asserting it about the code
// that ships, rather than about a shell pipeline that happens to resemble it.
//
// What it deliberately does NOT do is judge. The verdict is the host's (agent/host/readiness.go)
// and is unit-tested there; a lib.nix rig cannot run the host's install path at all, so the split
// here is the same one hass-upgrade-rollback.nix makes: drive the real signal, assert on the real
// signal, leave the decision to the tests that can run it.
//
//	service-probe <container> [token]     # token given => write it first
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
// combined output. The guest agent's own executor is not exported, and copying its three lines
// beats widening a product package for a test.
type runner struct{}

func (runner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}
func (runner) WriteFile(path string, data []byte) error { return os.WriteFile(path, data, 0o600) }
func (runner) ReadFile(path string) ([]byte, error)     { return os.ReadFile(path) }

func main() {
	args := os.Args[1:]
	if len(args) < 1 || len(args) > 2 {
		fmt.Fprintln(os.Stderr, "usage: service-probe <container> [token]")
		os.Exit(2)
	}
	token := ""
	if len(args) == 2 {
		token = args[1]
	}
	s, err := mosquitto.Probe(context.Background(), runner{}, args[0], token)
	if err != nil {
		fmt.Fprintf(os.Stderr, "service-probe: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("serving=%t token=%s\n", s.Serving, s.Token)
}
