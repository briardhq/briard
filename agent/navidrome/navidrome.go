// Package navidrome is the product-side knowledge Briard holds about Navidrome, the music server:
// what it is told on this node, how the reverse proxy signs the user into it, and which of its
// folders the household fills.
//
// Signed in by the reverse proxy, by header, trusting an address. Navidrome takes its user from a
// reverse proxy's header when the request's peer is in its list of trusted sources, and it has no
// secret to check beside that. The trusted source is the address the reverse proxy reaches the pod
// network from, one address and never the pool, so no other pod can sign in. An app on the guest's
// own network connects from that same address and can, which is accepted: it reaches a music
// server's admin. The first sign-in by header makes the user, an admin, with a generated password
// that user can replace without giving the current one; that password is what a phone's Subsonic
// client signs in with, on the one path the reverse proxy leaves open (Open).
//
// Its settings are a file, the one the catalog's manifest names in ND_CONFIGFILE, written here
// into the app's own directory, which the container sees read-only at /briard. The catalog's
// manifest stays what any Navidrome needs, and what briard leaves in Navidrome's own data is an
// ordinary Navidrome.
package navidrome

import (
	"context"
	"fmt"
	"strings"

	"briard.io/shared/manifest"
	"briard.io/shared/routes"
)

// Name is Navidrome's catalog slug. Everything in this package is keyed on it.
const Name = "navidrome"

// Dir is Navidrome's own directory on the guest, the registry's AppDir for it; /run, so every
// boot re-derives it. ConfigPath is the settings file in it.
const (
	Dir        = "/run/briard/navidrome"
	ConfigPath = Dir + "/navidrome.toml"
)

// Music is the files-tier folder Navidrome's library is read from and the household fills,
// through Files or the drive.
const Music = "music"

// Container is the catalog's name for Navidrome's one container, which the dashboard runs
// Navidrome's own tool in to read its accounts.
const Container = "app"

// Open are the paths the reverse proxy forwards without the dashboard's session: the Subsonic
// API, which a phone's client calls with the username and Navidrome's own password. Never /auth,
// where anyone could make the first admin of a Navidrome with no user yet, and never /share,
// which is off.
var Open = []string{"/rest/"}

// userHeader is the header the reverse proxy names the user in.
const userHeader = "X-Forwarded-User"

// logoutURL is where Navidrome's logout goes: the dashboard, since signing out of Navidrome alone
// would sign straight back in through the gate.
const logoutURL = "http://" + routes.BareFlockHostName + "/"

// Executor is the guest-side command runner Prepare needs.
type Executor interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
	WriteFile(path string, data []byte) error
}

// Config is Navidrome's settings on a node whose reverse proxy reaches the pod network from
// trusted. The telemetry Navidrome sends by default is off, and it has no switch for it inside
// the app. Sharing is off, because a share link is under /share/, which only the gate would
// guard, so the person it was sent to could not open it.
func Config(trusted string) string {
	return fmt.Sprintf(`# Written by Briard at every start of the app; an edit here is replaced.
EnableInsightsCollector = false
EnableSharing = false

[ExtAuth]
TrustedSources = "%s/32"
UserHeader = "%s"
LogoutURL = "%s"
`, trusted, userHeader, logoutURL)
}

// Prepare writes Navidrome's settings on this node, before the container starts. trusted is the
// address the reverse proxy reaches the pod network from. The file holds no secret; it is
// written beside and renamed over, so a container that starts mid-Prepare never reads half of it.
func Prepare(ctx context.Context, x Executor, m manifest.Manifest, trusted string) error {
	if m.Name != Name {
		return nil
	}
	if trusted == "" {
		return fmt.Errorf("navidrome: no address for the reverse proxy on the pod network")
	}
	if _, err := x.Run(ctx, "mkdir", "-p", Dir); err != nil {
		return fmt.Errorf("navidrome: %s: %w", Dir, err)
	}
	tmp := ConfigPath + ".new"
	if err := x.WriteFile(tmp, []byte(Config(trusted))); err != nil {
		return fmt.Errorf("navidrome: write %s: %w", tmp, err)
	}
	for _, args := range [][]string{{"chmod", "0644", tmp}, {"mv", "-f", tmp, ConfigPath}} {
		if out, err := x.Run(ctx, args[0], args[1:]...); err != nil {
			return fmt.Errorf("navidrome: %s %s: %w: %s", args[0], ConfigPath, err, strings.TrimSpace(string(out)))
		}
	}
	return nil
}
