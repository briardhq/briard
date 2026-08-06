package manifest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"time"
)

const (
	// MaxManifestSize bounds the download before anything is parsed or verified. A service
	// manifest is a short JSON document; 1 MiB is vast headroom and still a hard ceiling.
	maxManifestSize = 1 << 20
	sigSuffix       = ".sig"
)

// ErrNoVerifier is returned when no trust root is wired. Fail CLOSED: with no key there is no
// such thing as a valid signature, so every manifest is refused — never fail-open to "no key
// configured means allow anything" (the stance selfupdate.ErrNoKeys and install.ErrNoKeyring
// already take, kept identical here on purpose).
var ErrNoVerifier = errors.New("manifest: no release keyring — every service manifest refused")

// Verifier checks a detached signature over exact bytes. *selfupdate.Keyring satisfies it, and
// that is the point: the catalog reuses the release trust root rather than introducing a
// second one. A narrow interface for dependency injection, not a new seam.
type Verifier interface {
	Verify(artifact, sig []byte) error
}

// Catalog fetches signed service manifests from the published catalog (a signed,
// versioned, static, mirrorable artifact at a dumb URL, the apt-mirror shape rather than an
// API). BaseURL is the catalog root, served from briard.io; Client is optional.
//
// Operation never depends on our liveness: a fetched manifest is kept on the DRBD volume as the
// service's identity, so an installed service keeps running, failing over and rolling back with
// the catalog unreachable. This type is only ever on the INSTALL path, never the failover path.
type Catalog struct {
	BaseURL  string
	Verifier Verifier
	Client   *http.Client
}

// Fetch downloads <BaseURL>/<name>.json and its detached signature at <BaseURL>/<name>.json.sig,
// verifies the signature over the exact bytes received, and returns the parsed manifest with the
// identity of those bytes.
//
// Order matters and is the whole safety property: verify the signature over the RAW bytes first,
// parse second. Parsing untrusted input is a larger attack surface than checking 64 bytes of
// Ed25519 over it, and the identity must be the hash of what was signed — not of anything a
// parse-then-verify flow might have already normalised.
// The RAW bytes come back alongside the parsed form because they are what gets recorded on the
// volume: the identity is the hash of exactly these bytes, so storing a re-marshalling would
// store a different service.
func (c *Catalog) Fetch(ctx context.Context, name string) (Manifest, Identity, []byte, error) {
	if c.Verifier == nil {
		return Manifest{}, "", nil, ErrNoVerifier // fail closed before touching the network
	}
	// The name becomes a URL path element; a catalog name is a slug for the same reason a
	// service name is, and checking here keeps "../" out of the request we build.
	if !slug.MatchString(name) {
		return Manifest{}, "", nil, fmt.Errorf("%w: catalog name %q is not a slug", ErrInvalid, name)
	}
	doc := name + ".json"
	raw, err := c.get(ctx, doc)
	if err != nil {
		return Manifest{}, "", nil, fmt.Errorf("manifest: fetch %s: %w", doc, err)
	}
	sig, err := c.get(ctx, doc+sigSuffix)
	if err != nil {
		return Manifest{}, "", nil, fmt.Errorf("manifest: fetch %s%s: %w", doc, sigSuffix, err)
	}
	if err := c.Verifier.Verify(raw, sig); err != nil {
		return Manifest{}, "", nil, fmt.Errorf("manifest: %s: %w", doc, err)
	}
	m, id, err := Parse(raw)
	if err != nil {
		return Manifest{}, "", nil, err
	}
	if m.Name != name {
		// A manifest served at one name that calls itself another would install a service under
		// a handle the operator never asked for — and, since the name is the subvolume, would
		// point it at another service's data.
		return Manifest{}, "", nil, fmt.Errorf("%w: manifest at %q declares name %q", ErrInvalid, doc, m.Name)
	}
	return m, id, raw, nil
}

func (c *Catalog) get(ctx context.Context, name string) ([]byte, error) {
	u, err := joinURL(c.BaseURL, name)
	if err != nil {
		return nil, err
	}
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

func joinURL(base, name string) (string, error) {
	if base == "" {
		return "", errors.New("manifest: no catalog URL configured")
	}
	u, err := url.Parse(base)
	if err != nil {
		return "", fmt.Errorf("manifest: bad catalog URL %q: %w", base, err)
	}
	u.Path = path.Join(u.Path, name)
	return u.String(), nil
}
