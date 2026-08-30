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
// FAILURE IS FATAL TO CONVERGE, which is the harsher of the two available answers and
// the right one. Returning nil here would leave the rendered units naming bind sources
// that do not exist, and the degradation from that is not "HA starts without a token".
// MEASURED (docker 29.6.2 / runc, same OCI bind semantics podman drives): a missing bind
// source is CREATED as a root-owned directory and the container then refuses to start —
// "not a directory: Are you trying to mount a directory onto a file". It also poisons the
// path, since the next Prepare has to write a FILE where a directory now sits. A
// promotion that fails loudly beats one that half-succeeds and cannot recover.
func Prepare(ctx context.Context, x Executor, m manifest.Manifest) error {
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
		return fmt.Errorf("hass: extract %s: %w: %s", s6Run, err, strings.TrimSpace(string(out)))
	}
	if out, err := x.Run(ctx, "chmod", "0755", tmp); err != nil {
		return fmt.Errorf("hass: %s permissions: %w: %s", tmp, err, strings.TrimSpace(string(out)))
	}
	if out, err := x.Run(ctx, "mv", "-f", tmp, originalPath); err != nil {
		return fmt.Errorf("hass: install %s: %w: %s", originalPath, err, strings.TrimSpace(string(out)))
	}
	return nil
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
