package nic

import (
	"errors"
	"strings"
	"testing"
)

// The message IS the safety margin ([B.150](b)): every way the selection goes wrong ends with a
// guest that boots and is unreachable, and the only thing standing between a user and that is
// what this line tells them. So each failing shape must carry the override, a copy-pasteable
// example off THIS machine, and the devices to choose from.
func TestFixCarriesTheOverrideAndTheCandidates(t *testing.T) {
	for _, tc := range []struct {
		name string
		sel  Selection
		want []string
	}{
		{
			"no default route",
			Selection{Err: ErrNoDefaultRoute, Candidates: []string{"enp3s0", "wlan0"}},
			[]string{"no default route", "BRIARD_NIC=enp3s0", "enp3s0, wlan0"},
		},
		{
			"the default route is a full-tunnel VPN",
			Selection{Dev: "tun0", Probed: true, Candidates: []string{"enp3s0", "tun0"},
				Err: errors.New("a macvtap could not be created on it (Device does not support macvlan)")},
			[]string{"tun0 was chosen because it holds this machine's default route",
				"Device does not support macvlan", "BRIARD_NIC=enp3s0"},
		},
		{
			// A user who NAMED a device is not told how we would have guessed -- they already
			// disagreed with the guess.
			"BRIARD_NIC names something that cannot carry it",
			Selection{Dev: "docker0", Override: true, Probed: true, Candidates: []string{"enp3s0", "docker0"},
				Err: errors.New("a macvtap could not be created on it (Device does not support macvlan)")},
			[]string{"docker0 cannot carry the guest's network", "BRIARD_NIC=enp3s0"},
		},
		{
			"BRIARD_NIC names a device this machine does not have",
			Selection{Dev: "eth9", Override: true, Err: ErrNoSuchDevice, Candidates: []string{"enp3s0"}},
			[]string{"BRIARD_NIC names eth9, which this machine does not have", "enp3s0"},
		},
		{
			// The probe passes on a wireless station and the frames die at the access point, so
			// this message is the only warning there is.
			"wireless",
			Selection{Dev: "wlan0", Wireless: true, Probed: true, Candidates: []string{"wlan0"}},
			[]string{"wlan0 is wireless", "no household access point", "BRIARD_NIC=wlan0"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.sel.Fix()
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Errorf("Fix() = %q\n  missing %q", got, want)
				}
			}
		})
	}
}

// A machine with nothing but loopback gets the one message the override cannot help with: naming
// a device does not conjure one. Offering BRIARD_NIC here would be noise at the worst moment.
func TestFixOnABareMachineDoesNotOfferTheOverride(t *testing.T) {
	got := Selection{Err: ErrNoInterfaces}.Fix()
	if !strings.Contains(got, "connect this machine to your network") {
		t.Errorf("Fix() = %q", got)
	}
	if strings.Contains(got, "BRIARD_NIC") {
		t.Errorf("Fix() = %q, want no override on a machine with no device to name", got)
	}
}

// A usable selection has nothing to say. (Usable is what the install branches on, so a wireless
// device -- warned about, admitted -- must still read as usable here: its severity is the report
// card's call, not the selector's.)
func TestUsableAndTheSilentFix(t *testing.T) {
	ok := Selection{Dev: "eth0", Probed: true}
	if !ok.Usable() || ok.Fix() != "" {
		t.Errorf("a good selection: Usable=%v Fix=%q", ok.Usable(), ok.Fix())
	}
	wifi := Selection{Dev: "wlan0", Wireless: true, Probed: true}
	if !wifi.Usable() {
		t.Error("wireless is a warning, not an unusable selection")
	}
	bad := Selection{Err: ErrNoDefaultRoute}
	if bad.Usable() {
		t.Error("no device selected must not read as usable")
	}
}

// firstLine is what a user actually reads when `ip` refuses, so it must reach for iproute2's own
// sentence and strip the "Error: " noise -- and must never render an empty string when the
// command said nothing at all.
func TestFirstLine(t *testing.T) {
	out := "Error: argument \"tun0\" is wrong: Device does not support macvlan\n\n"
	if got := firstLine([]byte(out), errors.New("exit status 1")); got != "argument \"tun0\" is wrong: Device does not support macvlan" {
		t.Errorf("firstLine = %q", got)
	}
	if got := firstLine(nil, errors.New("exec: \"ip\": executable file not found in $PATH")); !strings.Contains(got, "not found") {
		t.Errorf("a silent command must fall back to the exec error, got %q", got)
	}
}
