package platform

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestSlirpAddressingMatchesGuestImage guards a pairing that only exists because the guest stopped
// running a DHCP client on eth0 (B.78): the host tells qemu what the SLIRP network is, and the
// guest configures eth0 statically from the same numbers. Nothing at runtime reconciles them --
// that is the point, since the reconciler we removed was the dhcpcd whose presence hijacked the
// VIP's own client -- so a change on one side and not the other yields a guest with an address on
// a network qemu is not running, i.e. no WAN, i.e. no image pulls, discovered late and far away.
//
// Same cheap mechanism as TestPayloadConstantsMatchGuestImage: read both sides, no build step.
func TestSlirpAddressingMatchesGuestImage(t *testing.T) {
	raw, err := os.ReadFile("../../guest-image/disk-image.nix")
	if err != nil {
		t.Fatalf("read guest disk image config: %v", err)
	}
	nix := string(raw)

	// The guest's address must sit inside the network we hand qemu. Compare prefixes rather than
	// the address itself: which host in the /24 the guest takes is ours to choose, but the
	// network it believes it is on is not.
	wantPrefix := strings.TrimSuffix(slirpNet, ".0/24") + "."
	for _, c := range []struct{ what, nixVal, want string }{
		{"eth0 address", nixAttr(t, nix, `address\s*=\s*"(10\.0\.2\.[0-9]+)"\s*;\s*prefixLength`), wantPrefix},
		{"default gateway", nixAttr(t, nix, `defaultGateway\s*=\s*\{[^}]*address\s*=\s*"([^"]*)"`), slirpGateway},
		{"nameserver", nixAttr(t, nix, `nameservers\s*=\s*\[\s*"([^"]*)"`), slirpDNS},
	} {
		switch c.what {
		case "eth0 address":
			if !strings.HasPrefix(c.nixVal, c.want) {
				t.Errorf("SLIRP drift: guest eth0 is %q but the host hands qemu net=%s — the guest would be on a network qemu is not running",
					c.nixVal, slirpNet)
			}
		default:
			if c.nixVal != c.want {
				t.Errorf("SLIRP drift: guest %s = %q but the host passes %q to qemu", c.what, c.nixVal, c.want)
			}
		}
	}
}

// nixAttr pulls the first capture of re out of a Nix source. A miss fails the test: the binding
// having been renamed or restructured is exactly the drift this guards, so silence is not a pass.
func nixAttr(t *testing.T, src, re string) string {
	t.Helper()
	m := regexp.MustCompile(re).FindStringSubmatch(src)
	if m == nil {
		t.Fatalf("guest disk-image config no longer matches %q — the SLIRP pairing test cannot find it", re)
	}
	return m[1]
}
