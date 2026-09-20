package guestagent

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestVolumePathsMatchGuestImage guards the one cross-language pairing that "shared types live in
// shared/" cannot cover. These paths on the replicated volume exist BOTH as Go consts here and as
// Nix let-bindings in guest-image/configuration.nix, because the host agent writes the files and
// the guest image reads the same ones — and they are different languages, so there is no shared Go
// constant to import. A rename on either side silently breaks the pairing; a comment cannot catch
// that, but this test reads both sides and fails loudly. It is the cheap mechanism [V3.16](j)
// chose over a generator: no build step, and the guest image is a sibling file already in the tree.
//
// ITS SUBJECT HAS CHANGED TWICE, which is the point of having the mechanism rather than the
// subject. It was written for the payload slot's `payloadPinPath`/`payloadServeTag`
// ([V3.16](j)), which [V3b.3](e1) deleted; it then paired the TLS directory, which [B.160]
// deleted from the Nix side when the front door's unit moved to the agent and nothing in the
// image read the path any more.
//
// WHAT IT PAIRS NOW is what [B.160] left genuinely shared. The tool profile is the load-bearing
// one: it is the ONLY thing the image and the pushed agent agree on by name, so a rename on
// either side is a guest whose units point at nothing -- and, because the agent refuses to
// render without it, a guest that will not serve at all. The other two are read by the image's
// own shell and written by the agent's Go.
func TestVolumePathsMatchGuestImage(t *testing.T) {
	raw, err := os.ReadFile("../../guest-image/configuration.nix")
	if err != nil {
		t.Fatalf("read guest image config: %v", err)
	}
	nix := string(raw)

	// THE TOOL PROFILE ([B.160]). Nix states it relative to /etc, because that is what
	// `environment.etc.<name>` takes; Go states the bin directory inside it, because that is
	// what a unit's PATH is. One pairing, spelled from each side's own end.
	if want := "/etc/" + nixLet(t, nix, "toolsEtc") + "/bin"; want != defaultToolsBin {
		t.Errorf("constant drift: Go defaultToolsBin = %q but the image publishes %q — every agent-written unit's PATH points at nothing",
			defaultToolsBin, want)
	}

	// The paths are written `${btrfsRoot}/...` in Nix; resolve that one interpolation before
	// comparing to the Go consts, which carry the fully-expanded path.
	root := nixLet(t, nix, "btrfsRoot")
	for _, c := range []struct{ goName, goVal, nixName string }{
		// The mount the VIP's scripts read the flock's stored address from, and the agent
		// mounts.
		{"dataMountRoot", dataMountRoot, "btrfsRoot"},
		// The topology word node storage writes and the hold's steps dispatch on ([B.145c]).
		// A rename here is a hold that reads "node storage has not run on this boot" on every
		// node and skips itself -- silent, and only visible when something has to hand over.
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
