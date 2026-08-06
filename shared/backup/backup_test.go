package backup

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// writeTree lays out a small `.storage`-like tree under dir and returns the relative
// file paths → contents it created, for later comparison.
func writeTree(t *testing.T, dir string) map[string]string {
	t.Helper()
	files := map[string]string{
		".storage/core.config_entries":    `{"version":1,"data":{"entries":[]}}`,
		".storage/auth":                   `{"users":["owner"]}`,
		".storage/nested/deep/thing.json": `{"deep":true}`,
		"configuration.yaml":              "default_config:\nbriard_canary:\n",
	}
	for rel, content := range files {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return files
}

// TestSaveLoadRoundTrip is the core contract: a home's config survives archive →
// encrypt → decrypt → extract byte-for-byte.
func TestSaveLoadRoundTrip(t *testing.T) {
	src := t.TempDir()
	want := writeTree(t, src)
	// A file NOT in the include list must not be backed up (e.g. the disposable DB).
	if err := os.WriteFile(filepath.Join(src, "home-assistant_v2.db"), []byte("recorder"), 0o600); err != nil {
		t.Fatal(err)
	}

	key, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	recip, err := ParseRecipient(key.Recipient)
	if err != nil {
		t.Fatal(err)
	}

	var blob bytes.Buffer
	if err := Save(src, []string{".storage", "configuration.yaml"}, recip, &blob); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if blob.Len() == 0 {
		t.Fatal("empty backup blob")
	}
	// The blob must be an age file (encrypted), not the plaintext.
	if bytes.Contains(blob.Bytes(), []byte("briard_canary")) {
		t.Fatal("plaintext config leaked into the encrypted blob")
	}

	dst := t.TempDir()
	id, err := ParseIdentity(key.Identity)
	if err != nil {
		t.Fatal(err)
	}
	if err := Load(bytes.NewReader(blob.Bytes()), id, dst); err != nil {
		t.Fatalf("Load: %v", err)
	}

	for rel, content := range want {
		got, err := os.ReadFile(filepath.Join(dst, rel))
		if err != nil {
			t.Errorf("restored %s: %v", rel, err)
			continue
		}
		if string(got) != content {
			t.Errorf("restored %s = %q, want %q", rel, got, content)
		}
	}
	// The excluded DB must be absent from the restore.
	if _, err := os.Stat(filepath.Join(dst, "home-assistant_v2.db")); !os.IsNotExist(err) {
		t.Errorf("excluded recorder DB was backed up (err=%v)", err)
	}
}

// TestLoadWrongIdentityFails proves the encryption is real: a different key can't decrypt.
func TestLoadWrongIdentityFails(t *testing.T) {
	src := t.TempDir()
	writeTree(t, src)
	key, _ := GenerateKey()
	recip, _ := ParseRecipient(key.Recipient)
	var blob bytes.Buffer
	if err := Save(src, []string{".storage"}, recip, &blob); err != nil {
		t.Fatal(err)
	}

	other, _ := GenerateKey()
	wrongID, _ := ParseIdentity(other.Identity)
	if err := Load(bytes.NewReader(blob.Bytes()), wrongID, t.TempDir()); err == nil {
		t.Fatal("Load succeeded with the wrong identity — encryption is not protecting the backup")
	}
}

// TestMissingIncludeSkipped: a home lacking an optional config file still backs up cleanly.
func TestMissingIncludeSkipped(t *testing.T) {
	src := t.TempDir()
	if err := os.MkdirAll(filepath.Join(src, ".storage"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, ".storage/x"), []byte("y"), 0o600); err != nil {
		t.Fatal(err)
	}
	key, _ := GenerateKey()
	recip, _ := ParseRecipient(key.Recipient)
	var blob bytes.Buffer
	// "secrets.yaml" doesn't exist — must be skipped, not error.
	if err := Save(src, []string{".storage", "secrets.yaml"}, recip, &blob); err != nil {
		t.Fatalf("Save with a missing include: %v", err)
	}
	dst := t.TempDir()
	id, _ := ParseIdentity(key.Identity)
	if err := Load(bytes.NewReader(blob.Bytes()), id, dst); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(dst, ".storage/x")); string(got) != "y" {
		t.Errorf("present file not restored (got %q)", got)
	}
}

// TestLoadOrCreateKey: first call mints + persists (0600); second call returns the same
// stable key — so every backup seals to one recipient and any is restorable.
func TestLoadOrCreateKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "backup-key.json")
	k1, err := LoadOrCreateKey(path)
	if err != nil {
		t.Fatal(err)
	}
	if k1.Identity == "" || k1.Recipient == "" {
		t.Fatal("minted key incomplete")
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("key file mode = %o, want 600 (private)", fi.Mode().Perm())
	}
	k2, err := LoadOrCreateKey(path)
	if err != nil {
		t.Fatal(err)
	}
	if k2 != k1 {
		t.Errorf("second call returned a different key: %+v vs %+v", k2, k1)
	}
}
