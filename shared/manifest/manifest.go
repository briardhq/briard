// Package manifest is the Briard app manifest: the signed, published description of a
// catalogued app, and the thing `briard app install` acts on.
//
// THREE PROPERTIES, none of them incidental:
//
//  1. It is runtime-NEUTRAL. The layering is manifest (published, signed) -> a renderer on the
//     node (podman/quadlet-specific, swappable) -> systemd units. The manifest names WHAT an
//     app is, never how one podman version spells it — the same reasoning that named
//     `reverse-proxy` for the role rather than the mechanism.
//
//  2. It is a SECURITY BOUNDARY. Upstream compose can ask for `privileged`, arbitrary host binds,
//     the docker socket. This schema cannot express any of them, and the node is designed to
//     execute what it is given without judgement — so capability comes by omission, and what we do
//     not model is not merely refused but UNSAYABLE. The discipline this demands: model only what
//     catalogued apps need, expand deliberately, or it becomes a compose reimplementation.
//
//     Host networking is the one capability that IS sayable, and modelling it is what made this
//     property true rather than aspirational: it was named here as unsayable while the renderer
//     granted it to every app unconditionally, so the doctrine described a schema the code did
//     not implement. `Network` now carries it, silence means private (omission yields the LESS
//     capable shape, which is the property working as stated), and asking for host is a deliberate
//     act recorded in the identity hash.
//
//     ⚠️ THE TEST FOR THE NEXT FIELD, because the risk here is not one bad field
//     but a plausible fourth, fifth and sixth: a field earns its place only if a CATALOGUED app
//     cannot be operated correctly without it, AND the alternative is worse than the field. Host
//     networking passes both — Home Assistant cannot find devices without it, and the alternative
//     was granting it to everyone forever. "Some app might want it", "compose has it", and
//     "it would be more flexible" are not arguments; the answer to those is a curated catalog.
//
//     Validate() is the second half of that boundary. A renderer writes INI, so every string
//     that reaches a unit file is checked for newlines here: without that, a single manifest
//     field could carry "\nPodmanArgs=--privileged" and re-open by injection precisely what the
//     schema closed by omission. Names are checked as slugs for the same reason — they become
//     path elements and unit names, where "../" is arbitrary file write as root.
//
//  3. Its CONTENT HASH IS THE APP IDENTITY, replacing the single OCI digest that
//     .payload-image held. That transitively pins the whole container set, preserves the
//     content-addressed / node-independent property, and drops into the existing
//     upgrade path unchanged: a new manifest is a new identity -> announce -> snapshot ->
//     switch -> health-gate -> roll back to the previous manifest, already on the volume.
package manifest

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"
)

// slug is what may become a systemd unit name or a path element: lowercase alphanumeric with
// internal dashes. Deliberately narrow — this is the check that keeps a catalog entry from
// naming "../../etc/systemd/system/anything" or a unit that collides with ours.
var slug = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$`)

// ValidName reports whether name has the shape of a catalog name, for a reader that receives
// one from outside the catalog and must not hold anything else.
func ValidName(name string) bool { return slug.MatchString(name) }

// envKey mirrors the shell/systemd convention. Values are free-form apart from the newline ban.
var envKey = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// digestRef matches a fully digest-pinned OCI reference (repo@sha256:<64 hex>). A tag is NOT
// accepted: a tag is mutable, so a tagged manifest would make the app identity a promise
// someone else can silently change — which is the one thing the content-hash identity exists to
// prevent.
//
// The optional `:<port>` after the first component is load-bearing, not decoration: a registry on
// a non-default port (`registry.example.com:5000/foo@sha256:…`) is an ordinary deployment, and an
// earlier version of this pattern had no `:` at all and silently refused every one of them. The
// integration test caught it by running exactly that shape.
var digestRef = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*(:[0-9]{1,5})?(/[a-zA-Z0-9._-]+)*@sha256:[a-f0-9]{64}$`)

// ErrInvalid wraps every schema violation, so a caller can refuse a whole manifest on one check.
var ErrInvalid = errors.New("manifest: invalid")

// Manifest describes one installable app. An app is one or more containers sharing a
// network namespace (a pod), and what they keep lives in the STORAGE TIERS: one area per tier the
// app mounts, each a subvolume of its own, with the directories its containers mount as plain
// subdirectories inside it. Plain, and a requirement rather than a preference: `data.restore`
// runs `btrfs subvolume delete`, which REFUSES on a subvolume that contains nested subvolumes, so
// a mounted directory cannot itself be a subvolume.
type Manifest struct {
	// Name is the app slug — its unit prefix, its subvolume name, and the handle a user
	// types. Stable across versions; changing it is a different app, not an upgrade.
	Name string `json:"name"`
	// Version is the upstream version this entry pins, for humans and status output only.
	// Identity is the content hash, never this string.
	Version string `json:"version"`
	// Containers is the pod, in the order they should be declared. Exactly one must be marked
	// Primary (see Container.Primary).
	Containers []Container `json:"containers"`
	// Size is what a node DOWNLOADS to install this app: the compressed layers of every
	// container's image, summed -- the number a progress bar is a fraction of.
	// InstalledSize is what the image store HOLDS once pulled: the same layers uncompressed --
	// the number a free-space check is against, because compressed bytes understate disk by
	// two to four times (Home Assistant 2026.7.1: 622 MB down, 2.49 GB on disk). Two scalars
	// rather than a layer table: the digest above already commits to every layer, and podman's
	// own store knows each finished layer's size; the totals are the only numbers a node cannot
	// learn until the pull is over. Both or neither; zero means the entry predates the fields
	// (no bar, no gate).
	Size          int64 `json:"size,omitempty"`
	InstalledSize int64 `json:"installedSize,omitempty"`
	// MinMemoryMB is the memory the app needs to run, in MB: its settled footprint with room
	// for its own startup, not its worst case. The node boots its guest with at least the system's
	// share plus every installed app's minimum, and grows the guest to it before installing
	// one more -- anything above it is found at runtime, by growth. Zero means the entry does not
	// say (the app counts for nothing until it needs more).
	MinMemoryMB int `json:"minMemoryMB,omitempty"`
	// Network is the pod's networking mode: "host" or "private". SILENCE MEANS PRIVATE, which is
	// property 2 in the small: omission yields the LESS capable shape, so a manifest that says
	// nothing about networking gets a pod that can reach nothing but itself and whatever the node
	// deliberately routes or publishes to it.
	//
	// IT IS A NEUTRAL WORD, NEVER A QUADLET DIRECTIVE — "host", not "Network=host". Property 1 is
	// the reason: this document names WHAT an app is, never how one podman version spells it, so
	// that the renderer stays swappable. A raw directive would weld a permanently-identified signed
	// artifact to podman's config syntax, and because the content hash is the identity you could
	// not quietly fix the spelling later — you would re-mint every entry in the catalog. `Mounts`
	// already draw this line correctly: they say `/config`, not `Volume=…:/config`.
	//
	// HOST NETWORKING IS FOR THE DISCOVERY-DEPENDENT FEW. Home Assistant needs mDNS/SSDP to find
	// devices on the household LAN, and the HASS periphery skews the same way (ESPHome, Music
	// Assistant). A broker, a media server or a photo app wants a private pod and to be reached
	// through the reverse proxy or a published port — which is why the default is the one that does
	// not hand a container the household's LAN.
	//
	// ⚠️ THE FIELD IS WHY THIS SCHEMA'S SECOND PROPERTY IS TRUE AGAIN. The package doc has always
	// named host networking as a capability the schema makes UNSAYABLE, while the renderer granted
	// it to every app unconditionally. Modelling it is what makes the doctrine describe the
	// code: the manifest's silence now MEANS something, and saying "host" is a deliberate,
	// reviewable act recorded in the identity hash.
	Network string `json:"network,omitempty"`
	// Ports are the container ports this app PUBLISHES to the household, each reachable at the
	// node's service address on the same number. A list rather than a host:container map: for every
	// app we can foresee the two are the same number, and a map would spend a second field on
	// expressing the identity function.
	//
	// THE CATALOG PICKS THEM, which makes this a node-level allocation decided in a published
	// document — safe because the catalog is curated and hand-checked for conflicts, which is the
	// same argument that refused a runtime port-collision pre-flight. Two entries
	// wanting one port is a review failure, not a runtime one.
	//
	// It is how an app that CANNOT be fronted reaches anyone. A reverse proxy can serve HTTP and
	// nothing else, so mosquitto's MQTT on 1883 has no route and never will; publishing is the
	// whole of its reachability, and the reverse proxy is not involved.
	//
	// ⚠️ MEANINGLESS UNDER HOST NETWORKING, and refused there rather than ignored: a host-networked
	// pod already holds the guest's ports, so there is nothing to publish, and podman rejects the
	// combination outright. A manifest that asked for both would describe an app no node could
	// run, which is exactly what Validate exists to catch before a household does.
	Ports []int `json:"ports,omitempty"`
}

// Container is one member of the app's pod.
type Container struct {
	// Name is the container's slug within the app, and its unit's.
	Name string `json:"name"`
	// Image is a digest-pinned OCI reference. Digest-pinned means trust is source-independent,
	// which is what lets the image come from upstream, from our mirror, or from a tarball
	// without changing what "this app" means.
	Image string `json:"image"`
	// Mounts are the only places the container may write that outlive it. None means the
	// container is stateless — the honest encoding of "this one keeps nothing", rather than
	// handing it a directory it will never write. There is no access to a tier as a whole: a
	// tier is what the node does with a directory, and the manifest names the directories.
	Mounts []Mount `json:"mounts,omitempty"`
	// Env is the container's environment. Values are opaque; only newlines are forbidden.
	Env map[string]string `json:"env,omitempty"`
	// Primary marks the container the reverse proxy forwards to and the health gate probes.
	// Exactly one container carries it, because "is the app serving?" must have one answer.
	Primary bool `json:"primary,omitempty"`
	// Port is the primary container's listening port, and HealthPath its readiness path. Both
	// are meaningful only on the primary; the reverse proxy's -backend/-backend-health are
	// built from them.
	Port       int    `json:"port,omitempty"`
	HealthPath string `json:"healthPath,omitempty"`
}

// Mount is one directory of the app's area in a tier, at Path inside the container. Two
// containers naming the same tier and Dir share the directory.
//
// A MOUNT IS A RENAME AND A RMDIR BOUNDARY: the container cannot move a file across it, nor
// remove the mount point itself. So a mount nested inside another goes only where the app's own
// code tolerates both, read in its upstream source for each entry.
type Mount struct {
	// Tier is what the node does with the directory: one of the Tier* values.
	Tier string `json:"tier"`
	// Dir is the directory's name in the app's area. In the files tier it is what the
	// household sees.
	Dir string `json:"dir"`
	// Path is where it appears inside the container: absolute and clean.
	Path string `json:"path"`
}

// THE STORAGE TIERS, named for what the node does with what they hold. A closed set, spelled here
// rather than at each end, so a catalog entry and a node cannot disagree about one.
const (
	// TierData is what the app keeps to run -- its settings, its database: snapshotted as its
	// History, backed up, rolled back with the app.
	TierData = "data"
	// TierFiles is what the household keeps in the app: versioned, backed up, never rolled back.
	TierFiles = "files"
	// TierCache is what the app can make again: neither snapshotted nor backed up, and emptied
	// whenever its data is rolled back -- a newer version's cache beside older data is how a
	// downgrade breaks an app.
	TierCache = "cache"
	// TierUnversioned is what is kept but not copied: neither snapshotted nor backed up, never
	// emptied -- an app's own backups, its exports.
	TierUnversioned = "unversioned"
)

// Tiers lists the storage tiers.
var Tiers = []string{TierData, TierFiles, TierCache, TierUnversioned}

// UsesTier reports whether any container of the app mounts the tier.
func (m Manifest) UsesTier(tier string) bool {
	return slices.ContainsFunc(m.Containers, func(c Container) bool { return c.UsesTier(tier) })
}

// UsesTier reports whether the container mounts the tier.
func (c Container) UsesTier(tier string) bool {
	return slices.ContainsFunc(c.Mounts, func(mt Mount) bool { return mt.Tier == tier })
}

// Identity is the manifest's content hash — the app identity (property 3 above). It is
// computed over the EXACT signed bytes, never a re-marshalling of the parsed struct, because
// re-encoding can reorder or reformat and would silently mint a different identity for the same
// signed document.
type Identity string

// Primary returns the container the reverse proxy forwards to. Validate guarantees exactly one.
func (m Manifest) Primary() Container {
	for _, c := range m.Containers {
		if c.Primary {
			return c
		}
	}
	return Container{}
}

// Parse decodes and validates a manifest, returning it with the identity of the bytes given.
// Unknown fields are REFUSED: a field this build does not understand may be one that grants a
// capability, and silently ignoring it would let a newer catalog widen what an older node runs
// without the node ever noticing.
func Parse(raw []byte) (Manifest, Identity, error) {
	var m Manifest
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return Manifest{}, "", fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if err := m.Validate(); err != nil {
		return Manifest{}, "", err
	}
	return m, identityOf(raw), nil
}

// identityOf is the Identity of exact bytes, before anything has parsed them.
func identityOf(raw []byte) Identity {
	sum := sha256.Sum256(raw)
	return Identity("sha256:" + hex.EncodeToString(sum[:]))
}

// Validate enforces the security boundary. Every rule here exists because the renderer writes
// INI and the paths become real paths; none of it is style.
func (m Manifest) Validate() error {
	if !slug.MatchString(m.Name) {
		return fmt.Errorf("%w: app name %q is not a slug", ErrInvalid, m.Name)
	}
	if err := safeValue("version", m.Version); err != nil {
		return err
	}
	switch m.Network {
	case "", NetworkPrivate, NetworkHost:
	default:
		// A CLOSED ENUM, refused rather than defaulted. Defaulting an unrecognised word to private
		// would be the safe-looking choice and the wrong one: a catalog entry that means to ask for
		// host networking and misspells it would install, run, and fail to find a single device,
		// with nothing anywhere saying why. Refusing names the typo at the door.
		return fmt.Errorf("%w: app %q network %q is not one of %q, %q (or absent, meaning private)",
			ErrInvalid, m.Name, m.Network, NetworkHost, NetworkPrivate)
	}
	seenPort := map[int]bool{}
	for _, p := range m.Ports {
		if p < 1 || p > 65535 {
			return fmt.Errorf("%w: app %q publishes port %d, out of range", ErrInvalid, m.Name, p)
		}
		if seenPort[p] {
			return fmt.Errorf("%w: app %q publishes port %d twice", ErrInvalid, m.Name, p)
		}
		seenPort[p] = true
	}
	if len(m.Ports) > 0 && m.HostNetwork() {
		// Not a style rule: podman refuses port bindings on a host-networked pod, so this manifest
		// describes an app no node could start. Caught here rather than at the household.
		return fmt.Errorf("%w: app %q asks for host networking and publishes ports; a host-networked pod already holds the guest's ports", ErrInvalid, m.Name)
	}
	if len(m.Containers) == 0 {
		return fmt.Errorf("%w: app %q declares no containers", ErrInvalid, m.Name)
	}
	if m.MinMemoryMB < 0 {
		return fmt.Errorf("%w: app %q has a negative minMemoryMB", ErrInvalid, m.Name)
	}
	if m.Size < 0 || m.InstalledSize < 0 {
		return fmt.Errorf("%w: app %q has a negative size", ErrInvalid, m.Name)
	}
	if (m.Size == 0) != (m.InstalledSize == 0) {
		// Both or neither: a bar with no gate, or a gate with no bar, is an entry half-measured.
		return fmt.Errorf("%w: app %q declares size and installedSize together or not at all", ErrInvalid, m.Name)
	}
	if m.InstalledSize < m.Size {
		return fmt.Errorf("%w: app %q is smaller installed (%d) than downloaded (%d); the two are swapped", ErrInvalid, m.Name, m.InstalledSize, m.Size)
	}
	seen := make(map[string]bool, len(m.Containers))
	primaries := 0
	for _, c := range m.Containers {
		if !slug.MatchString(c.Name) {
			return fmt.Errorf("%w: container name %q is not a slug", ErrInvalid, c.Name)
		}
		if seen[c.Name] {
			// Duplicate names would collide on both the unit name and the data subdirectory,
			// so two containers would silently share one directory.
			return fmt.Errorf("%w: duplicate container name %q", ErrInvalid, c.Name)
		}
		seen[c.Name] = true
		if !digestRef.MatchString(c.Image) {
			return fmt.Errorf("%w: container %q image %q is not digest-pinned (want repo@sha256:...)", ErrInvalid, c.Name, c.Image)
		}
		// `..` is a legal-looking path component to the pattern above (dots and dashes are allowed
		// inside a component) but is not one the OCI grammar permits, and a reference carrying it
		// is either malformed or trying something. Same refusal as mounts and health paths.
		if strings.Contains(c.Image, "..") {
			return fmt.Errorf("%w: container %q image %q contains %q", ErrInvalid, c.Name, c.Image, "..")
		}
		paths := map[string]bool{}
		for _, mt := range c.Mounts {
			if !slices.Contains(Tiers, mt.Tier) {
				// A closed enum, refused rather than defaulted, as Network is: a misspelt tier
				// read as data would roll a household's files back with the app.
				return fmt.Errorf("%w: container %q mounts tier %q, not one of %q", ErrInvalid, c.Name, mt.Tier, Tiers)
			}
			if !slug.MatchString(mt.Dir) {
				return fmt.Errorf("%w: container %q mount dir %q is not one plain name", ErrInvalid, c.Name, mt.Dir)
			}
			if !path.IsAbs(mt.Path) || path.Clean(mt.Path) != mt.Path || strings.Contains(mt.Path, "..") {
				return fmt.Errorf("%w: container %q mount %q is not a clean absolute path", ErrInvalid, c.Name, mt.Path)
			}
			if err := safeValue("mount", mt.Path); err != nil {
				return err
			}
			if paths[mt.Path] {
				return fmt.Errorf("%w: container %q mounts two directories at %q", ErrInvalid, c.Name, mt.Path)
			}
			paths[mt.Path] = true
		}
		for k, v := range c.Env {
			if !envKey.MatchString(k) {
				return fmt.Errorf("%w: container %q env key %q is not an identifier", ErrInvalid, c.Name, k)
			}
			if err := safeValue("env "+k, v); err != nil {
				return err
			}
		}
		if c.Primary {
			primaries++
			if c.Port < 1 || c.Port > 65535 {
				return fmt.Errorf("%w: primary container %q port %d out of range", ErrInvalid, c.Name, c.Port)
			}
			if !strings.HasPrefix(c.HealthPath, "/") || strings.Contains(c.HealthPath, "..") {
				return fmt.Errorf("%w: primary container %q healthPath %q must be an absolute URL path", ErrInvalid, c.Name, c.HealthPath)
			}
			if err := safeValue("healthPath", c.HealthPath); err != nil {
				return err
			}
		}
	}
	if primaries != 1 {
		// Not a style rule: the reverse proxy has one backend and the health gate one verdict, so
		// zero primaries leaves both undefined and two makes them ambiguous.
		return fmt.Errorf("%w: app %q must mark exactly one primary container, found %d", ErrInvalid, m.Name, primaries)
	}
	return nil
}

// safeValue rejects anything that could break out of a rendered INI line. A newline is the
// injection: "\nPodmanArgs=--privileged" in any rendered field would grant, by smuggling, the
// exact capability the schema withholds by omission. NUL and carriage return go too — both are
// line-terminator-adjacent and neither has a legitimate use in these fields.
func safeValue(what, v string) error {
	if strings.ContainsAny(v, "\n\r\x00") {
		return fmt.Errorf("%w: %s contains a line break or NUL", ErrInvalid, what)
	}
	return nil
}

// The values Manifest.Network takes. A closed set, spelled here rather than at each end, so a
// catalog entry and a node cannot disagree about what a manifest meant.
//
// NetworkPrivate is also what an ABSENT field means, and both spellings are accepted: the explicit
// word is clearer in a catalog entry a human reviews, while silence keeps every manifest written
// before the field existed valid and private.
const (
	NetworkHost    = "host"
	NetworkPrivate = "private"
)

// HostNetwork reports whether this app's pod shares the guest's network namespace. The one
// question the renderer asks, so that "" and "private" cannot be handled differently by accident.
func (m Manifest) HostNetwork() bool { return m.Network == NetworkHost }
