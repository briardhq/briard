package backup

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"filippo.io/age"
)

// Key is a household backup keypair as serialized strings. The Recipient (public,
// "age1…") is all the guest needs to *seal* a backup; the Identity (private,
// "AGE-SECRET-KEY-…") is needed only to *restore* and stays with the household. The host
// generates the pair once and holds it in its node-local state, exportable so the user
// can escrow the Identity off-box — without it, a fully-lost home cannot decrypt its own
// backups (durable cross-node/off-site key custody is the DR follow-on).
type Key struct {
	Identity  string // AGE-SECRET-KEY-… (private; restore only)
	Recipient string // age1…            (public; seal a backup)
}

// LoadOrCreateKey returns the household key persisted at path, minting + writing a fresh
// one (0600) on first use. The host holds this in its node-local state so every backup
// seals to the *same* recipient — any backup is then restorable with the one identity.
// The file is the household's key of record; the host should surface it for the user to
// escrow off-box (without it, a fully-lost home can't decrypt its backups).
func LoadOrCreateKey(path string) (Key, error) {
	b, err := os.ReadFile(path)
	if err == nil {
		var k Key
		if err := json.Unmarshal(b, &k); err != nil {
			return Key{}, fmt.Errorf("parse backup key %s: %w", path, err)
		}
		if k.Identity == "" || k.Recipient == "" {
			return Key{}, fmt.Errorf("backup key %s is incomplete", path)
		}
		return k, nil
	}
	if !os.IsNotExist(err) {
		return Key{}, fmt.Errorf("read backup key %s: %w", path, err)
	}
	k, err := GenerateKey()
	if err != nil {
		return Key{}, err
	}
	data, err := json.Marshal(k)
	if err != nil {
		return Key{}, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return Key{}, err
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return Key{}, fmt.Errorf("write backup key %s: %w", path, err)
	}
	return k, nil
}

// GenerateKey mints a fresh household backup keypair.
func GenerateKey() (Key, error) {
	id, err := age.GenerateX25519Identity()
	if err != nil {
		return Key{}, fmt.Errorf("generate age identity: %w", err)
	}
	return Key{Identity: id.String(), Recipient: id.Recipient().String()}, nil
}

// ParseRecipient parses a public "age1…" recipient (to seal a backup).
func ParseRecipient(s string) (age.Recipient, error) {
	r, err := age.ParseX25519Recipient(s)
	if err != nil {
		return nil, fmt.Errorf("parse recipient: %w", err)
	}
	return r, nil
}

// ParseIdentity parses a private "AGE-SECRET-KEY-…" identity (to restore a backup).
func ParseIdentity(s string) (age.Identity, error) {
	id, err := age.ParseX25519Identity(s)
	if err != nil {
		return nil, fmt.Errorf("parse identity: %w", err)
	}
	return id, nil
}
