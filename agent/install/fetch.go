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
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"

	"briard.io/agent/selfupdate"
)

// THE CHANNEL TREE ([B.86e]). The channel root serves one directory per release CHAIN, and under
// each chain one directory per version plus two POINTERS; the host chain adds one more level, the
// PLATFORM, because a host bundle is built per host OS while the guest image is the same VM on
// every host:
//
//	<root>/host/<version>/<platform>/   manifest.json(+.sig) and every artifact it names
//	<root>/host/stable/<platform>/      a byte-copy of one version's signed manifest (+ briard-agent)
//	<root>/host/latest/<platform>/      likewise
//	<root>/guest/<version>/             manifest.json(+.sig), nixos.qcow2.zst
//	<root>/guest/{stable,latest}/       manifest.json(+.sig)
//
// A pointer is just a path serving a copy of a version's signed manifest: no pointer file, no
// second signature format, one verified hop -- the signature verifies identically at any path,
// and the manifest's own `version` says which release you got. So a fetch names a TARGET
// (`stable`, `latest`, or an exact version id) to pick the manifest, and then resolves every
// artifact against the manifest's `version`, never against the path it was fetched from. That
// stops pointer paths from duplicating a 380 MB image, and it is an anti-substitution property: a
// signed manifest says where its own artifacts live. (`briard-agent` is the one artifact ALSO
// copied under the pointer paths, because install.sh has to curl a bootstrap before anything
// exists that can parse a manifest.)
//
// The chains are release LINES, not flavours of one release: the host bundle (agent, qemu,
// net-wrap) and the guest OS image move on different cadences and carry different version
// series. The manifest names its chain and platform so a crossed wire -- a guest manifest served
// where a host one was expected, a Windows bundle where the Linux one should be -- is refused by
// the signature's own content rather than compared by eye. The chain is a FIELD rather than
// something read off the version id's first token because that token is the epoch (`v3.`), which
// moves forward over time; a series check keyed on it would refuse every update across an epoch
// boundary, fleet-wide.
const (
	ChainHost  = "host"  // briard-agent, briard-net-wrap, qemu-bundle.tar.zst — per platform
	ChainGuest = "guest" // nixos.qcow2.zst — no platform level

	// PlatformLinux is the host platform this binary installs on; the Windows arm
	// (`windows`) is published beside it ([V3b.27](b)) with no consumer until v5.
	PlatformLinux = "linux"

	// TargetStable and TargetLatest are the two pointer paths every chain serves. Anything else
	// passed as a target is taken to be an exact version id.
	TargetStable = "stable"
	TargetLatest = "latest"

	// ManifestName is the signed index served at <root>/<chain>/<target>/manifest.json; its
	// detached signature is served alongside at manifest.json.sig.
	ManifestName = "manifest.json"
	sigSuffix    = ".sig"

	// MaxManifestSize bounds the index download. The manifest is a short JSON list; 1 MiB is
	// vast headroom while still a hard ceiling before the signature is even checked.
	maxManifestSize = 1 << 20
	// MaxArtifactSize is the absolute per-artifact ceiling, above the manifest's own Size (which
	// is the real bound). The guest image is ~2.5 GB; 8 GiB leaves room without being unbounded.
	maxArtifactSize = 8 << 30

	// CompressedSuffix marks an artifact shipped compressed and expanded here, after
	// verification, to its name minus the suffix. Measured: the guest image goes 1178 -> 377 MB
	// and the qemu bundle 86 -> 19 MB, which is what a household link actually waits on.
	//
	// Only the two big artifacts carry it. `briard-agent` deliberately does NOT: install.sh
	// fetches the bootstrap agent with plain curl/wget BEFORE any agent exists to decompress
	// anything, so a compressed agent would be an artifact the bootstrap cannot open. The one
	// file that has to be readable by a shell stays readable by a shell.
	compressedSuffix = ".zst"
)

// ErrNoKeyring is returned when no release keyring is wired. Fail CLOSED: with no trusted key
// there is no valid signature, so every artifact is refused (never fail-open to "no key means
// allow anything") — the same stance as selfupdate.ErrNoKeys.
var ErrNoKeyring = errors.New("install: no release keyring — every artifact refused")

// ErrManifest covers a malformed or empty manifest (verifies but describes nothing to fetch, or
// names no chain/version to resolve it against).
var ErrManifest = errors.New("install: manifest malformed or empty")

// ErrWrongChain is returned when a verified manifest belongs to a different chain or platform
// than the one asked for: the bytes are genuine, they are just not the release line this fetch
// is about.
var ErrWrongChain = errors.New("install: manifest is for a different chain or platform")

// ErrArtifactMismatch is returned when a downloaded artifact's SHA-256 or size does not match
// the signed manifest — a tampered or truncated blob, refused without being committed.
var ErrArtifactMismatch = errors.New("install: artifact does not match the signed manifest")

// Entry pins one artifact in the manifest: Name is both the URL path element
// (<root>/<chain>/<version>/<Name>) and the filename written under the staging dir; SHA256 is
// lowercase hex; Mode is the file mode (0 → 0644; the agent binary ships 0755).
type Entry struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
	Mode   uint32 `json:"mode,omitempty"`
}

// Manifest is the signed list of artifacts one release of one chain offers. Chain and Version
// sit INSIDE the signature: they are what lets a manifest fetched from a pointer path say which
// release it is and where its artifacts live, and an unsigned sidecar could say anything.
type Manifest struct {
	Chain     string  `json:"chain"`
	Platform  string  `json:"platform,omitempty"` // host chain only; the guest image has no platform
	Version   string  `json:"version"`
	Artifacts []Entry `json:"artifacts"`
}

// Fetcher downloads and verifies one chain's signed artifact set into a staging directory.
// BaseURL is the channel ROOT (the chains sit directly under it); Chain names the release line
// and Platform the arm within it ("" for a chain without the platform level); Keyring is the
// release trust root; Client/Logf are optional (nil → a bounded-timeout client / a discard log).
type Fetcher struct {
	BaseURL  string
	Chain    string
	Platform string
	Keyring  *selfupdate.Keyring
	Client   *http.Client
	Logf     func(string, ...any)
}

// FetchVerified downloads <root>/<chain>/<target>[/<platform>]/manifest.json, verifies it
// against the keyring and asserts it belongs to f.Chain and f.Platform, then downloads each
// listed artifact from <root>/<chain>/<manifest.version>[/<platform>]/ and checks its SHA-256 +
// size against the manifest. On full success the staging dir is populated -- the verified
// manifest bytes beside the artifacts, as manifest.json -- and atomically placed at dest; dest
// must not already exist. On ANY failure dest is left untouched and no partial staging survives
// (refuse-and-stay).
func (f *Fetcher) FetchVerified(ctx context.Context, target, dest string) error {
	logf := f.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if f.Keyring == nil || f.Keyring.Len() == 0 {
		return ErrNoKeyring // fail closed before touching the network
	}
	if !validSegment(f.Chain) {
		return fmt.Errorf("install: bad chain name %q", f.Chain)
	}
	if f.Platform != "" && !validSegment(f.Platform) {
		return fmt.Errorf("install: bad platform name %q", f.Platform)
	}
	if !validSegment(target) {
		return fmt.Errorf("install: bad release target %q", target)
	}
	if _, err := os.Stat(dest); err == nil {
		return fmt.Errorf("install: staging dest %s already exists", dest)
	}

	// 1. Fetch + verify the manifest — the trust root. Nothing else is trusted until this passes.
	at := path.Join(f.Chain, target, f.Platform) // Join drops an empty platform
	mBytes, err := f.get(ctx, path.Join(at, ManifestName), maxManifestSize)
	if err != nil {
		return fmt.Errorf("install: fetch manifest: %w", err)
	}
	sig, err := f.get(ctx, path.Join(at, ManifestName+sigSuffix), ed25519.SignatureSize+1)
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
	// The version is about to become a URL path element; a manifest that verifies but names
	// something unusable there is malformed, not merely surprising.
	if !validSegment(man.Version) {
		return fmt.Errorf("%w: version %q", ErrManifest, man.Version)
	}
	if man.Chain != f.Chain || man.Platform != f.Platform {
		return fmt.Errorf("%w: asked for %s, manifest says %q", ErrWrongChain, at, path.Join(man.Chain, man.Platform))
	}
	logf("install: %s manifest verified — release %s, %d artifact(s) to fetch", at, man.Version, len(man.Artifacts))

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
	from := path.Join(f.Chain, man.Version, f.Platform)
	for _, a := range man.Artifacts {
		if err := f.fetchArtifact(ctx, from, tmp, a); err != nil {
			return err // any mismatch/fetch error aborts the whole set; tmp is cleaned up
		}
		logf("install: verified %s (%d bytes)", a.Name, a.Size)
	}
	// The verified manifest travels with the set it describes: it is what an installed node
	// keeps beside its binaries to know which release it is on, and it is the exact bytes that
	// verified, not a re-serialisation.
	if err := os.WriteFile(filepath.Join(tmp, ManifestName), mBytes, 0o644); err != nil {
		return fmt.Errorf("install: keep manifest: %w", err)
	}

	// 3. All artifacts verified against the signed manifest — publish atomically.
	if err := os.Rename(tmp, dest); err != nil {
		return fmt.Errorf("install: place staging dir: %w", err)
	}
	committed = true
	return nil
}

// FetchArtifact streams <root>/<from>/<a.Name> into dir/<a.Name>, computing its SHA-256 as it
// goes and refusing the moment the byte count exceeds a.Size or the final hash disagrees with the
// signed manifest. The write is bounded by a.Size+1 so a longer-than-declared body is caught
// rather than silently truncated.
func (f *Fetcher) fetchArtifact(ctx context.Context, from, dir string, a Entry) error {
	if a.Size < 0 || a.Size > maxArtifactSize {
		return fmt.Errorf("%w: %s declares out-of-range size %d", ErrArtifactMismatch, a.Name, a.Size)
	}
	// Guard against a manifest name that escapes the staging dir (path traversal from a
	// compromised-but-somehow-signed manifest, or a plain mistake).
	clean := filepath.Clean(a.Name)
	if clean != a.Name || filepath.IsAbs(clean) || clean == ".." || filepath.Dir(clean) != "." || clean == ManifestName {
		return fmt.Errorf("%w: unsafe artifact name %q", ErrArtifactMismatch, a.Name)
	}

	resp, err := f.do(ctx, path.Join(from, a.Name))
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
	// ⚠️ EXPANSION HAPPENS HERE AND NOWHERE EARLIER. Everything above this line has established
	// that these bytes are the bytes the release signed; only now is it safe to hand them to a
	// decoder. Streaming the download THROUGH a decompressor would have been less code and would
	// have fed attacker-controlled input to a parser before any signature was checked — the
	// ordering, not the decompression, is the security-relevant part.
	if strings.HasSuffix(clean, compressedSuffix) {
		return expand(filepath.Join(dir, clean), mode)
	}
	return nil
}

// Expand decompresses src (a verified .zst) to src minus the suffix, then removes src. The
// staging dir therefore ends up holding exactly the filenames install.sh already expects
// (nixos.qcow2, qemu-bundle.tar), which is why shipping them compressed needed no change to
// install.sh at all.
//
// The output is bounded by maxArtifactSize even though these bytes are signed: the manifest pins
// the COMPRESSED size, so nothing in it constrains how far they expand, and a release that was
// mis-built (or a signing key that was misused) should hit a ceiling rather than fill the disk of
// a machine that has not finished installing yet. Defence in depth, one io.LimitReader.
func expand(src string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("install: open %s: %w", filepath.Base(src), err)
	}
	defer in.Close()
	zr, err := zstd.NewReader(in)
	if err != nil {
		return fmt.Errorf("install: zstd reader for %s: %w", filepath.Base(src), err)
	}
	defer zr.Close()

	dst := strings.TrimSuffix(src, compressedSuffix)
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return fmt.Errorf("install: create %s: %w", filepath.Base(dst), err)
	}
	n, err := io.Copy(out, io.LimitReader(zr.IOReadCloser(), maxArtifactSize+1))
	cerr := out.Close()
	if err != nil {
		return fmt.Errorf("install: decompress %s: %w", filepath.Base(src), err)
	}
	if cerr != nil {
		return fmt.Errorf("install: close %s: %w", filepath.Base(dst), cerr)
	}
	if n > maxArtifactSize {
		return fmt.Errorf("%w: %s expands past the %d-byte ceiling", ErrArtifactMismatch, filepath.Base(src), int64(maxArtifactSize))
	}
	// The compressed copy has served its purpose; leaving it would double the staging dir's
	// footprint on a disk the report card has already sized.
	if err := os.Remove(src); err != nil {
		return fmt.Errorf("install: remove %s: %w", filepath.Base(src), err)
	}
	return nil
}

// Get fetches <root>/<rel> and returns its body, refusing a non-200 or an over-limit
// response. Used for the small manifest + signature; artifacts stream via fetchArtifact.
func (f *Fetcher) get(ctx context.Context, rel string, limit int64) ([]byte, error) {
	resp, err := f.do(ctx, rel)
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
		return nil, fmt.Errorf("%s exceeds %d bytes", rel, limit)
	}
	return data, nil
}

// Do issues a bounded GET for <root>/<rel>. The caller owns resp.Body.
func (f *Fetcher) do(ctx context.Context, rel string) (*http.Response, error) {
	client := f.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Minute} // the guest image is large
	}
	u, err := joinURL(f.BaseURL, rel)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	return client.Do(req)
}

// joinURL appends the relative path rel to base, preserving base's scheme/host and any path
// prefix it carries (a channel served under a sub-path is still a channel).
func joinURL(base, rel string) (string, error) {
	if base == "" {
		return "", errors.New("install: empty BaseURL")
	}
	sep := "/"
	if base[len(base)-1] == '/' {
		sep = ""
	}
	return base + sep + path.Clean(rel), nil
}

// validSegment reports whether s can stand as ONE path element of the channel tree -- a chain
// name, a release target, a version id. The alphabet is what a version id is made of plus the
// two pointer words; anything that could climb, split or hide (`..`, a slash, a leading dot, a
// query character) is refused before it reaches a URL or a filesystem.
func validSegment(s string) bool {
	if s == "" || s[0] == '.' || len(s) > 128 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}
