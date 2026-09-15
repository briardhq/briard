package nic

import "testing"

// home is the fingerprint of an ordinary household LAN, recorded at a successful bring-up.
func home() Fingerprint {
	return Fingerprint{
		Parent: "eth0", ParentMAC: "aa:bb:cc:dd:ee:01",
		HostCIDR: "192.168.1.50/24", GatewayIP: "192.168.1.1", GatewayMAC: "11:22:33:44:55:66",
	}
}

// THE GATEWAY MAC IS THE STRONG SIGNAL, and these cases are the reason the fingerprint carries
// more than a subnet. 192.168.1.0/24 is the most common home subnet on earth, so "same subnet" is
// also exactly what carrying the box to a friend's house looks like -- and a replaced router
// renumbers what is unquestionably the same LAN. Neither direction survives on addressing alone.
func TestCompare(t *testing.T) {
	for _, tc := range []struct {
		name string
		have Fingerprint
		want Relation
	}{
		{
			// The NIC was replaced: new device, new MAC, new address even -- but the same router
			// answers on the same segment, so this is one house and one wire change.
			"a replaced NIC on the same wire",
			Fingerprint{Parent: "eth1", ParentMAC: "aa:bb:cc:dd:ee:02",
				HostCIDR: "192.168.1.77/24", GatewayIP: "192.168.1.1", GatewayMAC: "11:22:33:44:55:66"},
			SameWire,
		},
		{
			// Case-insensitive, because iproute2 and sysfs do not agree on how to spell a MAC and
			// a fingerprint that missed a match here would wait thirty minutes for no reason.
			"the same gateway, spelled in upper case",
			Fingerprint{Parent: "eth1", HostCIDR: "192.168.1.77/24", GatewayMAC: "11:22:33:44:55:AA",
				GatewayIP: "192.168.1.1"},
			SameSubnet, // a DIFFERENT mac in upper case is still different
		},
		{
			"the same gateway MAC in upper case",
			Fingerprint{Parent: "eth1", HostCIDR: "192.168.1.77/24", GatewayMAC: "11:22:33:44:55:66",
				GatewayIP: "192.168.1.1"},
			SameWire,
		},
		{
			// The router was replaced: same house, same addressing, different hardware. Identical
			// evidence to the box having been carried to another 192.168.1.0/24 -- which is why
			// this is the ambiguous tier rather than either of the confident ones.
			"a replaced router in the same house",
			Fingerprint{Parent: "eth0", ParentMAC: "aa:bb:cc:dd:ee:01",
				HostCIDR: "192.168.1.50/24", GatewayIP: "192.168.1.1", GatewayMAC: "99:99:99:99:99:99"},
			SameSubnet,
		},
		{
			"the machine moved to another network",
			Fingerprint{Parent: "eth0", HostCIDR: "10.7.0.4/24", GatewayIP: "10.7.0.1",
				GatewayMAC: "99:99:99:99:99:99"},
			Elsewhere,
		},
		{
			// A DIFFERENT-SIZED CLAIM ABOUT THE SAME STREET is not the same network: comparing
			// masked bases alone would call these one place and re-parent across a subnet change.
			"the same base address on a different prefix",
			Fingerprint{Parent: "eth0", HostCIDR: "192.168.1.50/16", GatewayIP: "192.168.1.1",
				GatewayMAC: "99:99:99:99:99:99"},
			Elsewhere,
		},
		{
			// MISSING EVIDENCE IS NEVER A MATCH. An unreadable address must send this to the
			// cautious answer, which refuses, rather than to a tier that acts.
			"nothing could be read",
			Fingerprint{Parent: "eth1"},
			Unknown,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Compare(home(), tc.have); got != tc.want {
				t.Errorf("Compare = %q, want %q", got, tc.want)
			}
		})
	}
}

// A node with NO record -- first boot after the feature landed, or a lost pet file -- must reach
// the cautious answer rather than match anything. The record is the thing a tier is measured
// against, so its absence cannot be permission.
func TestCompareWithNoRecordIsUnknown(t *testing.T) {
	if got := Compare(Fingerprint{}, home()); got != Unknown {
		t.Errorf("Compare(no record) = %q, want %q", got, Unknown)
	}
}
