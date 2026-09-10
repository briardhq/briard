// Package nodestorage is the node's block-storage spec: the document the HOST renders and the
// guest's briard-node-storage.service reads to build every tier this node holds ([V3b.33](d)).
//
// IT EXISTS BECAUSE STORAGE POLICY IS THE HOST'S (AGENTS §5: a node-scoped fact the host holds
// durably and pushes at bring-up). [V3b.33](b)/(c) shipped the seam as a BOOT unit whose only
// inputs were facts it could read off the machine -- today /proc/cpuinfo -- and that shape cannot
// carry a decision anybody made: the moment there is one (do not encrypt this node; force
// Adiantum; here is a key) a boot unit has nowhere to read it from. That is precisely why
// Adiantum went unbuilt in (c) -- it was specified as an install-time opt-in, and an install-time
// value had no channel to the guest at boot. This document is that channel, and the unit that
// reads it is started by the host rather than by multi-user.target.
//
// THE PRECEDENT IS shared/routes: one JSON document under /run that one side writes and another
// reads, node-local and on tmpfs, so it cannot outlive the boot it describes. It is a LIST of
// tiers because a node holds tiers -- one today (the replicated data volume), a bulk/NAS tier
// later -- and a per-tier document is the shape that does not have to be re-cut when the second
// one arrives.
//
// WHAT IS DELIBERATELY NOT HERE: the drbd-reactor promoter snippet. Arming the promoter is its
// own verb and deliberately the LAST act of bring-up ([V3b.16a]), so a document named
// node-storage that also carried the promotion chain would be exactly the drift the seam's own
// fence (INVARIANTS §13) exists to catch. Storage brings the resource up to `/dev/drbd0 attached`
// and stops there.
package nodestorage

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// Path is where the host writes the spec and briard-node-storage.service reads it. Under /run,
// which is tmpfs: the spec describes THIS boot's storage, and a stale one surviving a reboot is a
// node building a layout nobody asked for on this launch.
const Path = "/run/briard/node-storage.json"

// TierData is the tier every diskful node holds: the replicated volume DRBD attaches to, and the
// only tier there is today. Named so the second one (bulk/NAS, fenced out of v3b) arrives as an
// entry beside it rather than as a re-cut of this document.
const TierData = "data"

// Mode is a tier's encryption policy -- the knob (b)/(c) had nowhere to put.
type Mode string

const (
	// ModeAuto is the shipped default: encrypt with AES-XTS where the guest's CPU has `aes`,
	// run in the clear where it does not, and say which in the report card. Auto is resolved
	// INSIDE THE GUEST and cannot be resolved here, which is the whole reason it is a mode
	// rather than a boolean the host computes: the guest's view of the CPU is not the host's.
	// `-cpu max` passes the host's own CPU through under KVM and does not under TCG or WHPX,
	// and qemu's default qemu64 hides `aes` outright -- so the one place where silicon,
	// accelerator and CPU model compose into a single answer is /proc/cpuinfo in the guest.
	ModeAuto Mode = "auto"
	// ModeOff formats the tier in the clear whatever the CPU can do. It is not the same
	// statement as auto-on-an-AES-less-box: that one is hardware reporting a fact, this one is
	// somebody deciding.
	ModeOff Mode = "off"
	// ModeAdiantum encrypts with a cipher that does not need AES instructions -- the AES-less
	// fallback (c) specified and could not build. NEVER PROMOTED, and the item is explicit
	// about it: it is a documented opt-in for hardware that has no acceleration, not a second
	// default. A fleet that splits on the AES axis splits on something hardware-determined and
	// reportable; a fleet that splits on taste does not.
	ModeAdiantum Mode = "adiantum"
)

// Valid reports whether m is a mode this build knows how to carry out. There is no zero-value
// default: an empty mode is a spec that was built wrong, and guessing "auto" for it would let a
// host that meant `off` ship an encrypted node.
func (m Mode) Valid() bool {
	switch m {
	case ModeAuto, ModeOff, ModeAdiantum:
		return true
	}
	return false
}

// Tier is one block-storage tier this node holds: the raw device, the VG built on it (via LUKS,
// per Mode), and its two LVs -- the data LV DRBD attaches to and the metadata LV it keeps its
// external metadata on ([B.145a]).
//
// THE LINEAR LV IS THE SEAM ([V3b.33], INVARIANTS §13). The data LV's table is one `linear` line,
// so the backing can be moved onto an encrypted PV and back with `pvmove` while the device object
// above it never closes. Anything that adds a second dm layer here (striped, cached, thin) stops
// the seam being free, which is why the oracle fences the shape rather than trusting this
// document.
type Tier struct {
	// Name is what this tier is called in logs, the report card and later specs -- "data" for
	// the replicated volume every node has today. Unique within a spec.
	Name string `json:"name"`
	// Device is the raw block device the tier is built on, e.g. /dev/vdb. A HARDWARE FACT of
	// the VM the host builds, which is why it can be stated here rather than discovered.
	Device string `json:"device"`
	// VG and LV name the volume group and the data logical volume inside it. Both are
	// carried explicitly rather than derived from Name: what DRBD's .res file already names is
	// a mapper path, and a document that recomputed it from a tier name would be a second
	// opinion about the same string.
	VG string `json:"vg"`
	LV string `json:"lv"`
	// MetaLV names the SECOND logical volume in the VG: DRBD's external metadata ([B.145a]).
	// It sits at the END of the PV at a size computed from the data LV (MetadataBytes), which
	// is DRBD's own internal-metadata placement spelled in LVM -- and what makes a filesystem
	// that already fills the data LV convertible to a replicated one in place: `create-md`
	// writes here and never has to shrink anything.
	MetaLV string `json:"metaLV"`
	// Mode is this tier's encryption policy. Per tier because LUKS sits UNDER DRBD, so the
	// choice is a node-local one -- which is also what makes the AES axis free.
	Mode Mode `json:"mode"`
}

// Mapper is the path the LV appears at once the VG is active, and therefore what a resource
// config must name as its backing.
//
// ⚠️ THE DASH IS THE TRAP, and Validate refuses it rather than encoding it here: device-mapper
// escapes a dash in a VG or LV name by DOUBLING it, so a VG called `br-iard` appears at
// /dev/mapper/br--iard-data and this concatenation would quietly name a device that does not
// exist. Refusing the character keeps one rule instead of two implementations of an escaping
// scheme -- and our names have never had one.
func (t Tier) Mapper() string { return "/dev/mapper/" + t.VG + "-" + t.LV }

// MetaMapper is the metadata LV's path, and therefore what a resource config names as
// `meta-disk`. Same composition, same dash rule, as Mapper.
func (t Tier) MetaMapper() string { return "/dev/mapper/" + t.VG + "-" + t.MetaLV }

// MetadataBytes is how much external metadata DRBD needs for a data device of dataBytes with
// room for maxPeers peers -- the size the metadata LV is created at.
//
// THE FORMULA IS DRBD's, restated so the LV can be carved before drbdmeta ever runs: the bitmap
// keeps one bit per 4 KiB block PER PEER (so dataBytes/32768 bytes a peer), plus the activity log
// (32 KiB at the default stripe) and the superblock. The trailing MiB covers those two and rounds
// the answer away from "byte-tight": `drbdmeta` refuses a device that is too small and is content
// with one that is too large, so erring up is free and erring down is a conversion that fails at
// the one moment it must not. The caller rounds up to whole extents and adds one more.
//
// ⚠️ maxPeers IS BAKED INTO THE METADATA at create-md and cannot be changed without recreating it
// (a full resync), which is why it is carried in the spec and passed explicitly rather than left
// to drbdadm's default of "however many peers the .res names today".
func MetadataBytes(dataBytes int64, maxPeers int) int64 {
	const blocksPerByte = 4096 * 8 // one bitmap bit per 4 KiB block, eight bits a byte
	perPeer := (dataBytes + blocksPerByte - 1) / blocksPerByte
	perPeer = (perPeer + 4095) &^ 4095 // the bitmap is written in 4 KiB pages
	return perPeer*int64(maxPeers) + 1<<20
}

// Resource is the DRBD half of the node's storage bring-up: the host-rendered .res file, and the
// two facts that decide what is done with it.
type Resource struct {
	// Name is the resource, e.g. "r0".
	Name string `json:"name"`
	// Device is the replicated block device the resource presents, e.g. /dev/drbd0 -- what the
	// .res names as `device` and what briard-primary-storage mounts. Carried so the mount unit
	// reads the device off the same document the block layer was built from ([B.145a]) rather
	// than restating a constant beside it.
	Device string `json:"device"`
	// Replicated says the resource is real: DRBD runs on this node, the .res is written, the
	// metadata is created and attached, and Device is the DRBD device. False is a LONE node
	// ([B.145]): a home with one diskful member runs btrfs on the data LV directly -- no DRBD,
	// no metadata, no promoter, since with no second copy DRBD only turns a bad block into a
	// dead node -- and Device names that LV. The host decides it from the mesh (two or more
	// DISKFUL members; a witness beside one anchor holds no copy), the guest carries it out at
	// bring-up and reads it back for everything that used to key on DRBD state. A joiner and a
	// witness are always replicated.
	Replicated bool `json:"replicated,omitempty"`
	// MaxPeers is the number of peer slots the metadata is created with (`create-md
	// --max-peers`). A product constant the host states, because it is baked into the metadata
	// and the .res at creation time names fewer peers than a flock will ever have: a mesh of one
	// would otherwise get ONE slot, and the second anchor's join would need new metadata.
	MaxPeers int `json:"maxPeers,omitempty"`
	// Config is the rendered drbd.d/<name>.res, straight from drbd.Resource.Config(). The host
	// renders it because the mesh is the host's knowledge -- peers, addresses, witness
	// topology -- and the guest has never composed one.
	Config string `json:"config"`
	// Diskless says this node is a witness: it holds no tier, creates no metadata, and its
	// storage bring-up is the .res and the attach alone. A diskless spec carries no tiers, and
	// Validate insists on that rather than tolerating tiers nothing will build.
	Diskless bool `json:"diskless,omitempty"`
	// FreshInit is the host's half of the first-init decision: THIS node is the designated seed
	// of a NEW flock, so once its metadata is created the resource may be declared UpToDate
	// (skip the initial sync) and the volume formatted once. A joiner is hard-wired false.
	//
	// It is only half, and the other half is what retires /run/briard/data.fresh ([V3b.33](c)):
	// "did we just create this LV" was an inference from `drbdadm create-md`'s exit code, an
	// inference ENCRYPTION BROKE -- dm-crypt returns ciphertext for sectors nobody has written,
	// so create-md's blank-probe found "some data" on every fresh encrypted node. One program
	// now runs both `lvcreate` and `create-md`, so it knows in-process and no marker has to
	// carry the fact between two components.
	FreshInit bool `json:"freshInit,omitempty"`
}

// Spec is the whole document: every tier this node builds, and the resource it brings up on top.
type Spec struct {
	Tiers    []Tier   `json:"tiers"`
	Resource Resource `json:"resource"`
}

// Tier returns the named tier. The data tier is the one every caller wants today; the lookup
// exists so the second tier does not turn every consumer into an index.
func (s Spec) Tier(name string) (Tier, bool) {
	for _, t := range s.Tiers {
		if t.Name == name {
			return t, true
		}
	}
	return Tier{}, false
}

// Parse decodes and validates a spec. Unknown fields are REFUSED, for the same reason
// shared/routes and shared/manifest refuse them: the host that writes this and the pushed agent
// that reads it ship together ([B.139]), so a field this build does not understand means the two
// have drifted -- and the operation on the other side of this document is destructive. Building
// storage from a document you only partly understand is the one failure mode worth refusing to
// boot over.
func Parse(raw []byte) (Spec, error) {
	var s Spec
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return Spec{}, fmt.Errorf("node storage: %w", err)
	}
	if err := s.Validate(); err != nil {
		return Spec{}, err
	}
	return s, nil
}

// Marshal renders the spec for the file. Indented, because the first thing anyone debugging a
// node that will not come up does is `cat` it.
func (s Spec) Marshal() ([]byte, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("node storage: %w", err)
	}
	return append(b, '\n'), nil
}

// Validate enforces the rules that are not style. Every one of them is a way this document could
// name a device that does not exist or a layout nobody decided on -- and the unit that reads it
// runs luksFormat and lvcreate, so a spec that is merely PLAUSIBLE is not good enough.
func (s Spec) Validate() error {
	if s.Resource.Name == "" {
		return fmt.Errorf("node storage: the spec names no resource")
	}
	if s.Resource.Replicated && s.Resource.Config == "" {
		return fmt.Errorf("node storage: resource %s carries no .res config", s.Resource.Name)
	}
	if !strings.HasPrefix(s.Resource.Device, "/dev/") {
		return fmt.Errorf("node storage: resource %s: %q is not a device path", s.Resource.Name, s.Resource.Device)
	}
	if s.Resource.Diskless {
		// A witness exists to arbitrate between two copies. Alone, it would be a node that
		// builds nothing and attaches nothing -- a contradiction the host's mesh rule should
		// never write, refused here for a spec written by hand.
		if !s.Resource.Replicated {
			return fmt.Errorf("node storage: a diskless node in a home that runs no DRBD has nothing to do")
		}
		// A witness with tiers is a spec built by something that thinks it is diskful. Refuse
		// rather than ignore: silently skipping them would hide the disagreement until someone
		// wondered why a witness had no volume.
		if len(s.Tiers) > 0 {
			return fmt.Errorf("node storage: a diskless node carries %d tier(s); a witness builds none", len(s.Tiers))
		}
		if s.Resource.FreshInit {
			return fmt.Errorf("node storage: a diskless witness cannot seed a flock (FreshInit on %s)", s.Resource.Name)
		}
		return nil
	}
	if len(s.Tiers) == 0 {
		return fmt.Errorf("node storage: a diskful node with no tiers has nothing for %s to attach", s.Resource.Name)
	}
	if s.Resource.MaxPeers < 1 {
		return fmt.Errorf("node storage: resource %s: maxPeers %d -- metadata needs at least one peer slot, and the number is baked in at create-md", s.Resource.Name, s.Resource.MaxPeers)
	}
	seen := make(map[string]bool, len(s.Tiers))
	for _, t := range s.Tiers {
		if t.Name == "" {
			return fmt.Errorf("node storage: a tier has no name")
		}
		if seen[t.Name] {
			return fmt.Errorf("node storage: two tiers are both called %q", t.Name)
		}
		seen[t.Name] = true
		if !strings.HasPrefix(t.Device, "/dev/") {
			return fmt.Errorf("node storage: tier %s: %q is not a device path", t.Name, t.Device)
		}
		if err := lvmName(t.Name, "vg", t.VG); err != nil {
			return err
		}
		if err := lvmName(t.Name, "lv", t.LV); err != nil {
			return err
		}
		if err := lvmName(t.Name, "metaLV", t.MetaLV); err != nil {
			return err
		}
		if t.MetaLV == t.LV {
			return fmt.Errorf("node storage: tier %s: the data and metadata LVs are both called %q", t.Name, t.LV)
		}
		if !t.Mode.Valid() {
			return fmt.Errorf("node storage: tier %s: %q is not an encryption mode", t.Name, t.Mode)
		}
	}
	// ★ A LONE NODE MOUNTS THE DATA LV, AND THE DOCUMENT MUST SAY SO IN ONE PLACE. Replicated
	// and Device are two fields describing one fact, so they are held to agree here rather than
	// left for the mount to discover: "alone" with /dev/drbd0 would mount a device nothing
	// attached, and "replicated" with the LV would mount underneath a resource a peer is
	// writing to.
	if !s.Resource.Replicated {
		data, ok := s.Tier(TierData)
		if !ok {
			return fmt.Errorf("node storage: a node that runs no DRBD mounts its %s tier directly, and this spec has none", TierData)
		}
		if s.Resource.Device != data.Mapper() {
			return fmt.Errorf("node storage: resource %s is not replicated, so its device must be the %s LV %s, not %s", s.Resource.Name, TierData, data.Mapper(), s.Resource.Device)
		}
	}
	return nil
}

// lvmName refuses the two characters that would make Mapper lie: a dash, which device-mapper
// escapes by doubling, and a slash, which would leave the path naming a directory that is not
// there. Empty is refused for the obvious reason -- `/dev/mapper/-data` is nobody's device.
func lvmName(tier, what, name string) error {
	switch {
	case name == "":
		return fmt.Errorf("node storage: tier %s has no %s name", tier, what)
	case strings.ContainsAny(name, "-/"):
		return fmt.Errorf("node storage: tier %s: %s name %q contains - or /, which device-mapper escapes -- %s would not name the device it built", tier, what, name, "/dev/mapper/<vg>-<lv>")
	}
	return nil
}
