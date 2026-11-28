// Package freshrss is the product-side knowledge Briard holds about FreshRSS, the feed reader:
// what it is told on this node, and how the reverse proxy signs the user into it.
//
// Signed in by the reverse proxy, by header, trusting an address, as Navidrome is. FreshRSS's
// HTTP sign-in takes the user from Remote-User when the connection's own address is in
// TRUSTED_PROXY, and it has no secret to check beside that. The trusted address is the one the
// reverse proxy reaches the pod network from, one address and never the pool, so no other pod can
// sign in. An app on the guest's own network connects from that same address and can, which is
// accepted: it reaches a feed reader.
//
// FreshRSS reads its user from Remote-User or X-WebAuth-User and from no header of our choosing,
// and its trusted address is per node, which the catalog's static env cannot carry. Both are said
// in one file, written here into the app's own directory and bound where the image's Apache
// includes it: p/i/.htaccess, the place FreshRSS's own documentation gives for access control.
// The file copies the reverse proxy's X-Forwarded-User into Remote-User, drops X-WebAuth-User (a
// second user header signs nobody in, so a client's own could only lock the household out), and
// sets TRUSTED_PROXY for the request, which mod_php's getenv reads. It covers the pages under /i/
// and nothing else; the API under /api/ never reads a user header.
//
// The user is the installer's, made from the catalog's static env as FreshRSS's default user,
// which it counts as an admin. A phone's reader signs in with that user's API password, on the one
// path the reverse proxy leaves open (Open).
package freshrss

import (
	"context"
	"fmt"

	"briard.io/agent/appdir"
	"briard.io/shared/manifest"
)

// Name is FreshRSS's catalog slug. Everything in this package is keyed on it.
const Name = "freshrss"

// Dir is FreshRSS's own directory on the guest, the registry's AppDir for it; /run, so every boot
// re-derives it. ConfigPath is the Apache file in it.
const (
	Dir        = "/run/briard/freshrss"
	ConfigPath = Dir + "/freshrss.conf"
)

// Root is where the image keeps FreshRSS. Hook is the file its Apache includes for the /i/ pages
// when one exists; Data is the app's state, which the catalog's manifest mounts from the data tier.
const (
	Root = "/var/www/FreshRSS"
	Hook = Root + "/p/i/.htaccess"
	Data = Root + "/data"
)

// Container is the catalog's name for FreshRSS's one container, which the dashboard runs
// FreshRSS's own tools in. User is who they run as: the web server's user, as FreshRSS's image
// documents for its Alpine flavour, so what they write stays writable by FreshRSS.
const (
	Container = "app"
	User      = "apache"
)

// Open are the paths the reverse proxy forwards without the dashboard's session: the Google Reader
// API, which a phone's reader calls with the username and the user's API password. Never the
// pages, nor the rest of /api/: the Fever API, a user query shared by its token, WebSub.
var Open = []string{"/api/greader.php/"}

// userHeader is the header the reverse proxy names the user in.
const userHeader = "X-Forwarded-User"

// Executor is the guest-side command runner Prepare needs.
type Executor interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
	WriteFile(path string, data []byte) error
}

// Volumes binds the Apache file over the image's hook, read-only.
//
// ⚠️ A file bound on its own keeps the inode it had when the container started, so Prepare's
// rename over it reaches FreshRSS at the container's next start and not before, as FreshRSS's
// image says of any change to its .htaccess files. What it says changes only with the pod
// network, which restarts the app's containers.
func Volumes(m manifest.Manifest, c manifest.Container) []string {
	if m.Name != Name || !c.Primary {
		return nil
	}
	return []string{ConfigPath + ":" + Hook + ":ro"}
}

// Config is the Apache file on a node whose reverse proxy reaches the pod network from trusted.
func Config(trusted string) string {
	return fmt.Sprintf(`# Written by Briard at every start of the app; an edit here is replaced.
SetEnv TRUSTED_PROXY %s/32
RequestHeader unset X-WebAuth-User
RequestHeader set Remote-User "expr=%%{req:%s}"
`, trusted, userHeader)
}

// Prepare writes the Apache file on this node, before the container starts. trusted is the address
// the reverse proxy reaches the pod network from. The file holds no secret; it is written beside
// and renamed over, so a container that starts mid-Prepare never reads half of it.
func Prepare(ctx context.Context, x Executor, m manifest.Manifest, trusted string) error {
	if m.Name != Name {
		return nil
	}
	if trusted == "" {
		return fmt.Errorf("freshrss: no address for the reverse proxy on the pod network")
	}
	if _, err := x.Run(ctx, "mkdir", "-p", Dir); err != nil {
		return fmt.Errorf("freshrss: %s: %w", Dir, err)
	}
	if err := appdir.Write(ctx, x, ConfigPath, []byte(Config(trusted)), "", "0644"); err != nil {
		return fmt.Errorf("freshrss: %w", err)
	}
	return nil
}
