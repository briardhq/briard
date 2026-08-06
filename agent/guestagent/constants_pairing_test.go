package guestagent

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestPayloadConstantsMatchGuestImage guards the one cross-language pairing that's "shared types
// live in shared/" cannot cover. These paths and the serve tag exist BOTH as Go consts
// here and as Nix let-bindings in guest-image/configuration.nix, because the host agent writes the
// files and the guest image reads the same ones — and they are different languages, so there is no
// shared Go constant to import. A rename on either side silently breaks the pairing; a comment
// cannot catch that, but this test reads both sides and fails loudly. It is the cheap mechanism (j)
// chose over a generator: no build step, and the guest image is a sibling file already in the tree.
func TestPayloadConstantsMatchGuestImage(t *testing.T) {
	raw, err := os.ReadFile("../../guest-image/configuration.nix")
	if err != nil {
		t.Fatalf("read guest image config: %v", err)
	}
	nix := string(raw)
	// The pin paths are written `${btrfsRoot}/...` in Nix; resolve that one interpolation before
	// comparing to the Go consts, which carry the fully-expanded path.
	root := nixLet(t, nix, "btrfsRoot")

	for _, c := range []struct {
		goName, goVal, nixName string
		interpolated           bool
	}{
		{"payloadServeTag", payloadServeTag, "serveImage", false},
		{"payloadPinPath", payloadPinPath, "imagePinFile", true},
	} {
		nv := nixLet(t, nix, c.nixName)
		if c.interpolated {
			nv = strings.ReplaceAll(nv, "${btrfsRoot}", root)
		}
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
