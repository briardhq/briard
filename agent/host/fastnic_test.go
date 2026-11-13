package host

import (
	"strings"
	"testing"

	"briard.io/shared/notify"
)

// Faster than gigabit opens the notice, naming the NIC and the procedure; gigabit and below, or
// no NIC in the bridge, resolves it; a speed that cannot be read says nothing. The rig is gigabit
// by construction, so this is where the open arm is proven.
func TestFastNICAlert(t *testing.T) {
	for _, tc := range []struct {
		member string
		bps    uint64
		want   notify.Kind // "" for nothing said
	}{
		{"Ethernet", 1_000_000_000, notify.Resolved},
		{"Ethernet", 100_000_000, notify.Resolved},
		{"Ethernet", 2_500_000_000, notify.Open},
		{"", 10_000_000_000, notify.Resolved}, // no NIC in the bridge: the condition is gone
		{"", 0, notify.Resolved},
		{"Ethernet", 0, ""}, // the speed unread: an unknown is neither
	} {
		al, ok := fastNICAlert(tc.member, tc.bps)
		if got := map[bool]notify.Kind{true: al.Kind}[ok]; got != tc.want {
			t.Errorf("fastNICAlert(%q, %d) = %q, want %q", tc.member, tc.bps, got, tc.want)
		}
		if ok && al.Key != "fast-nic" {
			t.Errorf("alert key %q, want fast-nic", al.Key)
		}
		if al.Kind == notify.Open && (!strings.Contains(al.Body, tc.member) || !strings.Contains(al.Body, "2.5 Gbit/s")) {
			t.Errorf("alert does not name the NIC and its speed: %+v", al)
		}
	}
}
