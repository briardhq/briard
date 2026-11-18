// Package install is the signed-artifact fetch from the release channel: the network half of
// `briard-agent install` (the alternative to a local BRIARD_ARTIFACTS dir), and the agent's own
// update (Update).
//
// The release keyring (selfupdate.Keyring) is the single verify-or-refuse trust root. To scale to
// the large guest image, a release signs one small manifest, not each artifact (the standard way
// to extend one signature over large blobs): the keyring verifies its detached Ed25519 signature
// in memory, the manifest pins every artifact by SHA-256 + size, and each artifact streams to disk
// hash-checked, never the whole image in memory. All-or-nothing, verify-before-commit: everything
// lands in a private temp dir, atomically renamed into place only once the manifest verifies and
// every artifact's hash and size match. Any missing, tampered, unsigned or wrong-size artifact, or
// an empty/absent keyring, aborts the whole fetch with the destination untouched, so the install
// never consumes a half-verified staging dir (refuse-and-stay, the same doctrine as selfupdate's
// refuse-and-keep-current).
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
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"

	"briard.io/agent/selfupdate"
)

// The channel tree: the root serves one directory per release chain, under each chain one per
// version plus the pointers, and under those one per platform. The briard bundle is built per host
// OS and CPU (`linux-amd64`); the VM image per CPU alone (`amd64`), being the same VM on every
// host OS but running on the host's CPU:
//
//	<root>/briard/<version>/<platform>/ manifest.json(+.sig) and every artifact it names
//	<root>/briard/stable/<platform>/    a byte-copy of one version's signed manifest (+ briard-agent)
//	<root>/briard/latest/<platform>/    likewise
//	<root>/vm/<version>/<cpu>/          manifest.json(+.sig), nixos.qcow2.zst
//	<root>/vm/{stable,latest}/<cpu>/    manifest.json(+.sig)
//
// Chains are named for the upgrade unit, not the side: `briard` is every briard artifact wherever
// it lands (agent, net-wrap, qemu bundle, guest bundle, frozen scripts, units, install.sh), `vm`
// the OS image and the closure it boots. `host` and `guest` are runtime locations (which journal,
// which agent, which side of the virtio port) and name no release line: our binaries run on both
// sides, so a side-named chain lies about half of what it carries, and a word doing both jobs
// leaves every refusal message speaking two vocabularies.
//
// A pointer is just a path serving a copy of a version's signed manifest: no pointer file, no
// second signature format, one verified hop (the signature verifies identically at any path, and
// the manifest's own `version` says which release you got). A fetch picks the manifest by target
// (`stable`, `latest`, an exact version id) but resolves every artifact against that `version`,
// never the path, so pointers never duplicate a 380 MB image and a signed manifest says where its
// own artifacts live (anti-substitution). Only `briard-agent` (`.exe` on Windows) is also copied
// under the pointers, because install.sh must curl a bootstrap before anything can parse a
// manifest.
//
// The chains are release lines, not flavours of one release: different cadences, different
// version series. The manifest names its chain and platform so a crossed wire (a vm manifest where
// a briard one was expected, a Windows bundle where the Linux one should be) is refused by the
// signed content, not compared by eye. The chain is a field rather than the version id's first
// token because that token is the epoch (`v5.`), which moves forward: a series check keyed on it
// would refuse every update across an epoch boundary, fleet-wide.
const (
	ChainBriard = "briard" // briard-agent, briard-net-wrap, qemu-bundle.tar.zst, guest-bundle.tar.zst — per OS and CPU
	ChainVM     = "vm"     // nixos.qcow2.zst — per CPU

	// PlatformHost is the briard arm this binary installs, named for the OS and CPU it was
	// built for: `linux-amd64`, or `windows-amd64` for the Windows agent.
	// PlatformVM is the vm arm it boots -- the guest runs on the host's CPU, so the same CPU.
	PlatformHost = runtime.GOOS + "-" + runtime.GOARCH
	PlatformVM   = runtime.GOARCH

	// TargetStable, TargetLatest and TargetDev are the three pointer paths every chain serves
	// (IsPointer). Anything else passed as a target is taken to be an exact version id. `dev` is
	// the ungated one: whatever the publisher last cut from HEAD, for a test node and nobody
	// else -- it moves without a gate and its previous release is deleted when it does.
	TargetStable = "stable"
	TargetLatest = "latest"
	TargetDev    = "dev"

	// ManifestName is the signed index served at <root>/<chain>/<target>/manifest.json; its
	// detached signature is served alongside at manifest.json.sig.
	ManifestName = "manifest.json"
	sigSuffix    = ".sig"

	// maxManifestSize bounds the index download. The manifest is a short JSON list; 1 MiB is
	// vast headroom while still a hard ceiling before the signature is even checked.
	maxManifestSize = 1 << 20
	// maxArtifactSize is the absolute per-artifact ceiling, above the manifest's own Size (which
	// is the real bound). The guest image expands to ~1.2 GB; 8 GiB leaves room without being
	// unbounded.
	maxArtifactSize = 8 << 30

	// compressedSuffix marks an artifact shipped compressed and expanded here, after
	// verification, to its name minus the suffix. Measured: the guest image goes 1178 -> 377 MB
	// and the qemu bundle 86 -> 19 MB, which is what a household link actually waits on. Only the
	// two big artifacts carry it; `briard-agent` deliberately does not, because install.sh fetches
	// the bootstrap agent with plain curl/wget before any agent exists to decompress anything, so
	// the one file a shell must read stays readable by a shell.
	compressedSuffix = ".zst"
)

// ErrNoKeyring is returned when no release keyring is wired. Fail closed: with no trusted key
// there is no valid signature, so every artifact is refused (never fail-open to "no key means
// allow anything"), the same stance as selfupdate.ErrNoKeys.
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

// UnreachablePrefix opens the update's verdict line when the run failed for want of the channel
// rather than for anything the channel said: the agent that finishes an install retries that
// one in minutes and every other failure in an hour.
const UnreachablePrefix = "could not reach the channel: "

// Unreachable reports whether err is the network's -- a request that got no answer (no route, no
// name, refused, timed out) or a body cut short -- and not a refusal of what was fetched. By the
// error's type, never net.Error: every deadline is one, and a candidate that timed out starting
// is no network failure.
func Unreachable(err error) bool {
	var ue *url.Error
	var oe *net.OpError
	return errors.As(err, &ue) || errors.As(err, &oe) || errors.Is(err, io.ErrUnexpectedEOF)
}

// Entry pins one artifact in the manifest: Name is both the URL path element
// (<root>/<chain>/<version>/<platform>/<Name>) and the filename written under the staging dir;
// SHA256 is lowercase hex; Mode is the file mode (0 → 0644; the agent binary ships 0755).
type Entry struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
	Mode   uint32 `json:"mode,omitempty"`
}

// Manifest is the signed list of artifacts one release of one chain offers. Chain and Version
// sit inside the signature: they are what lets a manifest fetched from a pointer path say which
// release it is and where its artifacts live, and an unsigned sidecar could say anything.
type Manifest struct {
	Chain     string  `json:"chain"`
	Platform  string  `json:"platform,omitempty"` // PlatformHost on the briard chain, PlatformVM on the vm chain
	Version   string  `json:"version"`
	Artifacts []Entry `json:"artifacts"`
	// The vm chain names the closure, not just the image: System is the store path of the NixOS
	// toplevel the image boots, compared by the vm resolver with the guest's running closure and by
	// an image upgrade with the new boot. MinBriard is the oldest briard release this VM tolerates:
	// briard must tolerate old VMs, the VM need never tolerate old briards, and this closes the one
	// direction that can go wrong (an exact-pinned old briard resolving vm/stable). Both are empty
	// on the briard chain.
	System    string `json:"system,omitempty"`
	MinBriard string `json:"min_briard,omitempty"`
	// VM is the pair, named from the briard side: the vm release this one was published beside,
	// which an installer fetches after the briard chain and `promote` moves vm/stable to. The vm id
	// is not derived from the commit (the image is a function of its inputs, re-published only
	// when they change), so only the briard manifest can hold the pairing. Empty on the vm chain.
	VM string `json:"vm,omitempty"`
	// Inputs is the VM image's input hash (sha256 hex, flake.nix guestInputs): the fact that
	// decides whether a stage re-publishes the vm chain or reuses the release that already
	// serves these exact inputs. Empty on the briard chain.
	Inputs string `json:"inputs,omitempty"`
	// MinUpgradeFrom is the upgrade floor: the oldest installed release this one can be installed
	// over; empty (the normal state) means none. MinBriard points from the VM at briard (inner
	// declaring a minimum on the outer, so briard upgrades first), this from a release at its own
	// past; those are the only two directions a floor may point. One from briard at the VM is
	// forbidden: a briard needing a newer image upgrades the image rather than waiting for it.
	// Written from install.MinUpgradeFrom, so the release carries its own floor, not one it is told.
	MinUpgradeFrom string `json:"min_upgrade_from,omitempty"`
}

// Fetcher downloads and verifies one chain's signed artifact set into a staging directory.
// BaseURL is the channel root (the chains sit directly under it); Chain names the release line
// and Platform the arm within it (every chain has one); Keyring is the
// release trust root; Client/Logf are optional (nil → a bounded-timeout client / a discard log).
type Fetcher struct {
	BaseURL  string
	Chain    string
	Platform string
	Keyring  *selfupdate.Keyring
	Client   *http.Client
	Logf     func(string, ...any)
}

// FetchVerified downloads <root>/<chain>/<target>/<platform>/manifest.json, verifies it
// against the keyring and asserts it belongs to f.Chain and f.Platform, then downloads each
// listed artifact from <root>/<chain>/<manifest.version>/<platform>/ and checks its SHA-256 +
// size against the manifest. On full success the staging dir (the artifacts plus the verified
// manifest bytes as manifest.json) is atomically placed at dest, which must not already exist. On
// any failure dest is left untouched and no partial staging survives (refuse-and-stay).
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
	if !validSegment(f.Platform) {
		return fmt.Errorf("install: bad platform name %q", f.Platform)
	}
	if !validSegment(target) {
		return fmt.Errorf("install: bad release target %q", target)
	}
	if _, err := os.Stat(dest); err == nil {
		return fmt.Errorf("install: staging dest %s already exists", dest)
	}

	// 1. Fetch + verify the manifest — the trust root. Nothing else is trusted until this passes.
	man, mBytes, _, err := f.fetchManifest(ctx, target)
	if err != nil {
		return err
	}
	logf("install: %s/%s manifest verified — release %s, %d artifact(s) to fetch", f.Chain, target, man.Version, len(man.Artifacts))

	// 2. Stage into a private temp dir sibling of dest (same filesystem, so the final rename is
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

	// 3. All verified: publish atomically, deliberately not durably. The install (host.Install)
	// copies this staging dir out, deletes it and flushes what it laid down; flushing here too
	// would write the largest thing we ship to disk twice for a copy nobody keeps. A caller that
	// keeps what this places must flush it itself (atomicfile.SyncTree).
	if err := os.Rename(tmp, dest); err != nil {
		return fmt.Errorf("install: place staging dir: %w", err)
	}
	committed = true
	return nil
}

// Manifest resolves target on the fetcher's chain: the verified manifest and the exact bytes
// that verified, and nothing on disk -- for a caller that decides before it fetches (the guest
// chain's resolver, the Windows setup program), then fetches what it chose through Artifact.
func (f *Fetcher) Manifest(ctx context.Context, target string) (Manifest, []byte, error) {
	man, mBytes, _, err := f.fetchManifest(ctx, target)
	return man, mBytes, err
}

// fetchManifest downloads <root>/<chain>/<target>/<platform>/manifest.json and its detached
// signature and checks them (checkManifest), returning the parsed manifest, the exact bytes that
// verified and the signature. Nothing else is trusted until this passes; nothing touches disk
// here at all.
func (f *Fetcher) fetchManifest(ctx context.Context, target string) (Manifest, []byte, []byte, error) {
	var man Manifest
	if f.Keyring == nil || f.Keyring.Len() == 0 {
		return man, nil, nil, ErrNoKeyring // fail closed before touching the network
	}
	if !validSegment(f.Chain) {
		return man, nil, nil, fmt.Errorf("install: bad chain name %q", f.Chain)
	}
	if !validSegment(f.Platform) {
		return man, nil, nil, fmt.Errorf("install: bad platform name %q", f.Platform)
	}
	if !validSegment(target) {
		return man, nil, nil, fmt.Errorf("install: bad release target %q", target)
	}
	at := path.Join(f.Chain, target, f.Platform)
	mBytes, err := f.get(ctx, path.Join(at, ManifestName), maxManifestSize)
	if err != nil {
		return man, nil, nil, fmt.Errorf("install: fetch manifest: %w", err)
	}
	sig, err := f.get(ctx, path.Join(at, ManifestName+sigSuffix), ed25519.SignatureSize+1)
	if err != nil {
		return man, nil, nil, fmt.Errorf("install: fetch manifest signature: %w", err)
	}
	man, err = checkManifest(f.Keyring, f.Chain, f.Platform, mBytes, sig)
	if err != nil {
		return man, nil, nil, err
	}
	return man, mBytes, sig, nil
}

// checkManifest is the one rule for a manifest: its signature verifies against the keyring, it
// parses, it lists artifacts, it names a usable version, and it belongs to the chain and platform
// asked for. The fetch and VerifyAgent both apply it, so the agent that runs an update and the
// check the update launcher asks for cannot disagree about what a release is.
func checkManifest(kr *selfupdate.Keyring, chain, platform string, mBytes, sig []byte) (Manifest, error) {
	var man Manifest
	if kr == nil || kr.Len() == 0 {
		return man, ErrNoKeyring
	}
	if err := kr.Verify(mBytes, sig); err != nil {
		return man, err // ErrUnsigned / ErrBadSignature / ErrNoKeys — refuse, touch nothing
	}
	if err := json.Unmarshal(mBytes, &man); err != nil {
		return man, fmt.Errorf("%w: %v", ErrManifest, err)
	}
	if len(man.Artifacts) == 0 {
		return man, ErrManifest
	}
	// The version is about to become a URL path element; a manifest that verifies but names
	// something unusable there is malformed, not merely surprising.
	if !validSegment(man.Version) {
		return man, fmt.Errorf("%w: version %q", ErrManifest, man.Version)
	}
	if man.Chain != chain || man.Platform != platform {
		return man, fmt.Errorf("%w: asked for %s, manifest says %q", ErrWrongChain, path.Join(chain, platform), path.Join(man.Chain, man.Platform))
	}
	return man, nil
}

// VerifyAgent is the check the update launcher runs on the committed agent before letting the
// agent it just downloaded run as root. dir holds the target's manifest.json and .sig as the
// channel served them plus the downloaded agent under its artifact name; it passes only for a
// signed release of this chain and platform whose agent bytes are the ones the manifest names.
// Without it, whoever can write to the channel could run code on every node at its next update,
// with no signing key at all. A release's own update runs it from its candidate before the swap,
// so a release whose check cannot pass its own release never becomes the committed agent every
// later update depends on.
func VerifyAgent(kr *selfupdate.Keyring, dir string) error {
	mBytes, err := readBounded(filepath.Join(dir, ManifestName), maxManifestSize)
	if err != nil {
		return err
	}
	sig, err := readBounded(filepath.Join(dir, ManifestName+sigSuffix), ed25519.SignatureSize+1)
	if err != nil {
		return err
	}
	man, err := checkManifest(kr, ChainBriard, PlatformHost, mBytes, sig)
	if err != nil {
		return err
	}
	for _, a := range man.Artifacts {
		if a.Name != artifactAgent {
			continue
		}
		in, err := os.Open(filepath.Join(dir, artifactAgent))
		if err != nil {
			return fmt.Errorf("install: open the agent to verify: %w", err)
		}
		defer in.Close()
		h := sha256.New()
		n, err := io.Copy(h, io.LimitReader(in, a.Size+1))
		if err != nil {
			return fmt.Errorf("install: read the agent to verify: %w", err)
		}
		if got := hex.EncodeToString(h.Sum(nil)); n != a.Size || got != a.SHA256 {
			return fmt.Errorf("%w: %s (%d bytes, sha256 %s) is not the one %s's signed manifest names", ErrArtifactMismatch, artifactAgent, n, got, man.Version)
		}
		return nil
	}
	return fmt.Errorf("%w: release %s ships no %s", ErrManifest, man.Version, artifactAgent)
}

// readBounded reads a file the way the fetch reads a response: refused past limit bytes.
func readBounded(name string, limit int64) ([]byte, error) {
	in, err := os.Open(name)
	if err != nil {
		return nil, fmt.Errorf("install: %w", err)
	}
	defer in.Close()
	b, err := io.ReadAll(io.LimitReader(in, limit+1))
	if err != nil {
		return nil, fmt.Errorf("install: read %s: %w", filepath.Base(name), err)
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("install: %s is larger than %d bytes", filepath.Base(name), limit)
	}
	return b, nil
}

// fetchArtifact streams <root>/<from>/<a.Name> into dir/<a.Name>, hashing as it goes, and refuses
// once the byte count exceeds a.Size or the final hash disagrees with the signed manifest.
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
	// Bounded at Size+1 so a longer-than-declared body trips the size check, neither silently
	// truncated nor accepted and then failed on the hash.
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
	// ⚠️ Expansion happens here and nowhere earlier: only now that these bytes are proven to be the
	// ones the release signed is it safe to hand them to a decoder. Streaming the download through
	// a decompressor would be less code and would feed attacker-controlled input to a parser before
	// any signature was checked; the ordering, not the decompression, is the security-relevant part.
	if strings.HasSuffix(clean, compressedSuffix) {
		return expand(filepath.Join(dir, clean), mode)
	}
	return nil
}

// expand decompresses src (a verified .zst) to src minus the suffix, then removes src, so the
// staging dir holds the filenames the install reads (nixos.qcow2, qemu-bundle.tar). The output is
// bounded by maxArtifactSize even though the bytes are signed: the manifest pins the compressed
// size, which does not constrain expansion, and a mis-built release (or a misused signing key)
// should hit a ceiling rather than fill the disk of a machine that has not finished installing.
// Defence in depth, one io.LimitReader.
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
	// Removed, since keeping the compressed copy would double the staging dir's footprint on a disk
	// the report card has already sized. Closed first: Windows refuses to delete a file a handle
	// is still open on (a sharing violation), where Linux unlinks the name and lets the handle
	// live. Both Close calls are idempotent, so the defers above stay for the error paths.
	zr.Close()
	in.Close()
	if err := os.Remove(src); err != nil {
		return fmt.Errorf("install: remove %s: %w", filepath.Base(src), err)
	}
	return nil
}

// get fetches <root>/<rel> and returns its body, refusing a non-200 or an over-limit
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

// do issues a bounded GET for <root>/<rel>. The caller owns resp.Body.
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

// validSegment reports whether s can stand as one path element of the channel tree -- a chain
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

// Artifact fetches one entry of version's release into dir, verified against the manifest that
// named it -- the guest chain's image, for the host to stage beside the one in use, or the
// Windows setup program's install script.
func (f *Fetcher) Artifact(ctx context.Context, version, dir string, a Entry) error {
	if !validSegment(version) {
		return fmt.Errorf("install: bad release version %q", version)
	}
	return f.fetchArtifact(ctx, path.Join(f.Chain, version, f.Platform), dir, a)
}

// IsPointer says whether a target names a pointer path rather than an exact release id. The one
// place the three names are enumerated: a version that spells a pointer is refused by
// WriteManifest (the tree could not say which it is), and the update path floors only an exact id
// against stable.
func IsPointer(target string) bool {
	return target == TargetStable || target == TargetLatest || target == TargetDev
}

// FetchRelease downloads + verifies both chains' signed sets into dest, each beside the manifest
// that verified it: the briard bundle (this binary's arm) under dest/briard, the VM image under
// dest/vm. The vm release is the one the briard manifest names, since the image (a function of
// its inputs, re-published only when they change) has no id derivable from the briard id; one
// selector still installs one tested pair, resolved on the briard side. haveVM is the vm release
// already installed here ("" for none); when the briard release names it, the image is not
// fetched again and dest/vm is absent (400 MB a reinstall would only copy over itself).
// All-or-nothing across the chains as within each: dest appears only once both verified, so an
// install never sees a briard bundle without the VM image it was published beside, fetched or
// already installed (briard/stable + vm/stable is the tested pair, by construction).
func FetchRelease(ctx context.Context, base, release string, kr *selfupdate.Keyring, dest, haveVM string, logf func(string, ...any)) error {
	if _, err := os.Stat(dest); err == nil {
		return fmt.Errorf("staging dest %s already exists", dest)
	}
	tmp, err := os.MkdirTemp(filepath.Dir(dest), ".briard-install-")
	if err != nil {
		return fmt.Errorf("staging dir: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			os.RemoveAll(tmp)
		}
	}()
	bf := &Fetcher{BaseURL: base, Chain: ChainBriard, Platform: PlatformHost, Keyring: kr, Logf: logf}
	if err := bf.FetchVerified(ctx, release, filepath.Join(tmp, ChainBriard)); err != nil {
		return err
	}
	bm, err := ReadManifest(filepath.Join(tmp, ChainBriard, ManifestName))
	if err != nil {
		return fmt.Errorf("read the fetched briard manifest: %w", err)
	}
	if bm.VM == "" {
		return fmt.Errorf("briard release %s names no vm release -- published before releases carried one; the alpha reinstalls from a current channel", bm.Version)
	}
	if bm.VM == haveVM {
		logf("install: the guest image installed here is %s, the release's own; not fetched again", haveVM)
	} else {
		vf := &Fetcher{BaseURL: base, Chain: ChainVM, Platform: PlatformVM, Keyring: kr, Logf: logf}
		if err := vf.FetchVerified(ctx, bm.VM, filepath.Join(tmp, ChainVM)); err != nil {
			return err
		}
	}
	if err := os.Rename(tmp, dest); err != nil {
		return fmt.Errorf("place staging dir: %w", err)
	}
	committed = true
	return nil
}
