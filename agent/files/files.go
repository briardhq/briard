// Package files is the product-side knowledge Briard holds about Files: the household's window
// onto every app's directories, a web file browser (FileBrowser Quantum) shown one folder per app.
//
// IT IS THE ONE APP THAT READS OTHER APPS' AREAS, and that is why its binds live here, keyed on
// the catalog name, and never in a manifest: a catalog entry that could ask for another app's
// data would be a catalog that could hand any app the household's files. Being curated is the
// licence to hold this, as it is for Home Assistant's control channel and mosquitto's config.
//
// WHAT IT SEES IS A VIEW, NOT THE TIERS. The guest agent keeps /run/briard/files-view as one
// folder per app holding the directories that app's manifest mounts, each a bind of the real one,
// and Files mounts that tree once with slave propagation, so a bind the agent makes or replaces
// later reaches the running container. Binding each app directory into Files' own unit instead
// would go stale the moment a restore swapped the area underneath it -- the bind keeps the old,
// deleted copy, and an edit made through it vanishes -- so the binds are the agent's to keep true,
// and Files never restarts for another app's sake.
//
// SIGNED-IN BY THE DASHBOARD. The reverse proxy lets a browser through to Files only with the
// dashboard's own session, and then signs the household into the app with a short-lived token
// made with KeyPath's secret. Files verifies it on every request; nothing without the key can make
// one, wherever on the network it sits.
package files

import (
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"fmt"
	"strings"

	"briard.io/shared/manifest"
)

// Name is Files' catalog slug. Everything in this package is keyed on it.
const Name = "files"

// Dir is Files' own directory on the guest, mounted read-only at /briard in its container: its
// config, its mark, and the signing secret. /run, so every boot re-derives it.
const Dir = "/run/briard/files"

// ViewDir is the view the guest agent keeps, and ViewMount is where Files finds it.
const (
	ViewDir   = "/run/briard/files-view"
	ViewMount = "/srv"
)

// KeyPath is the secret the reverse proxy signs the household's token with, and Files verifies it
// with. Root's alone (0600): Files runs as root and reads it from its config, the proxy reads it
// from here.
const KeyPath = Dir + "/gate.key"

const (
	confPath    = Dir + "/config.yaml"
	faviconPath = Dir + "/favicon.png"
)

//go:embed config.yaml
var confSource string

//go:embed favicon.png
var favicon []byte

// secretToken is the placeholder config.yaml carries for the signing secret.
const secretToken = "@SECRET@"

// Executor is the guest-side command runner Prepare needs.
type Executor interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
	WriteFile(path string, data []byte) error
	ReadFile(path string) ([]byte, error)
}

// Volumes is the bind Files' container needs beyond its own: the view, with slave propagation so
// that what the agent binds into it after the container started is seen inside.
func Volumes(m manifest.Manifest, c manifest.Container) []string {
	if m.Name != Name || !c.Primary {
		return nil
	}
	return []string{ViewDir + ":" + ViewMount + ":rslave"}
}

// Prepare materialises Files' directory and the view's root on this node, before the container
// starts.
//
// THE SECRET IS KEPT FOR THE BOOT, never rotated per converge as the inbound token is: converge
// restarts only the apps whose units changed, so a secret minted anew would leave a running Files
// verifying against the old one while the proxy signs with the new. /run ends it with the boot.
func Prepare(ctx context.Context, x Executor, m manifest.Manifest) error {
	if m.Name != Name {
		return nil
	}
	for _, d := range []string{Dir, ViewDir} {
		if _, err := x.Run(ctx, "mkdir", "-p", d); err != nil {
			return fmt.Errorf("files: %s: %w", d, err)
		}
	}
	key, err := secret(ctx, x)
	if err != nil {
		return err
	}
	// Checked before the replace, because a Replace of a token that is not there is a silent no-op
	// and Files would start verifying against the placeholder.
	if strings.Count(confSource, secretToken) != 1 {
		return fmt.Errorf("files: the config template must carry %s exactly once", secretToken)
	}
	conf := strings.Replace(confSource, secretToken, key, 1)
	if err := write(ctx, x, confPath, []byte(conf), "0600"); err != nil {
		return err
	}
	return write(ctx, x, faviconPath, favicon, "0644")
}

// secret is the boot's signing secret: the one already written, or a new one.
func secret(ctx context.Context, x Executor) (string, error) {
	if raw, err := x.ReadFile(KeyPath); err == nil {
		if k := strings.TrimSpace(string(raw)); len(k) == 64 {
			return k, nil
		}
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("files: mint the signing secret: %w", err)
	}
	k := hex.EncodeToString(buf)
	if err := write(ctx, x, KeyPath, []byte(k), "0600"); err != nil {
		return "", err
	}
	return k, nil
}

// write installs a file: written beside, its mode set, renamed over, so a container that starts
// mid-Prepare never binds a half-written file.
func write(ctx context.Context, x Executor, path string, content []byte, mode string) error {
	tmp := path + ".new"
	if err := x.WriteFile(tmp, content); err != nil {
		return fmt.Errorf("files: write %s: %w", tmp, err)
	}
	if out, err := x.Run(ctx, "chmod", mode, tmp); err != nil {
		return fmt.Errorf("files: %s permissions: %w: %s", tmp, err, strings.TrimSpace(string(out)))
	}
	if out, err := x.Run(ctx, "mv", "-f", tmp, path); err != nil {
		return fmt.Errorf("files: install %s: %w: %s", path, err, strings.TrimSpace(string(out)))
	}
	return nil
}
