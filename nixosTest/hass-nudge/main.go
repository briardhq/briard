// Command hass-nudge is a TEST HELPER (nixosTest/hass-payload.nix), not a product binary.
//
// It runs the REAL push half of the Home Assistant control channel — agent/hass's Nudge, the one
// the install path drives over the service.home-assistant.nudge verb — against the Home Assistant
// on this node, and says whether it was taken.
//
// It exists for the same reason service-probe does: the rig that proves this has to prove it about
// the code that ships. Everything the unit tests assert about the request is asserted against a
// stub of Home Assistant's event view — the path, the Bearer, the JSON object body — and a stub is
// exactly where a wrong belief about somebody else's API survives. Here it meets the real view, on
// the pinned image, with the token the node actually minted.
//
//	hass-nudge <port>
//
// Exits 0 when Home Assistant took the event.
package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"

	"briard.io/agent/hass"
)

// runner is the smallest Executor agent/hass needs for this call: Nudge only ever reads the token
// off tmpfs, but the interface is the package's one Executor and the other two methods have to be
// there. Three lines, copied rather than exporting the guest agent's own.
type runner struct{}

func (runner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}
func (runner) WriteFile(path string, data []byte) error { return os.WriteFile(path, data, 0o600) }
func (runner) ReadFile(path string) ([]byte, error)     { return os.ReadFile(path) }

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: hass-nudge <port>")
		os.Exit(2)
	}
	port, err := strconv.Atoi(os.Args[1])
	if err != nil {
		fmt.Fprintf(os.Stderr, "hass-nudge: %v\n", err)
		os.Exit(2)
	}
	if err := hass.Nudge(context.Background(), runner{}, port); err != nil {
		fmt.Fprintf(os.Stderr, "hass-nudge: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("fired=%s\n", hass.EventReconsider)
}
