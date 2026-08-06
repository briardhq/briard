// Package install is the free-local installer's signed-artifact fetch (assertion e):
// the network half of scripts/install.sh, replacing the BRIARD_ARTIFACTS local-staging path.
//
// It reuses the release keyring (selfupdate.Keyring) as the single verify-or-refuse
// trust root, but scales to the multi-GB guest image by signing a small MANIFEST rather than
// each artifact. The keyring verifies the manifest's detached Ed25519 signature (small, held
// in memory, the exact self-update primitive); the manifest then pins every artifact by SHA-256 +
// size, and each artifact is STREAMED to disk with its hash checked against the signed
// manifest — never the whole 2.5 GB image in memory. A signed list of hashes is the standard
// way to extend one signature over large blobs, and it keeps the trust root identical to the
// agent self-update path.
//
// The safety property is all-or-nothing, verify-before-commit: everything lands in a private
// temp dir, and only when the manifest verifies AND every artifact's hash/size matches is that
// dir atomically renamed into place. Any missing, tampered, unsigned, or wrong-size artifact —
// or an empty/absent keyring — aborts the whole fetch and leaves the destination untouched, so
// install.sh never consumes a half-verified staging dir (refuse-and-stay, the same doctrine as
// selfupdate's refuse-and-keep-current).
package install

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"time"

	"briard.io/agent/selfupdate"
)

const (
	// ManifestName is the signed index served at <BaseURL>/manifest.json; its detached
	// signature is served alongside at <BaseURL>/manifest.json.sig.
	ManifestName = "manifest.json"
	sigSuffix    = ".sig"

	// MaxManifestSize bounds the index download. The manifest is a short JSON list; 1 MiB is
	// vast headroom while still a hard ceiling before the signature is even checked.
	maxManifestSize = 1 << 20
	// MaxArtifactSize is the absolute per-artifact ceiling, above the manifest's own Size (which
	// is the real bound). The guest image is ~2.5 GB; 8 GiB leaves room without being unbounded.
	maxArtifactSize = 8 << 30
)

// ErrNoKeyring is returned when no release keyring is wired. Fail CLOSED: with no trusted key
// there is no valid signature, so every artifact is refused (never fail-open to "no key means
// allow anything") — the same stance as selfupdate.ErrNoKeys.
var ErrNoKeyring = errors.New("install: no release keyring — every artifact refused")

// ErrManifest covers a malformed or empty manifest (verifies but describes nothing to fetch).
var ErrManifest = errors.New("install: manifest malformed or empty")

// ErrArtifactMismatch is returned when a downloaded artifact's SHA-256 or size does not match
// the signed manifest — a tampered or truncated blob, refused without being committed.
var ErrArtifactMismatch = errors.New("install: artifact does not match the signed manifest")

// Entry pins one artifact in the manifest: Name is both the URL path element
// (<BaseURL>/<Name>) and the filename written under the staging dir; SHA256 is lowercase hex;
// Mode is the file mode (0 → 0644; the agent binary ships 0755).
type Entry struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
	Mode   uint32 `json:"mode,omitempty"`
}

// Manifest is the signed list of artifacts the release channel offers for this install.
type Manifest struct {
	Artifacts []Entry `json:"artifacts"`
}

// Fetcher downloads and verifies the signed artifact set into a staging directory. BaseURL is
// the release channel root (the manifest + artifacts sit directly under it); Keyring is the
// release trust root; Client/Logf are optional (nil → a bounded-timeout client / a discard log).
type Fetcher struct {
	BaseURL string
	Keyring *selfupdate.Keyring
	Client  *http.Client
	Logf    func(string, ...any)
}

// FetchVerified downloads the signed manifest, verifies it against the keyring, then downloads
// each listed artifact and checks its SHA-256 + size against the manifest. On full success the
// staging dir is populated and atomically placed at dest; dest must not already exist. On ANY
// failure dest is left untouched and no partial staging survives (refuse-and-stay).
func (f *Fetcher) FetchVerified(ctx context.Context, dest string) error {
	logf := f.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if f.Keyring == nil || f.Keyring.Len() == 0 {
		return ErrNoKeyring // fail closed before touching the network
	}
	if _, err := os.Stat(dest); err == nil {
		return fmt.Errorf("install: staging dest %s already exists", dest)
	}

	// 1. Fetch + verify the manifest — the trust root. Nothing else is trusted until this passes.
	mBytes, err := f.get(ctx, ManifestName, maxManifestSize)
	if err != nil {
		return fmt.Errorf("install: fetch manifest: %w", err)
	}
	sig, err := f.get(ctx, ManifestName+sigSuffix, ed25519.SignatureSize+1)
	if err != nil {
		return fmt.Errorf("install: fetch manifest signature: %w", err)
	}
	if err := f.Keyring.Verify(mBytes, sig); err != nil {
		return err // ErrUnsigned / ErrBadSignature / ErrNoKeys — refuse, touch nothing
	}
	var man Manifest
	if err := json.Unmarshal(mBytes, &man); err != nil {
		return fmt.Errorf("%w: %v", ErrManifest, err)
	}
	if len(man.Artifacts) == 0 {
		return ErrManifest
	}
	logf("install: manifest verified — %d artifact(s) to fetch", len(man.Artifacts))

	// 2. Stage into a private temp dir SIBLING of dest (same filesystem, so the final rename is
	// atomic). Removed on every return unless step 3 renames it away.
	tmp, err := os.MkdirTemp(filepath.Dir(dest), ".briard-stage-")
	if err != nil {
		return fmt.Errorf("install: staging dir: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			os.RemoveAll(tmp)
		}
	}()
	for _, a := range man.Artifacts {
		if err := f.fetchArtifact(ctx, tmp, a); err != nil {
			return err // any mismatch/fetch error aborts the whole set; tmp is cleaned up
		}
		logf("install: verified %s (%d bytes)", a.Name, a.Size)
	}

	// 3. All artifacts verified against the signed manifest — publish atomically.
	if err := os.Rename(tmp, dest); err != nil {
		return fmt.Errorf("install: place staging dir: %w", err)
	}
	committed = true
	return nil
}

// FetchArtifact streams <BaseURL>/<a.Name> into dir/<a.Name>, computing its SHA-256 as it goes
// and refusing the moment the byte count exceeds a.Size or the final hash disagrees with the
// signed manifest. The write is bounded by a.Size+1 so a longer-than-declared body is caught
// rather than silently truncated.
func (f *Fetcher) fetchArtifact(ctx context.Context, dir string, a Entry) error {
	if a.Size < 0 || a.Size > maxArtifactSize {
		return fmt.Errorf("%w: %s declares out-of-range size %d", ErrArtifactMismatch, a.Name, a.Size)
	}
	// Guard against a manifest name that escapes the staging dir (path traversal from a
	// compromised-but-somehow-signed manifest, or a plain mistake).
	clean := filepath.Clean(a.Name)
	if clean != a.Name || filepath.IsAbs(clean) || clean == ".." || filepath.Dir(clean) != "." {
		return fmt.Errorf("%w: unsafe artifact name %q", ErrArtifactMismatch, a.Name)
	}

	resp, err := f.do(ctx, a.Name)
	if err != nil {
		return fmt.Errorf("install: fetch %s: %w", a.Name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("install: fetch %s: status %s", a.Name, resp.Status)
	}

	mode := os.FileMode(0o644)
	if a.Mode != 0 {
		mode = os.FileMode(a.Mode)
	}
	out, err := os.OpenFile(filepath.Join(dir, clean), os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return fmt.Errorf("install: create %s: %w", a.Name, err)
	}
	h := sha256.New()
	// +1 so a body of exactly Size+1 trips the ceiling instead of being accepted-then-hash-fail.
	n, err := io.Copy(io.MultiWriter(out, h), io.LimitReader(resp.Body, a.Size+1))
	cerr := out.Close()
	if err != nil {
		return fmt.Errorf("install: download %s: %w", a.Name, err)
	}
	if cerr != nil {
		return fmt.Errorf("install: close %s: %w", a.Name, cerr)
	}
	if n != a.Size {
		return fmt.Errorf("%w: %s is %d bytes, manifest says %d", ErrArtifactMismatch, a.Name, n, a.Size)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != a.SHA256 {
		return fmt.Errorf("%w: %s sha256 %s != manifest %s", ErrArtifactMismatch, a.Name, got, a.SHA256)
	}
	return nil
}

// Get fetches <BaseURL>/<name> and returns its body, refusing a non-200 or an over-limit
// response. Used for the small manifest + signature; artifacts stream via fetchArtifact.
func (f *Fetcher) get(ctx context.Context, name string, limit int64) ([]byte, error) {
	resp, err := f.do(ctx, name)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %s", resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%s exceeds %d bytes", name, limit)
	}
	return data, nil
}

// Do issues a bounded GET for <BaseURL>/<name>. The caller owns resp.Body.
func (f *Fetcher) do(ctx context.Context, name string) (*http.Response, error) {
	client := f.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Minute} // the guest image is large
	}
	u, err := joinURL(f.BaseURL, name)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	return client.Do(req)
}

// joinURL appends name as a single path element to base, preserving base's scheme/host.
func joinURL(base, name string) (string, error) {
	if base == "" {
		return "", errors.New("install: empty BaseURL")
	}
	sep := "/"
	if base[len(base)-1] == '/' {
		sep = ""
	}
	return base + sep + path.Clean(name), nil
}
