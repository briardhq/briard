package guestagent

import (
	"context"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"

	"briard.io/agent/quadlet"
	"briard.io/shared/manifest"
)

// Converge-at-promotion: a node that takes over makes itself match the VOLUME ([V3b.3](f)).
//
// THE PROBLEM IT SOLVES, measured on a real fleet 2026-08-27: an install renders and chains on
// the node that ran it, and a peer that was Secondary at the time (or absent, or joined later)
// promoted onto whatever it had last rendered — nothing. The survivor served the zero-service
// landing page and reported healthy, because the front door answers its own /healthz. So what a
// node was TOLD decided what the household got after a failover.
//
// THE SHAPE, and the chicken-and-egg it has to solve. The promoter chain is what drbd-reactor
// promotes WITH, but the volume is only readable AFTER promotion. So the chain is STATIC —
// `briard-data -> briard-services -> briard-vip` on every data node — and `briard-services` is
// the unit that, once the mount exists, reads the manifests, renders, warms and starts them.
// A constant chain is what made converge-at-promotion possible for the baked payload slot;
// generalising it is what makes it possible for N runtime-installed services.
//
// The manifests are TRUSTED here, and the signature is not re-checked. Identity is the host's:
// it fetched the manifest from the catalog, verified it against the release keyring, and wrote
// those exact bytes to a volume only a Primary can write. What the guest re-derives is the
// RENDERING, deliberately — the renderer belongs next to the podman it renders for. Parse still
// validates the structure, so a corrupt manifest is refused rather than rendered.
//
// ⚠️ This is the natural home for [B.116]'s code↔data identity gate; whether it gates per service
// or for the whole node is [B.116]'s question, and nothing here answers it yet.

// unitsFile records the ordered units converge started, so ConvergeStop can stop exactly those.
// /run (tmpfs), re-derived by every converge, therefore immune to the durable-write rule.
//
// Recording them beats re-deriving them at stop time: ExecStop runs while the volume is still
// mounted TODAY (drbd-reactor unwinds the chain in reverse, so briard-data goes last), but a stop
// that depends on reading the volume would fail exactly when the volume is what went wrong.
const unitsFile = "/run/briard/services.units"

// unitPrefix namespaces everything the renderer writes into quadletDir. Converge owns the files
// carrying it and no others: a file a user put there is theirs, and must survive.
const unitPrefix = "briard-"

// Converge makes this node run what the replicated volume says it should.
//
// Render -> write units -> reload -> warm images -> start. Idempotent: re-running it against an
// unchanged volume rewrites identical bytes and starts already-active units, which is what lets
// an install re-converge in place instead of restarting this unit (restarting a chain member
// would deactivate the promoter's target and demote the node).
//
// THE FAILURE RULE, and it is the whole reason this is split the way it is ([V3b.3](f)):
//
//   - CONVERGE's own failure — the volume unreadable, a manifest that will not render, units that
//     cannot be written, an image that is absent and cannot be fetched — returns an error. This
//     runs as a promoter chain member ahead of briard-vip, so the promotion fails, the node takes
//     no address, and snapshot() already reports a primary with no address as unhealthy. Loud,
//     through machinery that already exists.
//
//   - A SERVICE's own failure to start is logged and survived. A container that crashes must not
//     demote: a code fault is deterministic, so the peer running the identical closure hits it
//     identically and the failover only flaps — and one broken service must not take the other
//     N-1 down with it. Recovery is the unit's own Restart= (agent/quadlet), and the household
//     hears about it through per-service health, which reports and never gates.
//
// A node with nothing installed converges successfully to nothing. That is the shipped state, not
// an error.
func Converge(ctx context.Context, x Executor) error {
	rendered, units, err := renderVolume(ctx, x)
	if err != nil {
		return err
	}
	if err := writeUnits(ctx, x, rendered); err != nil {
		return err
	}
	// Warm before starting, not during: the containers are Pull=never, so an image that is not
	// resident fails the container rather than fetching it. Absence here means the design was not
	// upheld (install warms, prewarm puts the image on every standby) and a pull is the honest
	// recovery — the digest in the manifest pins the bytes, so a fetch returns exactly those or
	// fails. A pull that CANNOT happen fails converge, which takes the VIP down and says so,
	// rather than promoting into a half-started chain ([V3.17]'s doctrine, upheld by failing).
	warm := make([]string, 0, len(rendered.ImageRefs))
	for u := range rendered.ImageRefs {
		warm = append(warm, u)
	}
	sort.Strings(warm) // the images are independent; the order is for a log a human reads
	for _, u := range warm {
		if err := warmImage(ctx, x, u, rendered.ImageRefs[u]); err != nil {
			return fmt.Errorf("converge: %w", err)
		}
	}
	// Record BEFORE starting: a start that fails part-way still leaves units running, and a stop
	// that does not know about them would leave containers on an unmounted volume.
	if err := x.WriteFile(unitsFile, []byte(strings.Join(units, "\n")+"\n")); err != nil {
		return fmt.Errorf("converge: record started units: %w", err)
	}
	for _, u := range units {
		if _, err := x.Run(ctx, "systemctl", "start", u); err != nil {
			// Alert, do not demote. See the failure rule above.
			log.Printf("converge: %s did not start (%v); the node is serving what it can", u, err)
		}
	}
	if len(units) == 0 {
		log.Printf("converge: no services installed on the volume; nothing to start")
	} else {
		log.Printf("converge: %d unit(s) started from the volume: %s", len(units), strings.Join(units, " "))
	}
	return nil
}

// ConvergeStop stops the service units this node started, newest-dependency first. Run as
// briard-services' ExecStop, i.e. by drbd-reactor unwinding the chain on demote — the service
// units are not chain members, so nothing else would stop them and the containers would be left
// running on a volume about to be unmounted.
//
// Best-effort by design: a unit that is already gone, or that will not stop, must not fail the
// demote. A failed ExecStop would leave briard-services active, which blocks the unmount behind
// it — strictly worse than a container that outlives its data.
func ConvergeStop(ctx context.Context, x Executor) error {
	raw, err := x.ReadFile(unitsFile)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // never converged (or already stopped): nothing to stop
		}
		return fmt.Errorf("converge stop: read started units: %w", err)
	}
	units := nonEmptyLines(raw)
	// Reverse: containers before their pod, exactly the order the promoter used to unwind in.
	for i := len(units) - 1; i >= 0; i-- {
		if _, err := x.Run(ctx, "systemctl", "stop", units[i]); err != nil {
			log.Printf("converge stop: %s: %v (continuing)", units[i], err)
		}
	}
	return nil
}

// renderVolume reads every manifest on the volume and renders them, returning the merged file set
// and the ordered units to start.
//
// ORDER IS BY SERVICE NAME, and it owes nothing more than determinism today. A set of services
// has a real dependency order, which [V3b.3](c) settles once there is a second real service to
// decide against; until then alphabetical is the cheapest way to be the same on every node.
//
// An ABSENT manifest directory is the shipped zero-service node, not a failure. A manifest that
// is present and unusable IS a failure, and the difference is deliberate: absence is a state we
// ship, while a corrupt manifest means this node cannot honour what the volume says it runs, and
// promoting anyway is how a household silently loses a service.
func renderVolume(ctx context.Context, x Executor) (quadlet.Rendered, []string, error) {
	all := quadlet.Rendered{Files: map[string]string{}}
	names, err := manifestNames(ctx, x)
	if err != nil {
		return all, nil, err
	}
	var units []string
	for _, n := range names {
		raw, err := x.ReadFile(manifestDir + "/" + n)
		if err != nil {
			return all, nil, fmt.Errorf("converge: read %s: %w", n, err)
		}
		m, _, err := manifest.Parse(raw)
		if err != nil {
			return all, nil, fmt.Errorf("converge: %s does not parse: %w", n, err)
		}
		r, err := quadlet.Render(m)
		if err != nil {
			return all, nil, fmt.Errorf("converge: %s does not render: %w", n, err)
		}
		mergeInto(&all, r)
		units = append(units, r.Units...)
	}
	return all, units, nil
}

// manifestNames lists the volume's manifest files, sorted. Shelling out to `ls` rather than
// widening Executor with a directory read: the same shape gatherResources already uses, and it
// keeps the fake that drives the tests to four methods.
func manifestNames(ctx context.Context, x Executor) ([]string, error) {
	out, err := x.Run(ctx, "ls", "-1", manifestDir)
	if err != nil {
		// Absent directory = nothing installed = the shipped state. `ls` cannot distinguish that
		// from a genuinely broken read, and treating both as empty is the right trade: the
		// alternative is a node that refuses to promote because a directory it never had is
		// missing.
		return nil, nil
	}
	var names []string
	for _, n := range nonEmptyLines(out) {
		if strings.HasSuffix(n, ".json") {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	return names, nil
}

// writeUnits materialises the rendered set into quadletDir and reloads systemd, removing any
// briard- unit source that this converge did NOT render.
//
// The removal is what makes converge the whole truth rather than an addition to it: a service
// uninstalled on another node, or renamed, leaves orphan unit source behind, and an orphan
// .container is a unit a future converge could start against data that no longer exists. Only
// files carrying the renderer's own prefix are candidates — a quadlet file a user dropped there
// is theirs.
func writeUnits(ctx context.Context, x Executor, r quadlet.Rendered) error {
	if _, err := x.Run(ctx, "mkdir", "-p", quadletDir); err != nil {
		return fmt.Errorf("converge: %s: %w", quadletDir, err)
	}
	out, err := x.Run(ctx, "ls", "-1", quadletDir)
	if err != nil {
		return fmt.Errorf("converge: list %s: %w", quadletDir, err)
	}
	for _, n := range nonEmptyLines(out) {
		if !strings.HasPrefix(n, unitPrefix) {
			continue
		}
		if _, keep := r.Files[n]; keep {
			continue
		}
		if _, err := x.Run(ctx, "rm", "-f", quadletDir+"/"+n); err != nil {
			return fmt.Errorf("converge: remove orphan %s: %w", n, err)
		}
	}
	names := make([]string, 0, len(r.Files))
	for n := range r.Files {
		names = append(names, n)
	}
	sort.Strings(names) // deterministic write order, so a part-way failure is reproducible
	for _, n := range names {
		if err := safeUnitName(n); err != nil {
			return err
		}
		if err := x.WriteFile(quadletDir+"/"+n, []byte(r.Files[n])); err != nil {
			return fmt.Errorf("converge: write %s: %w", n, err)
		}
	}
	if _, err := x.Run(ctx, "systemctl", "daemon-reload"); err != nil {
		return fmt.Errorf("converge: daemon-reload: %w", err)
	}
	return nil
}

// warmImage ensures one image is present, starting its .image unit ONLY when it is missing.
//
// PRESENT => DONE, without touching the network. Starting the unit is a `podman image pull`:
// quadlet generates `Wants=network-online.target` + `ExecStart=podman image pull <ref>` with no
// already-present short-circuit (measured with podman's own generator), so an unconditional start
// made every guest reboot depend on reaching a registry — the running half of the doctrine that
// running and failover never need network ([V3b.3](e1)).
//
// MISSING => pull, and wait for it. Absence is not something to fail on: the image SHOULD already
// be here (install warms it, prewarm puts it on every standby, and service-install.nix asserts
// exactly that), so reaching this line means the design was not upheld and a short wait beats
// refusing to run. Deliberately NOT conditioned on whether some other node could promote instead:
// that is cluster-wide reasoning to handle a case that should not arise, and the complexity would
// outlive the edge case.
//
// PULLING IS SAFE BECAUSE THE REF IS DIGEST-PINNED, and that is the whole difference from the
// baked slot, which faces the same question and answers it the other way. `briard-converge`
// REFUSES to promote when its pinned image is not staged, and is right to: its pin is a tag plus
// a pin-file, so a fetch could not guarantee the same bytes. A manifest names `repo@sha256:…`
// (shared/manifest refuses anything else), so a pull returns exactly those bytes or fails —
// identity survives either outcome. One rule, two answers: refuse when you cannot verify what you
// would get, fetch when the digest pins it.
//
// Shared by the service.warm verb and by converge, which is the point: a converging survivor and
// an installing Primary must not disagree about what "the image is here" means.
func warmImage(ctx context.Context, x Executor, unit, ref string) error {
	if unit == "" || ref == "" {
		return fmt.Errorf("warm image: need a unit and an image ref")
	}
	if _, err := x.Run(ctx, "podman", "image", "exists", ref); err == nil {
		return nil
	}
	if out, err := x.Run(ctx, "systemctl", "start", unit); err != nil {
		return fmt.Errorf("warm image (%s): %w: %s", unit, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// mergeInto folds one service's rendering into the set converge writes. Unit filenames are
// service-prefixed by the renderer (briard-<service>-…), so the Files merge cannot collide.
func mergeInto(all *quadlet.Rendered, r quadlet.Rendered) {
	for name, body := range r.Files {
		all.Files[name] = body
	}
	all.Units = append(all.Units, r.Units...)
	all.ImageUnits = append(all.ImageUnits, r.ImageUnits...)
	all.ContainerUnits = append(all.ContainerUnits, r.ContainerUnits...)
	for unit, ref := range r.ImageRefs {
		if all.ImageRefs == nil {
			all.ImageRefs = map[string]string{}
		}
		all.ImageRefs[unit] = ref
	}
}
