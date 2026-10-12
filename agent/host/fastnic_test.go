package host

import (
	"strings"
	"testing"
)

// Gigabit and below say nothing; faster earns the warning, naming the NIC and the procedure. The
// rig is gigabit by construction, so this is where the positive arm is proven.
func TestFastNICAlert(t *testing.T) {
	for _, tc := range []struct {
		member string
		bps    uint64
		want   bool
	}{
		{"Ethernet", 1_000_000_000, false},
		{"Ethernet", 100_000_000, false},
		{"Ethernet", 2_500_000_000, true},
		{"", 10_000_000_000, false}, // no member found: nothing to say about it
	} {
		al, ok := fastNICAlert(tc.member, tc.bps)
		if ok != tc.want {
			t.Errorf("fastNICAlert(%q, %d) fired = %v, want %v", tc.member, tc.bps, ok, tc.want)
		}
		if ok && (!strings.Contains(al.Body, tc.member) || !strings.Contains(al.Body, "2.5 Gbit/s") || al.Key != "fast-nic") {
			t.Errorf("alert does not name the NIC, its speed and its key: %+v", al)
		}
	}
}
