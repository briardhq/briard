package host

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"briard.io/agent/drbd"
	"briard.io/shared/api"
)

// The value the household types, judged by the install's own gate.
func TestVIPSetting(t *testing.T) {
	defer func(orig func(string) bool) { answers = orig }(answers)
	answers = func(string) bool { return false }
	const host = "192.168.7.144/24"
	for _, tc := range []struct {
		value, live, want, err string
	}{
		{value: "dhcp", want: ""},
		{value: "192.168.7.50/24", want: "192.168.7.50/24"},
		{value: "192.168.7.50", want: "192.168.7.50/24"}, // a bare address takes the LAN's prefix
		{value: "10.0.0.5/24", err: "not on this machine's LAN"},
		{value: "192.168.7.144", err: "this machine's own address"},
		{value: "nonsense", err: "neither an address nor dhcp"},
	} {
		got, err := vipSetting(tc.value, host, tc.live)
		switch {
		case tc.err != "" && (err == nil || !strings.Contains(err.Error(), tc.err)):
			t.Errorf("%q: err = %v, want %q", tc.value, err, tc.err)
		case tc.err == "" && (err != nil || got != tc.want):
			t.Errorf("%q = %q, %v; want %q", tc.value, got, err, tc.want)
		}
	}
	// Something already answers for it: refused -- unless it is the guest's own current address.
	answers = func(string) bool { return true }
	if _, err := vipSetting("192.168.7.50/24", host, ""); err == nil || !strings.Contains(err.Error(), "already in use") {
		t.Errorf("an address in use was accepted: %v", err)
	}
	if got, err := vipSetting("192.168.7.50/24", host, "192.168.7.50"); err != nil || got != "192.168.7.50/24" {
		t.Errorf("pinning the address the guest holds = %q, %v", got, err)
	}
	if _, err := vipSetting("192.168.7.50", "", ""); err == nil {
		t.Error("a bare address with no readable LAN prefix must ask for one")
	}
}

// The key is set or removed; every other line is kept as it was.
func TestSetConfigKey(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.env")
	orig := "# written by install.sh\nQEMU=/opt/briard/qemu\nVIP_ADDR = 192.168.7.9/24\nNIC=wlan0\n"
	if err := os.WriteFile(p, []byte(orig), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := setConfigKey(p, "VIP_ADDR", "192.168.7.50/24"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	if want := "# written by install.sh\nQEMU=/opt/briard/qemu\nNIC=wlan0\nVIP_ADDR=192.168.7.50/24\n"; string(b) != want {
		t.Errorf("after set:\n%s\nwant:\n%s", b, want)
	}
	if err := setConfigKey(p, "VIP_ADDR", ""); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(p)
	if strings.Contains(string(b), "VIP_ADDR") || !strings.Contains(string(b), "NIC=wlan0\n") {
		t.Errorf("after removing:\n%s", b)
	}
}

// The whole act: recorded in config.env, live for the next bring-up, the refusal cleared, the guest
// restarted -- and nothing at all when the value is unchanged or the node has peers.
func TestApplyConfigSet(t *testing.T) {
	defer func(orig func(string) bool) { answers = orig }(answers)
	answers = func(string) bool { return false }
	dir := t.TempDir()
	t.Setenv("BRIARD_CONFIG", filepath.Join(dir, "config.env"))
	vip := ""
	cfg := Config{VIPDev: "eth2", vip: &vip, ipvtap: &ipvtapCopier{refusedAddr: "192.168.7.144"}}
	// No parent device here, so the prefix is given.
	set := func(c Config, key, value string) (api.DirectiveOutcome, *fakeRebooter) {
		p, _ := json.Marshal(api.ConfigSetting{Key: key, Value: value})
		rb := &fakeRebooter{}
		return c.applyConfigSet(context.Background(), api.Directive{ID: "d", Payload: string(p)},
			&fakeVIPGuest{}, rb, func(string, ...any) {}), rb
	}
	o, rb := set(cfg, "vip", "192.168.7.50/24")
	if o.State != api.OutcomeDone || rb.reboots != 1 {
		t.Fatalf("outcome %+v, restarts %d", o, rb.reboots)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "config.env"))
	if string(b) != "VIP_ADDR=192.168.7.50/24\n" || cfg.vipAddr() != "192.168.7.50/24" {
		t.Errorf("config.env %q, live %q", b, cfg.vipAddr())
	}
	if cfg.ipvtap.refused() {
		t.Error("naming an address must clear the refusal")
	}
	if o, rb := set(cfg, "vip", "192.168.7.50/24"); o.State != api.OutcomeDone || rb.reboots != 0 {
		t.Errorf("an unchanged value restarted the guest: %+v", o)
	}
	if o, _ := set(cfg, "nope", "x"); o.State != api.OutcomeFailed {
		t.Errorf("an unknown key was accepted: %+v", o)
	}
	paired := cfg
	paired.Resource.Peers = []drbd.Peer{{Name: "a"}, {Name: "b"}}
	if o, rb := set(paired, "vip", "dhcp"); o.State != api.OutcomeFailed || rb.reboots != 0 || !strings.Contains(o.Detail, "peers") {
		t.Errorf("a node with peers changed the flock's address: %+v", o)
	}
}
