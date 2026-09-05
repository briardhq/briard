package install

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"

	"briard.io/agent/selfupdate"
)

func pubPEM(t *testing.T, pub ed25519.PublicKey) []byte {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
}

func sha(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// The release every fixture publishes, and where the tree puts its pieces: the manifest is
// served from the `latest` pointer (what a fetch names), the artifacts only from the versioned
// directory (what the manifest names) -- the split the fetcher has to honour. The host chain
// carries the platform level, so the fixture does too.
const testVersion = "v3.20260905.abc1234"

func pointerPath(name string) string {
	return ChainHost + "/" + TargetLatest + "/" + PlatformLinux + "/" + name
}
func releasePath(name string) string {
	return ChainHost + "/" + testVersion + "/" + PlatformLinux + "/" + name
}

// channel is a fake signed release channel: a keyring holding one signer, a set of served
// bodies keyed by their path under the channel root, and an httptest server. Tests mutate the
// bodies (tamper an artifact, resign a different manifest, drop a file) before serving.
type channel struct {
	t       *testing.T
	priv    ed25519.PrivateKey
	kr      *selfupdate.Keyring
	bodies  map[string][]byte
	missing map[string]bool
}

// newChannel builds a channel whose signed manifest lists arts (as the host chain at
// testVersion), serving bytesByName for each artifact under the versioned directory and the
// signed manifest under both the versioned directory and the `latest` pointer -- the exact
// shape `publish-release.sh sign` lays out.
func newChannel(t *testing.T, arts []Entry, bytesByName map[string][]byte) *channel {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	kr, err := selfupdate.NewKeyring(pubPEM(t, pub))
	if err != nil {
		t.Fatal(err)
	}
	mb, err := json.Marshal(Manifest{Chain: ChainHost, Platform: PlatformLinux, Version: testVersion, Artifacts: arts})
	if err != nil {
		t.Fatal(err)
	}
	sig := ed25519.Sign(priv, mb)
	bodies := map[string][]byte{
		pointerPath(ManifestName):             mb,
		pointerPath(ManifestName + sigSuffix): sig,
		releasePath(ManifestName):             mb,
		releasePath(ManifestName + sigSuffix): sig,
	}
	for name, b := range bytesByName {
		bodies[releasePath(name)] = b
	}
	return &channel{t: t, priv: priv, kr: kr, bodies: bodies, missing: map[string]bool{}}
}

func (c *channel) serve() string {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/")
		if c.missing[name] {
			http.NotFound(w, r)
			return
		}
		b, ok := c.bodies[name]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write(b)
	}))
	c.t.Cleanup(srv.Close)
	return srv.URL
}

func (c *channel) fetcher() *Fetcher {
	return &Fetcher{BaseURL: c.serve(), Chain: ChainHost, Platform: PlatformLinux, Keyring: c.kr}
}

// fetchLatest is what install.sh does: name the pointer, get whatever release it points at.
func (c *channel) fetchLatest(dest string) error {
	return c.fetcher().FetchVerified(context.Background(), TargetLatest, dest)
}

// goodChannel is the happy-path fixture: three artifacts (agent binary, a large-ish guest
// image stand-in, a qemu tarball) with a correct signed manifest.
func goodChannel(t *testing.T) *channel {
	agent := []byte("the briard-agent static binary")
	guest := bytes.Repeat([]byte("guest-image-chunk;"), 4096) // ~72 KiB, streams through LimitReader/hash
	qemu := []byte("qemu-bundle.tar contents")
	bytesByName := map[string][]byte{
		"briard-agent":    agent,
		"nixos.qcow2":     guest,
		"qemu-bundle.tar": qemu,
	}
	arts := []Entry{
		{Name: "briard-agent", SHA256: sha(agent), Size: int64(len(agent)), Mode: 0o755},
		{Name: "nixos.qcow2", SHA256: sha(guest), Size: int64(len(guest))},
		{Name: "qemu-bundle.tar", SHA256: sha(qemu), Size: int64(len(qemu))},
	}
	return newChannel(t, arts, bytesByName)
}

// stagedFresh returns a dest path under a fresh temp dir; the parent exists (FetchVerified
// stages a sibling temp dir there), dest itself does not.
func stagedFresh(t *testing.T) string {
	return filepath.Join(t.TempDir(), "staging")
}

// assertRefused checks that a negative path both returned the wanted error AND left nothing
// behind: dest absent and no orphaned .briard-stage- temp dir (the all-or-nothing property).
func assertRefused(t *testing.T, dest string, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("got error %v, want %v", err, want)
	}
	if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
		t.Errorf("refused fetch left a staging dir at %s (want none)", dest)
	}
	parent := filepath.Dir(dest)
	ents, _ := os.ReadDir(parent)
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), ".briard-stage-") {
			t.Errorf("refused fetch left an orphan temp dir %s", e.Name())
		}
	}
}

func TestFetchVerifiedStagesTheSignedSet(t *testing.T) {
	c := goodChannel(t)
	dest := stagedFresh(t)
	if err := c.fetchLatest(dest); err != nil {
		t.Fatalf("good channel: %v", err)
	}
	// Every artifact landed with the exact bytes and the declared mode.
	for name, want := range map[string]string{
		"briard-agent":    "the briard-agent static binary",
		"qemu-bundle.tar": "qemu-bundle.tar contents",
	} {
		got, err := os.ReadFile(filepath.Join(dest, name))
		if err != nil {
			t.Fatalf("read staged %s: %v", name, err)
		}
		if string(got) != want {
			t.Errorf("staged %s = %q, want %q", name, got, want)
		}
	}
	fi, err := os.Stat(filepath.Join(dest, "briard-agent"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o755 {
		t.Errorf("agent mode = %o, want 0755", fi.Mode().Perm())
	}
	if _, err := os.Stat(filepath.Join(dest, "nixos.qcow2")); err != nil {
		t.Errorf("guest image not staged: %v", err)
	}
	// The verified manifest travels with the set -- the exact bytes that verified, so an
	// installed node can keep it beside the binaries and know which release it is on.
	kept, err := os.ReadFile(filepath.Join(dest, ManifestName))
	if err != nil {
		t.Fatalf("manifest not kept beside the artifacts: %v", err)
	}
	if !bytes.Equal(kept, c.bodies[pointerPath(ManifestName)]) {
		t.Error("the kept manifest is not the bytes that verified")
	}
}

// The load-bearing negatives (all failable, [[verification-assertions-must-fail]]): each one
// must refuse AND leave the staging dest pristine.

func TestFetchVerifiedRefusesTamperedArtifact(t *testing.T) {
	c := goodChannel(t)
	// Serve DIFFERENT bytes for the guest image than the (correctly signed) manifest pins.
	c.bodies[releasePath("nixos.qcow2")] = append(c.bodies[releasePath("nixos.qcow2")], []byte("EVIL PAYLOAD")...)
	dest := stagedFresh(t)
	assertRefused(t, dest, c.fetchLatest(dest), ErrArtifactMismatch)
}

func TestFetchVerifiedRefusesTamperedManifest(t *testing.T) {
	c := goodChannel(t)
	// The manifest bytes were signed; now serve a DIFFERENT manifest body under the same name,
	// so the (still-served) signature no longer verifies it.
	c.bodies[pointerPath(ManifestName)] = append(c.bodies[pointerPath(ManifestName)], ' ')
	dest := stagedFresh(t)
	assertRefused(t, dest, c.fetchLatest(dest), selfupdate.ErrBadSignature)
}

func TestFetchVerifiedRefusesUnsignedManifest(t *testing.T) {
	c := goodChannel(t)
	c.bodies[pointerPath(ManifestName+sigSuffix)] = nil // empty signature
	dest := stagedFresh(t)
	assertRefused(t, dest, c.fetchLatest(dest), selfupdate.ErrUnsigned)
}

func TestFetchVerifiedFailsClosedWithoutKeyring(t *testing.T) {
	c := goodChannel(t)
	dest := stagedFresh(t)
	f := &Fetcher{BaseURL: c.serve(), Chain: ChainHost, Platform: PlatformLinux, Keyring: nil} // no ring wired
	assertRefused(t, dest, f.FetchVerified(context.Background(), TargetLatest, dest), ErrNoKeyring)

	// An EMPTY (but non-nil) ring is equally fail-closed.
	empty, err := selfupdate.NewKeyring()
	if err != nil {
		t.Fatal(err)
	}
	dest2 := stagedFresh(t)
	f2 := &Fetcher{BaseURL: c.serve(), Chain: ChainHost, Platform: PlatformLinux, Keyring: empty}
	assertRefused(t, dest2, f2.FetchVerified(context.Background(), TargetLatest, dest2), ErrNoKeyring)
}

// A genuine, correctly signed manifest for the OTHER chain is refused: the signature proves the
// bytes are ours, the chain field proves they are the wrong release line, and a fetch that
// accepted them would install a guest image where a host bundle was expected (or compare a
// guest date against a host date and silently no-op, which is the failure [B.86a] names).
func TestFetchVerifiedRefusesWrongChain(t *testing.T) {
	c := goodChannel(t)
	// Serve the host chain's (signed, chain:"host") manifest where the guest chain's would be.
	for _, n := range []string{ManifestName, ManifestName + sigSuffix} {
		c.bodies[ChainGuest+"/"+TargetLatest+"/"+n] = c.bodies[pointerPath(n)]
	}
	dest := stagedFresh(t)
	f := &Fetcher{BaseURL: c.serve(), Chain: ChainGuest, Keyring: c.kr}
	assertRefused(t, dest, f.FetchVerified(context.Background(), TargetLatest, dest), ErrWrongChain)
}

// The target is a URL path element chosen by whoever runs the installer; it must not be able
// to climb the tree. Refused before any request is made, and nothing is staged.
func TestFetchVerifiedRefusesUnsafeTarget(t *testing.T) {
	c := goodChannel(t)
	for _, target := range []string{"", "..", "../host", "a/b", ".hidden", "v3?x=1"} {
		dest := stagedFresh(t)
		err := c.fetcher().FetchVerified(context.Background(), target, dest)
		if err == nil {
			t.Errorf("target %q was accepted", target)
		}
		if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
			t.Errorf("target %q left a staging dir", target)
		}
	}
}

func TestFetchVerifiedRefusesWrongSize(t *testing.T) {
	// A manifest whose Size disagrees with the served (and correctly-hashed) bytes: the size
	// guard trips before/at the hash check, so a length-lie is refused.
	agent := []byte("agent bytes")
	arts := []Entry{{Name: "briard-agent", SHA256: sha(agent), Size: int64(len(agent)) + 5}}
	c := newChannel(t, arts, map[string][]byte{"briard-agent": agent})
	dest := stagedFresh(t)
	assertRefused(t, dest, c.fetchLatest(dest), ErrArtifactMismatch)
}

func TestFetchVerifiedRefusesMissingArtifact(t *testing.T) {
	c := goodChannel(t)
	c.missing[releasePath("qemu-bundle.tar")] = true // 404 the third artifact
	dest := stagedFresh(t)
	err := c.fetchLatest(dest)
	if err == nil {
		t.Fatal("a 404 artifact should abort the fetch")
	}
	if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
		t.Errorf("a missing artifact left a staging dir at %s", dest)
	}
}

func TestFetchVerifiedRefusesEmptyManifest(t *testing.T) {
	c := newChannel(t, nil, nil) // signs an empty artifact list
	dest := stagedFresh(t)
	assertRefused(t, dest, c.fetchLatest(dest), ErrManifest)
}

func TestFetchVerifiedRefusesUnsafeArtifactName(t *testing.T) {
	// Even a validly-signed manifest must not be able to write outside the staging dir.
	body := []byte("x")
	arts := []Entry{{Name: "../escape", SHA256: sha(body), Size: int64(len(body))}}
	c := newChannel(t, arts, map[string][]byte{"../escape": body})
	dest := stagedFresh(t)
	assertRefused(t, dest, c.fetchLatest(dest), ErrArtifactMismatch)
}

func TestFetchVerifiedRefusesPreexistingDest(t *testing.T) {
	c := goodChannel(t)
	dest := stagedFresh(t)
	if err := os.Mkdir(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := c.fetchLatest(dest); err == nil {
		t.Fatal("FetchVerified into a pre-existing dest should error (never merge into it)")
	}
}

// ---- compressed artifacts (.zst) --------------------------------------------------------
//
// The big two ship compressed and are expanded HERE, after the signed-hash check. These tests
// pin both halves of that: the expansion happens at all, and it happens only downstream of
// verification — a tampered .zst must be refused without ever reaching the decoder.

// zstdOf compresses b the way the release pipeline does.
func zstdOf(t *testing.T, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, err := zstd.NewWriter(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(b); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// compressedChannel serves the guest image + qemu bundle as .zst, the shape the release
// publishes, with `briard-agent` left plain (the bootstrap fetches it with curl, before any
// agent exists to decompress).
func compressedChannel(t *testing.T) (*channel, []byte) {
	t.Helper()
	agent := []byte("the briard-agent static binary")
	guest := bytes.Repeat([]byte("guest-image-chunk;"), 4096)
	qemu := []byte("qemu-bundle.tar contents")
	guestZ, qemuZ := zstdOf(t, guest), zstdOf(t, qemu)
	bytesByName := map[string][]byte{
		"briard-agent":        agent,
		"nixos.qcow2.zst":     guestZ,
		"qemu-bundle.tar.zst": qemuZ,
	}
	arts := []Entry{
		{Name: "briard-agent", SHA256: sha(agent), Size: int64(len(agent)), Mode: 0o755},
		{Name: "nixos.qcow2.zst", SHA256: sha(guestZ), Size: int64(len(guestZ))},
		{Name: "qemu-bundle.tar.zst", SHA256: sha(qemuZ), Size: int64(len(qemuZ))},
	}
	return newChannel(t, arts, bytesByName), guest
}

func TestFetchVerifiedExpandsCompressedArtifacts(t *testing.T) {
	c, guest := compressedChannel(t)
	dest := stagedFresh(t)
	if err := c.fetchLatest(dest); err != nil {
		t.Fatalf("compressed channel: %v", err)
	}
	// The staging dir holds the names install.sh expects — which is why shipping these
	// compressed needed no change to install.sh.
	got, err := os.ReadFile(filepath.Join(dest, "nixos.qcow2"))
	if err != nil {
		t.Fatalf("guest image not expanded: %v", err)
	}
	if !bytes.Equal(got, guest) {
		t.Errorf("expanded guest image is %d bytes, want %d", len(got), len(guest))
	}
	qb, err := os.ReadFile(filepath.Join(dest, "qemu-bundle.tar"))
	if err != nil {
		t.Fatalf("qemu bundle not expanded: %v", err)
	}
	if string(qb) != "qemu-bundle.tar contents" {
		t.Errorf("expanded qemu bundle = %q", qb)
	}
	// The compressed copies are gone: leaving them would double the staging footprint on a disk
	// the report card has already sized.
	for _, n := range []string{"nixos.qcow2.zst", "qemu-bundle.tar.zst"} {
		if _, err := os.Stat(filepath.Join(dest, n)); !os.IsNotExist(err) {
			t.Errorf("%s survived expansion (want removed)", n)
		}
	}
}

// The ordering assertion: corrupt the COMPRESSED bytes and the fetch must die on the hash, with
// nothing expanded and nothing committed. If expansion ever moved upstream of verification this
// is the test that fails.
func TestFetchVerifiedRefusesTamperedCompressedArtifact(t *testing.T) {
	c, _ := compressedChannel(t)
	z := c.bodies[releasePath("nixos.qcow2.zst")]
	tampered := append([]byte(nil), z...)
	tampered[len(tampered)/2] ^= 0xff
	c.bodies[releasePath("nixos.qcow2.zst")] = tampered
	dest := stagedFresh(t)
	assertRefused(t, dest, c.fetchLatest(dest), ErrArtifactMismatch)
}

// A .zst whose hash matches but whose payload is not a zstd stream: a mis-built release, not an
// attack. It must fail loudly rather than commit a staging dir holding a corrupt guest image.
func TestFetchVerifiedRefusesUndecodableCompressedArtifact(t *testing.T) {
	junk := []byte("this is not a zstd stream at all")
	agent := []byte("the briard-agent static binary")
	c := newChannel(t, []Entry{
		{Name: "briard-agent", SHA256: sha(agent), Size: int64(len(agent)), Mode: 0o755},
		{Name: "nixos.qcow2.zst", SHA256: sha(junk), Size: int64(len(junk))},
	}, map[string][]byte{"briard-agent": agent, "nixos.qcow2.zst": junk})
	dest := stagedFresh(t)
	err := c.fetchLatest(dest)
	if err == nil {
		t.Fatal("undecodable .zst was accepted")
	}
	if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
		t.Errorf("undecodable .zst still produced a staging dir at %s", dest)
	}
}

// ---- the writer/reader round trip -------------------------------------------------------
//
// The point of WriteManifest existing at all: the bytes the release publishes are described by
// the same code that later reads them. This drives the REAL writer over a staging dir, signs its
// output, serves it, and runs the REAL FetchVerified against it — so a format change that breaks
// the contract fails here rather than at a stranger's first install.

func TestManifestRoundTripsThroughFetchVerified(t *testing.T) {
	stage := t.TempDir()
	agent := []byte("the briard-agent static binary")
	guest := bytes.Repeat([]byte("guest-image-chunk;"), 4096)
	write := func(name string, b []byte, mode os.FileMode) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(stage, name), b, mode); err != nil {
			t.Fatal(err)
		}
		// WriteFile respects umask, so force the mode we are asserting on.
		if err := os.Chmod(filepath.Join(stage, name), mode); err != nil {
			t.Fatal(err)
		}
	}
	write("briard-agent", agent, 0o755)
	write("nixos.qcow2.zst", zstdOf(t, guest), 0o644)

	if err := WriteManifest(stage, ChainHost, PlatformLinux, testVersion); err != nil {
		t.Fatalf("WriteManifest: %v", err)
	}
	mb, err := os.ReadFile(filepath.Join(stage, ManifestName))
	if err != nil {
		t.Fatal(err)
	}
	var man Manifest
	if err := json.Unmarshal(mb, &man); err != nil {
		t.Fatalf("the manifest we just wrote does not parse: %v", err)
	}
	if man.Chain != ChainHost || man.Platform != PlatformLinux || man.Version != testVersion {
		t.Errorf("chain/platform/version = %q/%q/%q, want %q/%q/%q", man.Chain, man.Platform, man.Version, ChainHost, PlatformLinux, testVersion)
	}
	if len(man.Artifacts) != 2 {
		t.Fatalf("manifest lists %d artifacts, want 2: %+v", len(man.Artifacts), man.Artifacts)
	}
	// The mode round-trips as a NUMBER the reader applies, which is the thing the old
	// hand-written "mode":493 got right only by luck.
	for _, a := range man.Artifacts {
		if a.Name == "briard-agent" && a.Mode != 0o755 {
			t.Errorf("agent mode = %o, want 0755", a.Mode)
		}
		if a.Name == "nixos.qcow2.zst" && a.Mode != 0 {
			t.Errorf("0644 artifact emitted a mode (%o); want omitted", a.Mode)
		}
	}
	// Running the writer AGAIN over its own output must not list the manifest as an artifact
	// of itself -- the one file a release directory holds that the manifest cannot name.
	if err := WriteManifest(stage, ChainHost, PlatformLinux, testVersion); err != nil {
		t.Fatalf("WriteManifest (second pass): %v", err)
	}
	mb2, _ := os.ReadFile(filepath.Join(stage, ManifestName))
	if !bytes.Equal(mb, mb2) {
		t.Error("re-describing the same directory produced different manifest bytes")
	}

	// Now serve exactly those bytes and let the real fetcher consume them.
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	kr, err := selfupdate.NewKeyring(pubPEM(t, pub))
	if err != nil {
		t.Fatal(err)
	}
	bodies := map[string][]byte{
		pointerPath(ManifestName):             mb,
		pointerPath(ManifestName + sigSuffix): ed25519.Sign(priv, mb),
	}
	for _, a := range man.Artifacts {
		b, err := os.ReadFile(filepath.Join(stage, a.Name))
		if err != nil {
			t.Fatal(err)
		}
		bodies[releasePath(a.Name)] = b
	}
	c := &channel{t: t, priv: priv, kr: kr, bodies: bodies, missing: map[string]bool{}}
	dest := stagedFresh(t)
	if err := (&Fetcher{BaseURL: c.serve(), Chain: ChainHost, Platform: PlatformLinux, Keyring: kr}).FetchVerified(context.Background(), TargetLatest, dest); err != nil {
		t.Fatalf("a manifest written by WriteManifest was refused by FetchVerified: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dest, "nixos.qcow2"))
	if err != nil {
		t.Fatalf("guest image not expanded: %v", err)
	}
	if !bytes.Equal(got, guest) {
		t.Error("round-tripped guest image differs from the staged one")
	}
	fi, err := os.Stat(filepath.Join(dest, "briard-agent"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o755 {
		t.Errorf("round-tripped agent mode = %o, want 0755", fi.Mode().Perm())
	}
}

// The writer refuses to describe a directory as a release whose id spells a pointer, or whose
// chain/version cannot stand as a path element: the tree would have no way to tell such a
// directory from the pointer it collides with.
func TestWriteManifestRefusesUnusableIds(t *testing.T) {
	stage := t.TempDir()
	if err := os.WriteFile(filepath.Join(stage, "briard-agent"), []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, tc := range [][2]string{
		{ChainHost, TargetStable}, {ChainHost, TargetLatest}, {ChainHost, ""}, {ChainHost, "../v3"},
		{"", testVersion}, {"host/extra", testVersion}, {".host", testVersion},
	} {
		if err := WriteManifest(stage, tc[0], "", tc[1]); err == nil {
			t.Errorf("WriteManifest(chain=%q, version=%q) was accepted", tc[0], tc[1])
		}
	}
	if _, err := os.Stat(filepath.Join(stage, ManifestName)); !os.IsNotExist(err) {
		t.Error("a refused WriteManifest still wrote a manifest")
	}
}

// Artifacts resolve against the manifest's own `version`, never against the path the manifest
// was fetched from. A pointer path serves a manifest and nothing else (bar the bootstrap
// agent), so this is what lets `stable/` be a 1 KB copy rather than a second 380 MB image --
// and it is an anti-substitution property: a signed manifest says where its own bytes live.
func TestFetchVerifiedResolvesArtifactsAgainstTheManifestVersion(t *testing.T) {
	c, guest := compressedChannel(t)
	// The channel as published: artifacts ONLY under the versioned directory. (The pointer
	// serves the manifest, which is how the fixture is built.)
	dest := stagedFresh(t)
	if err := c.fetchLatest(dest); err != nil {
		t.Fatalf("a channel with artifacts under <chain>/<version>/ was not fetchable: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dest, "nixos.qcow2"))
	if err != nil {
		t.Fatalf("guest image not staged from the versioned directory: %v", err)
	}
	if !bytes.Equal(got, guest) {
		t.Error("image fetched from the versioned directory differs")
	}

	// The failable control: move every artifact to sit BESIDE the pointer's manifest and away
	// from the versioned directory. A fetcher that resolved against the fetched path would now
	// succeed; the real one must fail, because the manifest says the bytes live elsewhere.
	moved := map[string][]byte{}
	for name, b := range c.bodies {
		if rest, ok := strings.CutPrefix(name, ChainHost+"/"+testVersion+"/"+PlatformLinux+"/"); ok {
			moved[pointerPath(rest)] = b
		} else {
			moved[name] = b
		}
	}
	c.bodies = moved
	if err := c.fetchLatest(stagedFresh(t)); err == nil {
		t.Error("artifacts served beside the pointer were accepted; resolution is not actually by manifest version")
	}
}

// An exact version id is a target like any other: <chain>/<id>/ serves its own manifest.
func TestFetchVerifiedAcceptsAnExactVersionTarget(t *testing.T) {
	c := goodChannel(t)
	dest := stagedFresh(t)
	if err := c.fetcher().FetchVerified(context.Background(), testVersion, dest); err != nil {
		t.Fatalf("exact version target: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "briard-agent")); err != nil {
		t.Errorf("agent not staged from the exact-version target: %v", err)
	}
}

// The platform is the second half of the crossed-wire check: the Linux manifest (genuine,
// signed, chain "host") served under the windows arm is refused, because the bundle it names
// cannot run there and a host that installed it would find out at the next guest launch.
func TestFetchVerifiedRefusesWrongPlatform(t *testing.T) {
	c := goodChannel(t)
	for _, n := range []string{ManifestName, ManifestName + sigSuffix} {
		c.bodies[ChainHost+"/"+TargetLatest+"/windows/"+n] = c.bodies[pointerPath(n)]
	}
	dest := stagedFresh(t)
	f := &Fetcher{BaseURL: c.serve(), Chain: ChainHost, Platform: "windows", Keyring: c.kr}
	assertRefused(t, dest, f.FetchVerified(context.Background(), TargetLatest, dest), ErrWrongChain)
	// And a platform-less fetch of a platformed manifest, likewise (the guest chain's shape
	// pointed at a host directory).
	for _, n := range []string{ManifestName, ManifestName + sigSuffix} {
		c.bodies[ChainHost+"/"+TargetLatest+"/"+n] = c.bodies[pointerPath(n)]
	}
	dest2 := stagedFresh(t)
	f2 := &Fetcher{BaseURL: c.serve(), Chain: ChainHost, Keyring: c.kr}
	assertRefused(t, dest2, f2.FetchVerified(context.Background(), TargetLatest, dest2), ErrWrongChain)
}

// A chain without the platform level (the guest) is served flat: no platform in the path, no
// platform in the manifest, and the fetcher asked for none.
func TestFetchVerifiedHandlesAPlatformlessChain(t *testing.T) {
	c := goodChannel(t)
	img := []byte("guest image bytes")
	gv := "guest.20260905.abc1234"
	mb, err := json.Marshal(Manifest{Chain: ChainGuest, Version: gv, Artifacts: []Entry{
		{Name: "nixos.qcow2", SHA256: sha(img), Size: int64(len(img))},
	}})
	if err != nil {
		t.Fatal(err)
	}
	c.bodies[ChainGuest+"/"+TargetStable+"/"+ManifestName] = mb
	c.bodies[ChainGuest+"/"+TargetStable+"/"+ManifestName+sigSuffix] = ed25519.Sign(c.priv, mb)
	c.bodies[ChainGuest+"/"+gv+"/nixos.qcow2"] = img
	dest := stagedFresh(t)
	f := &Fetcher{BaseURL: c.serve(), Chain: ChainGuest, Keyring: c.kr}
	if err := f.FetchVerified(context.Background(), TargetStable, dest); err != nil {
		t.Fatalf("guest chain: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dest, "nixos.qcow2"))
	if err != nil || !bytes.Equal(got, img) {
		t.Fatalf("guest image not staged from guest/%s/: %v", gv, err)
	}
}
