// Package manifest is the Briard service manifest: the signed, published description of a
// catalogued service, and the thing `briard service install` acts on.
//
// THREE PROPERTIES, none of them incidental:
//
//  1. It is runtime-NEUTRAL. The layering is manifest (published, signed) -> a renderer on the
//     node (podman/quadlet-specific, swappable) -> systemd units. The manifest names WHAT a
//     service is, never how one podman version spells it — the same reasoning that named
//     `reverse-proxy` for the role rather than the mechanism.
//
//  2. It is a SECURITY BOUNDARY. Upstream compose can ask for `privileged`, arbitrary host binds,
//     the docker socket. This schema cannot express any of them, and the node is designed to
//     execute what it is given without judgement — so capability comes by omission, and what we do
//     not model is not merely refused but UNSAYABLE. The discipline this demands: model only what
//     catalogued services need, expand deliberately, or it becomes a compose reimplementation.
//
//     Host networking is the one capability that IS sayable, and modelling it is what made this
//     property true rather than aspirational: it was named here as unsayable while the renderer
//     granted it to every service unconditionally, so the doctrine described a schema the code did
//     not implement. `Network` now carries it, silence means private (omission yields the LESS
//     capable shape, which is the property working as stated), and asking for host is a deliberate
//     act recorded in the identity hash.
//
//     ⚠️ THE TEST FOR THE NEXT FIELD, owed by [B.48](a) because the risk here is not one bad field
//     but a plausible fourth, fifth and sixth: a field earns its place only if a CATALOGUED service
//     cannot be operated correctly without it, AND the alternative is worse than the field. Host
//     networking passes both — Home Assistant cannot find devices without it, and the alternative
//     was granting it to everyone forever. "Some service might want it", "compose has it", and
//     "it would be more flexible" are not arguments; the answer to those is a curated catalog.
//
//     Validate() is the second half of that boundary. A renderer writes INI, so every string
//     that reaches a unit file is checked for newlines here: without that, a single manifest
//     field could carry "\nPodmanArgs=--privileged" and re-open by injection precisely what the
//     schema closed by omission. Names are checked as slugs for the same reason — they become
//     path elements and unit names, where "../" is arbitrary file write as root.
//
//  3. Its CONTENT HASH IS THE SERVICE IDENTITY, replacing the single OCI digest that
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
	"strings"
)

// slug is what may become a systemd unit name or a path element: lowercase alphanumeric with
// internal dashes. Deliberately narrow — this is the check that keeps a catalog entry from
// naming "../../etc/systemd/system/anything" or a unit that collides with ours.
var slug = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$`)

// envKey mirrors the shell/systemd convention. Values are free-form apart from the newline ban.
var envKey = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// digestRef matches a fully digest-pinned OCI reference (repo@sha256:<64 hex>). A tag is NOT
// accepted: a tag is mutable, so a tagged manifest would make the service identity a promise
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

// Manifest describes one installable service. A service is one or more containers sharing a
// network namespace (a pod) and one data subvolume, with per-container plain subdirectories
// inside it — the layer-1 shape, and a requirement rather than a preference:
// `data.restore` runs `btrfs subvolume delete`, which REFUSES on a subvolume that contains
// nested subvolumes, so per-container storage cannot itself be a subvolume.
//
// Layer 2 (service user areas — Immich originals, Jellyfin libraries) is v5's, so
// there is deliberately no field for it here: the catalog starts at entries with no user area.
type Manifest struct {
	// Name is the service slug — its unit prefix, its subvolume name, and the handle a user
	// types. Stable across versions; changing it is a different service, not an upgrade.
	Name string `json:"name"`
	// Version is the upstream version this entry pins, for humans and status output only.
	// Identity is the content hash, never this string.
	Version string `json:"version"`
	// Containers is the pod, in the order they should be declared. Exactly one must be marked
	// Primary (see Container.Primary).
	Containers []Container `json:"containers"`
	// Network is the pod's networking mode: "host" or "private". SILENCE MEANS PRIVATE, which is
	// property 2 in the small: omission yields the LESS capable shape, so a manifest that says
	// nothing about networking gets a pod that can reach nothing but itself and whatever the node
	// deliberately routes or publishes to it.
	//
	// IT IS A NEUTRAL WORD, NEVER A QUADLET DIRECTIVE — "host", not "Network=host". Property 1 is
	// the reason: this document names WHAT a service is, never how one podman version spells it, so
	// that the renderer stays swappable. A raw directive would weld a permanently-identified signed
	// artifact to podman's config syntax, and because the content hash is the identity you could
	// not quietly fix the spelling later — you would re-mint every entry in the catalog. `Mount`
	// already draws this line correctly: it says `/config`, not `Volume=…:/config`.
	//
	// HOST NETWORKING IS FOR THE DISCOVERY-DEPENDENT FEW. Home Assistant needs mDNS/SSDP to find
	// devices on the household LAN, and the HASS periphery skews the same way (ESPHome, Music
	// Assistant). A broker, a media server or a photo app wants a private pod and to be reached
	// through the front door or a published port — which is why the default is the one that does
	// not hand a container the household's LAN.
	//
	// ⚠️ THE FIELD IS WHY THIS SCHEMA'S SECOND PROPERTY IS TRUE AGAIN. The package doc has always
	// named host networking as a capability the schema makes UNSAYABLE, while the renderer granted
	// it to every service unconditionally. Modelling it is what makes the doctrine describe the
	// code: the manifest's silence now MEANS something, and saying "host" is a deliberate,
	// reviewable act recorded in the identity hash.
	Network string `json:"network,omitempty"`
}

// Container is one member of the service's pod.
type Container struct {
	// Name is the container's slug within the service; it is also the name of its plain
	// subdirectory inside the service's single data subvolume.
	Name string `json:"name"`
	// Image is a digest-pinned OCI reference. Digest-pinned means trust is source-independent,
	// which is what lets the image come from upstream, from our mirror, or from a tarball
	// without changing what "this service" means.
	Image string `json:"image"`
	// Mount is the absolute path INSIDE the container where its data subdirectory appears.
	// Empty means the container is stateless and gets no mount at all — the honest encoding of
	// "this one keeps nothing", rather than handing it a directory it will never write.
	Mount string `json:"mount,omitempty"`
	// Env is the container's environment. Values are opaque; only newlines are forbidden.
	Env map[string]string `json:"env,omitempty"`
	// Primary marks the container the front door forwards to and the health gate probes.
	// Exactly one container carries it, because "is the service serving?" must have one answer.
	Primary bool `json:"primary,omitempty"`
	// Port is the primary container's listening port, and HealthPath its readiness path. Both
	// are meaningful only on the primary; the reverse proxy's -backend/-backend-health are
	// built from them.
	Port       int    `json:"port,omitempty"`
	HealthPath string `json:"healthPath,omitempty"`
}

// Identity is the manifest's content hash — the service identity (property 3 above). It is
// computed over the EXACT signed bytes, never a re-marshalling of the parsed struct, because
// re-encoding can reorder or reformat and would silently mint a different identity for the same
// signed document.
type Identity string

// Primary returns the container the front door forwards to. Validate guarantees exactly one.
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
	sum := sha256.Sum256(raw)
	return m, Identity("sha256:" + hex.EncodeToString(sum[:])), nil
}

// Validate enforces the security boundary. Every rule here exists because the renderer writes
// INI and the paths become real paths; none of it is style.
func (m Manifest) Validate() error {
	if !slug.MatchString(m.Name) {
		return fmt.Errorf("%w: service name %q is not a slug", ErrInvalid, m.Name)
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
		return fmt.Errorf("%w: service %q network %q is not one of %q, %q (or absent, meaning private)",
			ErrInvalid, m.Name, m.Network, NetworkHost, NetworkPrivate)
	}
	if len(m.Containers) == 0 {
		return fmt.Errorf("%w: service %q declares no containers", ErrInvalid, m.Name)
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
		if c.Mount != "" {
			if !path.IsAbs(c.Mount) || path.Clean(c.Mount) != c.Mount || strings.Contains(c.Mount, "..") {
				return fmt.Errorf("%w: container %q mount %q is not a clean absolute path", ErrInvalid, c.Name, c.Mount)
			}
			if err := safeValue("mount", c.Mount); err != nil {
				return err
			}
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
		// Not a style rule: the front door has one backend and the health gate one verdict, so
		// zero primaries leaves both undefined and two makes them ambiguous.
		return fmt.Errorf("%w: service %q must mark exactly one primary container, found %d", ErrInvalid, m.Name, primaries)
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

// HostNetwork reports whether this service's pod shares the guest's network namespace. The one
// question the renderer asks, so that "" and "private" cannot be handled differently by accident.
func (m Manifest) HostNetwork() bool { return m.Network == NetworkHost }
