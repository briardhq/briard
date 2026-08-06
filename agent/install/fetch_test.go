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
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

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

// channel is a fake signed release channel: a keyring holding one signer, a set of served
// bodies (manifest.json + its .sig + the artifacts), and an httptest server. Tests mutate the
// bodies (tamper an artifact, resign a different manifest, drop a file) before serving.
type channel struct {
	t       *testing.T
	priv    ed25519.PrivateKey
	kr      *selfupdate.Keyring
	bodies  map[string][]byte
	missing map[string]bool
}

// newChannel builds a channel whose signed manifest lists arts, serving bytesByName for each
// artifact (and a valid signature over the marshalled manifest).
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
	mb, err := json.Marshal(Manifest{Artifacts: arts})
	if err != nil {
		t.Fatal(err)
	}
	bodies := map[string][]byte{
		ManifestName:             mb,
		ManifestName + sigSuffix: ed25519.Sign(priv, mb),
	}
	maps.Copy(bodies, bytesByName)
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
	return &Fetcher{BaseURL: c.serve(), Keyring: c.kr}
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
	if err := c.fetcher().FetchVerified(context.Background(), dest); err != nil {
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
}

// The load-bearing negatives (all failable, [[verification-assertions-must-fail]]): each one
// must refuse AND leave the staging dest pristine.

func TestFetchVerifiedRefusesTamperedArtifact(t *testing.T) {
	c := goodChannel(t)
	// Serve DIFFERENT bytes for the guest image than the (correctly signed) manifest pins.
	c.bodies["nixos.qcow2"] = append(c.bodies["nixos.qcow2"], []byte("EVIL PAYLOAD")...)
	dest := stagedFresh(t)
	err := c.fetcher().FetchVerified(context.Background(), dest)
	assertRefused(t, dest, err, ErrArtifactMismatch)
}

func TestFetchVerifiedRefusesTamperedManifest(t *testing.T) {
	c := goodChannel(t)
	// The manifest bytes were signed; now serve a DIFFERENT manifest body under the same name,
	// so the (still-served) signature no longer verifies it.
	c.bodies[ManifestName] = append(c.bodies[ManifestName], ' ')
	dest := stagedFresh(t)
	err := c.fetcher().FetchVerified(context.Background(), dest)
	assertRefused(t, dest, err, selfupdate.ErrBadSignature)
}

func TestFetchVerifiedRefusesUnsignedManifest(t *testing.T) {
	c := goodChannel(t)
	c.bodies[ManifestName+sigSuffix] = nil // empty signature
	dest := stagedFresh(t)
	err := c.fetcher().FetchVerified(context.Background(), dest)
	assertRefused(t, dest, err, selfupdate.ErrUnsigned)
}

func TestFetchVerifiedFailsClosedWithoutKeyring(t *testing.T) {
	c := goodChannel(t)
	dest := stagedFresh(t)
	f := &Fetcher{BaseURL: c.serve(), Keyring: nil} // no ring wired
	assertRefused(t, dest, f.FetchVerified(context.Background(), dest), ErrNoKeyring)

	// An EMPTY (but non-nil) ring is equally fail-closed.
	empty, err := selfupdate.NewKeyring()
	if err != nil {
		t.Fatal(err)
	}
	dest2 := stagedFresh(t)
	f2 := &Fetcher{BaseURL: c.serve(), Keyring: empty}
	assertRefused(t, dest2, f2.FetchVerified(context.Background(), dest2), ErrNoKeyring)
}

func TestFetchVerifiedRefusesWrongSize(t *testing.T) {
	// A manifest whose Size disagrees with the served (and correctly-hashed) bytes: the size
	// guard trips before/at the hash check, so a length-lie is refused.
	agent := []byte("agent bytes")
	arts := []Entry{{Name: "briard-agent", SHA256: sha(agent), Size: int64(len(agent)) + 5}}
	c := newChannel(t, arts, map[string][]byte{"briard-agent": agent})
	dest := stagedFresh(t)
	err := c.fetcher().FetchVerified(context.Background(), dest)
	assertRefused(t, dest, err, ErrArtifactMismatch)
}

func TestFetchVerifiedRefusesMissingArtifact(t *testing.T) {
	c := goodChannel(t)
	c.missing["qemu-bundle.tar"] = true // 404 the third artifact
	dest := stagedFresh(t)
	err := c.fetcher().FetchVerified(context.Background(), dest)
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
	err := c.fetcher().FetchVerified(context.Background(), dest)
	assertRefused(t, dest, err, ErrManifest)
}

func TestFetchVerifiedRefusesUnsafeArtifactName(t *testing.T) {
	// Even a validly-signed manifest must not be able to write outside the staging dir.
	body := []byte("x")
	arts := []Entry{{Name: "../escape", SHA256: sha(body), Size: int64(len(body))}}
	c := newChannel(t, arts, map[string][]byte{"../escape": body})
	dest := stagedFresh(t)
	err := c.fetcher().FetchVerified(context.Background(), dest)
	assertRefused(t, dest, err, ErrArtifactMismatch)
}

func TestFetchVerifiedRefusesPreexistingDest(t *testing.T) {
	c := goodChannel(t)
	dest := stagedFresh(t)
	if err := os.Mkdir(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := c.fetcher().FetchVerified(context.Background(), dest); err == nil {
		t.Fatal("FetchVerified into a pre-existing dest should error (never merge into it)")
	}
}
