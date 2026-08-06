package selfupdate

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"testing"
)

// pubPEM marshals an Ed25519 public key to the PKIX "PUBLIC KEY" PEM the keyring parses.
func pubPEM(t *testing.T, pub ed25519.PublicKey) []byte {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
}

func genKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return pub, priv
}

func TestVerifyAcceptsAGoodSignature(t *testing.T) {
	pub, priv := genKey(t)
	kr, err := NewKeyring(pubPEM(t, pub))
	if err != nil {
		t.Fatal(err)
	}
	artifact := []byte("a legitimate signed briard-agent binary")
	sig := ed25519.Sign(priv, artifact)
	if err := kr.Verify(artifact, sig); err != nil {
		t.Errorf("a validly-signed artifact was refused: %v", err)
	}
}

// The load-bearing negative: a tampered artifact (or a wrong key) must be refused, so the
// caller never stages it and the committed binary stays. [[verification-assertions-must-fail]]
func TestVerifyRefusesTamperedArtifact(t *testing.T) {
	pub, priv := genKey(t)
	kr, _ := NewKeyring(pubPEM(t, pub))
	artifact := []byte("the original binary")
	sig := ed25519.Sign(priv, artifact)
	tampered := append([]byte{}, artifact...)
	tampered[0] ^= 0xff
	if err := kr.Verify(tampered, sig); !errors.Is(err, ErrBadSignature) {
		t.Errorf("tampered artifact: got %v, want ErrBadSignature", err)
	}
}

func TestVerifyRefusesSignatureFromAnUntrustedKey(t *testing.T) {
	pub, _ := genKey(t)
	_, evilPriv := genKey(t) // a key NOT in the ring
	kr, _ := NewKeyring(pubPEM(t, pub))
	artifact := []byte("payload")
	if err := kr.Verify(artifact, ed25519.Sign(evilPriv, artifact)); !errors.Is(err, ErrBadSignature) {
		t.Errorf("signature from an untrusted key: got %v, want ErrBadSignature", err)
	}
}

func TestVerifyRefusesUnsignedArtifact(t *testing.T) {
	pub, _ := genKey(t)
	kr, _ := NewKeyring(pubPEM(t, pub))
	if err := kr.Verify([]byte("payload"), nil); !errors.Is(err, ErrUnsigned) {
		t.Errorf("empty signature: got %v, want ErrUnsigned", err)
	}
}

// Fail CLOSED: an empty ring must refuse everything, never fail-open to "no key = allow".
func TestVerifyEmptyKeyringRefusesEverything(t *testing.T) {
	kr, err := NewKeyring()
	if err != nil {
		t.Fatal(err)
	}
	if kr.Len() != 0 {
		t.Fatalf("empty keyring Len = %d", kr.Len())
	}
	_, priv := genKey(t)
	artifact := []byte("even a well-formed signature")
	if err := kr.Verify(artifact, ed25519.Sign(priv, artifact)); !errors.Is(err, ErrNoKeys) {
		t.Errorf("empty keyring: got %v, want ErrNoKeys", err)
	}
}

// Additive rotation: a ring holding several keys accepts an artifact signed by ANY of them
// (so a new signer can be trusted before the old one is retired — no flag-day).
func TestVerifyAcceptsAnyKeyInTheRing(t *testing.T) {
	pub1, _ := genKey(t)
	pub2, priv2 := genKey(t)
	pub3, _ := genKey(t)
	kr, err := NewKeyring(pubPEM(t, pub1), pubPEM(t, pub2), pubPEM(t, pub3))
	if err != nil {
		t.Fatal(err)
	}
	if kr.Len() != 3 {
		t.Fatalf("ring Len = %d, want 3", kr.Len())
	}
	artifact := []byte("signed by the middle key")
	if err := kr.Verify(artifact, ed25519.Sign(priv2, artifact)); err != nil {
		t.Errorf("artifact signed by an in-ring key was refused: %v", err)
	}
}

// Several PEM blocks may arrive concatenated in one blob (a keyring file); parse them all.
func TestNewKeyringParsesConcatenatedBlocks(t *testing.T) {
	pub1, _ := genKey(t)
	pub2, _ := genKey(t)
	blob := append(pubPEM(t, pub1), pubPEM(t, pub2)...)
	kr, err := NewKeyring(blob)
	if err != nil {
		t.Fatal(err)
	}
	if kr.Len() != 2 {
		t.Errorf("concatenated PEM: Len = %d, want 2", kr.Len())
	}
}

func TestNewKeyringRejectsNonEd25519Key(t *testing.T) {
	// An RSA/EC key in PKIX PEM form must be rejected at load, not silently ignored.
	block := &pem.Block{Type: "PUBLIC KEY", Bytes: []byte("not a real DER key")}
	if _, err := NewKeyring(pem.EncodeToMemory(block)); err == nil {
		t.Error("a malformed public key was accepted")
	}
}
