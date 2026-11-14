package host

import (
	"fmt"

	"briard.io/agent/drbd"
	"briard.io/agent/platform"
	"briard.io/shared/nodestorage"
)

// The node's storage spec is the host's to render. Storage policy is a node-scoped fact the host
// holds durably and pushes at bring-up: the host renders the whole spec here and the guest carries
// it out. A guest deciding alone could use only facts it reads off the machine, so could never
// carry a decision such as the install-time DATA_ENCRYPTION choice (Adiantum).

// dataTier is the one tier every diskful node holds: the replicated volume, on the second virtio
// disk, as the two-LV VG the seam invariant fences -- the data LV and, at the end of the PV, DRBD's
// external metadata. The names come from the two packages that already own them (the raw device
// from the file that attaches it, the VG and LVs from the package DRBD's backing paths are
// composed in), so this introduces no further literal for any of them.
func dataTier(mode nodestorage.Mode) nodestorage.Tier {
	return nodestorage.Tier{
		Name:   nodestorage.TierData,
		Device: platform.GuestDataDevice,
		VG:     drbd.DataVG,
		LV:     drbd.DataLV,
		MetaLV: drbd.MetaLV,
		Mode:   mode,
	}
}

// StorageSpec renders the node-storage spec for one resource bring-up: what block layout this
// node builds, and the .res it attaches on top of it.
//
// It takes the resource and the two decisions rather than reading them off c because the join
// path (pair.go) brings up a different resource under different terms -- a joiner is hard-wired
// FreshInit=false, a witness is diskless -- and a builder that read cfg.FreshInit would seed a
// joiner. It returns an error rather than a spec-and-a-hope: the unit on the other side runs
// luksFormat and lvcreate, so a policy nobody can parse must stop bring-up here, loudly, rather
// than quietly becoming the default -- the whole reason DataEncryption is not validated at
// ConfigFromEnv time and then trusted: one gate, at the point of use.
//
// Exported for the test driver, the harness that stands in for this agent (nixosTest/driver): it
// renders its spec through here rather than assembling its own, so a rig exercises the product's
// composition -- the DATA_ENCRYPTION knob included -- instead of a second opinion about it.
func (c Config) StorageSpec(res drbd.Resource, diskless, freshInit bool) (nodestorage.Spec, error) {
	// A resource is worth running only with two copies: with one diskful member DRBD protects
	// nothing and turns any single bad block into a dead node, so a lone anchor runs btrfs on its
	// LV directly and DRBD is introduced when the second anchor joins. The rule is the diskful
	// count: a witness beside one anchor is that same lone anchor, and a witness with nothing to
	// arbitrate between is a spec that must not be written at all.
	replicated := res.DiskfulPeers() >= 2
	if diskless && !replicated {
		return nodestorage.Spec{}, fmt.Errorf("node %s: a diskless node in a mesh with %d diskful member(s) has nothing to arbitrate between", c.Node, res.DiskfulPeers())
	}
	spec := nodestorage.Spec{
		Resource: nodestorage.Resource{
			Name:       res.Name,
			Device:     res.Device,
			Replicated: replicated,
			Config:     res.Config(),
			Diskless:   diskless,
			MaxPeers:   drbd.MaxPeers,
			// A diskless node never seeds, and dropping the flag here keeps that structural rather
			// than a rule somebody has to remember. cfg.FreshInit is computed from the peer list
			// ("am I the first peer"), which knows nothing about roles, so a witness can carry it,
			// though there has never been anything for it to do: a diskless bring-up creates no
			// metadata and holds no volume to format. In the document it would be a claim the
			// guest could act on, so it does not reach it -- and Validate refuses the pair
			// outright, for a spec written by hand.
			FreshInit: freshInit && !diskless,
		},
	}
	if !diskless {
		tier := dataTier(c.DataEncryption)
		spec.Tiers = []nodestorage.Tier{tier}
		// The one field that differs between the topologies: what briard-primary-storage mounts.
		if !replicated {
			spec.Resource.Device = tier.Mapper()
		}
		// ★ The tier this node builds must be the device its own `on` stanza attaches. The two
		// halves are rendered from different places -- the tier from the seam's VG/LV, the .res
		// from the peer list -- and nothing else in bring-up holds both, so a disagreement
		// surfaces as a node that creates a volume and then attaches nothing ("No valid meta data
		// found"), the cause three components away from the message. Matched on the peer whose
		// name is ours, as DRBD itself picks the stanza; a node absent from its own mesh is a
		// different fault, and DRBD says so loudly enough that guessing here would only get in
		// the way.
		for _, p := range res.Peers {
			if p.Name == c.Node && p.Disk != "" && p.Disk != tier.Mapper() {
				return nodestorage.Spec{}, fmt.Errorf("node %s: the seam builds %s but %s attaches %s -- every diskful node runs on the one LV", c.Node, tier.Mapper(), res.Name, p.Disk)
			}
		}
	}
	if err := spec.Validate(); err != nil {
		return nodestorage.Spec{}, fmt.Errorf("node %s: %w", c.Node, err)
	}
	return spec, nil
}
