package guestagent

import (
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"briard.io/agent/guestagent/deadman"
)

// TestVolumePathsMatchGuestImage guards the one cross-language pairing that "shared types live in
// shared/" cannot cover: these paths are both Go consts here and Nix let-bindings in
// guest-image/configuration.nix, because the pushed guest agent writes (or mounts) them and the
// image reads them, in another language with no Go constant to import. A rename on either side
// breaks the pairing silently; this test reads both sides and fails loudly. Chosen over a
// generator: no build step, and the guest image is a sibling file already in the tree.
//
// It pairs what the image and the agent genuinely share. The tool profile is the load-bearing
// one, the only thing they agree on by name: renamed on either side, the units point at nothing,
// and since the agent refuses to render without it, the guest will not serve at all. The other
// two are read by the image's own shell and written by the agent's Go.
func TestVolumePathsMatchGuestImage(t *testing.T) {
	raw, err := os.ReadFile("../../guest-image/configuration.nix")
	if err != nil {
		t.Fatalf("read guest image config: %v", err)
	}
	nix := string(raw)

	// The tool profile: Nix states it relative to /etc, what `environment.etc.<name>` takes; Go
	// states the bin directory inside it, what a unit's PATH is.
	if want := "/etc/" + nixLet(t, nix, "toolsEtc") + "/bin"; want != defaultToolsBin {
		t.Errorf("constant drift: Go defaultToolsBin = %q but the image publishes %q — every agent-written unit's PATH points at nothing",
			defaultToolsBin, want)
	}

	// The paths are written `${btrfsRoot}/...` in Nix; resolve that one interpolation before
	// comparing to the Go consts, which carry the fully-expanded path.
	root := nixLet(t, nix, "btrfsRoot")
	for _, c := range []struct{ goName, goVal, nixName string }{
		// The mount the agent makes and the VIP's scripts read the flock's stored address from.
		{"dataMountRoot", dataMountRoot, "btrfsRoot"},
		// The topology word node storage writes and the hold's steps dispatch on. Renamed, every
		// node's hold reads "node storage has not run on this boot" and skips itself -- silent
		// until something has to hand over.
		{"topologyEnvPath", topologyEnvPath, "topologyEnvPath"},
	} {
		nv := strings.ReplaceAll(nixLet(t, nix, c.nixName), "${btrfsRoot}", root)
		if nv != c.goVal {
			t.Errorf("constant drift: Go %s = %q but guest-image %s = %q — the host/guest pairing is broken",
				c.goName, c.goVal, c.nixName, nv)
		}
	}
}

// nixLet extracts a `name = "literal";` string binding from a Nix source. A missing binding fails
// the test — that absence is itself the signal a name was renamed away.
func nixLet(t *testing.T, src, name string) string {
	t.Helper()
	re := regexp.MustCompile(name + `\s*=\s*"([^"]*)"`)
	m := re.FindStringSubmatch(src)
	if m == nil {
		t.Fatalf("guest-image config no longer defines %q — the pairing test cannot find it", name)
	}
	return m[1]
}

// TestGatePortMatchesGuestImage pairs the reboot gate's port: the host dials deadman.GatePort, the
// guest's deadman unit listens on guest-image/disk-image.nix's BRIARD_GATE_ADDR. Drifted, the host
// reads every gate as unreachable, which it takes as no opinion, so a guest whose reboot would
// drop the cluster below quorum is rebooted anyway, and nothing fails to say so.
func TestGatePortMatchesGuestImage(t *testing.T) {
	raw, err := os.ReadFile("../../guest-image/disk-image.nix")
	if err != nil {
		t.Fatalf("read guest disk image: %v", err)
	}
	if got, want := nixLet(t, string(raw), "BRIARD_GATE_ADDR"), ":"+strconv.Itoa(deadman.GatePort); got != want {
		t.Errorf("constant drift: the deadman unit listens on BRIARD_GATE_ADDR = %q but the host dials port %d", got, deadman.GatePort)
	}
}
