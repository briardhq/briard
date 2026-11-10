// Package mealie is the product-side knowledge Briard holds about Mealie, the recipe manager: what
// it is told on this node, and how the door signs the user into it.
//
// SIGNED IN BY THE DOOR, BY HEADER. Mealie reads its user from a reverse proxy's header when the
// proxy also sends a secret it shares with Mealie, and its login page asks for that sign-in as it
// loads, so it opens with no dialog. The door lets a browser through only with the dashboard's
// session, and then sends the user's username and the secret (shared/routes: Gate, GateHeader).
// Nothing that reaches Mealie's address without the door holds the secret, which only root and
// Mealie's own uid on this node can read. Signing up by header stays off: the one account is
// Mealie's own first one, which the dashboard takes over at the first open.
//
// ITS SETTINGS ARE FILES. Mealie reads any setting from a file of that name in /run/secrets, so
// everything briard decides about it -- the sign-in, the password form, its address -- is written
// here, one file per setting, into a directory bound there. The catalog's manifest stays what any
// Mealie needs, and what briard leaves in Mealie's own data is an ordinary Mealie.
package mealie

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"maps"
	"slices"
	"strings"

	"briard.io/shared/manifest"
)

// Name is Mealie's catalog slug. Everything in this package is keyed on it.
const Name = "mealie"

// Dir is Mealie's own directory on the guest; /run, so every boot re-derives it. SettingsDir is
// the part bound read-only at SettingsMount in the container.
const (
	Dir           = "/run/briard/mealie"
	SettingsDir   = Dir + "/settings"
	SettingsMount = "/run/secrets"
)

// KeyPath is the secret the door sends Mealie, in SecretHeader; Mealie compares it with this same
// file.
const (
	KeyPath      = SettingsDir + "/REVERSE_PROXY_AUTH_SECRET"
	SecretHeader = "X-Mealie-Reverse-Proxy-Secret"
)

// uid is the user Mealie's image drops to before it reads its settings (its PUID default).
const uid = "911"

// BaseURL is the address Mealie puts in the links it makes (an invitation, a calendar feed, a
// shared recipe's preview): its bare alias, the name the dashboard opens it on. The same on every
// node and with casa on or off, so turning casa on re-renders nothing.
const BaseURL = "http://briard-mealie.local"

// settings is every setting briard gives Mealie, beside the secret: the password form gone (the API
// it hides refuses the one account's forgotten password anyway), the door's sign-in on, and
// nobody new made by a header.
var settings = map[string]string{
	"ALLOW_PASSWORD_LOGIN":              "false",
	"REVERSE_PROXY_AUTH_ENABLED":        "true",
	"REVERSE_PROXY_AUTH_SIGNUP_ENABLED": "false",
	"BASE_URL":                          BaseURL,
}

// Executor is the guest-side command runner Prepare needs.
type Executor interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
	WriteFile(path string, data []byte) error
	ReadFile(path string) ([]byte, error)
}

// Volumes is the bind Mealie's container needs beyond its own data: its settings, read-only.
func Volumes(m manifest.Manifest, c manifest.Container) []string {
	if m.Name != Name || !c.Primary {
		return nil
	}
	return []string{SettingsDir + ":" + SettingsMount + ":ro"}
}

// Prepare writes Mealie's settings on this node, before the container starts: Mealie's uid owns
// them and nobody else but root reads them.
//
// THE SECRET IS KEPT FOR THE BOOT, as Files' is: converge restarts only the apps whose units
// changed, so a secret minted anew would leave a running Mealie comparing against the old one
// while the door sends the new. /run ends it with the boot.
func Prepare(ctx context.Context, x Executor, m manifest.Manifest) error {
	if m.Name != Name {
		return nil
	}
	if _, err := x.Run(ctx, "mkdir", "-p", SettingsDir); err != nil {
		return fmt.Errorf("mealie: %s: %w", SettingsDir, err)
	}
	for _, args := range [][]string{{"chown", uid + ":" + uid, SettingsDir}, {"chmod", "0500", SettingsDir}} {
		if out, err := x.Run(ctx, args[0], args[1:]...); err != nil {
			return fmt.Errorf("mealie: %s %s: %w: %s", args[0], SettingsDir, err, strings.TrimSpace(string(out)))
		}
	}
	key, err := secret(x)
	if err != nil {
		return err
	}
	all := map[string]string{"REVERSE_PROXY_AUTH_SECRET": key}
	maps.Copy(all, settings)
	for _, k := range slices.Sorted(maps.Keys(all)) {
		if err := write(ctx, x, SettingsDir+"/"+k, all[k]); err != nil {
			return err
		}
	}
	return nil
}

// secret is the boot's secret: the one already written, or a new one.
func secret(x Executor) (string, error) {
	if raw, err := x.ReadFile(KeyPath); err == nil {
		if k := strings.TrimSpace(string(raw)); len(k) == 64 {
			return k, nil
		}
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("mealie: mint the secret: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// write installs one setting: written beside, owned by Mealie's uid and readable by it alone,
// renamed over, so a container that starts mid-Prepare never reads a half-written file.
func write(ctx context.Context, x Executor, path, value string) error {
	tmp := path + ".new"
	if err := x.WriteFile(tmp, []byte(value)); err != nil {
		return fmt.Errorf("mealie: write %s: %w", tmp, err)
	}
	for _, args := range [][]string{{"chown", uid + ":" + uid, tmp}, {"chmod", "0400", tmp}, {"mv", "-f", tmp, path}} {
		if out, err := x.Run(ctx, args[0], args[1:]...); err != nil {
			return fmt.Errorf("mealie: %s %s: %w: %s", args[0], path, err, strings.TrimSpace(string(out)))
		}
	}
	return nil
}
