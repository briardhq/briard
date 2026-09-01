// Package hass is the one place Home Assistant's operational knowledge lives.
//
// A curated catalog is not a smaller app store — it is the licence to encode
// service-specific knowledge in the product. Briard restricts itself to a handful of
// services precisely so it can know how each one is operated, and the first thing it
// has to know about Home Assistant is how to TALK to it: HA's config-entry states are
// what the S1 health gate reads to tell "the container answers" from "half the house's
// integrations are dead", and reading them needs an authenticated, admin-scoped API.
//
// So this package delivers exactly one capability: an authenticated HA API, available
// to the guest from the moment HA starts. It is deliberately auth-only. There is no
// briard code inside Home Assistant — no custom integration, no plugin, no planted
// files — because a token is the whole need, and two mechanisms over one credential is
// two failure modes and split authority.
//
// THE MECHANISM, and why every part of it is where it is:
//
//   - The token VALUE is ours and is chosen first, written to tmpfs at converge. So
//     consumers know it from t=0: no return channel, no writable mount, no startup
//     race. HA validates a refresh token by looking the string up in its store, so a
//     chosen value is as good as a generated one.
//
//   - The mint happens in a STOPPED window, because no other kind exists. HA's auth
//     store is memory-cached: a write under a running HA is invisible to it until
//     restart and is clobbered by its next save, and every live route that could issue
//     a token needs credentials already held. A live mint from outside is not a
//     compromise we refused — it does not exist.
//
//   - The stopped window is s6's `run` script, which the image re-executes at EVERY
//     service start: container start, every exit-100 restart (`homeassistant.restart`),
//     and the boot after a config restore. That last one is why this hook and no other:
//     a restore WIPES /config, `.storage/auth` included, and the restore is itself an
//     exit-100 restart — so the mint that lands after it is the one that sticks, with
//     no extra restart and no marker to race against.
//
//   - We SHADOW that script rather than patch it, and the thing we hand over to is the
//     image's own `run`, extracted byte-for-byte from the digest-pinned image. Relaying
//     HA's bootstrap instead of authoring a copy of it is what keeps an upstream layout
//     change from becoming a correlated, fleet-wide "HA never starts". What it becomes
//     instead is a blessed-image check that fails in CI (nixosTest/hass-payload.nix),
//     before the image can enter the catalog.
//
// Ephemerality is one-sided by HA's model. Our custody is tmpfs-only and the value
// rotates every guest boot; HA must persist the current token to validate it, so the
// then-current value rides in HA's own backups. The prune is the answer to that: every
// start drops every OTHER token on our user, so one resurrected by a restore is revoked
// as a side effect. Failover needs no secret sync at all — the standby mints its own
// boot value into the replicated store when it promotes.
package hass

import (
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"briard.io/shared/manifest"
)

// Name is Home Assistant's catalog slug. Everything in this package is keyed on it:
// a service that is not this one gets nothing from here, which is what makes the
// knowledge encoded here service-specific by construction rather than by discipline.
const Name = "home-assistant"

// Dir is where the control channel's files live: node-local, tmpfs, re-derived by
// every converge. Nothing here is replicated and nothing survives a guest reboot —
// deliberately, since the token rotates per boot and the extracted script must match
// the digest THIS node is running.
const Dir = "/run/briard/hass"

// TokenPath is the refresh token, 0600 on tmpfs. It is the guest-side half of the
// control channel: whatever needs to talk to HA reads the value here and does the
// documented refresh -> access exchange (POST /auth/token, grant_type=refresh_token,
// client_id omitted) per use.
const TokenPath = Dir + "/token"

// mountPoint is where Dir appears inside the container — one read-only bind for the
// whole directory rather than one per file.
const mountPoint = "/briard"

// s6Run is the script the image re-executes at every service start, and the only hook
// that fires on the post-restore boot. Ours is mounted over it; the image's own is
// extracted next to it and exec'd by ours.
const s6Run = "/etc/services.d/home-assistant/run"

// originalPath holds the image's own `run`, extracted byte-for-byte at converge.
const originalPath = Dir + "/run.original"

// wrapperPath is our shadow of s6Run.
const wrapperPath = Dir + "/run"

// scriptPath is the mint, run one-shot on the image's own python.
const scriptPath = Dir + "/ensure-token.py"

// wirerPath is the mqtt wiring ([V3b.30](c)), spawned detached by the wrapper and left to wait
// for a serving Home Assistant on its own.
const wirerPath = Dir + "/wire-mqtt.py"

// mqttPortToken is the placeholder wire-mqtt.py carries for the broker's port. The value is
// agent/mosquitto's, handed down by the registry rather than restated here: this is the one fact
// that spans two catalogued services, and a second copy of it would be a second thing to keep in
// step with the catalog.
const mqttPortToken = "@MQTT_PORT@"

// haPortToken is the placeholder for Home Assistant's OWN port. Substituted from the manifest
// for the reason Readiness states: the catalog names it once, and a second copy here would be a
// second thing to keep in step with it.
const haPortToken = "@HA_PORT@"

// extractCtr is the throwaway container the original `run` is copied out of. Created
// and removed, never started: `podman cp` reads the image's filesystem without
// executing anything from it.
const extractCtr = "briard-hass-extract"

// tokenBytes matches what HA generates for a refresh token of its own
// (`secrets.token_hex(64)`), so ours is indistinguishable in strength and in shape.
const tokenBytes = 64

//go:embed run.sh
var wrapperSource string

//go:embed ensure-token.py
var scriptSource string

//go:embed wire-mqtt.py
var wirerSource string

// Executor is the narrow slice of the guest agent's executor this package needs. A
// local interface for dependency injection, not a seam: it exists so the package can
// be driven by a fake in tests without importing the guest agent (which imports this).
type Executor interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
	WriteFile(path string, data []byte) error
	ReadFile(path string) ([]byte, error)
}

// Volumes returns the extra quadlet Volume= values one container needs, which for
// every service but Home Assistant is none.
//
// The manifest schema cannot express a host bind, and that is not an oversight to work
// around: capability comes by omission there, so that a catalog entry cannot ask for
// the host. These two binds are the product's own knowledge about one service, applied
// on the node — which is exactly where the readiness registry puts service-specific
// knowledge too. Render stays a pure function of the manifest: the same manifest
// renders the same units on every node.
//
// The whole directory goes in read-only under one mount point, and our wrapper is
// shadow-mounted over the image's `run`. Both sources live outside /config, so HA's
// restore wipe never sees them.
func Volumes(m manifest.Manifest, c manifest.Container) []string {
	if m.Name != Name || !c.Primary {
		return nil
	}
	return []string{
		Dir + ":" + mountPoint + ":ro",
		wrapperPath + ":" + s6Run + ":ro",
	}
}

// Prepare materialises the control channel on this node, and is a no-op for every
// other service.
//
// IT RUNS AT CONVERGE, not at install, and the difference is a node that was down when
// the install ran. Converge is what a promoting survivor executes against the volume
// ([V3b.3](f)); an install-time-only step would leave that node mounting a wrapper
// that does not exist. /run is tmpfs, so this is also the only placement that survives
// a guest reboot. One code path covers install, upgrade, reboot and cold promotion.
//
// It must run AFTER the image is warm (the extraction reads it) and BEFORE the
// containers start (the mounts must resolve).
//
// EVERY WRITE IS A RENAME over a possibly-live bind mount source. A truncate-in-place
// would be visible inside a running container mid-write — including on the script s6
// is about to exec. A rename swaps the directory entry, so a container that is already
// running keeps the inode it started with and the next start gets the new one.
//
// FAILURE MUST NOT BE SURVIVED SILENTLY, and the caller is what decides how loudly. The
// rendered units name bind sources this writes, and MEASURED (docker 29.6.2 / runc, the
// same OCI bind semantics podman drives) a missing bind source is CREATED as a root-owned
// directory, after which the container refuses to start — "not a directory: Are you trying
// to mount a directory onto a file" — and the path is poisoned for the next attempt, since
// a file has to go where a directory now sits. So a caller that ignores this error and
// starts the container anyway gets a service that can never run again.
//
// What converge does with it is NOT to fail the whole node: it declines to start THIS
// service and lets the others promote, which is the only safe use of the error and is
// argued at its call site. Never starting the unit is what keeps podman from creating the
// bad source in the first place.
// mqttPort is the broker's port, passed in by the registry (agent/services) because it belongs
// to the OTHER service. Zero means "this build knows of no broker", and the wirer is then
// materialised with nothing to dial -- it still runs, and its own socket check is what stops it.
func Prepare(ctx context.Context, x Executor, m manifest.Manifest, mqttPort int) error {
	if m.Name != Name {
		return nil
	}
	if _, err := x.Run(ctx, "mkdir", "-p", Dir); err != nil {
		return fmt.Errorf("hass: %s: %w", Dir, err)
	}
	// 0700: the token is a credential for the whole HA API.
	if _, err := x.Run(ctx, "chmod", "700", Dir); err != nil {
		return fmt.Errorf("hass: %s permissions: %w", Dir, err)
	}
	if err := ensureToken(ctx, x); err != nil {
		return err
	}
	if err := write(ctx, x, scriptPath, scriptSource, "0644"); err != nil {
		return err
	}
	if err := writeWirer(ctx, x, m.Primary().Port, mqttPort); err != nil {
		return err
	}
	if err := write(ctx, x, wrapperPath, wrapperSource, "0755"); err != nil {
		return err
	}
	return extractOriginal(ctx, x, m.Primary().Image)
}

// ensureToken mints this boot's value once and keeps it.
//
// ENSURE, NOT ROTATE: converge runs on every install and every promotion, and a value
// that changed under a container that is already running would break every consumer
// until something restarted HA. Rotation comes from tmpfs instead — a guest reboot
// clears /run, and nothing is running then either.
func ensureToken(ctx context.Context, x Executor) error {
	if raw, err := x.ReadFile(TokenPath); err == nil && len(strings.TrimSpace(string(raw))) >= 2*tokenBytes {
		return nil
	}
	buf := make([]byte, tokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Errorf("hass: generate token: %w", err)
	}
	return write(ctx, x, TokenPath, hex.EncodeToString(buf)+"\n", "0600")
}

// extractOriginal copies the image's own `run` out of the pinned image, every time.
//
// Unconditionally, because the digest is what it must match: an upgrade changes the
// image and the extracted script has to change with it. The container is created and
// removed without ever being started, so nothing from the image executes here.
func extractOriginal(ctx context.Context, x Executor, image string) error {
	if image == "" {
		return fmt.Errorf("hass: the manifest names no primary image")
	}
	// A leftover from a converge that died between create and rm would make the create
	// fail on a name collision, so clear it first rather than diagnose it later.
	_, _ = x.Run(ctx, "podman", "rm", "-f", extractCtr)
	if out, err := x.Run(ctx, "podman", "create", "--name", extractCtr, image); err != nil {
		return fmt.Errorf("hass: stage %s for extraction: %w: %s", image, err, strings.TrimSpace(string(out)))
	}
	defer func() { _, _ = x.Run(ctx, "podman", "rm", "-f", extractCtr) }()
	tmp := originalPath + ".new"
	if out, err := x.Run(ctx, "podman", "cp", extractCtr+":"+s6Run, tmp); err != nil {
		return fmt.Errorf("hass: extract %s: %w: %s%s", s6Run, err, strings.TrimSpace(string(out)), describeLayout(ctx, x))
	}
	// WHAT CAME BACK HAS TO BE A SCRIPT. `podman cp` copies a DIRECTORY just as happily as a
	// file, so a layout change that turned this path into a directory — a native s6-rc service
	// is a directory holding `run` — would otherwise be mounted over HA's `run` and produce the
	// same unstartable container as a missing source. ReadFile answers both questions at once:
	// a directory is an error, and an empty file has a length.
	body, err := x.ReadFile(tmp)
	if err != nil {
		return fmt.Errorf("hass: extracted %s is not a readable file: %w%s", s6Run, err, describeLayout(ctx, x))
	}
	if len(body) == 0 {
		return fmt.Errorf("hass: extracted %s is empty%s", s6Run, describeLayout(ctx, x))
	}
	if out, err := x.Run(ctx, "chmod", "0755", tmp); err != nil {
		return fmt.Errorf("hass: %s permissions: %w: %s", tmp, err, strings.TrimSpace(string(out)))
	}
	if out, err := x.Run(ctx, "mv", "-f", tmp, originalPath); err != nil {
		return fmt.Errorf("hass: install %s: %w: %s", originalPath, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// serviceRoots are the two places an s6-overlay image can keep its service definitions: the
// legacy tree (what Home Assistant uses, and what s6-overlay v3 still supports through its
// `legacy-services` bundle) and the native s6-rc tree beside it.
//
// They are here to DESCRIBE, never to search. Finding the script elsewhere and adapting to it
// would defeat the point of relaying it at all: the bind path is fixed when the unit is rendered,
// before this code can look at anything, and an upstream layout change is meant to fail a blessed
// image in catalog CI rather than be papered over on a household's node (§6.4). What breadth buys
// is a diagnosis — the failure names the layout the image actually has instead of an errno.
var serviceRoots = []string{"/etc/services.d", "/etc/s6-overlay/s6-rc.d"}

// describeLayout reports what service definitions the staged image carries, for the error message
// that a layout change will produce. Best-effort and silent about its own failures: it runs only
// on a path that has already failed, and a diagnosis that could itself fail the caller would be
// worse than no diagnosis.
func describeLayout(ctx context.Context, x Executor) string {
	var found []string
	for _, root := range serviceRoots {
		dst := Dir + "/layout"
		_, _ = x.Run(ctx, "rm", "-rf", dst)
		if _, err := x.Run(ctx, "podman", "cp", extractCtr+":"+root, dst); err != nil {
			continue
		}
		out, err := x.Run(ctx, "ls", "-1", dst)
		if err != nil {
			continue
		}
		names := strings.Fields(string(out))
		sort.Strings(names)
		found = append(found, fmt.Sprintf("%s: %s", root, strings.Join(names, " ")))
		_, _ = x.Run(ctx, "rm", "-rf", dst)
	}
	if len(found) == 0 {
		return " (the image carries no service directory this build knows of)"
	}
	return " (the image's service layout is " + strings.Join(found, "; ") + ")"
}

// write puts content at path atomically, with the mode given. See Prepare on why every
// write here is a rename.
func write(ctx context.Context, x Executor, path, content, mode string) error {
	tmp := path + ".new"
	if err := x.WriteFile(tmp, []byte(content)); err != nil {
		return fmt.Errorf("hass: write %s: %w", tmp, err)
	}
	if out, err := x.Run(ctx, "chmod", mode, tmp); err != nil {
		return fmt.Errorf("hass: %s permissions: %w: %s", tmp, err, strings.TrimSpace(string(out)))
	}
	if out, err := x.Run(ctx, "mv", "-f", tmp, path); err != nil {
		return fmt.Errorf("hass: install %s: %w: %s", path, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// writeWirer materialises the mqtt wiring with both ports substituted in: Home Assistant's own,
// so the wirer talks to the port the CATALOG names rather than one restated here, and the
// broker's, which arrives from the registry because it belongs to the other service.
//
// EACH SUBSTITUTION IS CHECKED BOTH WAYS, and the before-check is the half that actually bites: a
// Replace of a token that is not there is a silent no-op, so a check only AFTER it catches a
// SECOND placeholder and never a MISSING one -- and a missing one is the case that installs a
// wirer dialling a port named "@MQTT_PORT@", failing forever and silently, at every Home
// Assistant start in the fleet.
func writeWirer(ctx context.Context, x Executor, haPort, mqttPort int) error {
	src := wirerSource
	for _, sub := range []struct {
		token string
		value int
	}{{haPortToken, haPort}, {mqttPortToken, mqttPort}} {
		if !strings.Contains(src, sub.token) {
			return fmt.Errorf("hass: the wirer no longer carries %s", sub.token)
		}
		src = strings.Replace(src, sub.token, strconv.Itoa(sub.value), 1)
		if strings.Contains(src, sub.token) {
			return fmt.Errorf("hass: the wirer carries %s more than once", sub.token)
		}
	}
	return write(ctx, x, wirerPath, src, "0644")
}
