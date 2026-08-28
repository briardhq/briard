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
// It used to pair the payload slot's `payloadPinPath`/`payloadServeTag`, which is what it was
// written for; those are deleted ([V3b.3](e1)) and the TLS directory is what is left paired the
// same way. The mechanism outliving its first subject is the point of having it.
func TestVolumePathsMatchGuestImage(t *testing.T) {
	raw, err := os.ReadFile("../../guest-image/configuration.nix")
	if err != nil {
		t.Fatalf("read guest image config: %v", err)
	}
	nix := string(raw)
	// The paths are written `${btrfsRoot}/...` in Nix; resolve that one interpolation before
	// comparing to the Go consts, which carry the fully-expanded path.
	root := nixLet(t, nix, "btrfsRoot")

	for _, c := range []struct{ goName, goVal, nixName string }{
		{"tlsDir", tlsDir, "tlsDir"},
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
