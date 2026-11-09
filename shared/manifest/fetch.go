package manifest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"time"
)

const (
	// MaxManifestSize bounds the download before anything is parsed or verified. An app
	// manifest is a short JSON document; 1 MiB is vast headroom and still a hard ceiling.
	maxManifestSize = 1 << 20
	sigSuffix       = ".sig"
)

// ErrNoVerifier is returned when no trust root is wired. Fail CLOSED: with no key there is no
// such thing as a valid signature, so every manifest is refused — never fail-open to "no key
// configured means allow anything" (the stance selfupdate.ErrNoKeys and install.ErrNoKeyring
// already take, kept identical here on purpose).
var ErrNoVerifier = errors.New("manifest: no release keyring — every app manifest refused")

// Verifier checks a detached signature over exact bytes. *selfupdate.Keyring satisfies it, and
// that is the point: the catalog reuses the release trust root rather than introducing a
// second one. A narrow interface for dependency injection, not a new seam.
type Verifier interface {
	Verify(artifact, sig []byte) error
}

// Catalog fetches signed app manifests from the published catalog (a signed,
// versioned, static, mirrorable artifact at a dumb URL, the apt-mirror shape rather than an
// API). URL is the signed LIST a node reads (`…/catalog/latest.json`, served from briard.io);
// every app version a list names is a file beside it, `<app>/<version>.json`. Client is
// optional.
//
// ONLY THE LIST IS SIGNED. A list line carries the identity of its version file -- the sha256 of
// its exact bytes -- and a version file pins its images by digest, so one signature covers
// everything down to the image bytes, and every version ever published stays where it is,
// readable by whoever still needs it, without being signed again.
//
// Operation never depends on our liveness: a fetched manifest is kept on the DRBD volume as the
// app's identity, so an installed app keeps running, failing over and rolling back with
// the catalog unreachable. This type is only ever on the INSTALL path, never the failover path.
type Catalog struct {
	URL      string
	Verifier Verifier
	Client   *http.Client
}

// Fetch reads the signed list, takes the named app's line, downloads the version file the line
// names from beside the list and refuses it unless its sha256 is the line's identity. It returns
// the parsed manifest with the identity of those bytes.
//
// Order matters and is the whole safety property: the list's signature is verified over its RAW
// bytes before the list is parsed, and the version file's hash is checked against the signed
// line before the file is parsed. Parsing untrusted input is a larger attack surface than
// checking a signature or a hash over it, and the identity must be the hash of what was signed
// for -- not of anything a parse-then-verify flow might have already normalised.
// The RAW bytes come back alongside the parsed form because they are what gets recorded on the
// volume: the identity is the hash of exactly these bytes, so storing a re-marshalling would
// store a different app.
func (c *Catalog) Fetch(ctx context.Context, name string) (Manifest, Identity, []byte, error) {
	if c.Verifier == nil {
		return Manifest{}, "", nil, ErrNoVerifier // fail closed before touching the network
	}
	// The name becomes a URL path element; a catalog name is a slug for the same reason an
	// app name is, and checking here keeps "../" out of the request we build.
	if !slug.MatchString(name) {
		return Manifest{}, "", nil, fmt.Errorf("%w: catalog name %q is not a slug", ErrInvalid, name)
	}
	idx, _, err := c.FetchIndex(ctx)
	if err != nil {
		return Manifest{}, "", nil, err
	}
	i := slices.IndexFunc(idx.Apps, func(a App) bool { return a.Name == name })
	if i < 0 || idx.Apps[i].Version == "" {
		return Manifest{}, "", nil, fmt.Errorf("%w: the catalog lists no installable version of %q", ErrInvalid, name)
	}
	line := idx.Apps[i]
	doc := name + "/" + line.Version + ".json" // the version is a path element: ParseIndex checked it
	u, err := besideList(c.URL, doc)
	if err != nil {
		return Manifest{}, "", nil, err
	}
	raw, err := c.get(ctx, u)
	if err != nil {
		return Manifest{}, "", nil, fmt.Errorf("manifest: fetch %s: %w", doc, err)
	}
	if id := identityOf(raw); id != line.Identity {
		return Manifest{}, "", nil, fmt.Errorf("manifest: %s is %s; the signed list names %s", doc, id, line.Identity)
	}
	m, id, err := Parse(raw)
	if err != nil {
		return Manifest{}, "", nil, err
	}
	if m.Name != name || m.Version != line.Version {
		// A manifest served at one name that calls itself another would install an app under
		// a handle the operator never asked for — and, since the name is the subvolume, would
		// point it at another app's data. The hash binds the bytes to the line; this binds the
		// line to what the bytes say they are.
		return Manifest{}, "", nil, fmt.Errorf("%w: manifest at %q declares name %q version %q", ErrInvalid, doc, m.Name, m.Version)
	}
	return m, id, raw, nil
}

// get downloads one catalog document, bounded.
func (c *Catalog) get(ctx context.Context, u string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	client := c.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", u, resp.Status)
	}
	// Bound the read regardless of Content-Length: a hostile or broken server can lie about it,
	// and this runs before any signature has been checked.
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxManifestSize+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > maxManifestSize {
		return nil, fmt.Errorf("%s: larger than %d bytes", u, maxManifestSize)
	}
	return b, nil
}

// besideList resolves a document against the list's URL the way a relative link resolves
// against a page: `home-assistant/2026.9.4.json` beside `…/catalog/latest.json` is
// `…/catalog/home-assistant/2026.9.4.json`.
func besideList(list, name string) (string, error) {
	u, err := url.Parse(list)
	if err != nil {
		return "", fmt.Errorf("manifest: bad catalog URL %q: %w", list, err)
	}
	return u.ResolveReference(&url.URL{Path: name}).String(), nil
}
