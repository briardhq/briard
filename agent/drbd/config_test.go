package drbd

import (
	"strings"
	"testing"
)

// The 2-disk + diskless-witness topology.
func witnessResource() Resource {
	return Resource{
		Name:   "r0",
		Device: "/dev/drbd0",
		Peers: []Peer{
			{Name: "node1", NodeID: 0, Address: "10.0.0.1:7789", Disk: "/dev/vdb"},
			{Name: "node2", NodeID: 1, Address: "10.0.0.2:7789", Disk: "/dev/vdb"},
			{Name: "witness", NodeID: 2, Address: "10.0.0.3:7789"},
		},
	}
}

// forwardedWitnessResource is the 2-anchor + cloud-witness mesh: each anchor reaches the
// diskless witness through its OWN host forwarder (WitnessLocal 10.9.9.2 → forwarder 10.9.9.1),
// while the anchors interconnect directly on the LAN (10.7.0.1/.2).
func forwardedWitnessResource() Resource {
	return Resource{
		Name:   "r0",
		Device: "/dev/drbd0",
		Peers: []Peer{
			{Name: "n1", NodeID: 0, Address: "10.7.0.1:7789", Disk: "/dev/vdb", WitnessLocal: "10.9.9.2:7789"},
			{Name: "n2", NodeID: 1, Address: "10.7.0.2:7789", Disk: "/dev/vdb", WitnessLocal: "10.9.9.2:7789"},
			{Name: "cloud-witness", NodeID: 2, Address: "10.9.9.1:7789"},
		},
	}
}

// A forwarded witness renders explicit `connection` stanzas (not connection-mesh) so the
// anchor↔witness path can carry a per-connection local address. Golden-matched
// against the nixosTest/witness-proxy.nix shape, re-addressed for the host-side private link.
func TestResourceConfigForwardedWitness(t *testing.T) {
	const want = `resource r0 {
	net {
		protocol C;
	}
	options {
		auto-promote no;
		quorum majority;
		on-no-quorum io-error;
		on-suspended-primary-outdated force-secondary;
	}
	on n1 {
		node-id 0;
		volume 0 {
			device /dev/drbd0;
			disk /dev/vdb;
			meta-disk internal;
		}
	}
	on n2 {
		node-id 1;
		volume 0 {
			device /dev/drbd0;
			disk /dev/vdb;
			meta-disk internal;
		}
	}
	on cloud-witness {
		node-id 2;
		volume 0 {
			device /dev/drbd0;
			disk none;
		}
	}
	connection {
		host n1 address 10.7.0.1:7789;
		host n2 address 10.7.0.2:7789;
	}
	connection {
		host n1 address 10.9.9.2:7789;
		host cloud-witness address 10.9.9.1:7789;
	}
	connection {
		host n2 address 10.9.9.2:7789;
		host cloud-witness address 10.9.9.1:7789;
	}
}
`
	if got := forwardedWitnessResource().Config(); got != want {
		t.Errorf("forwarded-witness .res mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// The forwarded-witness form must NOT fall through to connection-mesh, and its `on` blocks must
// carry no address (addresses live in the connections). These are the two ways a fall-through to
// the plain renderer would show up — assert against both ([[verification-assertions-must-fail]]).
func TestForwardedWitnessAvoidsMeshForm(t *testing.T) {
	got := forwardedWitnessResource().Config()
	if strings.Contains(got, "connection-mesh") {
		t.Error("forwarded witness must use explicit connections, not connection-mesh")
	}
	if strings.Contains(got, "\t\taddress ") {
		t.Error("explicit form must keep addresses in connections, not in `on` blocks")
	}
	// The witness is reached through the per-anchor host forwarder, once per anchor.
	if n := strings.Count(got, "host cloud-witness address 10.9.9.1:7789;"); n != 2 {
		t.Errorf("want the witness forwarder addressed once per anchor (2), got %d", n)
	}
}

// A diskless witness WITHOUT any WitnessLocal is the plain LAN-witness case: it must stay on
// the connection-mesh path, unchanged. Guards the split predicate against over-triggering.
func TestLANWitnessStaysMesh(t *testing.T) {
	got := witnessResource().Config()
	if !strings.Contains(got, "connection-mesh") {
		t.Error("a LAN witness (no WitnessLocal) must still render as connection-mesh")
	}
}

func TestResourceConfig(t *testing.T) {
	const want = `resource r0 {
	net {
		protocol C;
	}
	options {
		auto-promote no;
		quorum majority;
		on-no-quorum io-error;
		on-suspended-primary-outdated force-secondary;
	}
	on node1 {
		node-id 0;
		address 10.0.0.1:7789;
		volume 0 {
			device /dev/drbd0;
			disk /dev/vdb;
			meta-disk internal;
		}
	}
	on node2 {
		node-id 1;
		address 10.0.0.2:7789;
		volume 0 {
			device /dev/drbd0;
			disk /dev/vdb;
			meta-disk internal;
		}
	}
	on witness {
		node-id 2;
		address 10.0.0.3:7789;
		volume 0 {
			device /dev/drbd0;
			disk none;
		}
	}
	connection-mesh {
		hosts node1 node2 witness;
	}
}
`
	if got := witnessResource().Config(); got != want {
		t.Errorf("Config() mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// The witness is diskless — the sole "disk none" (single-primary is preserved by
// auto-promote no; a diskless voter never carries data). Guards the branch.
func TestResourceConfigDisklessWitness(t *testing.T) {
	got := witnessResource().Config()
	if n := strings.Count(got, "disk none;"); n != 1 {
		t.Errorf("want exactly 1 diskless volume, got %d", n)
	}
	if n := strings.Count(got, "meta-disk internal;"); n != 2 {
		t.Errorf("want 2 disk-backed volumes with meta-disk, got %d", n)
	}
}

func TestReactorConfigStartOrder(t *testing.T) {
	// adjust-resource-on-start pins the layer line: the promoter must NOT attach the backing
	// device, because the agent brings the resource up via drbd@<res>.target and reads the result.
	// Asserted literally -- the default is true, so its ABSENCE is the bug (V3.22).
	const want = `[[promoter]]
[promoter.resources.r0]
adjust-resource-on-start = false
start = [ "briard-data.service", "podman-briard-payload.service", "briard-vip.service" ]
`
	got := ReactorConfig("r0", []string{
		"briard-data.service",
		"podman-briard-payload.service",
		"briard-vip.service",
	})
	if got != want {
		t.Errorf("ReactorConfig() mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}
