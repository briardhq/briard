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
// It pairs what the image and the agent genuinely share: the tool profile, the only thing they
// agree on by name. Renamed on either side, the units point at nothing, and since the agent
// refuses to render without it, the guest will not serve at all.
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
}

// TestScriptsNameThePathsTheAgentUses pairs the embedded scripts' literals with the Go side: one
// package now, but two languages, so nothing imports across. Each pair is a path one side writes
// and the other reads; renamed on one side only, the failure is silent until the moment it
// matters.
func TestScriptsNameThePathsTheAgentUses(t *testing.T) {
	read := func(name string) string {
		b, err := scripts.ReadFile("scripts/" + name)
		if err != nil {
			t.Fatalf("read embedded script %s: %v", name, err)
		}
		return string(b)
	}
	for _, c := range []struct{ script, want, why string }{
		// The interpreter, in the profile WriteUnits checks for it: renamed, every script fails
		// to exec on the promotion path.
		{"briard-vip-up", "#!" + defaultToolsBin + "/bash\n", "its interpreter"},
		{"briard-hold-confirm", "#!" + defaultToolsBin + "/bash\n", "its interpreter"},
		// The mount the agent makes, where the VIP's claim reads the flock's stored address.
		{"briard-vip-up", dataMountRoot + "/.vip-address", "the stored address"},
		// The topology word node storage writes and the hold's steps dispatch on. Renamed, every
		// node's hold reads "node storage has not run on this boot" and skips itself -- silent
		// until something has to hand over.
		{"briard-hold-only-if-holding", topologyEnvPath, "the topology word"},
		// Upstream's shim, under the profile's lib/drbd link.
		{"briard-hold-confirm", strings.TrimSuffix(defaultToolsBin, "/bin") + "/lib/drbd/scripts/drbd-service-shim.sh", "the drbd shim"},
	} {
		if !strings.Contains(read(c.script), c.want) {
			t.Errorf("constant drift: %s does not name %s as %q", c.script, c.why, c.want)
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
