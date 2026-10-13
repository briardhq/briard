package nic

import "testing"

// The one path a reader cannot infer: the knob is per-DEVICE under conf/<dev>/, not the `default`
// or `all` node -- writing those would change the host's own policy rather than this one macvtap's.
func TestDisableIPv6PathIsPerDevice(t *testing.T) {
	if w := "/proc/sys/net/ipv6/conf/sys-n1/disable_ipv6"; disableIPv6Path("sys-n1") != w {
		t.Errorf("disableIPv6Path = %q, want %q", disableIPv6Path("sys-n1"), w)
	}
}
