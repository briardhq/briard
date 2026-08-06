package selfupdate

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
)

// A release artifact is trusted only if it carries a valid detached Ed25519 signature from
// the release-signing root. The agent verifies BEFORE it stages —
// refuse-and-stay is the whole point: a bad or absent signature must leave the committed
// binary untouched, never trial an unverified binary. This is the client half of the four
// key-management points (keyring / offline-blesses-warm root / M-of-N + escrow /
// verify-or-refuse); full TUF (roles, timestamp/snapshot freshness) is deferred.

// ErrUnsigned is returned when an artifact arrives with no signature — refused, not trialed.
var ErrUnsigned = errors.New("selfupdate: unsigned artifact refused")

// ErrNoKeys is returned when the keyring is empty. Fail CLOSED: with no trusted key there is
// no such thing as a valid signature, so every artifact is refused (never fail-open to "no
// key configured means allow anything").
var ErrNoKeys = errors.New("selfupdate: empty release keyring — every artifact refused")

// ErrBadSignature is returned when a signature verifies against none of the trusted keys —
// a tampered artifact, a wrong/rotated-away key, or a truncated signature.
var ErrBadSignature = errors.New("selfupdate: signature does not verify against any release key")

// Keyring is the set of release-signing public keys the agent trusts. An artifact is accepted
// iff at least ONE key verifies its signature, so:
//   - key rotation is additive — ship the next public key in a base-image update BEFORE
//     signing releases with it, and the union keeps verifying across the cutover;
//   - a lost signing key is survivable — the offline M-of-N root blesses a fresh warm signer
//     and its public key joins the ring; agents trust the union, no flag-day.
//
// The ring itself is part of the frozen base install, not something the volatile agent can
// rewrite — same trust boundary as the pivot (a compromised agent must not be able to widen
// what it will accept).
type Keyring struct {
	keys []ed25519.PublicKey
}

// NewKeyring parses one or more PEM blobs (each may hold several concatenated blocks) of PKIX
// "PUBLIC KEY" Ed25519 keys into a ring. It errors on a malformed block or a non-Ed25519 key
// (a misconfigured ring must fail loudly at load, not silently accept nothing at verify time).
// An empty ring is allowed to construct but refuses everything at Verify (ErrNoKeys) — that is
// the fail-closed default a node with no keys provisioned lands on.
func NewKeyring(pems ...[]byte) (*Keyring, error) {
	kr := &Keyring{}
	for _, blob := range pems {
		rest := blob
		for {
			var block *pem.Block
			block, rest = pem.Decode(rest)
			if block == nil {
				break
			}
			if block.Type != "PUBLIC KEY" {
				return nil, fmt.Errorf("selfupdate: keyring: unexpected PEM block %q (want PUBLIC KEY)", block.Type)
			}
			pub, err := x509.ParsePKIXPublicKey(block.Bytes)
			if err != nil {
				return nil, fmt.Errorf("selfupdate: keyring: parse public key: %w", err)
			}
			ed, ok := pub.(ed25519.PublicKey)
			if !ok {
				return nil, fmt.Errorf("selfupdate: keyring: %T is not an Ed25519 key", pub)
			}
			kr.keys = append(kr.keys, ed)
		}
	}
	return kr, nil
}

// Len reports how many keys the ring trusts (0 → Verify always refuses).
func (k *Keyring) Len() int { return len(k.keys) }

// Verify checks a detached Ed25519 signature over the exact artifact bytes. It returns nil iff
// the ring is non-empty, sig is present, and at least one trusted key verifies. Errors are
// specific (ErrNoKeys / ErrUnsigned / ErrBadSignature) so the caller can alert precisely and,
// crucially, NOT stage the artifact — the committed binary stays. The message signed is the
// whole artifact (stdlib PureEdDSA), so there is no separate hash step to get wrong.
func (k *Keyring) Verify(artifact, sig []byte) error {
	if len(k.keys) == 0 {
		return ErrNoKeys
	}
	if len(sig) == 0 {
		return ErrUnsigned
	}
	if len(sig) != ed25519.SignatureSize {
		return ErrBadSignature // a truncated/oversized sig can never verify; reject before the loop
	}
	for _, key := range k.keys {
		if ed25519.Verify(key, artifact, sig) {
			return nil
		}
	}
	return ErrBadSignature
}
