package host

import (
	"fmt"

	"briard.io/agent/drbd"
	"briard.io/agent/platform"
	"briard.io/shared/nodestorage"
)

// THE NODE'S STORAGE SPEC IS THE HOST'S TO RENDER ([V3b.33](d), AGENTS §5).
//
// Storage policy is a node-scoped fact the host holds durably and pushes at bring-up, and until
// this existed the guest decided it: [V3b.33](b)/(c) built the seam in a BOOT unit whose only
// inputs were facts it could read off the machine. That shape has no way to carry a decision --
// which is exactly why Adiantum, specified as an install-time opt-in, could not be built. The
// host renders the whole spec here and the guest carries it out.

// dataTier is the one tier every diskful node holds: the replicated volume, on the second virtio
// disk, as the two-LV VG the seam invariant fences (INVARIANTS §13) -- the data LV and, at the
// end of the PV, DRBD's external metadata ([B.145a]).
//
// The names come from the two packages that already own them -- the raw device from the file
// that attaches it, the VG and LVs from the package DRBD's backing paths are composed in -- so
// this function introduces no further literal for any of them.
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
// It takes the resource and the two decisions rather than reading them off c, because the join
// path brings up a DIFFERENT resource under different terms -- a joiner is hard-wired
// FreshInit=false, and a witness is diskless -- and a builder that read cfg.FreshInit would seed
// a joiner ([V3b.33](d) does not change that rule; it only moves where it is carried out).
//
// It returns an error rather than a spec-and-a-hope: the unit on the other side of this document
// runs luksFormat and lvcreate, so a policy nobody can parse must stop bring-up here, loudly,
// rather than quietly becoming the default. That is the whole reason DataEncryption is not
// validated at ConfigFromEnv time and then trusted -- one gate, at the point of use.
//
// EXPORTED FOR THE TEST DRIVER, the harness that stands in for this agent (nixosTest/driver): it
// renders its spec through here rather than assembling one of its own, so a rig exercises the
// product's composition -- the DATA_ENCRYPTION knob included -- instead of a second opinion about
// it. That is the whole point of [V3b.33](d)'s re-cut.
func (c Config) StorageSpec(res drbd.Resource, diskless, freshInit bool) (nodestorage.Spec, error) {
	spec := nodestorage.Spec{
		Resource: nodestorage.Resource{
			Name:     res.Name,
			Device:   res.Device,
			Config:   res.Config(),
			Diskless: diskless,
			MaxPeers: drbd.MaxPeers,
			// A DISKLESS NODE NEVER SEEDS, and dropping the flag here is what keeps that
			// structural rather than a rule somebody has to remember. cfg.FreshInit is computed
			// from the peer list ("am I the first peer"), which knows nothing about roles, so a
			// witness can carry it; there has simply never been anything for it to do, since a
			// diskless bring-up creates no metadata and holds no volume to format. Expressed in
			// the document it would be a claim the guest could act on, so it does not reach it
			// -- and Validate refuses the pair outright, for a spec written by hand.
			FreshInit: freshInit && !diskless,
		},
	}
	if !diskless {
		tier := dataTier(c.DataEncryption)
		spec.Tiers = []nodestorage.Tier{tier}
		// ★ THE TIER THIS NODE BUILDS MUST BE THE DEVICE ITS OWN `on` STANZA ATTACHES. The two
		// halves are rendered from different places -- the tier from the seam's VG/LV, the .res
		// from the peer list -- and nothing else in bring-up holds both, so a disagreement
		// surfaces as a node that creates a volume and then attaches nothing ("No valid meta
		// data found"), with the cause three components away from the message.
		//
		// Matched on the peer whose name is ours, because that is how DRBD itself picks the
		// stanza. A node absent from its own mesh is a different fault, and DRBD says so
		// loudly enough that guessing here would only get in the way.
		for _, p := range res.Peers {
			if p.Name == c.Node && p.Disk != "" && p.Disk != tier.Mapper() {
				return nodestorage.Spec{}, fmt.Errorf("node %s: the seam builds %s but %s attaches %s -- every diskful node runs on the one LV since [V3b.33](b)", c.Node, tier.Mapper(), res.Name, p.Disk)
			}
		}
	}
	if err := spec.Validate(); err != nil {
		return nodestorage.Spec{}, fmt.Errorf("node %s: %w", c.Node, err)
	}
	return spec, nil
}
