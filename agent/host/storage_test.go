package host

import (
	"strings"
	"testing"

	"briard.io/agent/drbd"
	"briard.io/agent/platform"
	"briard.io/shared/nodestorage"
)

func demoRes() drbd.Resource {
	return drbd.Resource{
		Name:   "r0",
		Device: "/dev/drbd0",
		Peers:  []drbd.Peer{{Name: "n1", NodeID: 0, Address: "10.0.0.1:7789", Disk: drbd.DataDevice}},
	}
}

// ★ THE FENCE THAT MAKES THE SPEC AND THE .RES NAME ONE DEVICE. The host tells the guest which VG
// and LV to build, and separately tells DRBD which mapper path to attach; if those two stop
// composing to the same string the node creates a volume and then attaches nothing. Nothing else
// in the tree holds both halves.
//
// PROVEN ABLE TO FAIL by the second half: a peer whose Disk is the raw disk is refused rather
// than brought up. That is a LIVE path, not a hypothetical: `parsePeers` normalises any PEERS
// value to the seam LV, but `meshTarget` copies Disk verbatim off the wire from the cloud's
// pairing directive (pair.go) -- so the mesh a joiner is handed is the one thing that can still
// name a device this node does not build, and a joiner that attaches nothing is the worst place
// to find out.
func TestStorageSpecFencesTheBackingDevice(t *testing.T) {
	cfg := Config{Node: "n1", DataEncryption: nodestorage.ModeAuto}
	spec, err := cfg.StorageSpec(demoRes(), false, false)
	if err != nil {
		t.Fatalf("the shipped wiring was refused: %v", err)
	}
	tier, _ := spec.Tier(nodestorage.TierData)
	if !strings.Contains(spec.Resource.Config, tier.Mapper()) {
		t.Errorf("the .res does not name the tier this node builds (%s):\n%s", tier.Mapper(), spec.Resource.Config)
	}
	if tier.Device != platform.GuestDataDevice {
		t.Errorf("the tier is built on %s, but the host attaches the data disk at %s", tier.Device, platform.GuestDataDevice)
	}

	elsewhere := demoRes()
	elsewhere.Peers[0].Disk = platform.GuestDataDevice // the raw disk, under the seam
	if _, err := cfg.StorageSpec(elsewhere, false, false); err == nil {
		t.Error("a .res attaching the raw disk was accepted; the node would build an LV and attach nothing")
	}

	// A PEER THAT IS NOT US IS NOT OURS TO JUDGE: the mesh is written once and shipped to every
	// node, and DRBD picks its stanza by hostname. Refusing on someone else's line would make a
	// node fail over a device it never touches.
	other := demoRes()
	other.Peers = append(other.Peers, drbd.Peer{Name: "n2", NodeID: 1, Address: "10.0.0.2:7789", Disk: "/dev/sdz"})
	if _, err := cfg.StorageSpec(other, false, false); err != nil {
		t.Errorf("refused over another node's backing: %v", err)
	}
}

func TestStorageSpecDiskful(t *testing.T) {
	cfg := Config{Node: "n1", DataEncryption: nodestorage.ModeAuto}
	spec, err := cfg.StorageSpec(demoRes(), false, true)
	if err != nil {
		t.Fatal(err)
	}
	tier, ok := spec.Tier(nodestorage.TierData)
	if !ok {
		t.Fatalf("a diskful node got no data tier: %+v", spec)
	}
	if tier.Mode != nodestorage.ModeAuto {
		t.Errorf("tier mode = %q; the host's policy did not reach the spec", tier.Mode)
	}
	if !spec.Resource.FreshInit {
		t.Error("the seed's FreshInit did not reach the spec; the volume would never be formatted")
	}
	if spec.Resource.Config != demoRes().Config() {
		t.Error("the spec carries a .res the host did not render")
	}
}

// Every mode the host may hold has to survive the trip, Adiantum included -- carrying it is the
// whole reason this document exists ([V3b.33](c) could not build it for want of a channel).
func TestStorageSpecCarriesEveryMode(t *testing.T) {
	for _, m := range []nodestorage.Mode{nodestorage.ModeAuto, nodestorage.ModeOff, nodestorage.ModeAdiantum} {
		cfg := Config{Node: "n1", DataEncryption: m}
		spec, err := cfg.StorageSpec(demoRes(), false, false)
		if err != nil {
			t.Fatalf("mode %q: %v", m, err)
		}
		if tier, _ := spec.Tier(nodestorage.TierData); tier.Mode != m {
			t.Errorf("mode %q arrived as %q", m, tier.Mode)
		}
	}
}

// A policy nobody can parse stops bring-up here rather than rounding to the default: the caller
// asked for something, and quietly encrypting a node that asked not to be is not a recoverable
// mistake.
func TestStorageSpecRefusesAnUnknownMode(t *testing.T) {
	cfg := Config{Node: "n1", DataEncryption: "aes256"}
	if _, err := cfg.StorageSpec(demoRes(), false, false); err == nil {
		t.Error("an unknown encryption mode was accepted")
	}
	// ...and the empty one, which is what a Config built by hand rather than by ConfigFromEnv
	// carries. Silently reading it as `auto` is the same mistake in the other direction.
	if _, err := (Config{Node: "n1"}).StorageSpec(demoRes(), false, false); err == nil {
		t.Error("an unset encryption mode was accepted")
	}
}

// A witness builds no tier, and cannot seed a flock however the peer list numbered it: FreshInit
// is computed from "am I the first peer", which knows nothing about roles.
func TestStorageSpecDiskless(t *testing.T) {
	cfg := Config{Node: "w1", DataEncryption: nodestorage.ModeAuto}
	spec, err := cfg.StorageSpec(demoRes(), true, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(spec.Tiers) != 0 {
		t.Errorf("a diskless witness was given %d tier(s): %+v", len(spec.Tiers), spec.Tiers)
	}
	if spec.Resource.FreshInit {
		t.Error("a diskless witness carries FreshInit; it holds no volume to declare UpToDate")
	}
	if spec.Resource.Config != demoRes().Config() {
		t.Error("a witness still needs the .res -- it attaches nothing but it connects")
	}
}

// ConfigFromEnv's default is the shipped policy, and BRIARD_DATA_ENCRYPTION reaches it.
func TestConfigFromEnvDataEncryption(t *testing.T) {
	if got := ConfigFromEnv().DataEncryption; got != nodestorage.ModeAuto {
		t.Errorf("DataEncryption default = %q, want %q", got, nodestorage.ModeAuto)
	}
	t.Setenv("DATA_ENCRYPTION", string(nodestorage.ModeOff))
	if got := ConfigFromEnv().DataEncryption; got != nodestorage.ModeOff {
		t.Errorf("DATA_ENCRYPTION=off arrived as %q", got)
	}
}

// THE TWO-LV LAYOUT REACHES THE SPEC ([B.145a]): the metadata LV by the name the .res's
// `meta-disk` will look for, the replicated device the mount unit will read, and the peer-slot
// count the metadata is created with -- all from the constants the .res itself is rendered from.
func TestStorageSpecCarriesTheMetadataLayout(t *testing.T) {
	cfg := Config{Node: "n1", DataEncryption: nodestorage.ModeAuto}
	spec, err := cfg.StorageSpec(demoRes(), false, false)
	if err != nil {
		t.Fatal(err)
	}
	tier, _ := spec.Tier(nodestorage.TierData)
	if tier.MetaLV != drbd.MetaLV || tier.MetaMapper() != drbd.MetaDevice {
		t.Errorf("metadata LV = %q (%s), want %q (%s)", tier.MetaLV, tier.MetaMapper(), drbd.MetaLV, drbd.MetaDevice)
	}
	if !strings.Contains(spec.Resource.Config, "meta-disk "+tier.MetaMapper()+";") {
		t.Errorf("the .res does not keep its metadata on the LV this node builds:\n%s", spec.Resource.Config)
	}
	if spec.Resource.Device != demoRes().Device {
		t.Errorf("resource device = %q, want %q", spec.Resource.Device, demoRes().Device)
	}
	if spec.Resource.MaxPeers != drbd.MaxPeers {
		t.Errorf("maxPeers = %d, want the product constant %d", spec.Resource.MaxPeers, drbd.MaxPeers)
	}
}
