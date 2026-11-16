// Package hass is the one place Home Assistant's operational knowledge lives.
//
// A curated catalog is the licence to encode app-specific knowledge in the product, and the first
// thing Briard must know about Home Assistant is how to talk to it: the S1 health gate reads HA's
// config-entry states to tell "the container answers" from "half the house's integrations are
// dead", and reading them needs an authenticated API.
//
// The package delivers two capabilities, split by what each can do only from where it stands: an
// authenticated HA API, available to the guest from the moment HA starts, for consumers outside
// Home Assistant; and briard's own integration, running inside it, for everything that wants a
// running HA rather than a stopped one (offering the node's broker, the apps' update entities,
// Repairs issues for the node's alerts, the owner's login, the recorder's lock), which belongs in
// a process that already holds `hass`, not in a stopped-window script talking HTTP to a process
// it is inside of. The two share no credential: the integration needs no token to act in-process,
// so it never mints the control token; the mint stays where only it can be done, and the token
// serves the outside.
//
// The mechanism, and why each part is where it is:
//
//   - The token value is ours, chosen first and written to tmpfs at converge, so consumers know
//     it from t=0: no return channel, no writable mount, no startup race. HA validates a refresh
//     token by looking the string up in its store, so a chosen value is as good as a generated
//     one.
//
//   - The mint happens in a stopped window because no other kind exists. HA's auth store is
//     memory-cached: a write under a running HA is invisible to it until restart and is clobbered
//     by its next save, and every live route that could issue a token needs credentials already
//     held. A live mint from outside is not a compromise we refused — it does not exist.
//
//   - The stopped window is s6's `run` script, which the image re-executes at every s6 service
//     start: container start, every exit-100 restart (`homeassistant.restart`), and the boot
//     after a config restore. That last is why this hook and no other: a restore wipes /config,
//     `.storage/auth` included, and is itself an exit-100 restart, so the mint after it is the
//     one that sticks, with no extra restart and no marker to race against.
//
//   - We shadow that script rather than patch it, handing over to the image's own `run`,
//     extracted byte-for-byte from the digest-pinned image. Relaying HA's bootstrap instead of
//     authoring a copy keeps an upstream layout change from becoming a correlated, fleet-wide
//     "HA never starts"; it becomes a blessed-image check that fails in CI
//     (nixosTest/hass-payload.nix) before the image can enter the catalog.
//
// Ephemerality is one-sided by HA's model. Our custody is tmpfs-only and the value rotates every
// guest boot; HA must persist the current token to validate it, so the then-current value rides in
// HA's own backups. Hence the prune: every start drops every other token on our user, so one
// resurrected by a restore is revoked as a side effect. Failover needs no secret sync at all — the
// standby mints its own boot value into the replicated store when it promotes.
package hass

import (
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"

	"briard.io/shared/casa"
	"briard.io/shared/dashboard"
	"briard.io/shared/manifest"
	"briard.io/shared/routes"
)

// Name is Home Assistant's catalog slug. Everything in this package is keyed on it:
// an app that is not this one gets nothing from here, which is what makes the
// knowledge encoded here app-specific by construction rather than by discipline.
const Name = "home-assistant"

// Dir is where the control channel's files live: node-local, tmpfs, re-derived by every converge.
// Nothing here is replicated and nothing survives a guest reboot — deliberately, since the token
// rotates per boot and the extracted script must match the digest this node is running.
//
// ⚠️ Named for the app, not this package: the agent resolves an inbound caller by taking a
// directory name in the run directory as the app, so a directory named after the Go package would
// resolve to an app that does not exist. TestAppDirMatchesTheRegistry (agent/apps) asserts the
// two agree.
const Dir = "/run/briard/home-assistant"

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

// notifierPath is the inbound-channel client: it tells the guest agent this app is
// starting and waits for the reply, which is what makes the stopped window an ordering guarantee
// rather than a race. It decides nothing -- see notify.py.
const notifierPath = Dir + "/notify.py"

// planterPath is the /config-side planting, run one-shot in the same stopped window as the mint.
const planterPath = Dir + "/plant.py"

// stubDir holds the integration's stub package, which the planter copies into
// /config/custom_components/briard at every start. It is staged here rather than planted from
// Go because only the container can see /config.
const stubDir = Dir + "/stub"

// implDir is where the integration's implementation is mounted, read-only, outside /config —
// which is the whole placement decision: the restore wipe never sees it, no backup
// can carry it, and the stub in /config stays inert wherever it is restored. The stub names
// this same path; the two agree on a path and a module name and on nothing else.
const implDir = Dir + "/integration"

// implPath is the implementation itself, one module under implDir.
const implPath = implDir + "/briard_ha.py"

// mqttPortToken is the placeholder the integration carries for the broker's port. The value is
// agent/mosquitto's, handed down by the registry rather than restated here: this is the one fact
// that spans two catalogued apps, and a second copy of it would be a second thing to keep in
// step with the catalog.
const mqttPortToken = "@MQTT_PORT@"

// alertsPathToken, appsPathToken, dashboardURLToken and externalURLPathToken are the
// integration's other placeholders: where it reads the alert copy and the apps' releases inside
// the container, the page its Repairs issues and updates link to, and where it reads the
// external URL. Each value has one definition elsewhere (the
// binds, shared/routes, ExternalURLPath), so the integration carries a token rather than a
// second copy.
const (
	alertsPathToken      = "@ALERTS_PATH@"
	appsPathToken        = "@APPS_PATH@"
	dashboardURLToken    = "@DASHBOARD_URL@"
	externalURLPathToken = "@EXTERNAL_URL_PATH@"
)

// ExternalURLPath is the URL the integration sets as Home Assistant's external_url when it has
// none: `https://` and the app's casa name, written by the guest agent from the host's casa
// view only once the household's network is known to resolve that name, and empty otherwise.
// It is runtime state in the app directory, which the container sees at /briard: converge
// writes its own files there by name and never clears the directory, and a change to it
// restarts nothing (only a changed unit file does).
const ExternalURLPath = Dir + "/external-url"

// ExternalURL is that file's content for the host's casa view: the URL only for a registered
// name the household's network resolves, else "". The view names the flock's apex
// (`<flock>.briard.casa`); anything else yields "".
func ExternalURL(c dashboard.Casa) string {
	if c.State != casa.ClaimRegistered || c.Resolves != dashboard.CasaResolves {
		return ""
	}
	flock, ok := strings.CutSuffix(c.Name, "."+casa.Zone)
	if !ok || flock == "" || strings.Contains(flock, ".") {
		return ""
	}
	return "https://" + routes.CasaHostName(flock, Name)
}

// alertsMount is where the guest's alert copy (shared/dashboard.AlertsDir) appears inside Home
// Assistant's container: read-only, and a sibling of /briard, never inside it (a bind nested in
// a read-only bind cannot be created).
const alertsMount = "/briard-alerts"

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

//go:embed notify.py
var notifierSource string

//go:embed plant.py
var planterSource string

//go:embed component/__init__.py
var stubSource string

//go:embed component/manifest.json
var stubManifest string

// stubUpdate is the stub's update platform: Home Assistant imports a platform only from the
// integration's package, so the apps' update entities need a file there that delegates.
//
//go:embed component/update.py
var stubUpdate string

//go:embed briard_ha.py
var implSource string

// stubTranslations is the stub's one string: a Repairs issue's title and description are
// translation keys, and Home Assistant reads an integration's translations from beside its
// package -- the stub in /config -- so the alert's own words pass through placeholders.
//
//go:embed component/translations/en.json
var stubTranslations string

// stubIcon and stubIcon2x are Briard's mark for Home Assistant's integration page and its Repairs
// issues. Home Assistant serves a custom integration's brand images from beside its package,
// and nothing else of ours is there to serve them from.
//
//go:embed component/brand/icon.png
var stubIcon string

//go:embed component/brand/icon@2x.png
var stubIcon2x string

// Executor is the narrow slice of the guest agent's executor this package needs. A
// local interface for dependency injection, not a seam: it exists so the package can
// be driven by a fake in tests without importing the guest agent (which imports this).
type Executor interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
	WriteFile(path string, data []byte) error
	ReadFile(path string) ([]byte, error)
	Stat(path string) (fs.FileInfo, error)
	ListDir(path string) ([]string, error)
}

// Volumes returns the extra quadlet Volume= values one container needs, which for
// every app but Home Assistant is none.
//
// The manifest schema cannot express a host bind, and that is not an oversight to work around:
// capability comes by omission there, so a catalog entry cannot ask for the host. These binds are
// the product's own knowledge about one app, applied on the node — exactly where the readiness
// registry puts app-specific knowledge too — and Render stays a pure function of the manifest:
// the same manifest renders the same units on every node. The app's own directory (read-only at
// /briard) is not here: it is the product's general shape, so agent/apps adds it for every app
// that has one. What is here is Home Assistant knowledge: our s6 wrapper shadow-mounted over the
// image's own `run`, at a path only this image has, and the node's alerts for the integration.
// Both sources live outside /config, so HA's restore wipe never sees them.
func Volumes(m manifest.Manifest, c manifest.Container) []string {
	if m.Name != Name || !c.Primary {
		return nil
	}
	return []string{
		wrapperPath + ":" + s6Run + ":ro",
		// The node's alerts, for the integration to mirror into Repairs (briard_ha.py). The
		// directory, not the file: the copy is replaced by a rename, which a file bind never shows.
		dashboard.AlertsDir + ":" + alertsMount + ":ro",
	}
}

// RestoreMarker is the file Home Assistant writes inside its config directory to say that a
// restore of one of its own backups is in flight. The household asks a live HA for the restore;
// HA writes this and exits 100 to be restarted into it, and the restore unlinks the marker in a
// `finally` right after parsing and before the wipe (its own "prevent a boot loop" step). So it
// exists only across the restart in between — the window the s6 wrapper's notification lands in,
// which is why that notification is the only thing in the system that can see one.
//
// ⚠️ It is also a landmine inside a ring member. A member taken while it is present carries it,
// and HA's wipe deliberately keeps `backups/`, so restoring that member naively would hand HA back
// both the request and the tar, and the household would land straight back where they were trying
// to leave. Members are swept of it as they are materialised: apps.RestoreMarkers names it, and
// the guest's data.replace removes it from the staged copy.
const RestoreMarker = ".HA_RESTORE"

// Prepare materialises the control channel on this node, and is a no-op for every
// other app.
//
// It runs at converge, not at install, for the sake of a node that was down when the install ran:
// converge is what a promoting survivor executes against the volume, and an install-time-only step
// would leave that node mounting a wrapper that does not exist. /run is tmpfs, so this is also the
// only placement that survives a guest reboot; one code path covers install, upgrade, reboot and
// cold promotion. It must run after the image is warm (the extraction reads it) and before the
// containers start (the mounts must resolve).
//
// Every write is a rename over a possibly-live bind mount source: a truncate-in-place would be
// visible inside a running container mid-write, including on the script s6 is about to exec,
// while a rename swaps the directory entry, so an already-running container keeps the inode it
// started with and the next start gets the new one.
//
// Failure must not be survived silently. The rendered units name bind sources this writes, and
// (measured on runc, the OCI bind semantics podman drives) a missing bind source is created as a
// root-owned directory, after which the container refuses to start — "not a directory: Are you
// trying to mount a directory onto a file" — and the path is poisoned for the next attempt, since
// a file has to go where a directory now sits: a caller that ignores this error and starts the
// container anyway gets an app that can never run again. Converge declines to start this app and
// lets the others promote (argued at its call site in agent/guestagent).
//
// mqttPort is the broker's port, passed in by the registry (agent/apps) because it belongs to the
// other app. Zero means "this build knows of no broker": the integration is then materialised with
// nothing to dial -- it still loads, and its own socket check is what stops it.
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
	if err := write(ctx, x, notifierPath, notifierSource, "0644"); err != nil {
		return err
	}
	if err := write(ctx, x, planterPath, planterSource, "0644"); err != nil {
		return err
	}
	if err := writeIntegration(ctx, x, mqttPort); err != nil {
		return err
	}
	if err := write(ctx, x, wrapperPath, wrapperSource, "0755"); err != nil {
		return err
	}
	return extractOriginal(ctx, x, m.Primary().Image)
}

// ensureToken mints this boot's value once and keeps it. Ensure, not rotate: converge runs on
// every install, promotion and guest boot, and a value changed under an already-running container
// would break every consumer until something restarted HA. Rotation comes from tmpfs instead — a
// guest reboot clears /run, and nothing is running then either.
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

// extractOriginal copies the image's own `run` out of the pinned image, every time, because the
// digest is what it must match: an upgrade changes the image and the extracted script has to
// change with it. The container is created and removed without ever being started, so nothing
// from the image executes here.
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
	// What came back has to be a script: `podman cp` copies a directory as happily as a file, so
	// a layout change that turned this path into a directory (a native s6-rc service is a
	// directory holding `run`) would otherwise be mounted over HA's `run`, the same unstartable
	// container as a missing source. ReadFile answers both questions at once: a directory is an
	// error, and an empty file has a length.
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
// s6 legacy-services bundle) and the native s6-rc tree beside it.
//
// They are here to describe, never to search: adapting to the script found elsewhere would defeat
// relaying it at all. The bind path is fixed when the unit is rendered, before this code can look
// at anything, and an upstream layout change is meant to fail a blessed image in catalog CI rather
// than be papered over on a household's node. What breadth buys is a diagnosis: the failure names
// the layout the image actually has instead of an errno.
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
		names, err := x.ListDir(dst)
		if err != nil {
			continue
		}
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

// writeIntegration materialises briard's own integration: the stub the planter will copy into
// /config, and the implementation it delegates to, staged outside /config where no restore and
// no backup can reach it. Every placeholder is substituted rather than restated in the
// integration: each value has one definition elsewhere (the tokens' comments above say where).
//
// The substitution is checked both ways, and the before-check is the half that bites: a Replace
// of an absent token is a silent no-op, so a check only after it catches a second placeholder and
// never a missing one -- and a missing one installs an integration dialling a port named
// "@MQTT_PORT@", failing silently at every Home Assistant start in the fleet.
func writeIntegration(ctx context.Context, x Executor, mqttPort int) error {
	src := implSource
	for _, t := range [][2]string{
		{mqttPortToken, strconv.Itoa(mqttPort)},
		{alertsPathToken, alertsMount + "/" + filepath.Base(dashboard.AlertsPath)},
		{appsPathToken, alertsMount + "/" + filepath.Base(dashboard.AppsPath)},
		{dashboardURLToken, "http://" + routes.BareFlockHostName + "/"},
		{externalURLPathToken, mountPoint + "/" + filepath.Base(ExternalURLPath)},
	} {
		if !strings.Contains(src, t[0]) {
			return fmt.Errorf("hass: the integration no longer carries %s", t[0])
		}
		src = strings.Replace(src, t[0], t[1], 1)
		if strings.Contains(src, t[0]) {
			return fmt.Errorf("hass: the integration carries %s more than once", t[0])
		}
	}
	// The alert copy's directory too: it is a bind source of the container, and one that is
	// missing at start fails the container. The guest agent fills it when the host pushes.
	for _, dir := range []string{stubDir, stubDir + "/translations", stubDir + "/brand", implDir, dashboard.AlertsDir} {
		if out, err := x.Run(ctx, "mkdir", "-p", dir); err != nil {
			return fmt.Errorf("hass: %s: %w: %s", dir, err, strings.TrimSpace(string(out)))
		}
	}
	for _, f := range [][2]string{
		{stubDir + "/__init__.py", stubSource},
		{stubDir + "/manifest.json", stubManifest},
		{stubDir + "/update.py", stubUpdate},
		{stubDir + "/translations/en.json", stubTranslations},
		{stubDir + "/brand/icon.png", stubIcon},
		{stubDir + "/brand/icon@2x.png", stubIcon2x},
	} {
		if err := write(ctx, x, f[0], f[1], "0644"); err != nil {
			return err
		}
	}
	return write(ctx, x, implPath, src, "0644")
}
