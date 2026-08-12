package deadman

import (
	"os"
	"strconv"
	"strings"
	"testing"
)

// The private host<->guest link is spelled out in four languages — Go (here), Nix (the guest image
// bakes the guest's eth3 address and the deadman unit's BRIARD_GATE_ADDR), shell (install.sh puts
// the host's end on the tap), and again in the cloud's mesh composer. Nothing can import across
// those boundaries, so a rename or a renumber on any one side is silent.
//
// It is worth a test rather than a comment because of WHAT breaks. The gate is read by the host
// exactly when the control channel is dead, so a mismatch does not fail anywhere visible: the
// host dials an address nothing answers on, reads that as "unreachable", and falls through to
// rebooting the guest unconditionally — which is the pre-gate behaviour, passing every other test
// in the tree. The guard would be gone and nothing would say so.
func TestGateAddressesMatchTheGuestImageAndInstaller(t *testing.T) {
	image := readFile(t, "../../../guest-image/disk-image.nix")
	installer := readFile(t, "../../../scripts/install.sh")

	// The guest bakes its own end on eth3, and points the deadman's listener at the same address.
	if !strings.Contains(image, `address = "`+GuestIP+`"`) {
		t.Errorf("guest image does not bake eth3 = %s (Go GuestIP); the gate would listen where nothing routes", GuestIP)
	}
	if want := `BRIARD_GATE_ADDR = "` + GateAddr() + `"`; !strings.Contains(image, want) {
		t.Errorf("guest image deadman unit does not set %s — Go says the host will dial %s", want, GateAddr())
	}

	// The host puts its end on the tap. Without it the link has one end and the guest's replies
	// have nowhere to go.
	if !strings.Contains(installer, `PRIV_HOST_CIDR="`+HostIP+`/`) {
		t.Errorf("install.sh does not address the private tap with %s (Go HostIP)", HostIP)
	}
	// And it must create that link on EVERY install, not only a managed pairing — the whole point
	// of the change that introduced this test. WITNESS_TAP unset means no eth3 at all.
	if !strings.Contains(installer, "Environment=WITNESS_TAP=$PRIV_TAP") {
		t.Error("install.sh no longer passes WITNESS_TAP unconditionally; the private link (and the gate with it) is absent on plain installs")
	}

	// The port travels only inside GateAddr above, so assert it is the number the docs and the
	// firewall-free guest assume, rather than silently following a typo.
	if GatePort != 7790 {
		t.Errorf("GatePort = %d; the guest image, DESIGN and the host rung all say 7790", GatePort)
	}
	if !strings.HasSuffix(GateAddr(), ":"+strconv.Itoa(GatePort)) {
		t.Errorf("GateAddr() = %q does not end in the gate port %d", GateAddr(), GatePort)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}
