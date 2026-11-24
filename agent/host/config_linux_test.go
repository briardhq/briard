package host

import (
	"os"
	"testing"
)

// ⚠️ The two halves of `declared`, which lets the NIC names default in config.go without losing
// the one meaning an empty value carries. A key nobody named takes the shipped answer (every
// install: the install writes no device names). A key named empty keeps the empty: that is how a
// node says it has no such NIC, which env() cannot express (it reads an explicit empty as unset
// and returns the default). Getting it wrong renders a NIC the node does not have, on the
// substrate where that is precisely the fork. Linux's names: Windows ships one tap and no other
// (config_windows.go), so there is nothing there to name empty.
func TestConfigFromEnv_NICNamesDefaultButAnExplicitEmptyWins(t *testing.T) {
	for _, k := range []string{"SYSTEM_TAP", "SERVICE_TAP", "WITNESS_TAP", "SYSTEM_DEV", "VIP_DEV"} {
		os.Unsetenv(k)
		t.Cleanup(func() { os.Unsetenv(k) })
	}
	c := ConfigFromEnv()
	for _, g := range []struct{ name, got, want string }{
		{"SystemTap", c.SystemTap, "briard-drbd0"},
		{"ServiceTap", c.ServiceTap, "briard0"},
		{"WitnessTap", c.WitnessTap, "briard-priv0"},
		{"SystemDev", c.SystemDev, "eth1"},
		{"VIPDev", c.VIPDev, "eth2"},
	} {
		if g.got != g.want {
			t.Errorf("unset %s = %q, want the shipped %q", g.name, g.got, g.want)
		}
	}
	t.Setenv("SERVICE_TAP", "")
	t.Setenv("WITNESS_TAP", "")
	if c := ConfigFromEnv(); c.ServiceTap != "" || c.WitnessTap != "" {
		t.Errorf("an explicit empty was overruled by the default: service tap=%q witness tap=%q",
			c.ServiceTap, c.WitnessTap)
	}
}
