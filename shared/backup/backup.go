// Package backup produces and restores an encrypted, off-site copy of a home's
// *sacred* Home Assistant state — the `.storage` config directory plus top-level YAML —
// the durability floor DR calls for. This is the cheap, tarball half of DR
// : the full btrfs-stream cold-restore ladder is deferred. The recorder DB
// is deliberately excluded — it is disposable, so leaving it out keeps the
// archive small and the restore fast; the sacred data (loss = re-pair) is what we copy.
//
// Encrypted **client-side** with age (X25519): the archive is sealed to the household's
// public key before it leaves the home, and only the matching private identity — held by
// the household, never by the cloud — can restore it (the same posture as the on-prem
// node never holding the cloud's cert-issuance token). The archive is tar+gzip (`.storage` is JSON,
// so it compresses well) inside the age envelope.
package backup

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"filippo.io/age"
)

// Save writes an encrypted tar.gz of each include (a file or directory path relative to
// base) to w, sealed to recipient. Directories are archived recursively; entry names are
// stored relative to base so Load reconstructs the same tree. Only regular files and
// directories are archived — anything else (symlink, device, socket) is skipped, since
// HA's `.storage` holds none and following them would be a restore-time hazard.
func Save(base string, includes []string, recipient age.Recipient, w io.Writer) (err error) {
	enc, err := age.Encrypt(w, recipient)
	if err != nil {
		return fmt.Errorf("age encrypt: %w", err)
	}
	gz := gzip.NewWriter(enc)
	tw := tar.NewWriter(gz)

	for _, inc := range includes {
		if e := archivePath(tw, base, inc); e != nil {
			return e
		}
	}
	// Close inner→outer so every buffer flushes into the age stream before it seals.
	if e := tw.Close(); e != nil {
		return fmt.Errorf("close tar: %w", e)
	}
	if e := gz.Close(); e != nil {
		return fmt.Errorf("close gzip: %w", e)
	}
	if e := enc.Close(); e != nil {
		return fmt.Errorf("close age: %w", e)
	}
	return nil
}

// archivePath walks one include (relative to base) and writes its regular files +
// directories to tw with base-relative names. A missing include is skipped (a home may
// not have every optional config file), not an error.
func archivePath(tw *tar.Writer, base, include string) error {
	root := filepath.Join(base, include)
	if _, err := os.Lstat(root); os.IsNotExist(err) {
		return nil
	}
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() && !d.IsDir() {
			return nil // skip symlinks/devices/etc.
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(base, path)
		if err != nil {
			return err
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		if d.IsDir() {
			hdr.Name += "/"
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(tw, f)
		return err
	})
}

// Load decrypts r with identity and extracts the tar.gz into base, recreating the tree
// Save recorded. Existing files are overwritten; the caller decides whether base should
// be wiped first (a restore onto a clean `.storage` vs. a merge). Entry paths are
// validated to stay within base (no traversal out via `..`).
func Load(r io.Reader, identity age.Identity, base string) error {
	dec, err := age.Decrypt(r, identity)
	if err != nil {
		return fmt.Errorf("age decrypt: %w", err)
	}
	gz, err := gzip.NewReader(dec)
	if err != nil {
		return fmt.Errorf("gzip reader: %w", err)
	}
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("tar next: %w", err)
		}
		dest, err := safeJoin(base, hdr.Name)
		if err != nil {
			return err
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(dest, fs.FileMode(hdr.Mode)&0o777|0o700); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
				return err
			}
			if err := writeFile(dest, tr, fs.FileMode(hdr.Mode)&0o777); err != nil {
				return err
			}
		default:
			// Save never writes these; ignore defensively rather than fail a restore.
		}
	}
	return gz.Close()
}

// safeJoin joins name onto base, rejecting any entry that would escape base (zip-slip).
func safeJoin(base, name string) (string, error) {
	clean := filepath.Clean(filepath.Join(base, filepath.FromSlash(name)))
	if clean != base && !strings.HasPrefix(clean, base+string(os.PathSeparator)) {
		return "", fmt.Errorf("archive entry %q escapes the restore root", name)
	}
	return clean, nil
}

func writeFile(dest string, r io.Reader, mode fs.FileMode) error {
	f, err := os.OpenFile(dest, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
